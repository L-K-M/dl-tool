package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRecordedFile creates one file of the delete fixtures: a path under
// dir holding content, so the assertions can read it back byte-for-byte.
func writeRecordedFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("make parent of %q: %v", path, err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}

	return path
}

// TestHardlinkedCopySurvives pins the single /data mount's normal outcome
// (ADR-0012): a recorded file hardlinked into a media library is one
// inode with two names, so unlinking dl-tool's name leaves the library
// copy byte-for-byte intact — not a partial deletion.
func TestHardlinkedCopySurvives(t *testing.T) {
	root := t.TempDir()
	taskDir := filepath.Join(root, "task")
	library := filepath.Join(root, "library")
	if err := os.MkdirAll(library, 0o755); err != nil {
		t.Fatalf("make library: %v", err)
	}

	content := []byte("the payload the library keeps")
	recorded := writeRecordedFile(t, taskDir, "movie.mkv", content)
	linked := filepath.Join(library, "movie.mkv")
	if err := os.Link(recorded, linked); err != nil {
		t.Fatalf("hardlink %q -> %q: %v", recorded, linked, err)
	}

	result, err := DeleteData(t.Context(), []string{root}, taskDir, []Target{
		{Path: recorded, Bytes: int64(len(content))},
	})
	if err != nil {
		t.Fatalf("DeleteData: %v", err)
	}
	if !result.Deleted || !result.DeleteData {
		t.Errorf("result = %+v, want a deletion that ran", result)
	}
	if result.FilesUnlinked != 1 || result.BytesUnlinked != int64(len(content)) || result.Missing != 0 {
		t.Errorf("counts = %d files, %d bytes, %d missing; want 1, %d, 0",
			result.FilesUnlinked, result.BytesUnlinked, result.Missing, len(content))
	}

	// dl-tool's name is gone, and the emptied task directory went with it.
	if _, err := os.Stat(recorded); !os.IsNotExist(err) {
		t.Errorf("recorded path still present: %v", err)
	}
	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Errorf("empty task directory still present: %v", err)
	}

	// The library copy opens with its original contents.
	kept, err := os.ReadFile(linked)
	if err != nil {
		t.Fatalf("hardlinked copy does not open: %v", err)
	}
	if string(kept) != string(content) {
		t.Errorf("hardlinked copy = %q, want %q", kept, content)
	}
}

// TestOneEscapingTargetAbortsAll pins the re-check's ordering: it runs to
// completion before the first unlink, so one target resolving outside the
// roots — here through a symlink, proving resolution happens before the
// check — fails the whole call with ErrPathRejected naming it and the
// valid targets of the same batch stay on disk.
func TestOneEscapingTargetAbortsAll(t *testing.T) {
	root := t.TempDir()
	taskDir := filepath.Join(root, "task")
	valid := writeRecordedFile(t, taskDir, "keep.iso", []byte("keep"))

	outside := t.TempDir()
	secret := writeRecordedFile(t, outside, "secret.bin", []byte("not dl-tool's"))
	link := filepath.Join(taskDir, "link.bin")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink %q -> %q: %v", link, secret, err)
	}

	result, err := DeleteData(t.Context(), []string{root}, taskDir, []Target{
		{Path: valid, Bytes: 4},
		{Path: link, Bytes: 13},
	})
	if !errors.Is(err, ErrPathRejected) {
		t.Fatalf("err = %v, want ErrPathRejected", err)
	}
	if !strings.Contains(err.Error(), link) {
		t.Errorf("err = %q, want it naming the escaping path %q", err, link)
	}
	if result != (DeleteResult{}) {
		t.Errorf("result = %+v, want the zero result on rejection", result)
	}

	// Nothing at all is unlinked: not the escaping link, not the target
	// it pointed at, and not the valid file of the same batch.
	for _, path := range []string{valid, link, secret} {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("%q was touched by the aborted delete: %v", path, err)
		}
	}
}

// TestOnlyRecordedFilesUnlinked pins the bounded operation: task_files is
// the only source of targets, so a file sitting in the task's own
// directory without a row is never seen — no glob, no directory walk —
// and the directory it keeps non-empty stays in place.
func TestOnlyRecordedFilesUnlinked(t *testing.T) {
	root := t.TempDir()
	taskDir := filepath.Join(root, "task")

	recorded := writeRecordedFile(t, taskDir, "recorded.bin", []byte("recorded"))
	unrecorded := writeRecordedFile(t, taskDir, "unrecorded.bin", []byte("unrecorded"))

	result, err := DeleteData(t.Context(), []string{root}, taskDir, []Target{
		{Path: recorded, Bytes: 8},
	})
	if err != nil {
		t.Fatalf("DeleteData: %v", err)
	}
	if result.FilesUnlinked != 1 || result.Missing != 0 {
		t.Errorf("counts = %d files, %d missing; want 1, 0", result.FilesUnlinked, result.Missing)
	}

	if _, err := os.Stat(recorded); !os.IsNotExist(err) {
		t.Errorf("recorded file still present: %v", err)
	}
	kept, err := os.ReadFile(unrecorded)
	if err != nil {
		t.Fatalf("unrecorded file was unlinked: %v", err)
	}
	if string(kept) != "unrecorded" {
		t.Errorf("unrecorded file = %q, want its original contents", kept)
	}
	if _, err := os.Stat(taskDir); err != nil {
		t.Errorf("the non-empty task directory was removed: %v", err)
	}
}

// TestUnlinkFailureContinues pins the tolerance the delete path had before
// the executor: an unlink that errors for a reason other than ENOENT is
// logged and the pass continues — the remaining targets are still unlinked
// and the call still succeeds, because the operation is irreversible and
// the counts report what actually happened. The failing target here is a
// recorded path that names a non-empty directory, which unlink refuses.
func TestUnlinkFailureContinues(t *testing.T) {
	root := t.TempDir()
	taskDir := filepath.Join(root, "task")

	present := writeRecordedFile(t, taskDir, "present.bin", []byte("present"))
	// A recorded path that is a non-empty directory: os.Remove refuses it
	// with ENOTEMPTY, the "other failure" branch of the unlink loop.
	blocked := filepath.Join(taskDir, "blocked")
	writeRecordedFile(t, blocked, "inside.bin", []byte("inside"))

	result, err := DeleteData(t.Context(), []string{root}, taskDir, []Target{
		{Path: blocked, Bytes: 0},
		{Path: present, Bytes: 7},
	})
	if err != nil {
		t.Fatalf("DeleteData returned an error for a failed unlink: %v", err)
	}
	if result.FilesUnlinked != 1 || result.Missing != 0 {
		t.Errorf("counts = %d files, %d missing; want 1, 0 — the failed unlink must not abort the pass",
			result.FilesUnlinked, result.Missing)
	}
	if _, err := os.Stat(present); !os.IsNotExist(err) {
		t.Errorf("the later target was not unlinked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(blocked, "inside.bin")); err != nil {
		t.Errorf("the refused target's content was touched: %v", err)
	}
	// The task directory still holds the refused directory, so it stays.
	if _, err := os.Stat(taskDir); err != nil {
		t.Errorf("the non-empty task directory was removed: %v", err)
	}
}

// TestMissingFileCounted pins the recorded-but-gone file: it is counted in
// missing, not an error, and the recorded byte total is summed from the
// files that were actually unlinked.
func TestMissingFileCounted(t *testing.T) {
	root := t.TempDir()
	taskDir := filepath.Join(root, "task")

	present := writeRecordedFile(t, taskDir, "present.bin", []byte("present"))
	gone := filepath.Join(taskDir, "gone.bin")

	result, err := DeleteData(t.Context(), []string{root}, taskDir, []Target{
		{Path: present, Bytes: 7},
		{Path: gone, Bytes: 100},
	})
	if err != nil {
		t.Fatalf("DeleteData: %v", err)
	}
	if result.FilesUnlinked != 1 || result.BytesUnlinked != 7 || result.Missing != 1 {
		t.Errorf("counts = %d files, %d bytes, %d missing; want 1, 7, 1",
			result.FilesUnlinked, result.BytesUnlinked, result.Missing)
	}
}
