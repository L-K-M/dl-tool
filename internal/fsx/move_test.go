//go:build linux

package fsx_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/fsx"
	"github.com/L-K-M/dl-tool/internal/jobs"
	"github.com/L-K-M/dl-tool/internal/store"
)

// crossFSRoot returns a directory on a different filesystem than anchor —
// /dev/shm is a tmpfs beside whatever holds the test's temp dir — so the
// rename attempt fails with EXDEV and the copy-verify-delete path runs for
// real. The test skips where no second filesystem is mounted.
func crossFSRoot(t *testing.T, anchor string) string {
	t.Helper()

	for _, candidate := range []string{"/dev/shm", "/run"} {
		info, err := os.Stat(candidate)
		if err != nil || !info.IsDir() {
			continue
		}
		dir, err := os.MkdirTemp(candidate, "dl-tool-move-test-*")
		if err != nil {
			continue
		}
		same, err := fsx.SameFilesystem(anchor, dir)
		if err != nil || same {
			_ = os.RemoveAll(dir)
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })

		return dir
	}

	t.Skip("no second filesystem available for an EXDEV move")
	return ""
}

// progressRecorder collects the onProgress calls of one Move.
type progressRecorder struct {
	calls [][2]int64
}

func (r *progressRecorder) record(copied, total int64) {
	r.calls = append(r.calls, [2]int64{copied, total})
}

// stagingLitter lists any .dl-tool-move-* entries left in dir.
func stagingLitter(t *testing.T, dir string) []string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(dir, ".dl-tool-move-*"))
	require.NoError(t, err)

	return matches
}

func TestSameFilesystemUsesRename(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "payload.bin")
	dst := filepath.Join(dir, "delivered.bin")
	body := []byte("same-filesystem payload")
	require.NoError(t, os.WriteFile(src, body, 0o644))

	progress := &progressRecorder{}
	require.NoError(t, fsx.Move(t.Context(), src, dst, progress.record))

	// The rename path copied nothing: the callback never ran, the payload
	// sits at dst byte-for-byte and src is gone.
	assert.Empty(t, progress.calls, "a same-filesystem move must not report copy progress")
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, body, got)
	_, err = os.Stat(src)
	assert.True(t, errors.Is(err, os.ErrNotExist), "src must be gone after the rename")
	assert.Empty(t, stagingLitter(t, dir))
}

func TestEXDEVFallsBackToCopy(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := crossFSRoot(t, srcRoot)

	// A tree with a nested directory, so the fallback has to recreate
	// structure, not just bytes.
	src := filepath.Join(srcRoot, "payload")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "sub", "deep"), 0o755))
	files := map[string][]byte{
		"one.bin":            []byte("first file body"),
		"sub/two.bin":        []byte("second file body, a little longer"),
		"sub/deep/three.bin": []byte("third file body"),
	}
	for rel, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(src, rel), body, 0o644))
	}
	dst := filepath.Join(dstRoot, "payload")

	progress := &progressRecorder{}
	require.NoError(t, fsx.Move(t.Context(), src, dst, progress.record))

	// The destination holds the whole tree byte-for-byte at the documented
	// modes, and the source is gone — verification ran before the removal.
	for rel, body := range files {
		got, err := os.ReadFile(filepath.Join(dst, rel))
		require.NoError(t, err)
		assert.Equal(t, body, got, "copied bytes differ at %s", rel)
		info, err := os.Stat(filepath.Join(dst, rel))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "copied file mode at %s", rel)
	}
	info, err := os.Stat(filepath.Join(dst, "sub"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "copied dir mode")

	_, err = os.Stat(src)
	assert.True(t, errors.Is(err, os.ErrNotExist), "src must be gone after a verified copy")
	assert.Empty(t, stagingLitter(t, dstRoot))

	// Progress reported a copied count climbing against the byte total —
	// never decreasing, never above one report a second. The throttle may
	// legitimately drop the trailing tick, so the last report need not
	// equal the total; the handler lands it.
	require.NotEmpty(t, progress.calls, "a cross-filesystem move must report progress")
	var wantTotal int64
	for _, body := range files {
		wantTotal += int64(len(body))
	}
	var previous int64 = -1
	for _, call := range progress.calls {
		assert.Equal(t, wantTotal, call[1], "every report carries the same total")
		assert.GreaterOrEqual(t, call[0], int64(0))
		assert.LessOrEqual(t, call[0], wantTotal)
		if previous >= 0 {
			assert.GreaterOrEqual(t, call[0], previous, "copied must not decrease")
		}
		previous = call[0]
	}
}

func TestVerifyFailureKeepsSource(t *testing.T) {
	// A procfs file reports size 0 to stat but yields real bytes on read:
	// the copy writes a non-empty staged file that can never match the
	// recorded zero — a deterministic size mismatch for the verify step.
	src := "/proc/version"
	srcInfo, err := os.Stat(src)
	if err != nil || srcInfo.Size() != 0 {
		t.Skipf("procfs file %s does not report size 0", src)
	}
	dstRoot := t.TempDir()
	dst := filepath.Join(dstRoot, "version")

	err = fsx.Move(t.Context(), src, dst, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, fsx.ErrVerifyFailed), "want ErrVerifyFailed, got %v", err)

	// The source is byte-for-byte untouched and the staging path is gone.
	before, err := os.ReadFile(src)
	require.NoError(t, err)
	assert.NotEmpty(t, before)
	_, err = os.Stat(dst)
	assert.True(t, errors.Is(err, os.ErrNotExist), "dst must not exist after a failed verify")
	assert.Empty(t, stagingLitter(t, dstRoot))
}

func TestCancelledMoveKeepsSource(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := crossFSRoot(t, srcRoot)

	src := filepath.Join(srcRoot, "payload")
	require.NoError(t, os.MkdirAll(src, 0o755))
	bodies := map[string][]byte{
		"a.bin": []byte(strings.Repeat("a", 1<<20)),
		"b.bin": []byte(strings.Repeat("b", 1<<20)),
	}
	for rel, body := range bodies {
		require.NoError(t, os.WriteFile(filepath.Join(src, rel), body, 0o644))
	}
	dst := filepath.Join(dstRoot, "payload")

	// Cancel inside the first progress report — mid-copy, not before the
	// attempt — so the abort exercises the copy loop's cleanup. The
	// contract reports the first write immediately, so the callback is
	// guaranteed to fire on any non-empty copy.
	ctx, cancel := context.WithCancel(t.Context())
	fired := false
	err := fsx.Move(ctx, src, dst, func(copied, total int64) {
		fired = true
		cancel()
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "want context.Canceled, got %v", err)
	assert.True(t, fired, "expected at least one progress report during the copy")

	// The source tree is intact and no staging path survives beside dst.
	for rel, body := range bodies {
		got, err := os.ReadFile(filepath.Join(src, rel))
		require.NoError(t, err)
		assert.Equal(t, body, got)
	}
	_, err = os.Stat(dst)
	assert.True(t, errors.Is(err, os.ErrNotExist), "a cancelled move must not deliver dst")
	assert.Empty(t, stagingLitter(t, dstRoot))
}

// moveTestDB opens a migrated database for the handler tests.
func moveTestDB(t *testing.T) *sqlx.DB {
	t.Helper()

	dir := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(dir, "dl-tool.db"), filepath.Join(dir, "backups"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	return db
}

// moveJob builds the job row the worker would hand the handler.
func moveJob(t *testing.T, taskID, src, dst string) store.Job {
	t.Helper()

	payload, err := json.Marshal(map[string]string{"task_id": taskID, "src": src, "dst": dst})
	require.NoError(t, err)

	return store.Job{ID: "job_move_" + taskID, Kind: jobs.JobKindMove, TaskID: &taskID, PayloadJSON: string(payload)}
}

func moveTaskEvents(t *testing.T, db *sqlx.DB, taskID string) []string {
	t.Helper()

	var codes []string
	require.NoError(t, db.SelectContext(
		t.Context(), &codes,
		`SELECT code FROM task_events WHERE task_id = ? ORDER BY at`, taskID,
	))

	return codes
}

func TestTaskPassesThroughMoving(t *testing.T) {
	db := moveTestDB(t)
	srcRoot := t.TempDir()
	dstRoot := crossFSRoot(t, srcRoot)

	src := filepath.Join(srcRoot, "payload.mkv")
	body := []byte("the completed payload")
	require.NoError(t, os.WriteFile(src, body, 0o644))
	dst := filepath.Join(dstRoot, "payload.mkv")

	// The test filesystem is small, so the destination root's floor is set
	// to 0 — an explicit entry disables the default 2 GiB head-room and
	// lets the reservation admit the payload.
	setMinFreeSpace(t, db, dstRoot, 0)

	tasks := store.NewTaskStore(db)
	task, err := tasks.Create(t.Context(), store.Task{
		Engine:         "aria2",
		SourceKind:     "http",
		Name:           "payload.mkv",
		State:          "completed",
		Destination:    dstRoot,
		ContentPath:    &src,
		TotalBytes:     ptrInt64(int64(len(body))),
		CompletedBytes: int64(len(body)),
	})
	require.NoError(t, err)

	handler := jobs.NewMoveHandler(db, tasks, []string{dstRoot})
	require.NoError(t, handler.Handle(t.Context(), moveJob(t, task.ID, src, dst)))

	// The task is back to completed with content_path at the new location,
	// and the event log proves it passed through moving: the move.started
	// row is the entry, move.completed the return.
	reloaded, err := tasks.Get(t.Context(), task.ID)
	require.NoError(t, err)
	assert.Equal(t, "completed", reloaded.State)
	require.NotNil(t, reloaded.ContentPath)
	assert.Equal(t, dst, *reloaded.ContentPath)

	codes := moveTaskEvents(t, db, task.ID)
	assert.Contains(t, codes, "postprocess.move.started")
	assert.Contains(t, codes, "postprocess.move.completed")

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, body, got)
	_, err = os.Stat(src)
	assert.True(t, errors.Is(err, os.ErrNotExist))
}

func TestMoveRefusedByDiskFloorPauses(t *testing.T) {
	db := moveTestDB(t)
	srcRoot := t.TempDir()
	dstRoot := crossFSRoot(t, srcRoot)

	src := filepath.Join(srcRoot, "payload.bin")
	require.NoError(t, os.WriteFile(src, []byte("payload bytes"), 0o644))
	dst := filepath.Join(dstRoot, "payload.bin")

	// A floor above any real free-space answer refuses the reservation —
	// the deterministic shape of "destination has no room".
	setMinFreeSpace(t, db, dstRoot, int64(1)<<62)

	tasks := store.NewTaskStore(db)
	task, err := tasks.Create(t.Context(), store.Task{
		Engine:      "aria2",
		SourceKind:  "http",
		Name:        "payload.bin",
		State:       "completed",
		Destination: dstRoot,
		ContentPath: &src,
	})
	require.NoError(t, err)

	handler := jobs.NewMoveHandler(db, tasks, []string{dstRoot})
	require.NoError(t, handler.Handle(t.Context(), moveJob(t, task.ID, src, dst)))

	// The task is parked with disk_full and nothing was deleted: src holds
	// its bytes, dst does not exist, no staging litter survives.
	reloaded, err := tasks.Get(t.Context(), task.ID)
	require.NoError(t, err)
	assert.Equal(t, "paused", reloaded.State)
	require.NotNil(t, reloaded.ErrorCode)
	assert.Equal(t, "disk_full", *reloaded.ErrorCode)

	got, err := os.ReadFile(src)
	require.NoError(t, err)
	assert.Equal(t, []byte("payload bytes"), got)
	_, err = os.Stat(dst)
	assert.True(t, errors.Is(err, os.ErrNotExist))
	assert.Empty(t, stagingLitter(t, dstRoot))
}

func TestSameFSMoveThroughHandler(t *testing.T) {
	db := moveTestDB(t)
	root := t.TempDir()

	src := filepath.Join(root, "incomplete", "payload.bin")
	dst := filepath.Join(root, "done", "payload.bin")
	body := []byte("same-filesystem payload")
	require.NoError(t, os.MkdirAll(filepath.Dir(src), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	require.NoError(t, os.WriteFile(src, body, 0o644))

	tasks := store.NewTaskStore(db)
	task, err := tasks.Create(t.Context(), store.Task{
		Engine:      "aria2",
		SourceKind:  "http",
		Name:        "payload.bin",
		State:       "completed",
		Destination: filepath.Dir(dst),
		ContentPath: &src,
	})
	require.NoError(t, err)

	// The rename path needs no floor override: the space pre-check runs
	// only when source and destination live on different filesystems.
	handler := jobs.NewMoveHandler(db, tasks, []string{root})
	require.NoError(t, handler.Handle(t.Context(), moveJob(t, task.ID, src, dst)))

	reloaded, err := tasks.Get(t.Context(), task.ID)
	require.NoError(t, err)
	assert.Equal(t, "completed", reloaded.State)
	require.NotNil(t, reloaded.ContentPath)
	assert.Equal(t, dst, *reloaded.ContentPath)

	codes := moveTaskEvents(t, db, task.ID)
	assert.Contains(t, codes, "postprocess.move.started")
	assert.Contains(t, codes, "postprocess.move.completed")

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, body, got)
	_, err = os.Stat(src)
	assert.True(t, errors.Is(err, os.ErrNotExist))
	assert.Empty(t, stagingLitter(t, filepath.Dir(dst)))
}

// setMinFreeSpace writes the destination root's min_free_space floor the
// move handler's pre-check reads.
func setMinFreeSpace(t *testing.T, db *sqlx.DB, root string, floor int64) {
	t.Helper()

	value, err := json.Marshal(map[string]int64{root: floor})
	require.NoError(t, err)

	_, err = db.ExecContext(
		t.Context(),
		`INSERT OR REPLACE INTO settings (id, key, value_json, created_at, updated_at)
			VALUES ('set_test_min_free_space', 'min_free_space', ?, 0, 0)`,
		string(value),
	)
	require.NoError(t, err)
}

func ptrInt64(v int64) *int64 { return &v }
