//go:build integration

// Proof that SpeedLimitRoundTrips observes the daemon's configured limit
// through DownloadLimitReadback instead of inferring it from transfer
// timing alone: a daemon holding three quarters of the request still fits
// the timing window, and an engine without a readback cannot meet the
// exact-setting obligation at all. The fakes run without a daemon; the
// two dishonest engines run in a re-executed test process, because a
// failure the suite correctly records still fails this test's own tree.

package enginetest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/engine"
)

// dishonestEnv names the engine a re-executed test process must feed to
// testSpeedLimits. Its values are the two regressions this file pins:
const (
	dishonestEnv = "DLTOOL_ENGINETEST_DISHONEST_ENGINE"

	// dishonestFraction applies and reports three quarters of every
	// requested limit — the wrong daemon value that once passed the suite.
	dishonestFraction = "fraction"

	// dishonestNoReadback implements engine.Engine but no
	// DownloadLimitReadback, hiding the daemon's configuration entirely.
	dishonestNoReadback = "no-readback"
)

// fakeThrottle simulates one daemon honouring every limit exactly: Add
// parks, Resume starts an 8 MiB transfer at the applied cap, and the
// readback reports the configured limit from the same store the setter
// wrote. numerator/denominator scale the applied limit to simulate a
// daemon that keeps only a fraction of what it was asked for.
type fakeThrottle struct {
	engine.Engine // nil: only the calls SpeedLimitRoundTrips makes are implemented

	mu          sync.Mutex
	limits      map[string]int64 // the fake's daemon truth; "" is the global limit
	started     map[string]time.Time
	nextID      int
	numerator   int64
	denominator int64
	readbacks   []string
	firstTaskID string
}

func newFakeThrottle(numerator, denominator int64) *fakeThrottle {
	return &fakeThrottle{
		limits:      map[string]int64{},
		started:     map[string]time.Time{},
		numerator:   numerator,
		denominator: denominator,
	}
}

func (f *fakeThrottle) Add(_ context.Context, req engine.AddRequest) (string, error) {
	if !req.StartPaused {
		return "", fmt.Errorf("fake: the suite always adds paused")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := "fake:" + strconv.Itoa(f.nextID)
	if f.firstTaskID == "" {
		f.firstTaskID = id
	}
	return id, nil
}

// SetRateLimits writes the fraction the fake daemon actually keeps; the
// suite's requested value is scaled, which is exactly the defect the
// readback exists to catch.
func (f *fakeThrottle) SetRateLimits(_ context.Context, id string, down, _ *int64) error {
	if down == nil {
		return nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.limits[id] = *down * f.numerator / f.denominator
	return nil
}

// DaemonDownloadLimit reports the fake's daemon truth and records the
// consultation, so the pass-path case can prove the suite asked.
func (f *fakeThrottle) DaemonDownloadLimit(_ context.Context, id string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readbacks = append(f.readbacks, id)

	limit, ok := f.limits[id]
	if !ok {
		return 0, fmt.Errorf("fake: no limit configured for %q", id)
	}
	return limit, nil
}

func (f *fakeThrottle) Resume(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started[id] = time.Now()
	return nil
}

// Get advances the transfer in wall-clock time at the applied cap, so the
// suite's elapsed bounds and rate floor see a believable throttled
// transfer. A task without its own cap runs under the global one, the
// same way a real daemon applies its global limit.
func (f *fakeThrottle) Get(_ context.Context, id string) (engine.TaskInfo, error) {
	f.mu.Lock()
	limit, taskCapped := f.limits[id]
	global := f.limits[""]
	started := f.started[id]
	f.mu.Unlock()
	if !taskCapped {
		limit = global
	}

	// bytes = elapsed × (limit B/s): Duration arithmetic keeps the ns
	// scale, the final quotient is the byte count.
	completed := min(int64(time.Since(started)*time.Duration(limit)/time.Second), fixtureBytes)
	state := engine.StateDownloading
	if completed >= fixtureBytes {
		state = engine.StateCompleted
	}
	total := int64(fixtureBytes)
	return engine.TaskInfo{
		ID:             id,
		Engine:         "fake",
		State:          state,
		TotalBytes:     &total,
		CompletedBytes: completed,
		DownloadRate:   limit,
	}, nil
}

func (f *fakeThrottle) consultedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.readbacks...)
}

func TestSpeedLimitsReadBackTheDaemonLimit(t *testing.T) {
	// A re-executed process with dishonestEnv set feeds one dishonest
	// engine to testSpeedLimits; its whole purpose is to exit non-zero,
	// which the parent asserts below.
	if target := os.Getenv(dishonestEnv); target != "" {
		testSpeedLimits(t, dishonestEngine(t, target))
		return
	}

	t.Run("exact readback passes and is consulted", func(t *testing.T) {
		fake := newFakeThrottle(1, 1)
		testSpeedLimits(t, func(*testing.T) engine.Engine { return fake })

		// The suite must have asked the daemon for the per-task and the
		// global limit, in that order — a suite that stopped asking is
		// the original defect again.
		require.Equal(t, []string{fake.firstTaskID, ""}, fake.consultedIDs(),
			"the suite must read both limits back from the daemon")
	})

	for _, target := range []string{dishonestFraction, dishonestNoReadback} {
		t.Run("the suite rejects the "+target+" engine", func(t *testing.T) {
			requireSuiteProcessFails(t, target)
		})
	}
}

// dishonestEngine builds the engine a re-executed process must feed to
// testSpeedLimits.
func dishonestEngine(t *testing.T, target string) func(*testing.T) engine.Engine {
	t.Helper()

	switch target {
	case dishonestFraction:
		return func(*testing.T) engine.Engine { return newFakeThrottle(3, 4) }
	case dishonestNoReadback:
		// Embedding the interface value hides the fake's readback: only
		// the engine.Engine surface remains.
		return func(*testing.T) engine.Engine {
			return struct{ engine.Engine }{newFakeThrottle(1, 1)}
		}
	default:
		t.Fatalf("unknown dishonest engine %q", target)
		return nil
	}
}

// requireSuiteProcessFails re-executes this test binary against one
// dishonest engine and asserts SpeedLimitRoundTrips fails it: the failure
// cannot be observed in-process, because a failure the suite records on a
// subtest of this tree fails this test too.
func requireSuiteProcessFails(t *testing.T, target string) {
	t.Helper()

	cmd := exec.Command(os.Args[0],
		"-test.run", "^TestSpeedLimitsReadBackTheDaemonLimit$",
		"-test.count", "1",
		"-test.timeout", "60s")
	cmd.Env = append(os.Environ(), dishonestEnv+"="+target)

	output, err := cmd.CombinedOutput()
	require.Error(t, err,
		"SpeedLimitRoundTrips must fail the %s engine, but it passed. Output:\n%s", target, output)
}
