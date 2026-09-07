//go:build linux

// Disk-space accounting of FR-047 and FR-048: the statfs answer, the
// filesystem identity that pools destinations on one mount, and the
// reservation arithmetic that decides whether a task's remaining bytes
// fit beside every other active task's committed-but-unwritten bytes and
// the root's min_free_space floor. The runtime image is Linux
// (docs/10-deployment-and-compose.md); the build tag states the statfs
// dependency instead of failing on a missing field.
package fsx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// DefaultMinFreeBytes is the min_free_space floor of every root the stored
// settings map does not name (docs/11-config-reference.md section 5): 2 GiB
// of head-room no task is ever promised.
const DefaultMinFreeBytes int64 = 2147483648

// Floor resolves one root's floor from the stored min_free_space map: an
// explicit entry wins — an explicit 0 disables the floor for that root —
// and a root the map does not carry, or one carrying a negative value a
// hand-edited row could produce, gets the 2 GiB default: a bad entry must
// never turn into extra promisable space. The lookup cleans both sides,
// so a trailing slash on either the root or a stored key cannot make an
// operator's explicit floor silently miss; the map has one entry per
// configured root, so the scan is negligible. Entries for roots no longer
// present in DLTOOL_DATA_ROOTS never reach this function: the caller
// resolves the destination's root first and looks only that root up.
func Floor(minFree map[string]int64, root string) int64 {
	cleaned := filepath.Clean(root)
	match := false
	var floor int64
	for key, value := range minFree {
		// Two keys that clean to the same root — a hand-edited row — must
		// not make the floor depend on map iteration order: the largest
		// matching floor wins, so a bad duplicate can never manufacture
		// extra promisable space.
		if value >= 0 && filepath.Clean(key) == cleaned && (!match || value > floor) {
			floor, match = value, true
		}
	}
	if match {
		return floor
	}

	return DefaultMinFreeBytes
}

// Space is the answer of one statfs call, in bytes. Both values are plain
// integers, never KB.
type Space struct {
	FreeBytes  int64
	TotalBytes int64
}

// FreeSpace reports the space at path. The path must already have been
// resolved by ResolveDestination; FreeSpace performs no containment check
// of its own. FreeBytes is f_bavail * f_frsize — the bytes an unprivileged
// process may actually take (docs/17-operations-and-runbook.md section 5);
// du is never the source, because a hardlinked library copy double-counts.
func FreeSpace(path string) (Space, error) {
	actual, err := existingAncestor(path)
	if err != nil {
		return Space{}, err
	}

	var st syscall.Statfs_t
	if err := syscall.Statfs(actual, &st); err != nil {
		return Space{}, fmt.Errorf("fsx: statfs %s (nearest existing ancestor of %s): %w", actual, path, err)
	}

	frsize := int64(st.Frsize)
	// A zero f_frsize would zero the whole answer; statfs-fill filesystems
	// report the block size instead, and a genuine zero-zero answer keeps
	// zero — nothing is free.
	if frsize == 0 {
		frsize = int64(st.Bsize)
	}

	// A negative f_frsize is garbage — the field is signed and a buggy
	// FUSE mount controls its own statfs answer — and it would bypass the
	// overflow guard below and hand back negative byte counts. Fail closed
	// like every other absurd statfs answer.
	if frsize < 0 {
		return Space{}, fmt.Errorf("fsx: statfs %s (nearest existing ancestor of %s): negative fragment size %d", actual, path, frsize)
	}

	// The products assume a filesystem whose byte counts fit in int64; past
	// 8 EiB the multiplication wraps modulo 2^64 and can land positive, so
	// an overflowing answer is an error, never a wrapped guess.
	const maxInt64 = uint64(1<<63 - 1)
	if frsize > 0 && (uint64(st.Bavail) > maxInt64/uint64(frsize) || uint64(st.Blocks) > maxInt64/uint64(frsize)) {
		return Space{}, fmt.Errorf("fsx: statfs %s (nearest existing ancestor of %s): byte count overflows int64", actual, path)
	}

	return Space{
		FreeBytes:  int64(st.Bavail) * frsize,
		TotalBytes: int64(st.Blocks) * frsize,
	}, nil
}

// FilesystemID returns an identifier for the filesystem holding
// path, so two destinations on one mount share one reservation pool. Two
// paths on the same device return the same value; the identifier is the
// device number of stat(2) — exactly the boundary across which rename(2)
// fails with EXDEV. The identifier is stable within one process run
// only: anonymous device numbers (overlayfs, btrfs subvolumes) are
// reallocated on every mount, so never persist it across runs — the
// pools are rebuilt from the destinations each pass anyway. Known
// limit: btrfs gives every subvolume its own device number, so two
// roots on subvolumes of one btrfs filesystem pool separately while
// both pools count the same shared free bytes — the pair can jointly
// over-admit until ENOSPC, and FR-048's pause-and-resume keeps the
// result correct.
func FilesystemID(path string) (string, error) {
	actual, err := existingAncestor(path)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(actual)
	if err != nil {
		return "", fmt.Errorf("fsx: stat %s (nearest existing ancestor of %s): %w", actual, path, err)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("fsx: stat %s: no stat_t available", actual)
	}

	return "dev:" + strconv.FormatUint(uint64(sys.Dev), 10), nil
}

// existingAncestor climbs from path to its nearest existing ancestor: a
// destination directory may legitimately not exist yet — creation only
// resolves the path (T020); mkdir arrives with T047 — but the filesystem
// that will hold it is already mounted and stat-able at its parent.
// ENOTDIR — a path component is a regular file — climbs like not-exist:
// the mount holding that file is where the destination would live. But
// when the nearest existing ancestor is itself a regular file, the
// destination can never be created — mkdir on it fails forever — so the
// climb fails closed instead of handing back a space answer for an
// uncreatable destination. Any other stat failure (a permission wall,
// an I/O error) says nothing
// about which filesystem holds the path: promising an ancestor's space
// for a destination dl-tool cannot see would over-admit, so it fails
// closed instead. A relative path is rejected up front: climbing one
// would pool the destination against the process working directory's
// mount — a plausible but wrong answer, exactly the mispooling the
// caller's contract (destinations are absolute, resolved by
// ResolveDestination) exists to prevent.
func existingAncestor(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("fsx: path %q is not absolute; destinations are resolved before space checks", path)
	}

	for current := path; ; {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("fsx: %s (nearest existing ancestor of %s) is not a directory", current, path)
			}

			return current, nil
		} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return "", fmt.Errorf("fsx: stat %s: %w", current, err)
		}
		if current == string(filepath.Separator) {
			return "", fmt.Errorf("fsx: no existing ancestor of %s", path)
		}

		next := filepath.Dir(current)
		if next == current {
			return "", fmt.Errorf("fsx: no existing ancestor of %s", path)
		}
		current = next
	}
}

// Reservation is the committed-but-unwritten accounting for one filesystem.
type Reservation struct {
	FilesystemID   string
	FreeBytes      int64 // as reported by statfs right now
	CommittedBytes int64 // sum of total_bytes - completed_bytes over active tasks on this filesystem
	MinFreeBytes   int64 // this root's Floor() result; zero disables the floor, it is not the 2 GiB default
}

// Admits reports whether a task needing remaining bytes may start:
//
//	FreeBytes - CommittedBytes - MinFreeBytes >= remaining
//
// A task whose total_bytes is still unknown is checked with
// remaining = 0 — still held when the filesystem sits below its floor —
// and re-checked when metadata resolves. The subtraction is stepwise so no
// intermediate can overflow, and garbage never admits: a negative
// commitment, floor, free answer or request is not head-room, it is a bug in the
// caller's arithmetic, and holding every task is the safe answer.
func (r Reservation) Admits(remaining int64) bool {
	if r.FreeBytes < 0 || r.CommittedBytes < 0 || r.MinFreeBytes < 0 || remaining < 0 {
		return false
	}

	available := r.FreeBytes
	if available < r.MinFreeBytes {
		return false // the floor alone exhausts the free answer
	}
	available -= r.MinFreeBytes
	if available < r.CommittedBytes {
		return false // the committed pool alone exhausts it
	}
	available -= r.CommittedBytes

	return available >= remaining
}

// ErrDiskFull is returned when a write failed with ENOSPC. It wraps
// syscall.ENOSPC itself, so a caller holding ErrDiskFull and a caller
// holding the raw errno both resolve the same disk-full answer through
// IsENOSPC or errors.Is. The caller pauses the task with the
// tasks.error_code value disk_full and unlinks nothing.
var ErrDiskFull = fmt.Errorf("fsx: %w", syscall.ENOSPC)

// IsENOSPC reports whether err is or wraps syscall.ENOSPC, so a write
// failure surfaced through any number of fmt.Errorf("%w") layers still
// qualifies for the disk-full pause of FR-048.
func IsENOSPC(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}
