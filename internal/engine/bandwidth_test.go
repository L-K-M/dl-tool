package engine_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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

	gov := engine.NewGovernor(reg, nil)
	require.NoError(t, gov.ApplyTask(context.Background(), "aria2:2089b05ecca3d829", p64(1048576), p64(524288)))

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

	gov := engine.NewGovernor(reg, nil)
	require.NoError(t, gov.ApplyTask(context.Background(), "qbittorrent:8f9c3a2b", p64(0), p64(0)))

	// 0 is pushed as the literal unlimited value, never dropped as unset.
	require.Equal(t, []bandwidthCall{{id: "8f9c3a2b", down: p64(0), up: p64(0)}}, e.calls)
}

func TestApplyTaskNilDirectionLeftUntouched(t *testing.T) {
	reg := engine.NewRegistry()
	e := &bandwidthEngine{name: engine.NameAria2}
	reg.Register(e)

	gov := engine.NewGovernor(reg, nil)
	require.NoError(t, gov.ApplyTask(context.Background(), "aria2:2089b05ecca3d829", p64(2097152), nil))

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

	gov := engine.NewGovernor(reg, nil)
	require.NoError(t, gov.ApplyTask(context.Background(), "aria2:2089b05ecca3d829", p64(1048576), p64(524288)))

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

	gov := engine.NewGovernor(reg, nil)
	err := gov.ApplyTask(context.Background(), "aria2:2089b05ecca3d829", p64(1048576), nil)

	// yt-dlp's answer: the caller treats it as success and stores the
	// value for the next spawn, so the sentinel must arrive intact.
	require.ErrorIs(t, err, engine.ErrNotSupported)
}

func TestApplyTaskUnregisteredEngine(t *testing.T) {
	gov := engine.NewGovernor(engine.NewRegistry(), nil)
	err := gov.ApplyTask(context.Background(), "aria2:2089b05ecca3d829", p64(1048576), nil)

	require.ErrorIs(t, err, engine.ErrUnavailable)
	require.Contains(t, err.Error(), engine.NameAria2,
		"the error must name the engine that is not registered",
	)
}

func TestApplyTaskMalformedID(t *testing.T) {
	reg := engine.NewRegistry()
	e := &bandwidthEngine{name: engine.NameAria2}
	reg.Register(e)

	gov := engine.NewGovernor(reg, nil)
	for _, id := range []string{"2089b05ecca3d829", ":2089b05ecca3d829", "aria2:", ""} {
		require.Errorf(t, gov.ApplyTask(context.Background(), id, p64(1048576), nil),
			"id %q has no usable engine namespace", id)
	}
	require.Empty(t, e.calls, "an id that cannot be routed reaches no engine")
}

func TestApplyTaskBothDirectionsNilCallsNothing(t *testing.T) {
	reg := engine.NewRegistry()
	e := &bandwidthEngine{name: engine.NameAria2}
	reg.Register(e)

	gov := engine.NewGovernor(reg, nil)
	require.NoError(t, gov.ApplyTask(context.Background(), "aria2:2089b05ecca3d829", nil, nil))
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

	gov := engine.NewGovernor(reg, nil)
	require.NoError(t, gov.ApplyTask(context.Background(), "qbittorrent:8f9c3a2b", p64(1048576), p64(524288)))

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
