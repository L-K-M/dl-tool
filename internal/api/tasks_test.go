package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

// The four URIs of FR-001: three routable shapes plus one garbage line.
const (
	mixedHTTPS   = "https://releases.example.com/26.04/ubuntu-26.04-desktop-amd64.iso"
	mixedFTP     = "ftp://mirror.example.org/pub/file.iso"
	mixedMagnet  = "magnet:?xt=urn:btih:8f9c3a2b1d4e5f60718293a4b5c6d7e8f9a0b1c2"
	mixedGarbage = "not a uri at all"

	// ed2kExample is parsed for display and refused with the exact message
	// of doc 06 section 2 row 7.
	ed2kExample = "ed2k://|file|x|1|0123456789abcdef0123456789abcdef|/"

	ftpUser = "ftpuser"
	// Alphanumeric so the userinfo encoding keeps it verbatim: the test
	// asserts the exact stored form.
	ftpPassword = "Sup3rS3cretPw"
)

// tasksTestEnv is one humatest server against a real migrated store, with
// the auth gate satisfied by a seeded bearer token and the engine registry
// holding recording stand-ins.
type tasksTestEnv struct {
	api         humatest.TestAPI
	db          *sqlx.DB
	logs        *strings.Builder
	aria2       *recordingEngine
	qbittorrent *recordingEngine
	dataRoot    string
	bearer      string
}

// newTasksTestEnv builds the server with the app logger writing to a buffer,
// so a test can prove no log line carries a submitted secret.
func newTasksTestEnv(t *testing.T) *tasksTestEnv {
	t.Helper()

	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	if err := os.Mkdir(dataRoot, 0o755); err != nil {
		t.Fatalf("make data root: %v", err)
	}

	return newTasksTestEnvWithRoots(t, dataRoot, []string{dataRoot})
}

// newTasksTestEnvWithRoots builds the env against an explicit root set, so a
// test can pin the containment rules of unusual configurations.
func newTasksTestEnvWithRoots(t *testing.T, dataRoot string, roots []string) *tasksTestEnv {
	t.Helper()

	root := filepath.Dir(dataRoot)
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

	logs := &strings.Builder{}
	server, err := NewServer(
		&config.Config{
			ConfigDir:  configDir,
			SessionTTL: time.Hour,
			DataRoots:  roots,
		},
		db,
		// Debug level so the leak test sees everything the process could
		// emit, not just Info and above.
		slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	env := &tasksTestEnv{
		api:         humatest.Wrap(t, server.API),
		db:          db,
		logs:        logs,
		aria2:       newRecordingEngine(engine.NameAria2, acceptsAria2Lanes),
		qbittorrent: newRecordingEngine(engine.NameQBittorrent, acceptsBitTorrent),
		dataRoot:    dataRoot,
	}
	server.Engines.Register(env.aria2)
	server.Engines.Register(env.qbittorrent)

	user := seedUser(t, db)
	env.bearer = seedLiveAPIToken(t, db, user.ID)

	return env
}

// recordingEngine is a routing stand-in: Name, Capabilities and Accepts are
// the real routing inputs, and every I/O method records its call so a test
// can prove the create path performs none.
type recordingEngine struct {
	name    string
	accepts func(string) bool

	mu    sync.Mutex
	calls []string
}

func newRecordingEngine(name string, accepts func(string) bool) *recordingEngine {
	return &recordingEngine{name: name, accepts: accepts}
}

func (e *recordingEngine) Name() string                      { return e.name }
func (e *recordingEngine) Capabilities() []engine.Capability { return nil }
func (e *recordingEngine) Accepts(uri string) bool           { return e.accepts(uri) }

func (e *recordingEngine) record(call string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, call)
}

// recorded returns every I/O method call made so far.
func (e *recordingEngine) recorded() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]string(nil), e.calls...)
}

func (e *recordingEngine) Connect(context.Context) error          { e.record("Connect"); return nil }
func (e *recordingEngine) Close() error                           { e.record("Close"); return nil }
func (e *recordingEngine) Health(context.Context) (string, error) { e.record("Health"); return "", nil }
func (e *recordingEngine) Add(context.Context, engine.AddRequest) (string, error) {
	e.record("Add")
	return "", nil
}
func (e *recordingEngine) List(context.Context) ([]engine.TaskInfo, error) {
	e.record("List")
	return nil, nil
}
func (e *recordingEngine) Get(context.Context, string) (engine.TaskInfo, error) {
	e.record("Get")
	return engine.TaskInfo{}, nil
}
func (e *recordingEngine) Files(context.Context, string) ([]engine.FileEntry, error) {
	e.record("Files")
	return nil, nil
}
func (e *recordingEngine) Pause(context.Context, string) error  { e.record("Pause"); return nil }
func (e *recordingEngine) Resume(context.Context, string) error { e.record("Resume"); return nil }
func (e *recordingEngine) Remove(context.Context, string) error { e.record("Remove"); return nil }

func (e *recordingEngine) SetFiles(context.Context, string, []int, map[int]int) error {
	e.record("SetFiles")
	return engine.ErrNotSupported
}
func (e *recordingEngine) SetLocation(context.Context, string, string) error {
	e.record("SetLocation")
	return engine.ErrNotSupported
}
func (e *recordingEngine) Rename(context.Context, string, string) error {
	e.record("Rename")
	return engine.ErrNotSupported
}
func (e *recordingEngine) SetCategory(context.Context, string, string) error {
	e.record("SetCategory")
	return engine.ErrNotSupported
}
func (e *recordingEngine) SetRateLimits(context.Context, string, *int64, *int64) error {
	e.record("SetRateLimits")
	return engine.ErrNotSupported
}
func (e *recordingEngine) SetShareLimits(context.Context, string, *float64, *int64) error {
	e.record("SetShareLimits")
	return engine.ErrNotSupported
}
func (e *recordingEngine) Events(context.Context) (<-chan engine.TaskEvent, error) {
	e.record("Events")
	return make(chan engine.TaskEvent), nil
}

// acceptsAria2Lanes mirrors the aria2 adapter's own Accepts.
func acceptsAria2Lanes(uri string) bool {
	return engineAcceptsSchemes(uri, "http", "https", "ftp", "sftp")
}

// acceptsBitTorrent mirrors the qBittorrent lanes of the routing table:
// magnet URIs and .torrent URLs.
func acceptsBitTorrent(uri string) bool {
	return engineAcceptsSchemes(uri, "magnet") || strings.HasSuffix(strings.ToLower(uri), ".torrent")
}

func engineAcceptsSchemes(raw string, schemes ...string) bool {
	scheme, _, found := strings.Cut(raw, ":")
	if !found {
		return false
	}
	for _, s := range schemes {
		if strings.EqualFold(scheme, s) {
			return true
		}
	}

	return false
}

// createTasks posts one submission with the test bearer credential.
func (e *tasksTestEnv) createTasks(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Post("/tasks", body, "Authorization: Bearer "+e.bearer)
}

// countTasks counts the tasks rows a submission left behind.
func (e *tasksTestEnv) countTasks(t *testing.T) int {
	t.Helper()

	var count int
	if err := e.db.GetContext(t.Context(), &count, `SELECT COUNT(*) FROM tasks`); err != nil {
		t.Fatalf("count tasks: %v", err)
	}

	return count
}

// listTasks calls GET /tasks with the test bearer credential.
func (e *tasksTestEnv) listTasks(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Get("/tasks"+query, "Authorization: Bearer "+e.bearer)
}

// getTask calls GET /tasks/{id} with the test bearer credential.
func (e *tasksTestEnv) getTask(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Get("/tasks/"+id, "Authorization: Bearer "+e.bearer)
}

// decodeListBody decodes the cursor pagination envelope of doc 05 1.4.
func decodeListBody(t *testing.T, recorder *httptest.ResponseRecorder) struct {
	Items      []TaskDTO `json:"items"`
	NextCursor *string   `json:"next_cursor"`
	Total      int       `json:"total"`
} {
	t.Helper()

	var body struct {
		Items      []TaskDTO `json:"items"`
		NextCursor *string   `json:"next_cursor"`
		Total      int       `json:"total"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body
}

// seedTaskStates writes one task per state straight through the store,
// because POST /tasks can only reach queued and paused.
func (e *tasksTestEnv) seedTaskStates(t *testing.T) map[string]string {
	t.Helper()

	tasks := store.NewTaskStore(e.db)
	stateOf := map[string]string{} // id -> state
	for _, state := range []string{
		"queued", "downloading", "checking", "paused", "seeding",
		"completed", "extracting", "moving", "error", "removed",
	} {
		task, err := tasks.Create(t.Context(), store.Task{
			Engine:      "aria2",
			SourceKind:  "http",
			Name:        "fixture-" + state,
			State:       state,
			Destination: "/data",
		})
		if err != nil {
			t.Fatalf("seed task in %s: %v", state, err)
		}
		stateOf[task.ID] = state
	}

	return stateOf
}

// decodeCreateBody decodes a create response envelope. The wire shape is
// flat — Huma serialises the output Body field's members at the top level —
// so the decoder mirrors the envelope rather than the output struct.
func decodeCreateBody(t *testing.T, recorder *httptest.ResponseRecorder) struct {
	Created  []TaskDTO     `json:"created"`
	Rejected []RejectedURI `json:"rejected"`
} {
	t.Helper()

	var body struct {
		Created  []TaskDTO     `json:"created"`
		Rejected []RejectedURI `json:"rejected"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body
}

// TestCreateTasksMixedBatch pins FR-001: a four-line submission of an https,
// an ftp, a magnet and one garbage line creates three tasks — routed by the
// table — and reports one rejected entry, without contacting an engine.
func TestCreateTasksMixedBatch(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.createTasks(t, map[string]any{
		"uris":        []string{mixedHTTPS, mixedFTP, mixedMagnet, mixedGarbage},
		"destination": filepath.Join(env.dataRoot, "iso"),
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}

	body := decodeCreateBody(t, response)
	if len(body.Created) != 3 {
		t.Fatalf("created %d tasks, want 3: %+v", len(body.Created), body.Created)
	}
	if len(body.Rejected) != 1 || body.Rejected[0].URI != mixedGarbage {
		t.Fatalf("rejected = %+v, want exactly the garbage line", body.Rejected)
	}
	if body.Rejected[0].Type != SlugUnsupportedScheme {
		t.Errorf("rejection type = %q, want %q", body.Rejected[0].Type, SlugUnsupportedScheme)
	}

	// Routing: magnet to qBittorrent, https and ftp to aria2.
	wantEngines := map[string]string{
		mixedHTTPS:  engine.NameAria2,
		mixedFTP:    engine.NameAria2,
		mixedMagnet: engine.NameQBittorrent,
	}
	enginesByURI := map[string]string{}
	for _, created := range body.Created {
		if !strings.HasPrefix(created.ID, "tsk_") || len(created.ID) != len(store.PrefixTask)+26 {
			t.Errorf("id %q is not a tsk_ ULID", created.ID)
		}
		if created.State != string(engine.StateQueued) {
			t.Errorf("task %s state = %q, want %q", created.ID, created.State, engine.StateQueued)
		}
		if created.SourceURI == nil {
			t.Errorf("task %s has no source_uri", created.ID)
			continue
		}
		enginesByURI[*created.SourceURI] = created.Engine
	}
	for raw, want := range wantEngines {
		if got := enginesByURI[raw]; got != want {
			t.Errorf("uri %s routed to %q, want %q", raw, got, want)
		}
	}

	// No engine method runs at creation time, and no engine_ref is set.
	for name, e := range map[string]*recordingEngine{
		engine.NameAria2:       env.aria2,
		engine.NameQBittorrent: env.qbittorrent,
	} {
		if calls := e.recorded(); len(calls) != 0 {
			t.Errorf("%s engine was called during creation: %v", name, calls)
		}
	}

	var engineRefs int
	if err := env.db.GetContext(t.Context(), &engineRefs,
		`SELECT COUNT(*) FROM tasks WHERE engine_ref IS NOT NULL`); err != nil {
		t.Fatalf("count engine refs: %v", err)
	}
	if engineRefs != 0 {
		t.Errorf("%d tasks carry an engine_ref, want 0", engineRefs)
	}
}

// TestCreateTasksRejectsED2K pins FR-004's exact message: an ed2k-only
// submission is refused whole with 422 /problems/unsupported-scheme.
func TestCreateTasksRejectsED2K(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.createTasks(t, map[string]any{"uris": []string{ed2kExample}})
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
	}

	problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugUnsupportedScheme)
	if problem.Detail != "ed2k is not supported in v1" {
		t.Errorf("detail = %q, want %q", problem.Detail, "ed2k is not supported in v1")
	}
	if env.countTasks(t) != 0 {
		t.Errorf("ed2k submission created tasks, want none")
	}
}

// TestCreateTasksRejectsDestination pins the root jail: a destination that
// escapes the configured roots is a 403 that creates nothing.
func TestCreateTasksRejectsDestination(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.createTasks(t, map[string]any{
		"uris":        []string{mixedHTTPS},
		"destination": "/data/../etc",
	})
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusForbidden, response.Body.String())
	}

	problem := assertProblem(t, response, http.StatusForbidden, SlugPathRejected)
	if len(problem.Errors) != 1 || problem.Errors[0].Location != "body.destination" {
		t.Errorf("errors = %+v, want the body.destination field error", problem.Errors)
	}
	if env.countTasks(t) != 0 {
		t.Errorf("rejected submission created tasks, want none")
	}
}

// TestCreateTasksHidesFTPPassword pins FR-009: the credentials ride with the
// row for the admission pass, but no response body and no log line carries
// the password.
func TestCreateTasksHidesFTPPassword(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.createTasks(t, map[string]any{
		"uris": []string{
			"ftp://ftpuser:Sup3rS3cretPw@mirror.example.org/pub/file.iso",
			"http://webuser:Sup3rS3cretPw@www.example.org/pub/page.html",
		},
		"ftp_credentials": map[string]string{
			"username": ftpUser,
			"password": ftpPassword,
		},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	if strings.Contains(response.Body.String(), ftpPassword) {
		t.Errorf("response body leaks the ftp password: %s", response.Body.String())
	}
	if strings.Contains(env.logs.String(), ftpPassword) {
		t.Errorf("a log line leaks the ftp password: %s", env.logs.String())
	}

	// The row's server-only source carries the credentials so the admission
	// pass can forward them to aria2 (docs/04-data-model.md section 3.3); the
	// http row carries none, its userinfo was stripped at ingest.
	var stored []string
	if err := env.db.SelectContext(t.Context(), &stored,
		`SELECT source_uri FROM tasks ORDER BY source_kind`); err != nil {
		t.Fatalf("read stored sources: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored %d sources, want 2: %v", len(stored), stored)
	}
	if !strings.Contains(stored[0], ftpPassword) {
		t.Errorf("ftp source_uri = %q, want the credential-bearing engine source", stored[0])
	}
	if strings.Contains(stored[1], ftpPassword) {
		t.Errorf("http source_uri = %q, want the stripped display URI", stored[1])
	}
}

// TestCreateTasksValidation pins the shape limits: an empty submission and a
// 51-URI submission are 422 validation failures that create nothing.
func TestCreateTasksValidation(t *testing.T) {
	env := newTasksTestEnv(t)

	cases := []struct {
		name string
		uris []string
		null bool
	}{
		{"empty submission", []string{}, false},
		{"null uris", nil, true},
		{"fifty-one uris", make([]string, 51), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.uris {
				tc.uris[i] = mixedHTTPS
			}
			body := map[string]any{"uris": tc.uris}
			if tc.null {
				body = map[string]any{"uris": nil}
			}

			response := env.createTasks(t, body)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
			}

			assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
			if env.countTasks(t) != 0 {
				t.Errorf("%s created tasks, want none", tc.name)
			}
		})
	}
}

// TestCreateTasksUnknownCategory pins the category rule: a name that does
// not exist is a validation failure before any row is written.
func TestCreateTasksUnknownCategory(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.createTasks(t, map[string]any{
		"uris":     []string{mixedHTTPS},
		"category": "no-such-category",
	})
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
	}

	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	if env.countTasks(t) != 0 {
		t.Errorf("unknown category created tasks, want none")
	}
}

// TestCreateTasksPaused pins the paused flag: the tasks are created in
// paused instead of queued, still without an engine round-trip.
func TestCreateTasksPaused(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.createTasks(t, map[string]any{
		"uris":   []string{mixedHTTPS},
		"paused": true,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}

	body := decodeCreateBody(t, response)
	if len(body.Created) != 1 {
		t.Fatalf("created %d tasks, want 1", len(body.Created))
	}
	if state := body.Created[0].State; state != string(engine.StatePaused) {
		t.Errorf("state = %q, want %q", state, engine.StatePaused)
	}
}

// TestCreateTasksExplicitEngine pins the override rule of doc 06 section 2:
// the field wins only when that engine accepts the URI, and an unregistered
// engine is a whole-request 503.
func TestCreateTasksExplicitEngine(t *testing.T) {
	env := newTasksTestEnv(t)

	// qbittorrent refuses an https URI: the override is denied per-URI while
	// the batch's other task is still created.
	response := env.createTasks(t, map[string]any{
		"uris":   []string{mixedHTTPS, mixedMagnet},
		"engine": engine.NameQBittorrent,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	body := decodeCreateBody(t, response)
	if len(body.Created) != 1 || body.Created[0].Engine != engine.NameQBittorrent {
		t.Fatalf("created = %+v, want only the magnet on qbittorrent", body.Created)
	}
	if len(body.Rejected) != 1 || body.Rejected[0].URI != mixedHTTPS {
		t.Fatalf("rejected = %+v, want exactly the https line", body.Rejected)
	}

	// An engine that is not registered at all is the whole-request 503, even
	// though nothing is submitted to it.
	response = env.createTasks(t, map[string]any{
		"uris":   []string{mixedHTTPS},
		"engine": engine.NameYtDlp,
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	assertProblem(t, response, http.StatusServiceUnavailable, SlugEngineUnavailable)
	if got := env.countTasks(t); got != 1 {
		t.Errorf("unavailable engine left %d tasks, want only the 1 from the first request", got)
	}
}

// TestCreateTasksDuplicateTorrent pins the uniqueness rule the tasks table
// enforces through its partial unique indexes: a repeated torrent — in one
// submission or against a live task — is a per-URI conflict, not a 500.
func TestCreateTasksDuplicateTorrent(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.createTasks(t, map[string]any{
		"uris": []string{mixedMagnet, mixedMagnet},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	body := decodeCreateBody(t, response)
	if len(body.Created) != 1 {
		t.Fatalf("created %d tasks, want 1", len(body.Created))
	}
	if len(body.Rejected) != 1 || body.Rejected[0].Type != SlugConflict {
		t.Fatalf("rejected = %+v, want one conflict entry", body.Rejected)
	}

	// The same magnet in a later submission hits the live row the same way;
	// with every URI refused it answers the all-rejected 422, its detail
	// carrying the conflict reason.
	response = env.createTasks(t, map[string]any{"uris": []string{mixedMagnet}})
	problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugUnsupportedScheme)
	if problem.Detail != duplicateDetail {
		t.Errorf("detail = %q, want %q", problem.Detail, duplicateDetail)
	}
	if env.countTasks(t) != 1 {
		t.Errorf("%d tasks after duplicate submissions, want 1", env.countTasks(t))
	}
}

// TestCreateTasksRequestedDestination pins the canonical echo rule: when
// the server resolves the client's destination to a different path (here
// through a symlink), the response carries both.
func TestCreateTasksRequestedDestination(t *testing.T) {
	env := newTasksTestEnv(t)

	real := filepath.Join(env.dataRoot, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("make real dir: %v", err)
	}
	link := filepath.Join(env.dataRoot, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	response := env.createTasks(t, map[string]any{
		"uris":        []string{mixedHTTPS},
		"destination": filepath.Join(link, "iso"),
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}

	body := decodeCreateBody(t, response)
	created := body.Created[0]
	if created.Destination != filepath.Join(real, "iso") {
		t.Errorf("destination = %q, want the resolved %q", created.Destination, filepath.Join(real, "iso"))
	}
	if created.RequestedDestination == nil || *created.RequestedDestination != filepath.Join(link, "iso") {
		t.Errorf("requested_destination = %v, want %q", created.RequestedDestination, filepath.Join(link, "iso"))
	}
}

// TestCreateTasksFilesystemRoot pins the containment edge of a root that is
// the filesystem root itself: every absolute destination stays inside.
func TestCreateTasksFilesystemRoot(t *testing.T) {
	env := newTasksTestEnvWithRoots(t, t.TempDir(), []string{"/"})

	response := env.createTasks(t, map[string]any{
		"uris":        []string{mixedHTTPS},
		"destination": filepath.Join(env.dataRoot, "iso"),
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	if env.countTasks(t) != 1 {
		t.Errorf("%d tasks, want 1", env.countTasks(t))
	}
}

// TestNewServerRegistersAria2 pins the composition-root branch: a
// configured aria2 endpoint joins the registry, and a malformed one fails
// server construction loudly rather than degrading silently.
func TestNewServerRegistersAria2(t *testing.T) {
	t.Parallel()

	valid, err := NewServer(
		&config.Config{
			Aria2URL:    "http://aria2.test:6800/jsonrpc",
			Aria2Secret: secure.Secret("rpc-secret"),
		},
		nil,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("NewServer with aria2: %v", err)
	}
	if _, ok := valid.Engines.Get(engine.NameAria2); !ok {
		t.Errorf("aria2 is not registered")
	}

	if _, err := NewServer(
		&config.Config{Aria2URL: "not-a-url", Aria2Secret: secure.Secret("rpc-secret")},
		nil,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	); err == nil {
		t.Errorf("NewServer with a malformed aria2 url succeeded, want an error")
	}
}

// TestSidebarFiltersOverHTTP pins FR-012/FR-013 through the endpoint: each
// of the seven sidebar filters returns exactly the states of the membership
// table, removed tombstones appear only under an explicit state=removed.
func TestSidebarFiltersOverHTTP(t *testing.T) {
	env := newTasksTestEnv(t)
	stateOf := env.seedTaskStates(t)

	want := map[string][]string{
		"all":         {"queued", "downloading", "checking", "paused", "seeding", "completed", "extracting", "moving", "error"},
		"downloading": {"downloading"},
		"completed":   {"completed", "seeding"},
		"active":      {"downloading", "seeding"},
		"inactive":    {"error", "queued", "paused"},
		"stopped":     {"paused"},
		"error":       {"error"},
	}
	for _, filter := range slices.Sorted(maps.Keys(want)) {
		t.Run(filter, func(t *testing.T) {
			response := env.listTasks(t, "?state="+filter)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
			}

			body := decodeListBody(t, response)
			if body.Total != len(want[filter]) {
				t.Errorf("total = %d, want %d", body.Total, len(want[filter]))
			}
			got := make([]string, len(body.Items))
			for i, item := range body.Items {
				got[i] = stateOf[item.ID]
			}
			if !equalAsSets(want[filter], got) {
				t.Errorf("states = %v, want %v", got, want[filter])
			}
			if body.NextCursor != nil {
				t.Errorf("next_cursor = %v, want null", body.NextCursor)
			}
		})
	}

	t.Run("no state behaves like all", func(t *testing.T) {
		response := env.listTasks(t, "")
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
		}
		body := decodeListBody(t, response)
		if body.Total != len(want["all"]) {
			t.Errorf("total = %d, want %d", body.Total, len(want["all"]))
		}
	})

	t.Run("explicit removed lists tombstones", func(t *testing.T) {
		response := env.listTasks(t, "?state=removed")
		body := decodeListBody(t, response)
		if body.Total != 1 || len(body.Items) != 1 {
			t.Fatalf("total = %d items = %d, want one tombstone", body.Total, len(body.Items))
		}
		if stateOf[body.Items[0].ID] != "removed" {
			t.Errorf("returned %v, want the removed task", body.Items[0].ID)
		}
	})

	t.Run("unknown state is 422", func(t *testing.T) {
		response := env.listTasks(t, "?state=downloding")
		assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	})
}

func equalAsSets(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// TestListTasksFilterAndSort pins the remaining query dimensions over HTTP:
// category, tag and the name substring resolve, and a documented sort key
// orders the page.
func TestListTasksFilterAndSort(t *testing.T) {
	env := newTasksTestEnv(t)

	// POST /tasks requires the category to exist already.
	_, err := env.db.ExecContext(t.Context(),
		`INSERT INTO categories (id, name, save_path, created_at, updated_at)
VALUES (?, 'linux', '/data/linux', 0, 0)`, store.NewID(store.PrefixCategory))
	if err != nil {
		t.Fatalf("seed category: %v", err)
	}

	seeded := func(name, category string, tags ...string) string {
		body := map[string]any{"uris": []string{"https://example.org/" + name + ".iso"}}
		if category != "" {
			body["category"] = category
		}
		if len(tags) > 0 {
			body["tags"] = tags
		}
		response := env.createTasks(t, body)
		if response.Code != http.StatusCreated {
			t.Fatalf("seed %s: status %d body %s", name, response.Code, response.Body.String())
		}
		created := decodeCreateBody(t, response).Created
		if len(created) != 1 {
			t.Fatalf("seed %s: created %d tasks, want 1", name, len(created))
		}

		return created[0].ID
	}

	ubuntu := seeded("Ubuntu", "linux", "iso")
	debian := seeded("Debian", "")
	arch := seeded("Arch", "", "iso")

	t.Run("category name and empty category", func(t *testing.T) {
		body := decodeListBody(t, env.listTasks(t, "?category=linux"))
		if body.Total != 1 || len(body.Items) != 1 || body.Items[0].ID != ubuntu {
			t.Errorf("category=linux returned %+v, want the ubuntu task", body.Items)
		}
		if cat := body.Items[0].Category; cat == nil || *cat != "linux" {
			t.Errorf("category member = %v, want the name linux", cat)
		}

		body = decodeListBody(t, env.listTasks(t, "?category="))
		if body.Total != 2 {
			t.Errorf("empty category total = %d, want 2", body.Total)
		}
	})

	t.Run("tag name and empty tag", func(t *testing.T) {
		body := decodeListBody(t, env.listTasks(t, "?tag=iso"))
		if body.Total != 2 {
			t.Errorf("tag=iso total = %d, want 2", body.Total)
		}
		gotTags := map[string][]string{}
		for _, item := range body.Items {
			gotTags[item.ID] = item.Tags
		}
		if !slices.Equal(gotTags[arch], []string{"iso"}) {
			t.Errorf("tags of the arch task = %v, want [iso]", gotTags[arch])
		}

		body = decodeListBody(t, env.listTasks(t, "?tag="))
		if body.Total != 1 || len(body.Items) != 1 || body.Items[0].ID != debian {
			t.Fatalf("empty tag returned %+v, want the untagged debian task", body.Items)
		}
		if items := body.Items[0].Tags; items == nil || len(items) != 0 {
			t.Errorf("tags of an untagged task = %v, want []", items)
		}
	})

	t.Run("name substring", func(t *testing.T) {
		body := decodeListBody(t, env.listTasks(t, "?q=ubu"))
		if body.Total != 1 || len(body.Items) != 1 || body.Items[0].ID != ubuntu {
			t.Fatalf("q=ubu returned %+v, want the ubuntu task", body.Items)
		}
	})

	t.Run("sort orders the page", func(t *testing.T) {
		body := decodeListBody(t, env.listTasks(t, "?sort=name"))
		names := []string{}
		for _, item := range body.Items {
			names = append(names, item.Name)
		}
		if !slices.Equal(names, []string{"Arch.iso", "Debian.iso", "Ubuntu.iso"}) {
			t.Errorf("names = %v, want ascending", names)
		}
	})
}

// TestListTasksCursorWalk pins the envelope: pages chain through
// next_cursor, total stays the filter's count on every page, and the last
// page carries null.
func TestListTasksCursorWalk(t *testing.T) {
	env := newTasksTestEnv(t)
	// Distinct names, so sort=name orders the walk deterministically.
	for _, uri := range []string{
		"https://example.org/walk-a.iso",
		"https://example.org/walk-b.iso",
		"https://example.org/walk-c.iso",
	} {
		response := env.createTasks(t, map[string]any{"uris": []string{uri}})
		if response.Code != http.StatusCreated {
			t.Fatalf("seed: status %d body %s", response.Code, response.Body.String())
		}
	}

	seen := map[string]bool{}
	var names []string
	query := "?limit=2&sort=name"
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatalf("cursor walk exceeded 10 pages; the server keeps issuing cursors")
		}
		response := env.listTasks(t, query)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body %s", response.Code, response.Body.String())
		}
		body := decodeListBody(t, response)
		if len(body.Items) > 2 {
			t.Errorf("page returned %d items, want at most limit=2", len(body.Items))
		}
		if body.Total != 3 {
			t.Errorf("total = %d, want 3 on every page", body.Total)
		}
		for _, item := range body.Items {
			if seen[item.ID] {
				t.Fatalf("task %s returned twice", item.ID)
			}
			seen[item.ID] = true
			names = append(names, item.Name)
		}
		if body.NextCursor == nil {
			break
		}
		query = "?limit=2&sort=name&cursor=" + url.QueryEscape(*body.NextCursor)
	}

	if len(seen) != 3 {
		t.Errorf("walk returned %d tasks, want 3", len(seen))
	}
	if !slices.Equal(names, []string{"walk-a.iso", "walk-b.iso", "walk-c.iso"}) {
		t.Errorf("names across pages = %v, want ascending", names)
	}
}

// TestListTasksRejectsUnknownSort pins the 422 of a sort key outside the
// allowlist, with the offending field located in errors[].
func TestListTasksRejectsUnknownSort(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.listTasks(t, "?sort=password")
	problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	if len(problem.Errors) != 1 || problem.Errors[0].Location != "query.sort" {
		t.Errorf("errors = %+v, want the query.sort field error", problem.Errors)
	}
}

// TestListTasksRejectsStaleCursor pins the cursor binding: a token replayed
// under a different filter or sort is a 422, never a wrong page.
func TestListTasksRejectsStaleCursor(t *testing.T) {
	env := newTasksTestEnv(t)
	env.seedTaskStates(t)

	response := env.listTasks(t, "?state=active&limit=1")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	first := decodeListBody(t, response)
	if first.NextCursor == nil {
		t.Fatalf("first page carries no cursor: %d items", len(first.Items))
	}
	cursor := url.QueryEscape(*first.NextCursor)

	for _, query := range []string{
		"?state=error&limit=1&cursor=" + cursor,
		"?state=active&limit=1&sort=name&cursor=" + cursor,
	} {
		response := env.listTasks(t, query)
		problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
		if len(problem.Errors) != 1 || problem.Errors[0].Location != "query.cursor" {
			t.Errorf("errors = %+v, want the query.cursor field error", problem.Errors)
		}
	}
}

// TestListTasksRejectsUnknownQueryKey pins FR-012's strictness: a mistyped
// filter key is a 422, never silently ignored.
func TestListTasksRejectsUnknownQueryKey(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.listTasks(t, "?stats=downloading")
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
}

// TestListTasksPresenceSurvivesMalformedPair pins the presence detection:
// a query with one malformed escape must not silently drop the empty
// category filter — the pairs that do decode stay authoritative.
func TestListTasksPresenceSurvivesMalformedPair(t *testing.T) {
	env := newTasksTestEnv(t)
	stateOf := env.seedTaskStates(t)

	// ?q=100% holds a lone percent sign that fails QueryUnescape; the
	// well-formed ?category= beside it still selects the uncategorised set.
	// The default state set omits the removed tombstone, so nine of the ten
	// seeded tasks remain.
	response := env.listTasks(t, "?q=100%&category=")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	body := decodeListBody(t, response)
	if body.Total != len(stateOf)-1 {
		t.Fatalf("total = %d, want the uncategorised subset of %d", body.Total, len(stateOf)-1)
	}
	for _, item := range body.Items {
		if item.Category != nil {
			t.Errorf("task %s carries category %q, want the uncategorised set", item.ID, *item.Category)
		}
	}
}

// TestListTasksLimitRange pins the documented limit range 1..500.
func TestListTasksLimitRange(t *testing.T) {
	env := newTasksTestEnv(t)

	for _, limit := range []string{"0", "501"} {
		response := env.listTasks(t, "?limit="+limit)
		assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	}
	for _, limit := range []string{"1", "500"} {
		response := env.listTasks(t, "?limit="+limit)
		if response.Code != http.StatusOK {
			t.Errorf("limit=%s: status = %d, want %d", limit, response.Code, http.StatusOK)
		}
	}
}

// TestGetTask pins GET /tasks/{id}: one Task object for a known id, the
// registered not-found problem for an unknown one.
func TestGetTask(t *testing.T) {
	env := newTasksTestEnv(t)

	created := decodeCreateBody(t, env.createTasks(t, map[string]any{"uris": []string{mixedHTTPS}})).Created
	if len(created) != 1 {
		t.Fatalf("seed failed: %d tasks created", len(created))
	}

	response := env.getTask(t, created[0].ID)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}

	var task TaskDTO
	if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode body %q: %v", response.Body.String(), err)
	}
	if task.ID != created[0].ID {
		t.Errorf("id = %q, want %q", task.ID, created[0].ID)
	}
	if task.SourceURI == nil || *task.SourceURI != mixedHTTPS {
		t.Errorf("source_uri = %v, want the display uri", task.SourceURI)
	}
	if task.State != "queued" {
		t.Errorf("state = %q, want queued", task.State)
	}

	response = env.getTask(t, "tsk_01JKQ8Z9YV6M3P0R2S4T6V8W0X")
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
}

// TestListAndGetHideFTPPassword pins FR-009 on the read path: the stored
// engine source carries the credentials, and neither the list nor the
// single-task response echoes them.
func TestListAndGetHideFTPPassword(t *testing.T) {
	env := newTasksTestEnv(t)

	created := decodeCreateBody(t, env.createTasks(t, map[string]any{
		"uris": []string{"ftp://mirror.example.org/pub/file.iso"},
		"ftp_credentials": map[string]string{
			"username": ftpUser,
			"password": ftpPassword,
		},
	})).Created
	if len(created) != 1 {
		t.Fatalf("seed failed: %d tasks created", len(created))
	}

	list := env.listTasks(t, "")
	if strings.Contains(list.Body.String(), ftpPassword) {
		t.Errorf("list response leaks the ftp password: %s", list.Body.String())
	}
	single := env.getTask(t, created[0].ID)
	if strings.Contains(single.Body.String(), ftpPassword) {
		t.Errorf("task response leaks the ftp password: %s", single.Body.String())
	}
}
