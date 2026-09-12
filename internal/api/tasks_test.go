package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/stretchr/testify/require"
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
	if body.Rejected[0].Detail != duplicateRepeatDetail {
		t.Errorf("repeat detail = %q, want %q", body.Rejected[0].Detail, duplicateRepeatDetail)
	}

	// The same magnet in a later submission hits the live row the same way;
	// with every URI refused it answers the all-rejected 422, its detail
	// naming the existing task id (doc 05 section 5.2's conflict rule).
	response = env.createTasks(t, map[string]any{"uris": []string{mixedMagnet}})
	problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugUnsupportedScheme)
	existingID := body.Created[0].ID
	if !strings.Contains(problem.Detail, duplicateDetail) || !strings.Contains(problem.Detail, existingID) {
		t.Errorf("detail = %q, want %q naming task %q", problem.Detail, duplicateDetail, existingID)
	}
	if env.countTasks(t) != 1 {
		t.Errorf("%d tasks after duplicate submissions, want 1", env.countTasks(t))
	}
}

// The two spellings of one v1 identity and a hybrid's second identity:
// every duplicate form the create path must recognise (FR-023).
const (
	// mixedMagnet's hash in the 32-character base32 form BEP 9 permits.
	duplicateBase32 = "R6ODUKY5JZPWA4MCSOSLLRWX5D42BMOC"

	fixtureV2Hash = "5a7f9c3b1d4e5f60718293a4b5c6d7e8f9a0b1c2d3e4f5061728394a5b6c7d8e"
)

// TestCreateTasksDuplicateTorrentForms walks every duplicate form of
// FR-023 through the create endpoint: the base32 spelling of a v1 magnet,
// a hybrid's v2 magnet, and the bare-hash forms of both — each rejected
// with /problems/conflict naming the row that holds the identity.
func TestCreateTasksDuplicateTorrentForms(t *testing.T) {
	env := newTasksTestEnv(t)

	// The winner: a task created from the hex magnet.
	created := decodeCreateBody(t, env.createTasks(t, map[string]any{"uris": []string{mixedMagnet}}))
	require.Len(t, created.Created, 1)
	winner := created.Created[0]

	// A hybrid of a second identity: a different v1 hash and the fixture's
	// v2, created once, so the v2-only forms below have a row to hit.
	hybridMagnet := "magnet:?xt=urn:btih:" + strings.Repeat("e", 40) + "&xt=urn:btmh:1220" + fixtureV2Hash
	hybrid := decodeCreateBody(t, env.createTasks(t, map[string]any{"uris": []string{hybridMagnet}}))
	require.Len(t, hybrid.Created, 1)
	if hybrid.Created[0].InfohashV1 == nil || hybrid.Created[0].InfohashV2 == nil {
		t.Fatalf("hybrid = %+v, want both hashes stored", hybrid.Created[0])
	}

	forms := []struct {
		name  string
		uri   string
		owner string // the task the detail must name
	}{
		{name: "base32 magnet", uri: "magnet:?xt=urn:btih:" + duplicateBase32, owner: winner.ID},
		{name: "v2 magnet", uri: "magnet:?xt=urn:btmh:1220" + fixtureV2Hash, owner: hybrid.Created[0].ID},
		{name: "bare v1", uri: strings.ToUpper(mixedMagnet[len(mixedMagnet)-40:]), owner: winner.ID},
		{name: "bare v2", uri: fixtureV2Hash, owner: hybrid.Created[0].ID},
	}
	uris := make([]string, 0, len(forms))
	for _, form := range forms {
		uris = append(uris, form.uri)
	}

	// One submission carrying every duplicate form plus a fresh https URI:
	// partial success — one task, four conflicts.
	body := decodeCreateBody(t, env.createTasks(t, map[string]any{"uris": append(uris, mixedHTTPS)}))
	if len(body.Created) != 1 {
		t.Fatalf("created %d tasks, want 1 (the https uri)", len(body.Created))
	}
	if len(body.Rejected) != len(forms) {
		t.Fatalf("rejected %d entries, want %d", len(body.Rejected), len(forms))
	}
	byURI := map[string]RejectedURI{}
	for _, entry := range body.Rejected {
		byURI[entry.URI] = entry
	}
	for _, form := range forms {
		entry, ok := byURI[form.uri]
		if !ok {
			t.Errorf("form %s (%q) has no rejected entry", form.name, form.uri)
			continue
		}
		if entry.Type != SlugConflict {
			t.Errorf("form %s: type = %q, want %q", form.name, entry.Type, SlugConflict)
		}
		if !strings.Contains(entry.Detail, form.owner) {
			t.Errorf("form %s: detail = %q, want it to name task %q", form.name, entry.Detail, form.owner)
		}
	}

	// Three rows: the winner, the hybrid, the https task — no fourth.
	if count := env.countTasks(t); count != 3 {
		t.Errorf("%d tasks after every duplicate form, want 3", count)
	}
}

// TestCreateTasksHybridAndBareOverlap pins the within-submission set's
// per-hash keying: one submission holding a hybrid magnet and the same
// torrent's bare v1 hash — keys that share one hash but not both — is a
// duplicate, not a constraint failure at insert time.
func TestCreateTasksHybridAndBareOverlap(t *testing.T) {
	env := newTasksTestEnv(t)

	hybridV1 := strings.Repeat("e", 40)
	hybrid := "magnet:?xt=urn:btih:" + hybridV1 + "&xt=urn:btmh:1220" + fixtureV2Hash

	body := decodeCreateBody(t, env.createTasks(t, map[string]any{
		"uris": []string{hybrid, hybridV1, fixtureV2Hash},
	}))
	if len(body.Created) != 1 {
		t.Fatalf("created %d tasks, want 1", len(body.Created))
	}
	if len(body.Rejected) != 2 {
		t.Fatalf("rejected %d entries, want 2", len(body.Rejected))
	}
	for _, entry := range body.Rejected {
		if entry.Type != SlugConflict || entry.Detail != duplicateRepeatDetail {
			t.Errorf("entry = %+v, want a within-submission conflict", entry)
		}
	}
	if count := env.countTasks(t); count != 1 {
		t.Errorf("%d tasks, want 1", count)
	}
}

// TestCreateTasksBareInfohash pins the bare-infohash lane of routing-table
// row 2 (docs/06 section 2): a bare 40-hex or 64-hex submission becomes a
// magnet task of its own hash, stored lowercase, routed to qBittorrent.
func TestCreateTasksBareInfohash(t *testing.T) {
	env := newTasksTestEnv(t)

	body := decodeCreateBody(t, env.createTasks(t, map[string]any{
		"uris": []string{strings.ToUpper(mixedMagnet[len(mixedMagnet)-40:]), fixtureV2Hash},
	}))
	if len(body.Created) != 2 {
		t.Fatalf("created %d tasks, want 2", len(body.Created))
	}

	v1Task, v2Task := body.Created[0], body.Created[1]
	if v1Task.Engine != engine.NameQBittorrent || v1Task.SourceKind != "magnet" {
		t.Errorf("bare v1 task = %s/%s, want qbittorrent/magnet", v1Task.Engine, v1Task.SourceKind)
	}
	if v1Task.InfohashV1 == nil || *v1Task.InfohashV1 != mixedMagnet[len(mixedMagnet)-40:] {
		t.Errorf("bare v1 infohash = %v, want the lowercase hash", v1Task.InfohashV1)
	}
	if v1Task.InfohashV2 != nil {
		t.Errorf("bare v1 task carries a v2 hash: %v", *v1Task.InfohashV2)
	}
	if v1Task.SourceURI == nil || *v1Task.SourceURI != mixedMagnet {
		t.Errorf("bare v1 source = %v, want the rebuilt magnet %q", v1Task.SourceURI, mixedMagnet)
	}

	if v2Task.InfohashV2 == nil || *v2Task.InfohashV2 != fixtureV2Hash {
		t.Errorf("bare v2 infohash = %v, want %q", v2Task.InfohashV2, fixtureV2Hash)
	}
	if v2Task.InfohashV1 != nil {
		t.Errorf("bare v2 task carries a v1 hash: %v", *v2Task.InfohashV1)
	}
	wantV2Source := "magnet:?xt=urn:btmh:1220" + fixtureV2Hash
	if v2Task.SourceURI == nil || *v2Task.SourceURI != wantV2Source {
		t.Errorf("bare v2 source = %v, want %q", v2Task.SourceURI, wantV2Source)
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

// minimalTorrentBytes is one minimal, valid v1 .torrent: a single 1 KiB
// file, one all-'A' piece hash. An Add-time identity fetch of a .torrent
// URL parses it; its actual hash value is irrelevant to the test — the
// mirror daemon reports the identity, never the bytes.
const minimalTorrentBytes = "d4:infod6:lengthi1024e4:name8:file.bin12:piece lengthi16384e6:pieces20:AAAAAAAAAAAAAAAAAAAAee"

// mirroredTask is one live qbittorrent row the mirror daemon reports.
type mirroredTask struct {
	EngineRef string `db:"engine_ref"`
	State     string `db:"state"`
}

// qbMirrorDaemon is a stand-in qBittorrent daemon for the late-resolution
// test. It answers the probes Connect makes, the admission pass's mutating
// calls, and serves minimal torrent bytes for an Add-time identity fetch.
// Its sync/maindata reports one torrent per live engine_ref whose resolved
// identity is the collision hash — the shape FR-023's late half needs —
// with the state the DAEMON holds: a torrent it was told to stop stays
// stopped no matter what the row does, because a real daemon's state is
// its own, never a mirror of dl-tool's rows (mirroring the row back would
// let a transient row flip erase an engine-side pause).
type qbMirrorDaemon struct {
	srv *httptest.Server
	db  *sqlx.DB
	// collision is the infohash_v1 every mirrored torrent reports.
	collision string
	// stopped holds the hashes the daemon was told to stop and deleted the
	// ones it was told to remove, so a test can assert a duplicate was
	// stopped, never deleted. Only a start call clears a stop. Guarded by
	// mu.
	mu      sync.Mutex
	stopped map[string]bool
	deleted map[string]bool
}

// newQBMirrorDaemon starts the daemon over db.
func newQBMirrorDaemon(t *testing.T, db *sqlx.DB, collision string) *qbMirrorDaemon {
	t.Helper()

	f := &qbMirrorDaemon{db: db, collision: collision, stopped: map[string]bool{}, deleted: map[string]bool{}}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)

	return f
}

// ServeHTTP routes one WebAPI call. Unknown paths under /api/v2/torrents/
// answer Ok. — the mutating family the admission pass may call.
func (f *qbMirrorDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/v2/auth/login":
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: "late-dup", Path: "/"})
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/api/v2/app/version":
		_, _ = w.Write([]byte("v5.2.3"))
	case r.URL.Path == "/api/v2/app/webapiVersion":
		_, _ = w.Write([]byte("2.11.2"))
	case r.URL.Path == "/api/v2/sync/maindata":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.maindataBody()))
	case r.URL.Path == "/api/v2/torrents/add":
		// A pending add: the client keeps the identity it resolved itself.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"success_count":0,"pending_count":1,"failure_count":0}`))
	case strings.HasPrefix(r.URL.Path, "/api/v2/torrents/"):
		// The mutating family: stop records the daemon-side stop, start
		// clears it, everything else is a silent Ok. The hashes travel
		// form-encoded, so ParseForm, not the query string.
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)

			return
		}
		if strings.HasSuffix(r.URL.Path, "/stop") || strings.HasSuffix(r.URL.Path, "/pause") {
			for _, hash := range pipeJoinedHashes(r) {
				f.setStopped(hash, true)
			}
		}
		if strings.HasSuffix(r.URL.Path, "/start") || strings.HasSuffix(r.URL.Path, "/resume") {
			for _, hash := range pipeJoinedHashes(r) {
				f.setStopped(hash, false)
			}
		}
		if strings.HasSuffix(r.URL.Path, "/delete") {
			for _, hash := range pipeJoinedHashes(r) {
				f.mu.Lock()
				f.deleted[hash] = true
				f.mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Ok."))
	case strings.HasSuffix(strings.ToLower(r.URL.Path), ".torrent"):
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = w.Write([]byte(minimalTorrentBytes))
	default:
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}

// pipeJoinedHashes splits the hashes form value the daemon's mutators
// carry: one pipe-joined string per the WebAPI, not one value per hash.
func pipeJoinedHashes(r *http.Request) []string {
	var hashes []string
	for _, joined := range r.Form["hashes"] {
		hashes = append(hashes, strings.Split(joined, "|")...)
	}

	return hashes
}

// setStopped records one hash's daemon-side stop state.
func (f *qbMirrorDaemon) setStopped(hash string, stopped bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.stopped[hash] = stopped
}

// isStopped reports one hash's daemon-side stop state.
func (f *qbMirrorDaemon) isStopped(hash string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.stopped[hash]
}

// isDeleted reports whether the daemon was ever told to remove the hash.
func (f *qbMirrorDaemon) isDeleted(hash string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.deleted[hash]
}

// mirrorDaemonState maps a task row's state onto the daemon state string
// that normalises back to it (docs/06-download-engines.md section 5.6);
// a hash the daemon was told to stop reports stoppedDL whatever the row
// says — the daemon's state is its own.
func mirrorDaemonState(rowState string) string {
	switch rowState {
	case "paused":
		return "pausedDL"
	case "seeding":
		return "stalledUP"
	case "checking", "extracting", "moving":
		return "checkingDL"
	default:
		// queued and downloading both report metadata downloading.
		return "metaDL"
	}
}

// maindataBody renders one full_update over the live rows.
func (f *qbMirrorDaemon) maindataBody() string {
	var rows []mirroredTask
	// A read failure mirrors as an empty daemon: the poll keeps its last
	// accepted state and retries, which the test tolerates.
	_ = f.db.Select(&rows, `SELECT engine_ref, state FROM tasks
WHERE engine = 'qbittorrent' AND engine_ref IS NOT NULL AND state NOT IN ('completed', 'removed', 'error')
ORDER BY engine_ref`)

	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		state := mirrorDaemonState(row.State)
		if f.isStopped(row.EngineRef) {
			state = "stoppedDL"
		}
		parts = append(parts, fmt.Sprintf(
			`%q:{"hash":%q,"name":"m-%s","state":%q,"progress":0.5,"dlspeed":0,"completed":4096,"size":4096,"total_size":4096,"infohash_v1":%q,"infohash_v2":""}`,
			row.EngineRef, row.EngineRef, row.EngineRef[:6], state, f.collision,
		))
	}

	return `{"full_update":true,"rid":1,"torrents":{` + strings.Join(parts, ",") + `}}`
}

// newLateResolutionEnv builds the server against the mirror daemon: a real
// qBittorrent adapter constructed, registered and infohash-wired by
// NewServer itself, so the test observes the write-back through the
// composition root. No recording stand-in takes the qBittorrent lane.
func newLateResolutionEnv(t *testing.T) (*tasksTestEnv, *qbMirrorDaemon) {
	t.Helper()

	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	if err := os.Mkdir(dataRoot, 0o755); err != nil {
		t.Fatalf("make data root: %v", err)
	}
	configDir := filepath.Join(root, "config")
	db, err := store.Open(t.Context(), filepath.Join(configDir, "dl-tool.db"), filepath.Join(root, "backups"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	daemon := newQBMirrorDaemon(t, db, mixedMagnet[len(mixedMagnet)-40:])

	logs := &strings.Builder{}
	server, err := NewServer(
		&config.Config{
			ConfigDir:       configDir,
			SessionTTL:      time.Hour,
			DataRoots:       []string{dataRoot},
			QBittorrentURL:  daemon.srv.URL,
			QBittorrentUser: "admin",
			QBittorrentPass: secure.Secret("adminadmin"),
		},
		db,
		slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	env := &tasksTestEnv{
		api:      humatest.Wrap(t, server.API),
		db:       db,
		logs:     logs,
		dataRoot: dataRoot,
	}
	// No recording stand-in for any lane: the real qBittorrent adapter the
	// server built is the only engine this test submits to.

	user := seedUser(t, db)
	env.bearer = seedLiveAPIToken(t, db, user.ID)

	return env, daemon
}

// TestLateDuplicatePausesTask pins FR-023's late half end to end, through
// the composition root: a task created from a .torrent URL carries no
// identity, and when the daemon's delta later resolves it onto a hash a
// live task already holds, the write-back pauses the task in place —
// error_code torrent_duplicate, one event row, the row and its counters
// intact, the winner untouched, and the engine-side transfer stopped
// rather than deleted.
func TestLateDuplicatePausesTask(t *testing.T) {
	env, daemon := newLateResolutionEnv(t)
	tasks := store.NewTaskStore(env.db)

	// The winner: created from the magnet, so it owns the identity already.
	winnerBody := decodeCreateBody(t, env.createTasks(t, map[string]any{"uris": []string{mixedMagnet}}))
	if len(winnerBody.Created) != 1 {
		t.Fatalf("created %d winner tasks, want 1", len(winnerBody.Created))
	}
	winnerID := winnerBody.Created[0].ID

	// The late task: a .torrent URL has no identity at create time.
	lateBody := decodeCreateBody(t, env.createTasks(t, map[string]any{
		"uris": []string{daemon.srv.URL + "/fixture.torrent"},
	}))
	if len(lateBody.Created) != 1 {
		t.Fatalf("created %d late tasks, want 1", len(lateBody.Created))
	}
	lateID := lateBody.Created[0].ID
	if lateBody.Created[0].InfohashV1 != nil || lateBody.Created[0].InfohashV2 != nil {
		t.Fatalf("late task already carries an identity: %+v", lateBody.Created[0])
	}

	// The late task goes live mid-transfer: a handle and some progress.
	// The live admission pass races this seeding — it may release the task
	// (or the write-back may even resolve and pause it) before the seed
	// lands — so a refusal here is tolerated whenever the row already sits
	// in the end state the test asserts anyway.
	lateRef := strings.Repeat("d", 40)
	if err := tasks.SetEngineRef(t.Context(), lateID, lateRef); err != nil {
		t.Fatalf("set engine ref: %v", err)
	}
	seeded := true
	if err := tasks.Transition(t.Context(), lateID, "downloading", engine.CodeTaskReconciled, "test move"); err != nil {
		current, readErr := tasks.Get(t.Context(), lateID)
		if readErr != nil || current.ErrorCode == nil || *current.ErrorCode != "torrent_duplicate" {
			t.Fatalf("move to downloading: %v", err)
		}
		seeded = false
	}
	if seeded {
		total := int64(4096)
		if err := tasks.UpdateProgress(t.Context(), lateID, store.Progress{TotalBytes: &total, CompletedBytes: 4096}); err != nil {
			t.Fatalf("seed progress: %v", err)
		}
	}

	// The daemon's delta resolves the late task onto the winner's hash;
	// the write-back lands the pause. Everything here is 1 Hz, so the
	// budget is generous but the landing typically takes a few ticks.
	require.Eventually(t, func() bool {
		task, err := tasks.Get(t.Context(), lateID)
		if err != nil {
			return false
		}
		return task.State == "paused" && task.ErrorCode != nil && *task.ErrorCode == "torrent_duplicate"
	}, 20*time.Second, 100*time.Millisecond, "the late duplicate was never paused")

	// The pause deleted nothing: the row and its counters survive intact.
	// In the unseeded ordering (the write-back won the seeding race) the
	// mirror daemon's own reporting supplies the 4096 shortly; the seeded
	// ordering pinned it exactly.
	late, err := tasks.Get(t.Context(), lateID)
	if err != nil {
		t.Fatalf("the paused duplicate row vanished: %v", err)
	}
	if seeded && late.CompletedBytes != 4096 {
		t.Errorf("completed_bytes = %d, want 4096 unchanged", late.CompletedBytes)
	}
	if late.EngineRef == nil || *late.EngineRef == "" {
		t.Errorf("engine_ref = %v, want the handle kept", late.EngineRef)
	}

	// Exactly one duplicate-pause event row, beside task.created and the
	// reconciler's own adoptions.
	var events []store.TaskEvent
	if err := env.db.SelectContext(t.Context(), &events,
		`SELECT id, task_id, at, level, code, message, detail_json, created_at, updated_at
		 FROM task_events WHERE task_id = ? AND code = ?`,
		lateID, store.CodeTaskDuplicatePaused); err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("%d duplicate-pause events, want exactly 1", len(events))
	}

	// The daemon holds the transfer stopped, not deleted: the write-back
	// paused it engine-side and removed nothing (FR-023's "deletes
	// nothing" covers the engine side too). The store pause lands before
	// the engine call completes, so the stop gets its own wait. The row's
	// engine_ref is the handle whatever seeding won the race.
	if late.EngineRef == nil || *late.EngineRef == "" {
		t.Fatalf("engine_ref = %v, want the handle kept", late.EngineRef)
	}
	require.Eventually(t, func() bool { return daemon.isStopped(*late.EngineRef) },
		10*time.Second, 50*time.Millisecond, "the daemon was never told to stop the duplicate transfer")
	if daemon.isDeleted(*late.EngineRef) {
		t.Errorf("the daemon was told to delete the duplicate transfer; the write-back must never delete")
	}

	// The winner is untouched by the collision.
	winner, err := tasks.Get(t.Context(), winnerID)
	if err != nil {
		t.Fatalf("get winner: %v", err)
	}
	if winner.State == "paused" || winner.ErrorCode != nil {
		t.Errorf("winner = %s/%v, want live and unflagged", winner.State, winner.ErrorCode)
	}
}
