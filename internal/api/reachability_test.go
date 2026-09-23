// The reachability tests of T107: the tag and watch-folder tables over
// HTTP — doc 05 sections 8.2 and 15, and the table-reachability rule of
// doc 04 section 8.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/L-K-M/dl-tool/internal/jobs"
)

// reachTorrentPieces is one syntactically valid 20-byte pieces value —
// the same fixture shape the jobs watch tests hash offline.
const reachTorrentPieces = "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"

// reachTorrent is a complete single-file v1 torrent the scan can load.
const reachTorrent = "d8:announce35:http://tracker.example.com/announce" +
	"4:infod6:lengthi11e4:name9:hello.txt12:piece lengthi16384e6:pieces20:" + reachTorrentPieces + "ee"

// patchTag renames one tag with the test bearer credential; the name is
// percent-encoded the way doc 05 section 8.2 addresses it.
func (e *tasksTestEnv) patchTag(t *testing.T, name string, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Patch("/tags/"+url.PathEscape(name), body, "Authorization: Bearer "+e.bearer)
}

// deleteTag deletes one tag by name with the test bearer credential.
func (e *tasksTestEnv) deleteTag(t *testing.T, name string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Delete("/tags/"+url.PathEscape(name), "Authorization: Bearer "+e.bearer)
}

// createWatchFolder posts one folder with the test bearer credential.
func (e *tasksTestEnv) createWatchFolder(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Post("/watch-folders", body, "Authorization: Bearer "+e.bearer)
}

// getWatchFolders calls GET /watch-folders with the test bearer
// credential.
func (e *tasksTestEnv) getWatchFolders(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Get("/watch-folders", "Authorization: Bearer "+e.bearer)
}

// scanWatchFolder triggers the on-demand scan of one folder with the
// test bearer credential.
func (e *tasksTestEnv) scanWatchFolder(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Post("/watch-folders/"+id+"/scan", "Authorization: Bearer "+e.bearer)
}

// decodeWatchFolderBody decodes the flat folder object POST and PATCH
// return.
func decodeWatchFolderBody(t *testing.T, recorder *httptest.ResponseRecorder) WatchFolderView {
	t.Helper()

	var body WatchFolderView
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body
}

// decodeScanBody decodes the POST /watch-folders/{id}/scan result.
func decodeScanBody(t *testing.T, recorder *httptest.ResponseRecorder) jobs.ScanResult {
	t.Helper()

	var body jobs.ScanResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body
}

// taskTags reads the tag names one task carries through the API.
func (e *tasksTestEnv) taskTags(t *testing.T, id string) []string {
	t.Helper()

	response := e.getTask(t, id)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	var task TaskDTO
	if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode task body %q: %v", response.Body.String(), err)
	}

	return task.Tags
}

// createTaggedTask creates one task carrying tags and returns its id.
func (e *tasksTestEnv) createTaggedTask(t *testing.T, uri string, tags []string) string {
	t.Helper()

	response := e.createTasks(t, map[string]any{
		"uris": []string{uri},
		"tags": tags,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	created := decodeCreateBody(t, response).Created
	if len(created) != 1 {
		t.Fatalf("created %d tasks, want 1", len(created))
	}

	return created[0].ID
}

// TestRenameTagKeepsTasks pins the rename semantics of doc 05 section
// 8.2: the tag row is renamed in place, so every task carrying it
// carries the new name at once and no task row is touched.
func TestRenameTagKeepsTasks(t *testing.T) {
	env := newTasksTestEnv(t)

	ids := []string{
		env.createTaggedTask(t, mixedHTTPS, []string{"weekly"}),
		env.createTaggedTask(t, mixedFTP, []string{"weekly"}),
		env.createTaggedTask(t, mixedMagnet, []string{"weekly"}),
	}

	response := env.patchTag(t, "weekly", map[string]any{"new_name": "isos"})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	var renamed TagDTO
	if err := json.Unmarshal(response.Body.Bytes(), &renamed); err != nil {
		t.Fatalf("decode tag body %q: %v", response.Body.String(), err)
	}
	if renamed.Name != "isos" || renamed.TaskCount != 3 {
		t.Errorf("renamed = %+v, want {isos 3}", renamed)
	}

	for _, id := range ids {
		tags := env.taskTags(t, id)
		if len(tags) != 1 || tags[0] != "isos" {
			t.Errorf("task %s tags = %v, want [isos]", id, tags)
		}
	}
	if env.countTasks(t) != 3 {
		t.Errorf("%d tasks survived the rename, want 3", env.countTasks(t))
	}
}

// TestDeleteTagKeepsTasks pins the delete semantics of doc 05 section
// 8.2: the tag detaches from every task and the row goes, but NO TASK IS
// EVER DELETED.
func TestDeleteTagKeepsTasks(t *testing.T) {
	env := newTasksTestEnv(t)

	ids := []string{
		env.createTaggedTask(t, mixedHTTPS, []string{"weekly"}),
		env.createTaggedTask(t, mixedFTP, []string{"weekly"}),
		env.createTaggedTask(t, mixedMagnet, []string{"weekly"}),
	}

	response := env.deleteTag(t, "weekly")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusNoContent, response.Body.String())
	}

	for _, id := range ids {
		if tags := env.taskTags(t, id); len(tags) != 0 {
			t.Errorf("task %s tags = %v, want none after the delete", id, tags)
		}
	}
	if env.countTasks(t) != 3 {
		t.Errorf("%d tasks survived the delete, want 3", env.countTasks(t))
	}

	tags := decodeTagList(t, env.getTags(t))
	if len(tags) != 0 {
		t.Errorf("tags = %+v, want the deleted row gone", tags)
	}
}

// TestRenameOntoExistingIsConflict pins doc 05 section 8.2: a rename
// onto an existing name is 409 /problems/conflict, never a silent merge.
func TestRenameOntoExistingIsConflict(t *testing.T) {
	env := newTasksTestEnv(t)

	env.createTaggedTask(t, mixedHTTPS, []string{"iso"})
	env.seedTag(t, "weekly")

	response := env.patchTag(t, "weekly", map[string]any{"new_name": "iso"})
	assertProblem(t, response, http.StatusConflict, SlugConflict)

	// Both rows survive the refused write.
	tags := decodeTagList(t, env.getTags(t))
	if len(tags) != 2 {
		t.Fatalf("tags = %+v, want [iso weekly] intact", tags)
	}
}

// TestTagNameValidation pins the 422 of doc 05 section 8.2: an empty
// new_name, a name carrying , or /, and a percent-encoded separator in
// the path are all refused.
func TestTagNameValidation(t *testing.T) {
	env := newTasksTestEnv(t)

	env.createTaggedTask(t, mixedHTTPS, []string{"iso"})

	response := env.patchTag(t, "iso", map[string]any{"new_name": ""})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	response = env.patchTag(t, "iso", map[string]any{"new_name": "a,b"})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	response = env.patchTag(t, "iso", map[string]any{"new_name": "a/b"})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	// A percent-encoded separator in the path segment is the same 422 —
	// the decoded name carries the forbidden rune.
	response = env.api.Patch("/tags/a%2Fb", map[string]any{"new_name": "x"}, "Authorization: Bearer "+env.bearer)
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	response = env.api.Delete("/tags/a%2Fb", "Authorization: Bearer "+env.bearer)
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	// And a name no row carries is 404 on both verbs.
	response = env.patchTag(t, "ghost", map[string]any{"new_name": "spectre"})
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)

	response = env.deleteTag(t, "ghost")
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
}

// TestScanCreatesTaskImmediately pins FR-046 and doc 05 section 15:
// POST /watch-folders/{id}/scan runs the sweep synchronously, so a
// .torrent dropped into the folder becomes a task inside the request —
// without waiting for poll_interval_s.
func TestScanCreatesTaskImmediately(t *testing.T) {
	env := newTasksTestEnv(t)

	watchDir := filepath.Join(env.dataRoot, "watch")
	if err := os.MkdirAll(watchDir, 0o755); err != nil {
		t.Fatalf("make watch dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(watchDir, "hello.torrent"), []byte(reachTorrent), 0o644); err != nil {
		t.Fatalf("drop torrent: %v", err)
	}

	response := env.createWatchFolder(t, map[string]any{
		"path":        watchDir,
		"destination": env.dataRoot,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	folder := decodeWatchFolderBody(t, response)
	if !strings.HasPrefix(folder.ID, "wfd_") || folder.Path != resolvedPath(t, watchDir) {
		t.Fatalf("created folder = %+v, want a wfd_ id on the resolved watch dir", folder)
	}
	if !folder.Enabled || folder.PollIntervalS != 10 || folder.Category != nil {
		t.Errorf("created folder = %+v, want enabled with the 10s default and no category", folder)
	}

	response = env.scanWatchFolder(t, folder.ID)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	result := decodeScanBody(t, response)
	if result.Scanned != 1 || len(result.Created) != 1 || len(result.Skipped) != 0 {
		t.Fatalf("scan = %+v, want one scanned file and one created task", result)
	}

	// The task exists the moment the call returns — queued through the
	// ordinary creation path.
	response = env.getTask(t, result.Created[0])
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}

	// A second scan is idempotent: the loaded set answers already_loaded,
	// never a second task.
	response = env.scanWatchFolder(t, folder.ID)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	result = decodeScanBody(t, response)
	if len(result.Created) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Reason != jobs.SkipAlreadyLoaded {
		t.Fatalf("rescan = %+v, want the file skipped already_loaded", result)
	}
	if env.countTasks(t) != 1 {
		t.Errorf("%d tasks after two scans, want exactly 1", env.countTasks(t))
	}
}

// TestWatchFolderOutsideRootsRejected pins the jail of doc 05 section
// 15: a path or destination outside every configured root is 403
// /problems/path-rejected and writes no row.
func TestWatchFolderOutsideRootsRejected(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.createWatchFolder(t, map[string]any{
		"path":        "/etc",
		"destination": env.dataRoot,
	})
	assertProblem(t, response, http.StatusForbidden, SlugPathRejected)

	response = env.createWatchFolder(t, map[string]any{
		"path":        env.dataRoot,
		"destination": "/etc",
	})
	assertProblem(t, response, http.StatusForbidden, SlugPathRejected)

	response = env.getWatchFolders(t)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	var list struct {
		WatchFolders []WatchFolderView `json:"watch_folders"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode watch folder list %q: %v", response.Body.String(), err)
	}
	if len(list.WatchFolders) != 0 {
		t.Errorf("watch folders = %+v, want no row written", list.WatchFolders)
	}
}

// TestTagPathNameDecodesOnce locks the decode-exactly-once invariant:
// chi has already unescaped the path segment, so tagPathName must answer
// the decoded name — and leave a literal %25 untouched as %, not a
// second decode pass.
func TestTagPathNameDecodesOnce(t *testing.T) {
	name, err := tagPathName("50%25%20off")
	if err != nil {
		t.Fatalf("tagPathName: %v", err)
	}
	if name != "50% off" {
		t.Errorf("name = %q, want %q — one decode, not two", name, "50% off")
	}

	if _, err := tagPathName("a%2Fb"); err == nil {
		t.Error("tagPathName accepted a decoded /, want an invalid-name error")
	}
}

// TestWatchFolderValidation pins the 422s the schema and handler enforce
// on PATCH: a poll interval below 1 and an empty path or destination
// string are refused the same way they are on create.
func TestWatchFolderValidation(t *testing.T) {
	env := newTasksTestEnv(t)

	watchDir := filepath.Join(env.dataRoot, "watch")
	if err := os.MkdirAll(watchDir, 0o755); err != nil {
		t.Fatalf("make watch dir: %v", err)
	}
	response := env.createWatchFolder(t, map[string]any{
		"path":        watchDir,
		"destination": env.dataRoot,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	folder := decodeWatchFolderBody(t, response)

	for _, body := range []map[string]any{
		{"poll_interval_s": 0},
		{"poll_interval_s": -3},
		{"path": ""},
		{"destination": ""},
	} {
		response = env.api.Patch("/watch-folders/"+folder.ID, body, "Authorization: Bearer "+env.bearer)
		assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	}

	response = env.createWatchFolder(t, map[string]any{
		"path":            filepath.Join(env.dataRoot, "other"),
		"destination":     env.dataRoot,
		"poll_interval_s": 0,
	})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
}

// TestScanDeletedDirectoryIsUnprocessable pins the scan error contract:
// a folder whose directory went away since its creation is 422
// /problems/validation-failed — the operator repoints or deletes the
// row — never a 500.
func TestScanDeletedDirectoryIsUnprocessable(t *testing.T) {
	env := newTasksTestEnv(t)

	watchDir := filepath.Join(env.dataRoot, "watch")
	if err := os.MkdirAll(watchDir, 0o755); err != nil {
		t.Fatalf("make watch dir: %v", err)
	}
	response := env.createWatchFolder(t, map[string]any{
		"path":        watchDir,
		"destination": env.dataRoot,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	folder := decodeWatchFolderBody(t, response)

	if err := os.RemoveAll(watchDir); err != nil {
		t.Fatalf("remove watch dir: %v", err)
	}

	response = env.scanWatchFolder(t, folder.ID)
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
}
