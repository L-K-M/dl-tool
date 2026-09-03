// Package sync owns the SSE sync payload: the rid ring buffer that keeps the
// last deltas and the hub that fans them out to subscribers at 1 Hz. The wire
// format is docs/05-api-contract.md section 6.1; the endpoints that frame
// these deltas as SSE are T025's, not this package's.
package sync

import (
	"encoding/json"
	"slices"
	"sync"
)

// RingDepth is 300 deltas, about five minutes at the 1 Hz push rate
// (docs/03-architecture.md section 8.6). No other depth exists.
const RingDepth = 300

// Stats is the aggregate block every sync payload carries.
type Stats struct {
	SpeedDown int64 `json:"speed_down"` // bytes per second
	SpeedUp   int64 `json:"speed_up"`   // bytes per second
	Active    int   `json:"active"`
	Queued    int   `json:"queued"`
}

// Delta is one sync event. It marshals to exactly the JSON of
// docs/05-api-contract.md section 6.1; GET /sync returns the identical bytes.
type Delta struct {
	RID               int64                      `json:"rid"`
	FullUpdate        bool                       `json:"full_update"`
	Tasks             map[string]json.RawMessage `json:"tasks"`
	TasksRemoved      []string                   `json:"tasks_removed"`
	Categories        map[string]json.RawMessage `json:"categories,omitempty"`
	CategoriesRemoved []string                   `json:"categories_removed,omitempty"`
	Stats             Stats                      `json:"stats"`
	SeqGap            bool                       `json:"seq_gap"`
}

// Ring stores the last RingDepth deltas keyed by rid. Rids are assigned by
// Hub.Publish and are contiguous, so entry rid lives at buf[rid%RingDepth].
type Ring struct {
	mu     sync.Mutex
	buf    [RingDepth]Delta
	newest int64 // rid of the newest entry; 0 while the ring is empty
	count  int   // entries stored, never more than RingDepth
}

// NewRing returns an empty ring.
func NewRing() *Ring {
	return &Ring{}
}

// Append stores d under d.RID, evicting the oldest entry once the ring is full.
func (r *Ring) Append(d Delta) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.buf[d.RID%RingDepth] = d
	r.newest = d.RID
	if r.count < RingDepth {
		r.count++
	}
}

// oldest is the smallest rid still stored; valid once count > 0.
func (r *Ring) oldest() int64 {
	return r.newest - int64(r.count) + 1
}

// Since returns one Delta coalescing every entry after rid. ok is false when
// rid is outside the ring — empty ring, older than the oldest retained entry,
// or newer than the newest — and the caller must then answer with a full
// update carrying SeqGap true. rid == newest is a hit that coalesces to an
// empty diff.
func (r *Ring) Since(rid int64) (d Delta, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.count == 0 || rid < r.oldest() || rid > r.newest {
		return Delta{}, false
	}

	d = newDelta()
	for i := rid + 1; i <= r.newest; i++ {
		coalesceInto(&d, r.buf[i%RingDepth])
	}
	finalizeRemovals(&d)

	// The answer always carries where the reader now stands, and the freshest
	// stats, even when nothing followed rid.
	d.RID = r.newest
	d.Stats = r.buf[r.newest%RingDepth].Stats
	return d, true
}

// newDelta returns a Delta whose collections are non-nil, so they marshal as
// {} and [] rather than null (docs/05-api-contract.md section 6.1).
func newDelta() Delta {
	return Delta{
		Tasks:             make(map[string]json.RawMessage),
		TasksRemoved:      []string{},
		Categories:        make(map[string]json.RawMessage),
		CategoriesRemoved: []string{},
	}
}

// coalesceInto merges next into dst, walking oldest first: Tasks and
// Categories entries win newest-last — a re-add after a removal cancels the
// tombstone — removals union, Stats always come from the newest entry, and
// a full snapshot replaces everything merged before it. FullUpdate is
// sticky — a full snapshot inside a range keeps the coalesced result a
// complete set, so no client deep-merges a full listing over a stale store.
func coalesceInto(dst *Delta, next Delta) {
	if next.FullUpdate {
		// A full snapshot is authoritative: its collections are the complete
		// post-state, so they replace anything merged before them rather than
		// unioning with it. The sticky assignment below keeps the flag set.
		*dst = newDelta()
	}
	for id, patch := range next.Tasks {
		dst.Tasks[id] = patch
		// Newest wins: an add after a removal cancels the tombstone.
		dst.TasksRemoved = slices.DeleteFunc(dst.TasksRemoved, func(v string) bool { return v == id })
	}
	dst.TasksRemoved = union(dst.TasksRemoved, next.TasksRemoved)
	for _, id := range next.TasksRemoved {
		// Newest wins here too: a removal after an add drops the stale patch.
		delete(dst.Tasks, id)
	}

	for name, cat := range next.Categories {
		dst.Categories[name] = cat
		// Newest wins, for the same reason, on the category side.
		dst.CategoriesRemoved = slices.DeleteFunc(dst.CategoriesRemoved, func(v string) bool { return v == name })
	}
	dst.CategoriesRemoved = union(dst.CategoriesRemoved, next.CategoriesRemoved)
	for _, name := range next.CategoriesRemoved {
		delete(dst.Categories, name)
	}

	if next.FullUpdate {
		dst.FullUpdate = true
	}
	dst.Stats = next.Stats
}

// finalizeRemovals guarantees the invariant the coalescing already keeps:
// no id sits in both the updates and the removals of an exposed payload, so
// the result is self-consistent whatever order a client applies it in.
func finalizeRemovals(d *Delta) {
	for _, id := range d.TasksRemoved {
		delete(d.Tasks, id)
	}
	for _, name := range d.CategoriesRemoved {
		delete(d.Categories, name)
	}
}

// union appends every element of addition that dst does not already carry.
func union(dst, addition []string) []string {
	for _, v := range addition {
		if !slices.Contains(dst, v) {
			dst = append(dst, v)
		}
	}
	return dst
}
