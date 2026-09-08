// The sync/maindata delta protocol of docs/06-download-engines.md section
// 5.4: one 1 Hz poll feeding a merged torrent cache, and the ownership
// filter of section 8 that keeps a transfer dl-tool did not create out of
// it. List, Get and Events are served from the cache alone — no other
// polling loop exists in this adapter.
//
//   Connect ──▶ startPoll ──▶ pollLoop (1 Hz)
//                                 │ GET sync/maindata?rid=N
//                                 ▼
//                        cache.merge (full or delta)
//                                 │ changed/removed hashes
//                                 ▼
//                       List / Get / Events subscribers

package qbittorrent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/L-K-M/dl-tool/internal/engine"
)

const (
	// pathSyncMaindata is the delta endpoint the WebUI itself polls.
	pathSyncMaindata = "sync/maindata"

	// maindataPollInterval is the sync/maindata cadence of section 5.4.
	maindataPollInterval = time.Second

	// eventsBuffer sizes every subscriber channel: one event per changed
	// hash per tick, so a quiet consumer lags a burst without the poll
	// loop ever blocking on it.
	eventsBuffer = 128
)

// maindata is the GET /api/v2/sync/maindata?rid=N envelope. Torrents values
// are partial objects, so they are held as raw JSON and merged key by key —
// never decoded into torrentJSON before merging.
type maindata struct {
	Rid             int                        `json:"rid"`
	FullUpdate      bool                       `json:"full_update"`
	Torrents        map[string]json.RawMessage `json:"torrents"`
	TorrentsRemoved []string                   `json:"torrents_removed"`
	// ServerState is held for the envelope's completeness — the full or
	// partial global-state object — but nothing consumes it in this task;
	// global rates and totals are later tasks' to read.
	ServerState json.RawMessage `json:"server_state"`
}

// cache is the merged view. fields[hash] holds the accumulated JSON object
// for one torrent. owned decides whether a hash belongs to a dl-tool task;
// nil owns nothing, so a cache nobody configured holds nothing. The hash is
// also the object's own "hash" value: the daemon keys the torrents object
// by the same TorrentID torrents/info reports in its hash field.
type cache struct {
	rid    int
	fields map[string]map[string]any
	owned  func(hash string) bool
}

// rejectAll is the default ownership predicate: until the Reconciler of
// T026 installs the real one, every transfer is foreign.
func rejectAll(string) bool { return false }

// merge applies one response. On FullUpdate it replaces fields wholesale;
// otherwise it deep-merges each per-hash object and then applies
// TorrentsRemoved. It returns the hashes whose value changed and the hashes
// that disappeared, both sorted. A hash the ownership predicate rejects is
// never stored, never returned and never counted — the invariant is that
// fields holds owned hashes only, which is what lets the delta-removal
// path skip a second ownership check (the full-update path cannot: it
// reports old hashes the predicate may since have rejected, and those it
// drops silently). rid advances with the response even when the merge
// itself is a no-op: rid tracks the server's protocol state, not ours.
func (c *cache) merge(m maindata) (changed, removed []string) {
	owned := c.owned
	if owned == nil {
		owned = rejectAll
	}
	if c.fields == nil {
		// A zero-value cache is mergeable too: a partial can arrive before
		// any full response, and assigning into a nil map would panic the
		// poll goroutine — the one failure the keep-the-cache rule forbids.
		c.fields = make(map[string]map[string]any)
	}

	if m.FullUpdate {
		// A full response replaces the cache and re-runs the ownership
		// filter over every hash — the one place a hash whose task row
		// vanished since the last poll is dropped again.
		fresh := make(map[string]map[string]any, len(m.Torrents))
		for hash, raw := range m.Torrents {
			if !owned(hash) {
				continue
			}
			if fields, ok := decodeTorrentFields(raw); ok {
				fresh[hash] = fields
			}
		}
		for hash := range c.fields {
			_, still := fresh[hash]
			if !still && owned(hash) {
				// owned guards the report, not the drop: a hash the current
				// predicate rejects appears in no TaskEvent, so it vanishes
				// from the cache without a removal event for it.
				removed = append(removed, hash)
			}
		}
		// changed is a diff against the old cache, not every fresh hash:
		// a full resync after a lost session re-sends byte-identical
		// objects, and one event per changed hash means unchanged is no
		// event — otherwise a large queue re-floods every subscriber
		// buffer on every full response.
		for hash, fields := range fresh {
			if old, held := c.fields[hash]; !held || !torrentFieldsEqual(old, fields) {
				changed = append(changed, hash)
			}
		}
		c.fields = fresh
	} else {
		for hash, raw := range m.Torrents {
			if !owned(hash) {
				continue
			}
			partial, ok := decodeTorrentFields(raw)
			if !ok {
				continue
			}
			stored := c.fields[hash]
			if stored == nil {
				stored = make(map[string]any, len(partial))
				c.fields[hash] = stored
			} else if torrentFieldsEqual(stored, partial) {
				// The daemon re-sent values we already hold; no event.
				continue
			}
			for name, value := range partial {
				stored[name] = value
			}
			changed = append(changed, hash)
		}
		for _, hash := range m.TorrentsRemoved {
			if _, exists := c.fields[hash]; !exists {
				continue
			}
			delete(c.fields, hash)
			removed = append(removed, hash)
			// A hash the same delta both changed and removed — a re-add
			// racing a delete — is removed, full stop: fields no longer
			// holds it, so a leftover changed entry would project a
			// zero-value TaskEvent beside the removal.
			changed = slices.DeleteFunc(changed, func(h string) bool { return h == hash })
		}
	}

	slices.Sort(changed)
	slices.Sort(removed)
	c.rid = m.Rid
	return changed, removed
}

// decodeTorrentFields decodes one per-hash torrent value. A value that is
// not a JSON object cannot be merged; it is skipped with a warning rather
// than failing the whole response.
func decodeTorrentFields(raw json.RawMessage) (map[string]any, bool) {
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		slog.Warn("qbittorrent: sync/maindata carried a non-object torrent value",
			"engine", engine.NameQBittorrent, "error", err)
		return nil, false
	}
	return fields, true
}

// torrentFieldsEqual reports whether every key of partial is already stored
// with an equal value. The length check covers a partial carrying a key the
// stored object never had — a new key is a change even when its value is
// the zero of its type.
func torrentFieldsEqual(stored, partial map[string]any) bool {
	if len(stored) < len(partial) {
		return false
	}
	for name, value := range partial {
		if other, ok := stored[name]; !ok || !reflect.DeepEqual(other, value) {
			return false
		}
	}
	return true
}

// maindataTracker is the poll machinery: the merged cache with its rid, the
// ownership predicate, the event subscribers and the poll goroutine's
// lifecycle. Its zero value is an idle tracker over an empty, default-deny
// cache. mu guards every field; the poll goroutine holds it only for the
// in-memory merge and the non-blocking fan-out, never across I/O.
type maindataTracker struct {
	mu        sync.Mutex
	pollEvery time.Duration // test override; 0 means maindataPollInterval
	cache     cache
	subs      map[chan engine.TaskEvent]struct{}
	cancel    context.CancelFunc
	done      chan struct{}
	started   bool
	closed    bool
}

// startPoll launches the 1 Hz sync/maindata loop on a context the client
// owns, so Close — not any caller's context — ends it. Idempotent: a second
// Connect keeps the one goroutine.
func (c *Client) startPoll() {
	c.md.mu.Lock()
	defer c.md.mu.Unlock()

	if c.md.started || c.md.closed {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c.md.cancel = cancel
	c.md.done = done
	c.md.started = true
	go c.pollLoop(ctx, done)
}

// stopPoll cancels the poll goroutine's context and waits for it to exit,
// then closes every subscriber channel. Close returning means the loop is
// gone: no poll, no merge, no event can follow it.
func (c *Client) stopPoll() {
	c.md.mu.Lock()
	cancel := c.md.cancel
	done := c.md.done
	subs := c.md.subs
	c.md.cancel = nil
	c.md.done = nil
	c.md.subs = nil
	c.md.started = false
	c.md.closed = true
	c.md.mu.Unlock()

	if cancel != nil {
		cancel()
		<-done
	}
	for sub := range subs {
		close(sub)
	}
}

// SetOwnershipFilter installs the predicate deciding whether a qBittorrent
// hash belongs to a dl-tool task; it is supplied by the Reconciler of T026
// through NewReconciler. Hashes it rejects are dropped from the cache, from
// List, from Get and from every TaskEvent. Until it is set the cache holds
// nothing, so a caller that forgets to install it sees an empty queue
// rather than foreign transfers.
func (c *Client) SetOwnershipFilter(owned func(hash string) bool) {
	if owned == nil {
		owned = rejectAll
	}

	c.md.mu.Lock()
	defer c.md.mu.Unlock()

	c.md.cache.owned = owned
	// Re-filter what is already held: the filter may arrive after polls
	// have run, and a rejected hash must not survive until the next
	// full_update to disappear.
	for hash := range c.md.cache.fields {
		if !owned(hash) {
			delete(c.md.cache.fields, hash)
		}
	}
	// A rejected hash is dropped without a removal event — it appears in
	// no TaskEvent — and a hash the previous predicate rejected cannot
	// come back through a delta, which only carries changed torrents. So
	// the rid is reset here to force one full snapshot under the new
	// predicate on the next tick. This is a deliberate local resync on a
	// configuration change, not the transport-failure reset the task's
	// poll-loop table forbids.
	c.md.cache.rid = 0
}

// List returns every owned torrent in the cache, sorted by id. It performs
// no I/O: a cache misses an engine's transfer only until the next tick.
func (c *Client) List(ctx context.Context) ([]engine.TaskInfo, error) {
	c.md.mu.Lock()
	defer c.md.mu.Unlock()

	infos := make([]engine.TaskInfo, 0, len(c.md.cache.fields))
	for hash, fields := range c.md.cache.fields {
		if info, ok := taskInfoFromCache(hash, fields); ok {
			infos = append(infos, info)
		}
	}
	slices.SortFunc(infos, func(a, b engine.TaskInfo) int {
		return strings.Compare(a.ID, b.ID)
	})
	return infos, nil
}

// Get returns one torrent from the cache, engine.ErrNotFound when the cache
// does not hold the hash — foreign included, which is the point.
func (c *Client) Get(ctx context.Context, id string) (engine.TaskInfo, error) {
	hash := ref(id)

	c.md.mu.Lock()
	defer c.md.mu.Unlock()

	fields, exists := c.md.cache.fields[hash]
	if !exists {
		return engine.TaskInfo{}, fmt.Errorf("qbittorrent: %s: %w", id, engine.ErrNotFound)
	}
	info, ok := taskInfoFromCache(hash, fields)
	if !ok {
		return engine.TaskInfo{}, fmt.Errorf("qbittorrent: %s: %w", id, engine.ErrNotFound)
	}
	return info, nil
}

// Events subscribes to the poll goroutine's feed: one TaskEvent per changed
// hash per tick, EventRemoved with a nil Info for a removed hash. The
// channel is buffered and the loop drops an event rather than block when a
// subscriber's buffer is full, logging the drop; it closes when ctx is
// cancelled or Close is called, never twice — removal from the subscriber
// set and the close happen under one lock, on whichever path gets there
// first.
func (c *Client) Events(ctx context.Context) (<-chan engine.TaskEvent, error) {
	events := make(chan engine.TaskEvent, eventsBuffer)

	c.md.mu.Lock()
	if c.md.closed {
		c.md.mu.Unlock()
		close(events)
		return events, nil
	}
	if c.md.subs == nil {
		c.md.subs = make(map[chan engine.TaskEvent]struct{})
	}
	c.md.subs[events] = struct{}{}
	c.md.mu.Unlock()

	go func() {
		<-ctx.Done()
		c.dropSubscription(events)
	}()
	return events, nil
}

// dropSubscription removes one subscriber and closes its channel. After
// stopPoll took the set, the map is nil and the close belongs to stopPoll
// alone, so this is a no-op there.
func (c *Client) dropSubscription(events chan engine.TaskEvent) {
	c.md.mu.Lock()
	defer c.md.mu.Unlock()

	if _, subscribed := c.md.subs[events]; subscribed {
		delete(c.md.subs, events)
		close(events)
	}
}

// pollLoop drives the delta protocol: one poll per tick, the first
// included — "Interval: 1 s, from a time.Ticker" is the whole of the
// rule, so a caller between Connect and the first tick sees an empty
// cache, not a stale one. Every request carries the rid of the last
// response, rid=0 on the first, which the daemon answers full.
func (c *Client) pollLoop(ctx context.Context, done chan struct{}) {
	defer close(done)

	interval := c.pollIntervalSetting()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.pollOnce(ctx)
		}
	}
}

// pollIntervalSetting reads the test-overridable cadence.
func (c *Client) pollIntervalSetting() time.Duration {
	c.md.mu.Lock()
	defer c.md.mu.Unlock()

	if c.md.pollEvery > 0 {
		return c.md.pollEvery
	}
	return maindataPollInterval
}

// pollOnce performs one GET sync/maindata with the current rid. A transport
// failure, a non-2xx reply or an undecodable body logs at warn and leaves
// both the cache and the rid untouched — the next tick retries with the
// same rid; it is reset only when the daemon itself answers full_update
// (06 section 5.4). The merge and the fan-out run under md.mu so the cache
// and its rid publish together and a decode failure can advance neither.
func (c *Client) pollOnce(ctx context.Context) {
	rid := c.currentRid()

	body, err := c.do(ctx, http.MethodGet, pathSyncMaindata, url.Values{"rid": {strconv.Itoa(rid)}})
	if err != nil {
		// A cancelled context is Close or shutdown, not an outage: the
		// warn is reserved for a poll that failed on a live loop.
		if ctx.Err() == nil {
			slog.Warn("qbittorrent: sync/maindata poll failed; keeping the last cache and rid",
				"engine", engine.NameQBittorrent, "rid", rid, "error", err)
		}
		return
	}

	var m maindata
	if err := json.Unmarshal(body, &m); err != nil {
		slog.Warn("qbittorrent: sync/maindata reply undecodable; keeping the last cache and rid",
			"engine", engine.NameQBittorrent, "rid", rid, "error", err)
		return
	}

	c.md.mu.Lock()
	defer c.md.mu.Unlock()

	// A response is stale the moment a filter install reset the rid while
	// the request was in flight: applying it would clobber the reset —
	// and the full snapshot it asks for — with the old session's last
	// delta. Only a reply to the rid the cache still holds may merge.
	if c.md.cache.rid != rid {
		return
	}

	changed, removed := c.md.cache.merge(m)
	c.emitLocked(c.eventsLocked(changed, removed))
}

// currentRid snapshots the rid of the last applied response.
func (c *Client) currentRid() int {
	c.md.mu.Lock()
	defer c.md.mu.Unlock()
	return c.md.cache.rid
}

// eventsLocked projects the merge result onto TaskEvents: the kind implied
// by the new state for a changed hash, EventRemoved with a nil Info for a
// disappeared one. Caller holds md.mu.
func (c *Client) eventsLocked(changed, removed []string) []engine.TaskEvent {
	events := make([]engine.TaskEvent, 0, len(changed)+len(removed))

	for _, hash := range changed {
		info, ok := taskInfoFromCache(hash, c.md.cache.fields[hash])
		if !ok {
			continue
		}
		events = append(events, engine.TaskEvent{
			TaskID: engine.NameQBittorrent + ":" + hash,
			Kind:   eventKindFor(info.State),
			Info:   &info,
		})
	}
	for _, hash := range removed {
		events = append(events, engine.TaskEvent{
			TaskID: engine.NameQBittorrent + ":" + hash,
			Kind:   engine.EventRemoved,
			Info:   nil, // the torrent is gone; there is no info to carry
		})
	}
	return events
}

// eventKindFor maps the normalised state onto the event vocabulary of the
// task file: downloading, seeding, checking and queued are progress of one
// living transfer; the other states name their own kind. Caller holds md.mu.
func eventKindFor(state engine.TaskState) engine.EventKind {
	switch state {
	case engine.StatePaused:
		return engine.EventPaused
	case engine.StateCompleted:
		return engine.EventCompleted
	case engine.StateError:
		return engine.EventError
	default:
		return engine.EventProgress
	}
}

// emitLocked fans one merge's events out to every subscriber, without
// blocking: a full buffer drops the event with a warning — a slow consumer
// loses deltas, never the poll loop. Sending under md.mu is what makes the
// send-race with a subscriber's close impossible. Caller holds md.mu.
func (c *Client) emitLocked(events []engine.TaskEvent) {
	for _, event := range events {
		for sub := range c.md.subs {
			select {
			case sub <- event:
			default:
				slog.Warn("qbittorrent: event dropped: subscriber buffer full",
					"engine", engine.NameQBittorrent, "task_id", event.TaskID, "kind", event.Kind)
			}
		}
	}
}

// taskInfoFromCache projects one cached torrent object onto engine.TaskInfo
// by round-tripping it through torrentJSON — the merged object is exactly
// one torrents/info element, so toTaskInfo of T029 applies unchanged. The
// map key is the authoritative hash: it is the daemon's own TorrentID, the
// same value torrents/info reports in its hash field.
func taskInfoFromCache(hash string, fields map[string]any) (engine.TaskInfo, bool) {
	raw, err := json.Marshal(fields)
	if err != nil {
		slog.Warn("qbittorrent: cached torrent object is not marshalable",
			"engine", engine.NameQBittorrent, "hash", hash, "error", err)
		return engine.TaskInfo{}, false
	}

	var t torrentJSON
	if err := json.Unmarshal(raw, &t); err != nil {
		slog.Warn("qbittorrent: cached torrent object does not match the torrents/info shape",
			"engine", engine.NameQBittorrent, "hash", hash, "error", err)
		return engine.TaskInfo{}, false
	}
	t.Hash = hash
	return toTaskInfo(t), true
}
