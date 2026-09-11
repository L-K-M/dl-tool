package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/engine/aria2"
	"github.com/L-K-M/dl-tool/internal/engine/qbittorrent"
	"github.com/L-K-M/dl-tool/internal/fsx"
	"github.com/L-K-M/dl-tool/internal/store"
)

// The engine-side fixture handles; any string the stand-ins record verbatim.
const (
	aria2GID  = "2089b05ecca3d829"
	qbtHash   = "8f9c3a2b1d4e5f60718293a4b5c6d7e8f0a0b1c2"
	unknownID = "tsk_01JKQ8Z9YV6M3P0R2S4T6V8W0X"

	testDLLimit = int64(2097152) // 2 MiB/s
)

// actionsTestEnv is one humatest server against a real migrated store,
// with action-aware engine stand-ins whose calls and failures a test can
// pin. It is deliberately separate from tasksTestEnv: those stand-ins
// answer ErrNotSupported for everything, while these record and decide.
type actionsTestEnv struct {
	api         humatest.TestAPI
	db          *sqlx.DB
	aria2       *actionEngine
	qbittorrent *actionEngine
	bearer      string
	dataRoot    string // the configured DLTOOL_DATA_ROOTS entry
}

// newActionsTestEnv builds the env with both stand-ins registered, each
// declaring the capability set of the real adapter it plays, so the
// capability gates answer the same verdicts production would.
func newActionsTestEnv(t *testing.T) *actionsTestEnv {
	t.Helper()

	aria2 := newActionEngine(engine.NameAria2, acceptsAria2Lanes)
	aria2.caps = []engine.Capability{
		engine.CapFTP, engine.CapHTTP, engine.CapMetalink,
		engine.CapPerFileSelect, engine.CapPushEvents, engine.CapSetLocation, engine.CapSFTP,
	}
	qbittorrent := newActionEngine(engine.NameQBittorrent, acceptsBitTorrent)
	qbittorrent.caps = []engine.Capability{
		engine.CapBitTorrent, engine.CapBTV2, engine.CapCategories, engine.CapMagnet,
		engine.CapPerFilePriority, engine.CapPerFileSelect, engine.CapRename,
		engine.CapSequential, engine.CapSetLocation, engine.CapShareLimits, engine.CapTags,
	}
	env := newActionsTestEnvWithEngines(t, aria2, qbittorrent)
	env.aria2, env.qbittorrent = aria2, qbittorrent

	return env
}

// newActionsTestEnvWithEngines builds the env around an explicit engine
// set, so a test can register a stand-in with capabilities the defaults
// lack — the recheckable one below.
func newActionsTestEnvWithEngines(t *testing.T, engines ...engine.Engine) *actionsTestEnv {
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

	for _, e := range engines {
		server.Engines.Register(e)
	}

	user := seedUser(t, db)

	return &actionsTestEnv{
		api:      humatest.Wrap(t, server.API),
		db:       db,
		bearer:   seedLiveAPIToken(t, db, user.ID),
		dataRoot: dataRoot,
	}
}

// actionEngine is an engine stand-in whose mutating methods record every
// call and whose failures are configurable, so a test can pin both the
// engine call an action makes and the per-id mapping of its errors. It
// deliberately has no Recheck method: the recheck capability is optional
// in the handler, and this stand-in plays every engine without it.
type actionEngine struct {
	name    string
	accepts func(string) bool
	caps    []engine.Capability

	mu    sync.Mutex
	calls []string

	pauseErr   error
	removeErr  error
	limitsErr  error
	recheckErr error
	resumeErr  error
	getErr     error
	// one configurable failure per T036 mutator
	categoryErr   error
	tagsErr       error
	sequentialErr error
	shareErr      error
	locationErr   error
	// resumeGate, when non-nil, holds every Resume call until the test
	// closes it: the sync point that proves the admission pass holds the
	// task-operation lease while a pause action waits behind it. Set
	// before any goroutine starts; never written concurrently.
	resumeGate chan struct{}
}

func newActionEngine(name string, accepts func(string) bool) *actionEngine {
	return &actionEngine{name: name, accepts: accepts}
}

func (e *actionEngine) Name() string                      { return e.name }
func (e *actionEngine) Capabilities() []engine.Capability { return slices.Clone(e.caps) }
func (e *actionEngine) Accepts(uri string) bool           { return e.accepts(uri) }

func (e *actionEngine) record(call string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.calls = append(e.calls, call)
}

// recorded returns every mutating engine call made so far.
func (e *actionEngine) recorded() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]string(nil), e.calls...)
}

// assertNoCalls fails the test when the engine was contacted at all.
func (e *actionEngine) assertNoCalls(t *testing.T) {
	t.Helper()

	if calls := e.recorded(); len(calls) != 0 {
		t.Errorf("%s engine was called: %v", e.name, calls)
	}
}

func (e *actionEngine) Connect(context.Context) error          { return nil }
func (e *actionEngine) Close() error                           { return nil }
func (e *actionEngine) Health(context.Context) (string, error) { return "stub", nil }

func (e *actionEngine) Add(context.Context, engine.AddRequest) (string, error) {
	return "", engine.ErrNotSupported
}
func (e *actionEngine) List(context.Context) ([]engine.TaskInfo, error) { return nil, nil }
func (e *actionEngine) Get(_ context.Context, _ string) (engine.TaskInfo, error) {
	// The parked-release confirmation reads ErrNotFound as "the engine
	// lost the handle" by default; a test overrides it to fail that probe
	// the way a down engine fails every call.
	if e.getErr == nil {
		return engine.TaskInfo{}, engine.ErrNotFound
	}

	return engine.TaskInfo{}, e.getErr
}
func (e *actionEngine) Files(context.Context, string) ([]engine.FileEntry, error) {
	return nil, engine.ErrNotSupported
}

func (e *actionEngine) Pause(_ context.Context, id string) error {
	e.record("Pause " + id)

	return e.pauseErr
}
func (e *actionEngine) Resume(_ context.Context, id string) error {
	e.record("Resume " + id)
	if e.resumeGate != nil {
		<-e.resumeGate
	}

	return e.resumeErr
}
func (e *actionEngine) Remove(_ context.Context, id string) error {
	e.record("Remove " + id)

	return e.removeErr
}

func (e *actionEngine) SetFiles(context.Context, string, []int, map[int]int) error {
	return engine.ErrNotSupported
}

func (e *actionEngine) SetLocation(_ context.Context, id, path string) error {
	e.record("SetLocation " + id + " " + path)

	return e.locationErr
}
func (e *actionEngine) Rename(context.Context, string, string) error { return engine.ErrNotSupported }

func (e *actionEngine) SetCategory(_ context.Context, id, category string) error {
	e.record("SetCategory " + id + " " + category)

	return e.categoryErr
}

// SetTags is the whole-set mutator of the tagMutator interface the patch
// handler narrows to.
func (e *actionEngine) SetTags(_ context.Context, id string, tags []string) error {
	e.record("SetTags " + id + " " + strings.Join(tags, ","))

	return e.tagsErr
}

// SetSequential is the sequentialEngine narrowing's setter.
func (e *actionEngine) SetSequential(_ context.Context, id string, sequential bool) error {
	e.record("SetSequential " + id + " " + strconv.FormatBool(sequential))

	return e.sequentialErr
}

func (e *actionEngine) SetRateLimits(_ context.Context, id string, down, up *int64) error {
	e.record("SetRateLimits " + id + " " + limitArgs(down, up))

	return e.limitsErr
}

func (e *actionEngine) SetShareLimits(_ context.Context, id string, ratio *float64, seed *int64) error {
	e.record("SetShareLimits " + id + " " + shareLimitArgs(ratio, seed))

	return e.shareErr
}

func (e *actionEngine) Events(context.Context) (<-chan engine.TaskEvent, error) {
	return make(chan engine.TaskEvent), nil
}

// limitArgs renders the two optional rate-limit directions for the call
// log: a nil direction prints nil so a test can pin "one direction only".
func limitArgs(down, up *int64) string {
	parts := make([]string, 0, 2)
	for _, v := range []*int64{down, up} {
		if v == nil {
			parts = append(parts, "nil")

			continue
		}
		parts = append(parts, strconv.FormatInt(*v, 10))
	}

	return strings.Join(parts, " ")
}

// shareLimitArgs renders the two optional share limits for the call log,
// the nil sentinel included so a test can pin the untouched half of a
// one-sided patch.
func shareLimitArgs(ratio *float64, seed *int64) string {
	ratioText := "nil"
	if ratio != nil {
		ratioText = strconv.FormatFloat(*ratio, 'f', -1, 64)
	}
	seedText := "nil"
	if seed != nil {
		seedText = strconv.FormatInt(*seed, 10)
	}

	return ratioText + " " + seedText
}

// recheckingEngine adds the optional Recheck capability to the stand-in,
// playing the engines that can re-verify data in place.
type recheckingEngine struct {
	actionEngine
}

func (e *recheckingEngine) Recheck(_ context.Context, id string) error {
	e.record("Recheck " + id)

	return e.recheckErr
}

// seedActionTask writes one task straight through the store, because
// POST /tasks can set neither an engine handle nor a queue position.
func (e *actionsTestEnv) seedActionTask(t *testing.T, mutate func(*store.Task)) string {
	t.Helper()

	ref := aria2GID
	task := store.Task{
		Engine:      engine.NameAria2,
		EngineRef:   &ref,
		SourceKind:  "http",
		Name:        "actions-fixture",
		State:       "downloading",
		Destination: "/data",
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

// seedBitTorrentTask is seedActionTask on the qBittorrent stand-in, for
// the patch fields only a BitTorrent engine takes (category, tags,
// sequential, share limits).
func (e *actionsTestEnv) seedBitTorrentTask(t *testing.T, mutate func(*store.Task)) string {
	t.Helper()

	return e.seedActionTask(t, func(task *store.Task) {
		ref := qbtHash
		task.Engine = engine.NameQBittorrent
		task.EngineRef = &ref
		task.SourceKind = "torrent"
		if mutate != nil {
			mutate(task)
		}
	})
}

// postActions posts one action batch with the test bearer credential.
func (e *actionsTestEnv) postActions(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Post("/tasks/actions", body, "Authorization: Bearer "+e.bearer)
}

// patchTask patches one task with the test bearer credential.
func (e *actionsTestEnv) patchTask(t *testing.T, id string, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Patch("/tasks/"+id, body, "Authorization: Bearer "+e.bearer)
}

// taskState reads one task's state straight from the store.
func (e *actionsTestEnv) taskState(t *testing.T, id string) string {
	t.Helper()

	var state string
	if err := e.db.GetContext(t.Context(), &state, `SELECT state FROM tasks WHERE id = ?`, id); err != nil {
		t.Fatalf("read state of %s: %v", id, err)
	}

	return state
}

// taskEventCodes reads one task's event codes straight from the store.
func (e *actionsTestEnv) taskEventCodes(t *testing.T, id string) []string {
	t.Helper()

	var codes []string
	if err := e.db.SelectContext(t.Context(), &codes,
		`SELECT code FROM task_events WHERE task_id = ? ORDER BY at, id`, id); err != nil {
		t.Fatalf("read events of %s: %v", id, err)
	}

	return codes
}

// queueOrder reads the queue's ids in queue_position order.
func (e *actionsTestEnv) queueOrder(t *testing.T) []string {
	t.Helper()

	var ids []string
	if err := e.db.SelectContext(t.Context(), &ids,
		`SELECT id FROM tasks WHERE queue_position IS NOT NULL ORDER BY queue_position`); err != nil {
		t.Fatalf("read queue order: %v", err)
	}

	return ids
}

// taskTags reads one task's tag names in name order.
func (e *actionsTestEnv) taskTags(t *testing.T, id string) []string {
	t.Helper()

	var tags []string
	if err := e.db.SelectContext(t.Context(), &tags,
		`SELECT t.name FROM task_tags tt JOIN tags t ON t.id = tt.tag_id
WHERE tt.task_id = ? ORDER BY t.name`, id); err != nil {
		t.Fatalf("read tags of %s: %v", id, err)
	}

	return tags
}

// decodeActionsBody decodes the results envelope.
func decodeActionsBody(t *testing.T, recorder *httptest.ResponseRecorder) struct {
	Results []ActionResult `json:"results"`
} {
	t.Helper()

	var body struct {
		Results []ActionResult `json:"results"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body
}

// decodeTaskBody decodes one canonical Task object.
func decodeTaskBody(t *testing.T, recorder *httptest.ResponseRecorder) TaskDTO {
	t.Helper()

	var task TaskDTO
	if err := json.Unmarshal(recorder.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return task
}

// TestActionsPerIDOutcomes pins FR-014: a three-id pause batch with one
// unknown id returns 200 with two successes and one not-found — one bad id
// never fails the batch.
func TestActionsPerIDOutcomes(t *testing.T) {
	env := newActionsTestEnv(t)

	admitted := env.seedActionTask(t, nil) // downloading, held by aria2
	held := env.seedActionTask(t, func(task *store.Task) { task.EngineRef = nil })

	response := env.postActions(t, map[string]any{
		"ids":    []string{admitted, held, unknownID},
		"action": actionPause,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}

	body := decodeActionsBody(t, response)
	want := []ActionResult{
		{ID: admitted, Ok: true},
		{ID: held, Ok: true},
		{ID: unknownID, Ok: false, Type: SlugNotFound, Detail: detailTaskNotFound},
	}
	if !reflect.DeepEqual(body.Results, want) {
		t.Errorf("results = %+v, want %+v", body.Results, want)
	}

	// The admitted task was paused through the engine under its
	// namespaced id; the queue-held task was not, because no engine holds
	// it yet.
	if calls := env.aria2.recorded(); !slices.Equal(calls, []string{"Pause aria2:" + aria2GID}) {
		t.Errorf("aria2 calls = %v, want exactly one Pause of the namespaced id", calls)
	}
	env.qbittorrent.assertNoCalls(t)

	for _, id := range []string{admitted, held} {
		if state := env.taskState(t, id); state != string(engine.StatePaused) {
			t.Errorf("task %s state = %q, want paused", id, state)
		}
		if codes := env.taskEventCodes(t, id); !slices.Equal(codes, []string{eventTaskPaused}) {
			t.Errorf("task %s event codes = %v, want [%s]", id, codes, eventTaskPaused)
		}
	}
}

// TestActionsRejectsUnknownAction pins the whole-request failure: an
// action outside the nine is a 422 that touches no engine and no row.
func TestActionsRejectsUnknownAction(t *testing.T) {
	env := newActionsTestEnv(t)

	id := env.seedActionTask(t, nil)

	response := env.postActions(t, map[string]any{"ids": []string{id}, "action": "explode"})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	env.aria2.assertNoCalls(t)
	if state := env.taskState(t, id); state != string(engine.StateDownloading) {
		t.Errorf("state = %q, want unchanged downloading", state)
	}
}

// TestActionsBatchLimit pins the batch caps: an empty id list and a
// 501-id list are 422 validation failures that touch nothing.
func TestActionsBatchLimit(t *testing.T) {
	env := newActionsTestEnv(t)

	id := env.seedActionTask(t, nil)

	cases := []struct {
		name string
		ids  []string
	}{
		{"empty ids", []string{}},
		{"null ids", nil},
		{"five hundred one ids", slices.Repeat([]string{id}, maxActionIDs+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := env.postActions(t, map[string]any{"ids": tc.ids, "action": actionPause})
			assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

			env.aria2.assertNoCalls(t)
			if state := env.taskState(t, id); state != string(engine.StateDownloading) {
				t.Errorf("state = %q, want unchanged downloading", state)
			}
		})
	}
}

// TestActionsPerAction pins each lifecycle action's engine call and state
// transition against the table of docs/05-api-contract.md section 5.7.
func TestActionsPerAction(t *testing.T) {
	t.Run("resume queues a paused task without an engine call", func(t *testing.T) {
		env := newActionsTestEnv(t)

		id := env.seedActionTask(t, func(task *store.Task) { task.State = "paused" })

		response := env.postActions(t, map[string]any{"ids": []string{id}, "action": actionResume})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}
		if result := decodeActionsBody(t, response).Results[0]; !result.Ok {
			t.Errorf("result = %+v, want ok", result)
		}

		// Engine.Resume belongs to the admission pass (T098), not here.
		env.aria2.assertNoCalls(t)
		if state := env.taskState(t, id); state != string(engine.StateQueued) {
			t.Errorf("state = %q, want queued", state)
		}
		if codes := env.taskEventCodes(t, id); !slices.Equal(codes, []string{eventTaskResumed}) {
			t.Errorf("event codes = %v, want [%s]", codes, eventTaskResumed)
		}
	})

	t.Run("remove calls the engine and tombstones the task", func(t *testing.T) {
		env := newActionsTestEnv(t)

		id := env.seedActionTask(t, nil)

		response := env.postActions(t, map[string]any{"ids": []string{id}, "action": actionRemove})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}
		if result := decodeActionsBody(t, response).Results[0]; !result.Ok {
			t.Errorf("result = %+v, want ok", result)
		}

		if calls := env.aria2.recorded(); !slices.Equal(calls, []string{"Remove aria2:" + aria2GID}) {
			t.Errorf("aria2 calls = %v, want exactly one Remove", calls)
		}
		if state := env.taskState(t, id); state != string(engine.StateRemoved) {
			t.Errorf("state = %q, want removed", state)
		}
	})

	t.Run("force_complete removes the handle and completes", func(t *testing.T) {
		env := newActionsTestEnv(t)

		id := env.seedActionTask(t, nil)

		response := env.postActions(t, map[string]any{"ids": []string{id}, "action": actionForceComplete})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}
		if result := decodeActionsBody(t, response).Results[0]; !result.Ok {
			t.Errorf("result = %+v, want ok", result)
		}

		if calls := env.aria2.recorded(); !slices.Equal(calls, []string{"Remove aria2:" + aria2GID}) {
			t.Errorf("aria2 calls = %v, want one Remove with the data retained", calls)
		}
		if state := env.taskState(t, id); state != string(engine.StateCompleted) {
			t.Errorf("state = %q, want completed", state)
		}
		if codes := env.taskEventCodes(t, id); !slices.Equal(codes, []string{eventTaskForceCompleted}) {
			t.Errorf("event codes = %v, want [%s]", codes, eventTaskForceCompleted)
		}
	})

	t.Run("pausing a paused task is idempotent", func(t *testing.T) {
		env := newActionsTestEnv(t)

		id := env.seedActionTask(t, func(task *store.Task) { task.State = "paused" })

		response := env.postActions(t, map[string]any{"ids": []string{id}, "action": actionPause})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}
		if result := decodeActionsBody(t, response).Results[0]; !result.Ok {
			t.Errorf("result = %+v, want ok", result)
		}

		// A non-move writes no event and needs no engine round-trip.
		env.aria2.assertNoCalls(t)
		if codes := env.taskEventCodes(t, id); len(codes) != 0 {
			t.Errorf("event codes = %v, want none for an idempotent pause", codes)
		}
	})

	t.Run("an action on a removed task fails per-id", func(t *testing.T) {
		env := newActionsTestEnv(t)

		id := env.seedActionTask(t, func(task *store.Task) { task.State = "removed" })

		response := env.postActions(t, map[string]any{"ids": []string{id}, "action": actionPause})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}

		want := ActionResult{ID: id, Ok: false, Type: SlugValidationFailed, Detail: detailIllegalState}
		if result := decodeActionsBody(t, response).Results[0]; result != want {
			t.Errorf("result = %+v, want %+v", result, want)
		}
	})
}

// TestActionsRecheck pins the capability seam: against an engine without
// the optional Recheck method the action is a per-id failure that leaves
// the state unchanged, and against one with it the task moves to checking.
func TestActionsRecheck(t *testing.T) {
	t.Run("aria2 cannot recheck", func(t *testing.T) {
		env := newActionsTestEnv(t)

		id := env.seedActionTask(t, nil)

		response := env.postActions(t, map[string]any{"ids": []string{id}, "action": actionRecheck})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}

		want := ActionResult{
			ID: id, Ok: false, Type: SlugValidationFailed, Detail: detailUnsupportedAction,
		}
		if result := decodeActionsBody(t, response).Results[0]; result != want {
			t.Errorf("result = %+v, want %+v", result, want)
		}
		if state := env.taskState(t, id); state != string(engine.StateDownloading) {
			t.Errorf("state = %q, want unchanged downloading", state)
		}
		env.aria2.assertNoCalls(t)
	})

	t.Run("a capable engine moves the task to checking", func(t *testing.T) {
		env := newActionsTestEnvWithEngines(t,
			&recheckingEngine{actionEngine: *newActionEngine(engine.NameQBittorrent, acceptsBitTorrent)})

		ref := qbtHash
		id := env.seedActionTask(t, func(task *store.Task) {
			task.Engine = engine.NameQBittorrent
			task.EngineRef = &ref
		})

		response := env.postActions(t, map[string]any{"ids": []string{id}, "action": actionRecheck})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}
		if result := decodeActionsBody(t, response).Results[0]; !result.Ok {
			t.Errorf("result = %+v, want ok", result)
		}

		if state := env.taskState(t, id); state != string(engine.StateChecking) {
			t.Errorf("state = %q, want checking", state)
		}
		if codes := env.taskEventCodes(t, id); !slices.Equal(codes, []string{eventTaskRechecking}) {
			t.Errorf("event codes = %v, want [%s]", codes, eventTaskRechecking)
		}
	})
}

// TestActionsEngineErrorMapping pins the per-id error mapping of doc 05
// section 5.7: an unavailable engine and an unsupported action are per-id
// failures that leave the state unchanged, never request failures.
func TestActionsEngineErrorMapping(t *testing.T) {
	t.Run("engine unavailable", func(t *testing.T) {
		env := newActionsTestEnv(t)
		env.aria2.pauseErr = engine.ErrUnavailable

		id := env.seedActionTask(t, nil)

		response := env.postActions(t, map[string]any{"ids": []string{id}, "action": actionPause})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}

		want := ActionResult{
			ID: id, Ok: false, Type: SlugEngineUnavailable, Detail: detailEngineFailed,
		}
		if result := decodeActionsBody(t, response).Results[0]; result != want {
			t.Errorf("result = %+v, want %+v", result, want)
		}
		if state := env.taskState(t, id); state != string(engine.StateDownloading) {
			t.Errorf("state = %q, want unchanged downloading", state)
		}
	})

	t.Run("engine does not support the action", func(t *testing.T) {
		env := newActionsTestEnv(t)
		env.aria2.removeErr = engine.ErrNotSupported

		id := env.seedActionTask(t, nil)

		response := env.postActions(t, map[string]any{"ids": []string{id}, "action": actionRemove})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}

		want := ActionResult{
			ID: id, Ok: false, Type: SlugValidationFailed, Detail: detailUnsupportedAction,
		}
		if result := decodeActionsBody(t, response).Results[0]; result != want {
			t.Errorf("result = %+v, want %+v", result, want)
		}
		if state := env.taskState(t, id); state != string(engine.StateDownloading) {
			t.Errorf("state = %q, want unchanged downloading", state)
		}
	})

	t.Run("the engine is not registered", func(t *testing.T) {
		env := newActionsTestEnvWithEngines(t) // no engine at all

		id := env.seedActionTask(t, nil)

		response := env.postActions(t, map[string]any{"ids": []string{id}, "action": actionPause})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}

		want := ActionResult{
			ID: id, Ok: false, Type: SlugEngineUnavailable, Detail: detailEngineFailed,
		}
		if result := decodeActionsBody(t, response).Results[0]; result != want {
			t.Errorf("result = %+v, want %+v", result, want)
		}
		if state := env.taskState(t, id); state != string(engine.StateDownloading) {
			t.Errorf("state = %q, want unchanged downloading", state)
		}
	})
}

// TestQueueActionsTouchNoEngine pins the four queue actions: they rewrite
// tasks.queue_position inside one transaction and issue no engine call,
// because dl-tool owns the queue (doc 05 section 5.7).
func TestQueueActionsTouchNoEngine(t *testing.T) {
	// newQueue seeds four queued tasks at positions 1..4, every one held
	// by the engine so a queue action that called it would be caught.
	newQueue := func(t *testing.T) (*actionsTestEnv, [4]string) {
		env := newActionsTestEnv(t)

		var ids [4]string
		for i := range ids {
			position := int64(i + 1)
			ref := fmt.Sprintf("%s-%d", aria2GID, i)
			ids[i] = env.seedActionTask(t, func(task *store.Task) {
				task.State = "queued"
				task.EngineRef = &ref
				task.QueuePosition = &position
			})
		}

		return env, ids
	}

	t.Run("queue_top moves the task to the front", func(t *testing.T) {
		env, ids := newQueue(t)

		response := env.postActions(t, map[string]any{"ids": []string{ids[3]}, "action": actionQueueTop})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}

		want := []string{ids[3], ids[0], ids[1], ids[2]}
		if order := env.queueOrder(t); !slices.Equal(order, want) {
			t.Errorf("queue order = %v, want %v", order, want)
		}
		env.aria2.assertNoCalls(t)
		for _, id := range ids {
			if state := env.taskState(t, id); state != "queued" {
				t.Errorf("task %s state = %q, want unchanged queued", id, state)
			}
		}
	})

	t.Run("queue_bottom moves the task to the end", func(t *testing.T) {
		env, ids := newQueue(t)

		response := env.postActions(t, map[string]any{"ids": []string{ids[0]}, "action": actionQueueBottom})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}

		want := []string{ids[1], ids[2], ids[3], ids[0]}
		if order := env.queueOrder(t); !slices.Equal(order, want) {
			t.Errorf("queue order = %v, want %v", order, want)
		}
		env.aria2.assertNoCalls(t)
	})

	t.Run("queue_up advances one slot", func(t *testing.T) {
		env, ids := newQueue(t)

		response := env.postActions(t, map[string]any{"ids": []string{ids[2]}, "action": actionQueueUp})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}

		want := []string{ids[0], ids[2], ids[1], ids[3]}
		if order := env.queueOrder(t); !slices.Equal(order, want) {
			t.Errorf("queue order = %v, want %v", order, want)
		}
		env.aria2.assertNoCalls(t)
	})

	t.Run("queue_down retards one slot", func(t *testing.T) {
		env, ids := newQueue(t)

		response := env.postActions(t, map[string]any{"ids": []string{ids[1]}, "action": actionQueueDown})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}

		want := []string{ids[0], ids[2], ids[1], ids[3]}
		if order := env.queueOrder(t); !slices.Equal(order, want) {
			t.Errorf("queue order = %v, want %v", order, want)
		}
		env.aria2.assertNoCalls(t)
	})

	t.Run("a contiguous batch moves as a block", func(t *testing.T) {
		env, ids := newQueue(t)

		response := env.postActions(t, map[string]any{
			"ids":    []string{ids[1], ids[2]},
			"action": actionQueueTop,
		})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}

		want := []string{ids[1], ids[2], ids[0], ids[3]}
		if order := env.queueOrder(t); !slices.Equal(order, want) {
			t.Errorf("queue order = %v, want %v", order, want)
		}
		env.aria2.assertNoCalls(t)
	})

	t.Run("a task outside the queue fails per-id", func(t *testing.T) {
		env, ids := newQueue(t)

		positionless := env.seedActionTask(t, func(task *store.Task) {
			task.State = "queued"
			task.EngineRef = nil
		})

		response := env.postActions(t, map[string]any{
			"ids":    []string{ids[1], positionless, unknownID},
			"action": actionQueueUp,
		})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}

		want := []ActionResult{
			{ID: ids[1], Ok: true},
			{ID: positionless, Ok: false, Type: SlugValidationFailed, Detail: detailNotInQueue},
			{ID: unknownID, Ok: false, Type: SlugNotFound, Detail: detailTaskNotFound},
		}
		if results := decodeActionsBody(t, response).Results; !reflect.DeepEqual(results, want) {
			t.Errorf("results = %+v, want %+v", results, want)
		}
		env.aria2.assertNoCalls(t)
	})
}

// TestPatchTaskAppliesRateLimit pins the live rate limit: PATCH with
// dl_limit calls SetRateLimits once on the engine that holds the task and
// returns the updated Task object, without restarting anything.
func TestPatchTaskAppliesRateLimit(t *testing.T) {
	env := newActionsTestEnv(t)

	id := env.seedActionTask(t, nil)

	response := env.patchTask(t, id, map[string]any{"dl_limit": testDLLimit})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}

	task := decodeTaskBody(t, response)
	if task.DLLimit != testDLLimit {
		t.Errorf("dl_limit = %d, want %d", task.DLLimit, testDLLimit)
	}

	want := fmt.Sprintf("SetRateLimits aria2:%s %d nil", aria2GID, testDLLimit)
	if calls := env.aria2.recorded(); !slices.Equal(calls, []string{want}) {
		t.Errorf("aria2 calls = %v, want exactly [%s]", calls, want)
	}

	// The limit is persisted for the admission pass and the poller.
	var stored int64
	if err := env.db.GetContext(t.Context(), &stored,
		`SELECT dl_limit FROM tasks WHERE id = ?`, id); err != nil {
		t.Fatalf("read dl_limit: %v", err)
	}
	if stored != testDLLimit {
		t.Errorf("stored dl_limit = %d, want %d", stored, testDLLimit)
	}

	// A second patch in the other direction alone touches only that one.
	response = env.patchTask(t, id, map[string]any{"ul_limit": testDLLimit})
	if response.Code != http.StatusOK {
		t.Fatalf("second patch status = %d, body %s", response.Code, response.Body.String())
	}
	want = fmt.Sprintf("SetRateLimits aria2:%s nil %d", aria2GID, testDLLimit)
	if calls := env.aria2.recorded(); len(calls) != 2 || calls[1] != want {
		t.Errorf("aria2 calls = %v, want the second to carry the up direction alone", calls)
	}
	if task := decodeTaskBody(t, response); task.DLLimit != testDLLimit || task.ULLimit != testDLLimit {
		t.Errorf("limits = %d/%d, want both %d", task.DLLimit, task.ULLimit, testDLLimit)
	}
}

// TestPatchTaskFields pins the remaining patchable columns over HTTP: the
// name, category, tags, share limits and the sequential flag persist, an
// omitted field is untouched, and the field-level violations are 422.
func TestPatchTaskFields(t *testing.T) {
	env := newActionsTestEnv(t)

	if _, err := env.db.ExecContext(t.Context(),
		`INSERT INTO categories (id, name, save_path, created_at, updated_at)
VALUES (?, 'linux', '/data/linux', 0, 0)`, store.NewID(store.PrefixCategory)); err != nil {
		t.Fatalf("seed category: %v", err)
	}

	// A BitTorrent task: the patch below carries the fields only such an
	// engine takes (category, tags, share limits, sequential).
	id := env.seedBitTorrentTask(t, nil)
	response := env.patchTask(t, id, map[string]any{"tags": []string{"seed"}})
	if response.Code != http.StatusOK {
		t.Fatalf("seed tags: status %d body %s", response.Code, response.Body.String())
	}

	t.Run("every patchable field", func(t *testing.T) {
		response := env.patchTask(t, id, map[string]any{
			"name":               "renamed fixture",
			"category":           "linux",
			"tags":               []string{"iso", "iso-image"},
			"ratio_limit":        2.5,
			"seeding_time_limit": 3600,
			"sequential":         true,
		})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}

		task := decodeTaskBody(t, response)
		if task.Name != "renamed fixture" {
			t.Errorf("name = %q", task.Name)
		}
		if task.Category == nil || *task.Category != "linux" {
			t.Errorf("category = %v, want linux", task.Category)
		}
		if !slices.Equal(task.Tags, []string{"iso", "iso-image"}) {
			t.Errorf("tags = %v, want the replaced set", task.Tags)
		}
		if task.RatioLimit == nil || *task.RatioLimit != 2.5 {
			t.Errorf("ratio_limit = %v, want 2.5", task.RatioLimit)
		}
		if task.SeedingTimeLimit == nil || *task.SeedingTimeLimit != 3600 {
			t.Errorf("seeding_time_limit = %v, want 3600", task.SeedingTimeLimit)
		}
		if !task.Sequential {
			t.Errorf("sequential = false, want true")
		}

		var stored struct {
			Sequential int64    `db:"sequential"`
			Ratio      *float64 `db:"ratio_limit"`
			SeedTime   *int64   `db:"seeding_time_limit"`
		}
		if err := env.db.GetContext(t.Context(), &stored,
			`SELECT sequential, ratio_limit, seeding_time_limit FROM tasks WHERE id = ?`, id); err != nil {
			t.Fatalf("read patched row: %v", err)
		}
		if stored.Sequential != 1 || stored.Ratio == nil || *stored.Ratio != 2.5 ||
			stored.SeedTime == nil || *stored.SeedTime != 3600 {
			t.Errorf("stored row = %+v, want sequential, 2.5 and 3600", stored)
		}
		if tags := env.taskTags(t, id); !slices.Equal(tags, []string{"iso", "iso-image"}) {
			t.Errorf("stored tags = %v, want the replaced set", tags)
		}
	})

	t.Run("an empty tags array clears the set", func(t *testing.T) {
		response := env.patchTask(t, id, map[string]any{"tags": []string{}})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}
		if task := decodeTaskBody(t, response); len(task.Tags) != 0 {
			t.Errorf("tags = %v, want the empty set", task.Tags)
		}
		if tags := env.taskTags(t, id); len(tags) != 0 {
			t.Errorf("stored tags = %v, want none", tags)
		}
	})

	t.Run("omitted fields are untouched", func(t *testing.T) {
		response := env.patchTask(t, id, map[string]any{"name": "once more"})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}

		task := decodeTaskBody(t, response)
		if task.Name != "once more" {
			t.Errorf("name = %q", task.Name)
		}
		if task.Category == nil || *task.Category != "linux" {
			t.Errorf("category = %v, want the untouched linux", task.Category)
		}
		if task.RatioLimit == nil || *task.RatioLimit != 2.5 {
			t.Errorf("ratio_limit = %v, want the untouched 2.5", task.RatioLimit)
		}
	})

	t.Run("unknown category is a field error", func(t *testing.T) {
		response := env.patchTask(t, id, map[string]any{"category": "no-such-category"})
		problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
		if len(problem.Errors) != 1 || problem.Errors[0].Location != "body.category" {
			t.Errorf("errors = %+v, want the body.category field error", problem.Errors)
		}
	})

	t.Run("negative limits are field errors", func(t *testing.T) {
		for field, value := range map[string]any{
			"dl_limit":           -1,
			"ul_limit":           -1,
			"ratio_limit":        -0.5,
			"seeding_time_limit": -1,
		} {
			response := env.patchTask(t, id, map[string]any{field: value})
			problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
			if len(problem.Errors) != 1 || problem.Errors[0].Location != "body."+field {
				t.Errorf("%s: errors = %+v, want the body.%s field error", field, problem.Errors, field)
			}
		}
	})

	t.Run("empty name is a field error", func(t *testing.T) {
		response := env.patchTask(t, id, map[string]any{"name": ""})
		problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
		if len(problem.Errors) != 1 || problem.Errors[0].Location != "body.name" {
			t.Errorf("errors = %+v, want the body.name field error", problem.Errors)
		}
	})

	t.Run("unknown id is 404", func(t *testing.T) {
		response := env.patchTask(t, unknownID, map[string]any{"name": "anything"})
		assertProblem(t, response, http.StatusNotFound, SlugNotFound)
	})
}

// TestPatchTaskWithoutEngineHandle pins the admission-time path: a task no
// engine holds yet persists its limit without any engine round-trip.
func TestPatchTaskWithoutEngineHandle(t *testing.T) {
	env := newActionsTestEnv(t)

	id := env.seedActionTask(t, func(task *store.Task) { task.EngineRef = nil })

	response := env.patchTask(t, id, map[string]any{"dl_limit": testDLLimit})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
	}
	if task := decodeTaskBody(t, response); task.DLLimit != testDLLimit {
		t.Errorf("dl_limit = %d, want %d", task.DLLimit, testDLLimit)
	}
	env.aria2.assertNoCalls(t)
}

// TestPatchTaskEngineUnavailable pins the 503 of doc 05 section 5.5: when
// the engine cannot take the new limit, nothing is persisted.
func TestPatchTaskEngineUnavailable(t *testing.T) {
	env := newActionsTestEnv(t)
	env.aria2.limitsErr = engine.ErrUnavailable

	id := env.seedActionTask(t, nil)

	response := env.patchTask(t, id, map[string]any{"dl_limit": testDLLimit})
	assertProblem(t, response, http.StatusServiceUnavailable, SlugEngineUnavailable)

	var stored int64
	if err := env.db.GetContext(t.Context(), &stored,
		`SELECT dl_limit FROM tasks WHERE id = ?`, id); err != nil {
		t.Fatalf("read dl_limit: %v", err)
	}
	if stored != 0 {
		t.Errorf("stored dl_limit = %d, want the untouched default 0", stored)
	}
}

// TestPatchTaskMutatorsReachEngine pins the five live applications of
// doc 05 section 5.5: each patched field reaches the engine exactly once,
// under the engine-namespaced id, and a one-sided share-limit patch rides
// the stored half of the pair untouched.
func TestPatchTaskMutatorsReachEngine(t *testing.T) {
	seed := func(t *testing.T) (*actionsTestEnv, string) {
		t.Helper()

		env := newActionsTestEnv(t)
		if _, err := env.db.ExecContext(t.Context(),
			`INSERT INTO categories (id, name, save_path, created_at, updated_at)
VALUES (?, 'linux', '/data/linux', 0, 0)`, store.NewID(store.PrefixCategory)); err != nil {
			t.Fatalf("seed category: %v", err)
		}

		return env, env.seedBitTorrentTask(t, nil)
	}

	wantOneCall := func(t *testing.T, env *actionsTestEnv, want string) {
		t.Helper()

		if calls := env.qbittorrent.recorded(); !slices.Equal(calls, []string{want}) {
			t.Errorf("qbittorrent calls = %v, want exactly [%s]", calls, want)
		}
		env.aria2.assertNoCalls(t)
	}

	t.Run("category", func(t *testing.T) {
		env, id := seed(t)

		response := env.patchTask(t, id, map[string]any{"category": "linux"})
		if response.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", response.Code, response.Body.String())
		}
		wantOneCall(t, env, "SetCategory qbittorrent:"+qbtHash+" linux")
	})

	t.Run("tags", func(t *testing.T) {
		env, id := seed(t)

		response := env.patchTask(t, id, map[string]any{"tags": []string{"iso", "image"}})
		if response.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", response.Code, response.Body.String())
		}
		wantOneCall(t, env, "SetTags qbittorrent:"+qbtHash+" iso,image")
	})

	t.Run("sequential", func(t *testing.T) {
		env, id := seed(t)

		response := env.patchTask(t, id, map[string]any{"sequential": true})
		if response.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", response.Code, response.Body.String())
		}
		wantOneCall(t, env, "SetSequential qbittorrent:"+qbtHash+" true")
	})

	t.Run("both share limits", func(t *testing.T) {
		env, id := seed(t)

		response := env.patchTask(t, id, map[string]any{"ratio_limit": 2.5, "seeding_time_limit": 3600})
		if response.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", response.Code, response.Body.String())
		}
		// The seconds stored by dl-tool are passed through verbatim; the
		// qbittorrent adapter is the only place that converts to minutes.
		wantOneCall(t, env, "SetShareLimits qbittorrent:"+qbtHash+" 2.5 3600")
	})

	t.Run("a one-sided share patch keeps the stored half", func(t *testing.T) {
		env, id := seed(t)

		// Establish 2.5 in the row, then patch the time half alone: the
		// call must carry the stored ratio, not the use-global nil.
		if response := env.patchTask(t, id, map[string]any{"ratio_limit": 2.5}); response.Code != http.StatusOK {
			t.Fatalf("seed ratio: status %d, body %s", response.Code, response.Body.String())
		}
		if calls := env.qbittorrent.recorded(); len(calls) != 1 {
			t.Fatalf("qbittorrent calls = %v, want one seeding call", calls)
		}

		response := env.patchTask(t, id, map[string]any{"seeding_time_limit": 3600})
		if response.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", response.Code, response.Body.String())
		}
		calls := env.qbittorrent.recorded()
		// The first call pinned the ratio half alone with the time half
		// nil — the stored column is NULL — so the second carrying the
		// stored 2.5 is a real merge, not a coincidence of defaults.
		want := "SetShareLimits qbittorrent:" + qbtHash + " 2.5 3600"
		if len(calls) != 2 || calls[0] != "SetShareLimits qbittorrent:"+qbtHash+" 2.5 nil" || calls[1] != want {
			t.Errorf("qbittorrent calls = %v, want [%s 2.5 nil] then [%s]", calls, "SetShareLimits", want)
		}
	})

	t.Run("destination", func(t *testing.T) {
		env := newActionsTestEnv(t)
		id := env.seedBitTorrentTask(t, func(task *store.Task) { task.State = "seeding" })

		destination := filepath.Join(env.dataRoot, "linux")
		response := env.patchTask(t, id, map[string]any{"destination": destination})
		if response.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", response.Code, response.Body.String())
		}
		wantOneCall(t, env, "SetLocation qbittorrent:"+qbtHash+" "+destination)
	})
}

// TestPatchSequentialUnsupported pins the capability gate: a sequential
// patch against an engine that declares no such capability is a 422
// field error that reaches no engine and writes no row.
func TestPatchSequentialUnsupported(t *testing.T) {
	env := newActionsTestEnv(t)

	id := env.seedActionTask(t, nil) // aria2 declares no sequential

	response := env.patchTask(t, id, map[string]any{"sequential": true})
	problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	if len(problem.Errors) != 1 || problem.Errors[0].Location != "body.sequential" {
		t.Errorf("errors = %+v, want the body.sequential field error", problem.Errors)
	}

	env.aria2.assertNoCalls(t)
	var stored int64
	if err := env.db.GetContext(t.Context(), &stored,
		`SELECT sequential FROM tasks WHERE id = ?`, id); err != nil {
		t.Fatalf("read sequential: %v", err)
	}
	if stored != 0 {
		t.Errorf("stored sequential = %d, want the untouched default 0", stored)
	}
}

// TestPatchDestination pins the relocation flow: the engine is told the
// resolved location before the row changes, the row then carries it and
// an admitted task enters moving with one task.moved event, an
// unadmitted task just changes its destination, a path outside every
// root is the 403 that touches nothing, and a state that cannot enter
// moving is refused before the engine is ever told.
func TestPatchDestination(t *testing.T) {
	destinationInside := func(t *testing.T, env *actionsTestEnv) string {
		t.Helper()

		resolved, err := fsx.ResolveDestination([]string{env.dataRoot}, filepath.Join(env.dataRoot, "linux"))
		if err != nil {
			t.Fatalf("resolve destination: %v", err)
		}

		return resolved
	}

	t.Run("an admitted task enters moving", func(t *testing.T) {
		env := newActionsTestEnv(t)
		id := env.seedBitTorrentTask(t, func(task *store.Task) { task.State = "seeding" })

		destination := destinationInside(t, env)
		response := env.patchTask(t, id, map[string]any{"destination": destination})
		if response.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", response.Code, response.Body.String())
		}
		if calls := env.qbittorrent.recorded(); !slices.Equal(calls,
			[]string{"SetLocation qbittorrent:" + qbtHash + " " + destination}) {
			t.Errorf("qbittorrent calls = %v", calls)
		}

		var stored struct {
			Destination string `db:"destination"`
			State       string `db:"state"`
		}
		if err := env.db.GetContext(t.Context(), &stored,
			`SELECT destination, state FROM tasks WHERE id = ?`, id); err != nil {
			t.Fatalf("read row: %v", err)
		}
		if stored.Destination != destination {
			t.Errorf("destination = %q, want %q", stored.Destination, destination)
		}
		if stored.State != string(engine.StateMoving) {
			t.Errorf("state = %q, want moving", stored.State)
		}
		if codes := env.taskEventCodes(t, id); !slices.Equal(codes, []string{eventTaskMoved}) {
			t.Errorf("event codes = %v, want exactly one task.moved", codes)
		}
	})

	t.Run("an unadmitted task changes the column alone", func(t *testing.T) {
		env := newActionsTestEnv(t)
		id := env.seedActionTask(t, func(task *store.Task) { task.EngineRef = nil })

		destination := destinationInside(t, env)
		response := env.patchTask(t, id, map[string]any{"destination": destination})
		if response.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", response.Code, response.Body.String())
		}

		env.aria2.assertNoCalls(t)
		if state := env.taskState(t, id); state != string(engine.StateDownloading) {
			t.Errorf("state = %q, want the unchanged downloading", state)
		}
		var stored string
		if err := env.db.GetContext(t.Context(), &stored,
			`SELECT destination FROM tasks WHERE id = ?`, id); err != nil {
			t.Fatalf("read destination: %v", err)
		}
		if stored != destination {
			t.Errorf("destination = %q, want %q", stored, destination)
		}
	})

	t.Run("a destination outside every root is 403", func(t *testing.T) {
		env := newActionsTestEnv(t)
		id := env.seedBitTorrentTask(t, nil)

		response := env.patchTask(t, id, map[string]any{"destination": "/etc"})
		assertProblem(t, response, http.StatusForbidden, SlugPathRejected)

		env.qbittorrent.assertNoCalls(t)
		var stored string
		if err := env.db.GetContext(t.Context(), &stored,
			`SELECT destination FROM tasks WHERE id = ?`, id); err != nil {
			t.Fatalf("read destination: %v", err)
		}
		if stored != "/data" {
			t.Errorf("destination = %q, want the untouched /data", stored)
		}
	})

	t.Run("a state that cannot enter moving is 422 before the engine call", func(t *testing.T) {
		env := newActionsTestEnv(t)
		id := env.seedBitTorrentTask(t, nil) // downloading: no moving edge

		response := env.patchTask(t, id,
			map[string]any{"destination": filepath.Join(env.dataRoot, "linux")})
		assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

		env.qbittorrent.assertNoCalls(t)
		if state := env.taskState(t, id); state != string(engine.StateDownloading) {
			t.Errorf("state = %q, want the unchanged downloading", state)
		}
		var stored string
		if err := env.db.GetContext(t.Context(), &stored,
			`SELECT destination FROM tasks WHERE id = ?`, id); err != nil {
			t.Fatalf("read destination: %v", err)
		}
		if stored != "/data" {
			t.Errorf("destination = %q, want the untouched /data", stored)
		}
	})

	t.Run("a non-normalized in-root destination is stored cleaned", func(t *testing.T) {
		env := newActionsTestEnv(t)
		id := env.seedBitTorrentTask(t, func(task *store.Task) { task.State = "seeding" })

		// Built by concatenation so nothing cleans it before the handler.
		messy := env.dataRoot + "/linux/../linux"
		response := env.patchTask(t, id, map[string]any{"destination": messy})
		if response.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", response.Code, response.Body.String())
		}

		want := filepath.Join(env.dataRoot, "linux")
		if calls := env.qbittorrent.recorded(); !slices.Equal(calls,
			[]string{"SetLocation qbittorrent:" + qbtHash + " " + want}) {
			t.Errorf("qbittorrent calls = %v, want the cleaned %s", calls, want)
		}
		if task := decodeTaskBody(t, response); task.Destination != want {
			t.Errorf("destination = %q, want the cleaned %q", task.Destination, want)
		}
	})
}

// TestPatchRollsBackOnEngineFailure pins the 503 of doc 05 section 5.5 for
// every mutator: when the engine cannot take a patched field, nothing is
// persisted and the row keeps its columns and its event log untouched —
// for the relocation, also the state and the destination it would have
// moved.
func TestPatchRollsBackOnEngineFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fail  func(*actionEngine)
		body  map[string]any
		state string // the seeded state; the relocation needs a moving-entry one
	}{
		{
			name: "category",
			fail: func(e *actionEngine) { e.categoryErr = engine.ErrUnavailable },
			body: map[string]any{"name": "renamed fixture", "category": "linux"},
		},
		{
			name: "tags",
			fail: func(e *actionEngine) { e.tagsErr = engine.ErrUnavailable },
			body: map[string]any{"tags": []string{"iso"}},
		},
		{
			name: "sequential",
			fail: func(e *actionEngine) { e.sequentialErr = engine.ErrUnavailable },
			body: map[string]any{"sequential": true},
		},
		{
			name: "share limits",
			fail: func(e *actionEngine) { e.shareErr = engine.ErrUnavailable },
			body: map[string]any{"ratio_limit": 2.5},
		},
		{
			name:  "location",
			fail:  func(e *actionEngine) { e.locationErr = engine.ErrUnavailable },
			state: "seeding",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newActionsTestEnv(t)
			tc.fail(env.qbittorrent)

			if _, err := env.db.ExecContext(t.Context(),
				`INSERT INTO categories (id, name, save_path, created_at, updated_at)
VALUES (?, 'linux', '/data/linux', 0, 0)`, store.NewID(store.PrefixCategory)); err != nil {
				t.Fatalf("seed category: %v", err)
			}

			state := string(engine.StateDownloading)
			if tc.state != "" {
				state = tc.state
			}
			id := env.seedBitTorrentTask(t, func(task *store.Task) { task.State = state })

			body := tc.body
			if body == nil {
				body = map[string]any{"destination": filepath.Join(env.dataRoot, "linux")}
			}
			response := env.patchTask(t, id, body)
			assertProblem(t, response, http.StatusServiceUnavailable, SlugEngineUnavailable)

			if calls := env.qbittorrent.recorded(); len(calls) != 1 {
				t.Errorf("qbittorrent calls = %v, want the one failed call", calls)
			}

			var stored struct {
				Name        string  `db:"name"`
				CategoryID  *string `db:"category_id"`
				State       string  `db:"state"`
				Destination string  `db:"destination"`
			}
			if err := env.db.GetContext(t.Context(), &stored,
				`SELECT name, category_id, state, destination FROM tasks WHERE id = ?`, id); err != nil {
				t.Fatalf("read row: %v", err)
			}
			if stored.Name != "actions-fixture" || stored.CategoryID != nil ||
				stored.State != state || stored.Destination != "/data" {
				t.Errorf("row = %+v, want the untouched seeded values", stored)
			}
			if codes := env.taskEventCodes(t, id); len(codes) != 0 {
				t.Errorf("event codes = %v, want none", codes)
			}
		})
	}
}

// TestActionStandInCapsMatchAdapters fails when the stand-in capability
// lists stop mirroring the real adapters: the capability gates under
// test must keep answering production's verdicts, and a drifted list
// renders yesterday's instead. Both constructors perform no I/O, so the
// comparison needs no daemon.
func TestActionStandInCapsMatchAdapters(t *testing.T) {
	aria2Real, err := aria2.New(aria2.Config{URL: "http://aria2:6800/jsonrpc"}, nil)
	if err != nil {
		t.Fatalf("build aria2 adapter: %v", err)
	}
	qbtReal, err := qbittorrent.New(qbittorrent.Config{BaseURL: "http://qbittorrent:8080"}, nil)
	if err != nil {
		t.Fatalf("build qbittorrent adapter: %v", err)
	}

	env := newActionsTestEnv(t)
	if !slices.Equal(env.aria2.Capabilities(), aria2Real.Capabilities()) {
		t.Errorf("aria2 stand-in caps = %v, real = %v",
			env.aria2.Capabilities(), aria2Real.Capabilities())
	}
	if !slices.Equal(env.qbittorrent.Capabilities(), qbtReal.Capabilities()) {
		t.Errorf("qbittorrent stand-in caps = %v, real = %v",
			env.qbittorrent.Capabilities(), qbtReal.Capabilities())
	}
}

// The T127 pause-takeover suite. pauseEnv is a bare store, registry and
// handler set — no server, so the actionsTestEnv's 1 Hz admission loop
// cannot race the seeded rows: parked and hold-stamped rows are exactly
// what a live pass selects. The admitter built beside the handlers shares
// the registry, so the pass's task-operation lease and the pause action's
// wait are one table, the coupling the criteria drive.
type pauseEnv struct {
	db          *sqlx.DB
	tasks       *store.TaskStore
	aria2       *actionEngine
	registry    *engine.Registry
	admit       *engine.Admitter
	handlers    *TaskHandlers
	destination string
}

func newPauseEnv(t *testing.T) *pauseEnv {
	t.Helper()

	root := t.TempDir()
	db, err := store.Open(
		t.Context(),
		filepath.Join(root, "dl-tool.db"),
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

	aria2 := newActionEngine(engine.NameAria2, acceptsAria2Lanes)
	registry := engine.NewRegistry()
	registry.Register(aria2)
	tasks := store.NewTaskStore(db)

	return &pauseEnv{
		db:          db,
		tasks:       tasks,
		aria2:       aria2,
		registry:    registry,
		destination: root,
		admit:       engine.NewAdmitter(registry, tasks, time.Second, nil),
		handlers:    NewTaskHandlers(db, registry, nil),
	}
}

// seedPauseTask writes one task straight through the store with the env's
// destination, so the space gate resolves a real filesystem.
func (e *pauseEnv) seedPauseTask(t *testing.T, mutate func(*store.Task)) string {
	t.Helper()

	ref := aria2GID
	task := store.Task{
		Engine:      engine.NameAria2,
		EngineRef:   &ref,
		SourceKind:  "http",
		Name:        "pause-takeover-fixture",
		State:       "downloading",
		Destination: e.destination,
	}
	if mutate != nil {
		mutate(&task)
	}

	created, err := e.tasks.Create(t.Context(), task)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}

	return created.ID
}

// seedParkedRow seeds the row the disk-space guard leaves behind: paused
// carrying the named hold stamp, its transfer held by the engine.
func (e *pauseEnv) seedParkedRow(t *testing.T, code, message string) string {
	t.Helper()

	return e.seedPauseTask(t, func(task *store.Task) {
		task.State = "paused"
		task.ErrorCode = &code
		task.ErrorMessage = &message
	})
}

// pause drives one pause batch through the full action path under ctx.
func (e *pauseEnv) pause(ctx context.Context, t *testing.T, id string) ActionResult {
	t.Helper()

	in := &ActionsInput{}
	in.Body.IDs = []string{id}
	in.Body.Action = actionPause

	output, err := e.handlers.Actions(ctx, in)
	if err != nil {
		t.Fatalf("pause action: %v", err)
	}

	return output.Body.Results[0]
}

// pauseStale drives the pause path against one frozen preloaded
// snapshot — the stale actionTask a request loaded before its lease wait,
// with the row's state as it stood then. The lease-race tests need the
// preload pinned to the pre-holder state however the goroutine schedules,
// so the reload under the lease is the only thing that can move the
// decision.
func (e *pauseEnv) pauseStale(ctx context.Context, t *testing.T, id, state string) ActionResult {
	t.Helper()

	ref := aria2GID
	task := actionTask{ID: id, Engine: engine.NameAria2, EngineRef: &ref, State: state}

	return e.handlers.applyAction(ctx, task, actionPause, nil)
}

// taskState reads one task's state.
func (e *pauseEnv) taskState(t *testing.T, id string) string {
	t.Helper()

	var state string
	if err := e.db.GetContext(t.Context(), &state, `SELECT state FROM tasks WHERE id = ?`, id); err != nil {
		t.Fatalf("read state of %s: %v", id, err)
	}

	return state
}

// taskHoldCode reads one task's error-code pair, "" for an absent stamp.
func (e *pauseEnv) taskHoldCode(t *testing.T, id string) (code, message string) {
	t.Helper()

	var row struct {
		ErrorCode    *string `db:"error_code"`
		ErrorMessage *string `db:"error_message"`
	}
	if err := e.db.GetContext(t.Context(), &row,
		`SELECT error_code, error_message FROM tasks WHERE id = ?`, id); err != nil {
		t.Fatalf("read error code of %s: %v", id, err)
	}
	if row.ErrorCode != nil {
		code = *row.ErrorCode
	}
	if row.ErrorMessage != nil {
		message = *row.ErrorMessage
	}

	return code, message
}

// taskEventCodes reads one task's event codes in insert order — rowid,
// not the ULID id: two events of one millisecond would otherwise sort by
// random entropy, and the sequence assertions below compare pairs that
// can land inside the same millisecond.
func (e *pauseEnv) taskEventCodes(t *testing.T, id string) []string {
	t.Helper()

	var codes []string
	if err := e.db.SelectContext(t.Context(), &codes,
		`SELECT code FROM task_events WHERE task_id = ? ORDER BY at, rowid`, id); err != nil {
		t.Fatalf("read events of %s: %v", id, err)
	}

	return codes
}

// pauseAdmittingPolicy is the pass policy under which the env's filesystem
// admits everything — floor 0, no concurrency limits — the "space and a
// slot available" side of the takeover criteria.
func pauseAdmittingPolicy(destination string) engine.Policy {
	return engine.Policy{Roots: []string{destination}, MinFree: map[string]int64{destination: 0}}
}

// waitResumeRecorded parks until the admission pass's engine call appears
// in the stand-in's log — the observable proof the pass holds the task's
// lease, because that call runs only under it (T128's ordering).
func (e *pauseEnv) waitResumeRecorded(t *testing.T) {
	t.Helper()

	want := "Resume aria2:" + aria2GID
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if calls := e.aria2.recorded(); len(calls) > 0 {
			if calls[0] != want {
				t.Fatalf("engine calls = %v, want the pass's blocked %q first", calls, want)
			}

			return
		}
	}

	t.Fatal("the admission pass never reached its blocked engine call")
}

// TestOperatorPauseTakesOverAParkedRow pins the takeover itself: an
// operator pause on a guard-parked row is the idempotent branch — no
// engine round-trip, no second pause event — and it clears the hold
// stamp, so the admission pass no longer selects the row even with space
// and a slot available, and the engine sees no Resume.
func TestOperatorPauseTakesOverAParkedRow(t *testing.T) {
	env := newPauseEnv(t)
	id := env.seedParkedRow(t, engine.ErrorCodeDiskFull,
		"no space left on device; the task resumes once space returns")

	result := env.pause(t.Context(), t, id)
	if !result.Ok {
		t.Fatalf("result = %+v, want ok", result)
	}

	// The transfer is already engine-paused and the row already paused:
	// the idempotent branch writes neither an engine call nor an event.
	env.aria2.assertNoCalls(t)
	if codes := env.taskEventCodes(t, id); len(codes) != 0 {
		t.Errorf("event codes = %v, want none for the idempotent branch", codes)
	}
	if state := env.taskState(t, id); state != string(engine.StatePaused) {
		t.Errorf("state = %q, want paused", state)
	}
	if code, message := env.taskHoldCode(t, id); code != "" || message != "" {
		t.Errorf("hold stamp = (%q, %q), want it wiped", code, message)
	}

	// With room and a free slot the row is no candidate: it carries no
	// disk_full stamp to be selected by, so the pass releases nothing and
	// the engine still sees no Resume.
	released, err := env.admit.Pass(t.Context(), pauseAdmittingPolicy(env.destination))
	if err != nil {
		t.Fatalf("admission pass: %v", err)
	}
	if len(released) != 0 {
		t.Errorf("released = %v, want nothing", released)
	}
	env.aria2.assertNoCalls(t)
	if state := env.taskState(t, id); state != string(engine.StatePaused) {
		t.Errorf("state after the pass = %q, want still paused", state)
	}
}

// TestOperatorPauseKeepsANonHoldCode pins the guard's far side: a paused
// row carrying a code that is no hold stamp — an operator's own note, a
// real failure — keeps it. The takeover wipes hold stamps only.
func TestOperatorPauseKeepsANonHoldCode(t *testing.T) {
	env := newPauseEnv(t)

	code, message := "timeout", "the engine timed out mid-transfer"
	id := env.seedPauseTask(t, func(task *store.Task) {
		task.State = "paused"
		task.ErrorCode = &code
		task.ErrorMessage = &message
	})

	result := env.pause(t.Context(), t, id)
	if !result.Ok {
		t.Fatalf("result = %+v, want ok", result)
	}

	env.aria2.assertNoCalls(t)
	if gotCode, gotMessage := env.taskHoldCode(t, id); gotCode != code || gotMessage != message {
		t.Errorf("hold pair = (%q, %q), want the kept (%q, %q)", gotCode, gotMessage, code, message)
	}
	if state := env.taskState(t, id); state != string(engine.StatePaused) {
		t.Errorf("state = %q, want paused", state)
	}
}

// TestOperatorPauseOnAnActiveRowKeepsEngineFirst pins the unchanged half:
// a pause on an active task stays engine-first with exactly one pause
// event, and the hold stamp — however it rode the row — is cleared with
// the pause.
func TestOperatorPauseOnAnActiveRowKeepsEngineFirst(t *testing.T) {
	env := newPauseEnv(t)

	stamp, stampMessage := engine.ErrorCodeDiskFull, "no space left on device; the task resumes once space returns"
	id := env.seedPauseTask(t, func(task *store.Task) {
		task.ErrorCode = &stamp
		task.ErrorMessage = &stampMessage
	})

	result := env.pause(t.Context(), t, id)
	if !result.Ok {
		t.Fatalf("result = %+v, want ok", result)
	}

	if calls := env.aria2.recorded(); !slices.Equal(calls, []string{"Pause aria2:" + aria2GID}) {
		t.Errorf("aria2 calls = %v, want exactly one Pause", calls)
	}
	if codes := env.taskEventCodes(t, id); !slices.Equal(codes, []string{eventTaskPaused}) {
		t.Errorf("event codes = %v, want [%s]", codes, eventTaskPaused)
	}
	if state := env.taskState(t, id); state != string(engine.StatePaused) {
		t.Errorf("state = %q, want paused", state)
	}
	if code, _ := env.taskHoldCode(t, id); code != "" {
		t.Errorf("hold stamp = %q, want it wiped with the pause", code)
	}
}

// TestOperatorPauseClearsAQueuedRowHoldStamp pins the queued direction:
// a queued row carrying a hold stamp — the release window, where the
// engine already holds the transfer — is paused engine-first like any
// active row, and the stamp does not survive the transition into the
// paused+hold pair the pass would resume.
func TestOperatorPauseClearsAQueuedRowHoldStamp(t *testing.T) {
	holds := map[string]string{
		"disk full":        engine.ErrorCodeDiskFull,
		"concurrency hold": engine.ErrorCodeConcurrencyLimit,
	}
	for name, code := range holds {
		t.Run(name, func(t *testing.T) {
			env := newPauseEnv(t)

			stamp, message := code, "held by the pass"
			id := env.seedPauseTask(t, func(task *store.Task) {
				task.State = "queued"
				task.ErrorCode = &stamp
				task.ErrorMessage = &message
			})

			result := env.pause(t.Context(), t, id)
			if !result.Ok {
				t.Fatalf("result = %+v, want ok", result)
			}

			if calls := env.aria2.recorded(); !slices.Equal(calls, []string{"Pause aria2:" + aria2GID}) {
				t.Errorf("aria2 calls = %v, want exactly one Pause", calls)
			}
			if codes := env.taskEventCodes(t, id); !slices.Equal(codes, []string{eventTaskPaused}) {
				t.Errorf("event codes = %v, want [%s]", codes, eventTaskPaused)
			}
			if state := env.taskState(t, id); state != string(engine.StatePaused) {
				t.Errorf("state = %q, want paused", state)
			}
			if code, message := env.taskHoldCode(t, id); code != "" || message != "" {
				t.Errorf("hold stamp = (%q, %q), want it wiped with the pause", code, message)
			}
		})
	}
}

// TestOperatorPauseWaitsForTheAdmissionRelease pins the release-first
// ordering: while the pass holds the lease mid-release, the pause action
// parks instead of failing; once the release lands inside the budget, the
// action reloads the released row, applies the ordinary engine pause and
// transition, and leaves engine and row paused.
func TestOperatorPauseWaitsForTheAdmissionRelease(t *testing.T) {
	env := newPauseEnv(t)
	id := env.seedParkedRow(t, engine.ErrorCodeDiskFull,
		"no space left on device; the task resumes once space returns")

	// The pass holds the task's lease while its engine call blocks on the
	// gate; the recorded Resume proves the hold.
	env.aria2.resumeGate = make(chan struct{})
	passDone := make(chan struct{})
	go func() {
		defer close(passDone)

		if _, err := env.admit.Pass(t.Context(), pauseAdmittingPolicy(env.destination)); err != nil {
			t.Errorf("admission pass: %v", err)
		}
	}()
	env.waitResumeRecorded(t)

	// The operator's pause joins the lease behind the pass — waiting mode,
	// never a busy failure. The goroutine is not synchronised with the
	// gate close below on purpose: whether it parks before the handoff or
	// acquires the just-freed lease a moment later, it reloads the same
	// released row and takes the same branch, and the frozen paused
	// snapshot it preloads makes the reload the only thing that can move
	// the decision. That it waits rather than fails is pinned by the
	// timeout test's busy outcome behind a held lease.
	pauseDone := make(chan ActionResult, 1)
	go func() { pauseDone <- env.pauseStale(t.Context(), t, id, string(engine.StatePaused)) }()

	// The release completes inside the budget: the row turns downloading,
	// the stamp clears with the release, and the lease hands to the
	// waiting action.
	close(env.aria2.resumeGate)
	<-passDone

	var result ActionResult
	select {
	case result = <-pauseDone:
	case <-time.After(pauseLeaseWait + 5*time.Second):
		t.Fatal("the pause action never returned after the release")
	}
	if !result.Ok {
		t.Fatalf("result = %+v, want ok", result)
	}

	if calls := env.aria2.recorded(); !slices.Equal(calls,
		[]string{"Resume aria2:" + aria2GID, "Pause aria2:" + aria2GID}) {
		t.Errorf("aria2 calls = %v, want the release's Resume then the action's Pause", calls)
	}
	if codes := env.taskEventCodes(t, id); !slices.Equal(codes,
		[]string{eventTaskResumed, eventTaskPaused}) {
		t.Errorf("event codes = %v, want the release's resumed then one paused", codes)
	}
	if state := env.taskState(t, id); state != string(engine.StatePaused) {
		t.Errorf("state = %q, want paused", state)
	}
	if code, message := env.taskHoldCode(t, id); code != "" || message != "" {
		t.Errorf("hold stamp = (%q, %q), want none on the operator's row", code, message)
	}
}

// TestOperatorPauseAfterAFailedRelease pins the failed-release ordering:
// an admission release that answers ErrUnavailable inside the budget
// leaves the row parked with its stamp (the release cleanup's clear
// refuses paused rows), and the waiting action — reloading exactly that —
// skips its own engine call and pause event, then clears the stamp.
func TestOperatorPauseAfterAFailedRelease(t *testing.T) {
	env := newPauseEnv(t)
	id := env.seedParkedRow(t, engine.ErrorCodeDiskFull,
		"no space left on device; the task resumes once space returns")

	// The pass claims the row and fails both of its engine calls with
	// ErrUnavailable while holding the lease; the row is left exactly as
	// parked — the release cleanup's clear refuses paused rows.
	env.aria2.resumeGate = make(chan struct{})
	env.aria2.resumeErr = engine.ErrUnavailable
	env.aria2.getErr = engine.ErrUnavailable
	passDone := make(chan struct{})
	go func() {
		defer close(passDone)

		if _, err := env.admit.Pass(t.Context(), pauseAdmittingPolicy(env.destination)); err != nil {
			t.Errorf("admission pass: %v", err)
		}
	}()
	env.waitResumeRecorded(t)

	pauseDone := make(chan ActionResult, 1)
	go func() { pauseDone <- env.pauseStale(t.Context(), t, id, string(engine.StatePaused)) }()

	close(env.aria2.resumeGate)
	<-passDone

	var result ActionResult
	select {
	case result = <-pauseDone:
	case <-time.After(pauseLeaseWait + 5*time.Second):
		t.Fatal("the pause action never returned after the failed release")
	}
	if !result.Ok {
		t.Fatalf("result = %+v, want ok", result)
	}

	// The action added no engine call to the failed release's Resume, and
	// the idempotent branch wrote no pause event beside the stamp clear.
	if calls := env.aria2.recorded(); !slices.Equal(calls, []string{"Resume aria2:" + aria2GID}) {
		t.Errorf("aria2 calls = %v, want only the failed release's Resume", calls)
	}
	if codes := env.taskEventCodes(t, id); len(codes) != 0 {
		t.Errorf("event codes = %v, want none", codes)
	}
	if state := env.taskState(t, id); state != string(engine.StatePaused) {
		t.Errorf("state = %q, want paused", state)
	}
	if code, message := env.taskHoldCode(t, id); code != "" || message != "" {
		t.Errorf("hold stamp = (%q, %q), want the takeover's wipe", code, message)
	}
}

// TestOperatorPauseTimesOutBehindTheLease pins the bounded wait: a holder
// that outlives the budget gets the named per-id retry outcome with
// nothing mutated, and a retry after the holder releases succeeds. The
// five-second constant is pinned directly; the timeout itself is driven
// by a shorter parent context, not by sleeping the budget out.
func TestOperatorPauseTimesOutBehindTheLease(t *testing.T) {
	if pauseLeaseWait != 5*time.Second {
		t.Errorf("pauseLeaseWait = %s, want the five-second operator budget", pauseLeaseWait)
	}

	env := newPauseEnv(t)
	id := env.seedParkedRow(t, engine.ErrorCodeDiskFull,
		"no space left on device; the task resumes once space returns")

	// The pass parks in its engine call holding the lease, and stays there
	// past the action's budget.
	env.aria2.resumeGate = make(chan struct{})
	passDone := make(chan struct{})
	go func() {
		defer close(passDone)

		if _, err := env.admit.Pass(t.Context(), pauseAdmittingPolicy(env.destination)); err != nil {
			t.Errorf("admission pass: %v", err)
		}
	}()
	env.waitResumeRecorded(t)

	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()

	result := env.pause(ctx, t, id)
	want := ActionResult{ID: id, Ok: false, Type: SlugValidationFailed, Detail: detailTaskOpBusy}
	if result != want {
		t.Fatalf("result = %+v, want %+v", result, want)
	}

	// The action mutated nothing: the row is exactly what the blocked pass
	// left — the parked pair — and the engine has seen only the pass's
	// Resume.
	if calls := env.aria2.recorded(); !slices.Equal(calls, []string{"Resume aria2:" + aria2GID}) {
		t.Errorf("aria2 calls = %v, want only the pass's blocked Resume", calls)
	}
	if state := env.taskState(t, id); state != string(engine.StatePaused) {
		t.Errorf("state = %q, want still the parked paused", state)
	}
	if code, _ := env.taskHoldCode(t, id); code != engine.ErrorCodeDiskFull {
		t.Errorf("hold stamp = %q, want the parked pair untouched", code)
	}

	// The holder releases; a retry succeeds — on the released row now,
	// through the ordinary engine pause.
	close(env.aria2.resumeGate)
	<-passDone

	if result := env.pause(t.Context(), t, id); !result.Ok {
		t.Fatalf("retry result = %+v, want ok after the holder released", result)
	}
	if calls := env.aria2.recorded(); !slices.Equal(calls,
		[]string{"Resume aria2:" + aria2GID, "Pause aria2:" + aria2GID}) {
		t.Errorf("aria2 calls = %v, want the release's Resume then the retry's Pause", calls)
	}
	if state := env.taskState(t, id); state != string(engine.StatePaused) {
		t.Errorf("state = %q, want paused by the retry", state)
	}
	if code, _ := env.taskHoldCode(t, id); code != "" {
		t.Errorf("hold stamp = %q, want none", code)
	}
}

// selectionPauseStore lands the operator's takeover between the pass's
// selection and its lease: the transition plus the paused-row hold clear
// are exactly what the pause action writes, applied to the queued snapshot
// the pass is about to act on.
type selectionPauseStore struct {
	engine.AdmissionStore
	tasks *store.TaskStore
	pause string
}

func (s selectionPauseStore) SelectQueuedCandidates(ctx context.Context, limit int) ([]store.Candidate, error) {
	candidates, err := s.AdmissionStore.SelectQueuedCandidates(ctx, limit)
	if err != nil {
		return nil, err
	}

	for _, cand := range candidates {
		if cand.ID != s.pause {
			continue
		}

		if err := s.tasks.Transition(ctx, cand.ID, string(engine.StatePaused),
			store.CodeTaskPaused, "paused by user request"); err != nil {
			return nil, err
		}
		if _, err := s.tasks.ClearPausedHoldCode(ctx, cand.ID); err != nil {
			return nil, err
		}
	}

	return candidates, nil
}

// TestStaleQueuedReleaseAbortsUnderTheLease pins the queued row's far
// side: an operator pause that lands after the pass selected a stamped
// queued row is caught by the pass's under-lease revalidation — the
// stale queued snapshot is not released, no engine call is made, and the
// row stays operator-paused with no hold stamp.
func TestStaleQueuedReleaseAbortsUnderTheLease(t *testing.T) {
	env := newPauseEnv(t)

	stamp, message := engine.ErrorCodeDiskFull, "held by the pass"
	id := env.seedPauseTask(t, func(task *store.Task) {
		task.State = "queued"
		task.ErrorCode = &stamp
		task.ErrorMessage = &message
	})

	admit := engine.NewAdmitter(env.registry,
		selectionPauseStore{AdmissionStore: env.tasks, tasks: env.tasks, pause: id},
		time.Second, nil)

	released, err := admit.Pass(t.Context(), pauseAdmittingPolicy(env.destination))
	if err != nil {
		t.Fatalf("admission pass: %v", err)
	}
	if len(released) != 0 {
		t.Errorf("released = %v, want the stale snapshot aborted", released)
	}

	env.aria2.assertNoCalls(t)
	if state := env.taskState(t, id); state != string(engine.StatePaused) {
		t.Errorf("state = %q, want the operator's paused", state)
	}
	if code, _ := env.taskHoldCode(t, id); code != "" {
		t.Errorf("hold stamp = %q, want the takeover's wipe", code)
	}
	if codes := env.taskEventCodes(t, id); !slices.Equal(codes, []string{eventTaskPaused}) {
		t.Errorf("event codes = %v, want only the operator's [%s]", codes, eventTaskPaused)
	}
}
