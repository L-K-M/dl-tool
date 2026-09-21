package engine_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/store"
)

// bandwidthCall records one Engine.SetRateLimits invocation; id stays the
// argument verbatim so a test can see the governor sent the global "" id.
type bandwidthCall struct {
	id   string
	down int64
	up   int64
}

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
	e.calls = append(e.calls, bandwidthCall{id: id, down: *down, up: *up})
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
			[]bandwidthCall{{id: "", down: 1048576, up: 524288}},
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
	require.Equal(t, []bandwidthCall{{id: "", down: 0, up: 0}}, e.calls)
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
		[]bandwidthCall{{id: "", down: 1048576, up: 0}},
		ok.calls,
		"the capable engine still receives the value",
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

	require.Equal(t, []bandwidthCall{{id: "", down: 1048576, up: 524288}}, mismatch.calls)

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
		[]bandwidthCall{{id: "", down: 1048576, up: 0}},
		up.calls,
		"one unreachable daemon must not block the other engine",
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
	require.Equal(t, []bandwidthCall{{id: "", down: 0, up: 0}}, e.calls)
	require.Equal(t, engine.RateLimits{Down: 0, Up: 0}, gov.Current())

	require.NoError(t, settings.SetInt64(context.Background(), "download_rate_limit", 2097152))
	require.NoError(t, settings.SetInt64(context.Background(), "upload_rate_limit", 524288))

	require.NoError(t, gov.LoadAndApply(context.Background()))
	require.Equal(t,
		[]bandwidthCall{{id: "", down: 0, up: 0}, {id: "", down: 2097152, up: 524288}},
		e.calls,
		"the stored pair must reach the engine verbatim, in bytes per second",
	)
	require.Equal(t, engine.RateLimits{Down: 2097152, Up: 524288}, gov.Current())
}
