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
}

// newActionsTestEnv builds the env with both stand-ins registered.
func newActionsTestEnv(t *testing.T) *actionsTestEnv {
	t.Helper()

	aria2 := newActionEngine(engine.NameAria2, acceptsAria2Lanes)
	qbittorrent := newActionEngine(engine.NameQBittorrent, acceptsBitTorrent)
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
		api:    humatest.Wrap(t, server.API),
		db:     db,
		bearer: seedLiveAPIToken(t, db, user.ID),
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

	mu    sync.Mutex
	calls []string

	pauseErr   error
	removeErr  error
	limitsErr  error
	recheckErr error
}

func newActionEngine(name string, accepts func(string) bool) *actionEngine {
	return &actionEngine{name: name, accepts: accepts}
}

func (e *actionEngine) Name() string                      { return e.name }
func (e *actionEngine) Capabilities() []engine.Capability { return nil }
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
func (e *actionEngine) Get(context.Context, string) (engine.TaskInfo, error) {
	return engine.TaskInfo{}, engine.ErrNotFound
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

	return nil
}
func (e *actionEngine) Remove(_ context.Context, id string) error {
	e.record("Remove " + id)

	return e.removeErr
}

func (e *actionEngine) SetFiles(context.Context, string, []int, map[int]int) error {
	return engine.ErrNotSupported
}
func (e *actionEngine) SetLocation(context.Context, string, string) error {
	return engine.ErrNotSupported
}
func (e *actionEngine) Rename(context.Context, string, string) error { return engine.ErrNotSupported }
func (e *actionEngine) SetCategory(context.Context, string, string) error {
	return engine.ErrNotSupported
}

func (e *actionEngine) SetRateLimits(_ context.Context, id string, down, up *int64) error {
	e.record("SetRateLimits " + id + " " + limitArgs(down, up))

	return e.limitsErr
}

func (e *actionEngine) SetShareLimits(context.Context, string, *float64, *int64) error {
	return engine.ErrNotSupported
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

	id := env.seedActionTask(t, nil)
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
