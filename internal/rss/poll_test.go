package rss

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

var testNow = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

// stubParser is the ItemParser stand-in T067 replaces with parse.go: it
// returns canned meta and items, or a canned error.
type stubParser struct {
	meta  FeedMeta
	items []store.FeedItem
	err   error
}

func (s stubParser) ParseFeed(feedID, _ string, _ []byte) (FeedMeta, []store.FeedItem, error) {
	items := make([]store.FeedItem, len(s.items))
	for i, item := range s.items {
		if item.FeedID == "" {
			item.FeedID = feedID
		}
		items[i] = item
	}

	return s.meta, items, s.err
}

func newTestDB(t *testing.T) *sqlx.DB {
	t.Helper()

	dir := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(dir, "dl-tool.db"), filepath.Join(dir, "backups"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	return db
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newFeed inserts one enabled feed pointing at rawURL, due immediately.
func newFeed(t *testing.T, db *sqlx.DB, rawURL string) store.Feed {
	t.Helper()

	feed := store.Feed{
		ID:          store.NewID(store.PrefixFeed),
		URL:         rawURL,
		Enabled:     true,
		ItemCap:     50,
		NextFetchAt: testNow.UnixMilli(),
	}
	require.NoError(t, store.CreateFeed(t.Context(), db, feed))

	return feed
}

func feedByID(t *testing.T, db *sqlx.DB, id string) store.Feed {
	t.Helper()

	feed, err := store.FeedByID(t.Context(), db, id)
	require.NoError(t, err)

	return feed
}

// testPoller builds a Poller wired like production — the shared SSRF guard
// with the private-range and origin-port lifts the loopback test server
// needs — but with a fixed clock and a startedAt already outside the
// startup grace so the ladder's disabled_till writes are observable.
func testPoller(t *testing.T, db *sqlx.DB, srv *httptest.Server, parser ItemParser) *Poller {
	t.Helper()

	return testPollerClock(t, db, srv, parser, func() time.Time { return testNow })
}

// testPollerClock is testPoller over an injected clock, for tests that
// advance time past a disabled_till between polls.
func testPollerClock(t *testing.T, db *sqlx.DB, srv *httptest.Server, parser ItemParser, now func() time.Time) *Poller {
	t.Helper()

	origin, err := url.Parse(srv.URL)
	require.NoError(t, err)

	client := secure.NewClient(secure.NewGuard(discardLog(), true).ForOrigin(origin))
	// Fail fast rather than ride the shared client's 120 s budget.
	client.Timeout = 10 * time.Second

	p := NewPoller(db, client, parser, nil, discardLog(), now)
	p.startedAt = testNow.Add(-time.Hour)

	return p
}

func feedItem(identity, title string) store.FeedItem {
	return store.FeedItem{Identity: identity, Title: title, TitleNorm: title}
}

func itemCount(t *testing.T, db *sqlx.DB, feedID string) int {
	t.Helper()

	var n int
	require.NoError(t, db.GetContext(t.Context(), &n,
		`SELECT COUNT(*) FROM feed_items WHERE feed_id = ?`, feedID))

	return n
}

// TestPollStoresItemsAndValidators: a 200 upserts the parsed items and lands
// both validators verbatim on the feed row.
func TestPollStoresItemsAndValidators(t *testing.T) {
	db := newTestDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `W/"6a953dfc-9443"`)
		w.Header().Set("Last-Modified", "Mon, 31 Aug 2026 08:40:28 GMT")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	feed := newFeed(t, db, srv.URL+"/rss.xml")
	poller := testPoller(t, db, srv, stubParser{items: []store.FeedItem{
		feedItem("guid-1", "First"),
		feedItem("guid-2", "Second"),
	}})

	res, err := poller.Poll(t.Context(), feed, false)
	require.NoError(t, err)
	require.True(t, res.Fetched)
	require.False(t, res.NotModified)
	require.Equal(t, 2, res.ItemsAdded)
	require.Empty(t, res.Error)

	stored := feedByID(t, db, feed.ID)
	require.NotNil(t, stored.ETag)
	require.Equal(t, `W/"6a953dfc-9443"`, *stored.ETag)
	require.NotNil(t, stored.LastModified)
	require.Equal(t, "Mon, 31 Aug 2026 08:40:28 GMT", *stored.LastModified)
	require.NotNil(t, stored.LastFetchAt)
	require.NotNil(t, stored.LastSuccessAt)
	require.Nil(t, stored.LastError)
	require.Equal(t, 2, itemCount(t, db, feed.ID))
}

// TestConditionalGetSendsBothValidators: the second poll replays the stored
// ETag and Last-Modified verbatim as If-None-Match and If-Modified-Since.
func TestConditionalGetSendsBothValidators(t *testing.T) {
	db := newTestDB(t)

	var gotETag, gotModified atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotETag.Store(r.Header.Get("If-None-Match"))
		gotModified.Store(r.Header.Get("If-Modified-Since"))
		w.Header().Set("ETag", `"abc123"`)
		w.Header().Set("Last-Modified", "Mon, 31 Aug 2026 08:40:28 GMT")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	feed := newFeed(t, db, srv.URL+"/feed")
	poller := testPoller(t, db, srv, stubParser{})

	_, err := poller.Poll(t.Context(), feed, false)
	require.NoError(t, err)
	require.Empty(t, gotETag.Load().(string))
	require.Empty(t, gotModified.Load().(string))

	_, err = poller.Poll(t.Context(), feedByID(t, db, feed.ID), false)
	require.NoError(t, err)
	require.Equal(t, `"abc123"`, gotETag.Load().(string))
	require.Equal(t, "Mon, 31 Aug 2026 08:40:28 GMT", gotModified.Load().(string))
}

// TestNotModifiedAddsNoItems: a 304 is a success — the ladder steps down,
// timestamps and reschedule land — but nothing is parsed or stored.
func TestNotModifiedAddsNoItems(t *testing.T) {
	db := newTestDB(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	feed := newFeed(t, db, srv.URL+"/feed")
	poller := testPoller(t, db, srv, stubParser{items: []store.FeedItem{feedItem("i1", "One")}})

	res, err := poller.Poll(t.Context(), feed, false)
	require.NoError(t, err)
	require.Equal(t, 1, res.ItemsAdded)

	res, err = poller.Poll(t.Context(), feedByID(t, db, feed.ID), false)
	require.NoError(t, err)
	require.True(t, res.Fetched)
	require.True(t, res.NotModified)
	require.Equal(t, 0, res.ItemsAdded)
	require.Equal(t, int32(2), calls.Load())
	require.Equal(t, 1, itemCount(t, db, feed.ID), "a 304 must not store items")

	stored := feedByID(t, db, feed.ID)
	require.NotNil(t, stored.ETag, "a 304 must not clear the stored ETag")
	require.Equal(t, `"v1"`, *stored.ETag)
}

// TestBackoffLadderEscalatesAndDecrements: three consecutive 500s walk the
// Sonarr ladder to levels 1, 2, 3 with disabled_till deltas of exactly 60,
// 300 and 900 seconds; a success then steps the level down to 2 — never reset.
func TestBackoffLadderEscalatesAndDecrements(t *testing.T) {
	db := newTestDB(t)

	var status atomic.Int32
	status.Store(http.StatusInternalServerError)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()

	feed := newFeed(t, db, srv.URL+"/feed")

	current := testNow
	poller := testPollerClock(t, db, srv, stubParser{}, func() time.Time { return current })

	wantDisabled := []int64{60, 300, 900}
	for level, delta := range wantDisabled {
		res, err := poller.Poll(t.Context(), feedByID(t, db, feed.ID), false)
		require.NoError(t, err)
		require.NotEmpty(t, res.Error)

		stored := feedByID(t, db, feed.ID)
		require.Equal(t, level+1, stored.EscalationLevel)
		require.NotNil(t, stored.DisabledTill)
		require.Equal(t, current.UnixMilli()+delta*1000, *stored.DisabledTill)
		require.Equal(t, *stored.DisabledTill, stored.NextFetchAt)
		require.NotNil(t, stored.LastError)

		// Advance to the moment the feed is allowed again for the next step.
		current = time.UnixMilli(*stored.DisabledTill)
	}

	status.Store(http.StatusOK)
	res, err := poller.Poll(t.Context(), feedByID(t, db, feed.ID), false)
	require.NoError(t, err)
	require.Empty(t, res.Error)

	stored := feedByID(t, db, feed.ID)
	require.Equal(t, 2, stored.EscalationLevel, "success decrements by one, never resets")
	require.Nil(t, stored.DisabledTill)
	require.Nil(t, stored.LastError)
}

// TestScheduledPollRespectsDisabledTill: without force, a feed inside its
// disabled_till window is skipped before any request leaves.
func TestScheduledPollRespectsDisabledTill(t *testing.T) {
	db := newTestDB(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	feed := newFeed(t, db, srv.URL+"/feed")
	disabled := testNow.Add(time.Hour).UnixMilli()
	feed.DisabledTill = &disabled
	feed.LastFetchAt = nil
	require.NoError(t, store.UpdateFeedFetchState(t.Context(), db, feed))

	poller := testPoller(t, db, srv, stubParser{})
	res, err := poller.Poll(t.Context(), feedByID(t, db, feed.ID), false)
	require.NoError(t, err)
	require.False(t, res.Fetched)
	require.Equal(t, int32(0), calls.Load())
}

// TestRefreshPollsDisabledFeed: force — the POST /feeds/{id}/refresh path —
// polls a feed whose disabled_till is in the future and clears the state on
// success.
func TestRefreshPollsDisabledFeed(t *testing.T) {
	db := newTestDB(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	feed := newFeed(t, db, srv.URL+"/feed")
	disabled := testNow.Add(time.Hour).UnixMilli()
	feed.DisabledTill = &disabled
	feed.EscalationLevel = 5
	require.NoError(t, store.UpdateFeedFetchState(t.Context(), db, feed))

	poller := testPoller(t, db, srv, stubParser{})
	res, err := poller.Poll(t.Context(), feedByID(t, db, feed.ID), true)
	require.NoError(t, err)
	require.True(t, res.Fetched)
	require.Equal(t, int32(1), calls.Load())

	stored := feedByID(t, db, feed.ID)
	require.Equal(t, 4, stored.EscalationLevel)
	require.Nil(t, stored.DisabledTill)
}

// TestBodyCapRejectsOversizeFeed: a response declaring a 17 MiB
// Content-Length fails the poll before the body is read, and no partial item
// is stored.
func TestBodyCapRejectsOversizeFeed(t *testing.T) {
	db := newTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(17<<20))
		w.WriteHeader(http.StatusOK)
		// Far less than the declaration: the client must reject on the
		// declared length without draining a byte more than it needs to.
		if _, err := w.Write([]byte("<rss")); err != nil {
			return
		}
	}))
	defer srv.Close()

	feed := newFeed(t, db, srv.URL+"/feed")
	poller := testPoller(t, db, srv, stubParser{items: []store.FeedItem{feedItem("i1", "One")}})

	res, err := poller.Poll(t.Context(), feed, false)
	require.NoError(t, err)
	require.Equal(t, "feed body exceeds 16 MiB", res.Error)
	require.Equal(t, 0, itemCount(t, db, feed.ID))

	stored := feedByID(t, db, feed.ID)
	require.Equal(t, 1, stored.EscalationLevel)
	require.NotNil(t, stored.LastError)
	require.Equal(t, "feed body exceeds 16 MiB", *stored.LastError)
}

// TestBodyCapRejectsLyingStream: a chunked 17 MiB body with no declared
// length trips the streaming cap all the same.
func TestBodyCapRejectsLyingStream(t *testing.T) {
	db := newTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Del("Content-Length")
		w.WriteHeader(http.StatusOK)
		chunk := make([]byte, 1<<20)
		for range 17 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer srv.Close()

	feed := newFeed(t, db, srv.URL+"/feed")
	poller := testPoller(t, db, srv, stubParser{})

	res, err := poller.Poll(t.Context(), feed, false)
	require.NoError(t, err)
	require.Equal(t, "feed body exceeds 16 MiB", res.Error)
	require.Equal(t, 0, itemCount(t, db, feed.ID))
}

// TestJitterIsDeterministicAndBounded: the jitter is stable per feed id and
// always inside [-0.10, +0.10].
func TestJitterIsDeterministicAndBounded(t *testing.T) {
	for _, id := range []string{"fed_01", "fed_02", "a", "", "fed_01JKQ7XYZ"} {
		require.Equal(t, Jitter(id), Jitter(id))
	}
	for i := range 5000 {
		id := "fed_" + strconv.Itoa(i)
		j := Jitter(id)
		require.GreaterOrEqual(t, j, -0.10, "feed %s", id)
		require.LessOrEqual(t, j, 0.10, "feed %s", id)
	}
}

// TestEffectiveIntervalFloor: the effective interval never drops under the
// 300-second floor, and the publisher hints can only push it up.
func TestEffectiveIntervalFloor(t *testing.T) {
	require.Equal(t, 300*time.Second, EffectiveInterval(0, 0, FeedMeta{}))
	require.Equal(t, 300*time.Second, EffectiveInterval(60, 120, FeedMeta{}))
	require.Equal(t, 1800*time.Second, EffectiveInterval(0, 1800, FeedMeta{}))
	require.Equal(t, 3600*time.Second, EffectiveInterval(0, 300, FeedMeta{TTLMinutes: 60}))
	require.Equal(t, 7200*time.Second, EffectiveInterval(600, 300, FeedMeta{ImpliedIntervalS: 7200}))
}

// TestNextFetchAtAppliesJitter: the reschedule lands inside the ±10 % band
// of the effective interval.
func TestNextFetchAtAppliesJitter(t *testing.T) {
	now := testNow.UnixMilli()
	d := 30 * time.Minute
	next := NextFetchAt(now, d, "fed_any")
	require.GreaterOrEqual(t, next, now+int64(27*time.Minute/time.Millisecond))
	require.LessOrEqual(t, next, now+int64(33*time.Minute/time.Millisecond))
}

// TestSkipHoursReschedules: a candidate landing inside a skipped GMT hour is
// pushed to the next allowed hour; a skipped weekday pushes to the next
// allowed day's first hour.
func TestSkipHoursReschedules(t *testing.T) {
	require.Equal(t, time.Saturday, testNow.Weekday(), "SkipDays expectations assume a Saturday")
	require.Equal(t, 12, testNow.Hour(), "SkipHours expectations assume 12:00 UTC")
	base := testNow.UnixMilli() // 12:00 UTC Saturday

	meta := FeedMeta{SkipHours: []int{13}}
	require.Equal(t, base, nextAllowed(base, meta))
	require.Equal(t,
		testNow.Add(2*time.Hour).UnixMilli(),
		nextAllowed(testNow.Add(time.Hour).UnixMilli(), meta))

	// Saturday and Sunday skipped: advancing hour by hour first lands on
	// Monday 00:00 — 36 hours out — which is allowed.
	days := FeedMeta{SkipDays: []time.Weekday{time.Saturday, time.Sunday}}
	require.Equal(t,
		testNow.Add(36*time.Hour).UnixMilli(),
		nextAllowed(base, days))
}

// TestStartupGraceKeepsPolling: inside the 15-minute startup grace a failure
// escalates the level but writes no disabled_till — the feed keeps polling
// at its normal interval.
func TestStartupGraceKeepsPolling(t *testing.T) {
	db := newTestDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	feed := newFeed(t, db, srv.URL+"/feed")

	origin, err := url.Parse(srv.URL)
	require.NoError(t, err)
	poller := NewPoller(db, secure.NewClient(secure.NewGuard(discardLog(), true).ForOrigin(origin)),
		stubParser{}, nil, discardLog(), func() time.Time { return testNow })
	// startedAt defaults to now(): inside the grace.

	res, err := poller.Poll(t.Context(), feed, false)
	require.NoError(t, err)
	require.NotEmpty(t, res.Error)

	stored := feedByID(t, db, feed.ID)
	require.Equal(t, 1, stored.EscalationLevel)
	require.Nil(t, stored.DisabledTill)
	require.Greater(t, stored.NextFetchAt, testNow.UnixMilli())
}

// TestPollDuePollsOnlyDueFeeds: the job handler polls every due feed and
// leaves disabled or not-yet-due feeds untouched.
func TestPollDuePollsOnlyDueFeeds(t *testing.T) {
	db := newTestDB(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	due := newFeed(t, db, srv.URL+"/due")
	second := newFeed(t, db, srv.URL+"/second")

	notDue := newFeed(t, db, srv.URL+"/later")
	later := testNow.Add(time.Hour).UnixMilli()
	_, err := db.ExecContext(t.Context(),
		`UPDATE feeds SET next_fetch_at = ? WHERE id = ?`, later, notDue.ID)
	require.NoError(t, err)

	poller := testPoller(t, db, srv, stubParser{})
	require.NoError(t, poller.PollDue(t.Context(), store.Job{Kind: "rss_poll"}))
	require.Equal(t, int32(2), calls.Load())
	require.NotNil(t, feedByID(t, db, due.ID).LastFetchAt)
	require.NotNil(t, feedByID(t, db, second.ID).LastFetchAt)
	require.Nil(t, feedByID(t, db, notDue.ID).LastFetchAt)
}

// TestPollDueSerialisesPerHost is implied by the map design; what is worth a
// regression pin is that a failed feed does not stop its host's queue or the
// other hosts.
func TestPollDueContinuesPastFailures(t *testing.T) {
	db := newTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/good" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	bad := newFeed(t, db, srv.URL+"/bad")
	good := newFeed(t, db, srv.URL+"/good")

	poller := testPoller(t, db, srv, stubParser{})
	require.NoError(t, poller.PollDue(t.Context(), store.Job{Kind: "rss_poll"}))
	require.Equal(t, 1, feedByID(t, db, bad.ID).EscalationLevel)
	require.NotNil(t, feedByID(t, db, good.ID).LastSuccessAt)
}

// TestConcurrentPollSameFeedSkipped: while one poll of a feed is in
// flight — regardless of which poller instance holds it — a second Poll of
// the same feed fetches nothing. A forced refresh reports the conflict;
// a scheduled poll skips quietly.
func TestConcurrentPollSameFeedSkipped(t *testing.T) {
	db := newTestDB(t)

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer func() {
		// A failed require Goexits before the explicit close below;
		// without this guard srv.Close() would block forever on the
		// handler's <-release.
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	feed := newFeed(t, db, srv.URL+"/feed")
	poller := testPoller(t, db, srv, stubParser{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = poller.Poll(t.Context(), feed, false)
	}()
	// Wait until the first poll holds the claim, then race it.
	require.Eventually(t, func() bool {
		_, busy := inFlightPolls.Load(feed.ID)
		return busy
	}, 2*time.Second, time.Millisecond)

	res, err := poller.Poll(t.Context(), feedByID(t, db, feed.ID), true)
	require.NoError(t, err)
	require.False(t, res.Fetched)
	require.Contains(t, res.Error, "already in progress")

	close(release)
	<-done
}

// TestResultIsTheRefreshBody pins the doc 05 section 10.1 refresh shape: a
// fetch failure still produces a populated Result with error set.
func TestResultIsTheRefreshBody(t *testing.T) {
	db := newTestDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	feed := newFeed(t, db, srv.URL+"/feed")
	poller := testPoller(t, db, srv, stubParser{})

	res, err := poller.Poll(t.Context(), feed, true)
	require.NoError(t, err)
	require.True(t, res.Fetched)
	require.Contains(t, res.Error, "HTTP 500")
}
