package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/L-K-M/dl-tool/internal/api"
	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/store"
)

// fakeEngine is the Engine stand-in of the sweep tests: List answers from
// an injected snapshot, Add records the submission and answers from
// injected fields, and every method the sweep never calls stays a panic so
// an unexpected call fails the test loudly.
type fakeEngine struct {
	name      string
	infos     []engine.TaskInfo
	listErr   error
	addID     string
	addErr    error
	removeErr error

	// onAdd, when set, runs under Add's call — the hook a test uses to
	// cancel the caller's context mid-submission, the way a real engine
	// call dies when its context does.
	onAdd func(ctx context.Context)

	mu          sync.Mutex
	addCalls    []engine.AddRequest
	removeCalls []string
}

func (f *fakeEngine) Name() string                      { return f.name }
func (f *fakeEngine) Capabilities() []engine.Capability { return nil }
func (f *fakeEngine) Accepts(string) bool               { return false }
func (f *fakeEngine) Connect(context.Context) error {
	panic("not called by the reconciler")
}
func (f *fakeEngine) Close() error { return nil }
func (f *fakeEngine) Health(context.Context) (string, error) {
	panic("not called by the reconciler")
}

func (f *fakeEngine) Add(ctx context.Context, req engine.AddRequest) (string, error) {
	f.mu.Lock()
	f.addCalls = append(f.addCalls, req)
	hook := f.onAdd
	hookCtx := ctx
	f.mu.Unlock()

	if hook != nil {
		hook(hookCtx)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if f.addErr != nil {
		return "", f.addErr
	}
	return f.addID, nil
}

func (f *fakeEngine) List(context.Context) ([]engine.TaskInfo, error) {
	return f.infos, f.listErr
}

func (f *fakeEngine) Get(context.Context, string) (engine.TaskInfo, error) {
	panic("not called by the reconciler")
}
func (f *fakeEngine) Files(context.Context, string) ([]engine.FileEntry, error) {
	panic("not called by the reconciler")
}
func (f *fakeEngine) Pause(context.Context, string) error  { panic("not called") }
func (f *fakeEngine) Resume(context.Context, string) error { panic("not called") }
func (f *fakeEngine) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls = append(f.removeCalls, id)
	return f.removeErr
}
func (f *fakeEngine) SetFiles(context.Context, string, []int, map[int]int) error {
	panic("not called")
}
func (f *fakeEngine) SetLocation(context.Context, string, string) error { panic("not called") }
func (f *fakeEngine) Rename(context.Context, string, string) error      { panic("not called") }
func (f *fakeEngine) SetCategory(context.Context, string, string) error { panic("not called") }
func (f *fakeEngine) SetRateLimits(context.Context, string, *int64, *int64) error {
	panic("not called")
}
func (f *fakeEngine) SetShareLimits(context.Context, string, *float64, *int64) error {
	panic("not called")
}
func (f *fakeEngine) Events(context.Context) (<-chan engine.TaskEvent, error) {
	closed := make(chan engine.TaskEvent)
	close(closed)
	return closed, nil
}

// fakeTasks records every TaskWriter call. The reconciler works on the
// snapshot ListNonTerminalByEngine returns, so the recorded calls are the
// whole observable behaviour of a sweep.
type fakeTasks struct {
	mu       sync.Mutex
	byEngine map[string]map[string]store.Reconcilable

	listCalls   int
	progress    []string
	transitions []transitionRecord
	engineRefs  map[string]string
	events      []eventRecord

	// failProgress names tasks whose UpdateProgress fails, to exercise the
	// per-task isolation of a sweep.
	failProgress map[string]error

	// failSetEngineRef makes the first N SetEngineRef calls fail, to
	// exercise the compensating removal of a transfer whose handle could
	// not be recorded.
	failSetEngineRef int
}

type transitionRecord struct {
	id, next, code, message string
}

type eventRecord struct {
	id, level, code, message string
}

func newFakeTasks() *fakeTasks {
	return &fakeTasks{byEngine: map[string]map[string]store.Reconcilable{}, engineRefs: map[string]string{}}
}

func (f *fakeTasks) withEngine(name string, tasks map[string]store.Reconcilable) *fakeTasks {
	f.byEngine[name] = tasks
	return f
}

func (f *fakeTasks) ListNonTerminalByEngine(_ context.Context, engineName string) (map[string]store.Reconcilable, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return f.byEngine[engineName], nil
}

func (f *fakeTasks) UpdateProgress(_ context.Context, id string, p store.Progress) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, inject := f.failProgress[id]; inject {
		return err
	}
	f.progress = append(f.progress, id)
	return nil
}

func (f *fakeTasks) SetEngineRef(_ context.Context, id, engineRef string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSetEngineRef > 0 {
		f.failSetEngineRef--
		return errors.New("store: write failed")
	}
	f.engineRefs[id] = engineRef
	return nil
}

func (f *fakeTasks) Transition(_ context.Context, id, next, code, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transitions = append(f.transitions, transitionRecord{id, next, code, message})
	return nil
}

func (f *fakeTasks) AppendEvent(_ context.Context, taskID, level, code, message string, _ any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, eventRecord{taskID, level, code, message})
	return nil
}

func ptr(s string) *string { return &s }

func source(uri string) *string { return &uri }

// newSweep builds a registry with one engine and a reconciler over the
// recording store. The reconciler logs through slog.Default, so the
// log-asserting tests swap that for a buffered handler around the call.
func newSweep(t *testing.T, e engine.Engine, tasks engine.TaskWriter) *engine.Reconciler {
	t.Helper()

	reg := engine.NewRegistry()
	reg.Register(e)

	return engine.NewReconciler(reg, tasks, time.Hour, nil)
}

func TestBootWritesKnownHandles(t *testing.T) {
	total := int64(34896138)
	rate := int64(1048576)
	eta := int64(42)

	tasks := newFakeTasks().withEngine(engine.NameAria2, map[string]store.Reconcilable{
		"g1": {ID: "tsk_1", EngineRef: "g1", State: "queued", SourceURI: source("https://example.org/one")},
		"g2": {ID: "tsk_2", EngineRef: "g2", State: "downloading", SourceURI: source("https://example.org/two")},
	})

	e := &fakeEngine{
		name: engine.NameAria2,
		infos: []engine.TaskInfo{
			{
				ID: engine.NameAria2 + ":g1", Engine: engine.NameAria2, State: engine.StateDownloading,
				TotalBytes: &total, CompletedBytes: 1024, DownloadRate: rate, ETASeconds: &eta,
			},
			{ID: engine.NameAria2 + ":g2", Engine: engine.NameAria2, State: engine.StateDownloading},
		},
	}
	r := newSweep(t, e, tasks)

	if err := r.Boot(t.Context()); err != nil {
		t.Fatalf("Boot: %v", err)
	}

	// Both known handles got their counters written.
	if !slices.Equal(tasks.progress, []string{"tsk_1", "tsk_2"}) {
		t.Errorf("progress writes = %v, want [tsk_1 tsk_2]", tasks.progress)
	}

	// g1 moves queued -> downloading and the move is the reconciler's event;
	// g2 is unchanged and must produce no event and no delta.
	if len(tasks.transitions) != 1 {
		t.Fatalf("transitions = %+v, want exactly the g1 adoption", tasks.transitions)
	}
	got := tasks.transitions[0]
	if got.id != "tsk_1" || got.next != "downloading" || got.code != engine.CodeTaskReconciled {
		t.Errorf("transition = %+v, want tsk_1 -> downloading with %q", got, engine.CodeTaskReconciled)
	}
	if len(tasks.events) != 0 {
		t.Errorf("events = %+v, want none for unchanged and adopted handles", tasks.events)
	}
}

func TestForeignTransferIsIgnored(t *testing.T) {
	tasks := newFakeTasks().withEngine(engine.NameAria2, map[string]store.Reconcilable{
		"g1": {ID: "tsk_1", EngineRef: "g1", State: "downloading", SourceURI: source("https://example.org/one")},
	})

	// The engine reports one handle dl-tool owns and one it does not.
	e := &fakeEngine{
		name: engine.NameAria2,
		infos: []engine.TaskInfo{
			{ID: engine.NameAria2 + ":g1", Engine: engine.NameAria2, State: engine.StateDownloading},
			{ID: engine.NameAria2 + ":foreign", Engine: engine.NameAria2, State: engine.StateSeeding},
		},
	}
	r := newSweep(t, e, tasks)

	if err := r.Boot(t.Context()); err != nil {
		t.Fatalf("Boot: %v", err)
	}

	// The foreign handle produces no tasks row, no task_events row and no
	// re-submission: exactly one progress write, for the known handle.
	if !slices.Equal(tasks.progress, []string{"tsk_1"}) {
		t.Errorf("progress writes = %v, want [tsk_1] alone", tasks.progress)
	}
	if len(tasks.transitions) != 0 || len(tasks.events) != 0 {
		t.Errorf("transitions = %+v events = %+v, want none", tasks.transitions, tasks.events)
	}
	if len(e.addCalls) != 0 {
		t.Errorf("Add calls = %+v, want none for a foreign handle", e.addCalls)
	}
}

func TestVanishedHandleIsResubmitted(t *testing.T) {
	uri := "https://example.org/resume-me"
	tasks := newFakeTasks().withEngine(engine.NameAria2, map[string]store.Reconcilable{
		"gone": {
			ID: "tsk_1", EngineRef: "gone", State: "downloading",
			SourceURI: source(uri), Destination: "/data",
		},
	})

	// The engine lists nothing: the handle vanished with the daemon.
	e := &fakeEngine{name: engine.NameAria2, addID: engine.NameAria2 + ":newgid"}
	r := newSweep(t, e, tasks)

	if err := r.Boot(t.Context()); err != nil {
		t.Fatalf("Boot: %v", err)
	}

	if len(e.addCalls) != 1 {
		t.Fatalf("Add calls = %d, want 1", len(e.addCalls))
	}
	req := e.addCalls[0]
	if !slices.Equal(req.URIs, []string{uri}) {
		t.Errorf("Add URIs = %v, want [%s]", req.URIs, uri)
	}
	if req.SaveDir != "/data" {
		t.Errorf("Add SaveDir = %q, want /data", req.SaveDir)
	}
	if req.Extra["continue"] != "true" {
		t.Errorf("Add Extra = %v, want continue=true for aria2 resume semantics", req.Extra)
	}

	if ref := tasks.engineRefs["tsk_1"]; ref != "newgid" {
		t.Errorf("engine_ref of tsk_1 = %q, want newgid", ref)
	}
	if len(tasks.events) != 1 || tasks.events[0].code != engine.CodeTaskReconciled {
		t.Errorf("events = %+v, want one %s", tasks.events, engine.CodeTaskReconciled)
	}
}

func TestVanishedHandleErrors(t *testing.T) {
	tasks := newFakeTasks().withEngine(engine.NameAria2, map[string]store.Reconcilable{
		"gone": {
			ID: "tsk_1", EngineRef: "gone", State: "downloading",
			SourceURI: source("https://example.org/resume-me"),
		},
		"left": {
			ID: "tsk_2", EngineRef: "left", State: "seeding",
			InfohashV1: ptr("0123456789abcdef0123456789abcdef01234567"),
		},
	})

	e := &fakeEngine{name: engine.NameAria2, addErr: errors.New("daemon refused")}
	r := newSweep(t, e, tasks)

	if err := r.Boot(t.Context()); err != nil {
		t.Fatalf("Boot: %v, want the refused re-submissions recorded, not failed", err)
	}

	// Every vanished task that fails its re-submission moves to error with
	// engine.unavailable — one transition each, and no engine_ref recorded.
	if len(tasks.transitions) != 2 {
		t.Fatalf("transitions = %+v, want one failure per vanished task", tasks.transitions)
	}
	for _, got := range tasks.transitions {
		if got.next != "error" || got.code != store.CodeEngineUnavailable {
			t.Errorf("transition = %+v, want -> error with %q", got, store.CodeEngineUnavailable)
		}
	}
	wantIDs := []string{"tsk_1", "tsk_2"}
	for i, got := range tasks.transitions {
		if got.id != wantIDs[i] {
			t.Errorf("transition %d id = %q, want %q (sorted handle order)", i, got.id, wantIDs[i])
		}
	}
	if len(tasks.engineRefs) != 0 {
		t.Errorf("engine refs = %v, want none recorded for a refused re-submission", tasks.engineRefs)
	}
	if len(tasks.events) != 0 {
		t.Errorf("events = %+v, want none beside each transition's own row", tasks.events)
	}
}

func TestVanishedHandleForTerminalTaskChangesNothing(t *testing.T) {
	// The store filter never lists terminal tasks; the sweep must hold even
	// if it ever saw one, so the guard is tested directly.
	tasks := newFakeTasks().withEngine(engine.NameAria2, map[string]store.Reconcilable{
		"done":    {ID: "tsk_1", EngineRef: "done", State: "completed"},
		"deleted": {ID: "tsk_2", EngineRef: "deleted", State: "removed"},
	})

	e := &fakeEngine{name: engine.NameAria2} // lists nothing: both handles vanished
	r := newSweep(t, e, tasks)

	if err := r.Boot(t.Context()); err != nil {
		t.Fatalf("Boot: %v", err)
	}

	if len(e.addCalls) != 0 || len(tasks.transitions) != 0 || len(tasks.events) != 0 || len(tasks.progress) != 0 {
		t.Errorf("terminal tasks were touched: adds=%d transitions=%+v events=%+v progress=%v",
			len(e.addCalls), tasks.transitions, tasks.events, tasks.progress)
	}
}

func TestVanishedHandleForQueuedAndPausedTasksLeftAlone(t *testing.T) {
	tasks := newFakeTasks().withEngine(engine.NameAria2, map[string]store.Reconcilable{
		"q": {ID: "tsk_1", EngineRef: "q", State: "queued", SourceURI: source("https://example.org/q")},
		"p": {ID: "tsk_2", EngineRef: "p", State: "paused", SourceURI: source("https://example.org/p")},
	})

	e := &fakeEngine{name: engine.NameAria2, addID: engine.NameAria2 + ":newgid"}
	r := newSweep(t, e, tasks)

	if err := r.Boot(t.Context()); err != nil {
		t.Fatalf("Boot: %v", err)
	}

	// Admission (T098) owns starting these; the reconciler must not race it.
	if len(e.addCalls) != 0 {
		t.Errorf("Add calls = %+v, want none for queued or paused tasks", e.addCalls)
	}
}

func TestUnreachableEngineLogsAndChangesNoState(t *testing.T) {
	tasks := newFakeTasks().withEngine(engine.NameAria2, map[string]store.Reconcilable{
		"g1": {ID: "tsk_1", EngineRef: "g1", State: "downloading", SourceURI: source("https://example.org/one")},
	})

	// The reconciler logs through slog.Default; swap it for a buffer so the
	// warning is observable, and restore it before the test ends.
	logs := &strings.Builder{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	defer slog.SetDefault(previous)

	e := &fakeEngine{name: engine.NameAria2, listErr: engine.ErrUnavailable}
	r := newSweep(t, e, tasks)

	if err := r.Boot(t.Context()); err != nil {
		t.Fatalf("Boot: %v, want nil — an unreachable engine is a warning", err)
	}

	if len(tasks.progress) != 0 || len(tasks.transitions) != 0 || len(tasks.events) != 0 {
		t.Errorf("task state changed under an unreachable engine: %+v %+v %+v",
			tasks.progress, tasks.transitions, tasks.events)
	}

	logged := logs.String()
	if !strings.Contains(logged, "engine unreachable") || !strings.Contains(logged, "engine="+engine.NameAria2) {
		t.Errorf("logs = %q, want one unreachable warning carrying the engine attribute", logged)
	}
}

// TestNewServerReconcilesBeforeServing observes the reconciler through the
// composition root: NewServer, pointed at a fake aria2 daemon and a real
// store, must run one full sweep before it returns — the seeded task row
// leaves queued with the engine-reported state and counters already
// written, which is only possible if the boot sweep ran inside NewServer,
// ahead of main's listener.
func TestNewServerReconcilesBeforeServing(t *testing.T) {
	const gid = "2089b05ecca3d829"

	// A minimal aria2 JSON-RPC endpoint. The reconciler's sweep arrives as
	// a batch array, the boot probe (T027) as one single-object
	// aria2.getVersion call, so the fake decodes either shape and mirrors
	// it in its reply — the client decodes the shape it sent.
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		type fakeRPC struct {
			ID     string `json:"id"`
			Method string `json:"method"`
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read rpc body: %v", err)
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}

		var requests []fakeRPC
		single := !bytes.HasPrefix(bytes.TrimSpace(body), []byte("["))
		if single {
			var req fakeRPC
			err = json.Unmarshal(body, &req)
			requests = append(requests, req)
		} else {
			err = json.Unmarshal(body, &requests)
		}
		if err != nil {
			t.Errorf("decode rpc request: %v", err)
			http.Error(w, "decode error", http.StatusBadRequest)
			return
		}

		replies := make([]map[string]any, 0, len(requests))
		for _, req := range requests {
			result := any([]any{})
			switch req.Method {
			case "aria2.tellActive":
				result = []any{map[string]any{
					"gid":             gid,
					"status":          "active",
					"totalLength":     "1000",
					"completedLength": "100",
					"downloadSpeed":   "1234",
					"dir":             "/data",
				}}
			case "aria2.getVersion":
				result = map[string]any{"version": "1.37.0"}
			}
			replies = append(replies, map[string]any{
				"jsonrpc": "2.0", "id": req.ID, "result": result,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		if single {
			if err := json.NewEncoder(w).Encode(replies[0]); err != nil {
				// This fake deliberately outlives the test (see the note below);
				// a testing.T method here would panic if the leaked reconciler's
				// poll hits an encode error after completion. The test's own
				// reconciliation assertions catch genuinely broken replies.
				return
			}
			return
		}
		if err := json.NewEncoder(w).Encode(replies); err != nil {
			// Same note as above.
			return
		}
	}))
	// The reconciler's Run loop starts inside NewServer under the process
	// lifetime and has no test-visible stop, so this server and the store
	// below deliberately outlive the test: closed, the loop would warn once
	// a second through its construction-time logger for the rest of the
	// process, while healthy it sweeps silently forever. Only the store
	// lives on TempDir files — t.TempDir unlinks them at cleanup, and the
	// open handles keep the inodes usable until exit; the daemon is a
	// listener and goroutine the process reaps at exit.

	root := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(root, "dl-tool.db"), filepath.Join(root, "backups"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	// One non-terminal task whose handle the fake daemon reports active.
	tasks := store.NewTaskStore(db)
	uri := "https://example.org/file.iso"
	task, err := tasks.Create(t.Context(), store.Task{
		Engine: engine.NameAria2, EngineRef: ptr(gid), SourceKind: "http", SourceURI: &uri,
		Name: "file.iso", State: "queued", Destination: "/data",
	})
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}

	_, err = api.NewServer(
		&config.Config{ConfigDir: root, Aria2URL: rpc.URL, SessionTTL: time.Hour},
		db,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// NewServer has returned; the boot sweep must already have adopted the
	// engine's report for the seeded row.
	after, err := tasks.Get(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("re-read task: %v", err)
	}
	if after.State != "downloading" {
		t.Errorf("task state after NewServer = %q, want downloading (adopted at boot)", after.State)
	}
	if after.CompletedBytes != 100 || after.DownloadRate != 1234 {
		t.Errorf("counters after NewServer = %d/%d, want 100/1234", after.CompletedBytes, after.DownloadRate)
	}
}

func TestRunStopsWithContext(t *testing.T) {
	tasks := newFakeTasks()
	e := &fakeEngine{name: engine.NameAria2}
	reg := engine.NewRegistry()
	reg.Register(e)
	r := engine.NewReconciler(reg, tasks, 10*time.Millisecond, nil)

	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Wait until at least two sweeps ran, so the ticker is provably live
	// before the cancellation is exercised. The deadline is computed once:
	// a fresh time.After per iteration could never win against the 2 ms
	// sleep, so a dead ticker would spin here until the package timeout.
	deadline := time.Now().Add(5 * time.Second)
	for {
		tasks.mu.Lock()
		sweeps := tasks.listCalls
		tasks.mu.Unlock()
		if sweeps >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fewer than two sweeps ran before the deadline (sweeps=%d)", sweeps)
		}
		time.Sleep(2 * time.Millisecond)
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	// The goroutine that ran Run has returned; give it a moment to retire,
	// but fail only if the count never settles back to the baseline — a
	// single sample after a fixed sleep flakes under CI load.
	settle := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(settle) {
			t.Errorf("goroutines before=%d after=%d, Run leaked a goroutine", before, runtime.NumGoroutine())
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// One task's write-back failure must not stop the sweep: the engine List
// already succeeded, so the failure is per task — logged and skipped — and
// the other tasks still get their writes, with Boot returning nil.
func TestSweepSurvivesWriteBackFailure(t *testing.T) {
	tasks := newFakeTasks().withEngine(engine.NameAria2, map[string]store.Reconcilable{
		"g1": {ID: "tsk_bad", EngineRef: "g1", State: "downloading", SourceURI: source("https://example.org/one")},
		"g2": {ID: "tsk_ok", EngineRef: "g2", State: "downloading", SourceURI: source("https://example.org/two")},
	})
	tasks.failProgress = map[string]error{"tsk_bad": errors.New("store: row deleted mid-sweep")}

	e := &fakeEngine{
		name: engine.NameAria2,
		infos: []engine.TaskInfo{
			{ID: engine.NameAria2 + ":g1", Engine: engine.NameAria2, State: engine.StateDownloading},
			{ID: engine.NameAria2 + ":g2", Engine: engine.NameAria2, State: engine.StateDownloading},
		},
	}
	r := newSweep(t, e, tasks)

	if err := r.Boot(t.Context()); err != nil {
		t.Fatalf("Boot: %v, want nil — the failure is per task, not store-wide", err)
	}

	tasks.mu.Lock()
	defer tasks.mu.Unlock()
	if !slices.Contains(tasks.progress, "tsk_ok") {
		t.Errorf("progress writes = %v, want tsk_ok written after tsk_bad failed", tasks.progress)
	}
	if ref, ok := tasks.engineRefs["tsk_ok"]; ok {
		t.Errorf("engine_ref for tsk_ok = %q, want none — its handle is still known", ref)
	}
}

// A caller-side cancellation mid-Add — the boot budget expiring, a shutdown —
// is not an engine refusal: no task moves to error, no engine.unavailable
// event is written, and Boot returns the context error for the caller to warn
// about once.
func TestCancelledReconcileDoesNotErrorTasks(t *testing.T) {
	tasks := newFakeTasks().withEngine(engine.NameAria2, map[string]store.Reconcilable{
		"g1": {ID: "tsk_1", EngineRef: "g1", State: "downloading", SourceURI: source("https://example.org/one")},
	})

	ctx, cancel := context.WithCancel(t.Context())
	e := &fakeEngine{
		name:  engine.NameAria2,
		addID: engine.NameAria2 + ":newgid",
		onAdd: func(context.Context) { cancel() },
	}
	r := newSweep(t, e, tasks)

	if err := r.Boot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Boot: %v, want context.Canceled", err)
	}
	if len(tasks.transitions) != 0 {
		t.Errorf("transitions = %+v, want none — cancellation is not a refusal", tasks.transitions)
	}
	if len(tasks.events) != 0 {
		t.Errorf("events = %+v, want none", tasks.events)
	}
}

// A successful Add whose SetEngineRef fails must not leave the transfer
// engine-side unnamed: the next sweep would re-add it, and every duplicate
// would be foreign under ADR-0017 — untouchable. The reconciler removes the
// transfer it just created, so a retrying store blip costs one Add per
// sweep, not one duplicate per sweep.
func TestFailedSetEngineRefCompensatesByRemoving(t *testing.T) {
	tasks := newFakeTasks().withEngine(engine.NameAria2, map[string]store.Reconcilable{
		"g1": {ID: "tsk_1", EngineRef: "g1", State: "downloading", SourceURI: source("https://example.org/one")},
	})
	tasks.failSetEngineRef = 1

	e := &fakeEngine{name: engine.NameAria2, addID: engine.NameAria2 + ":newgid"}
	r := newSweep(t, e, tasks)

	if err := r.Boot(t.Context()); err != nil {
		t.Fatalf("Boot: %v, want nil — the per-task failure is logged, not surfaced", err)
	}
	e.mu.Lock()
	removes := append([]string(nil), e.removeCalls...)
	e.mu.Unlock()
	if len(removes) != 1 || removes[0] != engine.NameAria2+":newgid" {
		t.Fatalf("Remove calls = %v, want exactly one for the just-added handle", removes)
	}

	// The retry records the next handle, so exactly one Add ran per sweep.
	if err := r.Boot(t.Context()); err != nil {
		t.Fatalf("second Boot: %v", err)
	}
	e.mu.Lock()
	adds := len(e.addCalls)
	totalRemoves := len(e.removeCalls)
	e.mu.Unlock()
	if adds != 2 {
		t.Errorf("Add calls = %d, want 2 (one per sweep, no duplicates)", adds)
	}
	if totalRemoves != 1 {
		t.Errorf("Remove calls = %d, want the single compensation of the first sweep", totalRemoves)
	}

	tasks.mu.Lock()
	defer tasks.mu.Unlock()
	if ref := tasks.engineRefs["tsk_1"]; ref != "newgid" {
		t.Errorf("engine_ref = %q, want newgid after the retry", ref)
	}
}

// A sweep that dies mid-flight because the owner cancelled the context is
// shutdown, not an outage: Run must return the context error without logging
// the "reconciliation sweep failed" warning.
func TestRunSweepCancellationIsNotAWarn(t *testing.T) {
	tasks := newFakeTasks().withEngine(engine.NameAria2, map[string]store.Reconcilable{
		"gone": {ID: "tsk_1", EngineRef: "gone", State: "downloading", SourceURI: source("https://example.org/one")},
	})

	// The reconciler logs through its own injected logger — none of the
	// swap machinery — so the absent warning is observable without touching
	// slog.Default().
	logs := &strings.Builder{}

	ctx, cancel := context.WithCancel(t.Context())
	e := &fakeEngine{
		name:  engine.NameAria2,
		addID: engine.NameAria2 + ":newgid",
		onAdd: func(context.Context) { cancel() },
	}
	reg := engine.NewRegistry()
	reg.Register(e)
	r := engine.NewReconciler(reg, tasks, 10*time.Millisecond, slog.New(slog.NewTextHandler(logs, nil)))

	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after a cancelled sweep")
	}
	if strings.Contains(logs.String(), "reconciliation sweep failed") {
		t.Errorf("shutdown was logged as a sweep failure: %q", logs.String())
	}
}

// Cancellation is not an outage anywhere in the sweep, including the
// engine listing: a cancelled context surfaces its own error and logs no
// "unreachable" warning, while a live context's listing failure still
// warns and changes no state.
func TestCancelledListIsNotUnreachable(t *testing.T) {
	tasks := newFakeTasks()

	logBuffer := &strings.Builder{}
	reg := engine.NewRegistry()
	reg.Register(&fakeEngine{name: engine.NameAria2, listErr: engine.ErrUnavailable})
	r := engine.NewReconciler(reg, tasks, time.Hour,
		slog.New(slog.NewTextHandler(logBuffer, nil)))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// A cancelled context over a failing List: the context error surfaces,
	// no warning is logged.
	if err := r.Boot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Boot: %v, want context.Canceled", err)
	}
	if strings.Contains(logBuffer.String(), "engine unreachable") {
		t.Errorf("cancellation was logged as an outage: %q", logBuffer.String())
	}

	// A live context over the same failing List: a warning, no state
	// change, and no Boot failure — the unreachable-engine policy.
	if err := r.Boot(t.Context()); err != nil {
		t.Fatalf("Boot: %v, want nil — an unreachable engine is a warning", err)
	}
	if !strings.Contains(logBuffer.String(), "engine unreachable") {
		t.Errorf("live-context listing failure was not warned: %q", logBuffer.String())
	}
	if len(tasks.progress) != 0 || len(tasks.transitions) != 0 {
		t.Errorf("task state changed under an unreachable engine")
	}
}
