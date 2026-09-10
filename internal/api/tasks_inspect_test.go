package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/store"
	"github.com/L-K-M/dl-tool/internal/uri"
)

// btihV1Hex is the magnet fixture's 40-character infohash.
const btihV1Hex = "0b1ec1a478f2b3c793bf88a45e9c0d6d81f2a3b4"

// The fixture torrent of the uri package's tests, replayed here so the
// endpoint cases assert against the same known manifest.
const inspectTorrentFixture = "d8:announce35:http://tracker.example.com/announce" +
	"4:infod6:lengthi11e4:name9:hello.txt12:piece lengthi16384e6:pieces20:" +
	"\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00e"

const (
	inspectMagnetFixture = "magnet:?xt=urn:btih:" + btihV1Hex
	inspectHTTPFixture   = "https://releases.example.com/26.04/ubuntu-26.04-desktop-amd64.iso"
)

// magnetInspectorEngine is a recordingEngine plus the InspectMagnet extension
// the inspect endpoint type-asserts for.
type magnetInspectorEngine struct {
	*recordingEngine

	mu      sync.Mutex
	inspect func(context.Context, string) (uri.Manifest, error)
}

func (e *magnetInspectorEngine) InspectMagnet(ctx context.Context, magnet string) (uri.Manifest, error) {
	e.mu.Lock()
	inspect := e.inspect
	e.mu.Unlock()

	return inspect(ctx, magnet)
}

// inspectTestEnv is a humatest server without engine stand-ins: the inspect
// endpoint contacts no engine except through magnetInspector, which tests
// register explicitly.
type inspectTestEnv struct {
	api    humatest.TestAPI
	db     *sqlx.DB
	server *Server
	bearer string
}

// newInspectTestEnv builds the env; engines are registered per test through
// env.server.Engines.
func newInspectTestEnv(t *testing.T) *inspectTestEnv {
	t.Helper()

	root := t.TempDir()
	db, err := store.Open(
		t.Context(),
		root+"/config/dl-tool.db",
		root+"/backups",
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
			ConfigDir:  root + "/config",
			SessionTTL: time.Hour,
			DataRoots:  []string{root + "/data"},
		},
		db,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	user := seedUser(t, db)
	bearer := seedLiveAPIToken(t, db, user.ID)

	return &inspectTestEnv{
		api:    humatest.Wrap(t, server.API),
		db:     db,
		server: server,
		bearer: bearer,
	}
}

// inspect posts one submission with the test bearer credential.
func (e *inspectTestEnv) inspect(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Post("/tasks/inspect", body, "Authorization: Bearer "+e.bearer)
}

// assertNoTask proves inspecting never inserts a tasks row (FR-006).
func (e *inspectTestEnv) assertNoTask(t *testing.T) {
	t.Helper()

	var count int
	if err := e.db.GetContext(t.Context(), &count, `SELECT COUNT(*) FROM tasks`); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if count != 0 {
		t.Fatalf("tasks rows = %d, want 0: inspect must not create tasks", count)
	}
}

// registerMagnetInspector adds an engine whose InspectMagnet behaves as
// inspect decides.
func (e *inspectTestEnv) registerMagnetInspector(
	t *testing.T,
	inspect func(context.Context, string) (uri.Manifest, error),
) *magnetInspectorEngine {
	t.Helper()

	eng := &magnetInspectorEngine{
		recordingEngine: newRecordingEngine(engine.NameQBittorrent, acceptsBitTorrent),
		inspect:         inspect,
	}
	e.server.Engines.Register(eng)

	return eng
}

// resolvedMagnetManifest is what a metadata-resolving inspector returns.
func resolvedMagnetManifest() uri.Manifest {
	size := int64(11)

	return uri.Manifest{
		Name:       "Ubuntu 26.04 LTS Desktop",
		TotalSize:  size,
		InfohashV1: btihV1Hex,
		Files: []uri.ManifestFile{
			{Index: 0, Path: "ubuntu-26.04-desktop-amd64.iso", Size: size},
		},
	}
}

func TestInspectTorrentBlob(t *testing.T) {
	env := newInspectTestEnv(t)

	resp := env.inspect(t, map[string]any{
		"uris":     []string{},
		"blob":     base64.StdEncoding.EncodeToString([]byte(inspectTorrentFixture)),
		"filename": "hello.torrent",
	})
	if resp.Code != 200 {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}

	var body struct {
		Manifests []ManifestDTO `json:"manifests"`
		Rejected  []RejectedURI `json:"rejected"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Manifests) != 1 {
		t.Fatalf("manifests = %d, want 1", len(body.Manifests))
	}

	manifest := body.Manifests[0]
	if manifest.Kind != string(uri.KindTorrent) {
		t.Errorf("kind = %q, want torrent", manifest.Kind)
	}
	if manifest.SourceURI != "hello.torrent" {
		t.Errorf("source_uri = %q, want the submitted filename", manifest.SourceURI)
	}
	if manifest.Name != "hello.txt" {
		t.Errorf("name = %q, want hello.txt", manifest.Name)
	}
	if manifest.InfohashV1 == nil || *manifest.InfohashV1 != "a094d623acb1eaa2fb3fdd896260e3def6ab6dbf" {
		t.Errorf("infohash_v1 = %v, want the fixture hash", manifest.InfohashV1)
	}
	if manifest.InfohashV2 != nil {
		t.Errorf("infohash_v2 = %v, want null for a v1 torrent", manifest.InfohashV2)
	}
	if manifest.TotalSize == nil || *manifest.TotalSize != 11 {
		t.Errorf("total_size = %v, want 11", manifest.TotalSize)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Path != "hello.txt" {
		t.Errorf("files = %+v, want one hello.txt entry", manifest.Files)
	}
	if manifest.MetadataPending {
		t.Error("metadata_pending = true, want false")
	}
	env.assertNoTask(t)
}

func TestInspectMagnetResolved(t *testing.T) {
	env := newInspectTestEnv(t)

	inspector := env.registerMagnetInspector(t, func(context.Context, string) (uri.Manifest, error) {
		return resolvedMagnetManifest(), nil
	})

	resp := env.inspect(t, map[string]any{"uris": []string{inspectMagnetFixture}})
	if resp.Code != 200 {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}

	var body struct {
		Manifests []ManifestDTO `json:"manifests"`
		Rejected  []RejectedURI `json:"rejected"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Manifests) != 1 {
		t.Fatalf("manifests = %d, want 1", len(body.Manifests))
	}

	manifest := body.Manifests[0]
	if manifest.MetadataPending {
		t.Error("metadata_pending = true, want false for resolved metadata")
	}
	if manifest.Name != "Ubuntu 26.04 LTS Desktop" && manifest.Name != resolvedMagnetManifest().Name {
		t.Errorf("name = %q, want the resolved name", manifest.Name)
	}
	if manifest.SourceURI != inspectMagnetFixture {
		t.Errorf("source_uri = %q, want the magnet as sent", manifest.SourceURI)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Path != "ubuntu-26.04-desktop-amd64.iso" {
		t.Errorf("files = %+v, want the resolved file", manifest.Files)
	}

	// The only permitted engine contact is the metadata fetch: no task
	// operation may run.
	for _, call := range inspector.recorded() {
		if call != "Connect" && call != "Health" && call != "Events" && call != "List" {
			t.Errorf("engine call %q made during inspect; only metadata fetch is permitted", call)
		}
	}
	env.assertNoTask(t)
}

func TestInspectMagnetPending(t *testing.T) {
	env := newInspectTestEnv(t)
	env.registerMagnetInspector(t, func(ctx context.Context, _ string) (uri.Manifest, error) {
		// Outlast the handler's deadline, forcing the pending fallback.
		<-ctx.Done()

		return uri.Manifest{}, ctx.Err()
	})
	// Shorten the deadline so the test does not wait a real minute.
	previous := magnetInspectDeadline
	magnetInspectDeadline = 50 * time.Millisecond
	t.Cleanup(func() { magnetInspectDeadline = previous })

	resp := env.inspect(t, map[string]any{"uris": []string{inspectMagnetFixture}})
	if resp.Code != 200 {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}

	var body struct {
		Manifests []ManifestDTO `json:"manifests"`
		Rejected  []RejectedURI `json:"rejected"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Manifests) != 1 {
		t.Fatalf("manifests = %d, want 1", len(body.Manifests))
	}

	manifest := body.Manifests[0]
	if !manifest.MetadataPending {
		t.Error("metadata_pending = false, want true after the deadline")
	}
	if manifest.Files != nil {
		t.Errorf("files = %+v, want null while metadata is pending", manifest.Files)
	}
	env.assertNoTask(t)
}

func TestInspectMagnetNoInspector(t *testing.T) {
	env := newInspectTestEnv(t)

	resp := env.inspect(t, map[string]any{"uris": []string{inspectMagnetFixture}})
	if resp.Code != 200 {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}

	var body struct {
		Manifests []ManifestDTO `json:"manifests"`
		Rejected  []RejectedURI `json:"rejected"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Manifests) != 1 || !body.Manifests[0].MetadataPending {
		t.Fatalf("manifests = %+v, want one metadata_pending manifest", body.Manifests)
	}
	if body.Manifests[0].Files != nil {
		t.Errorf("files = %+v, want null while metadata is pending", body.Manifests[0].Files)
	}
	env.assertNoTask(t)
}

func TestInspectMagnetEngineDown(t *testing.T) {
	env := newInspectTestEnv(t)
	env.registerMagnetInspector(t, func(context.Context, string) (uri.Manifest, error) {
		return uri.Manifest{}, engine.ErrUnavailable
	})

	resp := env.inspect(t, map[string]any{"uris": []string{inspectMagnetFixture}})
	if resp.Code != 503 {
		t.Fatalf("status = %d, want 503, body %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), SlugEngineUnavailable) {
		t.Errorf("body = %s, want problem type %s", resp.Body.String(), SlugEngineUnavailable)
	}
	env.assertNoTask(t)
}

func TestInspectHTTPURL(t *testing.T) {
	env := newInspectTestEnv(t)

	resp := env.inspect(t, map[string]any{"uris": []string{inspectHTTPFixture}})
	if resp.Code != 200 {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}

	var body struct {
		Manifests []ManifestDTO `json:"manifests"`
		Rejected  []RejectedURI `json:"rejected"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Manifests) != 1 {
		t.Fatalf("manifests = %d, want 1", len(body.Manifests))
	}

	manifest := body.Manifests[0]
	if manifest.Kind != string(uri.KindHTTP) {
		t.Errorf("kind = %q, want http", manifest.Kind)
	}
	if manifest.Name != "ubuntu-26.04-desktop-amd64.iso" {
		t.Errorf("name = %q, want the last path segment", manifest.Name)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Size != nil {
		t.Errorf("files = %+v, want one entry with null size", manifest.Files)
	}
	if manifest.InfohashV1 != nil || manifest.InfohashV2 != nil {
		t.Errorf("infohashes = %v / %v, want both null", manifest.InfohashV1, manifest.InfohashV2)
	}
	env.assertNoTask(t)
}

func TestInspectUnsupportedScheme(t *testing.T) {
	env := newInspectTestEnv(t)

	resp := env.inspect(t, map[string]any{"uris": []string{ed2kExample}})
	if resp.Code != 422 {
		t.Fatalf("status = %d, want 422, body %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), SlugUnsupportedScheme) {
		t.Errorf("body = %s, want problem type %s", resp.Body.String(), SlugUnsupportedScheme)
	}
	if !strings.Contains(resp.Body.String(), "ed2k is not supported in v1") {
		t.Errorf("body = %s, want the doc 06 section 2 row 7 message", resp.Body.String())
	}
	env.assertNoTask(t)
}

func TestInspectOversizedBlob(t *testing.T) {
	env := newInspectTestEnv(t)

	blob := base64.StdEncoding.EncodeToString(make([]byte, maxInspectBlobBytes+1))
	resp := env.inspect(t, map[string]any{"uris": []string{}, "blob": blob})
	if resp.Code != 413 {
		t.Fatalf("status = %d, want 413, body %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), SlugPayloadTooLarge) {
		t.Errorf("body = %s, want problem type %s", resp.Body.String(), SlugPayloadTooLarge)
	}
	env.assertNoTask(t)
}

func TestInspectEmptySubmission(t *testing.T) {
	env := newInspectTestEnv(t)

	resp := env.inspect(t, map[string]any{})
	if resp.Code != 422 {
		t.Fatalf("status = %d, want 422, body %s", resp.Code, resp.Body.String())
	}
	env.assertNoTask(t)
}

// deadInspectorError is never returned on the wire; a generic inspector
// failure is the pending fallback, not an error.
var errInspectorUnresolvable = errors.New("metadata not resolvable right now")

func TestInspectMagnetOtherErrorIsPending(t *testing.T) {
	env := newInspectTestEnv(t)
	env.registerMagnetInspector(t, func(context.Context, string) (uri.Manifest, error) {
		return uri.Manifest{}, errInspectorUnresolvable
	})

	resp := env.inspect(t, map[string]any{"uris": []string{inspectMagnetFixture}})
	if resp.Code != 200 {
		t.Fatalf("status = %d, want 200, body %s", resp.Code, resp.Body.String())
	}

	var body struct {
		Manifests []ManifestDTO `json:"manifests"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Manifests) != 1 || !body.Manifests[0].MetadataPending {
		t.Fatalf("manifests = %+v, want one metadata_pending manifest", body.Manifests)
	}
	env.assertNoTask(t)
}

// TestInspectCreatesNoTask runs every branch against one store and asserts
// the tasks count never moves (doc 05 section 5.3: inspecting never creates
// a task).
func TestInspectCreatesNoTask(t *testing.T) {
	env := newInspectTestEnv(t)

	submissions := []map[string]any{
		{
			"uris": []string{},
			"blob": base64.StdEncoding.EncodeToString([]byte(inspectTorrentFixture)),
		},
		{"uris": []string{inspectHTTPFixture}},
		{"uris": []string{inspectMagnetFixture}},
		{"uris": []string{ed2kExample}},
	}
	for i, body := range submissions {
		resp := env.inspect(t, body)
		if resp.Code >= 500 {
			t.Fatalf("submission %d: status = %d, body %s", i, resp.Code, resp.Body.String())
		}
		env.assertNoTask(t)
	}
}
