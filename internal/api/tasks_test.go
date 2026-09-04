package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
