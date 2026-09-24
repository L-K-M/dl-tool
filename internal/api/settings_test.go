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
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/engine/aria2"
	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

type conformProbeMode int

const (
	conformProbeOK conformProbeMode = iota
	conformProbeFailure
)

// conformAPI exercises the real adapter through NewServer, not a registered stand-in.
func conformAPI(t *testing.T, dirSuffix string, mode conformProbeMode) (*settingsTestEnv, *conformRPC) {
	t.Helper()
	root := t.TempDir()
	rpc := &conformRPC{dir: root + dirSuffix, concurrency: "2", fail: mode == conformProbeFailure}
	daemon := httptest.NewServer(rpc)
	t.Cleanup(daemon.Close)
	db, err := store.Open(t.Context(), filepath.Join(root, "dl-tool.db"), filepath.Join(root, "backups"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	server, err := NewServer(&config.Config{ConfigDir: root, SessionTTL: time.Hour, DataRoots: []string{root}, Aria2URL: daemon.URL}, db, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	require.NoError(t, err)
	adapter, ok := server.Engines.Get(engine.NameAria2)
	require.True(t, ok)
	t.Cleanup(func() { require.NoError(t, adapter.Close()) })
	// Registered after the engine's cleanup, so the background loops stop
	// before the engine they poll closes, not just before the store.
	t.Cleanup(server.Shutdown)
	user := seedUser(t, db)
	return &settingsTestEnv{api: humatest.Wrap(t, server.API), db: db, server: server, bearer: seedLiveAPIToken(t, db, user.ID)}, rpc
}

type conformRPC struct {
	mu               sync.Mutex
	dir, concurrency string
	reads            int
	fail             bool
}

func (f *conformRPC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var request struct {
		ID     any               `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "bad RPC", http.StatusBadRequest)
		return
	}
	var result any = []any{}
	switch request.Method {
	case "aria2.getVersion":
		result = map[string]string{"version": aria2Version}
	case "aria2.getGlobalOption":
		f.reads++
		if f.fail {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		result = map[string]string{"max-concurrent-downloads": f.concurrency, "dir": f.dir, "save-session": "session"}
	case "aria2.changeGlobalOption":
		var options map[string]string
		if len(request.Params) != 2 || string(request.Params[0]) != `"token:"` || json.Unmarshal(request.Params[1], &options) != nil {
			http.Error(w, "bad options", http.StatusBadRequest)
			return
		}
		f.concurrency = options["max-concurrent-downloads"]
		result = "OK"
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}); err != nil {
		http.Error(w, "encode RPC", http.StatusInternalServerError)
	}
}

func TestConformConfiguredRoots(t *testing.T) {
	for _, suffix := range []string{"", "-sibling"} {
		t.Run(suffix, func(t *testing.T) {
			env, rpc := conformAPI(t, suffix, conformProbeOK)
			row := engineByID(decodeEngines(t, env.listEngines(t)), store.EngineIDAria2)
			require.NotNil(t, row)
			require.NotNil(t, row.LastError, "boot correction must remain visible")
			require.Contains(t, *row.LastError, "max-concurrent-downloads")
			if suffix != "" {
				require.Contains(t, *row.LastError, "dir")
			} else {
				require.NotContains(t, *row.LastError, "dir")
			}
			require.True(t, row.Connected, "conformance warnings must preserve health")
			require.NotNil(t, row.Version)
			require.Equal(t, aria2Version, *row.Version)
			require.NotNil(t, row.LastSeenAt)
			rpc.mu.Lock()
			defer rpc.mu.Unlock()
			require.Equal(t, 1, rpc.reads, "listing must not probe")
			require.Equal(t, "5", rpc.concurrency)
		})
	}
}

func TestConformNeverFailsBoot(t *testing.T) {
	env, _ := conformAPI(t, "", conformProbeFailure)
	row := engineByID(decodeEngines(t, env.listEngines(t)), store.EngineIDAria2)
	require.NotNil(t, row)
	require.True(t, row.Connected)
	require.NotNil(t, row.LastError)
	require.Contains(t, *row.LastError, "max-concurrent-downloads")
	require.Contains(t, *row.LastError, "warn")
}

func TestConformTestEndpoint(t *testing.T) {
	env, rpc := conformAPI(t, "", conformProbeOK)
	_, err := env.db.ExecContext(t.Context(), "UPDATE settings SET value_json = ? WHERE key = ?", "8", settingMaxActiveTotal)
	require.NoError(t, err)
	response := env.testEngine(t, store.EngineIDAria2)
	require.Equal(t, http.StatusOK, response.Code)
	require.True(t, decodeTestEngine(t, response).Body.Ok)
	rpc.mu.Lock()
	require.Equal(t, "8", rpc.concurrency)
	require.Equal(t, 2, rpc.reads)
	rpc.mu.Unlock()
	row := engineByID(decodeEngines(t, env.listEngines(t)), store.EngineIDAria2)
	require.NotNil(t, row.LastError)
	require.Contains(t, *row.LastError, "max-concurrent-downloads")

	// A clean correction clears the previous warning; failed conformance retains health.
	require.True(t, decodeTestEngine(t, env.testEngine(t, store.EngineIDAria2)).Body.Ok)
	row = engineByID(decodeEngines(t, env.listEngines(t)), store.EngineIDAria2)
	require.Nil(t, row.LastError)
	rpc.mu.Lock()
	rpc.fail = true
	rpc.mu.Unlock()
	require.True(t, decodeTestEngine(t, env.testEngine(t, store.EngineIDAria2)).Body.Ok)
	row = engineByID(decodeEngines(t, env.listEngines(t)), store.EngineIDAria2)
	require.True(t, row.Connected)
	require.NotNil(t, row.LastError)
	require.Contains(t, *row.LastError, "max-concurrent-downloads")
}

// Secret sentinels seeded into secret_enc. GET /engines must never carry
// them: the store query never selects the column, and the test proves the
// whole response body agrees.
const (
	aria2SecretSentinel = "aria2-secret-ciphertext"
	qbtSecretSentinel   = "qbt-secret-ciphertext"

	// rpcSecret is the DLTOOL_ARIA2_SECRET stand-in handed to the real
	// adapter in the failure tests; no response may echo it.
	rpcSecret = "aria2-rpc-secret-0123456789abcdef"

	aria2Version = "1.37.0"
	aria2RPCURL  = "http://aria2:6800/jsonrpc"
	qbtBaseURL   = "http://qbittorrent:8080"
)

// settingsTestEnv is one humatest server against a real migrated store,
// with the auth gate satisfied by a seeded bearer token. Engines are
// registered per test, so each case controls the registry's contents.
type settingsTestEnv struct {
	api      humatest.TestAPI
	db       *sqlx.DB
	server   *Server
	bearer   string
	dataRoot string
	dbPath   string
}

func newSettingsTestEnv(t *testing.T) *settingsTestEnv {
	t.Helper()

	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	dataRoot := filepath.Join(root, "data")
	dbPath := filepath.Join(configDir, "dl-tool.db")
	db, err := store.Open(
		t.Context(),
		dbPath,
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
		&config.Config{
			ConfigDir:  configDir,
			DBPath:     dbPath,
			SessionTTL: time.Hour,
			DataRoots:  []string{dataRoot},
		},
		db,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// Registered after the store's cleanup, so the background loops stop
	// before the database they poll closes.
	t.Cleanup(server.Shutdown)

	user := seedUser(t, db)
	env := &settingsTestEnv{
		api:      humatest.Wrap(t, server.API),
		db:       db,
		server:   server,
		bearer:   seedLiveAPIToken(t, db, user.ID),
		dataRoot: dataRoot,
		dbPath:   dbPath,
	}

	return env
}

// listEngines calls GET /engines with the test bearer credential.
func (e *settingsTestEnv) listEngines(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Get("/engines", "Authorization: Bearer "+e.bearer)
}

// testEngine posts the probe with the test bearer credential.
func (e *settingsTestEnv) testEngine(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Post("/engines/"+id+"/test", http.NoBody, "Authorization: Bearer "+e.bearer)
}

// getSettings calls GET /settings with the test bearer credential.
func (e *settingsTestEnv) getSettings(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Get("/settings", "Authorization: Bearer "+e.bearer)
}

// patchSettings calls PATCH /settings with the test bearer credential.
func (e *settingsTestEnv) patchSettings(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Patch("/settings", body, "Authorization: Bearer "+e.bearer)
}

// getSystemInfo calls GET /system/info with the test bearer credential.
func (e *settingsTestEnv) getSystemInfo(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Get("/system/info", "Authorization: Bearer "+e.bearer)
}

// decodeSettingsBody decodes the flat settings object.
func decodeSettingsBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode settings body %q: %v", recorder.Body.String(), err)
	}

	return body
}

// probeEngine is a stand-in whose Health answers a fixed version and whose
// declared capability set is distinctive, so the test can prove the list
// renders the declared set rather than a guess.
type probeEngine struct {
	name    string
	version string
}

func (e *probeEngine) Name() string { return e.name }
func (e *probeEngine) Capabilities() []engine.Capability {
	return []engine.Capability{engine.CapFTP, engine.CapHTTP, engine.CapMetalink}
}
func (e *probeEngine) Accepts(string) bool                    { return true }
func (e *probeEngine) Connect(context.Context) error          { return nil }
func (e *probeEngine) Close() error                           { return nil }
func (e *probeEngine) Health(context.Context) (string, error) { return e.version, nil }
func (e *probeEngine) Add(context.Context, engine.AddRequest) (string, error) {
	return "", engine.ErrNotSupported
}
func (e *probeEngine) List(context.Context) ([]engine.TaskInfo, error) { return nil, nil }
func (e *probeEngine) Get(context.Context, string) (engine.TaskInfo, error) {
	return engine.TaskInfo{}, engine.ErrNotFound
}
func (e *probeEngine) Files(context.Context, string) ([]engine.FileEntry, error) {
	return nil, engine.ErrNotSupported
}
func (e *probeEngine) Pause(context.Context, string) error  { return engine.ErrNotSupported }
func (e *probeEngine) Resume(context.Context, string) error { return engine.ErrNotSupported }
func (e *probeEngine) Remove(context.Context, string) error { return engine.ErrNotSupported }
func (e *probeEngine) SetFiles(context.Context, string, []int, map[int]int) error {
	return engine.ErrNotSupported
}
func (e *probeEngine) SetLocation(context.Context, string, string) error {
	return engine.ErrNotSupported
}
func (e *probeEngine) Rename(context.Context, string, string) error {
	return engine.ErrNotSupported
}
func (e *probeEngine) SetCategory(context.Context, string, string) error {
	return engine.ErrNotSupported
}
func (e *probeEngine) SetRateLimits(context.Context, string, *int64, *int64) error {
	return engine.ErrNotSupported
}
func (e *probeEngine) SetShareLimits(context.Context, string, *float64, *int64) error {
	return engine.ErrNotSupported
}
func (e *probeEngine) Events(context.Context) (<-chan engine.TaskEvent, error) {
	return make(chan engine.TaskEvent), nil
}

// seedEngineRow inserts one engines row straight through the store so a
// test controls every column, including the secret_enc sentinel the
// response must never carry.
func seedEngineRow(t *testing.T, db *sqlx.DB, id, kind, name, url, secretEnc string) {
	t.Helper()

	now := time.Now().UnixMilli()
	_, err := db.ExecContext(t.Context(), `INSERT INTO engines
(id, kind, name, enabled, url, secret_enc, created_at, updated_at)
VALUES (?, ?, ?, 1, ?, ?, ?, ?)`,
		id, kind, name, url, secretEnc, now, now,
	)
	if err != nil {
		t.Fatalf("seed engine %s: %v", id, err)
	}
}

// decodeEngines decodes the GET /engines body.
func decodeEngines(t *testing.T, recorder *httptest.ResponseRecorder) []EngineDTO {
	t.Helper()

	var body ListEnginesOutput
	if err := json.Unmarshal(recorder.Body.Bytes(), &body.Body); err != nil {
		t.Fatalf("decode engines body %q: %v", recorder.Body.String(), err)
	}

	return body.Body.Engines
}

// decodeTestEngine decodes the POST /engines/{id}/test body.
func decodeTestEngine(t *testing.T, recorder *httptest.ResponseRecorder) TestEngineOutput {
	t.Helper()

	var output TestEngineOutput
	if err := json.Unmarshal(recorder.Body.Bytes(), &output.Body); err != nil {
		t.Fatalf("decode test-engine body %q: %v", recorder.Body.String(), err)
	}

	return output
}

// engineByID finds one entry of a decoded list.
func engineByID(engines []EngineDTO, id string) *EngineDTO {
	for i := range engines {
		if engines[i].ID == id {
			return &engines[i]
		}
	}

	return nil
}

func TestListEngines(t *testing.T) {
	env := newSettingsTestEnv(t)
	env.server.Engines.Register(&probeEngine{name: engine.NameAria2, version: aria2Version})
	seedEngineRow(t, env.db, store.EngineIDAria2, engine.NameAria2, engine.NameAria2, aria2RPCURL, aria2SecretSentinel)
	seedEngineRow(t, env.db, store.EngineIDQBittorrent, engine.NameQBittorrent, "qBittorrent", qbtBaseURL, qbtSecretSentinel)

	// Seed the qbittorrent row with a recorded failure — a last successful
	// contact followed by a refused probe, exactly the doc 05 11.3 example.
	lastSeen := time.Now().Add(-30 * time.Minute).UnixMilli()
	lastError := "dial tcp: connection refused"
	if _, err := env.db.ExecContext(t.Context(),
		`UPDATE engines SET last_seen_at = ?, last_error = ? WHERE id = ?`,
		lastSeen, lastError, store.EngineIDQBittorrent,
	); err != nil {
		t.Fatalf("seed qbittorrent probe state: %v", err)
	}

	// A probe of the healthy stub must precede the list, because the list
	// never dials: connected is the recorded outcome, not a live check.
	probe := env.testEngine(t, store.EngineIDAria2)
	if probe.Code != http.StatusOK {
		t.Fatalf("test engine status = %d, want 200; body %s", probe.Code, probe.Body.String())
	}
	probeBody := decodeTestEngine(t, probe)
	if !probeBody.Body.Ok || probeBody.Body.Version == nil || *probeBody.Body.Version != aria2Version {
		t.Fatalf("probe outcome = ok:%v version:%v, want ok:true version:%s", probeBody.Body.Ok, probeBody.Body.Version, aria2Version)
	}
	if probeBody.Body.ElapsedMS < 1 {
		t.Fatalf("probe elapsed_ms = %d, want at least 1", probeBody.Body.ElapsedMS)
	}
	if strings.Contains(probe.Body.String(), aria2SecretSentinel) || strings.Contains(probe.Body.String(), rpcSecret) {
		t.Fatalf("probe body leaked a secret: %s", probe.Body.String())
	}

	recorder := env.listEngines(t)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list engines status = %d, want 200; body %s", recorder.Code, recorder.Body.String())
	}

	engines := decodeEngines(t, recorder)
	if len(engines) != 2 {
		t.Fatalf("engines listed %d entries, want 2: %+v", len(engines), engines)
	}

	aria2Row := engineByID(engines, store.EngineIDAria2)
	if aria2Row == nil {
		t.Fatalf("engines list holds no %s row: %+v", store.EngineIDAria2, engines)
	}
	if !aria2Row.Connected {
		t.Fatalf("aria2 connected = false after a successful probe: %+v", aria2Row)
	}
	if aria2Row.Version == nil || *aria2Row.Version != aria2Version {
		t.Fatalf("aria2 version = %v, want %s", aria2Row.Version, aria2Version)
	}
	if aria2Row.URL == nil || *aria2Row.URL != aria2RPCURL {
		t.Fatalf("aria2 url = %v, want %s", aria2Row.URL, aria2RPCURL)
	}
	wantCapabilities := []string{"ftp", "http", "metalink"}
	if strings.Join(aria2Row.Capabilities, ",") != strings.Join(wantCapabilities, ",") {
		t.Fatalf("aria2 capabilities = %v, want the declared set %v", aria2Row.Capabilities, wantCapabilities)
	}
	if aria2Row.LastSeenAt == nil {
		t.Fatal("aria2 last_seen_at = nil after a successful probe")
	}
	if _, err := time.Parse(time.RFC3339, *aria2Row.LastSeenAt); err != nil {
		t.Fatalf("aria2 last_seen_at %q is not RFC 3339: %v", *aria2Row.LastSeenAt, err)
	}

	// The qbittorrent row has no engine in this process: it still lists,
	// with empty capabilities, connected false and its recorded error.
	qbtRow := engineByID(engines, store.EngineIDQBittorrent)
	if qbtRow == nil {
		t.Fatalf("engines list holds no %s row: %+v", store.EngineIDQBittorrent, engines)
	}
	if qbtRow.Connected {
		t.Fatalf("qbittorrent connected = true with a recorded failure: %+v", qbtRow)
	}
	if len(qbtRow.Capabilities) != 0 {
		t.Fatalf("qbittorrent capabilities = %v, want empty", qbtRow.Capabilities)
	}
	if qbtRow.LastError == nil || *qbtRow.LastError != lastError {
		t.Fatalf("qbittorrent last_error = %v, want %q", qbtRow.LastError, lastError)
	}

	if strings.Contains(recorder.Body.String(), aria2SecretSentinel) ||
		strings.Contains(recorder.Body.String(), qbtSecretSentinel) {
		t.Fatalf("engines body leaked a secret_enc value: %s", recorder.Body.String())
	}
}

func TestTestEngineFailureIs200(t *testing.T) {
	env := newSettingsTestEnv(t)

	// The real adapter against a server that is already closed: the probe
	// fails with the adapter's transport error, carrying the configured RPC
	// secret nowhere a response could echo it.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	stopped, err := aria2.New(aria2.Config{URL: deadURL, Secret: rpcSecret}, nil)
	if err != nil {
		t.Fatalf("build stopped aria2 engine: %v", err)
	}
	t.Cleanup(func() {
		if err := stopped.Close(); err != nil {
			t.Errorf("close stopped engine: %v", err)
		}
	})
	env.server.Engines.Register(stopped)
	seedEngineRow(t, env.db, store.EngineIDAria2, engine.NameAria2, engine.NameAria2, deadURL, aria2SecretSentinel)

	recorder := env.testEngine(t, store.EngineIDAria2)
	if recorder.Code != http.StatusOK {
		t.Fatalf("test engine status = %d, want 200 even for a failed probe; body %s", recorder.Code, recorder.Body.String())
	}

	output := decodeTestEngine(t, recorder)
	if output.Body.Ok {
		t.Fatalf("probe ok = true against a stopped engine: %+v", output.Body)
	}
	if output.Body.Error == nil || *output.Body.Error == "" {
		t.Fatal("probe error = nil, want the transport error")
	}
	// The adapter wraps every transport failure in ErrUnavailable.
	if !strings.Contains(*output.Body.Error, engine.ErrUnavailable.Error()) {
		t.Fatalf("probe error %q does not carry the unavailability sentinel", *output.Body.Error)
	}
	if output.Body.Version != nil {
		t.Fatalf("probe version = %v against a stopped engine, want nil", *output.Body.Version)
	}
	if output.Body.ElapsedMS < 1 {
		t.Fatalf("probe elapsed_ms = %d, want at least 1", output.Body.ElapsedMS)
	}
	if strings.Contains(recorder.Body.String(), rpcSecret) ||
		strings.Contains(recorder.Body.String(), aria2SecretSentinel) {
		t.Fatalf("probe body leaked a secret: %s", recorder.Body.String())
	}

	// The failed outcome is recorded: the list renders it without dialing.
	engines := decodeEngines(t, env.listEngines(t))
	aria2Row := engineByID(engines, store.EngineIDAria2)
	if aria2Row == nil {
		t.Fatalf("engines list holds no %s row: %+v", store.EngineIDAria2, engines)
	}
	if aria2Row.Connected {
		t.Fatalf("aria2 connected = true after a failed probe: %+v", aria2Row)
	}
	if aria2Row.LastError == nil {
		t.Fatal("aria2 last_error = nil after a failed probe")
	}
}

func TestTestEngineUnknownID(t *testing.T) {
	env := newSettingsTestEnv(t)

	recorder := env.testEngine(t, "eng_nosuchengine")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("test engine status = %d, want 404; body %s", recorder.Code, recorder.Body.String())
	}

	var problem struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem body %q: %v", recorder.Body.String(), err)
	}
	if problem.Type != SlugNotFound {
		t.Fatalf("problem type = %q, want %q", problem.Type, SlugNotFound)
	}
}

// TestNewServerWiresConfiguredAria2 pins the composition root: a
// configured DLTOOL_ARIA2_URL registers the adapter, creates its engines
// row and records the boot probe outcome — the acceptance criteria of the
// task, observed through NewServer.
func TestNewServerWiresConfiguredAria2(t *testing.T) {
	root := t.TempDir()
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

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	server, err := NewServer(
		&config.Config{
			ConfigDir:   configDir,
			SessionTTL:  time.Hour,
			DataRoots:   []string{filepath.Join(root, "data")},
			Aria2URL:    deadURL,
			Aria2Secret: secure.Secret(rpcSecret),
		},
		db,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("NewServer with an unreachable aria2: %v", err)
	}
	// Registered after the store's cleanup, so the background loops stop
	// before the database they poll closes.
	t.Cleanup(server.Shutdown)

	configured, ok := server.Engines.Get(engine.NameAria2)
	if !ok {
		t.Fatal("registry holds no aria2 engine after NewServer with DLTOOL_ARIA2_URL set")
	}
	t.Cleanup(func() {
		if err := configured.Close(); err != nil {
			t.Errorf("close configured engine: %v", err)
		}
	})

	user := seedUser(t, db)
	api := humatest.Wrap(t, server.API)
	bearer := "Authorization: Bearer " + seedLiveAPIToken(t, db, user.ID)

	recorder := api.Get("/engines", bearer)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list engines status = %d, want 200; body %s", recorder.Code, recorder.Body.String())
	}
	engines := decodeEngines(t, recorder)
	aria2Row := engineByID(engines, store.EngineIDAria2)
	if aria2Row == nil {
		t.Fatalf("engines list holds no boot-created %s row: %+v", store.EngineIDAria2, engines)
	}

	if aria2Row.URL == nil || *aria2Row.URL != deadURL {
		t.Fatalf("aria2 url = %v, want the configured %s", aria2Row.URL, deadURL)
	}
	if aria2Row.Connected {
		t.Fatalf("aria2 connected = true at boot against a dead daemon: %+v", aria2Row)
	}
	if aria2Row.LastError == nil || *aria2Row.LastError == "" {
		t.Fatal("aria2 last_error = nil after the failed boot probe")
	}
	if len(aria2Row.Capabilities) == 0 {
		t.Fatal("aria2 capabilities empty; the boot row must merge the adapter's declared set")
	}
	if strings.Contains(recorder.Body.String(), rpcSecret) {
		t.Fatalf("engines body leaked the configured DLTOOL_ARIA2_SECRET: %s", recorder.Body.String())
	}
}

// extractPasswordSentinel is a configured extract_passwords value no
// response body may carry — GET /settings renders only "__redacted__".
const extractPasswordSentinel = "extract-password-sentinel-value"

// settingsKeysOnWire is the whole fifteen-key set of
// docs/11-config-reference.md section 5 — the exact member list GET
// /settings must emit.
var settingsKeysOnWire = []string{
	"download_rate_limit", "upload_rate_limit",
	"alt_download_rate_limit", "alt_upload_rate_limit",
	"schedule_enabled", "default_destination", "min_free_space",
	"max_active_total", "max_active_per_engine",
	"process_order", "rss_enabled", "rss_interval_s",
	"auto_extract", "extract_passwords", "confirm_on_delete",
}

func TestSettingsRedactsExtractPasswords(t *testing.T) {
	env := newSettingsTestEnv(t)
	settings := store.NewSettingsStore(env.db)

	// Before any row exists the member is still the literal placeholder —
	// never an array, never the empty string.
	recorder := env.getSettings(t)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	body := decodeSettingsBody(t, recorder)
	require.Equal(t, RedactedPlaceholder, body["extract_passwords"])

	require.NoError(t, settings.AppendExtractPassword(t.Context(), extractPasswordSentinel))
	seedEngineRow(t, env.db, store.EngineIDAria2, engine.NameAria2, engine.NameAria2, aria2RPCURL, aria2SecretSentinel)

	recorder = env.getSettings(t)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	body = decodeSettingsBody(t, recorder)

	// Exactly the fifteen documented keys, no more.
	require.Len(t, body, len(settingsKeysOnWire))
	for _, key := range settingsKeysOnWire {
		require.Contains(t, body, key)
	}
	require.Equal(t, RedactedPlaceholder, body["extract_passwords"])
	require.Equal(t, map[string]any{}, body["min_free_space"],
		"the stored map renders verbatim — {} after the initial migration")
	require.Equal(t, env.dataRoot, body["default_destination"],
		"an unset default_destination renders the first data root")

	raw := recorder.Body.String()
	for _, secret := range []string{extractPasswordSentinel, aria2SecretSentinel} {
		require.NotContains(t, raw, secret, "GET /settings leaked a configured secret")
	}
}

// TestPatchRedactedIsNoOp pins the doc 11 section 6 write-back rule: a
// PATCH carrying "extract_passwords":"__redacted__" — the body shape a
// GET/PATCH round trip produces — leaves the stored secret byte-identical.
func TestPatchRedactedIsNoOp(t *testing.T) {
	env := newSettingsTestEnv(t)
	settings := store.NewSettingsStore(env.db)

	require.NoError(t, settings.AppendExtractPassword(t.Context(), extractPasswordSentinel))
	before, err := settings.ExtractPasswords(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{extractPasswordSentinel}, before)

	response := env.patchSettings(t, map[string]any{"extract_passwords": RedactedPlaceholder})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, RedactedPlaceholder, decodeSettingsBody(t, response)["extract_passwords"])
	require.NotContains(t, response.Body.String(), extractPasswordSentinel)

	after, err := settings.ExtractPasswords(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after, "the redacted placeholder must not overwrite the stored list")
}

// TestPatchUnknownKeyIs422 pins the closed key set: a key outside doc 11
// section 5 — including a hook-named key, which must not be distinguished
// from any other unknown key (FR-105) — is 422 /problems/validation-failed
// and writes nothing.
func TestPatchUnknownKeyIs422(t *testing.T) {
	env := newSettingsTestEnv(t)

	for _, key := range []string{"no_such_key", "completion_hook"} {
		response := env.patchSettings(t, map[string]any{key: 1})
		assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

		var stored int
		require.NoError(t, env.db.GetContext(t.Context(), &stored,
			`SELECT COUNT(*) FROM settings WHERE key = ?`, key))
		require.Zero(t, stored, "the rejected key %q must not be stored", key)
	}
}

// TestSettingsRejectsHookKey pins the FR-105 boundary: a hook-named key
// gets the same 422 /problems/validation-failed as any other unknown key
// — a distinct rejection would reveal the key is special — and GET
// /settings never returns the hook path, even with the hook installed.
func TestSettingsRejectsHookKey(t *testing.T) {
	env := newSettingsTestEnv(t)

	hookPath := filepath.Join(filepath.Dir(env.dbPath), "hooks", "on-complete")
	require.NoError(t, os.MkdirAll(filepath.Dir(hookPath), 0o755))
	require.NoError(t, os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	for _, key := range []string{"completion_hook", "on_complete_hook", "hook_command"} {
		response := env.patchSettings(t, map[string]any{key: hookPath})
		assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

		var stored int
		require.NoError(t, env.db.GetContext(t.Context(), &stored,
			`SELECT COUNT(*) FROM settings WHERE key = ?`, key))
		require.Zero(t, stored, "the rejected key %q must not be stored", key)
	}

	recorder := env.getSettings(t)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.NotContains(t, recorder.Body.String(), hookPath)
	require.NotContains(t, recorder.Body.String(), "on-complete")
}

// TestPatchOutOfRangeIs422 pins the per-key domain checks of the task:
// every malformed or out-of-domain member is 422 /problems/validation-failed
// and writes nothing.
func TestPatchOutOfRangeIs422(t *testing.T) {
	env := newSettingsTestEnv(t)

	cases := map[string]map[string]any{
		"rss_interval_s below the 300s floor":    {"rss_interval_s": 120},
		"rss_interval_s as a string":             {"rss_interval_s": "300"},
		"negative rate limit":                    {"download_rate_limit": -1},
		"negative max_active_total":              {"max_active_total": -1},
		"negative max_active_per_engine":         {"max_active_per_engine": -1},
		"negative min_free_space value":          {"min_free_space": map[string]any{"/data": -1}},
		"relative min_free_space key":            {"min_free_space": map[string]any{"data": 1}},
		"non-canonical min_free_space key":       {"min_free_space": map[string]any{"/data/": 1}},
		"non-canonical min_free_space traversal": {"min_free_space": map[string]any{"/data/../data": 1}},
		"min_free_space placeholder":             {"min_free_space": RedactedPlaceholder},
		"default_destination placeholder":        {"default_destination": RedactedPlaceholder},
		"default_destination empty":              {"default_destination": ""},
		"default_destination relative":           {"default_destination": "downloads"},
		"default_destination traversal":          {"default_destination": "/data/../etc"},
		"default_destination outside roots":      {"default_destination": "/not-a-data-root"},
		"max_active_total overflows int":         {"max_active_total": int64(1) << 33},
		"max_active_per_engine overflows int":    {"max_active_per_engine": int64(1) << 33},
		"min_free_space null":                    {"min_free_space": nil},
		"download_rate_limit null":               {"download_rate_limit": nil},
		"process_order other enum":               {"process_order": "newest_first"},
		"schedule_enabled as a string":           {"schedule_enabled": "true"},
		"extract_passwords null":                 {"extract_passwords": nil},
		"extract_passwords non-array":            {"extract_passwords": 4},
	}

	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			response := env.patchSettings(t, patch)
			assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
		})
	}
}

// TestPatchMinFreeSpaceReplacesWholesale pins the sparse-map semantics of
// doc 05 section 11.1: a member present in a patch replaces the whole
// stored map — omitted roots are gone afterward — while a patch without
// the member leaves the stored JSON byte-identical.
func TestPatchMinFreeSpaceReplacesWholesale(t *testing.T) {
	env := newSettingsTestEnv(t)

	response := env.patchSettings(t, map[string]any{
		"min_free_space": map[string]any{"/data": 1024, "/mnt": 2048},
	})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t,
		map[string]any{"/data": float64(1024), "/mnt": float64(2048)},
		decodeSettingsBody(t, response)["min_free_space"])

	storedJSON := func() string {
		var raw string
		require.NoError(t, env.db.GetContext(t.Context(), &raw,
			`SELECT value_json FROM settings WHERE key = 'min_free_space'`))
		return raw
	}

	// A patch naming the key again replaces the whole map: /data is gone.
	response = env.patchSettings(t, map[string]any{
		"min_free_space": map[string]any{"/mnt": 4096},
	})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t,
		map[string]any{"/mnt": float64(4096)},
		decodeSettingsBody(t, response)["min_free_space"])

	// A patch omitting the key leaves the stored value byte-identical.
	before := storedJSON()
	response = env.patchSettings(t, map[string]any{"rss_enabled": false})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, before, storedJSON(),
		"a patch without min_free_space must leave the stored map untouched")
	require.Equal(t,
		map[string]any{"/mnt": float64(4096)},
		decodeSettingsBody(t, env.getSettings(t))["min_free_space"])
}

// TestSystemInfoCarriesNoSecret asserts the doc 05 section 13 shape —
// all twelve top-level members — and that no configured engine secret
// appears anywhere in the serialised body.
func TestSystemInfoCarriesNoSecret(t *testing.T) {
	env := newSettingsTestEnv(t)
	seedEngineRow(t, env.db, store.EngineIDAria2, engine.NameAria2, engine.NameAria2, aria2RPCURL, aria2SecretSentinel)
	seedEngineRow(t, env.db, store.EngineIDQBittorrent, engine.NameQBittorrent, "qBittorrent", qbtBaseURL, qbtSecretSentinel)

	recorder := env.getSystemInfo(t)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	raw := recorder.Body.String()
	for _, secret := range []string{aria2SecretSentinel, qbtSecretSentinel} {
		require.NotContains(t, raw, secret, "GET /system/info leaked an engine secret")
	}

	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	for _, member := range []string{
		"version", "commit", "built_at", "go_version", "started_at", "uptime_s",
		"database", "engines", "tasks", "schedule", "limits", "jobs",
	} {
		require.Contains(t, body, member)
	}
	require.Len(t, body, 12)

	database, ok := body["database"].(map[string]any)
	require.True(t, ok, "database = %v", body["database"])
	require.Equal(t, env.dbPath, database["path"])
	require.Greater(t, database["size_bytes"], float64(0))
	liveVersion, err := store.SchemaVersion(t.Context(), env.db)
	require.NoError(t, err)
	require.Equal(t, float64(liveVersion), database["schema_version"])

	engines, ok := body["engines"].([]any)
	require.True(t, ok, "engines = %v", body["engines"])
	require.Len(t, engines, 2)
	for _, entry := range engines {
		brief, ok := entry.(map[string]any)
		require.True(t, ok)
		require.Contains(t, brief, "kind")
		require.Contains(t, brief, "connected")
		require.Contains(t, brief, "version")
		require.False(t, brief["connected"].(bool), "no engine is registered in this process")
	}

	tasks, ok := body["tasks"].(map[string]any)
	require.True(t, ok, "tasks = %v", body["tasks"])
	require.Contains(t, tasks, "total")
	require.Contains(t, tasks, "by_state")

	schedule, ok := body["schedule"].(map[string]any)
	require.True(t, ok, "schedule = %v", body["schedule"])
	require.Equal(t, false, schedule["enabled"])
	// The seeded grid is uniformly "default", so that is the cell in force.
	require.Equal(t, "default", schedule["active_mode"])
	require.IsType(t, "", schedule["timezone"])
	require.NotEmpty(t, schedule["timezone"])

	limits, ok := body["limits"].(map[string]any)
	require.True(t, ok, "limits = %v", body["limits"])
	require.Equal(t, float64(defaultMaxActiveTotal), limits["max_active_total"])
	require.Equal(t, float64(defaultMaxActivePerEngine), limits["max_active_per_engine"])

	jobs, ok := body["jobs"].(map[string]any)
	require.True(t, ok, "jobs = %v", body["jobs"])
	require.Equal(t, float64(0), jobs["pending"])
	require.Equal(t, float64(0), jobs["running"])
	require.Equal(t, float64(0), jobs["failed"])
}
