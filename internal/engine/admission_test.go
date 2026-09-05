package engine_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/store"
)

// admitEngine is the admission tests' Engine stand-in: Add mints a
// namespaced handle per submission, Resume records its calls, and
// failures are injectable per method. Every method the pass never calls
// panics, so an unexpected call fails the test loudly.
type admitEngine struct {
	name string

	mu        sync.Mutex
	next      int
	adds      []string // the URIs of every accepted submission
	resumes   []string // the engine task ids every Resume saw
	addErr    error
	resumeErr error
}

func newAdmitEngine(name string) *admitEngine {
	return &admitEngine{name: name}
}

func (e *admitEngine) Name() string                      { return e.name }
func (e *admitEngine) Capabilities() []engine.Capability { return nil }
func (e *admitEngine) Accepts(string) bool               { return false }
func (e *admitEngine) Connect(context.Context) error     { return nil }
func (e *admitEngine) Close() error                      { return nil }
func (e *admitEngine) Health(context.Context) (string, error) {
	return "fake", nil
}

func (e *admitEngine) Add(_ context.Context, req engine.AddRequest) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.addErr != nil {
		return "", e.addErr
	}
	e.next++
	e.adds = append(e.adds, req.URIs...)

	return fmt.Sprintf("%s:gid%03d", e.name, e.next), nil
}

func (e *admitEngine) Resume(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.resumeErr != nil {
		return e.resumeErr
	}
	e.resumes = append(e.resumes, id)

	return nil
}

func (e *admitEngine) Remove(context.Context, string) error { return nil }

func (e *admitEngine) recordedAdds() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]string(nil), e.adds...)
}

func (e *admitEngine) recordedResumes() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]string(nil), e.resumes...)
}

func (e *admitEngine) List(context.Context) ([]engine.TaskInfo, error) { panic("not called") }
func (e *admitEngine) Get(context.Context, string) (engine.TaskInfo, error) {
	panic("not called")
}
func (e *admitEngine) Files(context.Context, string) ([]engine.FileEntry, error) {
	panic("not called")
}
func (e *admitEngine) Pause(context.Context, string) error { panic("not called") }
func (e *admitEngine) SetFiles(context.Context, string, []int, map[int]int) error {
	panic("not called")
}
func (e *admitEngine) SetLocation(context.Context, string, string) error { panic("not called") }
func (e *admitEngine) Rename(context.Context, string, string) error      { panic("not called") }
func (e *admitEngine) SetCategory(context.Context, string, string) error { panic("not called") }
func (e *admitEngine) SetRateLimits(context.Context, string, *int64, *int64) error {
	panic("not called")
}
func (e *admitEngine) SetShareLimits(context.Context, string, *float64, *int64) error {
	panic("not called")
}
func (e *admitEngine) Events(context.Context) (<-chan engine.TaskEvent, error) {
	panic("not called")
}

// admitEnv is one real migrated store plus the two recording stand-ins:
// the pass's writes go through the real TaskStore — CountActive,
// SelectQueuedCandidates, SetErrorCode, Transition and SetEngineRef are
// the pass's collaborators, and a fake store would test nothing but the
// fake.
type admitEnv struct {
	tasks *store.TaskStore
	aria2 *admitEngine
	qbt   *admitEngine
	admit *engine.Admitter
}

func newAdmitEnv(t *testing.T) *admitEnv {
	t.Helper()

	root := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(root, "dl-tool.db"), filepath.Join(root, "backups"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	aria2 := newAdmitEngine(engine.NameAria2)
	qbt := newAdmitEngine(engine.NameQBittorrent)
	registry := engine.NewRegistry()
	registry.Register(aria2)
	registry.Register(qbt)

	tasks := store.NewTaskStore(db)

	return &admitEnv{
		tasks: tasks,
		aria2: aria2,
		qbt:   qbt,
		admit: engine.NewAdmitter(registry, tasks, time.Second),
	}
}

// seedTask inserts one task and returns its id. mutate runs before the
// insert, so a test can set the state, the handle or a stale error code.
func (e *admitEnv) seedTask(t *testing.T, engineName, name string, mutate func(*store.Task)) string {
	t.Helper()

	source := "https://example.org/" + name + ".iso"
	task := store.Task{
		Engine:      engineName,
		SourceKind:  "http",
		SourceURI:   &source,
		Name:        name,
		State:       "queued",
		Destination: "/data",
	}
	if mutate != nil {
		mutate(&task)
	}

	created, err := e.tasks.Create(t.Context(), task)
	if err != nil {
		t.Fatalf("seed task %s: %v", name, err)
	}

	return created.ID
}

// nextAddedAt forces a later added_at than every seed so far: creation
// order is process order (FR-095), and the ULID id tiebreak of one shared
// millisecond is random, so a test that asserts order must keep the
// milliseconds apart.
func nextAddedAt() {
	time.Sleep(2 * time.Millisecond)
}

// taskState reads one task's state straight from the store.
func (e *admitEnv) taskState(t *testing.T, id string) string {
	t.Helper()

	task, err := e.tasks.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("read task %s: %v", id, err)
	}

	return task.State
}

// taskErrorCode reads one task's error_code, "" when none is stored.
func (e *admitEnv) taskErrorCode(t *testing.T, id string) string {
	t.Helper()

	task, err := e.tasks.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("read task %s: %v", id, err)
	}
	if task.ErrorCode == nil {
		return ""
	}

	return *task.ErrorCode
}

// TestPassRespectsTotalAndPerEngine releases six queued tasks across two
// engines under max_active_total=2 and max_active_per_engine=1: exactly
// two start, one per engine, and the other four stay queued carrying
// concurrency_limit (docs/03-architecture.md section 6.4).
func TestPassRespectsTotalAndPerEngine(t *testing.T) {
	env := newAdmitEnv(t)

	var ids []string
	for i, name := range []string{"a1", "q1", "a2", "q2", "a3", "q3"} {
		engineName := engine.NameQBittorrent
		if i%2 == 0 {
			engineName = engine.NameAria2
		}
		ids = append(ids, env.seedTask(t, engineName, name, nil))
	}

	limits := engine.Limits{MaxActiveTotal: 2, MaxActivePerEngine: 1}
	released, err := env.admit.Pass(t.Context(), limits)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}

	if len(released) != 2 {
		t.Fatalf("released = %v (%d ids), want exactly 2", released, len(released))
	}

	perEngine := map[string]int{}
	for _, id := range released {
		task, err := env.tasks.Get(t.Context(), id)
		if err != nil {
			t.Fatalf("read released task %s: %v", id, err)
		}
		if task.State != string(engine.StateDownloading) {
			t.Errorf("released task %s state = %q, want downloading", id, task.State)
		}
		if task.EngineRef == nil {
			t.Errorf("released task %s has no engine_ref, want the handle Add returned", id)
		}
		perEngine[task.Engine]++
	}
	if perEngine[engine.NameAria2] != 1 || perEngine[engine.NameQBittorrent] != 1 {
		t.Errorf("released per engine = %v, want one aria2 and one qbittorrent", perEngine)
	}
	if adds := env.aria2.recordedAdds(); len(adds) != 1 {
		t.Errorf("aria2 adds = %v, want exactly one", adds)
	}
	if adds := env.qbt.recordedAdds(); len(adds) != 1 {
		t.Errorf("qbittorrent adds = %v, want exactly one", adds)
	}

	releasedSet := map[string]bool{}
	for _, id := range released {
		releasedSet[id] = true
	}
	for _, id := range ids {
		if releasedSet[id] {
			continue
		}
		if state := env.taskState(t, id); state != string(engine.StateQueued) {
			t.Errorf("held task %s state = %q, want queued", id, state)
		}
		if code := env.taskErrorCode(t, id); code != engine.ErrorCodeConcurrencyLimit {
			t.Errorf("held task %s error_code = %q, want %q", id, code, engine.ErrorCodeConcurrencyLimit)
		}
	}
}

// The zero limits mean unlimited in that dimension: with both zero, the
// pass releases every queued task however many there are.
func TestPassZeroMeansUnlimited(t *testing.T) {
	env := newAdmitEnv(t)

	for _, name := range []string{"a1", "a2", "q1", "q2"} {
		engineName := engine.NameQBittorrent
		if name[0] == 'a' {
			engineName = engine.NameAria2
		}
		env.seedTask(t, engineName, name, nil)
	}

	released, err := env.admit.Pass(t.Context(), engine.Limits{})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(released) != 4 {
		t.Fatalf("released = %v (%d ids), want all 4 under unlimited limits", released, len(released))
	}
	for _, id := range released {
		if state := env.taskState(t, id); state != string(engine.StateDownloading) {
			t.Errorf("task %s state = %q, want downloading", id, state)
		}
	}
}

// The queue is creation-ordered (FR-095): with one total slot, the oldest
// task starts and every later task waits, whatever their engines.
func TestPassReleasesInCreationOrder(t *testing.T) {
	env := newAdmitEnv(t)

	oldest := env.seedTask(t, engine.NameAria2, "oldest", nil)
	nextAddedAt()
	env.seedTask(t, engine.NameQBittorrent, "middle", nil)
	nextAddedAt()
	env.seedTask(t, engine.NameAria2, "newest", nil)

	released, err := env.admit.Pass(t.Context(), engine.Limits{MaxActiveTotal: 1})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(released) != 1 || released[0] != oldest {
		t.Fatalf("released = %v, want exactly the oldest task %s", released, oldest)
	}
}

// TestSeedingIsNotCounted fills max_active_total with seeding tasks and
// asserts a queued download still starts: seeding is excluded from every
// count, so a full seed list cannot starve new downloads
// (docs/04-data-model.md section 4.7).
func TestSeedingIsNotCounted(t *testing.T) {
	env := newAdmitEnv(t)

	for _, name := range []string{"seed1", "seed2"} {
		env.seedTask(t, engine.NameAria2, name, func(task *store.Task) {
			task.State = string(engine.StateSeeding)
			task.EngineRef = ptr(name + "-gid")
		})
	}

	download := env.seedTask(t, engine.NameAria2, "download", nil)

	released, err := env.admit.Pass(t.Context(), engine.Limits{MaxActiveTotal: 2})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(released) != 1 || released[0] != download {
		t.Fatalf("released = %v, want exactly the queued download %s", released, download)
	}
	if state := env.taskState(t, download); state != string(engine.StateDownloading) {
		t.Errorf("download state = %q, want downloading", state)
	}
	if adds := env.aria2.recordedAdds(); len(adds) != 1 {
		t.Errorf("aria2 adds = %v, want exactly one", adds)
	}

	// The seed list itself is untouched: the pass never contacts an engine
	// about a task it did not admit.
	if resumes := env.aria2.recordedResumes(); len(resumes) != 0 {
		t.Errorf("aria2 resumes = %v, want none", resumes)
	}
}

// TestHeldTaskCarriesConcurrencyLimit queues three tasks under
// max_active_total=1 — every one of them already stamped by an earlier
// holding pass: exactly one starts, its stale error_code is cleared, and
// the rest stay queued carrying concurrency_limit
// (docs/05-api-contract.md section 5.11).
func TestHeldTaskCarriesConcurrencyLimit(t *testing.T) {
	env := newAdmitEnv(t)

	var ids []string
	for _, name := range []string{"one", "two", "three"} {
		ids = append(ids, env.seedTask(t, engine.NameAria2, name, func(task *store.Task) {
			task.ErrorCode = ptr(engine.ErrorCodeConcurrencyLimit)
			task.ErrorMessage = ptr("1 of 1 slots in use")
		}))
	}

	released, err := env.admit.Pass(t.Context(), engine.Limits{MaxActiveTotal: 1})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(released) != 1 {
		t.Fatalf("released = %v (%d ids), want exactly one", released, len(released))
	}

	if code := env.taskErrorCode(t, released[0]); code != "" {
		t.Errorf("released task error_code = %q, want it cleared", code)
	}

	releasedSet := map[string]bool{}
	for _, id := range released {
		releasedSet[id] = true
	}
	for _, id := range ids {
		if releasedSet[id] {
			continue
		}
		if state := env.taskState(t, id); state != string(engine.StateQueued) {
			t.Errorf("held task %s state = %q, want queued", id, state)
		}
		if code := env.taskErrorCode(t, id); code != engine.ErrorCodeConcurrencyLimit {
			t.Errorf("held task %s error_code = %q, want %q", id, code, engine.ErrorCodeConcurrencyLimit)
		}
	}
}

// A queued task that already holds a handle is resumed, never re-added —
// Engine.Add is the first hand-off's, Engine.Resume every later one
// (docs/03-architecture.md section 6.4).
func TestPassResumesHandleHolder(t *testing.T) {
	env := newAdmitEnv(t)

	ref := "old-handle"
	id := env.seedTask(t, engine.NameAria2, "paused-mid-transfer", func(task *store.Task) {
		task.EngineRef = &ref
	})

	released, err := env.admit.Pass(t.Context(), engine.Limits{MaxActiveTotal: 5})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(released) != 1 || released[0] != id {
		t.Fatalf("released = %v, want exactly %s", released, id)
	}

	if resumes := env.aria2.recordedResumes(); len(resumes) != 1 || resumes[0] != engine.NameAria2+":"+ref {
		t.Errorf("aria2 resumes = %v, want exactly the stored handle", resumes)
	}
	if adds := env.aria2.recordedAdds(); len(adds) != 0 {
		t.Errorf("aria2 adds = %v, want none for a task that holds a handle", adds)
	}
	if state := env.taskState(t, id); state != string(engine.StateDownloading) {
		t.Errorf("state = %q, want downloading", state)
	}

	// The handle is unchanged: a resume releases, it does not re-register.
	task, err := env.tasks.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	if task.EngineRef == nil || *task.EngineRef != ref {
		t.Errorf("engine_ref = %v, want it unchanged at %q", task.EngineRef, ref)
	}
}

// A handle the engine no longer knows — an aria2 daemon restart — falls
// back to Add with resume semantics, the self-healing the reconciler
// applies to a handle that vanished mid-transfer.
func TestPassReAddsVanishedHandle(t *testing.T) {
	env := newAdmitEnv(t)
	env.aria2.resumeErr = engine.ErrNotFound

	ref := "vanished-handle"
	id := env.seedTask(t, engine.NameAria2, "orphaned", func(task *store.Task) {
		task.EngineRef = &ref
	})

	released, err := env.admit.Pass(t.Context(), engine.Limits{MaxActiveTotal: 5})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(released) != 1 || released[0] != id {
		t.Fatalf("released = %v, want exactly %s", released, id)
	}

	if adds := env.aria2.recordedAdds(); len(adds) != 1 {
		t.Fatalf("aria2 adds = %v, want one re-add after the vanished handle", adds)
	}

	task, err := env.tasks.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	if task.EngineRef == nil || *task.EngineRef == ref {
		t.Errorf("engine_ref = %v, want the fresh handle Add returned, not %q", task.EngineRef, ref)
	}
	if state := env.taskState(t, id); state != string(engine.StateDownloading) {
		t.Errorf("state = %q, want downloading", state)
	}
}

// An engine refusal on a live context is the engine.rejected moment of
// the task_events vocabulary: the task moves to error carrying the
// refusal, and error -> queued remains the retry path.
func TestEngineRefusalErrorsTask(t *testing.T) {
	env := newAdmitEnv(t)
	env.aria2.addErr = errors.New("unsupported URI scheme")

	id := env.seedTask(t, engine.NameAria2, "refused", nil)

	released, err := env.admit.Pass(t.Context(), engine.Limits{MaxActiveTotal: 5})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(released) != 0 {
		t.Fatalf("released = %v, want none", released)
	}
	if state := env.taskState(t, id); state != string(engine.StateError) {
		t.Fatalf("state = %q, want error", state)
	}

	events, _, _, err := env.tasks.ListEvents(t.Context(), id, 10, "")
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var codes []string
	for _, event := range events {
		codes = append(codes, event.Code)
	}
	if len(codes) == 0 || codes[0] != store.CodeEngineRejected {
		t.Errorf("event codes = %v, want the newest to be %q", codes, store.CodeEngineRejected)
	}
}

// An unreachable engine is an outage, not a refusal: the candidate stays
// queued with no error_code and the next tick retries — the reconciler's
// policy for an engine that is down. A task held by the limit on the
// previous tick also loses its stale stamp: the engine's absence, not
// the limit, is what holds it now.
func TestUnregisteredEngineStaysQueued(t *testing.T) {
	env := newAdmitEnv(t)

	id := env.seedTask(t, engine.NameYtDlp, "not-configured", func(task *store.Task) {
		task.ErrorCode = ptr(engine.ErrorCodeConcurrencyLimit)
		task.ErrorMessage = ptr("1 of 1 slots in use")
	})

	released, err := env.admit.Pass(t.Context(), engine.Limits{MaxActiveTotal: 5})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(released) != 0 {
		t.Fatalf("released = %v, want none", released)
	}
	if state := env.taskState(t, id); state != string(engine.StateQueued) {
		t.Errorf("state = %q, want queued", state)
	}
	if code := env.taskErrorCode(t, id); code != "" {
		t.Errorf("error_code = %q, want it cleared: the engine's absence is not a concurrency hold", code)
	}
}

// A freshly admitted task released again after a pause resumes through
// the stored handle, and the handle composition round-trips exactly once:
// Add returns the namespaced id, the row stores the bare ref, Resume
// receives the namespaced form again — never a double prefix.
func TestPassRoundTripsTheHandle(t *testing.T) {
	env := newAdmitEnv(t)

	id := env.seedTask(t, engine.NameAria2, "round-trip", nil)

	if _, err := env.admit.Pass(t.Context(), engine.Limits{MaxActiveTotal: 5}); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	// Back to queued the way a pause and a resume action would leave it,
	// keeping the handle the first release recorded.
	for _, next := range []string{string(engine.StatePaused), string(engine.StateQueued)} {
		if err := env.tasks.Transition(t.Context(), id, next, "test", "test step"); err != nil {
			t.Fatalf("transition to %s: %v", next, err)
		}
	}

	if _, err := env.admit.Pass(t.Context(), engine.Limits{MaxActiveTotal: 5}); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	task, err := env.tasks.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	if task.EngineRef == nil {
		t.Fatal("engine_ref is nil after the first release")
	}

	wantResume := engine.NameAria2 + ":" + *task.EngineRef
	if resumes := env.aria2.recordedResumes(); len(resumes) != 1 || resumes[0] != wantResume {
		t.Errorf("aria2 resumes = %v, want exactly [%s] with no double prefix", resumes, wantResume)
	}
	if adds := env.aria2.recordedAdds(); len(adds) != 1 {
		t.Errorf("aria2 adds = %v, want the one Add of the first release only", adds)
	}
	if state := env.taskState(t, id); state != string(engine.StateDownloading) {
		t.Errorf("state = %q, want downloading", state)
	}
}

// flakyClearStore fails every stamp-clearing SetErrorCode (empty code),
// so a test can pin that a failed clear after a successful release never
// routes the healthy downloading task into releaseFailed's refusal
// branch.
type flakyClearStore struct {
	engine.AdmissionStore
}

func (s flakyClearStore) SetErrorCode(ctx context.Context, id, errorCode, message string) error {
	if errorCode == "" {
		return errors.New("injected: stamp clear failed")
	}

	return s.AdmissionStore.SetErrorCode(ctx, id, errorCode, message)
}

// A failed stamp clear after the release's transition is a warning, not
// an error: the task stays downloading with its stale stamp until the
// next pass clears it — it is never flipped to error as a supposed
// refusal.
func TestFailedStampClearKeepsTheRelease(t *testing.T) {
	env := newAdmitEnv(t)

	registry := engine.NewRegistry()
	registry.Register(env.aria2)
	admit := engine.NewAdmitter(registry, flakyClearStore{AdmissionStore: env.tasks}, time.Second)

	id := env.seedTask(t, engine.NameAria2, "held-then-freed", func(task *store.Task) {
		task.ErrorCode = ptr(engine.ErrorCodeConcurrencyLimit)
		task.ErrorMessage = ptr("1 of 1 slots in use")
	})

	released, err := admit.Pass(t.Context(), engine.Limits{MaxActiveTotal: 1})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(released) != 1 || released[0] != id {
		t.Fatalf("released = %v, want exactly [%s]", released, id)
	}
	if state := env.taskState(t, id); state != string(engine.StateDownloading) {
		t.Fatalf("state = %q, want downloading — a failed stamp clear must not error the task", state)
	}
	if code := env.taskErrorCode(t, id); code != engine.ErrorCodeConcurrencyLimit {
		t.Errorf("error_code = %q, want the stale stamp the failed clear left behind", code)
	}
}

// Reserve consumes one unit of headroom for the named engine, total and
// per-engine — the arithmetic a resume batch relies on to stop promising
// one slot to two ids.
func TestActiveCountsReserve(t *testing.T) {
	counts := engine.ActiveCounts{Total: 1, ByEngine: map[string]int{engine.NameAria2: 1}}
	limits := engine.Limits{MaxActiveTotal: 5, MaxActivePerEngine: 2}

	if blocked, _ := limits.Blocked(counts, engine.NameAria2); blocked {
		t.Fatal("blocked before reserving, want headroom for one more")
	}

	counts.Reserve(engine.NameAria2)

	if blocked, message := limits.Blocked(counts, engine.NameAria2); !blocked {
		t.Fatal("not blocked after reserving the last slot")
	} else if message != "2 of 2 aria2 slots in use" {
		t.Errorf("message = %q, want the per-engine sentence", message)
	}
	if counts.Total != 2 {
		t.Errorf("total = %d, want 2", counts.Total)
	}

	// A zero-value snapshot has no map; Reserve must build one instead of
	// panicking, so a hand-built ActiveCounts is as safe as CountActive's.
	var zero engine.ActiveCounts
	zero.Reserve(engine.NameAria2)
	if zero.Total != 1 || zero.ByEngine[engine.NameAria2] != 1 {
		t.Errorf("zero-value reserve = %d/%v, want 1 and one aria2 slot", zero.Total, zero.ByEngine)
	}
}

// A non-positive tick is a composition bug: it must fail at construction,
// naming the value, rather than as a generic NewTicker panic inside the
// loop goroutine.
func TestNewAdmitterRejectsNonPositiveTick(t *testing.T) {
	env := newAdmitEnv(t)
	for _, tick := range []time.Duration{0, -time.Second} {
		tick := tick
		deferredPanic(t, "admission tick", func() { engine.NewAdmitter(engine.NewRegistry(), env.tasks, tick) })
	}
}

// deferredPanic asserts that call panics with a message containing
// substr, recovering it so the caller's remaining iterations still run.
func deferredPanic(t *testing.T, substr string, call func()) {
	t.Helper()
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Error("call did not panic")

			return
		}
		if msg := fmt.Sprintf("%v", recovered); !strings.Contains(msg, substr) {
			t.Errorf("panic value %v does not mention %q", recovered, substr)
		}
	}()
	call()
}

// Run stops with its context and drives one pass per tick.
func TestAdmitterRunStopsWithContext(t *testing.T) {
	env := newAdmitEnv(t)
	admit := engine.NewAdmitter(
		engine.NewRegistry(),
		env.tasks,
		time.Millisecond,
	)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- admit.Run(ctx, func(context.Context) (engine.Limits, error) {
			return engine.Limits{}, nil
		})
	}()

	time.Sleep(5 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after its context was cancelled")
	}
}
