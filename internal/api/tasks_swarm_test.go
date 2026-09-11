package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/engine/qbittorrent"
	"github.com/L-K-M/dl-tool/internal/store"
)

// swarmRequest is one captured tracker call of the wire fake.
type swarmRequest struct {
	Method string
	Path   string
	Form   url.Values
}

// The hash the wire fake serves; a task seeded with any other ref plays
// the engine's foreign-task answer.
const swarmHash = qbtHash

// capturedTrackersJSON is the trackers listing one live release-5.2.3
// daemon answered for a one-tracker fixture torrent (docs/tasks/
// T034-task-trackers.md Evidence): three synthetic rows — trimmed here to
// DHT — and the never-contacted real row, plus a working row with the
// counts the daemon reports once a tracker answers. The key names, the
// numeric status and the -1 unknown counts are the real wire shape.
const capturedTrackersJSON = `[
{"msg":"","num_downloaded":0,"num_leeches":0,"num_peers":0,"num_seeds":0,"status":2,"tier":-1,"url":"** [DHT] **"},
{"endpoints":[],"min_announce":0,"msg":"","next_announce":0,"num_downloaded":-1,"num_leeches":-1,"num_peers":-1,"num_seeds":-1,"status":1,"tier":0,"updating":false,"url":"udp://tracker.example.org:6969/announce"},
{"endpoints":[],"min_announce":77,"msg":"\"tracker\" was working","next_announce":1420,"num_downloaded":-1,"num_leeches":-1,"num_peers":118,"num_seeds":412,"status":2,"tier":0,"updating":false,"url":"http://9.9.9.9/announce"}]`

// capturedPeersJSON is the sync/torrentPeers full response one live
// release-5.2.3 daemon answered for the connected seeder of the capture
// swarm (docs/tasks/T035-task-peers.md Evidence): the envelope shape and
// the per-peer key names are the real wire. The first peer carries the
// captured values of a real GeoIP-named peer (country Switzerland); the
// second is the loopback shape — empty client and flags, the "N/A"
// unknown country — so one listing pins both the null and the non-null
// spelling of the optional fields.
const capturedPeersJSON = `{"full_update":true,"peers":{"198.51.100.9:51411":{"client":"qBittorrent/5.2.3","connection":"μTP","country":"Switzerland","country_code":"ch","dl_speed":695525,"downloaded":4487452,"files":"payload.bin","flags":"D X L P","flags_desc":"D = Interested (local) and unchoked (peer)\nX = Peer from PEX\nL = Peer from LSD\nP = μTP","host_name":"","ip":"198.51.100.9","peer_id_client":"-qB5230-","port":51411,"progress":1,"relevance":1,"up_speed":0,"uploaded":0},"203.0.113.7:51413":{"client":"","connection":"μTP","country":"N/A","country_code":"","dl_speed":0,"downloaded":0,"files":"","flags":"","flags_desc":"","host_name":"","ip":"203.0.113.7","peer_id_client":"","port":51413,"progress":0.5,"relevance":0,"up_speed":262144,"uploaded":0}},"rid":1,"show_flags":true}`

// swarmWireFake is a WebAPI stand-in for the three tracker endpoints. It
// serves one mutable listing in the captured row shape, mirrors
// addTrackers and removeTrackers onto that listing the way release-5.2.3
// does, and records every call so tests can pin the exact form values.
// The login and session rules mirror the daemon's (docs/06 section 5.2).
type swarmWireFake struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	calls      []swarmRequest
	rows       []string // served listing, one JSON object per row
	cookie     string
	listingErr int // answered to torrents/trackers; 0 means 200
	addErr     int // answered to torrents/addTrackers; 0 means 204
	peersErr   int // answered to sync/torrentPeers; 0 means 200
}

func newSwarmWireFake(t *testing.T) *swarmWireFake {
	t.Helper()

	f := &swarmWireFake{t: t, rows: splitTrackersJSON(capturedTrackersJSON)}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)

	return f
}

// splitTrackersJSON slices one listing into its row objects, preserving
// order, so the fake can add and drop rows without re-marshalling.
func splitTrackersJSON(listing string) []string {
	trimmed := strings.Trim(listing, " \n\t")
	trimmed = strings.TrimPrefix(strings.TrimSuffix(trimmed, "]"), "[")

	rows := make([]string, 0, 4)
	depth, start := 0, 0
	for i, c := range trimmed {
		switch c {
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			depth--
			if depth == 0 {
				rows = append(rows, trimmed[start:i+1])
			}
		}
	}

	return rows
}

func (f *swarmWireFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("parse form: %v", err)
	}
	rec := swarmRequest{Method: r.Method, Path: r.URL.Path, Form: r.PostForm}
	if r.Method == http.MethodGet {
		rec.Form = r.URL.Query()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.URL.Path {
	case "/api/v2/auth/login":
		if rec.Form.Get("username") != "admin" || rec.Form.Get("password") != "password123" {
			http.Error(w, "Fails.", http.StatusUnauthorized)

			return
		}
		f.cookie = "sid-swarm"
		http.SetCookie(w, &http.Cookie{Name: "QBT_SID_8080", Value: f.cookie, Path: "/", HttpOnly: true})
		w.WriteHeader(http.StatusNoContent)

	case "/api/v2/app/version":
		// The session probe of the client's login path; every version is
		// fine as long as it answers 200.
		_, _ = w.Write([]byte("v5.2.3"))

	case "/api/v2/torrents/trackers":
		if !f.sessionOK(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)

			return
		}
		f.calls = append(f.calls, rec)
		if f.hashUnknown(rec.Form.Get("hash")) {
			http.Error(w, "torrent not found", http.StatusNotFound)

			return
		}
		if f.listingErr != 0 {
			http.Error(w, "listing failed", f.listingErr)

			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[" + strings.Join(f.rows, ",") + "]"))

	case "/api/v2/torrents/addTrackers":
		if !f.sessionOK(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)

			return
		}
		f.calls = append(f.calls, rec)
		if f.addErr != 0 {
			http.Error(w, "add refused", f.addErr)

			return
		}
		// release-5.2.3 parses one tracker per line, each new tracker
		// starting not contacted with the unknown -1 counts.
		for _, raw := range strings.Split(rec.Form.Get("urls"), "\n") {
			if raw == "" {
				continue
			}
			f.rows = append(f.rows, `{"endpoints":[],"min_announce":0,"msg":"","next_announce":0,`+
				`"num_downloaded":-1,"num_leeches":-1,"num_peers":-1,"num_seeds":-1,`+
				`"status":1,"tier":0,"updating":false,"url":`+quoteJSON(raw)+"}")
		}
		w.WriteHeader(http.StatusNoContent)

	case "/api/v2/torrents/removeTrackers":
		if !f.sessionOK(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)

			return
		}
		f.calls = append(f.calls, rec)
		// release-5.2.3 splits the urls on pipes and percent-decodes each
		// element before matching.
		removed := map[string]bool{}
		for _, element := range strings.Split(rec.Form.Get("urls"), "|") {
			if decoded, err := url.PathUnescape(element); err == nil {
				removed[decoded] = true
			}
		}
		kept := f.rows[:0]
		for _, row := range f.rows {
			if !removed[f.rowURL(row)] {
				kept = append(kept, row)
			}
		}
		f.rows = kept
		w.WriteHeader(http.StatusNoContent)

	case "/api/v2/sync/torrentPeers":
		if !f.sessionOK(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)

			return
		}
		f.calls = append(f.calls, rec)
		if f.hashUnknown(rec.Form.Get("hash")) {
			http.Error(w, "torrent not found", http.StatusNotFound)

			return
		}
		if f.peersErr != 0 {
			http.Error(w, "peers listing failed", f.peersErr)

			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(capturedPeersJSON))

	default:
		http.Error(w, "unknown path", http.StatusNotFound)
	}
}

// quoteJSON renders s as one JSON string.
func quoteJSON(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}

	return string(encoded)
}

// rowURL extracts one row object's url member.
func (f *swarmWireFake) rowURL(row string) string {
	var parsed struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(row), &parsed); err != nil {
		f.t.Errorf("decode row %q: %v", row, err)
	}

	return parsed.URL
}

// hashUnknown reports whether the hash a call carried is one the fake
// does not serve — the daemon's not-found answer for a foreign torrent.
func (f *swarmWireFake) hashUnknown(hash string) bool {
	return hash != swarmHash
}

func (f *swarmWireFake) sessionOK(r *http.Request) bool {
	cookie, err := r.Cookie("QBT_SID_8080")

	return err == nil && cookie.Value == f.cookie
}

// trackerCalls returns the captured tracker-endpoint calls.
func (f *swarmWireFake) trackerCalls() []swarmRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]swarmRequest(nil), f.calls...)
}

// failListing makes torrents/trackers answer status.
func (f *swarmWireFake) failListing(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.listingErr = status
}

// failAdd makes torrents/addTrackers answer status.
func (f *swarmWireFake) failAdd(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.addErr = status
}

// failPeers makes sync/torrentPeers answer status.
func (f *swarmWireFake) failPeers(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.peersErr = status
}

// swarmWireEngine lifts the real qbittorrent client onto the Engine
// surface: the adapter is not a complete engine.Engine until T038 adds
// the last methods, and Remove's signature is the one gap. Everything
// else — Trackers, AddTrackers, RemoveTrackers included — is the real
// adapter's code, which is the point of this fixture.
type swarmWireEngine struct {
	*qbittorrent.Client
}

// Remove keeps payload data: Engine.Remove always retains data, and the
// deleteData switch belongs to the remove-task action alone.
func (e swarmWireEngine) Remove(ctx context.Context, id string) error {
	return e.Client.Remove(ctx, id, false)
}

// The four mutators below are T036's and T037's; the tracker tests never
// reach them, so the wrapper answers the interface's optional-method
// refusal instead of growing those methods early.
func (e swarmWireEngine) Rename(context.Context, string, string) error { return engine.ErrNotSupported }

func (e swarmWireEngine) SetCategory(context.Context, string, string) error {
	return engine.ErrNotSupported
}

func (e swarmWireEngine) SetLocation(context.Context, string, string) error {
	return engine.ErrNotSupported
}

func (e swarmWireEngine) SetRateLimits(context.Context, string, *int64, *int64) error {
	return engine.ErrNotSupported
}

func (e swarmWireEngine) SetShareLimits(context.Context, string, *float64, *int64) error {
	return engine.ErrNotSupported
}

// swarmTestEnv is the actions env with the real qbittorrent adapter over
// the wire fake registered — so the handler tests drive the adapter's
// actual decode and form building — and an aria2 stand-in without the
// BitTorrent capability.
type swarmTestEnv struct {
	*actionsTestEnv
	wire        *swarmWireFake
	aria2       *actionEngine
	qbittorrent swarmWireEngine
}

func newSwarmTestEnv(t *testing.T) *swarmTestEnv {
	t.Helper()

	wire := newSwarmWireFake(t)
	client, err := qbittorrent.New(
		qbittorrent.Config{BaseURL: wire.srv.URL, Username: "admin", Password: "password123"}, nil,
	)
	if err != nil {
		t.Fatalf("build qbittorrent client: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close qbittorrent client: %v", err)
		}
	})
	qbittorrentEngine := swarmWireEngine{Client: client}

	aria2 := newActionEngine(engine.NameAria2, acceptsAria2Lanes)
	env := newActionsTestEnvWithEngines(t, qbittorrentEngine, aria2)

	return &swarmTestEnv{actionsTestEnv: env, wire: wire, aria2: aria2, qbittorrent: qbittorrentEngine}
}

// seedSwarmTask writes one downloading task the qbittorrent adapter and
// the wire fake hold under swarmHash.
func (e *swarmTestEnv) seedSwarmTask(t *testing.T) string {
	t.Helper()

	ref := swarmHash

	return e.seedActionTask(t, func(task *store.Task) {
		task.Engine = engine.NameQBittorrent
		task.EngineRef = &ref
	})
}

// getTrackers drives GET /tasks/{id}/trackers.
func (e *swarmTestEnv) getTrackers(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Get("/tasks/"+id+"/trackers", "Authorization: Bearer "+e.bearer)
}

// postTrackers drives POST /tasks/{id}/trackers with a typed body.
func (e *swarmTestEnv) postTrackers(t *testing.T, id string, urls []string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Post("/tasks/"+id+"/trackers", map[string]any{"urls": urls},
		"Authorization: Bearer "+e.bearer)
}

// deleteTrackers drives DELETE /tasks/{id}/trackers with repeatable url
// query parameters.
func (e *swarmTestEnv) deleteTrackers(t *testing.T, id string, urls ...string) *httptest.ResponseRecorder {
	t.Helper()

	query := make([]string, 0, len(urls))
	for _, raw := range urls {
		query = append(query, "url="+url.QueryEscape(raw))
	}

	return e.api.Delete("/tasks/"+id+"/trackers?"+strings.Join(query, "&"),
		"Authorization: Bearer "+e.bearer)
}

// trackersBody decodes the listing envelope.
func trackersBody(t *testing.T, recorder *httptest.ResponseRecorder) []TrackerDTO {
	t.Helper()

	var body struct {
		Trackers []TrackerDTO `json:"trackers"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body.Trackers
}

// storedTrackers reads the task's task_trackers rows in insertion order.
func (e *swarmTestEnv) storedTrackers(t *testing.T, taskID string) []store.Tracker {
	t.Helper()

	var rows []store.Tracker
	if err := e.db.SelectContext(t.Context(), &rows,
		`SELECT url, status, update_timer_seconds, seeds, peers, message FROM task_trackers
WHERE task_id = ? ORDER BY rowid`, taskID); err != nil {
		t.Fatalf("read stored trackers of %s: %v", taskID, err)
	}

	return rows
}

// TestTrackersListPseudoRow pins the GET shape of doc 05 section 5.9 from
// the captured 5.2.3 response: the pseudo-tracker row lists with null
// seeds, the numeric status renders as its string, a working tracker
// carries its counts, and task_trackers holds exactly the listing.
func TestTrackersListPseudoRow(t *testing.T) {
	env := newSwarmTestEnv(t)
	id := env.seedSwarmTask(t)

	response := env.getTrackers(t, id)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}

	pseudo := TrackerDTO{URL: "** [DHT] **", Status: "2", Message: ""}
	working := TrackerDTO{
		URL:     "http://9.9.9.9/announce",
		Status:  "2",
		Seeds:   intPtr(412),
		Peers:   intPtr(118),
		Message: "\"tracker\" was working",
	}
	trackers := trackersBody(t, response)
	if len(trackers) != 3 {
		t.Fatalf("trackers = %+v, want the three-row listing", trackers)
	}
	if !sameTracker(trackers[0], pseudo) {
		t.Errorf("pseudo row = %+v, want %+v with null seeds and peers", trackers[0], pseudo)
	}
	if trackers[0].Seeds != nil || trackers[0].Peers != nil {
		t.Errorf("pseudo row seeds/peers = %v/%v, want null/null", trackers[0].Seeds, trackers[0].Peers)
	}
	if !sameTracker(trackers[2], working) {
		t.Errorf("working row = %+v, want %+v", trackers[2], working)
	}
	if trackers[1].Seeds != nil || trackers[1].Peers != nil {
		t.Errorf("never-contacted row seeds/peers = %v/%v, want null/null (the -1 unknowns)", trackers[1].Seeds, trackers[1].Peers)
	}

	// The listing was asked for under the bare engine hash, stripped of the
	// qbittorrent: namespace the handler passed down.
	calls := env.wire.trackerCalls()
	if len(calls) != 1 || calls[0].Form.Get("hash") != swarmHash {
		t.Errorf("tracker calls = %+v, want one listing under the bare hash", calls)
	}

	// The store holds exactly the listing: one row per tracker, the pseudo
	// row's seeds and peers NULL, the working row's counts stored.
	rows := env.storedTrackers(t, id)
	if len(rows) != 3 {
		t.Fatalf("stored rows = %+v, want three", rows)
	}
	if rows[0].URL != "** [DHT] **" || rows[0].Status != "2" || rows[0].Seeds != nil || rows[0].Peers != nil {
		t.Errorf("stored pseudo row = %+v, want status \"2\" and NULL counts", rows[0])
	}
	if rows[2].Seeds == nil || *rows[2].Seeds != 412 || rows[2].Peers == nil || *rows[2].Peers != 118 {
		t.Errorf("stored working row = %+v, want the 412/118 counts", rows[2])
	}
}

// sameTracker compares two DTOs by value, dereferencing the optional
// counts so a fresh pointer with the same number compares equal.
func sameTracker(got, want TrackerDTO) bool {
	if got.URL != want.URL || got.Status != want.Status || got.Message != want.Message {
		return false
	}

	return intPtrEqual(got.Seeds, want.Seeds) && intPtrEqual(got.Peers, want.Peers) &&
		intPtrEqual(got.UpdateTimerSeconds, want.UpdateTimerSeconds)
}

// intPtrEqual compares two optional ints by value.
func intPtrEqual(got, want *int) bool {
	if got == nil || want == nil {
		return got == want
	}

	return *got == *want
}

// TestAddTrackerReturns201 pins the POST answer: the engine receives
// torrents/addTrackers with the urls newline-joined in one form field —
// two urls here, so the separator itself is pinned — and the 201 body is
// the full updated listing with both new urls in it.
func TestAddTrackerReturns201(t *testing.T) {
	env := newSwarmTestEnv(t)
	id := env.seedSwarmTask(t)

	added := []string{"udp://8.8.8.8:6969/announce", "http://tracker.example.org/announce"}
	response := env.postTrackers(t, id, added)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}

	trackers := trackersBody(t, response)
	if len(trackers) != 5 {
		t.Fatalf("trackers = %+v, want the five-row post-add listing", trackers)
	}
	for i, url := range added {
		if row := trackers[3+i]; row.URL != url || row.Status != "1" {
			t.Errorf("added row %d = %+v, want %q at status \"1\"", i, row, url)
		}
	}

	// The engine saw one addTrackers with hash and the newline-joined urls.
	calls := env.wire.trackerCalls()
	var add *swarmRequest
	for i := range calls {
		if calls[i].Path == "/api/v2/torrents/addTrackers" {
			add = &calls[i]
		}
	}
	if add == nil {
		t.Fatalf("tracker calls = %+v, want one addTrackers call", calls)
	}
	if add.Form.Get("hash") != swarmHash {
		t.Errorf("addTrackers hash = %q, want the bare hash", add.Form.Get("hash"))
	}
	if want := "udp://8.8.8.8:6969/announce\nhttp://tracker.example.org/announce"; add.Form.Get("urls") != want {
		t.Errorf("addTrackers urls = %q, want the newline-joined %q", add.Form.Get("urls"), want)
	}

	// The store holds the updated listing, both new rows included.
	stored := env.storedTrackers(t, id)
	for _, url := range added {
		found := false
		for _, row := range stored {
			if row.URL == url {
				found = true
			}
		}
		if !found {
			t.Errorf("stored rows hold no %s row after the add", url)
		}
	}
}

// TestRemoveTrackerRoundTrip pins the DELETE path: the urls reach
// torrents/removeTrackers percent-encoded and pipe-joined, the removal
// lands, and the store holds the post-removal listing — the removed row
// leaves no stale copy behind.
func TestRemoveTrackerRoundTrip(t *testing.T) {
	env := newSwarmTestEnv(t)
	id := env.seedSwarmTask(t)

	removed := []string{"udp://tracker.example.org:6969/announce", "http://9.9.9.9/announce"}
	response := env.deleteTrackers(t, id, removed...)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusNoContent, response.Body.String())
	}

	// The removal reached the daemon under the bare hash and escaped: no
	// literal ':' or '/' rides the urls field, only the pipe separator is
	// plain — two urls here, so the separator itself is pinned.
	calls := env.wire.trackerCalls()
	var remove *swarmRequest
	for i := range calls {
		if calls[i].Path == "/api/v2/torrents/removeTrackers" {
			remove = &calls[i]
		}
	}
	if remove == nil {
		t.Fatalf("tracker calls = %+v, want one removeTrackers call", calls)
	}
	if remove.Form.Get("hash") != swarmHash {
		t.Errorf("removeTrackers hash = %q, want the bare hash", remove.Form.Get("hash"))
	}
	want := "udp%3A%2F%2Ftracker.example.org%3A6969%2Fannounce|http%3A%2F%2F9.9.9.9%2Fannounce"
	if remove.Form.Get("urls") != want {
		t.Errorf("removeTrackers urls = %q, want the percent-encoded, pipe-joined %q", remove.Form.Get("urls"), want)
	}

	// The listing no longer carries either url, and neither does the store.
	trackers := trackersBody(t, env.getTrackers(t, id))
	for _, row := range trackers {
		for _, url := range removed {
			if row.URL == url {
				t.Errorf("listing still carries the removed url %s: %+v", url, trackers)
			}
		}
	}
	for _, row := range env.storedTrackers(t, id) {
		for _, url := range removed {
			if row.URL == url {
				t.Errorf("task_trackers still holds the removed url %s: %+v", url, row)
			}
		}
	}
}

// TestRemovePseudoTrackerRejected pins the pseudo-tracker refusal: the
// adapter refuses locally with engine.ErrNotSupported, no request
// reaches the daemon, and the handler renders the refusal as the 422 of
// doc 05 section 5.9.
func TestRemovePseudoTrackerRejected(t *testing.T) {
	env := newSwarmTestEnv(t)
	id := env.seedSwarmTask(t)

	response := env.deleteTrackers(t, id, "** [DHT] **")
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	// The refusal is local: the daemon saw no removeTrackers.
	for _, call := range env.wire.trackerCalls() {
		if call.Path == "/api/v2/torrents/removeTrackers" {
			t.Errorf("removeTrackers reached the daemon: %+v", call)
		}
	}
}

// TestTrackerURLSSRFBlocked pins the block list of doc 12 section 2.1 on
// the add path: a private, loopback or otherwise denied address answers
// 403 /problems/ssrf-blocked and reaches no engine. Literal addresses
// only, so the test resolves nothing.
func TestTrackerURLSSRFBlocked(t *testing.T) {
	env := newSwarmTestEnv(t)
	id := env.seedSwarmTask(t)

	blocked := []string{
		"udp://192.0.2.1:6969/announce",            // TEST-NET-1, a denied prefix
		"http://10.0.0.5/announce",                 // RFC 1918
		"udp://[::1]:6969/announce",                // outside 2000::/3
		"http://[::ffff:169.254.169.254]/announce", // IPv4-mapped link-local
	}
	for _, raw := range blocked {
		t.Run(raw, func(t *testing.T) {
			response := env.postTrackers(t, id, []string{raw})
			assertProblem(t, response, http.StatusForbidden, SlugSSRFBlocked)
		})
	}

	if calls := env.wire.trackerCalls(); len(calls) != 0 {
		t.Errorf("engine was contacted: %+v", calls)
	}
}

// TestTrackersOnNonBitTorrentTask pins the capability gate: every verb on
// a task whose engine declares no bittorrent capability is the 422 of
// doc 05 section 5.9 — never an empty listing — and no engine call runs.
func TestTrackersOnNonBitTorrentTask(t *testing.T) {
	env := newSwarmTestEnv(t)
	id := env.seedActionTask(t, func(task *store.Task) {
		task.Engine = engine.NameAria2
		ref := aria2GID
		task.EngineRef = &ref
	})

	assertProblem(t, env.getTrackers(t, id), http.StatusUnprocessableEntity, SlugValidationFailed)
	assertProblem(t, env.postTrackers(t, id, []string{"udp://8.8.8.8:6969/announce"}),
		http.StatusUnprocessableEntity, SlugValidationFailed)
	assertProblem(t, env.deleteTrackers(t, id, "udp://8.8.8.8:6969/announce"),
		http.StatusUnprocessableEntity, SlugValidationFailed)

	env.aria2.assertNoCalls(t)
}

// TestTrackersSchemeRejections pins the shape rejections of the add path:
// a non-announce scheme, a hostless url and an empty or null urls body are
// 422s that reach no engine.
func TestTrackersSchemeRejections(t *testing.T) {
	env := newSwarmTestEnv(t)
	id := env.seedSwarmTask(t)

	for name, raw := range map[string]string{
		"ftp scheme": "ftp://tracker.example.org/announce",
		"no host":    "udp:///announce",
		// A port-only host is not a host: url.Parse accepts ":6969", every
		// dialer reads an empty host as loopback, and the block gate
		// cannot verify an empty name — the round-4 review's blocker.
		"port only":          "http://:80/announce",
		"port only, udp":     "udp://:6969/announce",
		"userinfo port only": "http://x@:8080/announce",
	} {
		t.Run(name, func(t *testing.T) {
			response := env.postTrackers(t, id, []string{raw})
			problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
			if len(problem.Errors) != 1 || problem.Errors[0].Location != "body.urls[0]" {
				t.Errorf("errors = %+v, want the body.urls[0] field error", problem.Errors)
			}
		})
	}

	t.Run("empty urls array", func(t *testing.T) {
		response := env.api.Post("/tasks/"+id+"/trackers", map[string]any{"urls": []string{}},
			"Authorization: Bearer "+env.bearer)
		assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	})

	t.Run("null urls", func(t *testing.T) {
		response := env.api.Post("/tasks/"+id+"/trackers", strings.NewReader(`{"urls":null}`),
			"Authorization: Bearer "+env.bearer, "Content-Type: application/json")
		assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	})

	// A DELETE that names no url at all is the same refusal.
	t.Run("absent remove urls", func(t *testing.T) {
		response := env.api.Delete("/tasks/"+id+"/trackers", "Authorization: Bearer "+env.bearer)
		assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	})

	if calls := env.wire.trackerCalls(); len(calls) != 0 {
		t.Errorf("engine was contacted: %+v", calls)
	}
}

// TestTrackersUnknownTask pins the 404 of all three verbs.
func TestTrackersUnknownTask(t *testing.T) {
	env := newSwarmTestEnv(t)

	assertProblem(t, env.getTrackers(t, unknownID), http.StatusNotFound, SlugNotFound)
	assertProblem(t, env.postTrackers(t, unknownID, []string{"udp://8.8.8.8:6969/announce"}),
		http.StatusNotFound, SlugNotFound)
	assertProblem(t, env.deleteTrackers(t, unknownID, "udp://8.8.8.8:6969/announce"),
		http.StatusNotFound, SlugNotFound)
}

// TestTrackersForeignAtEngine pins the foreign-task answer: a task whose
// hash the daemon no longer holds lists as 404, not 503.
func TestTrackersForeignAtEngine(t *testing.T) {
	env := newSwarmTestEnv(t)
	ref := filesOtherHash
	id := env.seedActionTask(t, func(task *store.Task) {
		task.Engine = engine.NameQBittorrent
		task.EngineRef = &ref
	})

	assertProblem(t, env.getTrackers(t, id), http.StatusNotFound, SlugNotFound)
}

// TestTrackersEngineFailures pins the 503s: a daemon that answers an
// error to the listing and one that refuses the change both leave the
// store untouched.
func TestTrackersEngineFailures(t *testing.T) {
	env := newSwarmTestEnv(t)
	id := env.seedSwarmTask(t)

	env.wire.failListing(http.StatusInternalServerError)
	assertProblem(t, env.getTrackers(t, id), http.StatusServiceUnavailable, SlugEngineUnavailable)

	// A task never listed holds no rows to serve and no add succeeded.
	if rows := env.storedTrackers(t, id); len(rows) != 0 {
		t.Errorf("stored rows = %+v, want none after the failed listing", rows)
	}

	// A daemon that refuses the change leaves the store untouched: nothing
	// was added, so no listing lands either.
	env.wire.failListing(0)
	env.wire.failAdd(http.StatusInternalServerError)
	assertProblem(t, env.postTrackers(t, id, []string{"udp://8.8.8.8:6969/announce"}),
		http.StatusServiceUnavailable, SlugEngineUnavailable)
	if rows := env.storedTrackers(t, id); len(rows) != 0 {
		t.Errorf("stored rows = %+v, want none after the refused add", rows)
	}
}

// TestTrackersNotAdmitted pins the not-admitted answer: a queued task no
// engine holds yet has no swarm to expose.
func TestTrackersNotAdmitted(t *testing.T) {
	env := newSwarmTestEnv(t)
	id := env.seedActionTask(t, func(task *store.Task) {
		task.Engine = engine.NameQBittorrent
		task.EngineRef = nil
	})

	assertProblem(t, env.getTrackers(t, id), http.StatusUnprocessableEntity, SlugValidationFailed)

	if calls := env.wire.trackerCalls(); len(calls) != 0 {
		t.Errorf("engine was contacted: %+v", calls)
	}
}

// TestTrackersListingReplacesRows pins the replace semantics of
// ReplaceTrackers: a second listing with fewer rows leaves no stale row
// behind. Duplicate handling is pinned by
// TestReplaceTrackersDeduplicatesRows.
func TestTrackersListingReplacesRows(t *testing.T) {
	env := newSwarmTestEnv(t)
	id := env.seedSwarmTask(t)

	if response := env.getTrackers(t, id); response.Code != http.StatusOK {
		t.Fatalf("seed listing: status %d body %s", response.Code, response.Body.String())
	}

	response := env.deleteTrackers(t, id, "udp://tracker.example.org:6969/announce", "http://9.9.9.9/announce")
	if response.Code != http.StatusNoContent {
		t.Fatalf("removal: status %d body %s", response.Code, response.Body.String())
	}

	rows := env.storedTrackers(t, id)
	if len(rows) != 1 || rows[0].URL != "** [DHT] **" {
		t.Errorf("stored rows = %+v, want only the pseudo row after the removals", rows)
	}
}

// TestReplaceTrackersDeduplicatesRows pins the no-duplicates half of
// the store invariant: a listing that carries one url twice stores one
// row, and the row set always mirrors exactly the last successful
// listing.
func TestReplaceTrackersDeduplicatesRows(t *testing.T) {
	env := newSwarmTestEnv(t)
	id := env.seedSwarmTask(t)

	tasks := store.NewTaskStore(env.db)
	rows := []store.Tracker{
		{URL: "udp://8.8.8.8:6969/announce", Status: "1"},
		{URL: "udp://8.8.8.8:6969/announce", Status: "2"},
		{URL: "** [DHT] **", Status: "2"},
	}
	if err := tasks.ReplaceTrackers(t.Context(), id, rows); err != nil {
		t.Fatalf("replace trackers: %v", err)
	}

	stored := env.storedTrackers(t, id)
	if len(stored) != 2 {
		t.Fatalf("stored rows = %+v, want two: the duplicate url stored once", stored)
	}
	if stored[0].URL != "udp://8.8.8.8:6969/announce" || stored[0].Status != "1" {
		t.Errorf("first row = %+v, want the url with its first occurrence's status", stored[0])
	}

	// A later listing replaces the row set wholesale.
	if err := tasks.ReplaceTrackers(t.Context(), id, rows[:1]); err != nil {
		t.Fatalf("replace trackers again: %v", err)
	}
	if stored = env.storedTrackers(t, id); len(stored) != 1 || stored[0].URL != "udp://8.8.8.8:6969/announce" {
		t.Errorf("stored rows = %+v, want exactly the last listing's row", stored)
	}
}

// TestAddTrackersRejectsEmbeddedLineBreaks pins the adapter-side guard
// of the add path: a url that is empty or carries a line break would be
// split by the daemon into announce urls the caller never named, so the
// client refuses it without issuing the request.
func TestAddTrackersRejectsEmbeddedLineBreaks(t *testing.T) {
	env := newSwarmTestEnv(t)
	id := "qbittorrent:" + swarmHash

	for name, urls := range map[string][]string{
		"newline injection": {"udp://a.example/announce\nudp://b.example/announce"},
		"trailing newline":  {"udp://a.example/announce\n"},
		"empty url":         {""},
	} {
		t.Run(name, func(t *testing.T) {
			err := env.qbittorrent.AddTrackers(t.Context(), id, urls)
			if err == nil {
				t.Fatalf("AddTrackers(%q) = nil, want the refusal", urls)
			}
		})
	}

	for _, call := range env.wire.trackerCalls() {
		if call.Path == "/api/v2/torrents/addTrackers" {
			t.Errorf("addTrackers reached the daemon: %+v", call)
		}
	}
}

// TestRemoveTrackersCapsURLCount pins the remove-side bound: a repeated
// query parameter has no schema tag of its own, so the handler mirrors
// the add side's 100-url cap itself.
func TestRemoveTrackersCapsURLCount(t *testing.T) {
	env := newSwarmTestEnv(t)
	id := env.seedSwarmTask(t)

	urls := make([]string, trackersMaxURLs+1)
	for i := range urls {
		urls[i] = "udp://8.8.8.8:6969/announce"
	}

	assertProblem(t, env.deleteTrackers(t, id, urls...), http.StatusUnprocessableEntity, SlugValidationFailed)

	for _, call := range env.wire.trackerCalls() {
		if call.Path == "/api/v2/torrents/removeTrackers" {
			t.Errorf("removeTrackers reached the daemon: %+v", call)
		}
	}
}

// TestDNSErrorTextOmitsTheHost pins the log-hygiene rule of doc 14
// section 3.3 on the block check's warn line: a net.DNSError renders the
// queried host in its own text, and an announce host can be
// user-identifying.
func TestDNSErrorTextOmitsTheHost(t *testing.T) {
	err := &net.DNSError{Err: "no such host", Name: "tracker.example.org", Server: "10.0.0.1:53"}

	if text := dnsErrorText(err); strings.Contains(text, "tracker.example.org") {
		t.Errorf("dnsErrorText = %q, want the host omitted", text)
	}
	if text := dnsErrorText(err); !strings.Contains(text, "no such host") {
		t.Errorf("dnsErrorText = %q, want the resolver's reason kept", text)
	}
	if text := dnsErrorText(nil); text != "no addresses" {
		t.Errorf("dnsErrorText(nil) = %q, want the no-addresses answer", text)
	}
	if text := dnsErrorText(errors.New("boom")); text != "boom" {
		t.Errorf("dnsErrorText(non-dns) = %q, want the error's own text", text)
	}

	// A DNSError whose Err wraps the transport error verbatim — the
	// native resolver embeds the local address and the resolver's —
	// degrades to the static reason and leaks nothing.
	transport := &net.DNSError{
		Err:    "read udp 10.0.0.5:59813->192.168.1.1:53: i/o timeout",
		Name:   "tracker.example.org",
		Server: "192.168.1.1:53",
	}
	if text := dnsErrorText(transport); text != dnsFailureReason {
		t.Errorf("dnsErrorText(transport-shaped DNSError) = %q, want %q", text, dnsFailureReason)
	}
	if text := dnsErrorText(transport); strings.Contains(text, "10.0.0.5") || strings.Contains(text, "192.168.1.1") {
		t.Errorf("dnsErrorText(transport-shaped DNSError) = %q, want no addresses in it", text)
	}

	// The known-safe spellings pass through, and an empty Err still
	// yields a reason — never the wrapped error's host-bearing text.
	if text := dnsErrorText(&net.DNSError{Err: "no such host"}); text != "no such host" {
		t.Errorf("dnsErrorText(no such host) = %q, want the safe reason kept", text)
	}
	empty := &net.DNSError{Err: "", Name: "tracker.example.org"}
	if text := dnsErrorText(empty); text != dnsFailureReason {
		t.Errorf("dnsErrorText(empty DNSError) = %q, want %q", text, dnsFailureReason)
	}
}

// TestTrackerHostBlockedEmptyHostFailsClosed pins the guard's totality:
// an empty host denotes loopback to every dialer, so it is blocked even
// though the shape check should have refused such a url already.
func TestTrackerHostBlockedEmptyHostFailsClosed(t *testing.T) {
	if !trackerHostBlocked(t.Context(), "") {
		t.Error("trackerHostBlocked(\"\") = false, want the fail-closed true")
	}
}

// TestAddTaskTrackersMaxItemsMatchesConstant pins the add body's schema
// tag to the constant the remove path enforces in code, so the two
// spellings of one limit cannot drift apart silently. The remove side
// has no schema tag to pin — huma renders no maxItems on a repeated
// query parameter — so its cap is pinned by TestRemoveTrackersCapsURLCount
// at the handler instead.
func TestAddTaskTrackersMaxItemsMatchesConstant(t *testing.T) {
	field, ok := reflect.TypeOf(AddTaskTrackersInput{}.Body).FieldByName("URLs")
	if !ok {
		t.Fatal("URLs field not found on the add body")
	}
	if got := field.Tag.Get("maxItems"); got != strconv.Itoa(trackersMaxURLs) {
		t.Fatalf("maxItems tag = %q, want %d (trackersMaxURLs)", got, trackersMaxURLs)
	}
}

// TestTrackerGateBudgetFailsClosedOnExpiry pins the gate's two expiry
// behaviours: a spent budget makes the next lookup fail — and the gate
// fail closed, never fail open — and the lookups really run under the
// gate's context, not the caller's. The host is an RFC 6761 .invalid
// name: under the expired budget the lookup errors with a deadline, not
// an authoritative NXDOMAIN, so the only verdict the correct code can
// return is blocked; a refactor that dropped the gate context would
// resolve the name to its NXDOMAIN answer and allow it. That contrast
// needs a resolver that answers .invalid authoritatively; where DNS is
// unreachable the lookup errors under either context and this assertion
// passes without exercising the gate-context routing it claims to pin.
func TestTrackerGateBudgetFailsClosedOnExpiry(t *testing.T) {
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()

	urls := []string{"udp://tracker-budget.invalid/announce"}
	if !trackerURLsBlocked(expired, urls) {
		t.Error("trackerURLsBlocked with a spent gate budget = false, want the fail-closed true")
	}
}

// TestTrackerIPBlockedRules pins the block list directly against doc 12
// section 2.1: every denied IPv4 family, the mapped-loopback unmapping,
// the IPv6 allow-list edge and one allowed address per family.
func TestTrackerIPBlockedRules(t *testing.T) {
	blocked := []string{
		"0.0.0.0", "10.1.2.3", "100.100.100.200", "127.0.0.1", "169.254.169.254",
		"172.16.0.1", "172.31.255.255", "192.0.0.192", "192.0.2.9", "192.88.99.1",
		"192.168.1.1", "198.18.0.5", "198.51.100.7", "203.0.113.7", "224.0.0.1",
		"240.0.0.1", "255.255.255.255",
		"::1", "::ffff:127.0.0.1", "::ffff:169.254.169.254", "fc00::1", "fe80::1",
		"ff02::1", "64:ff9b::169.254.169.254", "2001:db8::1", "2002:0a00:0001::",
		"3fff::1", "2620:4f:8000::1", "2001:100::1",
	}
	for _, raw := range blocked {
		if !trackerIPBlocked(mustParseAddr(t, raw)) {
			t.Errorf("trackerIPBlocked(%s) = false, want true", raw)
		}
	}

	allowed := []string{"8.8.8.8", "1.1.1.1", "203.0.114.1", "2606:4700::1111", "2001:4860:4860::8888"}
	for _, raw := range allowed {
		if trackerIPBlocked(mustParseAddr(t, raw)) {
			t.Errorf("trackerIPBlocked(%s) = true, want false", raw)
		}
	}
}

// mustParseAddr parses one literal address or fails the test.
func mustParseAddr(t *testing.T, raw string) netip.Addr {
	t.Helper()

	addr, err := netip.ParseAddr(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}

	return addr
}

// getPeers drives GET /tasks/{id}/peers.
func (e *swarmTestEnv) getPeers(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Get("/tasks/"+id+"/peers", "Authorization: Bearer "+e.bearer)
}

// peersBody decodes the listing envelope.
func peersBody(t *testing.T, recorder *httptest.ResponseRecorder) []PeerDTO {
	t.Helper()

	var body struct {
		Peers []PeerDTO `json:"peers"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body.Peers
}

// TestTaskPeersListing pins the GET shape of doc 05 section 5.9 from the
// captured 5.2.3 response: a GeoIP-named peer carries its client, flags
// and country verbatim, a peer with empty keys and the "N/A" unknown
// country answers null for all three, rates stay bytes per second and
// progress stays 0.0-1.0 — and the engine was asked under the bare hash
// at rid=0, the fresh client's first sync/torrentPeers request.
func TestTaskPeersListing(t *testing.T) {
	env := newSwarmTestEnv(t)
	id := env.seedSwarmTask(t)

	response := env.getPeers(t, id)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}

	peers := peersBody(t, response)
	if len(peers) != 2 {
		t.Fatalf("peers = %+v, want the two-peer listing", peers)
	}

	named := peers[0]
	if named.Address != "198.51.100.9:51411" {
		t.Errorf("first peer address = %q, want the sorted-first key", named.Address)
	}
	if named.Client == nil || *named.Client != "qBittorrent/5.2.3" {
		t.Errorf("named peer client = %v, want the reported client string", named.Client)
	}
	if named.Flags == nil || *named.Flags != "D X L P" {
		t.Errorf("named peer flags = %v, want the reported letter string", named.Flags)
	}
	if named.Country == nil || *named.Country != "Switzerland" {
		t.Errorf("named peer country = %v, want Switzerland", named.Country)
	}
	if named.Progress != 1.0 {
		t.Errorf("named peer progress = %v, want 1.0", named.Progress)
	}
	if named.DownloadRate != 695525 || named.UploadRate != 0 {
		t.Errorf("named peer rates = %d/%d, want the reported bytes per second", named.DownloadRate, named.UploadRate)
	}

	unknown := peers[1]
	if unknown.Address != "203.0.113.7:51413" {
		t.Errorf("second peer address = %q, want the sorted-second key", unknown.Address)
	}
	if unknown.Client != nil || unknown.Flags != nil || unknown.Country != nil {
		t.Errorf("unknown peer optionals = %v/%v/%v, want null/null/null",
			unknown.Client, unknown.Flags, unknown.Country)
	}
	if unknown.Progress != 0.5 || unknown.DownloadRate != 0 || unknown.UploadRate != 262144 {
		t.Errorf("unknown peer values = %+v, want the reported ones", unknown)
	}

	// The listing was asked for under the bare engine hash at rid=0: the
	// client is fresh, so no peer delta state exists yet.
	calls := env.wire.trackerCalls()
	peersCalls := make([]swarmRequest, 0, 1)
	for _, call := range calls {
		if call.Path == "/api/v2/sync/torrentPeers" {
			peersCalls = append(peersCalls, call)
		}
	}
	if len(peersCalls) != 1 ||
		peersCalls[0].Form.Get("hash") != swarmHash || peersCalls[0].Form.Get("rid") != "0" {
		t.Errorf("torrentPeers calls = %+v, want one under the bare hash at rid=0", peersCalls)
	}
}

// TestPeersOnNonBitTorrentTask pins the capability gate: a peers request
// on a task whose engine declares no bittorrent capability is the 422 of
// doc 05 section 5.9 — never an empty listing — and no engine call runs.
func TestPeersOnNonBitTorrentTask(t *testing.T) {
	env := newSwarmTestEnv(t)
	id := env.seedActionTask(t, func(task *store.Task) {
		task.Engine = engine.NameAria2
		ref := aria2GID
		task.EngineRef = &ref
	})

	assertProblem(t, env.getPeers(t, id), http.StatusUnprocessableEntity, SlugValidationFailed)

	env.aria2.assertNoCalls(t)
	for _, call := range env.wire.trackerCalls() {
		if call.Path == "/api/v2/sync/torrentPeers" {
			t.Errorf("torrentPeers reached the daemon: %+v", call)
		}
	}
}

// TestPeersRejections pins the remaining answers of the peers endpoint:
// 404 for an unknown task, 404 for a task the engine no longer holds and
// 503 when the engine's listing fails.
func TestPeersRejections(t *testing.T) {
	env := newSwarmTestEnv(t)

	assertProblem(t, env.getPeers(t, unknownID), http.StatusNotFound, SlugNotFound)

	ref := filesOtherHash
	foreign := env.seedActionTask(t, func(task *store.Task) {
		task.Engine = engine.NameQBittorrent
		task.EngineRef = &ref
	})
	assertProblem(t, env.getPeers(t, foreign), http.StatusNotFound, SlugNotFound)

	env.wire.failPeers(http.StatusInternalServerError)
	id := env.seedSwarmTask(t)
	assertProblem(t, env.getPeers(t, id), http.StatusServiceUnavailable, SlugEngineUnavailable)
}
