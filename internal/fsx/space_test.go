//go:build linux

package fsx_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/L-K-M/dl-tool/internal/fsx"
)

// TestAdmitsAccountsForCommittedBytes pins the reservation arithmetic of
// FR-047 at its boundary: head-room of exactly `remaining` admits, one
// byte more does not, and both the committed pool and the floor subtract
// before the comparison.
func TestAdmitsAccountsForCommittedBytes(t *testing.T) {
	r := fsx.Reservation{FilesystemID: "dev:1", FreeBytes: 1000, CommittedBytes: 300, MinFreeBytes: 200}

	// 1000 - 300 - 200 = 500: exactly the head-room admits.
	if !r.Admits(500) {
		t.Error("Admits(500) = false, want true at exactly the head-room")
	}
	// One byte past the head-room is held.
	if r.Admits(501) {
		t.Error("Admits(501) = true, want false one byte past the head-room")
	}

	// The committed pool subtracts: another active task's unwritten bytes
	// shrink the head-room the same amount of free space would offer.
	uncommitted := r
	uncommitted.CommittedBytes = 0
	if !uncommitted.Admits(800) {
		t.Error("Admits(800) = false with no committed bytes, want the pool to subtract")
	}

	// The floor subtracts even for a task that needs nothing: a filesystem
	// below its floor admits no new work at all.
	floored := fsx.Reservation{FilesystemID: "dev:1", FreeBytes: 100, CommittedBytes: 0, MinFreeBytes: 200}
	if floored.Admits(0) {
		t.Error("Admits(0) = true below the floor, want the floor to hold every task")
	}

	// An explicit zero floor disables that root's floor: everything free
	// is promisable.
	noFloor := fsx.Reservation{FilesystemID: "dev:1", FreeBytes: 100, MinFreeBytes: 0}
	if !noFloor.Admits(100) {
		t.Error("Admits(100) = false with a zero floor, want every free byte to promise")
	}

	// Garbage never admits: a negative commitment, floor or request holds
	// every task rather than promising bytes that do not exist.
	for name, bad := range map[string]fsx.Reservation{
		"negative commitment": {FilesystemID: "dev:1", FreeBytes: 100, CommittedBytes: -1},
		"negative floor":      {FilesystemID: "dev:1", FreeBytes: 100, MinFreeBytes: -1},
	} {
		if bad.Admits(0) {
			t.Errorf("Admits(0) = true with a %s, want garbage to hold every task", name)
		}
	}
	if r.Admits(-1) {
		t.Error("Admits(-1) = true, want a negative request to hold")
	}
}

// TestDefaultFloorIsTwoGiB pins the default of docs/11-config-reference.md
// section 5: a root the stored min_free_space map does not carry resolves
// to 2147483648 bytes, an explicit entry wins and an explicit 0 disables.
func TestDefaultFloorIsTwoGiB(t *testing.T) {
	const twoGiB = int64(2147483648)

	if got := fsx.Floor(nil, "/data"); got != twoGiB {
		t.Errorf("Floor(nil, /data) = %d, want the %d default", got, twoGiB)
	}
	if got := fsx.Floor(map[string]int64{}, "/data"); got != twoGiB {
		t.Errorf("Floor({}, /data) = %d, want %d for a root the stored map misses", got, twoGiB)
	}
	if got := fsx.Floor(map[string]int64{"/data": 0}, "/data"); got != 0 {
		t.Errorf("Floor({/data:0}, /data) = %d, want 0: an explicit zero disables the floor", got)
	}
	if got := fsx.Floor(map[string]int64{"/data": 4096}, "/data"); got != 4096 {
		t.Errorf("Floor({/data:4096}, /data) = %d, want the explicit 4096", got)
	}
	if got := fsx.Floor(map[string]int64{"/other": 4096}, "/data"); got != twoGiB {
		t.Errorf("Floor({/other:4096}, /data) = %d, want %d: another root's entry is not /data's floor", got, twoGiB)
	}
	// A negative entry is a hand-edited row, not a floor: it must not
	// become extra promisable space, but fall back to the default.
	if got := fsx.Floor(map[string]int64{"/data": -100}, "/data"); got != twoGiB {
		t.Errorf("Floor({/data:-100}, /data) = %d, want the %d default, never a negative floor", got, twoGiB)
	}
}

// Two paths on one mount — including a directory that does not exist yet —
// must share one filesystem identifier, because they share one reservation
// pool (FR-047).
func TestFilesystemIDSharedPerMount(t *testing.T) {
	dir := t.TempDir()

	id, err := fsx.FilesystemID(dir)
	if err != nil {
		t.Fatalf("FilesystemID(%s): %v", dir, err)
	}
	if id == "" {
		t.Fatal("FilesystemID returned an empty identifier")
	}

	// A sibling subdirectory that does not exist yet climbs to the parent
	// and answers with the same filesystem.
	nested, err := fsx.FilesystemID(filepath.Join(dir, "does", "not", "exist"))
	if err != nil {
		t.Fatalf("FilesystemID of a not-yet-created destination: %v", err)
	}
	if nested != id {
		t.Errorf("nested destination id = %q, want the parent's %q", nested, id)
	}

	// A subdirectory that does exist answers with the mount's id too.
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", sub, err)
	}

	existing, err := fsx.FilesystemID(sub)
	if err != nil {
		t.Fatalf("FilesystemID(%s): %v", sub, err)
	}
	if existing != id {
		t.Errorf("subdirectory id = %q, want the mount's %q", existing, id)
	}

	// A regular file mid-path climbs on ENOTDIR exactly like a missing
	// directory: the answer is the mount holding the file, so a mistyped
	// destination under a file still resolves instead of erroring.
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, nil, 0o644); err != nil {
		t.Fatalf("write %s: %v", regular, err)
	}
	throughFile, err := fsx.FilesystemID(filepath.Join(regular, "not", "a", "dir"))
	if err != nil {
		t.Fatalf("FilesystemID through a file component: %v", err)
	}
	if throughFile != id {
		t.Errorf("id through a file component = %q, want the mount's %q", throughFile, id)
	}
}

// FreeSpace reads the live statfs answer: non-negative free bytes — a
// full filesystem answers zero — and a total at least as large as the
// free answer.
func TestFreeSpaceReadsTheFilesystem(t *testing.T) {
	space, err := fsx.FreeSpace(t.TempDir())
	if err != nil {
		t.Fatalf("FreeSpace: %v", err)
	}
	if space.FreeBytes < 0 {
		t.Errorf("FreeBytes = %d, want a non-negative statfs answer", space.FreeBytes)
	}
	if space.TotalBytes < space.FreeBytes {
		t.Errorf("TotalBytes = %d < FreeBytes = %d", space.TotalBytes, space.FreeBytes)
	}
}

// IsENOSPC matches through wrapping and rejects everything else, so any
// write path may report its error upward and the pause decision stays a
// single errors.Is check (FR-048).
func TestIsENOSPCMatchesWrapped(t *testing.T) {
	wrapped := fmt.Errorf("write file: %w", fmt.Errorf("flush: %w", syscall.ENOSPC))
	if !fsx.IsENOSPC(wrapped) {
		t.Error("IsENOSPC(wrapped ENOSPC) = false, want true through every %w layer")
	}
	if !fsx.IsENOSPC(syscall.ENOSPC) {
		t.Error("IsENOSPC(ENOSPC) = false, want true")
	}
	// The sentinel wraps the errno itself, so ErrDiskFull and a raw ENOSPC
	// are the same answer to the check — one write path may return the
	// sentinel, another the errno the engine surfaced.
	if !fsx.IsENOSPC(fsx.ErrDiskFull) {
		t.Error("IsENOSPC(ErrDiskFull) = false, want the sentinel to wrap ENOSPC")
	}
	if fsx.IsENOSPC(errors.New("permission denied")) {
		t.Error("IsENOSPC(unrelated error) = true, want false")
	}
	if fsx.IsENOSPC(nil) {
		t.Error("IsENOSPC(nil) = true, want false")
	}
}
