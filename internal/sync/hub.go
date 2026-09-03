package sync

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// publishInterval is the maximum fan-out rate: one delivery per second
	// per hub (docs/03-architecture.md section 8.6). Everything published
	// inside a window coalesces into that window's single send.
	publishInterval = time.Second

	// subscriberBuffer bounds a subscriber's queue. A subscriber that lets it
	// fill is closed and must reconnect through the seq_gap path.
	subscriberBuffer = 8
)

// Hub owns the rid counter, the ring and every subscriber. It is safe for
// concurrent use.
type Hub struct {
	ring *Ring
	rid  atomic.Int64 // last rid handed out; rid 0 means "nothing published yet"

	mu      sync.Mutex
	subs    map[int64]chan Delta
	nextSub int64

	// delivery window, guarded by mu
	pending  Delta       // deltas coalesced since the last send, waiting for the window
	lastSend time.Time   // when the last window's send left; zero before the first
	timer    *time.Timer // the one-shot flush closing the open window, if any
}

// NewHub returns a hub whose rid counter starts at 0.
func NewHub() *Hub {
	return &Hub{
		ring:    NewRing(),
		subs:    make(map[int64]chan Delta),
		pending: newDelta(),
	}
}

// Publish assigns the next rid, stores the delta and fans it out, returning
// the assigned rid. Delivery is rate limited to one coalesced send per
// publishInterval. It drops nothing silently: a subscriber whose buffer is
// full is closed and removed, and that client reconnects through the
// seq_gap path.
func (h *Hub) Publish(d Delta) int64 {
	// rid assignment and Append share the lock so ring entries stay
	// contiguous and ordered even under concurrent publishers.
	h.mu.Lock()
	rid := h.rid.Add(1)
	d.RID = rid
	h.ring.Append(d)

	coalesceInto(&h.pending, d)
	h.pending.RID = rid
	h.armLocked()
	h.mu.Unlock()

	return rid
}

// armLocked either delivers pending now — the window has elapsed — or arms
// the one-shot timer that closes the window. At most one send can leave per
// publishInterval either way.
func (h *Hub) armLocked() {
	if h.timer != nil {
		return // the open window's flush will carry this delta too
	}
	if time.Since(h.lastSend) >= publishInterval {
		h.deliverLocked()
		return
	}
	h.timer = time.AfterFunc(time.Until(h.lastSend.Add(publishInterval)), h.flush)
}

// flush closes a delivery window. The timer goroutine is owned by the hub and
// terminates with this single fire.
func (h *Hub) flush() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.timer = nil
	h.deliverLocked()
}

// deliverLocked fans the coalesced pending delta out to every subscriber with
// a non-blocking send. A full buffer closes and removes that subscriber.
func (h *Hub) deliverLocked() {
	if len(h.subs) == 0 {
		// Nobody is listening; pending is spent without opening a window, so
		// the first delivery to a real subscriber is not delayed.
		h.pending = newDelta()
		return
	}

	h.lastSend = time.Now()

	d := h.pending
	finalizeRemovals(&d)
	h.pending = newDelta()

	for id, ch := range h.subs {
		select {
		case ch <- d:
		default:
			// Slow subscriber: closing ends its read loop; it reconnects and
			// takes the full_update/seq_gap path.
			close(ch)
			delete(h.subs, id)
		}
	}
}

// Subscribe returns a channel seeded with the caller's starting delta.
// lastEventID is the Last-Event-ID header value: on a ring hit the seed is
// the coalesced diff since that rid; an empty, unparseable or evicted value
// seeds snapshot() with FullUpdate and SeqGap forced true. cancel removes the
// subscriber, and so does cancellation of ctx; either closes the channel.
//
// snapshot runs while the hub lock is held so no Publish can slip between the
// seed and the registration — keep it cheap.
func (h *Hub) Subscribe(ctx context.Context, lastEventID string, snapshot func() Delta) (<-chan Delta, func()) {
	ch := make(chan Delta, subscriberBuffer)

	h.mu.Lock()
	seed := h.Snapshot(parseRID(lastEventID), snapshot)
	ch <- seed // a fresh buffer always takes the seed
	h.nextSub++
	id := h.nextSub
	h.subs[id] = ch
	h.mu.Unlock()

	done := make(chan struct{})
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			close(done)
			h.remove(id)
		})
	}

	// ctx cancellation is the other way a subscriber ends; this goroutine is
	// owned by Subscribe and exits on it or on the first cancel.
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-done:
		}
	}()

	return ch, cancel
}

// Snapshot answers GET /sync with the same envelope the stream carries: the
// coalesced diff since rid on a ring hit, otherwise full() with FullUpdate
// and SeqGap forced true and the current rid.
func (h *Hub) Snapshot(rid int64, full func() Delta) Delta {
	if d, ok := h.ring.Since(rid); ok {
		return d
	}

	// Read the cursor before the snapshot: a delta published after this load
	// can appear in both the snapshot and a later diff (a benign duplicate),
	// but a newer cursor over stale contents would lose it.
	cur := h.rid.Load()
	d := full()
	if d.Tasks == nil {
		d.Tasks = map[string]json.RawMessage{}
	}
	if d.TasksRemoved == nil {
		d.TasksRemoved = []string{}
	}
	d.RID = cur
	d.FullUpdate = true
	d.SeqGap = true
	return d
}

// Clients reports the current subscriber count for the dltool_sse_clients
// gauge.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return len(h.subs)
}

// remove deletes one subscriber and closes its channel so its read loop
// ends. A subscriber already evicted by deliverLocked is absent, which makes
// this a no-op.
func (h *Hub) remove(id int64) {
	h.mu.Lock()
	defer h.mu.Unlock()

	cur, ok := h.subs[id]
	if !ok {
		return
	}
	delete(h.subs, id)
	close(cur)
}

// parseRID reads the Last-Event-ID header. Anything but a non-negative
// integer is 0, which the ring treats as outside it and the caller answers
// with a full update.
func parseRID(lastEventID string) int64 {
	rid, err := strconv.ParseInt(strings.TrimSpace(lastEventID), 10, 64)
	if err != nil || rid < 0 {
		return 0
	}
	return rid
}
