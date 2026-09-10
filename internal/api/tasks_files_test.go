package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/store"
)

// setFilesCall is one recorded Engine.SetFiles invocation. selected keeps
// the nil-versus-empty distinction, which the deselect path relies on.
type setFilesCall struct {
	id         string
	selected   []int
	priorities map[int]int
}

// filesEngine is the action stand-in with the two file calls of T032
// added: a canned listing whose failures a test can pin, and a SetFiles
// that records its arguments.
type filesEngine struct {
	*actionEngine

	capabilities []engine.Capability
	files        []engine.FileEntry
	filesErr     error
	setErr       error

	mu       sync.Mutex
	setCalls []setFilesCall
	filesIDs []string
}

func newFilesEngine(name string, accepts func(string) bool, capabilities []engine.Capability) *filesEngine {
	return &filesEngine{
		actionEngine: newActionEngine(name, accepts),
		capabilities: capabilities,
		files:        defaultFilesListing(),
	}
}

func (e *filesEngine) Capabilities() []engine.Capability { return e.capabilities }

func (e *filesEngine) Files(_ context.Context, id string) ([]engine.FileEntry, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.filesIDs = append(e.filesIDs, id)

	if e.filesErr != nil {
		return nil, e.filesErr
	}

	return append([]engine.FileEntry(nil), e.files...), nil
}

func (e *filesEngine) SetFiles(_ context.Context, id string, selected []int, priorities map[int]int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.setCalls = append(e.setCalls, setFilesCall{id: id, selected: selected, priorities: priorities})

	if e.setErr != nil {
		return e.setErr
	}

	// The daemon mirrors the accepted change into its listing; so does the
	// stand-in, so a GET after a PATCH observes what the engine now
	// reports. A non-nil selected names the complete desired selection,
	// so it reselects its indices and deselects every other.
	if selected != nil {
		keep := make(map[int]bool, len(selected))
		for _, index := range selected {
			keep[index] = true
		}
		for i, entry := range e.files {
			e.files[i].Selected = keep[entry.Index]
		}
	}
	for index, priority := range priorities {
		for i, entry := range e.files {
			if entry.Index != index {
				continue
			}
			e.files[i].Selected = priority != 0
			e.files[i].Priority = &priority
		}
	}

	return nil
}

// recordedSetCalls returns every SetFiles invocation so far.
func (e *filesEngine) recordedSetCalls() []setFilesCall {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]setFilesCall(nil), e.setCalls...)
}

// recordedFilesIDs returns every listing's engine task id, in call order.
func (e *filesEngine) recordedFilesIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]string(nil), e.filesIDs...)
}

// setListing replaces the canned listing under the engine's mutex, so a
// test can pin the shape a GET observes without racing the handler.
func (e *filesEngine) setListing(files []engine.FileEntry) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.files = files
}

// failListing makes every Files call answer err under the mutex.
func (e *filesEngine) failListing(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.filesErr = err
}

// failSet makes every SetFiles call answer err under the mutex.
func (e *filesEngine) failSet(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.setErr = err
}

// assertNoSetCalls fails when the engine was asked to change a selection.
func (e *filesEngine) assertNoSetCalls(t *testing.T) {
	t.Helper()

	if calls := e.recordedSetCalls(); len(calls) != 0 {
		t.Errorf("%s engine SetFiles was called: %+v", e.name, calls)
	}
}

// filesOtherHash is a second engine handle, distinct so a second live
// qbittorrent task can coexist with the first under the unique index.
const filesOtherHash = "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"

// defaultFilesListing is the three-file listing of doc 05 section 5.8's
// shape: all three files selected — two at normal priority, one at high.
func defaultFilesListing() []engine.FileEntry {
	return []engine.FileEntry{
		{Index: 0, Path: "ubuntu-26.04-desktop-amd64.iso", Size: 1000, Completed: 500, Selected: true, Priority: intPtr(1)},
		{Index: 1, Path: "extras/SHA256SUMS", Size: 100, Completed: 100, Selected: true, Priority: intPtr(6)},
		{Index: 2, Path: "extras/sample.mkv", Size: 200, Completed: 0, Selected: true, Priority: intPtr(1)},
	}
}

// aria2FilesListing is the aria2 shape: no priorities at all, selection
// alone, one file already deselected.
func aria2FilesListing() []engine.FileEntry {
	return []engine.FileEntry{
		{Index: 0, Path: "video.mp4", Size: 800, Completed: 400, Selected: true},
		{Index: 1, Path: "audio.m4a", Size: 100, Completed: 0, Selected: false},
	}
}

// filesTestEnv is the actions env with the two file-aware stand-ins: the
// qbittorrent one declares both file capabilities, the aria2 one only
// per_file_select — the shape PATCH must refuse.
type filesTestEnv struct {
	*actionsTestEnv
	qbittorrent *filesEngine
	aria2       *filesEngine
}

func newFilesTestEnv(t *testing.T) *filesTestEnv {
	t.Helper()

	aria2 := newFilesEngine(engine.NameAria2, acceptsAria2Lanes, []engine.Capability{engine.CapPerFileSelect})
	aria2.setListing(aria2FilesListing())
	qbittorrent := newFilesEngine(engine.NameQBittorrent, acceptsBitTorrent,
		[]engine.Capability{engine.CapPerFileSelect, engine.CapPerFilePriority})

	return &filesTestEnv{
		actionsTestEnv: newActionsTestEnvWithEngines(t, aria2, qbittorrent),
		qbittorrent:    qbittorrent,
		aria2:          aria2,
	}
}

// seedQBTTask writes one downloading task the qbittorrent stand-in holds.
func (e *filesTestEnv) seedQBTTask(t *testing.T) string {
	t.Helper()

	ref := qbtHash

	return e.seedActionTask(t, func(task *store.Task) {
		task.Engine = engine.NameQBittorrent
		task.EngineRef = &ref
	})
}

// seedAria2Task writes one downloading task the aria2 stand-in holds.
func (e *filesTestEnv) seedAria2Task(t *testing.T) string {
	t.Helper()

	ref := aria2GID

	return e.seedActionTask(t, func(task *store.Task) {
		task.Engine = engine.NameAria2
		task.EngineRef = &ref
	})
}

// getFiles drives GET /tasks/{id}/files with the test bearer credential.
func (e *filesTestEnv) getFiles(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Get("/tasks/"+id+"/files", "Authorization: Bearer "+e.bearer)
}

// patchFilesRaw drives PATCH /tasks/{id}/files with a raw JSON body, so a
// test can send shapes the typed client would not produce.
func (e *filesTestEnv) patchFilesRaw(t *testing.T, id, body string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Patch("/tasks/"+id+"/files", strings.NewReader(body),
		"Authorization: Bearer "+e.bearer, "Content-Type: application/json")
}

// patchFiles drives PATCH /tasks/{id}/files with a typed body.
func (e *filesTestEnv) patchFiles(t *testing.T, id string, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Patch("/tasks/"+id+"/files", body, "Authorization: Bearer "+e.bearer)
}

// filesBody decodes the full-list envelope.
func filesBody(t *testing.T, recorder *httptest.ResponseRecorder) []TaskFileDTO {
	t.Helper()

	var body struct {
		Files []TaskFileDTO `json:"files"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body.Files
}

// storedFile reads one stored file row's selection pair.
func (e *filesTestEnv) storedFile(t *testing.T, taskID string, index int) store.TaskFile {
	t.Helper()

	var row store.TaskFile
	if err := e.db.GetContext(t.Context(), &row,
		`SELECT id, task_id, file_index, path, size_bytes, completed_bytes, selected, priority, created_at, updated_at
FROM task_files WHERE task_id = ? AND file_index = ?`, taskID, index); err != nil {
		t.Fatalf("read stored file %d of %s: %v", index, taskID, err)
	}

	return row
}

// TestListTaskFilesRendersListing pins the GET shape of doc 05 section
// 5.8: the engine listing lands in task_files and the store answers, with
// the stored integers named on the wire.
func TestListTaskFilesRendersListing(t *testing.T) {
	env := newFilesTestEnv(t)
	id := env.seedQBTTask(t)

	response := env.getFiles(t, id)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}

	want := []TaskFileDTO{
		{Index: 0, Path: "ubuntu-26.04-desktop-amd64.iso", SizeBytes: 1000, CompletedBytes: 500, Progress: 0.5, Selected: true, Priority: strPtr("normal")},
		{Index: 1, Path: "extras/SHA256SUMS", SizeBytes: 100, CompletedBytes: 100, Progress: 1, Selected: true, Priority: strPtr("high")},
		{Index: 2, Path: "extras/sample.mkv", SizeBytes: 200, CompletedBytes: 0, Progress: 0, Selected: true, Priority: strPtr("normal")},
	}
	if files := filesBody(t, response); !reflect.DeepEqual(files, want) {
		t.Errorf("files = %+v, want %+v", files, want)
	}

	// The listing was asked for under the namespaced engine id.
	if ids := env.qbittorrent.recordedFilesIDs(); len(ids) != 1 || ids[0] != engine.NameQBittorrent+":"+qbtHash {
		t.Errorf("listing ids = %v, want exactly the namespaced handle", ids)
	}

	// The rows the response came from are stored, selection and priority
	// included, ready for the delete path's enumeration.
	for index, wantSelected := range map[int]int{0: 1, 1: 1, 2: 1} {
		row := env.storedFile(t, id, index)
		if row.Selected != wantSelected {
			t.Errorf("stored selected of file %d = %d, want %d", index, row.Selected, wantSelected)
		}
	}
	if priority := env.storedFile(t, id, 1).Priority; priority == nil || *priority != 6 {
		t.Errorf("stored priority of file 1 = %v, want 6", priority)
	}
}

// TestListTaskFilesServesStoredRowsWhenEngineDown pins the stability of
// the GET answer: a failed engine listing is a warning while the store
// holds the last listing's rows, and the 503 only when there is nothing
// stored to serve.
func TestListTaskFilesServesStoredRowsWhenEngineDown(t *testing.T) {
	env := newFilesTestEnv(t)
	id := env.seedQBTTask(t)

	before := filesBody(t, env.getFiles(t, id))

	env.qbittorrent.failListing(engine.ErrUnavailable)
	response := env.getFiles(t, id)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	if after := filesBody(t, response); !reflect.DeepEqual(after, before) {
		t.Errorf("files during the outage = %+v, want the stored %+v", after, before)
	}

	// A task never listed has no rows to fall back on: the engine failure
	// is the 503 of doc 05 section 5.8. A distinct handle, because the
	// live-task unique index (engine, engine_ref) would refuse a second
	// row under the same one.
	otherRef := filesOtherHash
	unlisted := env.seedActionTask(t, func(task *store.Task) {
		task.Engine = engine.NameQBittorrent
		task.EngineRef = &otherRef
	})
	assertProblem(t, env.getFiles(t, unlisted), http.StatusServiceUnavailable, SlugEngineUnavailable)
}

// TestDeselectSetsSkip pins the FR-007/T032 criterion: PATCH with
// selected:false lands priority 0 at the engine and selected = 0 in
// task_files, and the unlisted indices keep their previous selection and
// priority.
func TestDeselectSetsSkip(t *testing.T) {
	env := newFilesTestEnv(t)
	id := env.seedQBTTask(t)

	response := env.patchFiles(t, id, map[string]any{
		"files": []map[string]any{{"index": 2, "selected": false}},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}

	// One engine call, carrying priority 0 for index 2 and nothing else;
	// selected stays nil so the unlisted indices are untouched.
	calls := env.qbittorrent.recordedSetCalls()
	if len(calls) != 1 {
		t.Fatalf("SetFiles calls = %+v, want exactly one", calls)
	}
	if calls[0].id != engine.NameQBittorrent+":"+qbtHash {
		t.Errorf("SetFiles id = %q, want the namespaced handle", calls[0].id)
	}
	if calls[0].selected != nil {
		t.Errorf("SetFiles selected = %v, want nil", calls[0].selected)
	}
	if want := map[int]int{2: 0}; !reflect.DeepEqual(calls[0].priorities, want) {
		t.Errorf("SetFiles priorities = %v, want %v", calls[0].priorities, want)
	}

	row := env.storedFile(t, id, 2)
	if row.Selected != 0 {
		t.Errorf("stored selected of file 2 = %d, want 0", row.Selected)
	}
	if row.Priority == nil || *row.Priority != 0 {
		t.Errorf("stored priority of file 2 = %v, want 0", row.Priority)
	}

	// The unlisted indices keep both columns exactly as the listing left
	// them: file 1 stays high, file 0 stays selected.
	if priority := env.storedFile(t, id, 1).Priority; priority == nil || *priority != 6 {
		t.Errorf("stored priority of the unlisted file 1 = %v, want the untouched 6", priority)
	}
	if row := env.storedFile(t, id, 0); row.Selected != 1 || row.Priority == nil || *row.Priority != 1 {
		t.Errorf("stored row of the unlisted file 0 = %+v, want untouched", row)
	}

	// The answer is the full list: file 2 deselected with priority skip,
	// the others as they stood.
	files := filesBody(t, response)
	if files[2].Selected || files[2].Priority == nil || *files[2].Priority != "skip" {
		t.Errorf("file 2 = %+v, want deselected with skip", files[2])
	}
	if files[1].Priority == nil || *files[1].Priority != "high" {
		t.Errorf("file 1 priority = %v, want the untouched high", files[1].Priority)
	}
}

// TestPatchFilesHigh pins the promote criterion: PATCH with
// priority:"high" sends priority 6 to the engine — never libtorrent's
// internal 4 — and both verbs answer the identical full-list body.
func TestPatchFilesHigh(t *testing.T) {
	env := newFilesTestEnv(t)
	id := env.seedQBTTask(t)

	response := env.patchFiles(t, id, map[string]any{
		"files": []map[string]any{{"index": 0, "priority": "high"}},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}

	calls := env.qbittorrent.recordedSetCalls()
	if len(calls) != 1 {
		t.Fatalf("SetFiles calls = %+v, want exactly one", calls)
	}
	if want := map[int]int{0: 6}; !reflect.DeepEqual(calls[0].priorities, want) {
		t.Errorf("SetFiles priorities = %v, want %v — the integer 4 is never sent", calls[0].priorities, want)
	}

	if priority := env.storedFile(t, id, 0).Priority; priority == nil || *priority != 6 {
		t.Errorf("stored priority of file 0 = %v, want 6", priority)
	}

	// Both verbs return the identical full-list body: the PATCH answer
	// equals what GET answers with right after it.
	patched := filesBody(t, response)
	listed := filesBody(t, env.getFiles(t, id))
	if !reflect.DeepEqual(patched, listed) {
		t.Errorf("PATCH body %+v differs from the GET body %+v", patched, listed)
	}
	if listed[0].Priority == nil || *listed[0].Priority != "high" {
		t.Errorf("file 0 priority = %v, want high after the promote", listed[0].Priority)
	}
}

// TestPatchFilesRejectsPriority4 pins the 4 rejection at the wire: the
// integer in either spelling — the JSON number a typed client could not
// produce, and the name-shaped string — is a 422 that reaches no engine.
func TestPatchFilesRejectsPriority4(t *testing.T) {
	env := newFilesTestEnv(t)
	id := env.seedQBTTask(t)

	cases := []struct {
		name string
		body string
	}{
		{"integer four", `{"files":[{"index":0,"priority":4}]}`},
		{"string four", `{"files":[{"index":0,"priority":"4"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := env.patchFilesRaw(t, id, tc.body)
			assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
		})
	}

	env.qbittorrent.assertNoSetCalls(t)
	if calls := env.qbittorrent.recorded(); len(calls) != 0 {
		t.Errorf("qbittorrent engine was contacted: %v", calls)
	}
}

// TestPatchFilesReselectSetsNormal pins the reverse of the deselect: a
// selected:true on a deselected file sends priority 1 to the engine and
// restores the stored row.
func TestPatchFilesReselectSetsNormal(t *testing.T) {
	env := newFilesTestEnv(t)
	id := env.seedQBTTask(t)

	deselect := env.patchFiles(t, id, map[string]any{
		"files": []map[string]any{{"index": 2, "selected": false}},
	})
	if deselect.Code != http.StatusOK {
		t.Fatalf("deselect: status %d body %s", deselect.Code, deselect.Body.String())
	}

	response := env.patchFiles(t, id, map[string]any{
		"files": []map[string]any{{"index": 2, "selected": true}},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("reselect: status %d body %s", response.Code, response.Body.String())
	}

	calls := env.qbittorrent.recordedSetCalls()
	if len(calls) != 2 {
		t.Fatalf("SetFiles calls = %+v, want the deselect then the reselect", calls)
	}
	if want := map[int]int{2: 1}; !reflect.DeepEqual(calls[1].priorities, want) {
		t.Errorf("reselect priorities = %v, want %v", calls[1].priorities, want)
	}

	row := env.storedFile(t, id, 2)
	if row.Selected != 1 || row.Priority == nil || *row.Priority != 1 {
		t.Errorf("stored row of file 2 = %+v, want restored selected/normal", row)
	}
}

// TestPatchFilesSkipStringDeselects pins the other spelling of the one
// concept: a lone priority:"skip" deselects exactly like selected:false.
func TestPatchFilesSkipStringDeselects(t *testing.T) {
	env := newFilesTestEnv(t)
	id := env.seedQBTTask(t)

	response := env.patchFiles(t, id, map[string]any{
		"files": []map[string]any{{"index": 2, "priority": "skip"}},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", response.Code, response.Body.String())
	}

	if want := map[int]int{2: 0}; !reflect.DeepEqual(env.qbittorrent.recordedSetCalls()[0].priorities, want) {
		t.Errorf("priorities = %v, want %v", env.qbittorrent.recordedSetCalls()[0].priorities, want)
	}

	row := env.storedFile(t, id, 2)
	if row.Selected != 0 || row.Priority == nil || *row.Priority != 0 {
		t.Errorf("stored row of file 2 = %+v, want deselected with skip", row)
	}
}

// TestListTaskFilesEmptyListingClearsRows pins the replace semantics of
// the upsert: a listing that carries no files clears the task's rows.
func TestListTaskFilesEmptyListingClearsRows(t *testing.T) {
	env := newFilesTestEnv(t)
	id := env.seedQBTTask(t)

	if response := env.getFiles(t, id); response.Code != http.StatusOK {
		t.Fatalf("seed listing: status %d body %s", response.Code, response.Body.String())
	}

	env.qbittorrent.setListing(nil)
	response := env.getFiles(t, id)
	if response.Code != http.StatusOK {
		t.Fatalf("empty listing: status %d body %s", response.Code, response.Body.String())
	}
	if files := filesBody(t, response); len(files) != 0 {
		t.Errorf("files = %+v, want the empty listing", files)
	}

	var rows int
	if err := env.db.GetContext(t.Context(), &rows,
		`SELECT COUNT(*) FROM task_files WHERE task_id = ?`, id); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("stored rows = %d, want 0 after the empty listing", rows)
	}
}

// TestPatchFilesUnknownIndex pins the 422 of an index outside the task's
// listing, with the offending field located in errors[].
func TestPatchFilesUnknownIndex(t *testing.T) {
	env := newFilesTestEnv(t)
	id := env.seedQBTTask(t)

	response := env.patchFiles(t, id, map[string]any{
		"files": []map[string]any{{"index": 99, "priority": "high"}},
	})
	problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	if len(problem.Errors) != 1 || problem.Errors[0].Location != "body.files[0].index" {
		t.Errorf("errors = %+v, want the body.files[0].index field error", problem.Errors)
	}

	env.qbittorrent.assertNoSetCalls(t)
}

// TestPatchFilesEntryValidation pins the per-entry rules: at least one of
// selected and priority, an entry carrying both must agree, an unknown
// priority name is refused, a repeated index is named instead of folded,
// and the files array must not be empty or null.
func TestPatchFilesEntryValidation(t *testing.T) {
	env := newFilesTestEnv(t)
	id := env.seedQBTTask(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"neither field", map[string]any{"files": []map[string]any{{"index": 0}}}},
		{"disagreeing fields", map[string]any{
			"files": []map[string]any{{"index": 0, "selected": true, "priority": "skip"}},
		}},
		{"unknown priority name", map[string]any{
			"files": []map[string]any{{"index": 0, "priority": "low"}},
		}},
		{"repeated index", map[string]any{
			"files": []map[string]any{
				{"index": 0, "selected": true},
				{"index": 0, "priority": "skip"},
			},
		}},
		{"empty files array", map[string]any{"files": []map[string]any{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := env.patchFiles(t, id, tc.body)
			assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
		})
	}

	// A JSON null decodes to a nil slice no schema tag can tell from an
	// absent one; the handler's backstop owns it.
	t.Run("null files array", func(t *testing.T) {
		response := env.patchFilesRaw(t, id, `{"files":null}`)
		assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	})

	// A mistyped query key is 422, never silently ignored.
	t.Run("unknown query parameter", func(t *testing.T) {
		response := env.api.Patch("/tasks/"+id+"/files?indx=2",
			map[string]any{"files": []map[string]any{{"index": 0, "priority": "high"}}},
			"Authorization: Bearer "+env.bearer)
		assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	})

	env.qbittorrent.assertNoSetCalls(t)
}

// TestAria2FilesHaveNullPriority pins the aria2 shape: every file of an
// engine without per_file_priority lists priority null and a real
// selected value.
func TestAria2FilesHaveNullPriority(t *testing.T) {
	env := newFilesTestEnv(t)
	id := env.seedAria2Task(t)

	response := env.getFiles(t, id)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}

	files := filesBody(t, response)
	if len(files) != 2 {
		t.Fatalf("files = %+v, want the two-file listing", files)
	}
	for _, file := range files {
		if file.Priority != nil {
			t.Errorf("file %d priority = %q, want null", file.Index, *file.Priority)
		}
	}
	if !files[0].Selected || files[1].Selected {
		t.Errorf("selected flags = %+v, want the engine's real values", files)
	}

	// The rows store the aria2 shape: selection without priority.
	if priority := env.storedFile(t, id, 0).Priority; priority != nil {
		t.Errorf("stored priority of file 0 = %v, want NULL", priority)
	}
}

// TestPatchFilesNeedsPerFilePriority pins the capability gate: a PATCH on
// a task whose engine does not declare per_file_priority is the 422 of
// doc 05 section 5.8, and no engine call is made.
func TestPatchFilesNeedsPerFilePriority(t *testing.T) {
	env := newFilesTestEnv(t)
	id := env.seedAria2Task(t)

	response := env.patchFiles(t, id, map[string]any{
		"files": []map[string]any{{"index": 0, "selected": false}},
	})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	env.aria2.assertNoSetCalls(t)
}

// TestTaskFilesUnknownTask pins the 404 of both verbs.
func TestTaskFilesUnknownTask(t *testing.T) {
	env := newFilesTestEnv(t)

	assertProblem(t, env.getFiles(t, unknownID), http.StatusNotFound, SlugNotFound)

	response := env.patchFiles(t, unknownID, map[string]any{
		"files": []map[string]any{{"index": 0, "selected": false}},
	})
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
}

// TestPatchFilesEngineFailure pins the 503 and the untouched store: when
// the engine cannot take the change, task_files keeps the previous
// selection.
func TestPatchFilesEngineFailure(t *testing.T) {
	env := newFilesTestEnv(t)
	id := env.seedQBTTask(t)

	if response := env.getFiles(t, id); response.Code != http.StatusOK {
		t.Fatalf("seed listing: status %d body %s", response.Code, response.Body.String())
	}

	env.qbittorrent.failSet(engine.ErrUnavailable)
	response := env.patchFiles(t, id, map[string]any{
		"files": []map[string]any{{"index": 2, "selected": false}},
	})
	assertProblem(t, response, http.StatusServiceUnavailable, SlugEngineUnavailable)

	row := env.storedFile(t, id, 2)
	if row.Selected != 1 || row.Priority == nil || *row.Priority != 1 {
		t.Errorf("stored row of file 2 = %+v, want the untouched listing", row)
	}
}

// TestPatchFilesWithoutEngineHandle pins the not-admitted answer: a task
// no engine holds yet cannot take a selection, and the create-time
// selection is that moment.
func TestPatchFilesWithoutEngineHandle(t *testing.T) {
	env := newFilesTestEnv(t)
	id := env.seedActionTask(t, func(task *store.Task) {
		task.Engine = engine.NameQBittorrent
		task.EngineRef = nil
	})

	response := env.patchFiles(t, id, map[string]any{
		"files": []map[string]any{{"index": 0, "selected": false}},
	})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	env.qbittorrent.assertNoSetCalls(t)
}

// strPtr is intPtr for the wire vocabulary.
func strPtr(s string) *string { return &s }
