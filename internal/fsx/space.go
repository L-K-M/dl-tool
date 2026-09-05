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
// never turn into extra promisable space. The lookup cleans the root so a
// trailing slash cannot make an operator's explicit floor silently miss
// (the loader cleans the stored keys to the same form). Entries for roots
// no longer present in DLTOOL_DATA_ROOTS never reach this function: the
// caller resolves the destination's root first and looks only that root
// up.
func Floor(minFree map[string]int64, root string) int64 {
	if floor, ok := minFree[filepath.Clean(root)]; ok && floor >= 0 {
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
		return Space{}, fmt.Errorf("fsx: statfs %s: %w", actual, err)
	}

	frsize := int64(st.Frsize)
	// A zero f_frsize would zero the whole answer; statfs-fill filesystems
	// report the block size instead, and a genuine zero-zero answer keeps
	// zero — nothing is free.
	if frsize == 0 {
		frsize = int64(st.Bsize)
	}

	return Space{
		FreeBytes:  int64(st.Bavail) * frsize,
		TotalBytes: int64(st.Blocks) * frsize,
	}, nil
}

// FilesystemID returns a stable identifier for the filesystem holding
// path, so two destinations on one mount share one reservation pool. Two
// paths on the same device return the same value; the identifier is the
// device number of stat(2) — exactly the boundary across which rename(2)
// fails with EXDEV. Known limit: btrfs gives every subvolume its own
// device number, so two roots on subvolumes of one btrfs filesystem pool
// separately while sharing the underlying free bytes; the reservation
// under-promises consolidation there, and FR-048's pause-and-resume keeps
// the result correct.
func FilesystemID(path string) (string, error) {
	actual, err := existingAncestor(path)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(actual)
	if err != nil {
		return "", fmt.Errorf("fsx: stat %s: %w", actual, err)
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
// ENOTDIR — a path component is a regular file — climbs like not-exist,
// because the file itself exists on the mount the climb then finds. Any
// other stat failure (a permission wall, an I/O error) says nothing
// about which filesystem holds the path: promising an ancestor's space
// for a destination dl-tool cannot see would over-admit, so it fails
// closed instead. Destinations are absolute, so the separator ends the
// climb; the fixed-point guard covers a relative path whose working
// directory has vanished — Dir(".") is "." — which would otherwise loop
// forever.
func existingAncestor(path string) (string, error) {
	for current := path; ; {
		if _, err := os.Stat(current); err == nil {
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
	MinFreeBytes   int64 // this root's min_free_space, default 2147483648
}

// Admits reports whether a task needing remaining bytes may start:
//
//	FreeBytes - CommittedBytes - MinFreeBytes >= remaining
//
// A task whose total_bytes is still unknown passes with remaining = 0 and
// is re-checked when metadata resolves. The subtraction is stepwise so no
// intermediate can overflow, and garbage never admits: a negative
// commitment, floor or request is not head-room, it is a bug in the
// caller's arithmetic, and holding every task is the safe answer.
func (r Reservation) Admits(remaining int64) bool {
	if r.CommittedBytes < 0 || r.MinFreeBytes < 0 || remaining < 0 {
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
var ErrDiskFull = fmt.Errorf("fsx: no space left on device: %w", syscall.ENOSPC)

// IsENOSPC reports whether err is or wraps syscall.ENOSPC, so a write
// failure surfaced through any number of fmt.Errorf("%w") layers still
// qualifies for the disk-full pause of FR-048.
func IsENOSPC(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}
