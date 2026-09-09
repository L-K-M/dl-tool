package qbittorrent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/store"
)

// maindataFixturePath is the captured full_update response of a real 5.2.3
// daemon (docs/13-testing-and-verification.md section 5; capture details in
// the T030 task file's Evidence).
const maindataFixturePath = "testdata/qb_maindata_full_5.2.3.json"

// fixtureHashes are the two torrents the capture holds, sorted — the order
// cache.merge reports them in.
var fixtureHashes = []string{
	"a1dfefec1a9dd7fa8a041ebeeea271db55126d2f",
	"d160b8d8ea35a5b4e52837468fc8f03d55cef1f7",
}

// mustReadMaindata loads and decodes the captured full_update fixture.
func mustReadMaindata(t *testing.T) maindata {
	t.Helper()

	raw, err := os.ReadFile(maindataFixturePath)
	require.NoError(t, err)

	var m maindata
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

// setOf builds the hash sets the tests install and assert on.
func setOf(hashes ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(hashes))
	for _, hash := range hashes {
		set[hash] = struct{}{}
	}
	return set
}

// staticSource is an ownership source whose snapshot never changes. The
// tests that need a changing snapshot swap the installed source under the
// cache mutex instead, which is how a TTL-refreshed listing changes what a
// fixed source returns.
func staticSource(set map[string]struct{}) func() map[string]struct{} {
	return func() map[string]struct{} { return set }
}

// currentEpoch reads the cache's ownership epoch.
func currentEpoch(c *Client) uint64 {
	c.md.mu.Lock()
	defer c.md.mu.Unlock()
	return c.md.cache.ownershipEpoch
}

// subscribeEvents registers one event subscriber with a lifetime bound to
// the test.
func subscribeEvents(t *testing.T, c *Client) <-chan engine.TaskEvent {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events, err := c.Events(ctx)
	require.NoError(t, err)
	return events
}

// seedTorrent installs one decoded torrent object into a cache. Caller
// holds md.mu or owns the cache outright.
func seedTorrent(t *testing.T, c *cache, hash, state string) {
	t.Helper()

	fields, ok := decodeTorrentFields(json.RawMessage(torrentBody(hash, state)))
	require.True(t, ok)
	c.fields[hash] = fields
}

func TestMergeFullUpdate(t *testing.T) {
	c := &cache{owned: setOf(fixtureHashes...)}

	disposition, changed, removed := c.merge(mustReadMaindata(t))
	require.Equal(t, mergeApplied, disposition)
	require.Equal(t, fixtureHashes, changed)
	require.Empty(t, removed)

	require.Equal(t, 1, c.rid)
	require.Len(t, c.fields, len(fixtureHashes))
	require.Empty(t, c.rejected)
	require.Empty(t, c.pendingFull)
	require.False(t, c.forceFull)
	require.False(t, c.lastFullAt.IsZero(), "an accepted full update restarts the full-sync interval")
	for _, hash := range fixtureHashes {
		require.Contains(t, c.fields, hash)
	}

	// The captured torrent projects through toTaskInfo unchanged: state
	// downloading, size known because metadata arrived, the daemon's own
	// counters carried over.
	info, ok := taskInfoFromCache(fixtureHashes[0], c.fields[fixtureHashes[0]])
	require.True(t, ok)
	require.Equal(t, engine.NameQBittorrent+":"+fixtureHashes[0], info.ID)
	require.Equal(t, engine.StateDownloading, info.State)
	require.NotNil(t, info.TotalBytes)
	require.Equal(t, int64(3303444480), *info.TotalBytes)
	require.Equal(t, int64(2855665664), info.CompletedBytes)
	require.Equal(t, int64(64245008), info.DownloadRate)
	require.Equal(t, fixtureHashes[0], info.InfohashV1)
}

func TestMergePartialKeepsUntouchedFields(t *testing.T) {
	// The literal partial of docs/06-download-engines.md section 5.4,
	// against a full update that seeded every field it omits.
	full := maindata{
		Rid:        14,
		FullUpdate: true,
		Torrents: map[string]json.RawMessage{
			testHash: json.RawMessage(`{"hash":"` + testHash + `","name":"test.iso","state":"downloading","progress":0.5,"dlspeed":1024,"save_path":"/data"}`),
		},
	}
	c := &cache{owned: setOf(testHash)}
	disposition, changed, removed := c.merge(full)
	require.Equal(t, mergeApplied, disposition)
	require.Equal(t, []string{testHash}, changed)
	require.Empty(t, removed)

	// Exactly the section's example: {"rid":15,"torrents":{"8c2127…":{"state":"pausedUP"}}}
	partial := maindata{
		Rid: 15,
		Torrents: map[string]json.RawMessage{
			testHash: json.RawMessage(`{"state":"pausedUP"}`),
		},
	}
	disposition, changed, removed = c.merge(partial)
	require.Equal(t, mergeApplied, disposition)
	require.Equal(t, []string{testHash}, changed)
	require.Empty(t, removed)

	// Only state changed; every field the delta omitted is still held.
	fields := c.fields[testHash]
	require.Equal(t, "pausedUP", fields["state"])
	require.Equal(t, "test.iso", fields["name"])
	require.Equal(t, 0.5, fields["progress"])
	require.Equal(t, 1024.0, fields["dlspeed"])
	require.Equal(t, "/data", fields["save_path"])
	require.Equal(t, 15, c.rid)

	info, ok := taskInfoFromCache(testHash, fields)
	require.True(t, ok)
	// pausedUP at progress < 1 normalises to paused, not completed.
	require.Equal(t, engine.StatePaused, info.State)
	require.Equal(t, "test.iso", info.Name)
}

func TestMergePartialIntoZeroCacheDoesNotPanic(t *testing.T) {
	// A zero-value cache — nil fields map — must survive a partial that
	// arrives before any full response: the poll goroutine has no recover,
	// so a panic here would take the process down. The first-seen
	// guarantee makes the reported object complete.
	c := &cache{owned: setOf(testHash)}
	disposition, changed, removed := c.merge(maindata{
		Rid: 3,
		Torrents: map[string]json.RawMessage{
			testHash: json.RawMessage(`{"state":"downloading"}`),
		},
	})
	require.Equal(t, mergeApplied, disposition)
	require.Equal(t, []string{testHash}, changed)
	require.Empty(t, removed)
	require.Contains(t, c.fields, testHash)
}

func TestMergeNoChangeEmitsNothing(t *testing.T) {
	// A delta that re-sends values already stored is not a change: the
	// daemon re-sends unchanged fields on some paths, and one event per
	// changed hash means unchanged is no event.
	c := &cache{owned: setOf(fixtureHashes...)}
	_, _, _ = c.merge(mustReadMaindata(t))

	// The capture's own values for state and dlspeed, re-sent verbatim.
	disposition, changed, removed := c.merge(maindata{
		Rid: 2,
		Torrents: map[string]json.RawMessage{
			fixtureHashes[0]: json.RawMessage(`{"state":"downloading","dlspeed":64245008}`),
		},
	})
	require.Equal(t, mergeApplied, disposition)
	require.Empty(t, changed)
	require.Empty(t, removed)
	require.Equal(t, 2, c.rid)
}

func TestFullUpdateDroppingFieldReportsChange(t *testing.T) {
	c := &cache{owned: setOf(testHash)}
	_, _, _ = c.merge(maindata{
		Rid:        1,
		FullUpdate: true,
		Torrents: map[string]json.RawMessage{
			testHash: json.RawMessage(`{"hash":"` + testHash + `","name":"old","state":"downloading"}`),
		},
	})

	disposition, changed, removed := c.merge(maindata{
		Rid:        2,
		FullUpdate: true,
		Torrents: map[string]json.RawMessage{
			testHash: json.RawMessage(`{"hash":"` + testHash + `","state":"downloading"}`),
		},
	})
	require.Equal(t, mergeApplied, disposition)
	require.Equal(t, []string{testHash}, changed)
	require.Empty(t, removed)
	require.NotContains(t, c.fields[testHash], "name")
}

func TestFullUpdateDecodeFailureKeepsLastGoodObject(t *testing.T) {
	for _, raw := range []string{`42`, `null`} {
		t.Run(raw, func(t *testing.T) {
			c := &cache{owned: setOf(testHash)}
			_, _, _ = c.merge(maindata{
				Rid:        1,
				FullUpdate: true,
				Torrents: map[string]json.RawMessage{
					testHash: json.RawMessage(torrentBody(testHash, "downloading")),
				},
			})
			before := mapsClone(c.fields[testHash])

			disposition, changed, removed := c.merge(maindata{
				Rid:        2,
				FullUpdate: true,
				Torrents: map[string]json.RawMessage{
					testHash: json.RawMessage(raw),
				},
			})
			require.Equal(t, mergeApplied, disposition)
			require.Empty(t, changed)
			require.Empty(t, removed)
			require.Equal(t, before, c.fields[testHash])
		})
	}
}

// mapsClone copies one merge level; the values are scalars in the fixture.
func mapsClone(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func TestHashChangedAndRemovedInOneDeltaIsRemovedOnly(t *testing.T) {
	// One delta that both changes and removes the same hash: removal
	// wins, and no event may carry a zero-value info for it.
	c := &cache{owned: setOf(testHash)}
	_, _, _ = c.merge(maindata{
		Rid: 1, FullUpdate: true,
		Torrents: map[string]json.RawMessage{
			testHash: json.RawMessage(torrentBody(testHash, "downloading")),
		},
	})
	disposition, changed, removed := c.merge(maindata{
		Rid: 2,
		Torrents: map[string]json.RawMessage{
			testHash: json.RawMessage(`{"state":"pausedDL"}`),
		},
		TorrentsRemoved: []string{testHash},
	})
	require.Equal(t, mergeApplied, disposition)
	require.Empty(t, changed)
	require.Equal(t, []string{testHash}, removed)
	require.NotContains(t, c.fields, testHash)
}

func TestUnchangedFullSnapshotEmitsNothing(t *testing.T) {
	c := &Client{}
	events := subscribeEvents(t, c)
	c.SetOwnershipFilter(staticSource(setOf(testHash)))

	m := maindata{
		Rid:        1,
		FullUpdate: true,
		Torrents:   fullTorrents(map[string]string{testHash: torrentBody(testHash, "downloading")}),
	}
	c.applyResponse(m, currentEpoch(c))
	require.Len(t, drainEvents(events), 1, "the first appearance is one event")

	// The same snapshot again, byte for byte: no hash changed, none
	// dropped, so nothing may emit.
	m.Rid = 2
	c.applyResponse(m, currentEpoch(c))
	require.Empty(t, drainEvents(events))
}

func TestFullSnapshotEmitsEventRemovedForDroppedHash(t *testing.T) {
	c := &Client{}
	events := subscribeEvents(t, c)
	c.SetOwnershipFilter(staticSource(setOf(testHash, otherHash)))

	c.applyResponse(maindata{
		Rid:        1,
		FullUpdate: true,
		Torrents:   fullTorrents(map[string]string{testHash: torrentBody(testHash, "downloading"), otherHash: torrentBody(otherHash, "downloading")}),
	}, currentEpoch(c))
	drainEvents(events)

	// The daemon dropped the first hash while the ownership snapshot
	// still accepts it: a real removal the event consumer must hear.
	c.applyResponse(maindata{
		Rid:        2,
		FullUpdate: true,
		Torrents:   fullTorrents(map[string]string{otherHash: torrentBody(otherHash, "downloading")}),
	}, currentEpoch(c))

	got := drainEvents(events)
	require.Len(t, got, 1)
	require.Equal(t, engine.EventRemoved, got[0].Kind)
	require.Equal(t, engine.NameQBittorrent+":"+testHash, got[0].TaskID)
	require.Nil(t, got[0].Info)

	_, err := c.Get(context.Background(), engine.NameQBittorrent+":"+testHash)
	require.ErrorIs(t, err, engine.ErrNotFound)
}

func TestFullSnapshotPrunesStaleRejectedHashes(t *testing.T) {
	// A rejected identifier the full response no longer reports is gone
	// for good: the rebuild keeps only what the daemon still holds.
	c := &cache{owned: setOf(testHash)}
	c.ensureMaps()
	c.rejected[otherHash] = struct{}{}

	disposition, changed, removed := c.merge(maindata{
		Rid:        2,
		FullUpdate: true,
		Torrents:   fullTorrents(map[string]string{testHash: torrentBody(testHash, "downloading")}),
	})
	require.Equal(t, mergeApplied, disposition)
	require.Equal(t, []string{testHash}, changed)
	require.Empty(t, removed)
	require.Empty(t, c.rejected)
	require.NotContains(t, c.rejected, otherHash)
}

func TestFullSnapshotPrunesStalePendingHashes(t *testing.T) {
	// A pending identifier the full response omits is pruned silently:
	// no event, no publication — a later partial reporting it would be
	// rejected with no remaining full-resync trigger.
	c := &cache{owned: setOf(testHash, otherHash)}
	c.ensureMaps()
	c.pendingFull[otherHash] = struct{}{}
	c.forceFull = true

	disposition, changed, removed := c.merge(maindata{
		Rid:        2,
		FullUpdate: true,
		Torrents:   fullTorrents(map[string]string{testHash: torrentBody(testHash, "downloading")}),
	})
	require.Equal(t, mergeApplied, disposition)
	require.Equal(t, []string{testHash}, changed)
	require.Empty(t, removed, "an omitted pending identifier is not a removal event")
	require.Empty(t, c.pendingFull)
	require.NotContains(t, c.fields, otherHash)
	require.False(t, c.forceFull, "the accepted full update cleared the flag")
}

func TestFullSnapshotPublishesPendingHash(t *testing.T) {
	// A pending identifier the full response reports is published
	// directly: the response carries complete fields, so no further
	// resync is owed.
	c := &cache{owned: setOf(testHash, otherHash)}
	_, _, _ = c.merge(maindata{
		Rid:        1,
		FullUpdate: true,
		Torrents:   fullTorrents(map[string]string{testHash: torrentBody(testHash, "downloading")}),
	})
	c.pendingFull[otherHash] = struct{}{}
	c.forceFull = true

	disposition, changed, removed := c.merge(maindata{
		Rid:        7,
		FullUpdate: true,
		Torrents:   fullTorrents(map[string]string{testHash: torrentBody(testHash, "downloading"), otherHash: torrentBody(otherHash, "downloading")}),
	})
	require.Equal(t, mergeApplied, disposition)
	require.Equal(t, []string{otherHash}, changed)
	require.Empty(t, removed)
	require.Contains(t, c.fields, otherHash)
	require.Empty(t, c.pendingFull)
	require.False(t, c.forceFull)
	require.Equal(t, 7, c.rid)
}

// fullTorrents renders one maindata torrents map from hash -> raw object
// pairs.
func fullTorrents(torrents map[string]string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(torrents))
	for hash, obj := range torrents {
		out[hash] = json.RawMessage(obj)
	}
	return out
}

// maindataServer is a WebAPI stand-in for the sync tests. It answers the
// login and version probes Connect needs, then serves sync/maindata from a
// scripted function the test can swap between polls. The function runs on
// the handler goroutine, so it must not call t.Fatalf.
type maindataServer struct {
	t   *testing.T
	srv *httptest.Server

	reply func(rid int) (status int, body string) // the scripted sync/maindata answer

	mu    sync.Mutex
	rids  []string // every rid the client presented, in order
	delay time.Duration
	// gateAfter holds one request — the (0-based) gateAfter-th — until
	// maindataRelease closes, for mid-flight epoch tests. -1 disables.
	requests    int
	gateAfter   int
	gateReached chan struct{}
	gateRelease chan struct{}

	webapiReached chan struct{} // optional gate for a Connect/Close race
	webapiRelease chan struct{}
}

func newMaindataServer(t *testing.T, reply func(rid int) (int, string)) *maindataServer {
	t.Helper()

	f := &maindataServer{t: t, reply: reply, gateAfter: -1}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *maindataServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v2/auth/login":
		http.SetCookie(w, &http.Cookie{Name: testCookie, Value: "sid-sync", Path: "/", HttpOnly: true})
		w.WriteHeader(http.StatusNoContent)

	case "/api/v2/app/version":
		_, _ = w.Write([]byte(testVersion))

	case "/api/v2/app/webapiVersion":
		if f.webapiReached != nil {
			select {
			case f.webapiReached <- struct{}{}:
			default:
			}
			<-f.webapiRelease
		}
		_, _ = w.Write([]byte(testWebapi))

	case "/api/v2/sync/maindata":
		rid, err := strconv.Atoi(r.URL.Query().Get("rid"))
		if err != nil {
			f.t.Errorf("sync/maindata without a numeric rid: %q", r.URL.RawQuery)
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.rids = append(f.rids, r.URL.Query().Get("rid"))
		n := f.requests
		f.requests++
		hold := n == f.gateAfter
		gateReached, gateRelease := f.gateReached, f.gateRelease
		delay := f.delay
		f.mu.Unlock()

		if hold {
			gateReached <- struct{}{}
			<-gateRelease
		}
		if delay > 0 {
			time.Sleep(delay)
		}

		status, body := f.reply(rid)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))

	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}

// ridsSeen returns the rid sequence the client has presented so far.
func (f *maindataServer) ridsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rids)
}

// setDelay arms or clears the artificial handler delay (timeouts).
func (f *maindataServer) setDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = d
}

// armGate holds the (0-based) nth maindata request until release closes.
func (f *maindataServer) armGate(n int) (reached, release chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gateAfter = n
	f.gateReached = make(chan struct{}, 1)
	f.gateRelease = make(chan struct{})
	return f.gateReached, f.gateRelease
}

// connectedSyncClient returns a client past Connect, polling at test
// speed against one maindataServer. The ownership source is installed
// before Connect: the loop starts with Connect, and a first poll under
// the default-deny source would reject the full snapshot a test scripts.
func connectedSyncClient(t *testing.T, f *maindataServer, owned map[string]struct{}) *Client {
	t.Helper()

	c, err := New(Config{BaseURL: f.srv.URL, Username: testUsername, Password: testPassword}, nil)
	require.NoError(t, err)
	c.md.pollEvery = 5 * time.Millisecond
	if owned != nil {
		c.SetOwnershipFilter(staticSource(owned))
	}
	require.NoError(t, c.Connect(context.Background()))
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	return c
}

// fullBody renders a full_update over the given hash -> object pairs,
// hash-sorted for determinism. Plain string building: it runs on the fake's
// handler goroutine, where testify's FailNow would Goexit the wrong one.
func fullBody(rid int, torrents map[string]string) string {
	parts := make([]string, 0, len(torrents))
	for hash, obj := range torrents {
		parts = append(parts, `"`+hash+`":`+obj)
	}
	slices.Sort(parts)
	return `{"rid":` + strconv.Itoa(rid) + `,"full_update":true,"torrents":{` + strings.Join(parts, ",") + `}}`
}

// torrentBody is one torrents/info-shaped object for the fake daemon.
func torrentBody(hash, state string) string {
	return `{"hash":"` + hash + `","name":"n-` + hash[:6] + `","state":"` + state + `","progress":0.5,"dlspeed":1024,"save_path":"/data"}`
}

// torrentBodySpeed is torrentBody with a chosen dlspeed, so tests can
// observe one specific value through Get.
func torrentBodySpeed(hash, state string, dlspeed int) string {
	return `{"hash":"` + hash + `","name":"n-` + hash[:6] + `","state":"` + state + `","progress":0.5,"dlspeed":` + strconv.Itoa(dlspeed) + `,"save_path":"/data"}`
}

// drainEvents moves every buffered event out of one channel.
func drainEvents(events <-chan engine.TaskEvent) []engine.TaskEvent {
	var got []engine.TaskEvent
	for {
		select {
		case ev := <-events:
			got = append(got, ev)
		default:
			return got
		}
	}
}

// eventsOfKind filters one collected event slice.
func eventsOfKind(events []engine.TaskEvent, kind engine.EventKind) []engine.TaskEvent {
	var out []engine.TaskEvent
	for _, ev := range events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func TestTorrentsRemovedEmitsEventRemoved(t *testing.T) {
	step := 0
	f := newMaindataServer(t, func(rid int) (int, string) {
		step++
		switch step {
		case 1:
			return http.StatusOK, fullBody(1, map[string]string{
				testHash:  torrentBody(testHash, "downloading"),
				otherHash: torrentBody(otherHash, "downloading"),
			})
		default:
			// Only the removal; the survivor is untouched.
			return http.StatusOK, `{"rid":2,"torrents_removed":["` + testHash + `"]}`
		}
	})
	c := connectedSyncClient(t, f, setOf(testHash, otherHash))

	events := subscribeEvents(t, c)

	// Both hashes arrive first; the removal follows on the next tick.
	var removed []engine.TaskEvent
	require.Eventually(t, func() bool {
		removed = eventsOfKind(drainEvents(events), engine.EventRemoved)
		return len(removed) == 1
	}, 2*time.Second, 2*time.Millisecond, "no removal event arrived")

	require.Equal(t, engine.NameQBittorrent+":"+testHash, removed[0].TaskID)
	require.Nil(t, removed[0].Info)

	// The cache dropped the hash; the survivor stayed.
	require.Eventually(t, func() bool {
		_, err := c.Get(context.Background(), engine.NameQBittorrent+":"+testHash)
		return errors.Is(err, engine.ErrNotFound)
	}, 2*time.Second, 2*time.Millisecond)

	listed, err := c.List(context.Background())
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, engine.NameQBittorrent+":"+otherHash, listed[0].ID)
}

func TestMergePartialAddsNewAcceptedHash(t *testing.T) {
	// The daemon reports a hash the cache has never seen — accepted by
	// the snapshot — and the first-seen guarantee makes that object
	// complete: the partial inserts it and emits its change event.
	step := 0
	f := newMaindataServer(t, func(rid int) (int, string) {
		step++
		switch step {
		case 1:
			return http.StatusOK, fullBody(1, map[string]string{
				testHash: torrentBody(testHash, "downloading"),
			})
		case 2:
			return http.StatusOK, `{"rid":2,"torrents":{"` + otherHash + `":` + torrentBody(otherHash, "downloading") + `}}`
		default:
			return http.StatusOK, `{"rid":3,"torrents":{"` + testHash + `":{"dlspeed":4096}}}`
		}
	})
	c := connectedSyncClient(t, f, setOf(testHash, otherHash))
	events := subscribeEvents(t, c)

	require.Eventually(t, func() bool {
		listed, err := c.List(context.Background())
		return err == nil && len(listed) == 2
	}, 2*time.Second, 2*time.Millisecond, "the new accepted hash never entered the cache")

	var seen []engine.TaskEvent
	require.Eventually(t, func() bool {
		seen = append(seen, drainEvents(events)...)
		return len(eventsOfKind(seen, engine.EventProgress)) >= 2
	}, 2*time.Second, 2*time.Millisecond, "no change events arrived")

	// The inserted hash carries an event of its own.
	ids := make([]string, 0, len(seen))
	for _, ev := range seen {
		ids = append(ids, ev.TaskID)
	}
	require.Contains(t, ids, engine.NameQBittorrent+":"+otherHash)

	info, err := c.Get(context.Background(), engine.NameQBittorrent+":"+otherHash)
	require.NoError(t, err)
	require.Equal(t, engine.StateDownloading, info.State)
}

// t030Engine completes the engine.Engine interface around the T030
// adapter: the file calls of T032 and the mutators of T036/T037 are still
// absent from *Client, so this test-only wrapper carries the
// ErrNotSupported spellings the interface documents for a missing
// capability. NewReconciler's install then reaches the real client's
// SetOwnershipFilter through promotion.
type t030Engine struct {
	*Client
}

func (t030Engine) Files(context.Context, string) ([]engine.FileEntry, error) {
	return nil, engine.ErrNotSupported
}

func (t030Engine) SetFiles(context.Context, string, []int, map[int]int) error {
	return engine.ErrNotSupported
}

func (t030Engine) SetLocation(context.Context, string, string) error {
	return engine.ErrNotSupported
}

func (t030Engine) Rename(context.Context, string, string) error {
	return engine.ErrNotSupported
}

func (t030Engine) SetCategory(context.Context, string, string) error {
	return engine.ErrNotSupported
}

func (t030Engine) SetRateLimits(context.Context, string, *int64, *int64) error {
	return engine.ErrNotSupported
}

func (t030Engine) SetShareLimits(context.Context, string, *float64, *int64) error {
	return engine.ErrNotSupported
}

// Remove is the engine.Engine spelling, which always retains data; the
// remove-with-data action calls the embedded three-argument form directly.
func (e t030Engine) Remove(ctx context.Context, id string) error {
	return e.Client.Remove(ctx, id, false)
}

// syncTasks is the engine.TaskWriter surface NewReconciler needs: one
// engine's owned handles, consulted live by the ownership source, with a
// switch that makes the listing fail and a counter proving one listing
// serves a whole pass.
type syncTasks struct {
	mu       sync.Mutex
	fail     bool
	calls    int
	byEngine map[string]map[string]store.Reconcilable
}

func (f *syncTasks) ListNonTerminalByEngine(_ context.Context, engineName string) (map[string]store.Reconcilable, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return nil, errors.New("store: unavailable")
	}
	return f.byEngine[engineName], nil
}

// listCalls reports how many store listings ran.
func (f *syncTasks) listCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// setFail flips the store's availability.
func (f *syncTasks) setFail(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

// addRow registers one owned handle.
func (f *syncTasks) addRow(engineName, hash, taskID string, state engine.TaskState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byEngine == nil {
		f.byEngine = map[string]map[string]store.Reconcilable{}
	}
	if f.byEngine[engineName] == nil {
		f.byEngine[engineName] = map[string]store.Reconcilable{}
	}
	f.byEngine[engineName][hash] = store.Reconcilable{ID: taskID, EngineRef: hash, State: string(state)}
}

func (f *syncTasks) UpdateProgress(context.Context, string, store.Progress) error { return nil }
func (f *syncTasks) SetEngineRef(context.Context, string, string) error           { return nil }
func (f *syncTasks) Transition(context.Context, string, string, string, string) error {
	return nil
}
func (f *syncTasks) AppendEvent(context.Context, string, string, string, string, any) error {
	return nil
}

// syncAdmitter is the DiskFullPauser surface NewReconciler requires.
type syncAdmitter struct{}

func (syncAdmitter) PauseDiskFull(context.Context, string, error) error { return nil }

// reconcilerSyncClient wires a real client into a real Reconciler over the
// fake store, so the ownership source under test is exactly the one
// production installs.
func reconcilerSyncClient(t *testing.T, f *maindataServer, tasks *syncTasks) *Client {
	t.Helper()

	c, err := New(Config{BaseURL: f.srv.URL, Username: testUsername, Password: testPassword}, nil)
	require.NoError(t, err)
	c.md.pollEvery = 5 * time.Millisecond

	registry := engine.NewRegistry()
	registry.Register(t030Engine{Client: c})
	engine.NewReconciler(registry, tasks, syncAdmitter{}, time.Second, nil)

	require.NoError(t, c.Connect(context.Background()))
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	return c
}

func TestForeignHashIsInvisible(t *testing.T) {
	// The snapshot source comes from NewReconciler — installed over a
	// registry that holds the real client — not from the adapter's
	// default.
	f := newMaindataServer(t, func(rid int) (int, string) {
		if rid == 0 {
			return http.StatusOK, fullBody(1, map[string]string{
				testHash:  torrentBody(testHash, "downloading"),
				otherHash: torrentBody(otherHash, "downloading"),
			})
		}
		// A moving value keeps one event per tick flowing for the owned
		// hash, so the subscriber cannot miss a one-shot burst.
		return http.StatusOK, `{"rid":` + strconv.Itoa(rid+1) + `,"torrents":{"` +
			testHash + `":{"dlspeed":` + strconv.Itoa(rid) + `},"` +
			otherHash + `":{"dlspeed":` + strconv.Itoa(rid) + `}}}`
	})

	tasks := &syncTasks{}
	tasks.addRow(engine.NameQBittorrent, testHash, "task-1", engine.StateDownloading)
	c := reconcilerSyncClient(t, f, tasks)
	events := subscribeEvents(t, c)

	// The foreign hash is in no List and no Get.
	require.Eventually(t, func() bool {
		listed, err := c.List(context.Background())
		return err == nil && len(listed) == 1
	}, 2*time.Second, 2*time.Millisecond)

	listed, err := c.List(context.Background())
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, engine.NameQBittorrent+":"+testHash, listed[0].ID)

	_, err = c.Get(context.Background(), engine.NameQBittorrent+":"+otherHash)
	require.ErrorIs(t, err, engine.ErrNotFound)
	_, err = c.Get(context.Background(), engine.NameQBittorrent+":"+testHash)
	require.NoError(t, err)

	// And in no TaskEvent: collect a few ticks' worth, then hold the
	// whole collection against the foreign id.
	var seen []engine.TaskEvent
	require.Eventually(t, func() bool {
		seen = append(seen, drainEvents(events)...)
		return len(seen) >= 3
	}, 2*time.Second, 2*time.Millisecond, "no events for the owned hash arrived")
	require.NotEmpty(t, seen)
	for _, ev := range seen {
		require.NotEqual(t, engine.NameQBittorrent+":"+otherHash, ev.TaskID)
	}

	// A withheld hash retains no fields: only its identifier is held, so
	// a later snapshot that owns it can be detected.
	c.md.mu.Lock()
	_, rejected := c.md.cache.rejected[otherHash]
	c.md.mu.Unlock()
	require.True(t, rejected, "the foreign hash must be retained as an identifier only")
}

func TestDefaultFilterOwnsNothing(t *testing.T) {
	// No SetOwnershipFilter at all: the cache holds nothing, so a caller
	// that forgets the source sees an empty queue, never a foreign
	// transfer.
	f := newMaindataServer(t, func(rid int) (int, string) {
		return http.StatusOK, fullBody(rid+1, map[string]string{
			testHash: torrentBody(testHash, "downloading"),
		})
	})
	c := connectedSyncClient(t, f, nil)

	require.Never(t, func() bool {
		listed, err := c.List(context.Background())
		return err == nil && len(listed) > 0
	}, 50*time.Millisecond, 5*time.Millisecond, "a hash entered the cache with no ownership source")
	// The window must have actually polled, or the Never above proved
	// nothing about the default source.
	require.NotEmpty(t, f.ridsSeen(), "the poll loop never ran; the Never above was vacuous")
}

func TestSetOwnershipFilterForcesFullResync(t *testing.T) {
	// A source installed after polls have run must heal the cache it
	// rejected: the install is one reset — rid 0 forced, rejected
	// identifiers moved to pendingFull — and the full snapshot under the
	// new source fills the cache. Deltas alone would never carry an idle
	// torrent that changed nothing since the rid the daemon remembers.
	f := newMaindataServer(t, func(rid int) (int, string) {
		if rid == 0 {
			return http.StatusOK, fullBody(1, map[string]string{
				testHash: torrentBody(testHash, "downloading"),
			})
		}
		return http.StatusOK, `{"rid":` + strconv.Itoa(rid+1) + `}`
	})
	c := connectedSyncClient(t, f, nil)

	require.Eventually(t, func() bool {
		return len(f.ridsSeen()) >= 2
	}, 2*time.Second, 2*time.Millisecond)

	c.SetOwnershipFilter(staticSource(setOf(testHash)))

	// The next request carries rid 0 — the install reset — and the full
	// snapshot under the new source fills the cache.
	require.Eventually(t, func() bool {
		listed, err := c.List(context.Background())
		return err == nil && len(listed) == 1
	}, 2*time.Second, 2*time.Millisecond)
	zeros := 0
	for _, seen := range f.ridsSeen() {
		if seen == "0" {
			zeros++
		}
	}
	require.Equal(t, 2, zeros, "the source install must force a second rid-0 request")
}

func TestOwnershipFilterImmediatelyDropsRejectedHash(t *testing.T) {
	// Installing a source is one reset: a visible hash the new snapshot
	// rejects loses its fields at once, silently — no event — and the
	// epoch bump forces the next request to rid 0.
	c := &Client{}
	events := subscribeEvents(t, c)
	c.SetOwnershipFilter(staticSource(setOf(testHash, otherHash)))
	c.applyResponse(maindata{
		Rid:        1,
		FullUpdate: true,
		Torrents:   fullTorrents(map[string]string{testHash: torrentBody(testHash, "downloading"), otherHash: torrentBody(otherHash, "downloading")}),
	}, currentEpoch(c))
	drainEvents(events)

	c.SetOwnershipFilter(staticSource(setOf(testHash)))

	c.md.mu.Lock()
	_, visible := c.md.cache.fields[otherHash]
	_, rejected := c.md.cache.rejected[otherHash]
	epoch := c.md.cache.ownershipEpoch
	c.md.mu.Unlock()
	require.False(t, visible, "the newly rejected hash kept its fields")
	require.True(t, rejected, "the newly rejected hash kept no identifier")
	require.Equal(t, uint64(2), epoch, "the install must bump the epoch exactly once")

	require.Empty(t, drainEvents(events), "the install drop is silent")

	rid, _ := c.requestPlan()
	require.Equal(t, 0, rid, "the install must force a full resync")

	_, err := c.Get(context.Background(), engine.NameQBittorrent+":"+otherHash)
	require.ErrorIs(t, err, engine.ErrNotFound)
}

func TestDeltaRevokingOwnershipDropsHash(t *testing.T) {
	// A delta that reports a hash the refreshed snapshot no longer
	// accepts: the hash drops silently, the response still applies, and
	// neither the epoch nor the force-full flag moves.
	c := &Client{}
	events := subscribeEvents(t, c)
	c.SetOwnershipFilter(staticSource(setOf(testHash)))
	epoch := currentEpoch(c)
	c.applyResponse(maindata{
		Rid:        5,
		FullUpdate: true,
		Torrents:   fullTorrents(map[string]string{testHash: torrentBodySpeed(testHash, "downloading", 2048)}),
	}, epoch)
	drainEvents(events)

	// The listing refreshed and stopped owning the hash. Swapping the
	// source's result under the mutex is how a TTL refresh changes what a
	// fixed source returns.
	c.md.mu.Lock()
	c.md.cache.ownershipSource = staticSource(nil)
	c.md.mu.Unlock()

	c.applyResponse(maindata{
		Rid:             6,
		Torrents:        fullTorrents(map[string]string{testHash: `{"dlspeed":999}`}),
		TorrentsRemoved: []string{otherHash},
	}, currentEpoch(c))

	c.md.mu.Lock()
	require.Empty(t, c.md.cache.fields, "the revoked hash stayed visible")
	_, rejected := c.md.cache.rejected[testHash]
	require.True(t, rejected)
	require.Equal(t, 6, c.md.cache.rid, "the applied delta advanced the rid")
	require.Equal(t, epoch, c.md.cache.ownershipEpoch, "a recheck drop is not a reset")
	require.False(t, c.md.cache.forceFull, "a recheck drop forces nothing")
	c.md.mu.Unlock()
	require.Empty(t, drainEvents(events), "an ownership rejection emits no removal event")
}

func TestOwnershipPrecheckDropsWithoutReset(t *testing.T) {
	// The pre-request pass drops a visible hash the refreshed snapshot
	// rejects, but — unlike an install — without the force-full flag or
	// an epoch increment, so the request keeps the current rid.
	c := &Client{}
	c.md.mu.Lock()
	c.md.cache.ownershipSource = staticSource(setOf(testHash, otherHash))
	c.md.cache.owned = setOf(testHash, otherHash)
	c.md.cache.ensureMaps()
	seedTorrent(t, &c.md.cache, testHash, "downloading")
	seedTorrent(t, &c.md.cache, otherHash, "downloading")
	c.md.cache.rid = 4
	c.md.mu.Unlock()

	// The next listing owns only the first hash.
	c.md.mu.Lock()
	c.md.cache.ownershipSource = staticSource(setOf(testHash))
	c.md.mu.Unlock()

	require.True(t, c.ownershipPrepass())

	c.md.mu.Lock()
	_, visible := c.md.cache.fields[otherHash]
	require.False(t, visible)
	_, rejected := c.md.cache.rejected[otherHash]
	require.True(t, rejected)
	require.Empty(t, c.md.cache.pendingFull)
	epoch := c.md.cache.ownershipEpoch
	forceFull := c.md.cache.forceFull
	c.md.mu.Unlock()
	require.Equal(t, uint64(0), epoch, "a precheck drop is not a reset")
	require.False(t, forceFull, "a precheck drop forces no full resync")

	rid, reqEpoch := c.requestPlan()
	require.Equal(t, 4, rid, "the next request keeps the accepted rid")
	require.Equal(t, epoch, reqEpoch)
}

func TestOwnershipRecheckUsesOneStoreListing(t *testing.T) {
	// One pass over any number of hashes performs at most one
	// ListNonTerminalByEngine read: the snapshot design answers every
	// membership check from the set it already holds.
	const hashes = 40

	tasks := &syncTasks{}
	for i := 0; i < hashes; i++ {
		hash := fmt.Sprintf("%040x", i)
		if i%2 == 0 {
			tasks.addRow(engine.NameQBittorrent, hash, "task-"+strconv.Itoa(i), engine.StateDownloading)
		}
	}
	r := engine.NewReconciler(engine.NewRegistry(), tasks, syncAdmitter{}, time.Second, nil)

	c := &Client{}
	c.md.mu.Lock()
	c.md.cache.ownershipSource = r.OwnedRefs(engine.NameQBittorrent)
	c.md.cache.owned = setOf() // the previous snapshot owned nothing: every hash was rejected
	c.md.cache.ensureMaps()
	for i := 0; i < hashes; i++ {
		c.md.cache.rejected[fmt.Sprintf("%040x", i)] = struct{}{}
	}
	c.md.mu.Unlock()

	require.True(t, c.ownershipPrepass())
	require.Equal(t, 1, tasks.listCalls(), "one pass must make exactly one store listing")

	// A second pass inside the TTL window is served from the memo.
	require.True(t, c.ownershipPrepass())
	require.Equal(t, 1, tasks.listCalls())

	// The reclassification did happen: half the rejected identifiers
	// moved to pendingFull, one reset for the whole pass.
	c.md.mu.Lock()
	require.Len(t, c.md.cache.pendingFull, hashes/2)
	require.Equal(t, uint64(1), c.md.cache.ownershipEpoch)
	require.True(t, c.md.cache.forceFull)
	c.md.mu.Unlock()
}

func TestOwnershipRefreshDoesNotBlockCacheReads(t *testing.T) {
	// The source runs outside the cache mutex, so a slow store listing
	// never blocks List, Get or event publication.
	c := &Client{}
	events := subscribeEvents(t, c)
	c.md.mu.Lock()
	c.md.cache.ownershipSource = staticSource(setOf(testHash))
	c.md.cache.owned = setOf(testHash)
	c.md.cache.ensureMaps()
	seedTorrent(t, &c.md.cache, testHash, "downloading")
	c.md.mu.Unlock()

	blocked := make(chan struct{})
	release := make(chan struct{})
	var signalOnce sync.Once
	c.md.mu.Lock()
	c.md.cache.ownershipSource = func() map[string]struct{} {
		signalOnce.Do(func() { close(blocked) })
		<-release
		return setOf(testHash)
	}
	c.md.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		require.True(t, c.ownershipPrepass())
	}()
	<-blocked

	listDone := make(chan struct{})
	go func() {
		defer close(listDone)
		_, err := c.List(context.Background())
		require.NoError(t, err)
	}()
	select {
	case <-listDone:
	case <-time.After(2 * time.Second):
		t.Fatal("List blocked behind the ownership source's store read")
	}

	if _, err := c.Get(context.Background(), engine.NameQBittorrent+":"+testHash); err != nil {
		t.Fatalf("Get blocked or failed behind the ownership source: %v", err)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the prepass never finished after the source released")
	}

	// Event publication survives the pass too: a later response emits.
	c.applyResponse(maindata{
		Rid:      2,
		Torrents: fullTorrents(map[string]string{testHash: `{"dlspeed":4096}`}),
	}, currentEpoch(c))
	require.Eventually(t, func() bool {
		return len(drainEvents(events)) == 1
	}, 2*time.Second, 2*time.Millisecond, "no event followed the pass")
}

func TestOwnedRefsFailsClosedBeforeFirstSuccess(t *testing.T) {
	// Before the first successful listing there is no prior truth to
	// serve, so the source answers empty — never admitting an
	// unaccounted transfer on the strength of a failed read. The first
	// recovered snapshot owns the row again.
	tasks := &syncTasks{}
	tasks.addRow(engine.NameQBittorrent, testHash, "task-1", engine.StateDownloading)
	tasks.setFail(true)
	r := engine.NewReconciler(engine.NewRegistry(), tasks, syncAdmitter{}, time.Second, nil)

	require.Empty(t, r.OwnedRefs(engine.NameQBittorrent)())

	tasks.setFail(false)
	require.Eventually(t, func() bool {
		_, owned := r.OwnedRefs(engine.NameQBittorrent)()[testHash]
		return owned
	}, 3*time.Second, 50*time.Millisecond, "store recovery never re-owned the handle")
}

func TestOwnershipTransitionResetsOnce(t *testing.T) {
	// A rejected hash the listing comes to own moves to pendingFull —
	// one reset, one epoch increment for the pass. Later passes over the
	// same still-pending hash never retrigger it, and the forced full
	// response publishes the hash directly.
	c := &Client{}
	events := subscribeEvents(t, c)
	c.md.mu.Lock()
	c.md.cache.ownershipSource = staticSource(setOf(testHash, otherHash))
	c.md.cache.owned = setOf(testHash)
	c.md.cache.ensureMaps()
	seedTorrent(t, &c.md.cache, testHash, "downloading")
	c.md.cache.rejected[otherHash] = struct{}{}
	c.md.mu.Unlock()

	require.True(t, c.ownershipPrepass())
	require.True(t, c.ownershipPrepass())
	require.True(t, c.ownershipPrepass())

	c.md.mu.Lock()
	require.Len(t, c.md.cache.pendingFull, 1)
	_, pending := c.md.cache.pendingFull[otherHash]
	require.True(t, pending)
	require.Equal(t, uint64(1), c.md.cache.ownershipEpoch, "the transition must reset exactly once across passes")
	require.True(t, c.md.cache.forceFull)
	c.md.mu.Unlock()

	rid, reqEpoch := c.requestPlan()
	require.Equal(t, 0, rid, "the reset forces a full resync")
	require.Equal(t, uint64(1), reqEpoch)

	// The forced full response publishes the pending hash and clears
	// both the set and the flag.
	c.applyResponse(maindata{
		Rid:        9,
		FullUpdate: true,
		Torrents:   fullTorrents(map[string]string{testHash: torrentBody(testHash, "downloading"), otherHash: torrentBody(otherHash, "downloading")}),
	}, currentEpoch(c))

	info, err := c.Get(context.Background(), engine.NameQBittorrent+":"+otherHash)
	require.NoError(t, err)
	require.Equal(t, engine.StateDownloading, info.State)

	c.md.mu.Lock()
	require.Empty(t, c.md.cache.pendingFull)
	require.False(t, c.md.cache.forceFull)
	require.Equal(t, 9, c.md.cache.rid)
	c.md.mu.Unlock()

	// The publication is exactly one event; no removal for the moved
	// identifier.
	require.Len(t, drainEvents(events), 1)
}

func TestRejectedPartialAppliesNothing(t *testing.T) {
	// While an accepted hash remains in pendingFull, a partial applies
	// none of its payload — the removal included — accepts no rid and
	// emits no event, and no epoch is consumed.
	c := &Client{}
	events := subscribeEvents(t, c)
	c.md.mu.Lock()
	c.md.cache.ownershipSource = staticSource(setOf(testHash, otherHash))
	c.md.cache.owned = setOf(testHash, otherHash)
	c.md.cache.ensureMaps()
	seedTorrent(t, &c.md.cache, testHash, "downloading")
	c.md.cache.pendingFull[otherHash] = struct{}{}
	c.md.cache.forceFull = true
	c.md.cache.rid = 3
	c.md.cache.ownershipEpoch = 7
	epoch := c.md.cache.ownershipEpoch
	c.md.mu.Unlock()

	c.applyResponse(maindata{
		Rid:             4,
		Torrents:        fullTorrents(map[string]string{testHash: `{"state":"pausedDL"}`}),
		TorrentsRemoved: []string{testHash},
	}, epoch)

	c.md.mu.Lock()
	require.Equal(t, "downloading", c.md.cache.fields[testHash]["state"], "the rejected partial changed a field")
	require.Contains(t, c.md.cache.fields, testHash, "the rejected partial removed a hash")
	require.Equal(t, 3, c.md.cache.rid, "the rejected partial advanced the rid")
	require.Len(t, c.md.cache.pendingFull, 1)
	require.Equal(t, uint64(7), c.md.cache.ownershipEpoch)
	require.True(t, c.md.cache.forceFull)
	c.md.mu.Unlock()
	require.Empty(t, drainEvents(events))
}

func TestPendingHashRevocationAllowsPartial(t *testing.T) {
	// A pending hash the refreshed snapshot rejects returns to
	// rejected without a reset, and the partial that follows applies.
	c := &Client{}
	events := subscribeEvents(t, c)
	c.SetOwnershipFilter(staticSource(setOf(testHash, otherHash)))
	epoch := currentEpoch(c)
	c.applyResponse(maindata{
		Rid:        1,
		FullUpdate: true,
		Torrents:   fullTorrents(map[string]string{testHash: torrentBody(testHash, "downloading")}),
	}, epoch)
	drainEvents(events)

	// B becomes owned-but-unseen, then the listing stops owning it.
	c.md.mu.Lock()
	c.md.cache.pendingFull[otherHash] = struct{}{}
	c.md.cache.forceFull = true
	c.md.cache.ownershipSource = staticSource(setOf(testHash))
	c.md.mu.Unlock()
	epoch = currentEpoch(c)

	c.applyResponse(maindata{
		Rid:      7,
		Torrents: fullTorrents(map[string]string{testHash: `{"state":"pausedDL"}`}),
	}, epoch)

	c.md.mu.Lock()
	require.Equal(t, "pausedDL", c.md.cache.fields[testHash]["state"], "the partial did not apply")
	require.Empty(t, c.md.cache.pendingFull, "the demoted hash stayed pending")
	_, rejected := c.md.cache.rejected[otherHash]
	require.True(t, rejected, "the demoted hash kept no identifier")
	require.Equal(t, 7, c.md.cache.rid)
	require.Equal(t, epoch, c.md.cache.ownershipEpoch, "the demotion must not reset")
	c.md.mu.Unlock()

	// The flag stays up until the next accepted full update — the
	// conservative reading — and no event names the demoted hash.
	rid, _ := c.requestPlan()
	require.Equal(t, 0, rid)
	for _, ev := range drainEvents(events) {
		require.NotEqual(t, engine.NameQBittorrent+":"+otherHash, ev.TaskID)
	}
}

func TestRejectedHashBecomesVisibleAfterOwnershipRefresh(t *testing.T) {
	var daemonMu sync.Mutex
	daemonHasOther := false
	deltaSent := false

	f := newMaindataServer(t, func(rid int) (int, string) {
		daemonMu.Lock()
		defer daemonMu.Unlock()

		torrents := map[string]string{testHash: torrentBody(testHash, "downloading")}
		if daemonHasOther {
			torrents[otherHash] = torrentBody(otherHash, "pausedDL")
		}
		if rid == 0 {
			return http.StatusOK, fullBody(1, torrents)
		}
		if daemonHasOther && !deltaSent {
			deltaSent = true
			return http.StatusOK, `{"rid":` + strconv.Itoa(rid+1) + `,"torrents":{"` +
				otherHash + `":` + torrentBody(otherHash, "pausedDL") + `}}`
		}
		return http.StatusOK, `{"rid":` + strconv.Itoa(rid+1) + `}`
	})

	tasks := &syncTasks{}
	tasks.addRow(engine.NameQBittorrent, testHash, "task-1", engine.StateDownloading)
	c := reconcilerSyncClient(t, f, tasks)

	require.Eventually(t, func() bool {
		listed, listErr := c.List(context.Background())
		return listErr == nil && len(listed) == 1
	}, 2*time.Second, 2*time.Millisecond)

	// First reject the daemon's one-shot add delta while the store does
	// not own it. Only then add the task, forcing the recovery path.
	daemonMu.Lock()
	daemonHasOther = true
	daemonMu.Unlock()
	require.Eventually(t, func() bool {
		daemonMu.Lock()
		defer daemonMu.Unlock()
		return deltaSent
	}, 2*time.Second, 2*time.Millisecond, "the one-shot add delta was not sent")
	require.Eventually(t, func() bool {
		c.md.mu.Lock()
		defer c.md.mu.Unlock()
		_, rejected := c.md.cache.rejected[otherHash]
		return rejected
	}, 2*time.Second, 2*time.Millisecond, "the add delta was not rejected")

	// A foreign transfer stays rejected while no row owns it.
	require.Never(t, func() bool {
		listed, listErr := c.List(context.Background())
		return listErr == nil && len(listed) > 1
	}, 300*time.Millisecond, 10*time.Millisecond, "the foreign hash became visible before ownership")

	tasks.addRow(engine.NameQBittorrent, otherHash, "task-2", engine.StatePaused)
	require.Eventually(t, func() bool {
		listed, listErr := c.List(context.Background())
		return listErr == nil && len(listed) == 2
	}, 3*time.Second, 5*time.Millisecond, "the rejected hash never recovered")

	// The recovery consumed exactly one forced full update; polling then
	// left rid 0 behind.
	require.Eventually(t, func() bool {
		seen := f.ridsSeen()
		return len(seen) > 0 && seen[len(seen)-1] != "0"
	}, 3*time.Second, 5*time.Millisecond, "polling remained stuck at rid 0")
	zeros := 0
	for _, seen := range f.ridsSeen() {
		if seen == "0" {
			zeros++
		}
	}
	require.Equal(t, 2, zeros, "ownership recovery must request one extra full update")

	c.md.mu.Lock()
	require.Empty(t, c.md.cache.pendingFull, "the published hash left pendingFull behind")
	require.False(t, c.md.cache.forceFull)
	c.md.mu.Unlock()
}

func TestOwnershipListingRecoveryRestoresInitialFullUpdate(t *testing.T) {
	// The store is down while the loop starts: ownership fails closed,
	// so the boot snapshot's hash is rejected and the queue serves
	// empty. Store recovery forces one full resync and the hash returns.
	f := newMaindataServer(t, func(rid int) (int, string) {
		if rid == 0 {
			return http.StatusOK, fullBody(1, map[string]string{
				testHash: torrentBody(testHash, "pausedDL"),
			})
		}
		return http.StatusOK, `{"rid":` + strconv.Itoa(rid+1) + `}`
	})

	tasks := &syncTasks{}
	tasks.addRow(engine.NameQBittorrent, testHash, "task-1", engine.StatePaused)
	tasks.setFail(true)
	c := reconcilerSyncClient(t, f, tasks)

	require.Never(t, func() bool {
		listed, listErr := c.List(context.Background())
		return listErr == nil && len(listed) > 0
	}, 300*time.Millisecond, 10*time.Millisecond, "a hash became owned while every listing failed")
	require.NotEmpty(t, f.ridsSeen(), "the poll loop never ran; the Never above was vacuous")

	tasks.setFail(false)
	require.Eventually(t, func() bool {
		listed, listErr := c.List(context.Background())
		return listErr == nil && len(listed) == 1
	}, 3*time.Second, 5*time.Millisecond, "store recovery did not restore the initial hash")
}

func TestOwnershipResetRejectsStaleResponse(t *testing.T) {
	// newSeededClient returns a client with one owned, visible hash and
	// a recorded epoch, plus its event subscriber.
	seed := func(t *testing.T) (*Client, <-chan engine.TaskEvent) {
		t.Helper()
		c := &Client{}
		events := subscribeEvents(t, c)
		c.SetOwnershipFilter(staticSource(setOf(testHash)))
		c.applyResponse(maindata{
			Rid:        5,
			FullUpdate: true,
			Torrents:   fullTorrents(map[string]string{testHash: torrentBodySpeed(testHash, "downloading", 2048)}),
		}, currentEpoch(c))
		drainEvents(events)
		return c, events
	}

	t.Run("delta", func(t *testing.T) {
		c, events := seed(t)
		c.md.mu.Lock()
		c.md.cache.rejected[otherHash] = struct{}{}
		c.md.mu.Unlock()

		// An install lands while the request is in flight: its epoch
		// bump makes the captured reply stale.
		c.SetOwnershipFilter(staticSource(setOf(testHash)))

		c.applyResponse(maindata{
			Rid:      6,
			Torrents: fullTorrents(map[string]string{testHash: `{"dlspeed":999}`}),
		}, 1) // the epoch the request captured, one before the install

		c.md.mu.Lock()
		require.Equal(t, 2048.0, c.md.cache.fields[testHash]["dlspeed"], "a stale delta published cache state")
		require.Equal(t, 5, c.md.cache.rid, "a stale delta advanced the rid")
		require.True(t, c.md.cache.forceFull, "a stale delta consumed the install's reset")
		c.md.mu.Unlock()
		require.Empty(t, drainEvents(events), "a stale delta emitted")

		rid, _ := c.requestPlan()
		require.Equal(t, 0, rid, "the install's forced resync must still happen")
	})

	t.Run("full", func(t *testing.T) {
		c, events := seed(t)
		staleInterval := time.Now().Add(-time.Minute)
		c.md.mu.Lock()
		c.md.cache.rejected[otherHash] = struct{}{}
		c.md.cache.lastFullAt = staleInterval
		c.md.mu.Unlock()

		c.SetOwnershipFilter(staticSource(setOf(testHash)))

		// A stale full response: it would prune the withheld identifier
		// and restart the interval if it were applied. It must not.
		c.applyResponse(maindata{
			Rid:        6,
			FullUpdate: true,
			Torrents:   fullTorrents(map[string]string{testHash: torrentBodySpeed(testHash, "downloading", 999)}),
		}, 1)

		c.md.mu.Lock()
		require.Equal(t, 2048.0, c.md.cache.fields[testHash]["dlspeed"], "a stale full response published cache state")
		_, rejected := c.md.cache.rejected[otherHash]
		require.True(t, rejected, "a stale full response published withheld identifiers")
		require.Equal(t, 5, c.md.cache.rid)
		require.True(t, c.md.cache.forceFull, "a stale full response cleared the force-full flag")
		require.Equal(t, staleInterval, c.md.cache.lastFullAt, "a stale full response restarted the interval")
		c.md.mu.Unlock()
		require.Empty(t, drainEvents(events))
	})

	t.Run("wire", func(t *testing.T) {
		// The whole loop: a request held mid-flight while a source
		// replacement lands is discarded, and the install's forced
		// resync follows on the wire.
		step := 0
		f := newMaindataServer(t, func(rid int) (int, string) {
			step++
			switch {
			case rid == 0:
				return http.StatusOK, fullBody(1, map[string]string{
					testHash: torrentBodySpeed(testHash, "downloading", 111),
				})
			case step == 2:
				// Exactly the held request: a sentinel the cache
				// must never accept.
				return http.StatusOK, `{"rid":2,"torrents":{"` + testHash + `":{"dlspeed":999}}}`
			default:
				return http.StatusOK, `{"rid":` + strconv.Itoa(rid+1) + `}`
			}
		})

		c, err := New(Config{BaseURL: f.srv.URL, Username: testUsername, Password: testPassword}, nil)
		require.NoError(t, err)
		c.md.pollEvery = 5 * time.Millisecond
		c.SetOwnershipFilter(staticSource(setOf(testHash)))
		require.NoError(t, c.Connect(context.Background()))
		t.Cleanup(func() { require.NoError(t, c.Close()) })

		gateReached, gateRelease := f.armGate(1) // hold request #2
		<-gateReached
		c.SetOwnershipFilter(staticSource(setOf(testHash))) // epoch bump mid-flight
		close(gateRelease)

		// The install's forced resync runs on the wire, and the sentinel
		// never lands.
		require.Eventually(t, func() bool {
			seen := f.ridsSeen()
			return len(seen) >= 3 && seen[len(seen)-1] == "0"
		}, 2*time.Second, 2*time.Millisecond, "the forced full resync never ran")

		require.Never(t, func() bool {
			info, err := c.Get(context.Background(), engine.NameQBittorrent+":"+testHash)
			return err == nil && info.DownloadRate == 999
		}, 200*time.Millisecond, 5*time.Millisecond, "the stale delta published")
	})
}

func TestPollFailureKeepsCacheAndForcesFullUpdate(t *testing.T) {
	// Every poll-failure class keeps the last accepted cache and rid and
	// raises the shared force-full flag, so every later request sends
	// rid=0 until an accepted full response succeeds again.
	type failureMode uint8
	const (
		failNon2xx failureMode = iota
		failDecode
		failTimeout
		failTransport
	)

	run := func(t *testing.T, mode failureMode) {
		var failing bool
		var mu sync.Mutex
		f := newMaindataServer(t, func(rid int) (int, string) {
			mu.Lock()
			failingNow := failing
			mu.Unlock()
			if failingNow {
				switch mode {
				case failNon2xx:
					return http.StatusInternalServerError, "boom"
				case failDecode:
					return http.StatusOK, `{"rid":`
				}
				// failTimeout: the delay below makes the reply late.
			}
			if rid == 0 {
				return http.StatusOK, fullBody(9, map[string]string{
					testHash: torrentBodySpeed(testHash, "downloading", 4096),
				})
			}
			return http.StatusOK, `{"rid":8,"torrents":{"` + testHash + `":{"dlspeed":2048}}}`
		})

		cfg := Config{BaseURL: f.srv.URL, Username: testUsername, Password: testPassword}
		if mode == failTimeout {
			cfg.Timeout = 40 * time.Millisecond
		}
		c, err := New(cfg, nil)
		require.NoError(t, err)
		c.md.pollEvery = 5 * time.Millisecond
		c.SetOwnershipFilter(staticSource(setOf(testHash)))
		require.NoError(t, c.Connect(context.Background()))
		t.Cleanup(func() { require.NoError(t, c.Close()) })

		// The healthy phase: a full snapshot, then a delta the cache
		// holds once the failures begin.
		require.Eventually(t, func() bool {
			info, err := c.Get(context.Background(), engine.NameQBittorrent+":"+testHash)
			return err == nil && info.DownloadRate == 2048
		}, 2*time.Second, 2*time.Millisecond)

		before := len(f.ridsSeen())
		mu.Lock()
		failing = true
		mu.Unlock()
		switch mode {
		case failTimeout:
			f.setDelay(150 * time.Millisecond)
		case failTransport:
			f.srv.Close()
		}

		// Every request after the failure — at least the last three of
		// them — carries rid 0, and the cache keeps serving. A closed
		// listener takes no more requests, so the transport case reads the
		// flag from the cache instead of the wire.
		require.Eventually(t, func() bool {
			if mode == failTransport {
				c.md.mu.Lock()
				defer c.md.mu.Unlock()
				return c.md.cache.forceFull
			}
			seen := f.ridsSeen()
			if len(seen) < before+4 {
				return false
			}
			tail := seen[len(seen)-3:]
			for _, rid := range tail {
				if rid != "0" {
					return false
				}
			}
			return true
		}, 3*time.Second, 5*time.Millisecond, "failed polls did not force rid 0")

		listed, err := c.List(context.Background())
		require.NoError(t, err)
		require.Len(t, listed, 1)
		info, err := c.Get(context.Background(), engine.NameQBittorrent+":"+testHash)
		require.NoError(t, err)
		require.Equal(t, engine.StateDownloading, info.State)
		require.Equal(t, int64(2048), info.DownloadRate)

		if mode == failTransport {
			return // a closed listener cannot recover; the rid-0 tail above is the proof
		}

		// Recovery: the forced rid-0 request lands a full update, which
		// clears the flag, so polling leaves rid 0 behind.
		mu.Lock()
		failing = false
		mu.Unlock()
		f.setDelay(0)
		require.Eventually(t, func() bool {
			seen := f.ridsSeen()
			return len(seen) > 0 && seen[len(seen)-1] != "0"
		}, 3*time.Second, 5*time.Millisecond, "polling stayed stuck at rid 0 after recovery")
		c.md.mu.Lock()
		require.False(t, c.md.cache.forceFull, "the accepted full update did not clear the force-full flag")
		c.md.mu.Unlock()
	}

	for name, mode := range map[string]failureMode{
		"non-2xx":   failNon2xx,
		"decode":    failDecode,
		"timeout":   failTimeout,
		"transport": failTransport,
	} {
		t.Run(name, func(t *testing.T) { run(t, mode) })
	}
}

func TestPeriodicFullSync(t *testing.T) {
	// lastFullAt seeded past the interval forces the next request to
	// rid 0 — no five-minute wait — and the accepted full update
	// restarts the interval, so polling leaves rid 0 behind.
	f := newMaindataServer(t, func(rid int) (int, string) {
		if rid == 0 {
			return http.StatusOK, fullBody(1, map[string]string{
				testHash: torrentBody(testHash, "downloading"),
			})
		}
		return http.StatusOK, `{"rid":` + strconv.Itoa(rid+1) + `}`
	})
	c := connectedSyncClient(t, f, setOf(testHash))

	// The healthy phase must reach non-zero rids before the seeding, or
	// the forced 0 below proves nothing.
	require.Eventually(t, func() bool {
		seen := f.ridsSeen()
		return len(seen) >= 2 && seen[len(seen)-1] != "0"
	}, 2*time.Second, 2*time.Millisecond)
	at := len(f.ridsSeen())

	c.md.mu.Lock()
	c.md.cache.lastFullAt = time.Now().Add(-qbtFullSyncInterval - time.Second)
	c.md.mu.Unlock()

	require.Eventually(t, func() bool {
		seen := f.ridsSeen()
		return len(seen) > at && slices.Contains(seen[at:], "0")
	}, 2*time.Second, 2*time.Millisecond, "the interval expiry did not force rid 0")

	require.Eventually(t, func() bool {
		seen := f.ridsSeen()
		return len(seen) > 0 && seen[len(seen)-1] != "0"
	}, 2*time.Second, 2*time.Millisecond, "polling stayed stuck at rid 0 after the periodic full sync")

	c.md.mu.Lock()
	require.False(t, c.md.cache.forceFull)
	require.True(t, time.Since(c.md.cache.lastFullAt) < qbtFullSyncInterval)
	c.md.mu.Unlock()
}

func TestEventsDropDoesNotBlockPoll(t *testing.T) {
	// A subscriber that never reads must not stall the loop: the buffer
	// fills, events drop, and the cache keeps advancing past the 128-event
	// buffer — the drop path itself is what the count exceeds.
	f := newMaindataServer(t, func(rid int) (int, string) {
		if rid == 0 {
			return http.StatusOK, fullBody(1, map[string]string{
				testHash: torrentBody(testHash, "downloading"),
			})
		}
		return http.StatusOK, `{"rid":` + strconv.Itoa(rid+1) + `,"torrents":{"` + testHash + `":{"dlspeed":` + strconv.Itoa(rid) + `}}`
	})
	c := connectedSyncClient(t, f, setOf(testHash))

	_, err := c.Events(context.Background()) // registered, never read
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		info, err := c.Get(context.Background(), engine.NameQBittorrent+":"+testHash)
		return err == nil && info.DownloadRate > int64(eventsBuffer)+eventsBufferSlack
	}, 3*time.Second, 2*time.Millisecond, "the poll loop stalled on a full subscriber buffer")
}

// eventsBufferSlack pushes the drop test's counter past the subscriber
// buffer with margin, so the asserted value can only exist if the loop kept
// polling after the buffer filled and drops began.
const eventsBufferSlack = 64

func TestEngineRidStaysInsideCache(t *testing.T) {
	const privateRid = 987654321

	c := &Client{}
	events := subscribeEvents(t, c)
	c.SetOwnershipFilter(staticSource(setOf(testHash)))
	c.applyResponse(maindata{
		Rid:        privateRid,
		FullUpdate: true,
		Torrents:   fullTorrents(map[string]string{testHash: torrentBody(testHash, "downloading")}),
	}, currentEpoch(c))
	got := drainEvents(events)

	listed, err := c.List(context.Background())
	require.NoError(t, err)
	info, err := c.Get(context.Background(), engine.NameQBittorrent+":"+testHash)
	require.NoError(t, err)

	payload, err := json.Marshal(struct {
		List   []engine.TaskInfo
		Get    engine.TaskInfo
		Events []engine.TaskEvent
	}{List: listed, Get: info, Events: got})
	require.NoError(t, err)
	require.NotContains(t, string(payload), strconv.Itoa(privateRid))

	c.md.mu.Lock()
	rid := c.md.cache.rid
	c.md.mu.Unlock()
	require.Equal(t, privateRid, rid, "the adapter must retain its private protocol rid")
}

func TestPollLifecycleIsIdempotentAndCloseWins(t *testing.T) {
	c := &Client{}
	c.md.pollEvery = time.Hour

	c.startPoll()
	c.md.mu.Lock()
	firstDone := c.md.done
	c.md.mu.Unlock()
	require.NotNil(t, firstDone)

	c.startPoll()
	c.md.mu.Lock()
	secondDone := c.md.done
	c.md.mu.Unlock()
	require.Equal(t, firstDone, secondDone, "a second start replaced the running poll")

	c.stopPoll()
	select {
	case <-firstDone:
	default:
		t.Fatal("stopPoll returned before the poll goroutine exited")
	}

	c.startPoll()
	c.md.mu.Lock()
	startedAfterClose := c.md.started
	closed := c.md.closed
	c.md.mu.Unlock()
	require.False(t, startedAfterClose, "a poll started after Close won the race")
	require.True(t, closed)

	c.stopPoll() // an already-stopped tracker is a safe no-op
}

func TestCloseWinningConnectRacePreventsPoll(t *testing.T) {
	f := newMaindataServer(t, func(rid int) (int, string) {
		return http.StatusOK, `{"rid":` + strconv.Itoa(rid+1) + `}`
	})
	f.webapiReached = make(chan struct{}, 1)
	f.webapiRelease = make(chan struct{})
	t.Cleanup(func() {
		select {
		case f.webapiRelease <- struct{}{}:
		default:
		}
	})

	c, err := New(Config{BaseURL: f.srv.URL, Username: testUsername, Password: testPassword}, nil)
	require.NoError(t, err)
	c.md.pollEvery = 5 * time.Millisecond

	connected := make(chan error, 1)
	go func() { connected <- c.Connect(context.Background()) }()
	select {
	case <-f.webapiReached:
	case <-time.After(2 * time.Second):
		t.Fatal("Connect did not reach the gated WebAPI probe")
	}

	require.NoError(t, c.Close())
	f.webapiRelease <- struct{}{}
	require.NoError(t, <-connected)
	require.Never(t, func() bool {
		return len(f.ridsSeen()) > 0
	}, 50*time.Millisecond, 5*time.Millisecond, "Connect started a poll after Close returned")

	events, err := c.Events(context.Background())
	require.NoError(t, err)
	_, open := <-events
	require.False(t, open)
}

func TestStartPollRacingStopPollLeavesNoLoop(t *testing.T) {
	const attempts = 100

	for range attempts {
		c := &Client{}
		c.md.pollEvery = time.Hour

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.startPoll()
		}()
		go func() {
			defer wg.Done()
			c.stopPoll()
		}()
		wg.Wait()

		c.md.mu.Lock()
		started := c.md.started
		closed := c.md.closed
		c.md.mu.Unlock()
		require.False(t, started)
		require.True(t, closed)
	}
}

func TestEventsContextCancellationClosesChannel(t *testing.T) {
	c := &Client{}
	ctx, cancel := context.WithCancel(context.Background())
	events, err := c.Events(ctx)
	require.NoError(t, err)

	cancel()
	select {
	case _, open := <-events:
		require.False(t, open)
	case <-time.After(2 * time.Second):
		t.Fatal("event channel stayed open after context cancellation")
	}
	require.NoError(t, c.Close())
}

func TestConcurrentStopWaitsForPollExit(t *testing.T) {
	c := &Client{}
	loopDone := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	c.md.started = true
	c.md.done = loopDone
	c.md.cancel = func() { close(cancelled) }
	go func() {
		<-release
		close(loopDone)
	}()

	firstReturned := make(chan struct{})
	go func() {
		c.stopPoll()
		close(firstReturned)
	}()
	<-cancelled

	secondReturned := make(chan struct{})
	go func() {
		c.stopPoll()
		close(secondReturned)
	}()
	select {
	case <-secondReturned:
		t.Fatal("a concurrent stop returned while the poll loop was running")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	select {
	case <-firstReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("the first stop did not return after poll exit")
	}
	select {
	case <-secondReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("the concurrent stop did not return after poll exit")
	}
}

func TestCloseStopsPollGoroutine(t *testing.T) {
	f := newMaindataServer(t, func(rid int) (int, string) {
		return http.StatusOK, `{"rid":` + strconv.Itoa(rid+1) + `}`
	})

	c, err := New(Config{BaseURL: f.srv.URL, Username: testUsername, Password: testPassword}, nil)
	require.NoError(t, err)
	c.md.pollEvery = 5 * time.Millisecond
	c.SetOwnershipFilter(staticSource(setOf(testHash)))
	require.NoError(t, c.Connect(context.Background()))

	// A subscriber registered before Close: its channel closes with Close.
	events, err := c.Events(context.Background())
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return len(f.ridsSeen()) >= 3
	}, 2*time.Second, 2*time.Millisecond)

	require.NoError(t, c.Close())

	// Close returned, so the goroutine is gone: the rid sequence stops
	// growing however long the test waits.
	at := len(f.ridsSeen())
	require.Never(t, func() bool {
		return len(f.ridsSeen()) > at
	}, 100*time.Millisecond, 10*time.Millisecond, "a poll followed Close")

	_, open := <-events
	require.False(t, open, "Close must close a live subscriber's channel")

	fresh, err := c.Events(context.Background())
	require.NoError(t, err)
	_, open = <-fresh
	require.False(t, open, "Events after Close must return a closed channel")
}

// The Reconciler ownership tests live here — not reconcile_test.go —
// because T030's Files table names this file; they cover the other half of
// the filter exercised above.
func TestBootInstallsOwnershipFilterOnLateEngine(t *testing.T) {
	registry := engine.NewRegistry()
	tasks := &syncTasks{}
	tasks.addRow(engine.NameQBittorrent, testHash, "task-1", engine.StatePaused)
	r := engine.NewReconciler(registry, tasks, syncAdmitter{}, time.Second, nil)

	c := &Client{}
	registry.Register(t030Engine{Client: c})
	require.NoError(t, r.Boot(context.Background()))

	c.md.mu.Lock()
	source := c.md.cache.ownershipSource
	c.md.mu.Unlock()
	require.NotNil(t, source, "Boot must install the source on a late engine")

	set := source()
	require.Contains(t, set, testHash)
	require.NotContains(t, set, otherHash)
}

func TestOwnedRefsReadsTheLiveTasksTable(t *testing.T) {
	tasks := &syncTasks{}
	tasks.addRow(engine.NameQBittorrent, testHash, "task-1", engine.StateDownloading)
	r := engine.NewReconciler(engine.NewRegistry(), tasks, syncAdmitter{}, time.Second, nil)

	source := r.OwnedRefs(engine.NameQBittorrent)
	require.Contains(t, source(), testHash)
	require.NotContains(t, source(), otherHash)

	// A row removed at runtime stops owning its handle on the next
	// listing — the source is live, not a boot snapshot.
	tasks.mu.Lock()
	delete(tasks.byEngine[engine.NameQBittorrent], testHash)
	tasks.mu.Unlock()
	require.Eventually(t, func() bool {
		_, owned := source()[testHash]
		return !owned
	}, 3*time.Second, 20*time.Millisecond)
}
