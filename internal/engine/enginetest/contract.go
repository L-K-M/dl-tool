//go:build integration

// Package enginetest holds the shared engine contract suite of
// docs/06-download-engines.md §11. RunContract drives the whole
// engine.Engine interface against a real daemon started by the call site's
// newEngine; an adapter that does not pass it is not done.
//
// Everything here builds only under the integration tag: the fast lane
// (make test) must stay green on a machine with no Docker and no daemon.
package enginetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"

	"github.com/L-K-M/dl-tool/internal/engine"
)

const (
	// fixtureBytes is the body every AddURL subtest downloads. 8 MiB at the
	// 1 MiB/s throttle below stays in flight for ~8 s, so every state the
	// subtests await is observable at the 250 ms poll cadence instead of
	// racing a loopback-speed completion.
	fixtureBytes = 8 << 20 // 8 MiB

	// pollInterval is the state-poll cadence: poll with a ticker and a
	// deadline, never a bare sleep.
	pollInterval = 250 * time.Millisecond

	// subtestTimeout bounds one subtest. Each subtest owns its deadline and
	// its own daemon, so no two share either.
	subtestTimeout = 120 * time.Second

	// rateLimitBytesPerSecond is the SpeedLimitRoundTrips obligation value
	// (1048576 B/s) and the throttle that keeps the lifecycle subtest's
	// phases observable.
	rateLimitBytesPerSecond int64 = 1 << 20

	// rateCeilingFactor widens the allowed reported rate above the cap.
// aria2's windowed speed calculation overshoots during the initial burst —
// 1.385× the cap observed on CI (1452256 under 1048576) — before the
// throttle's own average catches up, so the ceiling admits that burst and
// leaves a missing throttle (loopback rates, an order of magnitude up) to
// the elapsed-time bound below.
	rateCeilingFactor = 2.0

	// rateFloorFactor is how far below the cap the reported rate may sit: a
	// correctly throttled transfer reports close to the limit, not a stall.
	rateFloorFactor = 0.5

	// throttleSlack is the fraction of the physical minimum transfer time
	// (bytes / limit) a throttled download is still expected to take. It
	// absorbs poll granularity; it does not absorb a missing throttle.
	throttleSlack = 0.7

	// fabricatedRef is a daemon-shaped reference no adapter ever issued:
	// aria2 GIDs are 16 hex chars, so this parses everywhere it must.
	fabricatedRef = "deadbeefdeadbeef"
)

// fixture is one test's HTTP fixture: the URL engines download from and the
// SHA-256 of the body, for payload verification at the call site.
type fixture struct {
	url    string
	sha256 string
}

var (
	// fixturesByT shares one fixture server per *testing.T between the call
	// site — which must know the port before the container exists, to tunnel
	// it in — and the suite, which needs the URL for Add.
	fixturesMu sync.Mutex
	fixturesBy = map[*testing.T]*fixture{}

	fixtureOnce sync.Once
	fixtureBody []byte
	fixtureSum  string
	fixtureSeed = uint64(0x2545F4914F6CDD1D) // any non-zero seed
)

// Fixture serves the bytes an AddURL subtest downloads, so no test ever
// reaches a third-party host. It returns the URL of a deterministic 8 MiB
// body and stops with the test.
//
// The listener binds 0.0.0.0 and the URL's host is rewritten to
// testcontainers.HostInternal, because the downloading engine runs inside a
// container; the call site makes that name resolve by passing the port to
// testcontainers.WithHostPortAccess. Repeated calls with the same t return
// the same server.
func Fixture(t *testing.T) (fixtureURL string, sha256hex string) {
	t.Helper()

	fixturesMu.Lock()
	existing, found := fixturesBy[t]
	fixturesMu.Unlock()
	if found {
		return existing.url, existing.sha256
	}

	body, sum := deterministicBody()

	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("enginetest: bind fixture listener: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	// Replace httptest's loopback listener with the 0.0.0.0 one so
	// containers reach the server through the testcontainers host alias.
	server.Listener = listener
	server.Start()

	served, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("enginetest: parse fixture url %q: %v", server.URL, err)
	}
	_, port, err := net.SplitHostPort(served.Host)
	if err != nil {
		t.Fatalf("enginetest: split fixture host %q: %v", served.Host, err)
	}
	served.Host = net.JoinHostPort(testcontainers.HostInternal, port)

	f := &fixture{url: served.String(), sha256: sum}
	fixturesMu.Lock()
	fixturesBy[t] = f
	fixturesMu.Unlock()

	t.Cleanup(func() {
		server.Close()
		fixturesMu.Lock()
		delete(fixturesBy, t)
		fixturesMu.Unlock()
	})
	return f.url, f.sha256
}

// deterministicBody builds the 8 MiB download body once, from a hand-rolled
// xorshift64* generator with a fixed seed: the bytes — and therefore the
// SHA-256 — are identical on every platform and every Go release, with no
// dependence on math/rand's per-version sequences.
func deterministicBody() (body []byte, sha256hex string) {
	fixtureOnce.Do(func() {
		body := make([]byte, fixtureBytes)
		state := fixtureSeed
		for i := range body {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			body[i] = byte(state)
		}
		sum := sha256.Sum256(body)
		fixtureBody = body
		fixtureSum = hex.EncodeToString(sum[:])
	})
	return fixtureBody, fixtureSum
}

// Has reports whether e declares c. Subtests use it to skip a capability the
// adapter does not have, and to assert ErrNotSupported for every capability
// it does not declare.
func Has(e engine.Engine, c engine.Capability) bool {
	return slices.Contains(e.Capabilities(), c)
}

// RunContract asserts that an Engine implementation honours the interface in
// docs/06-download-engines.md against a real daemon. newEngine must return a
// connected Engine bound to a throwaway container and register its own
// t.Cleanup.
//
// Each subtest opens its own 120 s deadline and calls newEngine itself, so
// no subtest ever shares a daemon with another. Because every subtest gets
// its own fixture server (see Fixture), newEngine must call Fixture(t)
// itself — before creating the container — so the port can be tunneled to
// the daemon with testcontainers.WithHostPortAccess.
func RunContract(t *testing.T, newEngine func(t *testing.T) engine.Engine) {
	t.Helper()

	t.Run("AddURL/Progress/Pause/Resume/Remove", func(t *testing.T) {
		testLifecycle(t, newEngine)
	})
	t.Run("ListReturnsStableIDs", func(t *testing.T) {
		testListStableIDs(t, newEngine)
	})
	t.Run("UnknownIDReturnsErrNotFound", func(t *testing.T) {
		testUnknownID(t, newEngine)
	})
	t.Run("SpeedLimitRoundTrips", func(t *testing.T) {
		testSpeedLimits(t, newEngine)
	})
	t.Run("UnsupportedCapabilityReturnsErrNotSupported", func(t *testing.T) {
		testUnsupported(t, newEngine)
	})
}

// testLifecycle walks one task from add to remove: a namespaced id, a
// visible downloading phase with growing progress, pause, resume out of the
// pause, and a remove that makes the id vanish.
func testLifecycle(t *testing.T, newEngine func(t *testing.T) engine.Engine) {
	t.Helper()
	e := newEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), subtestTimeout)
	defer cancel()

	fixtureURL, _ := Fixture(t)

	id, err := e.Add(ctx, engine.AddRequest{URIs: []string{fixtureURL}, StartPaused: true})
	require.NoError(t, err)
	namespace := e.Name() + ":"
	require.True(t, strings.HasPrefix(id, namespace), "id %q must be namespaced %q", id, namespace)
	require.NotEmpty(t, strings.TrimPrefix(id, namespace), "id %q must carry the engine ref", id)

	// Cap the transfer while it is still paused: the 8 MiB body then takes
	// ~8 s, so each awaited phase is observable at the poll cadence.
	limit := rateLimitBytesPerSecond
	require.NoError(t, e.SetRateLimits(ctx, id, &limit, nil))

	require.NoError(t, e.Resume(ctx, id))
	downloading := pollUntil(t, ctx, e, id, "downloading", func(info engine.TaskInfo) bool {
		return info.State == engine.StateDownloading
	})
	grown := pollUntil(t, ctx, e, id, "completed bytes growth while downloading", func(info engine.TaskInfo) bool {
		return info.State == engine.StateDownloading && info.CompletedBytes > downloading.CompletedBytes
	})
	require.Greater(t, grown.CompletedBytes, downloading.CompletedBytes,
		"CompletedBytes must grow while downloading")

	require.NoError(t, e.Pause(ctx, id))
	pollUntil(t, ctx, e, id, "paused", func(info engine.TaskInfo) bool {
		return info.State == engine.StatePaused
	})

	require.NoError(t, e.Resume(ctx, id))
	pollUntil(t, ctx, e, id, "downloading again after resume", func(info engine.TaskInfo) bool {
		return info.State == engine.StateDownloading
	})

	require.NoError(t, e.Remove(ctx, id))
	_, err = e.Get(ctx, id)
	require.ErrorIs(t, err, engine.ErrNotFound, "Get after Remove must report ErrNotFound")
}

// testListStableIDs adds one task and asserts List reports its id,
// byte-identical, in three consecutive calls. The full id list is compared
// per call, so an id that drifts, vanishes or is joined by a phantom fails.
func testListStableIDs(t *testing.T, newEngine func(t *testing.T) engine.Engine) {
	t.Helper()
	e := newEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), subtestTimeout)
	defer cancel()

	fixtureURL, _ := Fixture(t)
	id, err := e.Add(ctx, engine.AddRequest{URIs: []string{fixtureURL}, StartPaused: true})
	require.NoError(t, err)

	var seen [3][]string
	for i := range seen {
		infos, err := e.List(ctx)
		require.NoError(t, err, "List call %d", i+1)
		for _, info := range infos {
			seen[i] = append(seen[i], info.ID)
		}
		require.Contains(t, seen[i], id, "List call %d must contain id %q", i+1, id)
	}
	require.Equal(t, seen[0], seen[1], "ids changed between List calls 1 and 2")
	require.Equal(t, seen[1], seen[2], "ids changed between List calls 2 and 3")
}

// testUnknownID drives Get, Files, Pause, Resume and Remove — the calls the
// obligations table names — with a fabricated id and requires ErrNotFound
// from each. SetRateLimits is not capability-gated and not in the table, so
// it is not probed here.
func testUnknownID(t *testing.T, newEngine func(t *testing.T) engine.Engine) {
	t.Helper()
	e := newEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), subtestTimeout)
	defer cancel()

	unknown := e.Name() + ":" + fabricatedRef

	_, err := e.Get(ctx, unknown)
	require.ErrorIs(t, err, engine.ErrNotFound, "Get on a fabricated id")
	_, err = e.Files(ctx, unknown)
	require.ErrorIs(t, err, engine.ErrNotFound, "Files on a fabricated id")
	require.ErrorIs(t, e.Pause(ctx, unknown), engine.ErrNotFound, "Pause on a fabricated id")
	require.ErrorIs(t, e.Resume(ctx, unknown), engine.ErrNotFound, "Resume on a fabricated id")
	require.ErrorIs(t, e.Remove(ctx, unknown), engine.ErrNotFound, "Remove on a fabricated id")
}

// testSpeedLimits proves a per-task and a global 1048576 B/s cap both reach
// the daemon: the reported download rate settles in a band around the cap,
// and the transfer takes at least the physical minimum bytes/limit time.
// Both limits are armed while the task is paused, so no unthrottled byte is
// ever transferred and the timing bound cannot be beaten by a race.
func testSpeedLimits(t *testing.T, newEngine func(t *testing.T) engine.Engine) {
	t.Helper()
	e := newEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), subtestTimeout)
	defer cancel()

	limit := rateLimitBytesPerSecond
	fixtureURL, _ := Fixture(t)

	taskID, err := e.Add(ctx, engine.AddRequest{URIs: []string{fixtureURL}, StartPaused: true})
	require.NoError(t, err)
	require.NoError(t, e.SetRateLimits(ctx, taskID, &limit, nil), "per-task limit")

	started := time.Now()
	require.NoError(t, e.Resume(ctx, taskID))
	maxRate := pollUntilCompleted(t, ctx, e, taskID)
	assertThrottled(t, maxRate, time.Since(started))

	globalID, err := e.Add(ctx, engine.AddRequest{URIs: []string{fixtureURL}, StartPaused: true})
	require.NoError(t, err)
	require.NoError(t, e.SetRateLimits(ctx, "", &limit, nil), "global limit")

	started = time.Now()
	require.NoError(t, e.Resume(ctx, globalID))
	maxRate = pollUntilCompleted(t, ctx, e, globalID)
	assertThrottled(t, maxRate, time.Since(started))
}

// assertThrottled checks one throttled transfer: the daemon must report a
// rate inside the band around the cap, and the transfer must last at least
// the physical minimum (bytes / cap) reduced by throttleSlack.
func assertThrottled(t *testing.T, maxReportedRate int64, elapsed time.Duration) {
	t.Helper()

	// The rate assertions sample once per pollInterval, so the throttled
	// transfer must span several samples for the floor to be meaningful.
	minimum := time.Duration(float64(fixtureBytes) / float64(rateLimitBytesPerSecond) * float64(time.Second))
	require.GreaterOrEqual(t, minimum, 3*pollInterval,
		"the fixture must span several poll intervals at the cap for the rate assertions to be reliable")

	ceiling := int64(float64(rateLimitBytesPerSecond) * rateCeilingFactor)
	floor := int64(float64(rateLimitBytesPerSecond) * rateFloorFactor)
	require.GreaterOrEqual(t, maxReportedRate, floor,
		"daemon must report a rate near %d B/s, not a stall", rateLimitBytesPerSecond)
	require.LessOrEqual(t, maxReportedRate, ceiling,
		"daemon must not report more than %d B/s under a %d B/s cap", ceiling, rateLimitBytesPerSecond)

	expected := time.Duration(float64(minimum) * throttleSlack)
	require.GreaterOrEqual(t, elapsed, expected,
		"%d bytes cannot arrive in under %s of a %d B/s cap; the limit never applied",
		fixtureBytes, expected, rateLimitBytesPerSecond)
}

// pollUntilCompleted samples Get every pollInterval until the task completes,
// returning the highest download rate the daemon reported for it along the
// way.
func pollUntilCompleted(t *testing.T, ctx context.Context, e engine.Engine, id string) int64 {
	t.Helper()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	maxRate := int64(0)
	var last engine.TaskInfo
	var lastErr error
	for {
		info, err := e.Get(ctx, id)
		switch {
		case err != nil:
			lastErr = err
		case info.State == engine.StateError:
			t.Fatalf("task %s entered error state while throttled: %+v (code %q: %s)",
				id, info, info.ErrorCode, info.ErrorMessage)
		default:
			lastErr = nil
			maxRate = max(maxRate, info.DownloadRate)
			last = info
			if info.State == engine.StateCompleted {
				return maxRate
			}
		}

		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for task %s to complete: last info %+v, last error %v",
				id, last, lastErr)
		case <-ticker.C:
		}
	}
}

// optionalMethod pairs one capability-backed optional Engine method with the
// call that exercises it. The argument values are irrelevant — the method
// must refuse before any I/O — but stay daemon-plausible so an adapter that
// wrongly acts on them is caught by the state re-read.
type optionalMethod struct {
	capability engine.Capability
	invoke     func(ctx context.Context, e engine.Engine, id string) error
}

// optionalMethods is the task's capability-to-method table: every optional
// method maps to the capability whose absence must make it refuse.
var optionalMethods = []optionalMethod{
	{engine.CapPerFileSelect, func(ctx context.Context, e engine.Engine, id string) error {
		return e.SetFiles(ctx, id, []int{0}, nil)
	}},
	{engine.CapPerFilePriority, func(ctx context.Context, e engine.Engine, id string) error {
		return e.SetFiles(ctx, id, []int{0}, map[int]int{0: 1}) // 1 = normal, §1.1
	}},
	{engine.CapSetLocation, func(ctx context.Context, e engine.Engine, id string) error {
		return e.SetLocation(ctx, id, "/tmp/enginetest-relocated")
	}},
	{engine.CapRename, func(ctx context.Context, e engine.Engine, id string) error {
		return e.Rename(ctx, id, "enginetest-renamed")
	}},
	{engine.CapCategories, func(ctx context.Context, e engine.Engine, id string) error {
		return e.SetCategory(ctx, id, "enginetest-category")
	}},
	{engine.CapShareLimits, func(ctx context.Context, e engine.Engine, id string) error {
		ratio, seedMinutes := 2.0, int64(60)
		return e.SetShareLimits(ctx, id, &ratio, &seedMinutes)
	}},
}

// testUnsupported walks the optional-method table: every capability the
// adapter does not declare must make its method return ErrNotSupported and
// change nothing — the task state is re-read after each refusal.
func testUnsupported(t *testing.T, newEngine func(t *testing.T) engine.Engine) {
	t.Helper()
	e := newEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), subtestTimeout)
	defer cancel()

	// A paused task gives the state re-read something stable to compare.
	fixtureURL, _ := Fixture(t)
	id, err := e.Add(ctx, engine.AddRequest{URIs: []string{fixtureURL}, StartPaused: true})
	require.NoError(t, err)

	before := settlePaused(t, ctx, e, id)

	for _, method := range optionalMethods {
		if Has(e, method.capability) {
			continue // declared: the adapter supports it, nothing to assert
		}
		err := method.invoke(ctx, e, id)
		require.ErrorIs(t, err, engine.ErrNotSupported,
			"%s is not declared, so its method must refuse", method.capability)

		after, err := e.Get(ctx, id)
		require.NoError(t, err, "state re-read after refusing %s", method.capability)
		require.Empty(t, cmp.Diff(before, after),
			"refusing %s must mutate nothing", method.capability)
	}
}

// settlePaused waits until two consecutive Get calls agree, so the diff
// baseline around each refusal only trips on mutations the refused method
// caused — never on fields an adapter still populates asynchronously after
// Add returned.
func settlePaused(t *testing.T, ctx context.Context, e engine.Engine, id string) engine.TaskInfo {
	t.Helper()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	previous, err := e.Get(ctx, id)
	require.NoError(t, err, "baseline read of task %s", id)
	for {
		current, err := e.Get(ctx, id)
		require.NoError(t, err, "re-read while settling task %s", id)
		if cmp.Equal(previous, current) {
			return current
		}
		previous = current

		select {
		case <-ctx.Done():
			t.Fatalf("paused task %s never settled to a stable state: %+v", id, current)
		case <-ticker.C:
		}
	}
}

// pollUntil polls Get every pollInterval until want returns true, failing
// the subtest with the last TaskInfo rendered when the deadline passes or
// the task errors out first.
func pollUntil(t *testing.T, ctx context.Context, e engine.Engine, id, what string, want func(engine.TaskInfo) bool) engine.TaskInfo {
	t.Helper()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var last engine.TaskInfo
	var lastErr error
	for {
		info, err := e.Get(ctx, id)
		switch {
		case err != nil:
			lastErr = err
		case info.State == engine.StateError && !want(info):
			t.Fatalf("task %s entered error state while waiting for %s: %+v (code %q: %s)",
				id, what, info, info.ErrorCode, info.ErrorMessage)
		case want(info):
			return info
		default:
			lastErr = nil
			last = info
		}

		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s of task %s: last info %+v, last error %v",
				what, id, last, lastErr)
		case <-ticker.C:
		}
	}
}
