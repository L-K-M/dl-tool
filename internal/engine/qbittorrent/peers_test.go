package qbittorrent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/engine"
)

// capturedFullPeers and capturedDeltaPeers are the responses one live
// release-5.2.3 daemon answered for a two-daemon localhost swarm
// (docs/tasks/T035-task-peers.md Evidence): a full_update at rid=0 and
// the delta that followed it. The key names, the letter-string flags,
// the per-peer partial objects of the delta and the peers_removed array
// are the real wire shape; the values are the captured ones, trimmed to
// the two loopback peers of the pair.
const capturedFullPeers = `{
 "full_update":true,
 "peers":{
  "127.0.0.1:53941":{"client":"qBittorrent/5.2.3","connection":"μTP","country":"N/A","country_code":"","dl_speed":695525,"downloaded":4487452,"files":"payload.bin","flags":"D X L P","flags_desc":"D = Interested (local) and unchoked (peer)\nX = Peer from PEX\nL = Peer from LSD\nP = μTP","host_name":"","ip":"127.0.0.1","peer_id_client":"-qB5230-","port":53941,"progress":1,"relevance":1,"up_speed":0,"uploaded":0},
  "172.26.0.3:53941":{"client":"qBittorrent/5.2.3","connection":"μTP","country":"N/A","country_code":"","dl_speed":535645,"downloaded":4552962,"files":"payload.bin","flags":"D X L P","flags_desc":"D = Interested (local) and unchoked (peer)\nX = Peer from PEX\nL = Peer from LSD\nP = μTP","host_name":"","ip":"172.26.0.3","peer_id_client":"-qB5230-","port":53941,"progress":1,"relevance":1,"up_speed":0,"uploaded":0}},
 "rid":1,
 "show_flags":true}`

const capturedDeltaPeers = `{
 "peers":{
  "127.0.0.1:53941":{"dl_speed":903247,"downloaded":6780302},
  "172.26.0.3:53941":{"dl_speed":531294,"downloaded":6780302}},
 "rid":2}`

const capturedRemovalPeers = `{"peers_removed":["172.26.0.3:53941","127.0.0.1:53941"],"rid":3}`

// onePeerDeltaPeers is the captured delta shape carrying a single peer
// and a single changed key: the same wire form as capturedDeltaPeers,
// reduced to one entry so a test can pin that an unreported peer is
// untouched by a delta.
const onePeerDeltaPeers = `{"peers":{"127.0.0.1:53941":{"dl_speed":4096}},"rid":3}`

// capturedStrangerFull is one row of the first exploratory capture,
// where DHT had connected a real internet peer: it is the only observed
// peer whose country and country_code the GeoIP database could name, so
// it pins the non-nil spelling of the optional fields. Wrapped in the
// full-response envelope it was listed with.
const capturedStrangerFull = `{"full_update":true,"peers":{
 "113.87.163.6:16619":{"client":"","connection":"μTP","country":"China","country_code":"cn","dl_speed":0,"downloaded":0,"files":"","flags":"H P","flags_desc":"H = Peer from DHT\nP = μTP","host_name":"","ip":"113.87.163.6","peer_id_client":"","port":16619,"progress":0,"relevance":0,"up_speed":0,"uploaded":0}},"rid":1,"show_flags":true}`

// peersFake is a WebAPI stand-in for sync/torrentPeers: it serves a
// scripted sequence of response bodies for the served hash, answers 404
// for any other hash — the daemon's not-found answer for a torrent it
// does not hold — and records the rid of every call. The login and
// session rules mirror fakeServer's (docs/06 section 5.2).
type peersFake struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	rids    []int    // the rid query of every torrentPeers call
	bodies  []string // scripted responses, served in order and then repeated
	missing bool     // answer 404 for the served hash, the daemon's gone-torrent answer
	cookie  string
}

func newPeersFake(t *testing.T, bodies ...string) *peersFake {
	t.Helper()

	// ServeHTTP indexes bodies unconditionally; an empty script would
	// panic inside the server goroutine instead of failing the test.
	if len(bodies) == 0 {
		t.Fatal("peersFake requires at least one scripted response body")
	}

	f := &peersFake{t: t, bodies: bodies}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)

	return f
}

func (f *peersFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("parse form: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.URL.Path {
	case "/api/v2/auth/login":
		if r.PostForm.Get("username") != testUsername || r.PostForm.Get("password") != testPassword {
			http.Error(w, "Fails.", http.StatusUnauthorized)

			return
		}
		f.cookie = "sid-peers"
		http.SetCookie(w, &http.Cookie{Name: testCookie, Value: f.cookie, Path: "/", HttpOnly: true})
		w.WriteHeader(http.StatusNoContent)

	case "/api/v2/app/version":
		// The session probe of the client's login path; every version is
		// fine as long as it answers 200.
		_, _ = w.Write([]byte(testVersion))

	case "/api/v2/" + pathSyncTorrentPeers:
		if !f.sessionOK(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)

			return
		}

		rid, err := strconv.Atoi(r.Form.Get("rid"))
		if err != nil {
			f.t.Errorf("parse rid %q: %v", r.Form.Get("rid"), err)
		}
		f.rids = append(f.rids, rid)

		if r.Form.Get("hash") != testHash || f.missing {
			http.Error(w, "torrent not found", http.StatusNotFound)

			return
		}

		index := len(f.rids) - 1
		if index >= len(f.bodies) {
			index = len(f.bodies) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.bodies[index]))

	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}

// sessionOK enforces the cookie of the latest login.
func (f *peersFake) sessionOK(r *http.Request) bool {
	cookie, err := r.Cookie(testCookie)

	return err == nil && f.cookie != "" && cookie.Value == f.cookie
}

// setMissing toggles the daemon's not-found answer for the served hash.
func (f *peersFake) setMissing(missing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.missing = missing
}

// sentRids returns the rid of every torrentPeers call in order.
func (f *peersFake) sentRids() []int {
	f.t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]int(nil), f.rids...)
}

// newPeersClient returns a client over one fake. Connect is never
// called: the peers endpoint reaches the daemon through the lazy
// re-login of authenticated, and skipping Connect keeps the
// sync/maindata poll out of the fixture.
func newPeersClient(t *testing.T, f *peersFake) *Client {
	t.Helper()

	c, err := New(Config{BaseURL: f.srv.URL, Username: testUsername, Password: testPassword}, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, c.Close())
	})

	return c
}

// holdInCache simulates one maindata poll snapshot that holds the hash,
// the state Peers consults to decide whether a torrent left the daemon.
func holdInCache(c *Client, hash string) {
	c.md.mu.Lock()
	defer c.md.mu.Unlock()

	if c.md.cache.fields == nil {
		c.md.cache.fields = make(map[string]map[string]any)
	}
	c.md.cache.fields[hash] = map[string]any{"state": "downloading"}
}

// releaseFromCache simulates the poll noticing the torrent's removal.
func releaseFromCache(c *Client, hash string) {
	c.md.mu.Lock()
	defer c.md.mu.Unlock()

	delete(c.md.cache.fields, hash)
}

// TestPeersFullUpdate pins the full_response shape from the captured
// 5.2.3 body: the peers object's "ip:port" keys become the entries'
// addresses, the letter-string flags pass through, the "N/A" country and
// the empty-key optionals stay nil, the rates stay bytes per second, and
// the next call carries the rid the response named. A hash the daemon
// does not hold wraps engine.ErrNotFound and resets the rid.
func TestPeersFullUpdate(t *testing.T) {
	f := newPeersFake(t, capturedFullPeers, capturedFullPeers)
	c := newPeersClient(t, f)
	holdInCache(c, testHash)

	peers, err := c.Peers(context.Background(), engine.NameQBittorrent+":"+testHash)
	require.NoError(t, err)
	require.Len(t, peers, 2)

	// Sorted by address, so the listing is stable between polls.
	first, second := peers[0], peers[1]
	require.Equal(t, "127.0.0.1:53941", first.Address)
	require.Equal(t, "172.26.0.3:53941", second.Address)

	require.NotNil(t, first.Client)
	require.Equal(t, "qBittorrent/5.2.3", *first.Client)
	require.NotNil(t, first.Flags)
	require.Equal(t, "D X L P", *first.Flags)
	// "N/A" is the daemon's unknown-country sentinel, not a value.
	require.Nil(t, first.Country, "the N/A country must serialise as null")
	require.Equal(t, 1.0, first.Progress)
	require.Equal(t, int64(695525), first.DownloadRate)
	require.Equal(t, int64(0), first.UploadRate)
	require.Equal(t, int64(535645), second.DownloadRate)

	// The full response's rid rides the next request, and a second full
	// response replaces the set rather than stacking onto it.
	peers, err = c.Peers(context.Background(), testHash)
	require.NoError(t, err)
	require.Len(t, peers, 2)
	require.Equal(t, []int{0, 1}, f.sentRids())

	// A daemon that no longer holds the torrent is the foreign-task
	// answer, and it drops the session so the re-add restarts at rid=0.
	f.setMissing(true)
	_, err = c.Peers(context.Background(), testHash)
	require.ErrorIs(t, err, engine.ErrNotFound)
	f.setMissing(false)
	_, err = c.Peers(context.Background(), testHash)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1, 1, 0}, f.sentRids())
}

// TestPeersDeltaKeepsFields pins the delta merge against the captured
// 5.2.3 body: a per-peer partial object — only the changed keys —
// updates exactly those fields and leaves every other peer's fields,
// and every untouched field of the changed peer, as the full response
// left them. The stranger row of the first capture pins the non-nil
// spelling: a peer the GeoIP database names carries its country
// verbatim, while an empty client key stays nil.
func TestPeersDeltaKeepsFields(t *testing.T) {
	f := newPeersFake(t, capturedFullPeers, capturedDeltaPeers, onePeerDeltaPeers)
	c := newPeersClient(t, f)
	holdInCache(c, testHash)

	_, err := c.Peers(context.Background(), testHash)
	require.NoError(t, err)

	peers, err := c.Peers(context.Background(), testHash)
	require.NoError(t, err)
	require.Len(t, peers, 2)

	// The delta carried only dl_speed and downloaded for each peer; the
	// client, flags, country, progress and up_speed of the full response
	// survive the merge.
	first := peers[0]
	require.Equal(t, "127.0.0.1:53941", first.Address)
	require.Equal(t, int64(903247), first.DownloadRate)
	require.NotNil(t, first.Client)
	require.Equal(t, "qBittorrent/5.2.3", *first.Client)
	require.NotNil(t, first.Flags)
	require.Equal(t, "D X L P", *first.Flags)
	require.Nil(t, first.Country)
	require.Equal(t, 1.0, first.Progress)
	require.Equal(t, int64(0), first.UploadRate)

	second := peers[1]
	require.Equal(t, int64(531294), second.DownloadRate)
	require.NotNil(t, second.Client)
	require.Equal(t, "qBittorrent/5.2.3", *second.Client)
	require.Equal(t, 1.0, second.Progress)

	// A delta that reports one peer only leaves the other peer exactly
	// as the previous responses left it: nothing but the reported key of
	// the reported peer moves.
	peers, err = c.Peers(context.Background(), testHash)
	require.NoError(t, err)
	require.Len(t, peers, 2)
	require.Equal(t, int64(4096), peers[0].DownloadRate)
	require.Equal(t, int64(531294), peers[1].DownloadRate)
	require.NotNil(t, peers[1].Client)
	require.Equal(t, "qBittorrent/5.2.3", *peers[1].Client)
	require.Equal(t, 1.0, peers[1].Progress)
	require.Nil(t, peers[1].Country)

	stranger := mergeCaptured(t, capturedStrangerFull)[0]
	require.Equal(t, "113.87.163.6:16619", stranger.Address)
	require.Nil(t, stranger.Client, "an empty client key stays nil")
	require.NotNil(t, stranger.Flags)
	require.Equal(t, "H P", *stranger.Flags)
	require.NotNil(t, stranger.Country)
	require.Equal(t, "China", *stranger.Country)
	require.Equal(t, 0.0, stranger.Progress)
	require.Equal(t, int64(0), stranger.DownloadRate)
}

// mergeCaptured decodes one captured envelope and merges it into a fresh
// session, the way Peers would have listed it.
func mergeCaptured(t *testing.T, envelope string) []PeerEntry {
	t.Helper()

	var resp peersResponse
	require.NoError(t, json.Unmarshal([]byte(envelope), &resp))

	s := &peerSession{}
	s.merge(resp)

	return s.entries()
}

// TestPeersRemoval pins the removal half of the delta: a peer named by
// peers_removed disappears from the next listing, the rid advances so
// the following request carries the delta's rid, and an empty delta —
// the daemon's no-change answer — keeps the swarm empty.
func TestPeersRemoval(t *testing.T) {
	f := newPeersFake(t, capturedFullPeers, capturedDeltaPeers, capturedRemovalPeers, `{"rid":4}`)
	c := newPeersClient(t, f)
	holdInCache(c, testHash)

	peers, err := c.Peers(context.Background(), testHash)
	require.NoError(t, err)
	require.Len(t, peers, 2)

	// A delta with no removals, then the removal delta.
	_, err = c.Peers(context.Background(), testHash)
	require.NoError(t, err)
	peers, err = c.Peers(context.Background(), testHash)
	require.NoError(t, err)
	require.Empty(t, peers, "both peers were named by peers_removed")

	// The empty swarm keeps polling on the removal delta's rid.
	peers, err = c.Peers(context.Background(), testHash)
	require.NoError(t, err)
	require.Empty(t, peers, "the swarm stays empty after the removal delta")
	require.Equal(t, []int{0, 1, 2, 3}, f.sentRids())
}

// TestPeersRidResetOnReadd pins the rid reset of a torrent that left the
// maindata cache: the next Peers call starts at rid=0 and merges a full
// response in place of the stale peer set, instead of riding a rid the
// re-added torrent's daemon state may no longer match.
func TestPeersRidResetOnReadd(t *testing.T) {
	f := newPeersFake(t, capturedFullPeers, capturedStrangerFull)
	c := newPeersClient(t, f)
	holdInCache(c, testHash)

	peers, err := c.Peers(context.Background(), testHash)
	require.NoError(t, err)
	require.Len(t, peers, 2)
	require.Equal(t, []int{0}, f.sentRids())

	// The torrent leaves the maindata cache — the poll noticed the
	// removal — and is re-added: the next call sends rid=0, not the 1 a
	// kept session would have ridden, and the re-add's full response
	// replaces the stale peer set wholesale.
	releaseFromCache(c, testHash)
	peers, err = c.Peers(context.Background(), testHash)
	require.NoError(t, err)
	require.Len(t, peers, 1, "the re-add's full response replaced the stale set")
	require.Equal(t, "113.87.163.6:16619", peers[0].Address)
	require.Equal(t, []int{0, 0}, f.sentRids())
}
