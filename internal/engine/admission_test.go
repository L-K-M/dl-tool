package engine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/fsx"
	"github.com/L-K-M/dl-tool/internal/store"
)

// seedDestinationRoot is the destination every seedTask carries unless a
// test overrides it: a path that always exists, so the filesystem identity
// needs no ancestor climb. The floor policy below pins its floor to 0 so
// the space gate never holds a test task on the host's own free space.
var seedDestinationRoot = os.TempDir()

// unlimitedFloor wraps the limits in the policy the pre-T099 tests run
// under: the seed root's floor is 0, so only the concurrency limits can
// hold a task and the assertions of T098 keep their meaning.
func unlimitedFloor(l engine.Limits) engine.Policy {
	return engine.Policy{Limits: l, Roots: []string{seedDestinationRoot}, MinFree: map[string]int64{seedDestinationRoot: 0}}
}

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
	pauses    []string // the engine task ids every Pause saw
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

func (e *admitEngine) Pause(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pauses = append(e.pauses, id)

	return nil
}

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

func (e *admitEngine) recordedPauses() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]string(nil), e.pauses...)
}

func (e *admitEngine) List(context.Context) ([]engine.TaskInfo, error) { panic("not called") }
func (e *admitEngine) Get(context.Context, string) (engine.TaskInfo, error) {
	panic("not called")
}
func (e *admitEngine) Files(context.Context, string) ([]engine.FileEntry, error) {
	panic("not called")
}
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
		admit: engine.NewAdmitter(registry, tasks, time.Second, nil),
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
		Destination: seedDestinationRoot,
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
	released, err := env.admit.Pass(t.Context(), unlimitedFloor(limits))
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

	released, err := env.admit.Pass(t.Context(), unlimitedFloor(engine.Limits{}))
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

	released, err := env.admit.Pass(t.Context(), unlimitedFloor(engine.Limits{MaxActiveTotal: 1}))
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

	released, err := env.admit.Pass(t.Context(), unlimitedFloor(engine.Limits{MaxActiveTotal: 2}))
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

	released, err := env.admit.Pass(t.Context(), unlimitedFloor(engine.Limits{MaxActiveTotal: 1}))
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

	released, err := env.admit.Pass(t.Context(), unlimitedFloor(engine.Limits{MaxActiveTotal: 5}))
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

	released, err := env.admit.Pass(t.Context(), unlimitedFloor(engine.Limits{MaxActiveTotal: 5}))
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

	released, err := env.admit.Pass(t.Context(), unlimitedFloor(engine.Limits{MaxActiveTotal: 5}))
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

	released, err := env.admit.Pass(t.Context(), unlimitedFloor(engine.Limits{MaxActiveTotal: 5}))
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

	if _, err := env.admit.Pass(t.Context(), unlimitedFloor(engine.Limits{MaxActiveTotal: 5})); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	// Back to queued the way a pause and a resume action would leave it,
	// keeping the handle the first release recorded.
	for _, next := range []string{string(engine.StatePaused), string(engine.StateQueued)} {
		if err := env.tasks.Transition(t.Context(), id, next, "test", "test step"); err != nil {
			t.Fatalf("transition to %s: %v", next, err)
		}
	}

	if _, err := env.admit.Pass(t.Context(), unlimitedFloor(engine.Limits{MaxActiveTotal: 5})); err != nil {
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

// flakyClearStore fails every hold-code clear (ClearHoldCode), so a test
// can pin that a failed clear after a successful release never routes the
// healthy downloading task into releaseFailed's refusal branch.
type flakyClearStore struct {
	engine.AdmissionStore
}

func (s flakyClearStore) ClearHoldCode(ctx context.Context, id string) error {
	return errors.New("injected: stamp clear failed")
}

// A failed stamp clear after the release's transition is a warning, not
// an error: the task stays downloading with its stale stamp until the
// next pass clears it — it is never flipped to error as a supposed
// refusal.
func TestFailedStampClearKeepsTheRelease(t *testing.T) {
	env := newAdmitEnv(t)

	registry := engine.NewRegistry()
	registry.Register(env.aria2)
	admit := engine.NewAdmitter(registry, flakyClearStore{AdmissionStore: env.tasks}, time.Second, nil)

	id := env.seedTask(t, engine.NameAria2, "held-then-freed", func(task *store.Task) {
		task.ErrorCode = ptr(engine.ErrorCodeConcurrencyLimit)
		task.ErrorMessage = ptr("1 of 1 slots in use")
	})

	released, err := admit.Pass(t.Context(), unlimitedFloor(engine.Limits{MaxActiveTotal: 1}))
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
		deferredPanic(t, "admission tick", func() { engine.NewAdmitter(engine.NewRegistry(), env.tasks, tick, nil) })
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
		nil,
	)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- admit.Run(ctx, func(context.Context) (engine.Policy, error) {
			return engine.Policy{}, nil
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

// mib is the byte unit the disk-space tests commit and request head-room
// in: large enough that a statfs tick between the test's read and the
// pass's read cannot flip an admission, small enough for any temp dir.
const mib = int64(1 << 20)

// floorLeaving returns a min_free_space floor that leaves exactly headRoom
// bytes of unreserved space on root's filesystem, computed from the live
// statfs answer — a "small root" without needing a privileged tmpfs. A
// negative headRoom (as TestENOSPCPausesAndKeepsData passes) instead sets
// the floor above the live free bytes — an unconditionally full disk. A
// negative floor means the temp filesystem is smaller than the test's
// commitment; that is an environment failure, not a skip.
func floorLeaving(t *testing.T, root string, headRoom int64) int64 {
	t.Helper()

	space, err := fsx.FreeSpace(root)
	if err != nil {
		t.Fatalf("read free space of %s: %v", root, err)
	}

	floor := space.FreeBytes - headRoom
	if floor < 0 {
		t.Fatalf("temp filesystem has only %d free bytes; test needs %d of head-room", space.FreeBytes, headRoom)
	}

	return floor
}

// policyOver returns the pass policy for root with the given floor and
// unlimited concurrency, so only the space gate can hold a task.
func policyOver(root string, floor int64) engine.Policy {
	return engine.Policy{Roots: []string{root}, MinFree: map[string]int64{root: floor}}
}

// TestThirdTaskStaysQueued is FR-047's scenario: two downloading tasks
// whose remaining bytes already commit the root, a third submitted task
// stays queued carrying disk_full instead of starting and failing, and it
// starts once the first two complete and their commitment lifts.
func TestThirdTaskStaysQueued(t *testing.T) {
	env := newAdmitEnv(t)
	root := t.TempDir()

	downloading := func(total, completed int64) func(*store.Task) {
		return func(task *store.Task) {
			task.State = string(engine.StateDownloading)
			task.Destination = root
			task.TotalBytes = &total
			task.CompletedBytes = completed
		}
	}

	first := env.seedTask(t, engine.NameAria2, "first", downloading(100*mib, 40*mib))
	nextAddedAt()
	second := env.seedTask(t, engine.NameAria2, "second", downloading(50*mib, 0))
	nextAddedAt()

	third := env.seedTask(t, engine.NameAria2, "third", func(task *store.Task) {
		task.Destination = root
		total := 10 * mib
		task.TotalBytes = &total
	})

	// The floor leaves the third task decisively short of what it needs
	// beside the first two's 110 MiB commitment — a gap free-space jitter
	// cannot flip.
	committed := 60*mib + 50*mib
	shortfall := 50 * mib
	holding := policyOver(root, floorLeaving(t, root, committed+10*mib-shortfall))

	released, err := env.admit.Pass(t.Context(), holding)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(released) != 0 {
		t.Fatalf("released = %v, want none: the filesystem is fully committed", released)
	}
	if state := env.taskState(t, third); state != string(engine.StateQueued) {
		t.Errorf("third task state = %q, want queued", state)
	}
	if code := env.taskErrorCode(t, third); code != engine.ErrorCodeDiskFull {
		t.Errorf("third task error_code = %q, want %q", code, engine.ErrorCodeDiskFull)
	}
	if adds := env.aria2.recordedAdds(); len(adds) != 0 {
		t.Errorf("aria2 adds = %v, want none while the disk holds the task", adds)
	}

	// The first two complete: their commitment lifts and the floor now
	// leaves room for the third.
	for _, id := range []string{first, second} {
		if err := env.tasks.Transition(t.Context(), id, string(engine.StateCompleted), "test", "completed"); err != nil {
			t.Fatalf("complete %s: %v", id, err)
		}
	}

	released, err = env.admit.Pass(t.Context(), holding)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(released) != 1 || released[0] != third {
		t.Fatalf("released = %v, want exactly the third task %s", released, third)
	}
	if state := env.taskState(t, third); state != string(engine.StateDownloading) {
		t.Errorf("third task state = %q, want downloading", state)
	}
	if code := env.taskErrorCode(t, third); code != "" {
		t.Errorf("third task error_code = %q, want the hold cleared on release", code)
	}
}

// TestENOSPCPausesAndKeepsData is FR-048: a write that fails with ENOSPC
// pauses the task carrying disk_full, unlinks nothing — the partial file
// is byte-for-byte unchanged — and the next admission pass resumes the
// same transfer once the filesystem admits again.
func TestENOSPCPausesAndKeepsData(t *testing.T) {
	env := newAdmitEnv(t)
	root := t.TempDir()

	partial := filepath.Join(root, "file.bin")
	want := []byte("partial download bytes, byte-for-byte precious")
	if err := os.WriteFile(partial, want, 0o600); err != nil {
		t.Fatalf("write partial file: %v", err)
	}

	ref := "gid001"
	id := env.seedTask(t, engine.NameAria2, "filling", func(task *store.Task) {
		task.State = string(engine.StateDownloading)
		task.EngineRef = &ref
		task.Destination = root
	})

	cause := fmt.Errorf("write %s: %w", partial, syscall.ENOSPC)
	if err := env.admit.PauseDiskFull(t.Context(), id, cause); err != nil {
		t.Fatalf("pause disk-full: %v", err)
	}

	if pauses := env.aria2.recordedPauses(); len(pauses) != 1 || pauses[0] != engine.NameAria2+":"+ref {
		t.Errorf("aria2 pauses = %v, want exactly the stored handle", pauses)
	}
	if state := env.taskState(t, id); state != string(engine.StatePaused) {
		t.Fatalf("state = %q, want paused", state)
	}
	if code := env.taskErrorCode(t, id); code != engine.ErrorCodeDiskFull {
		t.Fatalf("error_code = %q, want %q", code, engine.ErrorCodeDiskFull)
	}

	// Exactly one task.paused row explains the pause.
	events, _, _, err := env.tasks.ListEvents(t.Context(), id, 50, "")
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	pauses := 0
	for _, event := range events {
		if event.Code == store.CodeTaskPaused {
			pauses++
		}
	}
	if pauses != 1 {
		t.Errorf("task.paused events = %d, want exactly one", pauses)
	}

	// Nothing was unlinked or rewritten: the partial data survives intact.
	got, err := os.ReadFile(partial)
	if err != nil {
		t.Fatalf("partial file vanished: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("partial file changed on ENOSPC: %q", got)
	}

	// While the filesystem does not admit — free below its floor — the
	// pass leaves the task paused; the partial data is never restarted.
	// A floor 64 MiB above the free answer holds everything decisively.
	full := policyOver(root, floorLeaving(t, root, -64*mib))
	released, err := env.admit.Pass(t.Context(), full)
	if err != nil {
		t.Fatalf("pass on a full filesystem: %v", err)
	}
	if len(released) != 0 {
		t.Fatalf("released = %v, want none below the floor", released)
	}
	if state := env.taskState(t, id); state != string(engine.StatePaused) {
		t.Fatalf("state = %q, want still paused while the disk is full", state)
	}
	if code := env.taskErrorCode(t, id); code != engine.ErrorCodeDiskFull {
		t.Fatalf("error_code = %q, want %q retained while the disk is full", code, engine.ErrorCodeDiskFull)
	}

	// The holding pass wrote no second pause: a held candidate gets its
	// stamp refreshed at most, never another transition.
	events, _, _, err = env.tasks.ListEvents(t.Context(), id, 50, "")
	if err != nil {
		t.Fatalf("list events after the holding pass: %v", err)
	}
	pauses = 0
	for _, event := range events {
		if event.Code == store.CodeTaskPaused {
			pauses++
		}
	}
	if pauses != 1 {
		t.Errorf("task.paused events = %d after the holding pass, want still exactly one", pauses)
	}

	// Space returns: the pass resumes the stored handle — the same
	// transfer, never a second Add — and clears the hold.
	room := policyOver(root, 0)
	released, err = env.admit.Pass(t.Context(), room)
	if err != nil {
		t.Fatalf("pass with room: %v", err)
	}
	if len(released) != 1 || released[0] != id {
		t.Fatalf("released = %v, want exactly %s", released, id)
	}
	if resumes := env.aria2.recordedResumes(); len(resumes) != 1 || resumes[0] != engine.NameAria2+":"+ref {
		t.Errorf("aria2 resumes = %v, want exactly the stored handle", resumes)
	}
	if adds := env.aria2.recordedAdds(); len(adds) != 0 {
		t.Errorf("aria2 adds = %v, want none: the partial data is continued, not restarted", adds)
	}
	if state := env.taskState(t, id); state != string(engine.StateDownloading) {
		t.Errorf("state = %q, want downloading", state)
	}
	if code := env.taskErrorCode(t, id); code != "" {
		t.Errorf("error_code = %q, want the hold cleared on resume", code)
	}
}

// Two destinations on one mount share one reservation pool (FR-047): the
// bytes an active task committed on one destination hold a candidate on
// the other.
func TestTwoDestinationsShareOnePool(t *testing.T) {
	env := newAdmitEnv(t)
	root := t.TempDir()
	other := filepath.Join(root, "other")
	// The second destination exists on disk, so the scenario is honest
	// about pooling two real destinations rather than leaning on the
	// ancestor climb.
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatalf("mkdir second destination: %v", err)
	}

	env.seedTask(t, engine.NameAria2, "active", func(task *store.Task) {
		task.State = string(engine.StateDownloading)
		task.Destination = root
		total := 100 * mib
		task.TotalBytes = &total
	})

	candidate := env.seedTask(t, engine.NameAria2, "candidate", func(task *store.Task) {
		task.Destination = other
		total := 10 * mib
		task.TotalBytes = &total
	})

	// Fifty MiB short of the 110 MiB the two tasks need together: the
	// candidate is held by bytes committed on the *other* destination — a
	// decisive gap, so a small fluctuation of the live free-space answer
	// cannot flip the hold.
	holding := policyOver(root, floorLeaving(t, root, 110*mib-50*mib))
	released, err := env.admit.Pass(t.Context(), holding)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(released) != 0 {
		t.Fatalf("released = %v, want none: the shared pool is committed", released)
	}
	if state := env.taskState(t, candidate); state != string(engine.StateQueued) {
		t.Errorf("candidate state = %q, want queued", state)
	}
	if code := env.taskErrorCode(t, candidate); code != engine.ErrorCodeDiskFull {
		t.Errorf("candidate error_code = %q, want %q", code, engine.ErrorCodeDiskFull)
	}

	// Sixty-four MiB past the 110 MiB both need: the same pool now admits
	// the candidate — the hold was the shared commitment, not its own
	// destination's emptiness. The margin is as decisive on the admit side
	// as the hold-side gap, so concurrent consumers of the temp filesystem
	// cannot flip the release.
	fitting := policyOver(root, floorLeaving(t, root, 110*mib+64*mib))
	released, err = env.admit.Pass(t.Context(), fitting)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(released) != 1 || released[0] != candidate {
		t.Fatalf("released = %v, want exactly the candidate %s", released, candidate)
	}
	if state := env.taskState(t, candidate); state != string(engine.StateDownloading) {
		t.Errorf("candidate state = %q, want downloading once admitted", state)
	}
	if code := env.taskErrorCode(t, candidate); code != "" {
		t.Errorf("candidate error_code = %q, want the hold cleared on release", code)
	}
}

// One pass cannot over-commit a filesystem: the bytes a release just
// promised are spent from the in-memory reservation before the next
// candidate of the same pass is judged, exactly as a slot is.
func TestPassCommitsReleasedBytesInMemory(t *testing.T) {
	env := newAdmitEnv(t)
	root := t.TempDir()

	first := env.seedTask(t, engine.NameAria2, "first", func(task *store.Task) {
		task.Destination = root
		total := 60 * mib
		task.TotalBytes = &total
	})
	nextAddedAt()
	second := env.seedTask(t, engine.NameAria2, "second", func(task *store.Task) {
		task.Destination = root
		total := 60 * mib
		task.TotalBytes = &total
	})

	// Head-room for 100 MiB: the first task's 60 MiB fit, and its
	// commitment leaves only 40 MiB for the second.
	policy := policyOver(root, floorLeaving(t, root, 100*mib))

	released, err := env.admit.Pass(t.Context(), policy)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(released) != 1 || released[0] != first {
		t.Fatalf("released = %v, want exactly the older task %s", released, first)
	}
	if state := env.taskState(t, second); state != string(engine.StateQueued) {
		t.Errorf("second task state = %q, want queued: one pass must not over-commit", state)
	}
	if code := env.taskErrorCode(t, second); code != engine.ErrorCodeDiskFull {
		t.Errorf("second task error_code = %q, want %q", code, engine.ErrorCodeDiskFull)
	}
}

// A task whose total_bytes is NULL reserves nothing, whatever progress it
// already reports: its completed bytes must not enter the pool as a
// negative commitment that under-counts the destination's other active
// tasks (FR-047's unknown-size rule).
func TestUnknownTotalReservesNothing(t *testing.T) {
	env := newAdmitEnv(t)
	root := t.TempDir()

	known := env.seedTask(t, engine.NameAria2, "known", func(task *store.Task) {
		task.State = string(engine.StateDownloading)
		task.Destination = root
		total := 500 * mib
		task.TotalBytes = &total
	})
	env.seedTask(t, engine.NameAria2, "unknown", func(task *store.Task) {
		// Metadata unresolved, 100 MiB already on disk: TotalBytes stays
		// nil and the contribution must be 0 — not -100 MiB.
		task.State = string(engine.StateDownloading)
		task.Destination = root
		task.CompletedBytes = 100 * mib
	})
	env.seedTask(t, engine.NameAria2, "drift", func(task *store.Task) {
		// Engine accounting drift — completed reported past total — must
		// cancel none of the neighbours' reservations either.
		task.State = string(engine.StateDownloading)
		task.Destination = root
		total := int64(1000)
		task.TotalBytes = &total
		task.CompletedBytes = 1500
	})

	remaining, err := env.tasks.SumRemainingByDestination(t.Context())
	if err != nil {
		t.Fatalf("sum remaining by destination: %v", err)
	}
	if got := remaining[root]; got != 500*mib {
		t.Errorf("committed at %s = %d, want the known task's %d alone", root, got, 500*mib)
	}

	// And the pass sees the same honesty: head-room computed against the
	// 500 MiB commitment holds a 100 MiB candidate that a -100 MiB
	// under-count would wrongly admit.
	candidate := env.seedTask(t, engine.NameAria2, "candidate", func(task *store.Task) {
		task.Destination = root
		total := 100 * mib
		task.TotalBytes = &total
	})

	holding := policyOver(root, floorLeaving(t, root, 500*mib+50*mib))
	released, err := env.admit.Pass(t.Context(), holding)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(released) != 0 {
		t.Fatalf("released = %v, want none: an unknown total must not buy head-room", released)
	}
	if code := env.taskErrorCode(t, candidate); code != engine.ErrorCodeDiskFull {
		t.Errorf("candidate error_code = %q, want %q", code, engine.ErrorCodeDiskFull)
	}

	// The known task leaving the counted set lifts the whole commitment:
	// the candidate is admitted even beside the unknown-total task.
	if err := env.tasks.Transition(t.Context(), known, string(engine.StateSeeding), "test", "moved on"); err != nil {
		t.Fatalf("move the known task out of the counted set: %v", err)
	}

	released, err = env.admit.Pass(t.Context(), holding)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(released) != 1 || released[0] != candidate {
		t.Fatalf("released = %v, want exactly the candidate %s", released, candidate)
	}
	if state := env.taskState(t, candidate); state != string(engine.StateDownloading) {
		t.Errorf("candidate state = %q, want downloading once admitted", state)
	}
	if code := env.taskErrorCode(t, candidate); code != "" {
		t.Errorf("candidate error_code = %q, want the hold cleared on release", code)
	}
}

// An operator-paused task is not a candidate: the pass's paused intake is
// exactly the tasks carrying disk_full, so a task the user parked is never
// silently un-paused by an admission tick (the filter lives in the store's
// query; this pins it).
func TestOperatorPausedTaskIsNotACandidate(t *testing.T) {
	env := newAdmitEnv(t)

	parked := env.seedTask(t, engine.NameAria2, "user-paused", func(task *store.Task) {
		task.State = string(engine.StatePaused)
		task.EngineRef = ptr("user-handle")
	})

	// A disk_full pause, by contrast, is exactly what the pass re-examines.
	seeded := env.seedTask(t, engine.NameAria2, "disk-paused", func(task *store.Task) {
		task.State = string(engine.StatePaused)
		task.ErrorCode = ptr(engine.ErrorCodeDiskFull)
		task.ErrorMessage = ptr("no space left on device")
	})

	candidates, err := env.tasks.SelectQueuedCandidates(t.Context(), 0)
	if err != nil {
		t.Fatalf("select candidates: %v", err)
	}
	for _, cand := range candidates {
		if cand.ID == parked {
			t.Fatal("an operator-paused task reached the admission pass; only disk_full pauses may")
		}
	}

	found := false
	for _, cand := range candidates {
		if cand.ID == seeded {
			found = true
		}
	}
	if !found {
		t.Error("a disk_full-paused task did not reach the admission pass; it must be re-examined each tick")
	}
}

// flakyPauseStore fails every atomic pause landing (PauseWithCode), so a
// test can pin that a store failure inside PauseDiskFull leaves the row
// untouched — never half-paused, never paused without the disk_full code
// the pass selects on.
type flakyPauseStore struct {
	engine.AdmissionStore
}

func (s flakyPauseStore) PauseWithCode(ctx context.Context, id string, pause store.CodedPause) error {
	return errors.New("injected: pause write failed")
}

// A store failure inside PauseDiskFull is all-or-nothing: the atomic
// landing leaves the row downloading with no stamp — the state the next
// ENOSPC report revisits — and the retry completes the pause.
func TestPauseDiskFullFailureLeavesTheRowUntouched(t *testing.T) {
	env := newAdmitEnv(t)

	registry := engine.NewRegistry()
	registry.Register(env.aria2)
	flaky := engine.NewAdmitter(registry, flakyPauseStore{AdmissionStore: env.tasks}, time.Second, nil)

	ref := "gid002"
	id := env.seedTask(t, engine.NameAria2, "mid-write-failure", func(task *store.Task) {
		task.State = string(engine.StateDownloading)
		task.EngineRef = &ref
	})

	err := flaky.PauseDiskFull(t.Context(), id, errors.New("write: no space left on device"))
	if err == nil {
		t.Fatal("PauseDiskFull with a failing store returned nil, want the store error")
	}
	if state := env.taskState(t, id); state != string(engine.StateDownloading) {
		t.Fatalf("state = %q after the failed pause, want downloading: the atomic landing must leave nothing behind", state)
	}
	if code := env.taskErrorCode(t, id); code != "" {
		t.Fatalf("error_code = %q, want empty: the atomic landing wrote nothing", code)
	}

	// The store heals; the retry lands the whole pause at once.
	if err := env.admit.PauseDiskFull(t.Context(), id, errors.New("write: no space left on device")); err != nil {
		t.Fatalf("retry pause disk-full: %v", err)
	}
	if state := env.taskState(t, id); state != string(engine.StatePaused) {
		t.Fatalf("state = %q after the retry, want paused", state)
	}
	if code := env.taskErrorCode(t, id); code != engine.ErrorCodeDiskFull {
		t.Errorf("error_code = %q, want %q", code, engine.ErrorCodeDiskFull)
	}
}

// Only the counted active states may take a disk-full pause — they are
// the only ones whose write paths can observe ENOSPC. Everything else is
// refused untouched, and an already-paused task only refreshes its stamp
// without writing a second event.
func TestPauseDiskFullStateAllowList(t *testing.T) {
	active := map[string]bool{
		string(engine.StateDownloading): true,
		string(engine.StateChecking):    true,
		string(engine.StateExtracting):  true,
		string(engine.StateMoving):      true,
	}
	refused := []string{
		string(engine.StateQueued), string(engine.StateSeeding),
		string(engine.StateError), string(engine.StateCompleted), string(engine.StateRemoved),
	}

	states := slices.Clone(refused)
	for state := range active {
		states = append(states, state)
	}
	states = append(states, string(engine.StatePaused))
	sort.Strings(states)

	for _, state := range states {
		t.Run(state, func(t *testing.T) {
			env := newAdmitEnv(t)
			ref := "gid-" + state
			id := env.seedTask(t, engine.NameAria2, state, func(task *store.Task) {
				task.State = state
				task.EngineRef = &ref
				if state == string(engine.StatePaused) {
					// Only a row the guard itself parked carries the stamp; the
					// operator-pause shape (no code) is covered below the table.
					task.ErrorCode = ptr(engine.ErrorCodeDiskFull)
					task.ErrorMessage = ptr("no space left on device")
				}
			})

			// A task already parked by the guard only refreshes its stamp: the
			// state is unchanged and no second pause event is written.
			if state == string(engine.StatePaused) {
				if err := env.admit.PauseDiskFull(t.Context(), id, nil); err != nil {
					t.Fatalf("refresh a paused task's stamp: %v", err)
				}
				if got := env.taskState(t, id); got != string(engine.StatePaused) {
					t.Errorf("state = %q, want it to stay paused", got)
				}
				if code := env.taskErrorCode(t, id); code != engine.ErrorCodeDiskFull {
					t.Errorf("error_code = %q, want the refreshed %q", code, engine.ErrorCodeDiskFull)
				}
				events, _, _, err := env.tasks.ListEvents(t.Context(), id, 10, "")
				if err != nil {
					t.Fatalf("list events: %v", err)
				}
				for _, event := range events {
					if event.Code == store.CodeTaskPaused {
						t.Error("the paused refresh wrote a second pause event")
					}
				}
				return
			}

			err := env.admit.PauseDiskFull(t.Context(), id, nil)
			if active[state] {
				if err != nil {
					t.Fatalf("pause an active %s task: %v", state, err)
				}
				if got := env.taskState(t, id); got != string(engine.StatePaused) {
					t.Errorf("state = %q, want paused", got)
				}
				if code := env.taskErrorCode(t, id); code != engine.ErrorCodeDiskFull {
					t.Errorf("error_code = %q, want %q", code, engine.ErrorCodeDiskFull)
				}
				return
			}

			if err == nil {
				t.Fatalf("pausing a %s task returned nil, want a refusal", state)
			}
			if got := env.taskState(t, id); got != state {
				t.Errorf("state = %q after the refusal, want it untouched at %q", got, state)
			}
			if code := env.taskErrorCode(t, id); code != "" {
				t.Errorf("error_code = %q after the refusal, want none", code)
			}
		})
	}
}

// The release cleanup's hold-code clear is guarded on the row not being
// paused: a disk-full pause that lands between a release's transition and
// its clear must keep its stamp, or the pass would never select the row
// again (FR-048) — the clear loses the race on purpose.
func TestClearHoldCodeNeverWipesAPausedStamp(t *testing.T) {
	env := newAdmitEnv(t)

	paused := env.seedTask(t, engine.NameAria2, "parked", func(task *store.Task) {
		task.State = string(engine.StatePaused)
		task.ErrorCode = ptr(engine.ErrorCodeDiskFull)
		task.ErrorMessage = ptr("no space left on device")
	})
	running := env.seedTask(t, engine.NameAria2, "running", func(task *store.Task) {
		task.State = string(engine.StateDownloading)
		task.ErrorCode = ptr(engine.ErrorCodeConcurrencyLimit)
		task.ErrorMessage = ptr("1 of 1 slots in use")
	})

	if err := env.tasks.ClearHoldCode(t.Context(), paused); err != nil {
		t.Fatalf("clear hold code of a paused task: %v", err)
	}
	if code := env.taskErrorCode(t, paused); code != engine.ErrorCodeDiskFull {
		t.Errorf("paused task error_code = %q, want the disk_full stamp the guard kept", code)
	}

	// A row the release owns — downloading — still clears.
	if err := env.tasks.ClearHoldCode(t.Context(), running); err != nil {
		t.Fatalf("clear hold code of a running task: %v", err)
	}
	if code := env.taskErrorCode(t, running); code != "" {
		t.Errorf("running task error_code = %q, want it cleared", code)
	}
}

// The paused disk_full candidates share the queued candidates' single
// ordering: an older parked task is walked before a newer queued one, so
// sustained queuing cannot starve the task whose partial data is already
// on disk.
func TestParkedTaskKeepsItsPlaceInTheOrder(t *testing.T) {
	env := newAdmitEnv(t)
	root := t.TempDir()

	parked := env.seedTask(t, engine.NameAria2, "older-parked", func(task *store.Task) {
		task.State = string(engine.StatePaused)
		task.ErrorCode = ptr(engine.ErrorCodeDiskFull)
		task.ErrorMessage = ptr("no space left on device")
		task.Destination = root
	})
	nextAddedAt()
	env.seedTask(t, engine.NameAria2, "newer-queued", func(task *store.Task) {
		task.Destination = root
	})

	candidates, err := env.tasks.SelectQueuedCandidates(t.Context(), 0)
	if err != nil {
		t.Fatalf("select candidates: %v", err)
	}
	if len(candidates) != 2 || candidates[0].ID != parked {
		var order []string
		for _, cand := range candidates {
			order = append(order, cand.ID)
		}
		t.Fatalf("candidate order = %v, want the older parked task %s first", order, parked)
	}
}

// An operator-paused task — paused without disk_full — must never gain the
// stamp: the admission pass selects paused rows by that code, so a stamp
// gained here would let a later tick silently un-pause what the user
// parked.
func TestPauseDiskFullRefusesAnOperatorPause(t *testing.T) {
	env := newAdmitEnv(t)

	id := env.seedTask(t, engine.NameAria2, "user-parked", func(task *store.Task) {
		task.State = string(engine.StatePaused)
		ref := "user-handle"
		task.EngineRef = &ref
	})

	if err := env.admit.PauseDiskFull(t.Context(), id, nil); err == nil {
		t.Fatal("pausing an operator-paused task returned nil, want a refusal")
	}
	if got := env.taskState(t, id); got != string(engine.StatePaused) {
		t.Errorf("state = %q, want it to stay paused", got)
	}
	if code := env.taskErrorCode(t, id); code != "" {
		t.Errorf("error_code = %q, want none: an operator pause must not gain the stamp", code)
	}
}
