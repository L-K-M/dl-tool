package jobs

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/store"
)

// scheduleRateCall records one Engine.SetRateLimits invocation.
type scheduleRateCall struct {
	id   string
	down int64
	up   int64
}

// scheduleFakeEngine is the daemon double the schedule tests drive: it
// records the global-limit fan-outs and the per-transfer pauses the
// governor issues. The embedded nil engine.Engine leaves every method
// the scheduler must not call a panic.
type scheduleFakeEngine struct {
	engine.Engine
	name      string
	mu        sync.Mutex
	rateCalls []scheduleRateCall
	pauses    []string
}

func (e *scheduleFakeEngine) Name() string { return e.name }

func (e *scheduleFakeEngine) SetRateLimits(_ context.Context, id string, down, up *int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rateCalls = append(e.rateCalls, scheduleRateCall{id: id, down: *down, up: *up})
	return nil
}

func (e *scheduleFakeEngine) Pause(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pauses = append(e.pauses, id)
	return nil
}

func (e *scheduleFakeEngine) rateCallCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.rateCalls)
}

func (e *scheduleFakeEngine) pauseList() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.pauses)
}

// scheduleFixture bundles the collaborators a schedule tick needs: the
// real store over a throwaway database, a governor over fake engines
// and the scheduler the tick runs through.
type scheduleFixture struct {
	tasks     *store.TaskStore
	settings  *store.SettingsStore
	governor  *engine.Governor
	scheduler *Scheduler
	engines   map[string]*scheduleFakeEngine
}

func newScheduleFixture(t *testing.T, engineNames ...string) *scheduleFixture {
	t.Helper()

	db := newTestDB(t)
	reg := engine.NewRegistry()
	engines := make(map[string]*scheduleFakeEngine, len(engineNames))
	for _, name := range engineNames {
		e := &scheduleFakeEngine{name: name}
		reg.Register(e)
		engines[name] = e
	}

	tasks := store.NewTaskStore(db)
	settings := store.NewSettingsStore(db)
	governor := engine.NewGovernor(reg, settings).WithTasks(tasks)
	scheduler := NewScheduler(db, slog.New(slog.NewTextHandler(io.Discard, nil))).WithGovernor(governor)

	return &scheduleFixture{
		tasks: tasks, settings: settings,
		governor: governor, scheduler: scheduler, engines: engines,
	}
}

// writeGrid replaces the whole schedule: every cell default except the
// overrides, indexed day*24+hour with Monday as day 0.
func (f *scheduleFixture) writeGrid(t *testing.T, enabled bool, overrides map[int]store.ScheduleMode) {
	t.Helper()

	var cells [168]store.ScheduleMode
	for i := range cells {
		cells[i] = store.ScheduleDefault
	}
	for idx, mode := range overrides {
		cells[idx] = mode
	}
	require.NoError(t, f.settings.ReplaceSchedule(t.Context(), enabled, cells))
}

// addTask inserts one task row in the given state; an empty ref leaves
// engine_ref NULL — a task the admission pass never handed to an engine.
func (f *scheduleFixture) addTask(t *testing.T, engineName, state, ref string) store.Task {
	t.Helper()

	var engineRef *string
	if ref != "" {
		engineRef = &ref
	}
	task, err := f.tasks.Create(t.Context(), store.Task{
		Engine: engineName, EngineRef: engineRef, SourceKind: "http",
		Name: "task " + engineName + "/" + ref, Destination: "/data", State: state,
	})
	require.NoError(t, err)

	return task
}

func (f *scheduleFixture) taskState(t *testing.T, id string) string {
	t.Helper()

	task, err := f.tasks.Get(t.Context(), id)
	require.NoError(t, err)

	return task.State
}

func (f *scheduleFixture) eventCodes(t *testing.T, id string) []string {
	t.Helper()

	events, _, _, err := f.tasks.ListEvents(t.Context(), id, 100, "")
	require.NoError(t, err)

	codes := make([]string, 0, len(events))
	for _, event := range events {
		codes = append(codes, event.Code)
	}

	return codes
}

// scheduleTime renders a fixed local instant inside one grid cell:
// 2026-09-21 is a Monday — day 0 — so the hour alone selects the cell.
func scheduleTime(hour, minute int) time.Time {
	return time.Date(2026, 9, 21, hour, minute, 0, 0, time.Local)
}

func TestNoDownloadPausesAndResumesSameSet(t *testing.T) {
	f := newScheduleFixture(t, engine.NameAria2, engine.NameQBittorrent)
	ctx := t.Context()

	// Cell 9 is the default hour, cell 10 the No Download hour.
	f.writeGrid(t, true, map[int]store.ScheduleMode{10: store.ScheduleNoDownload})

	downloading := f.addTask(t, engine.NameAria2, "downloading", "gid-one")
	checking := f.addTask(t, engine.NameQBittorrent, "checking", "hash-two")
	queuedHeld := f.addTask(t, engine.NameAria2, "queued", "gid-three")
	queuedNew := f.addTask(t, engine.NameAria2, "queued", "")
	seeding := f.addTask(t, engine.NameQBittorrent, "seeding", "hash-five")

	// The 1-cell tick: the default pair fans out, nothing parks.
	require.NoError(t, f.scheduler.EvaluateSchedule(ctx, scheduleTime(9, 0)))
	require.Equal(t, engine.ModeDefault, f.governor.Mode())
	for _, e := range f.engines {
		require.Equal(t,
			[]scheduleRateCall{{id: "", down: 0, up: 0}},
			e.rateCalls, "default tick must fan out the stored pair, never a near-zero rate",
		)
	}

	// The 0-cell tick: every running task pauses engine-side and parks;
	// the queued rows park without an engine call — the handle-less one
	// has nothing to pause and the held one's transfer is already
	// stopped — and the seeding task is never touched.
	require.NoError(t, f.scheduler.EvaluateSchedule(ctx, scheduleTime(10, 0)))
	require.Equal(t, engine.ModeNoDownload, f.governor.Mode())
	require.ElementsMatch(t,
		[]string{"aria2:gid-one", "qbittorrent:hash-two"},
		slices.Concat(f.engines[engine.NameAria2].pauseList(), f.engines[engine.NameQBittorrent].pauseList()),
		"a 0 cell pauses the live transfers only; queued handles are already stopped",
	)

	for _, task := range []store.Task{downloading, checking, queuedHeld, queuedNew} {
		require.Equal(t, "paused", f.taskState(t, task.ID), "task %s must be parked", task.ID)
		require.Equal(t, store.CodeTaskSchedulePaused, f.eventCodes(t, task.ID)[0])
	}
	require.Equal(t, "seeding", f.taskState(t, seeding.ID))
	require.Empty(t, f.eventCodes(t, seeding.ID), "a seeding task is never schedule-parked")

	parked, err := f.tasks.ListScheduleParked(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t,
		[]string{downloading.ID, checking.ID, queuedHeld.ID, queuedNew.ID},
		parked,
	)

	// Back to the 1-cell: exactly the parked ids are requeued, each with
	// its task.schedule.resumed row.
	require.NoError(t, f.scheduler.EvaluateSchedule(ctx, scheduleTime(9, 0)))
	require.Equal(t, engine.ModeDefault, f.governor.Mode())

	for _, task := range []store.Task{downloading, checking, queuedHeld, queuedNew} {
		require.Equal(t, "queued", f.taskState(t, task.ID), "task %s must be released", task.ID)
		require.Equal(t, store.CodeTaskScheduleResumed, f.eventCodes(t, task.ID)[0])
	}
	parked, err = f.tasks.ListScheduleParked(ctx)
	require.NoError(t, err)
	require.Empty(t, parked, "the parked set must be empty after the release")
}

func TestUserPausedTaskNotResumed(t *testing.T) {
	f := newScheduleFixture(t, engine.NameAria2)
	ctx := t.Context()

	f.writeGrid(t, true, map[int]store.ScheduleMode{10: store.ScheduleNoDownload})

	userPaused := f.addTask(t, engine.NameAria2, "downloading", "gid-user")
	require.NoError(t, f.tasks.Transition(ctx, userPaused.ID, "paused", store.CodeTaskPaused, "paused by user request"))
	scheduleParked := f.addTask(t, engine.NameAria2, "downloading", "gid-sched")

	require.NoError(t, f.scheduler.EvaluateSchedule(ctx, scheduleTime(9, 0)))
	require.NoError(t, f.scheduler.EvaluateSchedule(ctx, scheduleTime(10, 0)))

	// The operator's row was already paused when the 0 cell landed: the
	// park scan never sees it, so no task.schedule.paused event lands on
	// it and it stays out of the parked set.
	require.Equal(t, store.CodeTaskPaused, f.eventCodes(t, userPaused.ID)[0])
	require.Equal(t, store.CodeTaskSchedulePaused, f.eventCodes(t, scheduleParked.ID)[0])

	require.NoError(t, f.scheduler.EvaluateSchedule(ctx, scheduleTime(9, 0)))

	require.Equal(t, "paused", f.taskState(t, userPaused.ID))
	require.Equal(t, store.CodeTaskPaused, f.eventCodes(t, userPaused.ID)[0],
		"the schedule must never resume a task the user paused",
	)
	require.Equal(t, "queued", f.taskState(t, scheduleParked.ID))
	require.Equal(t, store.CodeTaskScheduleResumed, f.eventCodes(t, scheduleParked.ID)[0])
}

func TestAlternativeReachesAria2(t *testing.T) {
	f := newScheduleFixture(t, engine.NameAria2, engine.NameQBittorrent)
	ctx := t.Context()

	f.writeGrid(t, true, map[int]store.ScheduleMode{11: store.ScheduleAlternative})
	require.NoError(t, f.settings.SetInt64(ctx, "download_rate_limit", 100))
	require.NoError(t, f.settings.SetInt64(ctx, "upload_rate_limit", 50))
	require.NoError(t, f.settings.SetInt64(ctx, "alt_download_rate_limit", 2048))
	require.NoError(t, f.settings.SetInt64(ctx, "alt_upload_rate_limit", 1024))

	require.NoError(t, f.scheduler.EvaluateSchedule(ctx, scheduleTime(9, 0)))
	require.NoError(t, f.scheduler.EvaluateSchedule(ctx, scheduleTime(11, 0)))
	require.Equal(t, engine.ModeAlternative, f.governor.Mode())

	// Alternative speed is a second global value through the same
	// SetRateLimits calls — the aria2 fake receives it exactly like the
	// qBittorrent one, through no toggleSpeedLimitsMode equivalent.
	for _, e := range f.engines {
		require.Equal(t,
			[]scheduleRateCall{{id: "", down: 100, up: 50}, {id: "", down: 2048, up: 1024}},
			e.rateCalls,
			"engine %s must receive the default pair then the alternative pair", e.name,
		)
	}
}

func TestApplyGlobalHonoursAlternativeCell(t *testing.T) {
	f := newScheduleFixture(t, engine.NameAria2)
	ctx := t.Context()

	f.writeGrid(t, true, map[int]store.ScheduleMode{11: store.ScheduleAlternative})
	require.NoError(t, f.settings.SetInt64(ctx, "alt_download_rate_limit", 2048))
	require.NoError(t, f.settings.SetInt64(ctx, "alt_upload_rate_limit", 1024))

	require.NoError(t, f.scheduler.EvaluateSchedule(ctx, scheduleTime(11, 0)))
	require.Equal(t, engine.ModeAlternative, f.governor.Mode())

	// A global-limit write inside the 2 cell must not push the global
	// pair — the engines run the pair the active cell names.
	require.NoError(t, f.governor.ApplyGlobal(ctx, engine.RateLimits{Down: 100, Up: 50}))

	aria2 := f.engines[engine.NameAria2]
	require.Equal(t,
		[]scheduleRateCall{{id: "", down: 2048, up: 1024}, {id: "", down: 2048, up: 1024}},
		aria2.rateCalls,
		"a global write inside the alternative cell re-applies the cell pair, not the global one",
	)
	require.Equal(t, engine.RateLimits{Down: 2048, Up: 1024}, f.governor.Current())
	require.Equal(t, engine.ModeAlternative, f.governor.Mode())
}

func TestTickWithinCellIsIdempotent(t *testing.T) {
	f := newScheduleFixture(t, engine.NameAria2)
	ctx := t.Context()

	f.writeGrid(t, true, map[int]store.ScheduleMode{10: store.ScheduleNoDownload})
	task := f.addTask(t, engine.NameAria2, "downloading", "gid-one")
	aria2 := f.engines[engine.NameAria2]

	// Two ticks inside the same 0 cell: the first parks, the second —
	// minutes later, same cell — changes nothing at the engines.
	require.NoError(t, f.scheduler.EvaluateSchedule(ctx, scheduleTime(10, 5)))
	require.NoError(t, f.scheduler.EvaluateSchedule(ctx, scheduleTime(10, 45)))

	require.Equal(t, []string{"aria2:gid-one"}, aria2.pauseList())
	require.Zero(t, aria2.rateCallCount(), "a 0 cell must never push a near-zero rate")
	require.Len(t, f.eventCodes(t, task.ID), 1, "the park must log exactly one event")

	// The same holds after a release: a second tick inside the 1 cell
	// repeats neither the fan-out nor the resume.
	require.NoError(t, f.scheduler.EvaluateSchedule(ctx, scheduleTime(9, 0)))
	rateCalls := aria2.rateCallCount()
	require.NoError(t, f.scheduler.EvaluateSchedule(ctx, scheduleTime(9, 30)))
	require.Equal(t, rateCalls, aria2.rateCallCount())
	require.Len(t, f.eventCodes(t, task.ID), 2)
}

func TestDisabledScheduleDoesNothing(t *testing.T) {
	f := newScheduleFixture(t, engine.NameAria2)
	ctx := t.Context()

	// No ReplaceSchedule ever ran: schedule_enabled keeps its documented
	// absent-means-false default. A task sits mid-download and the grid
	// is all default anyway — the tick must touch nothing.
	task := f.addTask(t, engine.NameAria2, "downloading", "gid-one")
	aria2 := f.engines[engine.NameAria2]

	require.NoError(t, f.scheduler.EvaluateSchedule(ctx, scheduleTime(10, 0)))

	require.Zero(t, aria2.rateCallCount())
	require.Empty(t, aria2.pauseList())
	require.Equal(t, "downloading", f.taskState(t, task.ID))
	require.Empty(t, f.eventCodes(t, task.ID))
	require.Equal(t, engine.Mode(""), f.governor.Mode(), "a disabled schedule applies no mode")
}

func TestAppendedEventKeepsParkedMembership(t *testing.T) {
	f := newScheduleFixture(t, engine.NameAria2)
	ctx := t.Context()

	f.writeGrid(t, true, map[int]store.ScheduleMode{10: store.ScheduleNoDownload})
	task := f.addTask(t, engine.NameAria2, "downloading", "gid-one")

	require.NoError(t, f.scheduler.EvaluateSchedule(ctx, scheduleTime(10, 0)))
	require.Equal(t, "paused", f.taskState(t, task.ID))

	// A reconcile-style info event appended after the park must not
	// eject the row: parked membership follows the newest pause/resume
	// event, never the newest event of any kind.
	require.NoError(t, f.tasks.AppendEvent(ctx, task.ID, "info", engine.CodeTaskReconciled,
		"reconciled while parked", nil))

	parked, err := f.tasks.ListScheduleParked(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{task.ID}, parked, "an appended info event must not eject a parked task")

	require.NoError(t, f.scheduler.EvaluateSchedule(ctx, scheduleTime(9, 0)))
	require.Equal(t, "queued", f.taskState(t, task.ID), "the parked task must still be released")
}

func TestStartAppliesCellImmediately(t *testing.T) {
	f := newScheduleFixture(t, engine.NameAria2)
	f.writeGrid(t, true, map[int]store.ScheduleMode{10: store.ScheduleNoDownload})
	task := f.addTask(t, engine.NameAria2, "downloading", "gid-one")
	aria2 := f.engines[engine.NameAria2]

	// The injected clock sits inside the 0 cell; the real clock does not
	// matter — Start must evaluate before the first minute tick.
	f.scheduler.now = func() time.Time { return scheduleTime(10, 30) }

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		f.scheduler.Start(ctx)
		close(done)
	}()

	require.Eventually(t, func() bool {
		row, err := f.tasks.Get(t.Context(), task.ID)
		// The park commits after the engine pause lands; wait for the
		// state, not the pause list, so the assertions below are stable.
		return err == nil && row.State == "paused"
	}, 2*time.Second, 10*time.Millisecond,
		"Start must apply the active cell without waiting for a minute boundary",
	)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not drain after ctx cancel")
	}

	require.Equal(t, []string{"aria2:gid-one"}, aria2.pauseList())
	require.Equal(t, "paused", f.taskState(t, task.ID))
	require.Equal(t, store.CodeTaskSchedulePaused, f.eventCodes(t, task.ID)[0])
}

func TestNoDownloadWithoutTaskStoreFailsClosed(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()

	reg := engine.NewRegistry()
	aria2 := &scheduleFakeEngine{name: engine.NameAria2}
	reg.Register(aria2)

	// The governor deliberately gets no WithTasks: a 0 cell must error
	// before any engine sees a pause — the parked ids could never be
	// resumed without the store.
	governor := engine.NewGovernor(reg, store.NewSettingsStore(db))
	scheduler := NewScheduler(db, slog.New(slog.NewTextHandler(io.Discard, nil))).WithGovernor(governor)

	settings := store.NewSettingsStore(db)
	var cells [168]store.ScheduleMode
	for i := range cells {
		cells[i] = store.ScheduleDefault
	}
	cells[10] = store.ScheduleNoDownload
	require.NoError(t, settings.ReplaceSchedule(ctx, true, cells))

	require.Error(t, scheduler.EvaluateSchedule(ctx, scheduleTime(10, 0)))
	require.Empty(t, aria2.pauseList(), "no engine may see a pause the parked set could not record")
	require.Equal(t, engine.Mode(""), governor.Mode(), "a failed cell is not recorded as applied")
}
