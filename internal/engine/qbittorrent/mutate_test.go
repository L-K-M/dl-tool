package qbittorrent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/engine"
)

// mutateRequest is one captured call of the mutation endpoints.
type mutateRequest struct {
	Method string
	Path   string
	Form   url.Values
}

// mutateFake is a WebAPI stand-in for the six endpoints of T036: it
// answers every mutation with a configurable status and records each
// call. The login and session rules mirror fakeServer's (docs/06
// section 5.2).
type mutateFake struct {
	t   *testing.T
	srv *httptest.Server

	mu     sync.Mutex
	calls  []mutateRequest
	status map[string]int // answered per path; 0 means 200
	cookie string
}

func newMutateFake(t *testing.T, tune func(*mutateFake)) *mutateFake {
	t.Helper()

	f := &mutateFake{t: t}
	if tune != nil {
		tune(f)
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)

	return f
}

func (f *mutateFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		f.cookie = "sid-mutate"
		http.SetCookie(w, &http.Cookie{Name: testCookie, Value: f.cookie, Path: "/", HttpOnly: true})
		w.WriteHeader(http.StatusNoContent)

	case "/api/v2/app/version":
		if !f.sessionOK(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)

			return
		}
		_, _ = w.Write([]byte(testVersion))

	case "/api/v2/torrents/setShareLimits",
		"/api/v2/torrents/setLocation",
		"/api/v2/torrents/setCategory",
		"/api/v2/torrents/rename",
		"/api/v2/torrents/addTags",
		"/api/v2/torrents/removeTags",
		"/api/v2/torrents/toggleSequentialDownload":
		if !f.sessionOK(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)

			return
		}
		record()
		status := f.status[r.URL.Path]
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)

	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}

// sessionOK enforces the cookie of the latest login.
func (f *mutateFake) sessionOK(r *http.Request) bool {
	cookie, err := r.Cookie(testCookie)

	return err == nil && f.cookie != "" && cookie.Value == f.cookie
}

// callsTo returns every recorded call of one endpoint, in order.
func (f *mutateFake) callsTo(path string) []mutateRequest {
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

// newMutateClient returns a client over one fake. Connect is never
// called: the mutations reach the daemon through the lazy re-login of
// authenticated, and skipping Connect keeps the sync/maindata poll out of
// the fixture.
func newMutateClient(t *testing.T, f *mutateFake) *Client {
	t.Helper()

	c, err := New(Config{BaseURL: f.srv.URL, Username: testUsername, Password: testPassword}, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, c.Close())
	})

	return c
}

// seedMutateCache publishes one full maindata snapshot over the client's
// cache, so the cache-backed guards (seq_dl, tags) hold a current value
// without a running poll.
func seedMutateCache(t *testing.T, c *Client, body string) {
	t.Helper()

	c.SetOwnershipFilter(staticSource(setOf(testHash)))
	c.applyResponse(maindata{
		Rid:        1,
		FullUpdate: true,
		Torrents:   fullTorrents(map[string]string{testHash: body}),
	}, currentEpoch(c))
}

// mutateTorrentBody is one maindata torrent object carrying the two
// fields the cache-backed guards read: tags and seq_dl.
func mutateTorrentBody(tags string, seqDl bool) string {
	return `{"hash":"` + testHash + `","state":"downloading","tags":` +
		strconv.Quote(tags) + `,"seq_dl":` + strconv.FormatBool(seqDl) + `}`
}

// TestShareLimitsSendsBoth pins the OR semantics of FR-019: both limits
// ride one call, in their own units, beside the two constants dl-tool
// does not expose.
func TestShareLimitsSendsBoth(t *testing.T) {
	f := newMutateFake(t, nil)
	c := newMutateClient(t, f)

	ratio := 2.5
	seed := int64(3600) // one hour, in dl-tool's stored seconds
	require.NoError(t, c.SetShareLimits(context.Background(), engine.NameQBittorrent+":"+testHash, &ratio, &seed))

	calls := f.callsTo(pathSetShareLimits)
	require.Len(t, calls, 1)
	require.Equal(t, http.MethodPost, calls[0].Method)
	require.Equal(t, url.Values{
		"hashes":                   {testHash},
		"ratioLimit":               {"2.5"},
		"seedingTimeLimit":         {"60"},
		"inactiveSeedingTimeLimit": {shareLimitUseGlobal},
		"shareLimitAction":         {shareLimitActionDefault},
	}, calls[0].Form)
}

// TestNilLimitsSendGlobalSentinel pins the sentinel encoding: nil on
// either limit becomes the observed use-the-global sentinel, and an
// explicit 0 stays 0 — stop as soon as the daemon's check runs.
func TestNilLimitsSendGlobalSentinel(t *testing.T) {
	f := newMutateFake(t, nil)
	c := newMutateClient(t, f)

	require.NoError(t, c.SetShareLimits(context.Background(), engine.NameQBittorrent+":"+testHash, nil, nil))
	calls := f.callsTo(pathSetShareLimits)
	require.Len(t, calls, 1)
	require.Equal(t, shareLimitUseGlobal, calls[0].Form.Get("ratioLimit"))
	require.Equal(t, shareLimitUseGlobal, calls[0].Form.Get("seedingTimeLimit"))

	zeroRatio := 0.0
	zeroSeed := int64(0)
	require.NoError(t, c.SetShareLimits(context.Background(), engine.NameQBittorrent+":"+testHash, &zeroRatio, &zeroSeed))
	calls = f.callsTo(pathSetShareLimits)
	require.Len(t, calls, 2)
	require.Equal(t, "0", calls[1].Form.Get("ratioLimit"))
	require.Equal(t, "0", calls[1].Form.Get("seedingTimeLimit"))
}

// TestSeedTimeRoundsUpToMinutes pins the one conversion point: seconds in,
// minutes out, rounded up — 90 s is 2 min, not 1.
func TestSeedTimeRoundsUpToMinutes(t *testing.T) {
	require.Equal(t, int64(0), secondsToSeedMinutes(0))
	require.Equal(t, int64(1), secondsToSeedMinutes(1))
	require.Equal(t, int64(1), secondsToSeedMinutes(60))
	require.Equal(t, int64(2), secondsToSeedMinutes(61))
	require.Equal(t, int64(2), secondsToSeedMinutes(90))
	require.Equal(t, int64(2), secondsToSeedMinutes(119))
	require.Equal(t, int64(120), secondsToSeedMinutes(7199))

	f := newMutateFake(t, nil)
	c := newMutateClient(t, f)

	seed := int64(90)
	require.NoError(t, c.SetShareLimits(context.Background(), engine.NameQBittorrent+":"+testHash, nil, &seed))
	calls := f.callsTo(pathSetShareLimits)
	require.Len(t, calls, 1)
	require.Equal(t, "2", calls[0].Form.Get("seedingTimeLimit"))
}

// TestSequentialToggleGuard pins the read-then-toggle rule: the endpoint
// flips, so it is posted only when the cached seq_dl differs, a hash the
// cache does not hold errors instead of toggling blind, and a successful
// toggle writes the requested value back so a retry cannot flip the
// torrent to the opposite of what was asked.
func TestSequentialToggleGuard(t *testing.T) {
	f := newMutateFake(t, nil)
	c := newMutateClient(t, f)
	seedMutateCache(t, c, mutateTorrentBody("", false))

	require.NoError(t, c.SetSequential(context.Background(), engine.NameQBittorrent+":"+testHash, true))
	calls := f.callsTo(pathToggleSeq)
	require.Len(t, calls, 1)
	require.Equal(t, url.Values{"hashes": {testHash}}, calls[0].Form)

	// A repeat of the same request inside one poll interval must not
	// toggle again: the cache now holds the applied value.
	require.NoError(t, c.SetSequential(context.Background(), engine.NameQBittorrent+":"+testHash, true))
	require.Len(t, f.callsTo(pathToggleSeq), 1, "a repeat of the same value must not toggle")

	// The opposite request does toggle, and lands the new value in the
	// cache the same way.
	require.NoError(t, c.SetSequential(context.Background(), engine.NameQBittorrent+":"+testHash, false))
	calls = f.callsTo(pathToggleSeq)
	require.Len(t, calls, 2)
	require.NoError(t, c.SetSequential(context.Background(), engine.NameQBittorrent+":"+testHash, false))
	require.Len(t, f.callsTo(pathToggleSeq), 2, "the written-back false must hold")

	// A hash the cache does not hold: nothing safe to post.
	unknown := newMutateClient(t, f)
	err := unknown.SetSequential(context.Background(), engine.NameQBittorrent+":"+otherHash, true)
	require.ErrorIs(t, err, engine.ErrNotFound)
	require.Len(t, f.callsTo(pathToggleSeq), 2)
}

// TestSetLocationForm pins torrents/setLocation: hashes plus the
// already-jailed path, verbatim, never joined or cleaned here.
func TestSetLocationForm(t *testing.T) {
	f := newMutateFake(t, nil)
	c := newMutateClient(t, f)

	require.NoError(t, c.SetLocation(context.Background(), engine.NameQBittorrent+":"+testHash, "/data/linux"))
	calls := f.callsTo(pathSetLocation)
	require.Len(t, calls, 1)
	require.Equal(t, url.Values{"hashes": {testHash}, "location": {"/data/linux"}}, calls[0].Form)
}

// TestSetCategoryForm pins torrents/setCategory, including the empty
// category that clears the assignment.
func TestSetCategoryForm(t *testing.T) {
	f := newMutateFake(t, nil)
	c := newMutateClient(t, f)

	require.NoError(t, c.SetCategory(context.Background(), engine.NameQBittorrent+":"+testHash, "linux"))
	require.NoError(t, c.SetCategory(context.Background(), engine.NameQBittorrent+":"+testHash, ""))

	calls := f.callsTo(pathSetCategory)
	require.Len(t, calls, 2)
	require.Equal(t, url.Values{"hashes": {testHash}, "category": {"linux"}}, calls[0].Form)
	require.Equal(t, url.Values{"hashes": {testHash}, "category": {""}}, calls[1].Form)
}

// TestRenameForm pins torrents/rename: the singular hash form and the
// 404 of a hash the daemon does not hold, mapped onto engine.ErrNotFound.
func TestRenameForm(t *testing.T) {
	f := newMutateFake(t, nil)
	c := newMutateClient(t, f)

	require.NoError(t, c.Rename(context.Background(), engine.NameQBittorrent+":"+testHash, "renamed fixture"))
	calls := f.callsTo(pathRename)
	require.Len(t, calls, 1)
	require.Equal(t, url.Values{"hash": {testHash}, "name": {"renamed fixture"}}, calls[0].Form)

	notHeld := newMutateFake(t, func(fake *mutateFake) {
		fake.status = map[string]int{"/api/v2/torrents/rename": http.StatusNotFound}
	})
	notHeldClient := newMutateClient(t, notHeld)
	err := notHeldClient.Rename(context.Background(), engine.NameQBittorrent+":"+testHash, "anything")
	require.ErrorIs(t, err, engine.ErrNotFound)
}

// TestSetTagsDiff pins the two-sided diff: drops go out as one
// removeTags, additions as one addTags, an unchanged set issues nothing,
// and clearing every tag sends only the remove.
func TestSetTagsDiff(t *testing.T) {
	f := newMutateFake(t, nil)
	c := newMutateClient(t, f)
	seedMutateCache(t, c, mutateTorrentBody("old,keep", false))

	require.NoError(t, c.SetTags(context.Background(), engine.NameQBittorrent+":"+testHash, []string{"keep", "new"}))
	drops := f.callsTo(pathRemoveTags)
	require.Len(t, drops, 1)
	require.Equal(t, url.Values{"hashes": {testHash}, "tags": {"old"}}, drops[0].Form)
	adds := f.callsTo(pathAddTags)
	require.Len(t, adds, 1)
	require.Equal(t, url.Values{"hashes": {testHash}, "tags": {"new"}}, adds[0].Form)

	// An unchanged set: no request on either side.
	same := newMutateClient(t, f)
	seedMutateCache(t, same, mutateTorrentBody("keep", false))
	require.NoError(t, same.SetTags(context.Background(), engine.NameQBittorrent+":"+testHash, []string{"keep"}))
	require.Len(t, f.callsTo(pathRemoveTags), 1)
	require.Len(t, f.callsTo(pathAddTags), 1)

	// Clearing every tag is the remove side alone — never an empty-value
	// removeTags, which the daemon reads as "remove all" only because the
	// value names nothing, but which would also ride an unneeded call.
	clearing := newMutateClient(t, f)
	seedMutateCache(t, clearing, mutateTorrentBody("old,keep", false))
	require.NoError(t, clearing.SetTags(context.Background(), engine.NameQBittorrent+":"+testHash, nil))
	require.Len(t, f.callsTo(pathRemoveTags), 2)
	require.Equal(t, "old,keep", f.callsTo(pathRemoveTags)[1].Form.Get("tags"))
	require.Len(t, f.callsTo(pathAddTags), 1)

	// A hash the cache does not hold is one the daemon does not know.
	unknown := newMutateClient(t, f)
	err := unknown.SetTags(context.Background(), engine.NameQBittorrent+":"+otherHash, []string{"new"})
	require.ErrorIs(t, err, engine.ErrNotFound)
	require.Len(t, f.callsTo(pathRemoveTags), 2)
	require.Len(t, f.callsTo(pathAddTags), 1)
}

// TestSetTagsWriteback pins the cache write-back: the applied set is what
// the next diff inside one poll interval sees, and a failed add side
// leaves the cache stale so a retry recomputes the same diff.
func TestSetTagsWriteback(t *testing.T) {
	f := newMutateFake(t, nil)
	c := newMutateClient(t, f)
	seedMutateCache(t, c, mutateTorrentBody("", false))

	// a, then b, inside one poll interval: the second diff must run
	// against the applied {a}, so it drops a and adds b — not a silent
	// union {a, b}.
	require.NoError(t, c.SetTags(context.Background(), engine.NameQBittorrent+":"+testHash, []string{"a"}))
	require.NoError(t, c.SetTags(context.Background(), engine.NameQBittorrent+":"+testHash, []string{"b"}))
	drops := f.callsTo(pathRemoveTags)
	require.Len(t, drops, 1)
	require.Equal(t, "a", drops[0].Form.Get("tags"))
	adds := f.callsTo(pathAddTags)
	require.Len(t, adds, 2)
	require.Equal(t, "a", adds[0].Form.Get("tags"))
	require.Equal(t, "b", adds[1].Form.Get("tags"))

	// A repeat of the applied set issues nothing on either side.
	require.NoError(t, c.SetTags(context.Background(), engine.NameQBittorrent+":"+testHash, []string{"b"}))
	require.Len(t, f.callsTo(pathRemoveTags), 1)
	require.Len(t, f.callsTo(pathAddTags), 2)

	// A failed add leaves the cache at the last fully applied set: the
	// retry recomputes the same diff and converges.
	failing := newMutateFake(t, func(fake *mutateFake) {
		fake.status = map[string]int{"/api/v2/torrents/addTags": http.StatusInternalServerError}
	})
	failingClient := newMutateClient(t, failing)
	seedMutateCache(t, failingClient, mutateTorrentBody("", false))
	err := failingClient.SetTags(context.Background(), engine.NameQBittorrent+":"+testHash, []string{"x"})
	require.Error(t, err)
	failedAdds := failing.callsTo(pathAddTags)
	require.Len(t, failedAdds, 1)

	failing.status["/api/v2/torrents/addTags"] = 0
	require.NoError(t, failingClient.SetTags(context.Background(), engine.NameQBittorrent+":"+testHash, []string{"x"}))
	require.Len(t, failing.callsTo(pathRemoveTags), 0)
	require.Len(t, failing.callsTo(pathAddTags), 2)
}

// TestShareLimitsNotFoundMap pins the 404 mapping of the hashes-carried
// mutations, using setShareLimits as the stand-in.
func TestShareLimitsNotFoundMap(t *testing.T) {
	f := newMutateFake(t, func(fake *mutateFake) {
		fake.status = map[string]int{"/api/v2/torrents/setShareLimits": http.StatusNotFound}
	})
	c := newMutateClient(t, f)

	ratio := 1.0
	err := c.SetShareLimits(context.Background(), engine.NameQBittorrent+":"+testHash, &ratio, nil)
	require.ErrorIs(t, err, engine.ErrNotFound)
}
