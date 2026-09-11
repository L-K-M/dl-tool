package qbittorrent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/engine"
)

// Limits of the tests, in bytes per second. The values deliberately
// contain no substring a unit conversion would leave behind: nothing in
// this file may read like a 1024.
const (
	testDownLimit int64 = 262144
	testUpLimit   int64 = 65536
	testOldLimit  int64 = 131072
	testDrift     int64 = 393216
)

// limitsFake is a WebAPI stand-in for the five endpoints of T037: the
// four set endpoints, which answer a configurable status and remember the
// last accepted limit, and transfer/info, which serves those values back —
// or the override a mismatch test installs. The login and session rules
// mirror fakeServer's (docs/06 section 5.2). Any other path, the
// lifecycle calls and the alternative-speed endpoints included, fails the
// test: a rate limit must disturb nothing else.
type limitsFake struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	calls    []mutateRequest
	status   map[string]int // answered per path; 0 means 200
	applied  map[string]int64
	infoDown *int64 // nil: echo the applied transfer limit
	infoUp   *int64 // nil: echo the applied transfer limit
	cookie   string
}

func newLimitsFake(t *testing.T, tune func(*limitsFake)) *limitsFake {
	t.Helper()

	f := &limitsFake{t: t, applied: map[string]int64{}}
	if tune != nil {
		tune(f)
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)

	return f
}

func (f *limitsFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("parse form: %v", err)
	}
	rec := mutateRequest{Method: r.Method, Path: r.URL.Path, Form: r.PostForm}

	f.mu.Lock()
	defer f.mu.Unlock()

	// Only a request the session accepted is observable behaviour; the
	// lazy re-login's first 401 attempt is not part of what a test pins.
	record := func() { f.calls = append(f.calls, rec) }

	switch r.URL.Path {
	case "/api/v2/auth/login":
		if rec.Form.Get("username") != testUsername || rec.Form.Get("password") != testPassword {
			http.Error(w, "Fails.", http.StatusUnauthorized)

			return
		}
		f.cookie = "sid-limits"
		http.SetCookie(w, &http.Cookie{Name: testCookie, Value: f.cookie, Path: "/", HttpOnly: true})
		w.WriteHeader(http.StatusNoContent)

	case "/api/v2/app/version":
		if !f.sessionOK(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)

			return
		}
		_, _ = w.Write([]byte(testVersion))

	case "/api/v2/torrents/setDownloadLimit", "/api/v2/torrents/setUploadLimit",
		"/api/v2/transfer/setDownloadLimit", "/api/v2/transfer/setUploadLimit":
		if !f.sessionOK(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)

			return
		}
		record()
		limit, err := strconv.ParseInt(rec.Form.Get("limit"), 10, 64)
		if err != nil {
			f.t.Errorf("limit field is not an integer: %q", rec.Form.Get("limit"))
		} else {
			f.applied[r.URL.Path] = limit
		}
		status := f.status[r.URL.Path]
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)

	case "/api/v2/transfer/info":
		if !f.sessionOK(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)

			return
		}
		record()
		down := f.applied["/api/v2/"+pathTransferSetDownloadLimit]
		if f.infoDown != nil {
			down = *f.infoDown
		}
		up := f.applied["/api/v2/"+pathTransferSetUploadLimit]
		if f.infoUp != nil {
			up = *f.infoUp
		}
		w.Header().Set("Content-Type", "application/json")
		_, err := fmt.Fprintf(w,
			`{"dl_info_speed":0,"up_info_speed":0,"dl_rate_limit":%d,"up_rate_limit":%d,"dht_nodes":0,"connection_status":"connected","use_alt_speed_limits":false}`,
			down, up)
		if err != nil {
			f.t.Errorf("write transfer info body: %v", err)
		}

	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}

// sessionOK enforces the cookie of the latest login.
func (f *limitsFake) sessionOK(r *http.Request) bool {
	cookie, err := r.Cookie(testCookie)

	return err == nil && f.cookie != "" && cookie.Value == f.cookie
}

// callsTo returns every recorded call of one endpoint, in order.
func (f *limitsFake) callsTo(path string) []mutateRequest {
	f.t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	var calls []mutateRequest
	for _, call := range f.calls {
		if call.Path == "/api/v2/"+path {
			calls = append(calls, call)
		}
	}

	return calls
}

// allCalls returns every session-accepted call the fake saw.
func (f *limitsFake) allCalls() []mutateRequest {
	f.t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.calls)
}

// newLimitsClient returns a client over one fake. Connect is never
// called: the sets reach the daemon through the lazy re-login of
// authenticated, and skipping Connect keeps the sync/maindata poll out of
// the fixture. The per-task verification reads the merged cache, which
// the seeding below populates without a poll.
func newLimitsClient(t *testing.T, f *limitsFake) *Client {
	t.Helper()

	c, err := New(Config{BaseURL: f.srv.URL, Username: testUsername, Password: testPassword}, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, c.Close())
	})

	return c
}

// seedLimitsCache publishes one full maindata snapshot over the client's
// cache, so the per-task verification holds a current dl_limit/up_limit
// pair without a running poll.
func seedLimitsCache(t *testing.T, c *Client, body string) {
	t.Helper()

	c.SetOwnershipFilter(staticSource(setOf(testHash)))
	c.applyResponse(maindata{
		Rid:        1,
		FullUpdate: true,
		Torrents:   fullTorrents(map[string]string{testHash: body}),
	}, currentEpoch(c))
}

// limitsTorrentBody is one maindata torrent object carrying the state
// and the two limit fields the cache verification reads.
func limitsTorrentBody(state string, dlLimit, upLimit int64) string {
	return `{"hash":"` + testHash + `","name":"limits.iso","state":"` + state +
		`","progress":0.5,"dl_limit":` + strconv.FormatInt(dlLimit, 10) +
		`,"up_limit":` + strconv.FormatInt(upLimit, 10) + `}`
}

// lockedBuffer is the io.Writer captureWarns hands slog: warns arrive
// from the watcher goroutine while the test polls the contents, so the
// writes and the reads need one mutex between them.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// captureWarns swaps the default logger for one that writes into a
// buffer, so a test can pin what a warn carried. The default logger is
// process-wide, which is why no test of this package is parallel.
func captureWarns(t *testing.T) *lockedBuffer {
	t.Helper()

	var logs lockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return &logs
}

// TestPerTaskLimitOneRequest pins the per-task fan-out of section 10.1: a
// download-only set issues exactly one request — torrents/setDownloadLimit
// with hashes and limit, the namespace already split off — and nothing
// else: no second request, no read-back call.
func TestPerTaskLimitOneRequest(t *testing.T) {
	f := newLimitsFake(t, nil)
	c := newLimitsClient(t, f)
	seedLimitsCache(t, c, limitsTorrentBody("downloading", testOldLimit, 0))

	down := testDownLimit
	require.NoError(t, c.SetRateLimits(context.Background(), engine.NameQBittorrent+":"+testHash, &down, nil))

	calls := f.allCalls()
	require.Len(t, calls, 1)
	require.Equal(t, http.MethodPost, calls[0].Method)
	require.Equal(t, "/api/v2/torrents/setDownloadLimit", calls[0].Path)
	require.Equal(t, url.Values{
		"hashes": {testHash},
		"limit":  {strconv.FormatInt(testDownLimit, 10)},
	}, calls[0].Form)
}

// TestGlobalLimitUsesTransferPaths pins the global fan-out and its
// read-back: transfer/setDownloadLimit then transfer/setUploadLimit, each
// carrying only limit, then one GET transfer/info whose echo closes the
// loop.
func TestGlobalLimitUsesTransferPaths(t *testing.T) {
	f := newLimitsFake(t, nil)
	c := newLimitsClient(t, f)

	down, up := testDownLimit, testUpLimit
	require.NoError(t, c.SetRateLimits(context.Background(), "", &down, &up))

	calls := f.allCalls()
	require.Len(t, calls, 3)
	require.Equal(t, "/api/v2/transfer/setDownloadLimit", calls[0].Path)
	require.Equal(t, url.Values{"limit": {strconv.FormatInt(testDownLimit, 10)}}, calls[0].Form)
	require.Equal(t, "/api/v2/transfer/setUploadLimit", calls[1].Path)
	require.Equal(t, url.Values{"limit": {strconv.FormatInt(testUpLimit, 10)}}, calls[1].Form)
	require.Equal(t, "/api/v2/transfer/info", calls[2].Path)
	require.Equal(t, http.MethodGet, calls[2].Method)

	gotDown, gotUp, err := c.GlobalLimits(context.Background())
	require.NoError(t, err)
	require.Equal(t, testDownLimit, gotDown)
	require.Equal(t, testUpLimit, gotUp)
}

// TestZeroMeansUnlimited pins the 0 encoding: 0 is unlimited and travels
// as a literal 0 on both the per-task and the global paths — never
// dropped as "unset" — and reads back as 0.
func TestZeroMeansUnlimited(t *testing.T) {
	f := newLimitsFake(t, nil)
	c := newLimitsClient(t, f)
	seedLimitsCache(t, c, limitsTorrentBody("downloading", testOldLimit, testUpLimit))

	zero := int64(0)
	require.NoError(t, c.SetRateLimits(context.Background(), engine.NameQBittorrent+":"+testHash, &zero, nil))
	taskCalls := f.callsTo("torrents/setDownloadLimit")
	require.Len(t, taskCalls, 1)
	require.Equal(t, "0", taskCalls[0].Form.Get("limit"))

	require.NoError(t, c.SetRateLimits(context.Background(), "", &zero, &zero))
	require.Equal(t, "0", f.callsTo("transfer/setDownloadLimit")[0].Form.Get("limit"))
	require.Equal(t, "0", f.callsTo("transfer/setUploadLimit")[0].Form.Get("limit"))
}

// TestBothNilIssuesNoRequest pins the nil encoding: both directions nil
// means nothing to say — no request, no read-back, no error.
func TestBothNilIssuesNoRequest(t *testing.T) {
	f := newLimitsFake(t, nil)
	c := newLimitsClient(t, f)

	require.NoError(t, c.SetRateLimits(context.Background(), engine.NameQBittorrent+":"+testHash, nil, nil))
	require.NoError(t, c.SetRateLimits(context.Background(), "", nil, nil))
	require.Empty(t, f.allCalls())
}

// TestReadBackMismatch pins the global read-back contract: a
// transfer/info that reports a different value than the one sent is
// logged at warn with both values and returned as ErrLimitNotApplied,
// wrapped with the direction that drifted.
func TestReadBackMismatch(t *testing.T) {
	f := newLimitsFake(t, func(fake *limitsFake) { fake.infoDown = ptr(testDrift) })
	c := newLimitsClient(t, f)
	logs := captureWarns(t)

	down := testDownLimit
	err := c.SetRateLimits(context.Background(), "", &down, nil)
	require.ErrorIs(t, err, ErrLimitNotApplied)
	require.Contains(t, err.Error(), "download")
	require.Contains(t, logs.String(), strconv.FormatInt(testDownLimit, 10))
	require.Contains(t, logs.String(), strconv.FormatInt(testDrift, 10))

	// The upload direction is named when it is the one that drifted.
	upFake := newLimitsFake(t, func(fake *limitsFake) { fake.infoUp = ptr(testDrift) })
	upClient := newLimitsClient(t, upFake)
	up := testUpLimit
	err = upClient.SetRateLimits(context.Background(), "", nil, &up)
	require.ErrorIs(t, err, ErrLimitNotApplied)
	require.Contains(t, err.Error(), "upload")
}

// TestPerTaskLimitLeavesRunningTaskAlone pins FR-094's "without
// restarting it": a limit applied while the cached state is downloading
// touches only the two set endpoints — no pause, resume, add or delete
// call, and no read-back request — and the cached state stays
// downloading.
func TestPerTaskLimitLeavesRunningTaskAlone(t *testing.T) {
	f := newLimitsFake(t, nil)
	c := newLimitsClient(t, f)
	seedLimitsCache(t, c, limitsTorrentBody("downloading", testOldLimit, testUpLimit))

	down, up := testDownLimit, testUpLimit
	require.NoError(t, c.SetRateLimits(context.Background(), engine.NameQBittorrent+":"+testHash, &down, &up))

	calls := f.allCalls()
	require.Len(t, calls, 2)
	for _, call := range calls {
		require.Equal(t, http.MethodPost, call.Method)
		require.Contains(t, []string{
			"/api/v2/torrents/setDownloadLimit",
			"/api/v2/torrents/setUploadLimit",
		}, call.Path)
	}

	info, err := c.Get(context.Background(), engine.NameQBittorrent+":"+testHash)
	require.NoError(t, err)
	require.Equal(t, engine.StateDownloading, info.State)
}

// TestDirectionFailureIsNamed pins the partial-failure shape of section
// 10.1: each direction's failure wraps its own name, so a one-sided
// failure is diagnosable.
func TestDirectionFailureIsNamed(t *testing.T) {
	f := newLimitsFake(t, func(fake *limitsFake) {
		fake.status = map[string]int{"/api/v2/transfer/setUploadLimit": http.StatusInternalServerError}
	})
	c := newLimitsClient(t, f)

	up := testUpLimit
	err := c.SetRateLimits(context.Background(), "", nil, &up)
	require.Error(t, err)
	require.Contains(t, err.Error(), "upload")

	taskFake := newLimitsFake(t, func(fake *limitsFake) {
		fake.status = map[string]int{"/api/v2/torrents/setDownloadLimit": http.StatusInternalServerError}
	})
	taskClient := newLimitsClient(t, taskFake)

	down := testDownLimit
	err = taskClient.SetRateLimits(context.Background(), engine.NameQBittorrent+":"+testHash, &down, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "download")
}

// TestPerTaskMismatchWarnsAfterThreeDeltas pins the per-task read-back:
// no request is ever issued to confirm a task limit; the cached
// dl_limit/up_limit of the deltas that follow are the confirmation, and a
// mismatch that survives three deltas logs one warn carrying both values.
// No poll runs in this fixture, so the cache keeps its seeded value and
// the mismatch survives every delta.
func TestPerTaskMismatchWarnsAfterThreeDeltas(t *testing.T) {
	f := newLimitsFake(t, nil)
	c := newLimitsClient(t, f)
	seedLimitsCache(t, c, limitsTorrentBody("downloading", testOldLimit, testUpLimit))
	c.md.pollEvery = time.Millisecond
	logs := captureWarns(t)

	down := testDownLimit
	require.NoError(t, c.SetRateLimits(context.Background(), engine.NameQBittorrent+":"+testHash, &down, nil))

	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "not confirmed")
	}, 5*time.Second, time.Millisecond)
	require.Contains(t, logs.String(), strconv.FormatInt(testDownLimit, 10))
	require.Contains(t, logs.String(), strconv.FormatInt(testOldLimit, 10))
}

// TestPerTaskCacheMatchRetiresWatcher pins the quiet paths of the
// per-task read-back: a cache that already holds the sent values retires
// the check immediately, and the delta that reports the applied value
// retires it just as well — no warn in either case.
func TestPerTaskCacheMatchRetiresWatcher(t *testing.T) {
	f := newLimitsFake(t, nil)
	c := newLimitsClient(t, f)
	seedLimitsCache(t, c, limitsTorrentBody("downloading", testDownLimit, testUpLimit))
	c.md.pollEvery = time.Millisecond
	logs := captureWarns(t)

	down, up := testDownLimit, testUpLimit
	require.NoError(t, c.SetRateLimits(context.Background(), engine.NameQBittorrent+":"+testHash, &down, &up))
	require.Never(t, func() bool {
		return strings.Contains(logs.String(), "not confirmed")
	}, 50*time.Millisecond, 5*time.Millisecond)

	// The delta that reports the applied value retires the watcher the
	// same way.
	deltaFake := newLimitsFake(t, nil)
	deltaClient := newLimitsClient(t, deltaFake)
	seedLimitsCache(t, deltaClient, limitsTorrentBody("downloading", testOldLimit, testUpLimit))
	deltaClient.md.pollEvery = time.Millisecond
	deltaLogs := captureWarns(t)

	require.NoError(t, deltaClient.SetRateLimits(context.Background(),
		engine.NameQBittorrent+":"+testHash, &down, nil))
	deltaClient.applyResponse(maindata{
		Rid:      2,
		Torrents: fullTorrents(map[string]string{testHash: limitsTorrentBody("downloading", testDownLimit, testUpLimit)}),
	}, currentEpoch(deltaClient))
	require.Never(t, func() bool {
		return strings.Contains(deltaLogs.String(), "not confirmed")
	}, 50*time.Millisecond, 5*time.Millisecond)
}

// ptr is the one-line pointer helper the override knobs take.
func ptr(v int64) *int64 { return &v }
