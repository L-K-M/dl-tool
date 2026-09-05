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

	// The committed pool alone can exhaust the free answer: active tasks
	// already holding more than is currently free hold every new task,
	// even with a zero floor and a zero request.
	overcommitted := fsx.Reservation{FilesystemID: "dev:1", FreeBytes: 100, CommittedBytes: 300}
	if overcommitted.Admits(0) {
		t.Error("Admits(0) = true with commitments beyond the free answer, want held")
	}

	// An explicit zero floor disables that root's floor: everything free
	// is promisable.
	noFloor := fsx.Reservation{FilesystemID: "dev:1", FreeBytes: 100, MinFreeBytes: 0}
	if !noFloor.Admits(100) {
		t.Error("Admits(100) = false with a zero floor, want every free byte to promise")
	}

	// Garbage never admits: a negative free answer, commitment, floor or
	// request holds every task rather than promising bytes that do not exist.
	for name, bad := range map[string]fsx.Reservation{
		"negative commitment": {FilesystemID: "dev:1", FreeBytes: 100, CommittedBytes: -1},
		"negative free":       {FilesystemID: "dev:1", FreeBytes: -1},
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
	// The lookup cleans both sides: a stored key carrying a trailing slash
	// — a hand-edited row or a write path that skipped the loader — still
	// resolves, and so does a root spelled with one.
	if got := fsx.Floor(map[string]int64{"/data/": 4096}, "/data"); got != 4096 {
		t.Errorf("Floor({/data/:4096}, /data) = %d, want the explicit 4096 behind an unclean key", got)
	}
	if got := fsx.Floor(map[string]int64{"/data": 4096}, "/data/"); got != 4096 {
		t.Errorf("Floor({/data:4096}, /data/) = %d, want the explicit 4096 behind an unclean root", got)
	}
	// Two keys cleaning to the same root are a hand-edited row: the answer
	// must be deterministic — the largest matching floor, so the bad
	// duplicate never manufactures extra promisable space.
	if got := fsx.Floor(map[string]int64{"/data": 0, "/data/": 8192}, "/data"); got != 8192 {
		t.Errorf("Floor({/data:0, /data/:8192}, /data) = %d, want the strictest match 8192", got)
	}
	if got := fsx.Floor(map[string]int64{"/data": 8192, "/data/": 0}, "/data"); got != 8192 {
		t.Errorf("Floor({/data:8192, /data/:0}, /data) = %d, want the strictest match 8192 in any order", got)
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

	// A regular file mid-path climbs on ENOTDIR like a missing directory
	// — until the climb reaches the file itself: a destination whose
	// nearest existing ancestor is a regular file can never be created,
	// so the answer is an error, never a space promise for an uncreatable
	// path.
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, nil, 0o644); err != nil {
		t.Fatalf("write %s: %v", regular, err)
	}
	throughFile, err := fsx.FilesystemID(filepath.Join(regular, "not", "a", "dir"))
	if err == nil {
		t.Fatalf("FilesystemID through a file component = %q, want an error: the destination can never be created", throughFile)
	}
	// FreeSpace shares the climb: the same uncreatable destination must
	// fail closed here too, never answer with the file's filesystem.
	if _, err := fsx.FreeSpace(filepath.Join(regular, "not", "a", "dir")); err == nil {
		t.Error("FreeSpace through a file component = nil error, want the shared fail-closed climb")
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

// A relative path never reaches statfs: the guard rejects it up front,
// because climbing one would pool the destination against the working
// directory's mount — the exact mispooling the contract forbids.
func TestSpaceRejectsRelativePaths(t *testing.T) {
	if _, err := fsx.FreeSpace("relative/path"); err == nil {
		t.Error("FreeSpace(relative/path) = nil error, want rejection before any stat")
	}
	if _, err := fsx.FilesystemID("relative/path"); err == nil {
		t.Error("FilesystemID(relative/path) = nil error, want rejection before any stat")
	}
}
