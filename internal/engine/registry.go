package engine

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// Registry holds one Engine per Name(). Instances are injected at the
// composition root; the registry itself never imports a concrete adapter
// (docs/06-download-engines.md §1). Adding an engine to dl-tool is one
// Register call beside the adapter's constructor.
//
// It also owns the task-operation lease table (T128): the one instance
// shared by the admission pass and the task action handlers, so an
// admission release and an operator action on the same task serialise
// in-process (ADR-0004's one-process deployment makes that the whole
// story beside the store's guarded writes).
type Registry struct {
	mu      sync.Mutex
	engines map[string]Engine
	// leases is the task-operation lock table: one entry per task id
	// that has a live holder or waiter. Entries are created lazily and
	// deleted by the last release, so the table does not grow with the
	// task count ever visited.
	leases map[string]*taskOpEntry
	// nextToken sequences ownership handles; guarded by mu.
	nextToken uint64
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{engines: make(map[string]Engine), leases: make(map[string]*taskOpEntry)}
}

// Register adds e under e.Name(). A duplicate name is a composition bug —
// two engines claiming one name — so it panics rather than silently
// shadowing the first registration.
func (r *Registry) Register(e Engine) {
	// Name() is an arbitrary interface call; take it before the lock so an
	// implementation that re-enters the registry cannot deadlock.
	name := e.Name()

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, dup := r.engines[name]; dup {
		panic("engine: duplicate registration: " + name)
	}
	r.engines[name] = e
}

// Get returns the engine registered under name, and whether one was.
func (r *Registry) Get(name string) (Engine, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.engines[name]
	return e, ok
}

// Names returns every registered engine name, sorted for stable iteration.
func (r *Registry) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	names := make([]string, 0, len(r.engines))
	for name := range r.engines {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TaskOpMode selects how AcquireTaskOp answers a held lease: an enum,
// because the two behaviours are call-site policies, not flags.
type TaskOpMode int

const (
	// TaskOpTry never waits: a held lease answers ErrTaskOpBusy at once,
	// so a periodic caller — the admission pass — skips a busy task
	// instead of parking its tick behind one slow action.
	TaskOpTry TaskOpMode = iota
	// TaskOpWait queues behind the current holder and returns with the
	// lease or with the context's error — the operator action's mode
	// (T127), which joins an in-flight admission release instead of
	// racing it.
	TaskOpWait
)

// ErrTaskOpBusy is TaskOpTry's answer when another operation holds the
// task's lease. It names contention, never failure: the caller skips or
// retries, and the admission pass treats it as a quiet skip.
var ErrTaskOpBusy = errors.New("engine: another task operation is in progress")

// taskOpToken is one acquisition's ownership handle. Holder identity
// is token identity, and each token carries a sequence number so two
// acquisitions never compare equal — a zero-sized token would make every
// allocation the runtime's shared zerobase address and collapse all
// holders into one. The release closure captured at acquisition acts
// only while its token is the entry's holder, which makes a second
// release of the same acquisition a no-op instead of a second handoff.
type taskOpToken struct{ seq uint64 }

// taskOpEntry is one task id's lease: the holder's token and the FIFO
// queue behind it. An entry lives in the registry's table exactly while
// a holder or a waiter can still refer to it — release deletes it only
// on an empty queue with no holder — so a later acquirer can never build
// a second live entry beside a parked waiter, and a parked waiter can
// never be overtaken by one.
type taskOpEntry struct {
	holder  *taskOpToken
	waiters []*taskOpWaiter
}

// taskOpWaiter is one parked TaskOpWait acquisition. The handoff that
// grants ownership closes ready exactly once, under the registry's
// mutex, and sets won in the same critical section — so a cancellation
// racing the handoff reads one verdict under that mutex and produces
// exactly one outcome: the lease with a closed ready, or ctx.Err().
// Ownership can never leak onto a goroutine that is not returning the
// release closure.
type taskOpWaiter struct {
	ctx   context.Context
	ready chan struct{}
	won   bool
	token *taskOpToken
}

// AcquireTaskOp takes the task-operation lease of taskID. Operations on
// one task id exclude each other; different ids never interact. mode
// selects the answer to a held lease: TaskOpTry returns ErrTaskOpBusy
// without waiting, TaskOpWait parks FIFO behind the holder and returns
// with the lease, or with ctx.Err() when its context ends first — a
// cancelled waiter leaves the queue, but a handoff that already won
// still returns the lease, because nobody else would release it. The
// returned release is idempotent.
func (r *Registry) AcquireTaskOp(ctx context.Context, taskID string, mode TaskOpMode) (func(), error) {
	r.mu.Lock()

	if r.leases == nil {
		// A zero-value Registry must stay usable: the lease table is lazy
		// the same way NewRegistry makes it.
		r.leases = make(map[string]*taskOpEntry)
	}

	entry := r.leases[taskID]
	if entry == nil {
		entry = &taskOpEntry{}
		r.leases[taskID] = entry
	}

	// Free lease: take it at once. No waiter can be queued on a free
	// entry — release deletes a holderless entry — so this cannot cut
	// ahead of anyone.
	if entry.holder == nil {
		token := r.mintTokenLocked()
		entry.holder = token
		r.mu.Unlock()

		return r.releaseTaskOp(taskID, entry, token), nil
	}

	if mode == TaskOpTry {
		r.mu.Unlock()

		return nil, ErrTaskOpBusy
	}

	waiter := &taskOpWaiter{ctx: ctx, ready: make(chan struct{}), token: r.mintTokenLocked()}
	entry.waiters = append(entry.waiters, waiter)
	r.mu.Unlock()

	select {
	case <-waiter.ready:
		// The handoff granted ownership under the mutex before closing
		// ready; this goroutine is now the holder.
		return r.releaseTaskOp(taskID, entry, waiter.token), nil
	case <-ctx.Done():
		r.mu.Lock()
		if waiter.won {
			// The handoff won the race before this goroutine took the
			// mutex: ownership already moved to this token, and only this
			// goroutine can return the release. Answering ctx.Err() here
			// would park the lease on a holder that never releases it.
			r.mu.Unlock()

			return r.releaseTaskOp(taskID, entry, waiter.token), nil
		}
		// Still queued: leave the queue and answer the context. The entry
		// keeps its holder — the queue can only exist behind one — so no
		// cleanup of the table is owed here.
		removeWaiter(entry, waiter)
		r.mu.Unlock()

		return nil, ctx.Err()
	}
}

// releaseTaskOp returns the idempotent release of one acquisition. Only
// the live holder's token acts; a second call from the same holder — or
// a call after ownership was handed on — finds another token in place
// and does nothing. Handoff grants the entry directly to the oldest
// waiter, skipping waiters whose context has already ended (their own
// goroutine answers ctx.Err()); with no grantable waiter left, the entry
// leaves the table, so ids accumulate nothing.
func (r *Registry) releaseTaskOp(taskID string, entry *taskOpEntry, token *taskOpToken) func() {
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()

		if entry.holder != token {
			return
		}

		for len(entry.waiters) > 0 {
			waiter := entry.waiters[0]
			entry.waiters = entry.waiters[1:]

			// A waiter whose context already ended answers ctx.Err() and
			// never returns the release; granting to it would strand the
			// lease. Skip it — its goroutine removes itself from the queue it
			// is still parked in, which removeWaiter tolerates by search.
			if waiter.ctx.Err() != nil {
				continue
			}

			entry.holder = waiter.token
			waiter.won = true
			close(waiter.ready)

			return
		}

		entry.holder = nil
		if r.leases[taskID] == entry {
			// The identity check keeps a release racing a stale entry from
			// deleting a fresh one a later acquirer built after this entry
			// left the table.
			delete(r.leases, taskID)
		}
	}
}

// removeWaiter takes waiter off entry's queue if it is still parked
// there. A handoff or a skip may have removed it already, so the removal
// searches rather than assumes a position.
func removeWaiter(entry *taskOpEntry, waiter *taskOpWaiter) {
	for i, candidate := range entry.waiters {
		if candidate != waiter {
			continue
		}
		entry.waiters = append(entry.waiters[:i], entry.waiters[i+1:]...)

		return
	}
}

// mintTokenLocked mints one distinct ownership handle. Caller holds mu.
func (r *Registry) mintTokenLocked() *taskOpToken {
	r.nextToken++

	return &taskOpToken{seq: r.nextToken}
}
