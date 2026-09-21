//go:build linux

// The completed-data move of FR-103: an atomic rename when source and
// destination share a filesystem, otherwise a copy-verify-delete through a
// staging directory on the destination filesystem, so the source is never
// touched before the copy is proven. This file is the only place in the
// codebase allowed to relocate a completed payload
// (docs/10-deployment-and-compose.md section 3.5). The build tag states the
// syscall.EXDEV dependency like space.go's: the runtime image is Linux.
package fsx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"
)

// Progress reports a running byte count during a cross-filesystem copy. It is
// called at most once per second and never with a decreasing copied value.
type Progress func(copied, total int64)

// ErrVerifyFailed is returned when a copied file's size does not match its
// source. Nothing is deleted.
var ErrVerifyFailed = errors.New("fsx: copied tree does not match source")

// moveCopyBuffer bounds one read-write step of the fallback copy; the
// context check between steps is what keeps a 40 GiB NAS copy cancellable.
const moveCopyBuffer = 4 << 20

// progressInterval is the minimum spacing between onProgress calls —
// the "at most once per second" of the Progress contract.
const progressInterval = time.Second

// Move relocates src to dst. Both paths must already have been resolved by
// ResolveDestination; Move performs no containment check of its own.
//
// It first attempts os.Rename. When that fails with EXDEV it falls back to
// copy-verify-delete:
//  1. walk src and sum the regular-file bytes into total;
//  2. copy every entry into a staging directory beside dst on dst's
//     filesystem, preserving the relative tree, files at 0644 and
//     directories at 0755;
//  3. verify: every destination file exists with the same size as its
//     source;
//  4. rename the staging directory onto dst, then remove src.
//
// Nothing under src is unlinked before step 3 succeeds. On any error the
// staging directory is removed and src is left byte-for-byte untouched.
func Move(ctx context.Context, src, dst string, onProgress Progress) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	// Only the cross-device answer buys the copy path; every other rename
	// failure — a missing source, a permission wall, a name collision — is
	// the verdict itself, returned unchanged.
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}

	return moveAcrossFilesystems(ctx, src, dst, onProgress)
}

// SameFilesystem reports whether a and b live on one filesystem, using
// FilesystemID from space.go.
func SameFilesystem(a, b string) (bool, error) {
	aID, err := filesystemIDOfPath(a)
	if err != nil {
		return false, err
	}
	bID, err := filesystemIDOfPath(b)
	if err != nil {
		return false, err
	}

	return aID == bID, nil
}

// filesystemIDOfPath resolves path to the filesystem holding it: a path
// that exists and is not a directory — the payload file a move carries —
// lives on its parent directory's filesystem, the boundary rename(2)
// actually checks; FilesystemID itself answers only for directories.
func filesystemIDOfPath(path string) (string, error) {
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		path = filepath.Dir(path)
	}

	return FilesystemID(path)
}

// moveAcrossFilesystems is the EXDEV fallback: stage the whole payload on the
// destination filesystem, verify it, deliver it with one rename, and only then
// remove the source. Staging keeps the payload's own name out of the
// destination directory until the copy is proven, so a crash mid-copy never
// presents a half-written file as the task's data. The ULID suffix keeps a
// retry's staging distinct from a crashed run's litter.
func moveAcrossFilesystems(ctx context.Context, src, dst string, onProgress Progress) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("fsx: move %s: %w", src, err)
	}
	if !srcInfo.IsDir() && !srcInfo.Mode().IsRegular() {
		return fmt.Errorf("fsx: move %s: not a regular file or directory", src)
	}
	// A symlink src would stat through to its target while WalkDir sees
	// the link — the copy would stage nothing and the removal would drop
	// the link, reporting a moved tree that never existed. Links are never
	// preserved, so refuse instead of guessing which side to copy.
	if linfo, lerr := os.Lstat(src); lerr == nil && linfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("fsx: move %s: a symlink cannot be copied across filesystems", src)
	}

	total, err := sumMoveBytes(ctx, src)
	if err != nil {
		return fmt.Errorf("fsx: move %s: %w", src, err)
	}

	staging := filepath.Join(filepath.Dir(dst), ".dl-tool-move-"+ulid.Make().String())

	reporter := &moveProgress{fn: onProgress, total: total}
	var copyErr error
	if srcInfo.IsDir() {
		copyErr = copyTree(ctx, src, staging, reporter)
	} else {
		copyErr = copyRegularFile(ctx, src, staging, reporter)
	}
	if copyErr != nil {
		return errors.Join(mapMoveError(copyErr), removeMoveStaging(staging))
	}

	var verifyErr error
	if srcInfo.IsDir() {
		verifyErr = verifyMovedTree(src, staging)
	} else {
		verifyErr = verifyMovedFile(src, staging)
	}
	if verifyErr != nil {
		return errors.Join(mapMoveError(verifyErr), removeMoveStaging(staging))
	}

	if err := os.Rename(staging, dst); err != nil {
		return errors.Join(
			mapMoveError(fmt.Errorf("fsx: deliver staged move to %s: %w", dst, err)),
			removeMoveStaging(staging),
		)
	}
	// The rename made dst durable-looking; the directory fsync makes it
	// durable in fact before the source is unlinked — doc 10 section 3.5's
	// "never delete a source before the destination is durable".
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return errors.Join(
			mapMoveError(fmt.Errorf("fsx: fsync destination directory: %w", err)),
			removeMoveStaging(dst),
		)
	}

	if err := os.RemoveAll(src); err != nil {
		// The verified copy is in place; only the source removal failed. The
		// handler still records the failure — a kept source is a duplicate,
		// not data loss.
		return fmt.Errorf("fsx: move %s: destination delivered but removing the source failed: %w", src, err)
	}

	return nil
}

// mapMoveError translates a copy-path failure into the error the caller maps:
// an ENOSPC anywhere in the fallback is fsx.ErrDiskFull, every other failure
// passes through untouched.
func mapMoveError(err error) error {
	if IsENOSPC(err) {
		return ErrDiskFull
	}

	return err
}

// removeMoveStaging discards the staging path; a failure is joined onto the
// verdict already in flight, so cleanup litter is recorded, never a changed
// verdict.
func removeMoveStaging(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("fsx: remove move staging %q: %w", path, err)
	}

	return nil
}

// sumMoveBytes walks src and sums the regular-file bytes — the total the
// progress callback reports against and the move handler's space pre-check
// mirrors.
func sumMoveBytes(ctx context.Context, src string) (int64, error) {
	var total int64
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}

		return nil
	})

	return total, err
}

// copyTree recreates the src tree inside staging — a fresh directory created
// beside dst — with files at 0644 and directories at 0755. Entries that are
// neither regular files nor directories (links, fifos, sockets, devices) are
// skipped: the task contract forbids preserving them, and a followed symlink
// could leave the resolved destination.
func copyTree(ctx context.Context, src, staging string, p *moveProgress) error {
	if err := os.Mkdir(staging, 0o755); err != nil {
		return fmt.Errorf("fsx: create move staging dir: %w", err)
	}
	if err := os.Chmod(staging, 0o755); err != nil {
		return fmt.Errorf("fsx: mode move staging dir: %w", err)
	}

	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == src {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(staging, rel)
		switch {
		case d.IsDir():
			if err := os.Mkdir(target, 0o755); err != nil {
				return fmt.Errorf("fsx: create move dir %q: %w", rel, err)
			}
			// Chmod lands the documented mode exactly — Mkdir honours the
			// process umask, which could tighten the mode below 0755.
			if err := os.Chmod(target, 0o755); err != nil {
				return fmt.Errorf("fsx: mode move dir %q: %w", rel, err)
			}
		case d.Type().IsRegular():
			if err := copyRegularFile(ctx, path, target, p); err != nil {
				return err
			}
		default:
			// Links and special files are skipped per the task contract.
		}

		return nil
	})
}

// copyRegularFile streams one file into the destination filesystem at the
// documented 0644 mode, fsynced before close so the verify step reads back
// what the disk holds, not what the page cache claims.
func copyRegularFile(ctx context.Context, src, dst string, p *moveProgress) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("fsx: open move source %q: %w", src, err)
	}
	defer func() {
		if cerr := in.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("fsx: close move source %q: %w", src, cerr))
		}
	}()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("fsx: create staged copy %q: %w", dst, err)
	}

	buf := make([]byte, moveCopyBuffer)
	var copyErr error
	for {
		if err := ctx.Err(); err != nil {
			copyErr = err
			break
		}
		n, readErr := in.Read(buf)
		if n > 0 {
			written, writeErr := out.Write(buf[:n])
			p.add(int64(written))
			if writeErr != nil {
				copyErr = fmt.Errorf("fsx: write staged copy %q: %w", dst, writeErr)
				break
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			copyErr = fmt.Errorf("fsx: read move source %q: %w", src, readErr)
			break
		}
	}
	if copyErr == nil {
		copyErr = out.Sync()
	}
	if cerr := out.Close(); cerr != nil {
		copyErr = errors.Join(copyErr, fmt.Errorf("fsx: close staged copy %q: %w", dst, cerr))
	}
	if copyErr != nil {
		return copyErr
	}
	// Chmod lands 0644 regardless of the process umask — the task contract
	// fixes the mode, the umask may only tighten an open-time mode.
	if err := os.Chmod(dst, 0o644); err != nil {
		return fmt.Errorf("fsx: mode staged copy %q: %w", dst, err)
	}

	return nil
}

// verifyMovedTree checks that every regular file under src exists under
// staging with the same size — the gate the source removal waits behind.
func verifyMovedTree(src, staging string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == src {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		srcInfo, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		stagedInfo, err := os.Stat(filepath.Join(staging, rel))
		if err != nil || stagedInfo.Size() != srcInfo.Size() {
			return ErrVerifyFailed
		}

		return nil
	})
}

// verifyMovedFile is the single-file case of verifyMovedTree.
func verifyMovedFile(src, staged string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("fsx: verify move source %q: %w", src, err)
	}
	stagedInfo, err := os.Stat(staged)
	if err != nil || stagedInfo.Size() != srcInfo.Size() {
		return ErrVerifyFailed
	}

	return nil
}

// syncDir fsyncs a directory so the rename landing inside it is durable.
func syncDir(dir string) (err error) {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := d.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("fsx: close dir %s: %w", dir, cerr))
		}
	}()

	return d.Sync()
}

// moveProgress throttles a copy's byte counter to the Progress contract: the
// first write always reports, later reports wait out the interval, and the
// count never decreases because copied only grows.
type moveProgress struct {
	fn     Progress
	total  int64
	copied int64
	last   time.Time
}

func (p *moveProgress) add(n int64) {
	p.copied += n
	if p.fn == nil {
		return
	}
	if p.last.IsZero() || time.Since(p.last) >= progressInterval {
		p.last = time.Now()
		p.fn(p.copied, p.total)
	}
}
