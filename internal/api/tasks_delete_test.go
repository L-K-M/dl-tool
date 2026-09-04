package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/store"
)

// deleteTestEnv is one humatest server against a real migrated store whose
// data root is a temp directory, with the action stand-ins so a test can
// pin the engine calls of the delete path and fail them. It reuses the
// stand-ins and the seeding helpers of tasks_actions_test.go.
type deleteTestEnv struct {
	api      humatest.TestAPI
	db       *sqlx.DB
	aria2    *actionEngine
	dataRoot string
	bearer   string
}

func newDeleteTestEnv(t *testing.T) *deleteTestEnv {
	t.Helper()

	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	if err := os.Mkdir(dataRoot, 0o755); err != nil {
		t.Fatalf("make data root: %v", err)
	}

	configDir := filepath.Join(root, "config")
	db, err := store.Open(
		t.Context(),
		filepath.Join(configDir, "dl-tool.db"),
		filepath.Join(root, "backups"),
	)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	server, err := NewServer(
		&config.Config{ConfigDir: configDir, SessionTTL: time.Hour, DataRoots: []string{dataRoot}},
		db,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	aria2 := newActionEngine(engine.NameAria2, acceptsAria2Lanes)
	server.Engines.Register(aria2)

	user := seedUser(t, db)

	return &deleteTestEnv{
		api:      humatest.Wrap(t, server.API),
		db:       db,
		aria2:    aria2,
		dataRoot: dataRoot,
		bearer:   seedLiveAPIToken(t, db, user.ID),
	}
}

// seededFile is one recorded file of the delete fixtures: a task_files row
// plus size bytes on disk, unless alreadyGone leaves the path absent — the
// recorded-but-missing file the response counts in missing.
type seededFile struct {
	path        string // relative to the task's destination
	size        int64
	alreadyGone bool
}

// seedTask writes one downloading task held by aria2 at destination,
// straight through the store because POST /tasks can set no engine handle.
// mutate adjusts the row before the insert, for the fixtures that need a
// queue position or another column.
func (e *deleteTestEnv) seedTask(t *testing.T, destination string, mutate func(*store.Task)) string {
	t.Helper()

	ref := aria2GID
	task := store.Task{
		Engine:      engine.NameAria2,
		EngineRef:   &ref,
		SourceKind:  "http",
		Name:        "delete-fixture",
		State:       "downloading",
		Destination: destination,
	}
	if mutate != nil {
		mutate(&task)
	}
	created, err := store.NewTaskStore(e.db).Create(t.Context(), task)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}

	return created.ID
}

// seedFiles records the task's file list and creates the bytes on disk.
func (e *deleteTestEnv) seedFiles(t *testing.T, taskID, destination string, files []seededFile) {
	t.Helper()

	now := time.Now().UnixMilli()
	for i, file := range files {
		_, err := e.db.ExecContext(t.Context(),
			`INSERT INTO task_files (id, task_id, file_index, path, size_bytes, completed_bytes, selected, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			store.NewID(store.PrefixTaskFile), taskID, i, file.path, file.size, file.size, now, now)
		if err != nil {
			t.Fatalf("record task file %q: %v", file.path, err)
		}

		if file.alreadyGone {
			continue
		}

		abs := filepath.Join(destination, file.path)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("make parent of %q: %v", abs, err)
		}
		if err := os.WriteFile(abs, bytes.Repeat([]byte{1}, int(file.size)), 0o644); err != nil {
			t.Fatalf("write %q: %v", abs, err)
		}
	}
}

// seedDownloadedTask is the whole fixture: a task plus its recorded files,
// with the shared data root as the destination. mutate adjusts the task row
// before the insert, for the fixtures that need a queue position or another
// column.
func (e *deleteTestEnv) seedDownloadedTask(
	t *testing.T,
	files []seededFile,
	mutate func(*store.Task),
) (id, destination string) {
	t.Helper()

	destination = e.dataRoot
	id = e.seedTask(t, destination, mutate)
	e.seedFiles(t, id, destination, files)

	return id, destination
}

// deleteTask issues the authenticated DELETE, optionally with a query
// string ("delete_data=true").
func (e *deleteTestEnv) deleteTask(t *testing.T, id, query string) *httptest.ResponseRecorder {
	t.Helper()

	path := "/tasks/" + id
	if query != "" {
		path += "?" + query
	}

	return e.api.Delete(path, "Authorization: Bearer "+e.bearer)
}

// taskRow rereads the stored task row.
func (e *deleteTestEnv) taskRow(t *testing.T, id string) store.Task {
	t.Helper()

	task, err := store.NewTaskStore(e.db).Get(t.Context(), id)
	if err != nil {
		t.Fatalf("reread task %s: %v", id, err)
	}

	return task
}

// taskEventRows reads the task's events oldest first, straight from the
// store so the level and detail of every row can be pinned.
func (e *deleteTestEnv) taskEventRows(t *testing.T, id string) []store.TaskEvent {
	t.Helper()

	var events []store.TaskEvent
	err := e.db.SelectContext(t.Context(), &events,
		`SELECT id, task_id, at, level, code, message, detail_json, created_at, updated_at
FROM task_events
WHERE task_id = ?
ORDER BY at, id`, id)
	if err != nil {
		t.Fatalf("read events of %s: %v", id, err)
	}

	return events
}

// recordedFileCount counts the task's retained task_files rows, which the
// removal must never delete.
func (e *deleteTestEnv) recordedFileCount(t *testing.T, id string) int {
	t.Helper()

	var count int
	if err := e.db.GetContext(t.Context(), &count,
		`SELECT COUNT(*) FROM task_files WHERE task_id = ?`, id); err != nil {
		t.Fatalf("count files of %s: %v", id, err)
	}

	return count
}

// deleteOutcome mirrors the wire shape of DeleteTaskOutput's body —
// Huma flattens the anonymous Body struct into the response object — so
// the pinned JSON keys are exactly the ones of docs/05-api-contract.md
// section 5.6.
type deleteOutcome struct {
	Removed       bool  `json:"removed"`
	DeleteData    bool  `json:"delete_data"`
	FilesUnlinked int   `json:"files_unlinked"`
	BytesUnlinked int64 `json:"bytes_unlinked"`
	Missing       int   `json:"missing"`
}

// decodeDeleteBody decodes the outcome envelope of DELETE /tasks/{id}.
func decodeDeleteBody(t *testing.T, recorder *httptest.ResponseRecorder) deleteOutcome {
	t.Helper()

	var body deleteOutcome
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body
}

// assertTombstone pins step 6: state removed, engine_ref, ETA and queue
// position cleared, both rates zeroed.
func assertTombstone(t *testing.T, row store.Task) {
	t.Helper()

	if row.State != string(engine.StateRemoved) {
		t.Errorf("state = %q, want removed", row.State)
	}
	if row.EngineRef != nil {
		t.Errorf("engine_ref = %q, want cleared", *row.EngineRef)
	}
	if row.DownloadRate != 0 || row.UploadRate != 0 {
		t.Errorf("rates = %d/%d, want both zero", row.DownloadRate, row.UploadRate)
	}
	if row.ETASeconds != nil {
		t.Errorf("eta_seconds = %d, want cleared", *row.ETASeconds)
	}
	if row.QueuePosition != nil {
		t.Errorf("queue_position = %d, want the task out of the queue", *row.QueuePosition)
	}
}

// dataDeletedEvent returns the task's single task.data_deleted row, and
// fails the test when there is not exactly one.
func dataDeletedEvent(t *testing.T, events []store.TaskEvent) store.TaskEvent {
	t.Helper()

	var matches []store.TaskEvent
	for _, event := range events {
		if event.Code == "task.data_deleted" {
			matches = append(matches, event)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("task.data_deleted events = %d, want exactly one: %+v", len(matches), matches)
	}

	return matches[0]
}

// TestDeleteKeepsData pins the default: no flag, no enumeration, no
// unlink — the task is tombstoned and every byte stays.
func TestDeleteKeepsData(t *testing.T) {
	env := newDeleteTestEnv(t)

	id, destination := env.seedDownloadedTask(t, []seededFile{
		{path: "keep.iso", size: 4096},
		{path: "keep.torrent", size: 128},
	}, func(task *store.Task) {
		// In the queue, so the removal has to take the task out of it.
		position := int64(1)
		task.QueuePosition = &position
	})

	response := env.deleteTask(t, id, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}

	body := decodeDeleteBody(t, response)
	want := deleteOutcome{Removed: true}
	if body != want {
		t.Errorf("body = %+v, want %+v", body, want)
	}

	for _, name := range []string{"keep.iso", "keep.torrent"} {
		if _, err := os.Stat(filepath.Join(destination, name)); err != nil {
			t.Errorf("recorded file %q did not survive: %v", name, err)
		}
	}

	// Step 3 runs even without delete_data: Pause then Remove.
	calls := []string{"Pause aria2:" + aria2GID, "Remove aria2:" + aria2GID}
	if recorded := env.aria2.recorded(); !slices.Equal(recorded, calls) {
		t.Errorf("aria2 calls = %v, want %v", recorded, calls)
	}

	assertTombstone(t, env.taskRow(t, id))

	// task.removed alone; task.data_deleted is the delete_data outcome.
	codes := make([]string, 0, 1)
	for _, event := range env.taskEventRows(t, id) {
		codes = append(codes, event.Code)
	}
	if !slices.Equal(codes, []string{"task.removed"}) {
		t.Errorf("event codes = %v, want exactly [task.removed]", codes)
	}
}

// TestDeleteUnlinksRecordedFiles pins the delete_data path: exactly the
// recorded files are unlinked, a missing one is counted, the task's own
// directory goes while empty, and one warn-level task.data_deleted event
// records the counts.
func TestDeleteUnlinksRecordedFiles(t *testing.T) {
	env := newDeleteTestEnv(t)

	// The content sits in its own subfolder of the destination, the shape
	// create_subfolder and a torrent's own name produce.
	id, destination := env.seedDownloadedTask(t, []seededFile{
		{path: "ubuntu/ubuntu.iso", size: 5000},
		{path: "ubuntu/SHA256SUMS", size: 300},
		{path: "ubuntu/already-gone.txt", size: 100, alreadyGone: true},
	}, nil)

	response := env.deleteTask(t, id, "delete_data=true")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}

	body := decodeDeleteBody(t, response)
	if !body.Removed || !body.DeleteData {
		t.Errorf("flags = removed:%v delete_data:%v, want both true", body.Removed, body.DeleteData)
	}
	if body.FilesUnlinked != 2 || body.BytesUnlinked != 5300 || body.Missing != 1 {
		t.Errorf("counts = %d files, %d bytes, %d missing; want 2, 5300, 1",
			body.FilesUnlinked, body.BytesUnlinked, body.Missing)
	}

	// The recorded files are gone and the now-empty own directory with
	// them; the destination itself stays.
	if _, err := os.Stat(filepath.Join(destination, "ubuntu", "ubuntu.iso")); !os.IsNotExist(err) {
		t.Errorf("ubuntu.iso still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "ubuntu", "SHA256SUMS")); !os.IsNotExist(err) {
		t.Errorf("SHA256SUMS still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "ubuntu")); !os.IsNotExist(err) {
		t.Errorf("the task's own directory still present: %v", err)
	}
	if _, err := os.Stat(env.dataRoot); err != nil {
		t.Errorf("the shared data root did not survive: %v", err)
	}

	// Step 3 finished before the first unlink.
	calls := []string{"Pause aria2:" + aria2GID, "Remove aria2:" + aria2GID}
	if recorded := env.aria2.recorded(); !slices.Equal(recorded, calls) {
		t.Errorf("aria2 calls = %v, want %v", recorded, calls)
	}

	assertTombstone(t, env.taskRow(t, id))

	// The child rows are retained so the file history stays queryable.
	if count := env.recordedFileCount(t, id); count != 3 {
		t.Errorf("task_files rows = %d, want the retained 3", count)
	}

	// One warn-level task.data_deleted whose detail carries the file count
	// and the byte total, plus the task.removed the transition writes.
	event := dataDeletedEvent(t, env.taskEventRows(t, id))
	if event.Level != "warn" {
		t.Errorf("task.data_deleted level = %q, want warn", event.Level)
	}
	var detail struct {
		Files int   `json:"files"`
		Bytes int64 `json:"bytes"`
	}
	if err := json.Unmarshal([]byte(*event.DetailJSON), &detail); err != nil {
		t.Fatalf("decode detail %q: %v", *event.DetailJSON, err)
	}
	if detail.Files != 2 || detail.Bytes != 5300 {
		t.Errorf("detail = %+v, want 2 files and 5300 bytes", detail)
	}
}

// TestDeleteRejectsEscapingPath pins step 2: one recorded path that
// resolves outside the roots aborts the whole request with 403, before the
// engine is contacted and before any file is unlinked.
func TestDeleteRejectsEscapingPath(t *testing.T) {
	env := newDeleteTestEnv(t)

	id, destination := env.seedDownloadedTask(t, []seededFile{
		{path: "keep.iso", size: 4096},
		{path: "../../../../etc/passwd", size: 1, alreadyGone: true},
	}, nil)

	response := env.deleteTask(t, id, "delete_data=true")
	problem := assertProblem(t, response, http.StatusForbidden, SlugPathRejected)

	escaping := filepath.Join(destination, "..", "..", "..", "..", "etc", "passwd")
	found := false
	for _, field := range problem.Errors {
		if value, ok := field.Value.(string); ok && value == filepath.Clean(escaping) {
			found = true
		}
	}
	if !found {
		t.Errorf("errors[] = %+v, want one entry naming %q", problem.Errors, filepath.Clean(escaping))
	}

	env.aria2.assertNoCalls(t)
	if _, err := os.Stat(filepath.Join(destination, "keep.iso")); err != nil {
		t.Errorf("the in-root file was touched: %v", err)
	}
	if row := env.taskRow(t, id); row.State != string(engine.StateDownloading) {
		t.Errorf("state = %q, want unchanged downloading", row.State)
	}
	if events := env.taskEventRows(t, id); len(events) != 0 {
		t.Errorf("events = %v, want none", events)
	}
}

// TestDeleteRefusesWhenEngineDown pins step 3's failure: an unavailable
// engine is 503, the task is not removed and nothing is unlinked.
func TestDeleteRefusesWhenEngineDown(t *testing.T) {
	env := newDeleteTestEnv(t)
	env.aria2.removeErr = engine.ErrUnavailable

	id, destination := env.seedDownloadedTask(t, []seededFile{{path: "keep.iso", size: 4096}}, nil)

	response := env.deleteTask(t, id, "delete_data=true")
	assertProblem(t, response, http.StatusServiceUnavailable, SlugEngineUnavailable)

	if _, err := os.Stat(filepath.Join(destination, "keep.iso")); err != nil {
		t.Errorf("the recorded file was unlinked despite the 503: %v", err)
	}

	row := env.taskRow(t, id)
	if row.State != string(engine.StateDownloading) {
		t.Errorf("state = %q, want unchanged downloading", row.State)
	}
	if row.EngineRef == nil || *row.EngineRef != aria2GID {
		t.Errorf("engine_ref = %v, want the retained handle", row.EngineRef)
	}
	if events := env.taskEventRows(t, id); len(events) != 0 {
		t.Errorf("events = %v, want none", events)
	}
}

// TestDeleteRejectsBothFlags pins the mutual exclusion: both flags true is
// 422 before any other work — no row, no engine call, no file touched.
func TestDeleteRejectsBothFlags(t *testing.T) {
	env := newDeleteTestEnv(t)

	id, destination := env.seedDownloadedTask(t, []seededFile{{path: "keep.iso", size: 4096}}, nil)

	response := env.deleteTask(t, id, "delete_data=true&force_complete=true")
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	env.aria2.assertNoCalls(t)
	if _, err := os.Stat(filepath.Join(destination, "keep.iso")); err != nil {
		t.Errorf("the recorded file was touched: %v", err)
	}
	if row := env.taskRow(t, id); row.State != string(engine.StateDownloading) {
		t.Errorf("state = %q, want unchanged downloading", row.State)
	}
}

// TestDeleteForceComplete pins the second flag: the handle is dropped with
// the data retained — exactly like the bulk action of the same name — the
// task moves to completed, and nothing is unlinked.
func TestDeleteForceComplete(t *testing.T) {
	env := newDeleteTestEnv(t)

	id, destination := env.seedDownloadedTask(t, []seededFile{{path: "keep.iso", size: 4096}}, nil)

	response := env.deleteTask(t, id, "force_complete=true")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}

	if body := decodeDeleteBody(t, response); body.Removed || body.DeleteData ||
		body.FilesUnlinked != 0 || body.BytesUnlinked != 0 || body.Missing != 0 {
		t.Errorf("body = %+v, want the all-zero non-removal outcome", body)
	}

	if calls := env.aria2.recorded(); !slices.Equal(calls, []string{"Remove aria2:" + aria2GID}) {
		t.Errorf("aria2 calls = %v, want exactly one Remove", calls)
	}
	if _, err := os.Stat(filepath.Join(destination, "keep.iso")); err != nil {
		t.Errorf("the data was not retained: %v", err)
	}

	row := env.taskRow(t, id)
	if row.State != string(engine.StateCompleted) {
		t.Errorf("state = %q, want completed", row.State)
	}

	codes := make([]string, 0, 1)
	for _, event := range env.taskEventRows(t, id) {
		codes = append(codes, event.Code)
	}
	if !slices.Equal(codes, []string{eventTaskForceCompleted}) {
		t.Errorf("event codes = %v, want [task.force_completed]", codes)
	}
}
