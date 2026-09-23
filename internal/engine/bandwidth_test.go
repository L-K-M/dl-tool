package engine_test

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/api"
	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/store"
)

// bandwidthCall records one Engine.SetRateLimits invocation verbatim —
// the id and both direction pointers — so a test can see the governor
// sent the global "" id and whether a direction was forwarded as nil.
type bandwidthCall struct {
	id   string
	down *int64
	up   *int64
}

// p64 lifts a literal into the *int64 shape SetRateLimits takes.
func p64(v int64) *int64 { return &v }

// bandwidthEngine is the fake daemon the governor drives. The embedded nil
// engine.Engine leaves every method the governor must not call a panic, so
// a regression that touches the engine beyond SetRateLimits fails loudly.
// setErr answers every call — ErrNotSupported for the skipped engine,
// ErrUnavailable for the unreachable one.
type bandwidthEngine struct {
	engine.Engine
	name   string
	setErr error
	calls  []bandwidthCall
}

func (e *bandwidthEngine) Name() string { return e.name }

func (e *bandwidthEngine) SetRateLimits(_ context.Context, id string, down, up *int64) error {
	if e.setErr != nil {
		return e.setErr
	}
	e.calls = append(e.calls, bandwidthCall{id: id, down: down, up: up})
	return nil
}

// bandwidthReadback adds the optional read-back surface the governor
// confirms through — the same GlobalLimits signature qbittorrent.Client
// already implements — returning the daemon truth the test configures.
type bandwidthReadback struct {
	*bandwidthEngine
	down, up int64
	err      error
}

func (e *bandwidthReadback) GlobalLimits(context.Context) (int64, int64, error) {
	return e.down, e.up, e.err
}

// blockingEngine is the black-holed daemon: SetRateLimits parks until the
// context ends and answers its error. A sequential fan-out would let it eat
// the whole deadline and starve every engine iterated after it.
type blockingEngine struct {
	engine.Engine
	name string
}

func (e *blockingEngine) Name() string { return e.name }

func (e *blockingEngine) SetRateLimits(ctx context.Context, _ string, _, _ *int64) error {
	<-ctx.Done()
	return ctx.Err()
}

// captureGovernorLogs swaps the default slog for one writing into the
// returned buffer, restored at cleanup.
func captureGovernorLogs(t *testing.T) *strings.Builder {
	t.Helper()
	logs := &strings.Builder{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

func TestApplyGlobalReachesEveryEngine(t *testing.T) {
	reg := engine.NewRegistry()
	aria2 := &bandwidthEngine{name: engine.NameAria2}
	qbittorrent := &bandwidthEngine{name: engine.NameQBittorrent}
	reg.Register(aria2)
	reg.Register(qbittorrent)

	gov := engine.NewGovernor(reg, nil)
	want := engine.RateLimits{Down: 1048576, Up: 524288}
	require.NoError(t, gov.ApplyGlobal(context.Background(), want))

	for _, e := range []*bandwidthEngine{aria2, qbittorrent} {
		require.Equal(t,
			[]bandwidthCall{{id: "", down: p64(1048576), up: p64(524288)}},
			e.calls,
			"engine %s must receive the global pair with the empty id", e.name,
		)
	}
	require.Equal(t, want, gov.Current(), "Current must report the limits just applied")
}

func TestZeroMeansUnlimited(t *testing.T) {
	reg := engine.NewRegistry()
	e := &bandwidthEngine{name: engine.NameAria2}
	reg.Register(e)

	gov := engine.NewGovernor(reg, nil)
	require.NoError(t, gov.ApplyGlobal(context.Background(), engine.RateLimits{Down: 0, Up: 0}))

	// 0 is pushed as the literal unlimited value, never dropped as unset.
	require.Equal(t, []bandwidthCall{{id: "", down: p64(0), up: p64(0)}}, e.calls)
}

func TestNotSupportedEngineSkipped(t *testing.T) {
	reg := engine.NewRegistry()
	unsupported := &bandwidthEngine{name: engine.NameAria2, setErr: engine.ErrNotSupported}
	ok := &bandwidthEngine{name: engine.NameQBittorrent}
	reg.Register(unsupported)
	reg.Register(ok)

	gov := engine.NewGovernor(reg, nil)
	require.NoError(t, gov.ApplyGlobal(context.Background(), engine.RateLimits{Down: 1048576, Up: 0}))

	require.Empty(t, unsupported.calls, "an ErrNotSupported engine mutates nothing")
	require.Equal(t,
		[]bandwidthCall{{id: "", down: p64(1048576), up: p64(0)}},
		ok.calls,
		"the capable engine still receives the value",
	)
	require.Equal(t, engine.RateLimits{Down: 1048576, Up: 0}, gov.Current(),
		"an ErrNotSupported skip still counts as success for Current",
	)
}

func TestEmptyRegistryCountsAsSuccess(t *testing.T) {
	gov := engine.NewGovernor(engine.NewRegistry(), nil)
	require.NoError(t, gov.ApplyGlobal(context.Background(), engine.RateLimits{Down: 1048576, Up: 0}))
	require.Equal(t, engine.RateLimits{Down: 1048576, Up: 0}, gov.Current(),
		"a fan-out over no engines has no rejection, so Current records it",
	)
}

func TestReadBackMismatchLogsOnly(t *testing.T) {
	reg := engine.NewRegistry()
	mismatch := &bandwidthReadback{
		bandwidthEngine: &bandwidthEngine{name: engine.NameQBittorrent},
		down:            2097152, // daemon kept a different value than the 1048576 sent
		up:              524288,
	}
	reg.Register(mismatch)

	logs := captureGovernorLogs(t)

	gov := engine.NewGovernor(reg, nil)
	require.NoError(t, gov.ApplyGlobal(context.Background(), engine.RateLimits{Down: 1048576, Up: 524288}))

	require.Equal(t, []bandwidthCall{{id: "", down: p64(1048576), up: p64(524288)}}, mismatch.calls)

	logged := logs.String()
	require.Contains(t, logged, "level=WARN")
	require.Contains(t, logged, "download rate limit read back different")
	require.Contains(t, logged, "sent_bps=1048576")
	require.Contains(t, logged, "read_bps=2097152")
	require.Contains(t, logged, "engine="+engine.NameQBittorrent)
	require.NotContains(t, logged, "upload rate limit read back different",
		"the direction the daemon did hold must not warn")
}

func TestUnreachableEngineJoinedError(t *testing.T) {
	reg := engine.NewRegistry()
	down := &bandwidthEngine{name: engine.NameAria2, setErr: engine.ErrUnavailable}
	up := &bandwidthEngine{name: engine.NameQBittorrent}
	reg.Register(down)
	reg.Register(up)

	gov := engine.NewGovernor(reg, nil)
	err := gov.ApplyGlobal(context.Background(), engine.RateLimits{Down: 1048576, Up: 0})

	require.Error(t, err)
	require.ErrorIs(t, err, engine.ErrUnavailable)
	require.Contains(t, err.Error(), engine.NameAria2,
		"the joined error must name the engine that failed")

	require.Equal(t,
		[]bandwidthCall{{id: "", down: p64(1048576), up: p64(0)}},
		up.calls,
		"one unreachable daemon must not block the other engine",
	)
	require.Equal(t, engine.RateLimits{}, gov.Current(),
		"a fan-out an engine refused is not recorded as applied",
	)
}

func TestHungEngineDoesNotStarveTheRest(t *testing.T) {
	reg := engine.NewRegistry()
	hung := &blockingEngine{name: engine.NameAria2}
	ok := &bandwidthEngine{name: engine.NameQBittorrent}
	reg.Register(hung)
	reg.Register(ok)

	gov := engine.NewGovernor(reg, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := gov.ApplyGlobal(ctx, engine.RateLimits{Down: 1048576, Up: 0})
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Contains(t, err.Error(), engine.NameAria2)

	require.Equal(t,
		[]bandwidthCall{{id: "", down: p64(1048576), up: p64(0)}},
		ok.calls,
		"an engine parked on the context must not starve the fan-out behind it",
	)
}

func TestLoadAndApplyPushesStoredLimits(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(t.Context(),
		filepath.Join(root, "dl-tool.db"), filepath.Join(root, "backups"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	reg := engine.NewRegistry()
	e := &bandwidthEngine{name: engine.NameAria2}
	reg.Register(e)

	settings := store.NewSettingsStore(db)
	gov := engine.NewGovernor(reg, settings)

	// Absent rows are the documented default: unlimited in both directions.
	require.NoError(t, gov.LoadAndApply(context.Background()))
	require.Equal(t, []bandwidthCall{{id: "", down: p64(0), up: p64(0)}}, e.calls)
	require.Equal(t, engine.RateLimits{Down: 0, Up: 0}, gov.Current())

	require.NoError(t, settings.SetInt64(context.Background(), "download_rate_limit", 2097152))
	require.NoError(t, settings.SetInt64(context.Background(), "upload_rate_limit", 524288))

	require.NoError(t, gov.LoadAndApply(context.Background()))
	require.Equal(t,
		[]bandwidthCall{{id: "", down: p64(0), up: p64(0)}, {id: "", down: p64(2097152), up: p64(524288)}},
		e.calls,
		"the stored pair must reach the engine verbatim, in bytes per second",
	)
	require.Equal(t, engine.RateLimits{Down: 2097152, Up: 524288}, gov.Current())
}

func TestMalformedStoredLimitErrorsRatherThanUnlimited(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(t.Context(),
		filepath.Join(root, "dl-tool.db"), filepath.Join(root, "backups"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	settings := store.NewSettingsStore(db)

	// The stored grammar is a bare JSON integer. Every other shape must
	// error — 0 means unlimited, so decoding a corrupted row to a guessed
	// number could silently drop a throttle the operator set.
	for _, raw := range []string{`null`, `"4"`, `4.0`, `4x`, ``} {
		_, err := db.ExecContext(t.Context(),
			`INSERT INTO settings (id, key, value_json, created_at, updated_at)
			 VALUES ('st_malformed', 'download_rate_limit', ?, 0, 0)`, raw)
		require.NoError(t, err)

		_, err = settings.GetInt64(t.Context(), "download_rate_limit", 7)
		require.Errorf(t, err, "malformed value_json %q must error, never decode to a guess", raw)

		reg := engine.NewRegistry()
		e := &bandwidthEngine{name: engine.NameAria2}
		reg.Register(e)
		gov := engine.NewGovernor(reg, settings)
		require.Error(t, gov.LoadAndApply(t.Context()))
		require.Empty(t, e.calls, "a malformed stored row must never reach the engines as a limit")

		_, err = db.ExecContext(t.Context(),
			`DELETE FROM settings WHERE key = 'download_rate_limit'`)
		require.NoError(t, err)
	}
}

// taskReadbackEngine adds the optional per-task read-back surface the
// governor confirms through, answering the daemon truth the test
// configures.
type taskReadbackEngine struct {
	*bandwidthEngine
	down, up int64
	err      error
}

func (e *taskReadbackEngine) TaskLimits(context.Context, string) (int64, int64, error) {
	return e.down, e.up, e.err
}

func TestApplyTaskUsesBareEngineRef(t *testing.T) {
	reg := engine.NewRegistry()
	e := &bandwidthEngine{name: engine.NameAria2}
	reg.Register(e)

	require.NoError(t, engine.ApplyTask(context.Background(), reg, "aria2:2089b05ecca3d829", p64(1048576), p64(524288)))

	require.Equal(t,
		[]bandwidthCall{{id: "2089b05ecca3d829", down: p64(1048576), up: p64(524288)}},
		e.calls,
		"the engine must receive the bare ref, stripped of its namespace",
	)
}

func TestApplyTaskZeroIsUnlimited(t *testing.T) {
	reg := engine.NewRegistry()
	e := &bandwidthEngine{name: engine.NameQBittorrent}
	reg.Register(e)

	require.NoError(t, engine.ApplyTask(context.Background(), reg, "qbittorrent:8f9c3a2b", p64(0), p64(0)))

	// 0 is pushed as the literal unlimited value, never dropped as unset.
	require.Equal(t, []bandwidthCall{{id: "8f9c3a2b", down: p64(0), up: p64(0)}}, e.calls)
}

func TestApplyTaskNilDirectionLeftUntouched(t *testing.T) {
	reg := engine.NewRegistry()
	e := &bandwidthEngine{name: engine.NameAria2}
	reg.Register(e)

	require.NoError(t, engine.ApplyTask(context.Background(), reg, "aria2:2089b05ecca3d829", p64(2097152), nil))

	// The unsent direction reaches the engine as nil — left unchanged —
	// not as a guessed value like 0, which would mean unlimited.
	require.Equal(t,
		[]bandwidthCall{{id: "2089b05ecca3d829", down: p64(2097152), up: nil}},
		e.calls,
	)
}

func TestApplyTaskNoLifecycleCalls(t *testing.T) {
	reg := engine.NewRegistry()
	e := &bandwidthEngine{name: engine.NameAria2}
	reg.Register(e)

	require.NoError(t, engine.ApplyTask(context.Background(), reg, "aria2:2089b05ecca3d829", p64(1048576), p64(524288)))

	// The embedded nil Engine turns every lifecycle method — Pause,
	// Resume, Remove — into a panic the moment it runs, so reaching this
	// assertion at all proves the apply never touched them; the call log
	// then pins the one call it is allowed to make.
	require.Equal(t,
		[]bandwidthCall{{id: "2089b05ecca3d829", down: p64(1048576), up: p64(524288)}},
		e.calls,
	)
}

func TestApplyTaskNotSupportedPropagates(t *testing.T) {
	reg := engine.NewRegistry()
	e := &bandwidthEngine{name: engine.NameAria2, setErr: engine.ErrNotSupported}
	reg.Register(e)

	err := engine.ApplyTask(context.Background(), reg, "aria2:2089b05ecca3d829", p64(1048576), nil)

	// yt-dlp's answer: the caller treats it as success and stores the
	// value for the next spawn, so the sentinel must arrive intact.
	require.ErrorIs(t, err, engine.ErrNotSupported)
}

func TestApplyTaskUnregisteredEngine(t *testing.T) {
	err := engine.ApplyTask(context.Background(), engine.NewRegistry(), "aria2:2089b05ecca3d829", p64(1048576), nil)

	require.ErrorIs(t, err, engine.ErrUnavailable)
	require.Contains(t, err.Error(), engine.NameAria2,
		"the error must name the engine that is not registered",
	)
}

func TestApplyTaskMalformedID(t *testing.T) {
	reg := engine.NewRegistry()
	e := &bandwidthEngine{name: engine.NameAria2}
	reg.Register(e)

	for _, id := range []string{"2089b05ecca3d829", ":2089b05ecca3d829", "aria2:", ""} {
		require.Errorf(t, engine.ApplyTask(context.Background(), reg, id, p64(1048576), nil),
			"id %q has no usable engine namespace", id)
	}
	require.Empty(t, e.calls, "an id that cannot be routed reaches no engine")
}

func TestApplyTaskBothDirectionsNilCallsNothing(t *testing.T) {
	reg := engine.NewRegistry()
	e := &bandwidthEngine{name: engine.NameAria2}
	reg.Register(e)

	require.NoError(t, engine.ApplyTask(context.Background(), reg, "aria2:2089b05ecca3d829", nil, nil))
	require.Empty(t, e.calls, "a patch with no direction touches no engine")
}

func TestApplyTaskReadBackMismatchLogsOnly(t *testing.T) {
	reg := engine.NewRegistry()
	mismatch := &taskReadbackEngine{
		bandwidthEngine: &bandwidthEngine{name: engine.NameQBittorrent},
		down:            2097152, // daemon kept a different value than the 1048576 sent
		up:              524288,
	}
	reg.Register(mismatch)

	logs := captureGovernorLogs(t)

	require.NoError(t, engine.ApplyTask(context.Background(), reg, "qbittorrent:8f9c3a2b", p64(1048576), p64(524288)))

	require.Equal(t,
		[]bandwidthCall{{id: "8f9c3a2b", down: p64(1048576), up: p64(524288)}},
		mismatch.calls,
	)

	logged := logs.String()
	require.Contains(t, logged, "level=WARN")
	require.Contains(t, logged, "per-task download limit read back different")
	require.Contains(t, logged, "sent_bps=1048576")
	require.Contains(t, logged, "read_bps=2097152")
	require.Contains(t, logged, "engine="+engine.NameQBittorrent)
	require.NotContains(t, logged, "per-task upload limit read back different",
		"the direction the daemon did hold must not warn")
}

func TestApplyTaskNilDirectionSkipsReadBackComparison(t *testing.T) {
	reg := engine.NewRegistry()
	probe := &taskReadbackEngine{
		bandwidthEngine: &bandwidthEngine{name: engine.NameAria2},
		down:            1048576, // matches what was sent
		up:              524288,  // the daemon holds a limit the patch never touched
	}
	reg.Register(probe)

	logs := captureGovernorLogs(t)

	require.NoError(t, engine.ApplyTask(context.Background(), reg, "aria2:2089b05ecca3d829", p64(1048576), nil))

	require.NotContains(t, logs.String(), "read back different",
		"a direction the patch left as nil must not be compared against the daemon",
	)
}

func TestApplyTaskReadBackErrorStillSucceeds(t *testing.T) {
	reg := engine.NewRegistry()
	probe := &taskReadbackEngine{
		bandwidthEngine: &bandwidthEngine{name: engine.NameQBittorrent},
		err:             errors.New("daemon query failed"),
	}
	reg.Register(probe)

	require.NoError(t, engine.ApplyTask(context.Background(), reg, "qbittorrent:8f9c3a2b", p64(1048576), p64(524288)))

	require.Equal(t,
		[]bandwidthCall{{id: "8f9c3a2b", down: p64(1048576), up: p64(524288)}},
		probe.calls,
		"a failed read-back must not fail an apply that already succeeded",
	)
}

// parkFakeEngine records the calls the resolved apply path may make —
// SetRateLimits verbatim and Pause — so a test can prove the resolved
// value is what the engine heard and that a No Download cell sent none.
// The embedded nil engine.Engine keeps every other method a panic.
type parkFakeEngine struct {
	*bandwidthEngine
	pauses []string
}

func newParkFakeEngine(name string) *parkFakeEngine {
	return &parkFakeEngine{bandwidthEngine: &bandwidthEngine{name: name}}
}

func (e *parkFakeEngine) Pause(_ context.Context, id string) error {
	e.pauses = append(e.pauses, id)
	return nil
}

// newGovernorStore opens a throwaway store for the resolve tests: the
// parked set and the settings rows are real tables, like the schedule
// tests' fixture.
func newGovernorStore(t *testing.T) (*store.TaskStore, *store.SettingsStore) {
	t.Helper()
	root := t.TempDir()
	db, err := store.Open(t.Context(),
		filepath.Join(root, "dl-tool.db"), filepath.Join(root, "backups"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	return store.NewTaskStore(db), store.NewSettingsStore(db)
}

// addGovernedTask inserts one task row carrying per-task limits; an
// empty ref leaves engine_ref NULL — a task the admission pass never
// handed to an engine.
func addGovernedTask(t *testing.T, tasks *store.TaskStore, engineName, state, ref string, dl, ul int64) store.Task {
	t.Helper()

	var engineRef *string
	if ref != "" {
		engineRef = &ref
	}
	task, err := tasks.Create(t.Context(), store.Task{
		Engine: engineName, EngineRef: engineRef, SourceKind: "http",
		Name: "task " + engineName + "/" + ref, Destination: "/data", State: state,
		DLLimit: dl, ULLimit: ul,
	})
	require.NoError(t, err)

	return task
}

// TestEffectiveWorkedCase is the literal worked example of FR-096:
// alternative-speed cell 5242880, global 10485760, per-task 1048576 —
// the per-task term wins.
func TestEffectiveWorkedCase(t *testing.T) {
	rate, pause := engine.Effective(engine.ModeAlternative, 5242880, 10485760, 1048576)
	require.Equal(t, int64(1048576), rate)
	require.False(t, pause)
}

// TestZeroExcludedFromMin: a 0 term means unlimited and is excluded from
// the minimum — it must never win it.
func TestZeroExcludedFromMin(t *testing.T) {
	rate, pause := engine.Effective(engine.ModeDefault, 0, 10485760, 1048576)
	require.Equal(t, int64(1048576), rate, "the zero cell term is excluded, not the minimum")
	require.False(t, pause)
}

// TestAllZeroIsUnlimited: every term at 0 resolves to 0, unlimited.
func TestAllZeroIsUnlimited(t *testing.T) {
	rate, pause := engine.Effective(engine.ModeDefault, 0, 0, 0)
	require.Zero(t, rate)
	require.False(t, pause)
}

// TestEffectivePrecedence is the rest of the chain's table: each term
// winning in turn.
func TestEffectivePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name               string
		cell, global, task int64
		want               int64
	}{
		{"cell wins", 524288, 10485760, 1048576, 524288},
		{"global wins", 2097152, 1048576, 5242880, 1048576},
		{"task wins", 2097152, 10485760, 524288, 524288},
		{"zero task excluded", 1048576, 524288, 0, 524288},
		{"zero global excluded", 1048576, 0, 524288, 524288},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rate, pause := engine.Effective(engine.ModeDefault, tc.cell, tc.global, tc.task)
			require.Equal(t, tc.want, rate)
			require.False(t, pause)
		})
	}
}

// TestNoDownloadPausesNotThrottles: a No Download cell is a pause, never
// a near-zero rate — Effective reports pause, and through the governor
// no engine sees a rate at all: not through the global fan-out and not
// through a task apply.
func TestNoDownloadPausesNotThrottles(t *testing.T) {
	rate, pause := engine.Effective(engine.ModeNoDownload, 5242880, 10485760, 1048576)
	require.Zero(t, rate)
	require.True(t, pause)

	tasks, settings := newGovernorStore(t)
	reg := engine.NewRegistry()
	e := newParkFakeEngine(engine.NameAria2)
	reg.Register(e)
	gov := engine.NewGovernor(reg, settings).WithTasks(tasks)

	require.NoError(t, gov.ApplyMode(t.Context(), engine.ModeNoDownload))
	require.NoError(t, gov.ApplyGlobal(t.Context(), engine.RateLimits{Down: 1048576, Up: 524288}))
	require.Empty(t, e.calls, "a no-download cell sends no rate, not even a global one")

	task := addGovernedTask(t, tasks, engine.NameAria2, "downloading", "gid-throttle", 1048576, 524288)
	require.NoError(t, gov.ApplyTask(t.Context(), task))
	require.Empty(t, e.calls, "the paused task receives no rate")
	require.Equal(t, []string{"aria2:gid-throttle"}, e.pauses,
		"the pause reaches the engine as a pause, not a throttle")

	stored, err := tasks.Get(t.Context(), task.ID)
	require.NoError(t, err)
	require.Equal(t, "paused", stored.State)

	// The parked task requeues on the cell change — the T081
	// park-and-release bookkeeping ends the pause.
	require.NoError(t, gov.ApplyMode(t.Context(), engine.ModeDefault))
	stored, err = tasks.Get(t.Context(), task.ID)
	require.NoError(t, err)
	require.Equal(t, "queued", stored.State)
}

// TestApplyTaskResolvesThroughTheChain is the worked case of FR-096 end
// to end: alternative-speed cell 5242880 / 262144, global 10485760 /
// 2097152, per-task 1048576 / 131072 — the engine-global receives
// min(cell, global) and the task min(cell, global, task).
func TestApplyTaskResolvesThroughTheChain(t *testing.T) {
	tasks, settings := newGovernorStore(t)
	reg := engine.NewRegistry()
	e := newParkFakeEngine(engine.NameAria2)
	reg.Register(e)
	gov := engine.NewGovernor(reg, settings).WithTasks(tasks)

	require.NoError(t, settings.SetInt64(t.Context(), "download_rate_limit", 10485760))
	require.NoError(t, settings.SetInt64(t.Context(), "upload_rate_limit", 2097152))
	require.NoError(t, settings.SetInt64(t.Context(), "alt_download_rate_limit", 5242880))
	require.NoError(t, settings.SetInt64(t.Context(), "alt_upload_rate_limit", 262144))
	require.NoError(t, gov.ApplyMode(t.Context(), engine.ModeAlternative))

	task := addGovernedTask(t, tasks, engine.NameAria2, "downloading", "gid-worked", 1048576, 131072)
	require.NoError(t, gov.ApplyTask(t.Context(), task))

	require.Equal(t,
		[]bandwidthCall{
			{id: "", down: p64(5242880), up: p64(262144)},
			{id: "gid-worked", down: p64(1048576), up: p64(131072)},
		},
		e.calls,
		"the engine-global is min(cell, global) and the per-task push min(cell, global, task)",
	)
}

// TestApplyTaskParksUnderNoDownload: Resolve's pause takes the T081
// park path — engine pause first, then the parked-set row move — and
// the release machinery requeues the task on the cell change.
func TestApplyTaskParksUnderNoDownload(t *testing.T) {
	tasks, settings := newGovernorStore(t)
	reg := engine.NewRegistry()
	e := newParkFakeEngine(engine.NameAria2)
	reg.Register(e)
	gov := engine.NewGovernor(reg, settings).WithTasks(tasks)

	require.NoError(t, gov.ApplyMode(t.Context(), engine.ModeNoDownload))

	task := addGovernedTask(t, tasks, engine.NameAria2, "downloading", "gid-park", 1048576, 524288)
	require.NoError(t, gov.ApplyTask(t.Context(), task))

	require.Equal(t, []string{"aria2:gid-park"}, e.pauses)
	require.Empty(t, e.calls)
	stored, err := tasks.Get(t.Context(), task.ID)
	require.NoError(t, err)
	require.Equal(t, "paused", stored.State)

	parked, err := tasks.ListScheduleParked(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{task.ID}, parked,
		"the task joins the parked set the cell change releases")
}

// TestApplyTaskWithoutHandleSkipsPush: a task the admission pass has not
// handed an engine has no handle to push to; the resolve still runs and
// the stored limits apply at admission.
func TestApplyTaskWithoutHandleSkipsPush(t *testing.T) {
	tasks, settings := newGovernorStore(t)
	reg := engine.NewRegistry()
	e := newParkFakeEngine(engine.NameAria2)
	reg.Register(e)
	gov := engine.NewGovernor(reg, settings).WithTasks(tasks)

	task := addGovernedTask(t, tasks, engine.NameAria2, "queued", "", 1048576, 524288)
	require.NoError(t, gov.ApplyTask(t.Context(), task))
	require.Empty(t, e.calls)
}

// TestScheduleReportsTimezone: the body GET /settings/schedule renders
// names the container zone — here Europe/Zurich — and the cell in
// force. The assertion lives in this external test package because
// package jobs cannot import internal/api — api imports jobs — while
// engine_test may.
func TestScheduleReportsTimezone(t *testing.T) {
	zone, err := time.LoadLocation("Europe/Zurich")
	require.NoError(t, err)
	previous := time.Local
	time.Local = zone
	t.Cleanup(func() { time.Local = previous })

	root := t.TempDir()
	db, err := store.Open(t.Context(),
		filepath.Join(root, "dl-tool.db"), filepath.Join(root, "backups"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	handlers := api.NewSettingsHandlers(db, engine.NewRegistry())
	out, err := handlers.GetSchedule(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, "Europe/Zurich", out.Body.Timezone,
		"the schedule response carries the container's TZ, never a client-sent zone")
	require.Equal(t, string(store.ScheduleDefault), out.Body.ActiveMode,
		"a fresh all-default grid reports the default cell in force")
}
