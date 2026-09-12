// The sync/maindata delta protocol of docs/06-download-engines.md section
// 5.4: one 1 Hz poll feeding a merged torrent cache, and the ownership
// filter of section 8 that keeps a transfer dl-tool did not create out of
// it. List, Get and Events are served from the cache alone — no other
// polling loop exists in this adapter.
//
//   Connect ──▶ startPoll ──▶ pollLoop (1 Hz)
//                                 │ prepass: refresh ownership snapshot
//                                 │ GET sync/maindata?rid=N
//                                 ▼
//                        applyResponse (pass → scan → merge)
//                                 │ events for changed/removed hashes
//                                 ▼
//                       List / Get / Events subscribers
//
// The engine's rid never leaves this file: it is the daemon's own delta
// cursor, unrelated to dl-tool's SSE rid (ADR-0006). Ownership is decided
// only by hash, against a snapshot the Reconciler of T026 supplies; a hash
// the snapshot rejects keeps only its identifier, in rejected, so a later
// snapshot that owns it can be detected — but its fields never enter the
// served cache until an accepted full response supplies them complete.

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

	// infohashFlushBudget bounds one metadata write-back batch's store
	// calls: the flush runs on the poll goroutine, so a wedged store must
	// not wedge the poll with it — the ownership listing's
	// ownershipCheckBudget is the precedent.
	infohashFlushBudget = 5 * time.Second

	// infohashFlushMaxAttempts is the retry budget of one resolution: a
	// store that keeps refusing the same pair (a genuinely different
	// identity for a column that already holds one, a malformed daemon
	// value) must not reappear in the log every tick for the process
	// lifetime.
	infohashFlushMaxAttempts = 5

	// qbtFullSyncInterval is the periodic full-resync cadence of section
	// 5.4: five minutes after the last accepted full update the shared
	// force-full flag goes up, so the next request carries rid=0.
	qbtFullSyncInterval = 5 * time.Minute

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

// InfohashWriter is the store surface the metadata write-back needs: one
// call per resolved torrent, keyed on the daemon's own hash because that
// is the value tasks.engine_ref stores verbatim. *store.TaskStore
// satisfies it; the adapter never names a store type, keeping the
// layering of docs/03-architecture.md section 5.2 (adapters stop at the
// engine package).
type InfohashWriter interface {
	// ResolveInfohashes lands one resolution: it maps the handle to the
	// live task, writes both hashes, and pauses the task when the resolved
	// identity collides with another live task — duplicate reporting that
	// landing so the caller stops the engine-side transfer too.
	ResolveInfohashes(ctx context.Context, engineName, ref, v1, v2 string) (duplicate bool, err error)
}

// infohashPair is one torrent's resolved identity: the 40-hex v1 and the
// 64-hex v2 from the daemon's infohash_v1/infohash_v2 keys, either empty
// when that form does not exist.
type infohashPair struct {
	v1 string
	v2 string
}

// pendingInfohash is one resolution awaiting its write-back, with the
// consecutive failures it has collected.
type pendingInfohash struct {
	pair     infohashPair
	attempts int
}

// infohashFlush is one batch entry handed to the writer.
type infohashFlush struct {
	hash string // the daemon's TorrentID — never reconstructed, never re-cased
	pair infohashPair
}

// mergeDisposition is what one merge proved about its response.
type mergeDisposition uint8

const (
	// mergeRejected fails closed: the response was not publishable, so
	// the caller accepts none of its payload, rid or events.
	mergeRejected mergeDisposition = iota
	// mergeApplied published the response; rid and events may follow.
	mergeApplied
)

// cache is the merged view. fields[hash] holds one owned torrent object.
// rejected holds foreign hash identifiers; pendingFull holds identifiers
// that became owned and await complete fields. Neither set retains torrent
// fields. owned is the ownership snapshot a pass stored; nil owns nothing.
type cache struct {
	rid             int
	fields          map[string]map[string]any
	rejected        map[string]struct{}
	pendingFull     map[string]struct{}
	owned           map[string]struct{}
	ownershipSource func() map[string]struct{}

	// Metadata-resolution bookkeeping of the T100 write-back:
	// infohashDone holds the last pair each hash successfully landed, so a
	// resolution fires once and an unchanged torrent never re-enters the
	// batch; infohashPending holds the pairs awaiting a landing, retried
	// on every tick until they succeed or exhaust their budget;
	// infohashDropped holds the pairs that exhausted it, so a permanently
	// refused resolution drops once instead of resurrecting on every later
	// merge that touches its torrent.
	infohashDone    map[string]infohashPair
	infohashPending map[string]pendingInfohash
	infohashDropped map[string]infohashPair

	// Recovery state shares the cache mutex so a response cannot consume
	// a reset: forceFull sends rid=0 until an accepted full update lands,
	// lastFullAt times the periodic full sync, and ownershipEpoch stamps
	// every ownership reset so a response captured before one cannot
	// publish after it.
	forceFull      bool
	lastFullAt     time.Time
	ownershipEpoch uint64
}

// cloneSet copies a hash set so the stored snapshot is never aliased to
// the one the source handed out. A nil set clones to empty.
func cloneSet(src map[string]struct{}) map[string]struct{} {
	dst := make(map[string]struct{}, len(src))
	for hash := range src {
		dst[hash] = struct{}{}
	}
	return dst
}

// ensureMaps makes the zero value safe to mutate: a partial can arrive
// before any full response, and the poll goroutine has no recover.
func (c *cache) ensureMaps() {
	if c.fields == nil {
		c.fields = make(map[string]map[string]any)
	}
	if c.rejected == nil {
		c.rejected = make(map[string]struct{})
	}
	if c.pendingFull == nil {
		c.pendingFull = make(map[string]struct{})
	}
	if c.infohashDone == nil {
		c.infohashDone = make(map[string]infohashPair)
	}
	if c.infohashPending == nil {
		c.infohashPending = make(map[string]pendingInfohash)
	}
	if c.infohashDropped == nil {
		c.infohashDropped = make(map[string]infohashPair)
	}
}

// noteInfohash records one merged torrent object's resolved identity: the
// pair the daemon's infohash_v1/infohash_v2 keys carry — never the map's
// hash key, which for a v2 torrent is the TorrentID truncation and not the
// v1 infohash (docs/06-download-engines.md section 3.5). A pair that is
// empty on both sides is no resolution at all; a pair the writer already
// landed is not one either. Caller holds md.mu.
func (c *cache) noteInfohash(hash string, fields map[string]any) {
	v1, _ := fields["infohash_v1"].(string)
	v2, _ := fields["infohash_v2"].(string)
	if v1 == "" && v2 == "" {
		return
	}

	pair := infohashPair{v1: v1, v2: v2}
	if c.infohashDone[hash] == pair {
		return
	}
	if c.infohashDropped[hash] == pair {
		// The budget already dropped exactly this pair; a later merge
		// re-reporting it must not resurrect the cycle.
		return
	}
	if pending, held := c.infohashPending[hash]; held && pending.pair == pair {
		// An unchanged pair keeps its accumulated attempts — an active
		// torrent is re-noted on nearly every delta, and resetting the
		// budget each time would make it unreachable.
		return
	}

	c.infohashPending[hash] = pendingInfohash{pair: pair}
}

// takeInfohashFlushes snapshots the resolutions awaiting a write-back and
// prunes the bookkeeping of hashes the cache no longer holds — their
// torrents are gone, and a later re-add re-resolves from a clean slate.
// Entries are returned in hash order so the batch is deterministic.
// Caller holds md.mu.
func (c *cache) takeInfohashFlushes() []infohashFlush {
	for hash := range c.infohashDone {
		if _, visible := c.fields[hash]; !visible {
			delete(c.infohashDone, hash)
		}
	}
	for hash := range c.infohashDropped {
		if _, visible := c.fields[hash]; !visible {
			delete(c.infohashDropped, hash)
		}
	}

	batch := make([]infohashFlush, 0, len(c.infohashPending))
	for hash := range c.infohashPending {
		if _, visible := c.fields[hash]; !visible {
			delete(c.infohashPending, hash)
			continue
		}
		if c.infohashDone[hash] == c.infohashPending[hash].pair {
			delete(c.infohashPending, hash)
			continue
		}
		batch = append(batch, infohashFlush{hash: hash, pair: c.infohashPending[hash].pair})
	}
	slices.SortFunc(batch, func(a, b infohashFlush) int { return strings.Compare(a.hash, b.hash) })

	return batch
}

// accepted reports whether the stored ownership snapshot owns hash.
// Membership checks run under the cache mutex and perform no I/O.
func (c *cache) accepted(hash string) bool {
	_, ok := c.owned[hash]
	return ok
}

// merge applies one response. On FullUpdate it rebuilds fields and
// rejected solely from the response and clears pendingFull; otherwise it
// reclassifies every reported hash, deep-merges each accepted one and then
// applies TorrentsRemoved to all three collections. It returns
// mergeRejected when pendingFull still contains an accepted hash — the
// caller then accepts no payload, rid or events. changed and removed
// contain sorted visible hashes only for mergeApplied.
func (c *cache) merge(m maindata) (mergeDisposition, []string, []string) {
	c.ensureMaps()
	if m.FullUpdate {
		return c.mergeFull(m)
	}
	return c.mergePartial(m)
}

// mergeFull rebuilds the whole cache from one full response. A pending
// hash the response reports is published directly — the object is
// complete — and one it omits is pruned. A previously visible hash the
// snapshot still accepts but the response omits is removed; one the
// snapshot now rejects disappears silently. The accepted response clears
// the force-full flag and restarts the full-sync interval.
func (c *cache) mergeFull(m maindata) (mergeDisposition, []string, []string) {
	fresh := make(map[string]map[string]any, len(m.Torrents))
	rejected := make(map[string]struct{})
	for hash, raw := range m.Torrents {
		if !c.accepted(hash) {
			rejected[hash] = struct{}{}
			continue
		}
		if fields, ok := decodeTorrentFields(raw); ok {
			fresh[hash] = fields
		} else if old, held := c.fields[hash]; held {
			// Invalid data is not a removal. Keep the last complete
			// object until the daemon reports a decodable value.
			fresh[hash] = old
		}
	}

	var changed, removed []string
	for hash := range c.fields {
		if _, reported := fresh[hash]; !reported && c.accepted(hash) {
			removed = append(removed, hash)
		}
	}
	for hash, fields := range fresh {
		// A complete object carries the daemon's resolved identity whenever
		// it has one; noteInfohash decides whether that is news.
		c.noteInfohash(hash, fields)
		if old, held := c.fields[hash]; !held || len(old) != len(fields) || !torrentFieldsEqual(old, fields) {
			changed = append(changed, hash)
		}
	}

	c.fields = fresh
	c.rejected = rejected
	c.pendingFull = make(map[string]struct{})
	c.forceFull = false
	c.lastFullAt = time.Now()
	c.rid = m.Rid

	slices.Sort(changed)
	slices.Sort(removed)
	return mergeApplied, changed, removed
}

// mergePartial applies one delta. Every reported hash is re-evaluated
// against the stored ownership snapshot: a now-rejected hash — visible or
// pending — moves to rejected without a reset, while a rejected hash the
// snapshot now accepts moves to pendingFull, which is an ownership reset:
// one epoch increment for the pass, the force-full flag up, and the whole
// response rejected, its removals included. While any accepted hash
// remains in pendingFull the response is rejected without another
// increment. Otherwise each accepted reported hash is inserted (the
// first-seen guarantee of section 5.4) or deep-merged, and
// torrents_removed applies to all three collections.
func (c *cache) mergePartial(m maindata) (mergeDisposition, []string, []string) {
	reset := false
	for hash := range m.Torrents {
		if c.accepted(hash) {
			if _, wasRejected := c.rejected[hash]; wasRejected {
				delete(c.rejected, hash)
				c.pendingFull[hash] = struct{}{}
				reset = true
			}
			continue
		}
		delete(c.fields, hash)
		delete(c.pendingFull, hash)
		c.rejected[hash] = struct{}{}
	}
	if reset {
		c.ownershipEpoch++
		c.forceFull = true
	}
	for hash := range c.pendingFull {
		if c.accepted(hash) {
			// A hash whose complete object is still owed: no part of
			// this response — removals included — may publish.
			return mergeRejected, nil, nil
		}
	}

	var changed, removed []string
	for hash, raw := range m.Torrents {
		if !c.accepted(hash) {
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
			continue
		}
		for name, value := range partial {
			stored[name] = value
		}
		// The merged object is the daemon's word on this torrent's
		// identity; when this delta is the one that delivered the metadata,
		// the pair becomes non-empty here for the first time.
		c.noteInfohash(hash, stored)
		changed = append(changed, hash)
	}
	for _, hash := range m.TorrentsRemoved {
		delete(c.rejected, hash)
		delete(c.pendingFull, hash)
		if _, visible := c.fields[hash]; !visible {
			continue
		}
		delete(c.fields, hash)
		removed = append(removed, hash)
		// Removal wins when one delta also changed the hash.
		changed = slices.DeleteFunc(changed, func(h string) bool { return h == hash })
	}

	c.rid = m.Rid
	slices.Sort(changed)
	slices.Sort(removed)
	return mergeApplied, changed, removed
}

// ownershipScan reclassifies the three collections against the snapshot a
// pass just stored in owned. A visible or pending hash the snapshot now
// rejects moves to rejected silently — no event, no force-full change, no
// epoch increment. A rejected hash it now accepts moves to pendingFull,
// which is the edge-triggered ownership reset: one epoch increment for the
// pass and the force-full flag up. A hash already pending never retriggers
// it while it remains accepted. Only the pre-request pass and a
// partial-response pass scan; a full response classifies its complete
// objects inside mergeFull.
func (c *cache) ownershipScan() {
	c.ensureMaps()
	for hash := range c.fields {
		if !c.accepted(hash) {
			delete(c.fields, hash)
			c.rejected[hash] = struct{}{}
		}
	}
	for hash := range c.pendingFull {
		if !c.accepted(hash) {
			delete(c.pendingFull, hash)
			c.rejected[hash] = struct{}{}
		}
	}
	reset := false
	for hash := range c.rejected {
		if c.accepted(hash) {
			delete(c.rejected, hash)
			c.pendingFull[hash] = struct{}{}
			reset = true
		}
	}
	if reset {
		c.ownershipEpoch++
		c.forceFull = true
	}
}

// installOwnership applies the reset of SetOwnershipFilter: install the
// snapshot, drop newly rejected torrent fields without events, move newly
// accepted rejected identifiers to pendingFull, and bump the epoch and the
// force-full flag unconditionally — installing or replacing the source is
// always one reset, whatever moved.
func (c *cache) installOwnership(set map[string]struct{}) {
	c.ensureMaps()
	for hash := range c.fields {
		if _, ok := set[hash]; !ok {
			delete(c.fields, hash)
			c.rejected[hash] = struct{}{}
		}
	}
	for hash := range c.pendingFull {
		if _, ok := set[hash]; !ok {
			delete(c.pendingFull, hash)
			c.rejected[hash] = struct{}{}
		}
	}
	for hash := range c.rejected {
		if _, ok := set[hash]; ok {
			delete(c.rejected, hash)
			c.pendingFull[hash] = struct{}{}
		}
	}
	c.owned = set
	c.ownershipEpoch++
	c.forceFull = true
}

// dueFullSync raises the shared force-full flag once the periodic
// full-sync interval has elapsed since the last accepted full update, so
// requests send rid=0 until an accepted full response clears it. Caller
// holds md.mu.
func (c *cache) dueFullSync() {
	if !c.lastFullAt.IsZero() && time.Since(c.lastFullAt) >= qbtFullSyncInterval {
		c.forceFull = true
	}
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
	if fields == nil {
		slog.Warn("qbittorrent: sync/maindata carried a null torrent value",
			"engine", engine.NameQBittorrent)
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

// maindataTracker is the poll machinery: the merged cache, the event
// subscribers and the poll goroutine's lifecycle. Its zero value is an
// idle tracker over an empty, default-deny cache. mu guards every field;
// the daemon HTTP and the ownership source's store read run outside it.
type maindataTracker struct {
	mu        sync.Mutex
	pollEvery time.Duration // test override; 0 means maindataPollInterval
	cache     cache
	subs      map[chan engine.TaskEvent]struct{}
	cancel    context.CancelFunc
	done      chan struct{} // poll goroutine exit
	stopped   chan struct{} // full Close completion, including subscribers
	started   bool
	closed    bool
	// infohashWriter is the store surface the delta path writes resolved
	// infohashes through; nil (the zero value, the openapi subcommand's
	// client) leaves the write-back off and the delta path skips it.
	infohashWriter InfohashWriter
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
	c.md.stopSignalLocked()
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
	stopped := c.md.stopSignalLocked()
	if c.md.closed {
		c.md.mu.Unlock()
		<-stopped
		return
	}
	c.md.closed = true
	cancel := c.md.cancel
	done := c.md.done
	subs := c.md.subs
	c.md.subs = nil
	c.md.mu.Unlock()

	if cancel != nil {
		cancel()
		<-done
	}
	for sub := range subs {
		close(sub)
	}

	c.md.mu.Lock()
	c.md.cancel = nil
	c.md.done = nil
	c.md.started = false
	c.md.mu.Unlock()
	close(stopped)
}

// stopSignalLocked returns the one channel closed after the poll and every
// subscriber stop. Caller holds md.mu.
func (m *maindataTracker) stopSignalLocked() chan struct{} {
	if m.stopped == nil {
		m.stopped = make(chan struct{})
	}
	return m.stopped
}

// SetInfohashWriter installs the store surface the delta path writes
// resolved infohashes through — the composition root's wiring of the T100
// write-back, the same shape SetOwnershipFilter has for the ownership
// filter. It is safe to call before Connect; resolutions collected before
// the installation land on the next tick.
func (c *Client) SetInfohashWriter(w InfohashWriter) {
	c.md.mu.Lock()
	defer c.md.mu.Unlock()

	c.md.infohashWriter = w
}

// SetOwnershipFilter installs the snapshot source deciding which
// qBittorrent hashes belong to dl-tool; it is supplied by the Reconciler
// of T026 through NewReconciler. Hashes it rejects are dropped from the
// cache, from List, from Get and from every TaskEvent, and only their
// identifiers are retained so later ownership can be detected. Until a
// source is installed the cache owns nothing, so a caller that forgets to
// install it sees an empty queue rather than foreign transfers.
// Installation is one ownership reset: newly rejected torrent fields are
// dropped synchronously without events, newly accepted rejected
// identifiers move to pendingFull, the ownership epoch is bumped once and
// a full resync is forced. The source is invoked and its result copied
// before the cache mutex is taken, so store I/O never blocks cache
// readers. A nil source owns nothing.
func (c *Client) SetOwnershipFilter(snapshot func() map[string]struct{}) {
	var set map[string]struct{}
	if snapshot != nil {
		set = cloneSet(snapshot())
	}

	c.md.mu.Lock()
	defer c.md.mu.Unlock()

	c.md.cache.ownershipSource = snapshot
	c.md.cache.installOwnership(set)
}

// fetchSourceSnapshot snapshots the source and the epoch under the cache
// mutex, invokes the source and copies its result without that mutex, then
// reports both. Whether the epoch survived the source call is the caller's
// to decide under the lock it stores with.
func (c *Client) fetchSourceSnapshot() (set map[string]struct{}, epoch uint64) {
	c.md.mu.Lock()
	source := c.md.cache.ownershipSource
	epoch = c.md.cache.ownershipEpoch
	c.md.mu.Unlock()

	if source != nil {
		set = cloneSet(source())
	} else {
		set = make(map[string]struct{})
	}
	return set, epoch
}

// ownershipPrepass runs the pre-request ownership pass of section 5.4:
// refresh the snapshot, store it and reclassify the cache. false means the
// epoch moved while the source ran — this tick skips its request, and the
// next tick retries.
func (c *Client) ownershipPrepass() bool {
	set, epoch := c.fetchSourceSnapshot()

	c.md.mu.Lock()
	defer c.md.mu.Unlock()

	if c.md.cache.ownershipEpoch != epoch {
		return false
	}
	c.md.cache.owned = set
	c.md.cache.ownershipScan()
	return true
}

// applyResponse runs the response-time pass and, when the reply is
// current, merges it, emits its events and hands the metadata resolutions
// the merge collected to the write-back — the store calls run outside the
// cache mutex, the same discipline the ownership source keeps. The pass
// snapshots the source and epoch under the cache mutex, compares the
// request's captured epoch before the source call, invokes the source
// without the cache mutex, then compares again after re-acquiring it: a
// reply whose captured epoch no longer matches — including a full_update
// one — is stale and publishes nothing, no matter what the daemon sent. A
// partial-response pass reclassifies before merging, so a
// rejected-to-pending transition found here rejects the payload while the
// reclassification itself stays published. A full-response pass only
// stores the snapshot: its merge classifies the complete objects directly.
func (c *Client) applyResponse(m maindata, reqEpoch uint64) {
	batch := c.applyResponseInner(m, reqEpoch)
	c.flushInfohashes(batch)
}

// applyResponseInner is applyResponse's body; it manages md.mu itself
// (unlike the *Locked helpers, which require the caller to hold it), and
// its result is the metadata-resolution batch the caller flushes.
func (c *Client) applyResponseInner(m maindata, reqEpoch uint64) []infohashFlush {
	c.md.mu.Lock()
	source := c.md.cache.ownershipSource
	epoch := c.md.cache.ownershipEpoch
	c.md.mu.Unlock()

	if epoch != reqEpoch {
		return nil // stale before the source call: nothing to fetch for
	}

	set := make(map[string]struct{})
	if source != nil {
		set = cloneSet(source())
	}

	c.md.mu.Lock()
	defer c.md.mu.Unlock()

	if c.md.cache.ownershipEpoch != epoch {
		return nil // a reset landed while the source ran
	}

	c.md.cache.owned = set
	if !m.FullUpdate {
		c.md.cache.ownershipScan()
	}

	disposition, changed, removed := c.md.cache.merge(m)
	if disposition != mergeApplied {
		return nil
	}
	c.emitLocked(c.eventsLocked(changed, removed))

	return c.md.cache.takeInfohashFlushes()
}

// flushInfohashes hands one merge's resolutions to the write-back: one
// ResolveInfohashes call per hash, on a context with its own budget
// because the poll goroutine owns this loop. A landing that succeeded —
// and a landing whose collision paused the task, which ResolveInfohashes
// answers with nil too — moves the pair from pending to done; a failure
// keeps it pending for the next tick until the retry budget runs out, so
// the write-back can never wedge the poll loop.
func (c *Client) flushInfohashes(batch []infohashFlush) {
	if len(batch) == 0 {
		return
	}

	c.md.mu.Lock()
	writer := c.md.infohashWriter
	c.md.mu.Unlock()
	if writer == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), infohashFlushBudget)
	defer cancel()

	for _, flush := range batch {
		duplicate, err := writer.ResolveInfohashes(ctx, engine.NameQBittorrent, flush.hash, flush.pair.v1, flush.pair.v2)
		if err != nil && ctx.Err() != nil {
			// The batch budget expired on this entry's call — still one
			// failed attempt for the entry that was in flight, while the
			// remaining entries never got their shot and keep both their
			// pending slot and their attempts for the next tick.
			c.chargeFlushAttempt(flush, err)
			break
		}

		if err == nil && duplicate {
			// Project the stop into the cache before the engine call: the
			// daemon now holds the torrent stopped, but until its next delta
			// arrives the cached object still says downloading, and a
			// reconciler sweep whose List predates this moment would adopt the
			// stale state and un-pause the row — the one write-back decision
			// no other loop may undo. Projecting first leaves that sweep a
			// microsecond window instead of the HTTP call's milliseconds; both
			// paused spellings normalise to paused, and the next delta
			// confirms or corrects the exact one.
			c.md.mu.Lock()
			if fields, held := c.md.cache.fields[flush.hash]; held {
				// A torrent on the upload side stops into the UP spelling;
				// both normalise to paused either way, but the cached string
				// should say what the daemon will say.
				stopped := stateStoppedDL
				if state, _ := fields["state"].(string); isUploadSideState(state) {
					stopped = stateStoppedUP
				}
				fields["state"] = stopped
			}
			c.md.mu.Unlock()

			// The transfer the store flagged must stop engine-side too, or the
			// next reconciler sweep would adopt the still-downloading torrent
			// and un-pause it. torrents/pause retains every byte: nothing is
			// deleted, on purpose. A failure retries the whole resolution on
			// the next tick — the store keeps reporting the collision until
			// the write lands, and PauseDuplicate is idempotent.
			if pauseErr := c.Pause(ctx, engine.NameQBittorrent+":"+flush.hash); pauseErr != nil {
				slog.Warn("qbittorrent: duplicate torrent paused in the store but not engine-side",
					"engine", engine.NameQBittorrent, "hash", flush.hash, "error", pauseErr)
				err = pauseErr
			}
		}

		c.md.mu.Lock()
		if err == nil {
			if c.md.cache.infohashPending[flush.hash].pair == flush.pair {
				delete(c.md.cache.infohashPending, flush.hash)
				c.md.cache.infohashDone[flush.hash] = flush.pair
			}
		} else {
			c.chargeFlushAttemptLocked(flush, err)
		}
		c.md.mu.Unlock()
	}
}

// chargeFlushAttempt records one failed write-back attempt: the attempts
// counter grows, and at the budget's end the pair drops for good — a pair
// the store keeps refusing must not reappear in the log every tick for
// the process lifetime.
func (c *Client) chargeFlushAttempt(flush infohashFlush, err error) {
	c.md.mu.Lock()
	defer c.md.mu.Unlock()

	c.chargeFlushAttemptLocked(flush, err)
}

// chargeFlushAttemptLocked is chargeFlushAttempt for a caller holding
// md.mu.
func (c *Client) chargeFlushAttemptLocked(flush infohashFlush, err error) {
	pending, still := c.md.cache.infohashPending[flush.hash]
	if !still || pending.pair != flush.pair {
		// A newer resolution replaced this one; it owns the retry.
		return
	}

	pending.attempts++
	if pending.attempts < infohashFlushMaxAttempts {
		c.md.cache.infohashPending[flush.hash] = pending
		slog.Warn("qbittorrent: infohash write-back failed; retrying on the next tick",
			"engine", engine.NameQBittorrent, "hash", flush.hash, "error", err)

		return
	}

	delete(c.md.cache.infohashPending, flush.hash)
	c.md.cache.infohashDropped[flush.hash] = flush.pair
	slog.Error("qbittorrent: infohash write-back kept failing; dropping the resolution",
		"engine", engine.NameQBittorrent, "hash", flush.hash, "error", err)
}

// requestPlan selects the next request's rid and captures the ownership
// epoch the request runs under. The rid is 0 while the shared force-full
// flag is set, else the last accepted response's rid; the periodic
// full-sync interval raises the flag here, under the lock.
func (c *Client) requestPlan() (rid int, epoch uint64) {
	c.md.mu.Lock()
	defer c.md.mu.Unlock()

	c.md.cache.dueFullSync()
	if c.md.cache.forceFull {
		return 0, c.md.cache.ownershipEpoch
	}
	return c.md.cache.rid, c.md.cache.ownershipEpoch
}

// forceFullFlag raises the shared force-full flag after a failed poll:
// the outcome is ambiguous, so every later request sends rid=0 until an
// accepted full_update lands.
func (c *Client) forceFullFlag() {
	c.md.mu.Lock()
	defer c.md.mu.Unlock()
	c.md.cache.forceFull = true
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
// subscriber's buffer is full, logging the drop. Events are hints: callers
// establish or recover their authoritative snapshot through List. The
// channel closes when ctx is cancelled or Close is called, never twice — removal from the subscriber
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
	stopped := c.md.stopSignalLocked()
	c.md.mu.Unlock()

	go func() {
		select {
		case <-ctx.Done():
		case <-stopped:
		}
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
// cache, not a stale one.
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

// pollOnce performs one pre-request ownership pass and one GET
// sync/maindata. A timeout, transport failure, non-2xx reply or an
// undecodable body logs at warn, keeps the last accepted cache and rid,
// and raises the shared force-full flag: every later poll sends rid=0
// until an accepted full_update clears it. The response-time pass, the
// merge and the fan-out run under md.mu so the cache, its sets and the
// rid publish together and a truncated or stale body can advance none of
// them.
func (c *Client) pollOnce(ctx context.Context) {
	if !c.ownershipPrepass() {
		return
	}
	rid, reqEpoch := c.requestPlan()

	body, err := c.do(ctx, http.MethodGet, pathSyncMaindata, url.Values{"rid": {strconv.Itoa(rid)}})
	if err != nil {
		// A cancelled context is Close or shutdown, not an outage: the
		// warn is reserved for a poll that failed on a live loop.
		if ctx.Err() == nil {
			slog.Warn("qbittorrent: sync/maindata poll failed; forcing a full resync",
				"engine", engine.NameQBittorrent, "rid", rid, "error", err)
		}
		c.forceFullFlag()
		return
	}

	var m maindata
	if err := json.Unmarshal(body, &m); err != nil {
		slog.Warn("qbittorrent: sync/maindata reply undecodable; forcing a full resync",
			"engine", engine.NameQBittorrent, "rid", rid, "error", err)
		c.forceFullFlag()
		return
	}

	c.applyResponse(m, reqEpoch)
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

// isUploadSideState reports whether a daemon state string is one of the
// upload-side spellings — a torrent there stops into the UP stopped
// spelling, not the DL one.
func isUploadSideState(state string) bool {
	switch state {
	case stateUploading, stateForcedUP, stateStalledUP, stateStoppedUP, stateQueuedUP, stateCheckingUP:
		return true
	default:
		return false
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
