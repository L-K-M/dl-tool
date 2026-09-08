package qbittorrent

import (
	"context"
	"encoding/json"
	"errors"
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

// mergeFixture merges the captured full update into a cache whose predicate
// owns every hash, the baseline state of the merge tests.
func mergeFixture(t *testing.T) *cache {
	t.Helper()

	c := &cache{owned: func(string) bool { return true }}
	changed, removed := c.merge(mustReadMaindata(t))
	require.Equal(t, fixtureHashes, changed)
	require.Empty(t, removed)
	return c
}

func TestMergeFullUpdate(t *testing.T) {
	c := mergeFixture(t)

	require.Equal(t, 1, c.rid)
	require.Len(t, c.fields, len(fixtureHashes))
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
	c := &cache{owned: func(string) bool { return true }}
	changed, removed := c.merge(full)
	require.Equal(t, []string{testHash}, changed)
	require.Empty(t, removed)

	partial := maindata{
		Rid: 15,
		Torrents: map[string]json.RawMessage{
			testHash: json.RawMessage(`{"state":"pausedUP"}`),
		},
	}
	changed, removed = c.merge(partial)
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

func TestMergeNoChangeEmitsNothing(t *testing.T) {
	// A delta that re-sends values already stored is not a change: the
	// daemon re-sends unchanged fields on some paths, and one event per
	// changed hash means unchanged is no event.
	c := mergeFixture(t)

	before := map[string]map[string]any{}
	for hash, fields := range c.fields {
		before[hash] = mapsClone(fields)
	}

	// The capture's own values for state and dlspeed, re-sent verbatim.
	partial := maindata{
		Rid: 2,
		Torrents: map[string]json.RawMessage{
			fixtureHashes[0]: json.RawMessage(`{"state":"downloading","dlspeed":64245008}`),
		},
	}
	changed, removed := c.merge(partial)
	require.Empty(t, changed)
	require.Empty(t, removed)
	require.Equal(t, 2, c.rid)

	for hash, fields := range c.fields {
		require.Equal(t, before[hash], fields)
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

func TestFullUpdateReAppliesOwnershipFilter(t *testing.T) {
	// The first full update owns both hashes; the row of the second is
	// then deleted, and the next full update must drop it from the cache —
	// silently: a hash the predicate now rejects appears in no report.
	owned := map[string]bool{fixtureHashes[0]: true, fixtureHashes[1]: true}
	c := &cache{owned: func(hash string) bool { return owned[hash] }}
	_, _ = c.merge(mustReadMaindata(t))
	require.Len(t, c.fields, 2)

	delete(owned, fixtureHashes[1])
	m := mustReadMaindata(t)
	changed, removed := c.merge(m)
	require.Empty(t, changed, "an identical full update changes nothing")
	require.Empty(t, removed, "a rejected hash disappears without a removal report")
	require.NotContains(t, c.fields, fixtureHashes[1])

	// An owned hash that vanished from the full response is removed and
	// reported — the re-added-and-then-deleted row is still a loss the
	// event consumer must hear about.
	delete(m.Torrents, fixtureHashes[0])
	changed, removed = c.merge(m)
	require.Empty(t, changed)
	require.Equal(t, []string{fixtureHashes[0]}, removed)
	require.NotContains(t, c.fields, fixtureHashes[0])
}

func TestFullUpdateDroppingFieldReportsChange(t *testing.T) {
	c := &cache{owned: func(string) bool { return true }}
	_, _ = c.merge(maindata{
		Rid:        1,
		FullUpdate: true,
		Torrents: map[string]json.RawMessage{
			testHash: json.RawMessage(`{"hash":"` + testHash + `","name":"old","state":"downloading"}`),
		},
	})

	changed, removed := c.merge(maindata{
		Rid:        2,
		FullUpdate: true,
		Torrents: map[string]json.RawMessage{
			testHash: json.RawMessage(`{"hash":"` + testHash + `","state":"downloading"}`),
		},
	})
	require.Equal(t, []string{testHash}, changed)
	require.Empty(t, removed)
	require.NotContains(t, c.fields[testHash], "name")
}

func TestMergePartialIntoZeroCacheDoesNotPanic(t *testing.T) {
	// A zero-value cache — nil fields map — must survive a partial that
	// arrives before any full response: the poll goroutine has no recover,
	// so a panic here would take the process down.
	c := &cache{owned: func(string) bool { return true }}
	changed, removed := c.merge(maindata{
		Rid: 3,
		Torrents: map[string]json.RawMessage{
			testHash: json.RawMessage(`{"state":"downloading"}`),
		},
	})
	require.Equal(t, []string{testHash}, changed)
	require.Empty(t, removed)
	require.Contains(t, c.fields, testHash)
}

func TestHashChangedAndRemovedInOneDeltaIsRemovedOnly(t *testing.T) {
	// One delta that both changes and removes the same hash: removal
	// wins, and no event may carry a zero-value info for it.
	c := &cache{owned: func(string) bool { return true }}
	_, _ = c.merge(maindata{
		Rid: 1, FullUpdate: true,
		Torrents: map[string]json.RawMessage{
			testHash: json.RawMessage(torrentBody(testHash, "downloading")),
		},
	})
	changed, removed := c.merge(maindata{
		Rid: 2,
		Torrents: map[string]json.RawMessage{
			testHash: json.RawMessage(`{"state":"pausedDL"}`),
		},
		TorrentsRemoved: []string{testHash},
	})
	require.Empty(t, changed)
	require.Equal(t, []string{testHash}, removed)
	require.NotContains(t, c.fields, testHash)
}

// maindataServer is a WebAPI stand-in for the sync tests. It answers the
// login and version probes Connect needs, then serves sync/maindata from a
// scripted function the test can swap between polls. The function runs on
// the handler goroutine, so it must not call t.Fatalf.
type maindataServer struct {
	t   *testing.T
	srv *httptest.Server

	mu    sync.Mutex
	rids  []string // every rid the client presented, in order
	reply func(rid int) (int, string)

	webapiReached chan struct{} // optional gate for a Connect/Close race
	webapiRelease chan struct{}
}

func newMaindataServer(t *testing.T, reply func(rid int) (int, string)) *maindataServer {
	t.Helper()

	f := &maindataServer{t: t, reply: reply}
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
		f.mu.Unlock()

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

// connectedSyncClient returns a client past Connect, polling at test
// speed against one maindataServer. The ownership filter is installed
// before Connect: the loop starts with Connect, and a first poll under the
// default-deny predicate would drop the full snapshot a test scripts.
func connectedSyncClient(t *testing.T, f *maindataServer, owned func(string) bool) *Client {
	t.Helper()

	c, err := New(Config{BaseURL: f.srv.URL, Username: testUsername, Password: testPassword}, nil)
	require.NoError(t, err)
	c.md.pollEvery = 5 * time.Millisecond
	if owned != nil {
		c.SetOwnershipFilter(owned)
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
	c := connectedSyncClient(t, f, func(string) bool { return true })

	events, err := c.Events(context.Background())
	require.NoError(t, err)

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
// engine's owned handles, consulted live by the ownership predicate, with
// a switch that makes the listing fail to exercise the stale window.
type syncTasks struct {
	mu       sync.Mutex
	fail     bool
	byEngine map[string]map[string]store.Reconcilable
}

func (f *syncTasks) ListNonTerminalByEngine(_ context.Context, engineName string) (map[string]store.Reconcilable, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errors.New("store: unavailable")
	}
	return f.byEngine[engineName], nil
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

func TestForeignHashIsInvisible(t *testing.T) {
	// The predicate comes from NewReconciler — installed over a registry
	// that holds the real client — not from the adapter's default.
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

	c, err := New(Config{BaseURL: f.srv.URL, Username: testUsername, Password: testPassword}, nil)
	require.NoError(t, err)
	c.md.pollEvery = 5 * time.Millisecond
	require.NoError(t, c.Connect(context.Background()))
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	registry := engine.NewRegistry()
	registry.Register(t030Engine{Client: c})

	// Exactly one of the two hashes has a tasks row.
	tasks := &syncTasks{byEngine: map[string]map[string]store.Reconcilable{
		engine.NameQBittorrent: {testHash: {ID: "task-1", EngineRef: testHash, State: string(engine.StateDownloading)}},
	}}
	engine.NewReconciler(registry, tasks, syncAdmitter{}, time.Second, nil)

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
	events, err := c.Events(context.Background())
	require.NoError(t, err)
	var seen []engine.TaskEvent
	require.Eventually(t, func() bool {
		seen = append(seen, drainEvents(events)...)
		return len(seen) >= 3
	}, 2*time.Second, 2*time.Millisecond, "no events for the owned hash arrived")
	require.NotEmpty(t, seen)
	for _, ev := range seen {
		require.NotEqual(t, engine.NameQBittorrent+":"+otherHash, ev.TaskID)
	}
}

func TestDefaultFilterOwnsNothing(t *testing.T) {
	// No SetOwnershipFilter at all: the cache holds nothing, so a caller
	// that forgets the filter sees an empty queue, never a foreign
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
	}, 50*time.Millisecond, 5*time.Millisecond, "a hash entered the cache with no ownership filter")
	// The window must have actually polled, or the Never above proved
	// nothing about the default filter.
	require.NotEmpty(t, f.ridsSeen(), "the poll loop never ran; the Never above was vacuous")
}

func TestSetOwnershipFilterForcesFullResync(t *testing.T) {
	// A filter installed after polls have run must heal the cache it
	// rejected: the rid resets, the next poll is a full snapshot, and the
	// now-owned hash enters — deltas alone would never carry an idle
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

	c.SetOwnershipFilter(func(string) bool { return true })

	// The next request carries rid 0 — the local resync — and the full
	// snapshot under the new filter fills the cache.
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
	require.Equal(t, 2, zeros, "the filter install must force a second rid-0 request")
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

	c, err := New(Config{BaseURL: f.srv.URL, Username: testUsername, Password: testPassword}, nil)
	require.NoError(t, err)
	c.md.pollEvery = 5 * time.Millisecond

	tasks := &syncTasks{byEngine: map[string]map[string]store.Reconcilable{
		engine.NameQBittorrent: {
			testHash: {ID: "task-1", EngineRef: testHash, State: string(engine.StateDownloading)},
		},
	}}
	registry := engine.NewRegistry()
	registry.Register(t030Engine{Client: c})
	engine.NewReconciler(registry, tasks, syncAdmitter{}, time.Second, nil)

	require.NoError(t, c.Connect(context.Background()))
	t.Cleanup(func() { require.NoError(t, c.Close()) })
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
		_, withheld := c.md.cache.withheld[otherHash]
		return withheld
	}, 2*time.Second, 2*time.Millisecond, "the add delta was not rejected")

	tasks.mu.Lock()
	tasks.byEngine[engine.NameQBittorrent][otherHash] = store.Reconcilable{
		ID: "task-2", EngineRef: otherHash, State: string(engine.StatePaused),
	}
	tasks.mu.Unlock()
	require.Eventually(t, func() bool {
		listed, listErr := c.List(context.Background())
		return listErr == nil && len(listed) == 2
	}, 2*time.Second, 2*time.Millisecond, "the rejected hash never recovered")

	// A successful full merge consumes the withheld identifier. A later
	// non-zero request proves the adapter did not enter a rid-0 loop.
	require.Eventually(t, func() bool {
		seen := f.ridsSeen()
		return len(seen) > 0 && seen[len(seen)-1] != "0"
	}, 2*time.Second, 2*time.Millisecond, "polling remained stuck at rid 0")
	zeros := 0
	for _, seen := range f.ridsSeen() {
		if seen == "0" {
			zeros++
		}
	}
	require.Equal(t, 2, zeros, "ownership recovery must request one extra full update")
}

func TestOwnershipListingRecoveryRestoresInitialFullUpdate(t *testing.T) {
	f := newMaindataServer(t, func(rid int) (int, string) {
		if rid == 0 {
			return http.StatusOK, fullBody(1, map[string]string{
				testHash: torrentBody(testHash, "pausedDL"),
			})
		}
		return http.StatusOK, `{"rid":` + strconv.Itoa(rid+1) + `}`
	})

	tasks := &syncTasks{
		fail: true,
		byEngine: map[string]map[string]store.Reconcilable{
			engine.NameQBittorrent: {
				testHash: {ID: "task-1", EngineRef: testHash, State: string(engine.StatePaused)},
			},
		},
	}
	c, err := New(Config{BaseURL: f.srv.URL, Username: testUsername, Password: testPassword}, nil)
	require.NoError(t, err)
	c.md.pollEvery = 5 * time.Millisecond
	registry := engine.NewRegistry()
	registry.Register(t030Engine{Client: c})
	engine.NewReconciler(registry, tasks, syncAdmitter{}, time.Second, nil)

	require.NoError(t, c.Connect(context.Background()))
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	require.Eventually(t, func() bool {
		c.md.mu.Lock()
		defer c.md.mu.Unlock()
		_, withheld := c.md.cache.withheld[testHash]
		return withheld
	}, 2*time.Second, 2*time.Millisecond, "the failed first listing did not reject the hash")

	tasks.mu.Lock()
	tasks.fail = false
	tasks.mu.Unlock()
	require.Eventually(t, func() bool {
		listed, listErr := c.List(context.Background())
		return listErr == nil && len(listed) == 1
	}, 2*time.Second, 2*time.Millisecond, "store recovery did not restore the initial hash")
}

func TestPollFailureKeepsCache(t *testing.T) {
	var fail bool
	var mu sync.Mutex
	f := newMaindataServer(t, func(rid int) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return http.StatusInternalServerError, "boom"
		}
		if rid == 0 {
			return http.StatusOK, fullBody(7, map[string]string{
				testHash: torrentBody(testHash, "downloading"),
			})
		}
		// The reply's rid is pinned at 8 so the sequence the client
		// presents stays deterministic however many partials land.
		return http.StatusOK, `{"rid":8,"torrents":{"` + testHash + `":{"dlspeed":2048}}}`
	})
	c := connectedSyncClient(t, f, func(string) bool { return true })

	require.Eventually(t, func() bool {
		info, err := c.Get(context.Background(), engine.NameQBittorrent+":"+testHash)
		return err == nil && info.DownloadRate == 2048
	}, 2*time.Second, 2*time.Millisecond)

	// From here on every poll fails. The cache and the rid must both
	// survive: List and Get keep answering, and every later request
	// carries the rid of the last successful reply, never a reset to 0.
	before := len(f.ridsSeen())
	mu.Lock()
	fail = true
	mu.Unlock()

	require.Eventually(t, func() bool {
		return len(f.ridsSeen()) >= before+3
	}, 2*time.Second, 2*time.Millisecond)

	for _, rid := range f.ridsSeen()[before:] {
		require.Equal(t, "8", rid, "a failed poll must not reset the rid")
	}

	listed, err := c.List(context.Background())
	require.NoError(t, err)
	require.Len(t, listed, 1)
	info, err := c.Get(context.Background(), engine.NameQBittorrent+":"+testHash)
	require.NoError(t, err)
	require.Equal(t, engine.StateDownloading, info.State)
	require.Equal(t, int64(2048), info.DownloadRate)
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
	c := connectedSyncClient(t, f, func(string) bool { return true })

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

func TestWithheldOwnershipRetriesFullUntilMerged(t *testing.T) {
	c := &Client{}
	c.md.cache = cache{
		rid:      7,
		owned:    func(string) bool { return true },
		withheld: map[string]struct{}{testHash: {}},
	}

	// No response was applied between these reads. Both must request a
	// full update so a failed first request cannot lose the recovery.
	require.Equal(t, 0, c.currentRid())
	require.Equal(t, 0, c.currentRid())

	c.md.mu.Lock()
	_, _ = c.md.cache.merge(maindata{
		Rid:        8,
		FullUpdate: true,
		Torrents: map[string]json.RawMessage{
			testHash: json.RawMessage(torrentBody(testHash, "downloading")),
		},
	})
	c.md.mu.Unlock()

	require.Equal(t, 8, c.currentRid())
	require.NotContains(t, c.md.cache.withheld, testHash)
}

func TestEngineRidStaysInsideCache(t *testing.T) {
	const privateRid = 987654321

	c := &Client{}
	c.md.cache.owned = func(string) bool { return true }
	changed, removed := c.md.cache.merge(maindata{
		Rid:        privateRid,
		FullUpdate: true,
		Torrents: map[string]json.RawMessage{
			testHash: json.RawMessage(torrentBody(testHash, "downloading")),
		},
	})
	require.Empty(t, removed)

	listed, err := c.List(context.Background())
	require.NoError(t, err)
	got, err := c.Get(context.Background(), engine.NameQBittorrent+":"+testHash)
	require.NoError(t, err)

	c.md.mu.Lock()
	events := c.eventsLocked(changed, nil)
	c.md.mu.Unlock()
	payload, err := json.Marshal(struct {
		List   []engine.TaskInfo
		Get    engine.TaskInfo
		Events []engine.TaskEvent
	}{List: listed, Get: got, Events: events})
	require.NoError(t, err)
	require.NotContains(t, string(payload), strconv.Itoa(privateRid))
	require.Equal(t, privateRid, c.md.cache.rid, "the adapter must retain its private protocol rid")
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
	close(f.webapiRelease)
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

func TestCloseStopsPollGoroutine(t *testing.T) {
	f := newMaindataServer(t, func(rid int) (int, string) {
		return http.StatusOK, `{"rid":` + strconv.Itoa(rid+1) + `}`
	})

	c, err := New(Config{BaseURL: f.srv.URL, Username: testUsername, Password: testPassword}, nil)
	require.NoError(t, err)
	c.md.pollEvery = 5 * time.Millisecond
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

// The two OwnedRefs tests live in this file — not reconcile_test.go —
// because the Files table of T030 names this one; the predicate is the
// reconciler half of the very filter the rest of this file exercises.
func TestOwnedRefsReadsTheLiveTasksTable(t *testing.T) {
	tasks := &syncTasks{byEngine: map[string]map[string]store.Reconcilable{
		engine.NameQBittorrent: {testHash: {ID: "task-1", EngineRef: testHash, State: string(engine.StateDownloading)}},
	}}
	r := engine.NewReconciler(engine.NewRegistry(), tasks, syncAdmitter{}, time.Second, nil)

	owned := r.OwnedRefs(engine.NameQBittorrent)
	require.True(t, owned(testHash))
	require.False(t, owned(otherHash))

	// A row removed at runtime stops owning its handle on the next
	// listing — the predicate is live, not a boot snapshot.
	tasks.mu.Lock()
	delete(tasks.byEngine[engine.NameQBittorrent], testHash)
	tasks.mu.Unlock()
	require.Eventually(t, func() bool {
		return !owned(testHash)
	}, 2*time.Second, 20*time.Millisecond)
}

func TestOwnedRefsFailsStaleAfterFirstListing(t *testing.T) {
	tasks := &syncTasks{byEngine: map[string]map[string]store.Reconcilable{
		engine.NameQBittorrent: {testHash: {ID: "task-1", EngineRef: testHash, State: string(engine.StateDownloading)}},
	}}
	r := engine.NewReconciler(engine.NewRegistry(), tasks, syncAdmitter{}, time.Second, nil)
	owned := r.OwnedRefs(engine.NameQBittorrent)
	require.True(t, owned(testHash), "the first listing must succeed for the stale window to be meaningful")

	// A store blip keeps the last good set: hiding an owned handle would
	// read as a vanished transfer downstream and cost a re-submission.
	tasks.mu.Lock()
	tasks.fail = true
	tasks.mu.Unlock()
	require.True(t, owned(testHash))
	require.False(t, owned(otherHash))

	// Before any successful listing there is no prior truth to serve, so
	// the predicate answers foreign — never admitting an unaccounted
	// transfer on the strength of a failed read.
	fresh := engine.NewReconciler(
		engine.NewRegistry(),
		&syncTasks{fail: true, byEngine: map[string]map[string]store.Reconcilable{}},
		syncAdmitter{}, time.Second, nil)
	require.False(t, fresh.OwnedRefs(engine.NameQBittorrent)(testHash))
}
