package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// leaseProbe counts concurrent holders of one lease, so the exclusion
// tests observe the invariant itself — never two holders at once — rather
// than only the ordering of two acquisitions.
type leaseProbe struct {
	inside atomic.Int64
	max    atomic.Int64
}

func (p *leaseProbe) enter() {
	inside := p.inside.Add(1)
	for {
		seen := p.max.Load()
		if inside <= seen || p.max.CompareAndSwap(seen, inside) {
			return
		}
	}
}

func (p *leaseProbe) leave() {
	p.inside.Add(-1)
}

// acquireOrFail drives AcquireTaskOp in Wait mode and fails the test on
// any answer but success. Test goroutine only: it FailNows.
func acquireOrFail(t *testing.T, reg *Registry, taskID string) func() {
	t.Helper()

	release, err := reg.AcquireTaskOp(t.Context(), taskID, TaskOpWait)
	if err != nil {
		t.Fatalf("acquire %s: %v", taskID, err)
	}

	return release
}

// waitForParkedWaiters polls the lease table until taskID's entry carries
// want queued waiters — the white-box determinisation of "the waiter has
// parked", so no test orders goroutines with sleeps.
func waitForParkedWaiters(t *testing.T, reg *Registry, taskID string, want int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		reg.mu.Lock()
		parked := 0
		if entry := reg.leases[taskID]; entry != nil {
			parked = len(entry.waiters)
		}
		reg.mu.Unlock()

		if parked == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("task %s never carried %d parked waiters", taskID, want)
}

// Operations on one task id exclude each other: while one holder is
// inside its critical section, a second Wait acquisition stays parked
// until the first release, and the two critical sections never overlap.
func TestTaskOpLeaseExcludesOneTaskID(t *testing.T) {
	reg := NewRegistry()
	probe := &leaseProbe{}
	const taskID = "tsk_one"

	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			release, err := reg.AcquireTaskOp(t.Context(), taskID, TaskOpWait)
			if err != nil {
				t.Errorf("worker %d: acquire: %v", i, err)

				return
			}
			defer release()

			probe.enter()
			defer probe.leave()

			// A held lease answers Try with the named busy error, so the
			// exclusion below is also observable from outside.
			if _, err := reg.AcquireTaskOp(t.Context(), taskID, TaskOpTry); !errors.Is(err, ErrTaskOpBusy) {
				t.Errorf("worker %d: Try while held = %v, want ErrTaskOpBusy", i, err)
			}

			time.Sleep(time.Millisecond)
		}()
	}
	wg.Wait()

	if got := probe.max.Load(); got != 1 {
		t.Fatalf("max concurrent holders = %d, want 1", got)
	}
}

// Different task ids never interact: two leases hold at once, and the
// release of one leaves the other untouched.
func TestTaskOpLeaseIndependentAcrossTaskIDs(t *testing.T) {
	reg := NewRegistry()

	first := acquireOrFail(t, reg, "tsk_a")
	second := acquireOrFail(t, reg, "tsk_b")

	if _, err := reg.AcquireTaskOp(t.Context(), "tsk_c", TaskOpTry); err != nil {
		t.Fatalf("Try of a third id while two are held: %v", err)
	}

	second()
	first()
}

// TaskOpTry answers the named busy error while the lease is held and
// takes it when free — the admission pass's whole interaction with the
// lease.
func TestTaskOpTryAnswersBusyWhileHeld(t *testing.T) {
	reg := NewRegistry()

	release := acquireOrFail(t, reg, "tsk_try")

	if _, err := reg.AcquireTaskOp(t.Context(), "tsk_try", TaskOpTry); !errors.Is(err, ErrTaskOpBusy) {
		t.Fatalf("Try while held = %v, want ErrTaskOpBusy", err)
	}

	release()

	if again, err := reg.AcquireTaskOp(t.Context(), "tsk_try", TaskOpTry); err != nil {
		t.Fatalf("Try once free = %v, want the lease", err)
	} else {
		again()
	}
}

// Waiting is FIFO: A hands to B — the only waiter queued when A releases
// — and a later acquirer can never overtake a parked waiter: C parks
// behind B, and receives the lease only after B's own release.
func TestTaskOpWaitIsFIFOAndCannotBeOvertaken(t *testing.T) {
	reg := NewRegistry()
	const taskID = "tsk_fifo"

	a := acquireOrFail(t, reg, taskID)

	var grants atomic.Int64
	granted := make(chan int64, 2)
	waiter := func() {
		release, err := reg.AcquireTaskOp(t.Context(), taskID, TaskOpWait)
		if err != nil {
			t.Errorf("waiter: %v", err)

			return
		}
		defer release()

		granted <- grants.Add(1)
	}

	go waiter() // B
	waitForParkedWaiters(t, reg, taskID, 1)

	go waiter() // C, parking behind B
	waitForParkedWaiters(t, reg, taskID, 2)

	a() // hand to B; C must stay parked behind it.

	select {
	case got := <-granted:
		if got != 1 {
			t.Fatalf("first grant went to waiter %d, want B (1)", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("A's release did not hand off to B")
	}

	select {
	case got := <-granted:
		if got != 2 {
			t.Fatalf("second grant went to waiter %d, want C (2)", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("B's release did not hand off to C")
	}
}

// A cancelled waiter is removed from the queue and answers its context
// error; the lease itself stays with the holder, and once the holder
// releases, a fresh acquisition takes the id — no ownership parked on the
// departed waiter.
func TestTaskOpCanceledWaiterAnswersItsContext(t *testing.T) {
	reg := NewRegistry()
	const taskID = "tsk_cancel"

	holder := acquireOrFail(t, reg, taskID)

	ctx, cancel := context.WithCancel(context.Background())
	waitErr := make(chan error, 1)
	go func() {
		_, err := reg.AcquireTaskOp(ctx, taskID, TaskOpWait)
		waitErr <- err
	}()

	waitForParkedWaiters(t, reg, taskID, 1)
	cancel()

	select {
	case err := <-waitErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled waiter did not return")
	}

	// The queue is empty again before the holder lets go.
	waitForParkedWaiters(t, reg, taskID, 0)
	holder()

	// The holder's release found an empty queue, so the id is free.
	if release, err := reg.AcquireTaskOp(ctx, taskID, TaskOpTry); err != nil {
		t.Fatalf("Try after the cancelled waiter left = %v, want the freed lease", err)
	} else {
		release()
	}
}

// The handoff/cancellation race produces exactly one outcome and cannot
// leak ownership: whichever side wins, the lease ends up releasable —
// with the waiter's own release when the handoff won, with the next
// holder or the table's cleanup when the cancellation won. The loop under
// -race shakes both interleavings.
func TestTaskOpHandoffCancelRaceHasOneOutcome(t *testing.T) {
	reg := NewRegistry()

	for i := range 100 {
		taskID := "tsk_race_a"
		if i%2 == 1 {
			taskID = "tsk_race_b"
		}

		holder := acquireOrFail(t, reg, taskID)

		ctx, cancel := context.WithCancel(context.Background())
		waitDone := make(chan func(), 1)
		go func() {
			release, err := reg.AcquireTaskOp(ctx, taskID, TaskOpWait)
			if err != nil {
				close(waitDone)

				return
			}
			waitDone <- release
		}()
		waitForParkedWaiters(t, reg, taskID, 1)

		// Cancel and release race: no sleep orders them.
		cancel()
		holder()

		select {
		case release, ok := <-waitDone:
			if ok {
				// The handoff won: the waiter owns the lease and its
				// release must work.
				release()
			}
			// Either way, the id must be free now.
			if probe, err := reg.AcquireTaskOp(context.Background(), taskID, TaskOpTry); err != nil {
				t.Fatalf("iteration %d: lease stuck after the race: %v", i, err)
			} else {
				probe()
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: the racing waiter never returned", i)
		}
	}
}

// The release of one acquisition is idempotent: a second call finds
// another token in place — the waiter the first call granted — and does
// nothing, so a double release neither frees a held lease nor grants a
// second one.
func TestTaskOpReleaseIsIdempotent(t *testing.T) {
	reg := NewRegistry()
	const taskID = "tsk_idem"

	a := acquireOrFail(t, reg, taskID)

	bDone := make(chan struct{})
	go func() {
		defer close(bDone)

		release, err := reg.AcquireTaskOp(t.Context(), taskID, TaskOpWait)
		if err != nil {
			t.Errorf("B: acquire: %v", err)

			return
		}
		defer release()

		// Inside B's critical section: A's second release must not have
		// freed the lease out from under B.
		if _, err := reg.AcquireTaskOp(t.Context(), taskID, TaskOpTry); !errors.Is(err, ErrTaskOpBusy) {
			t.Errorf("Try after A's double release = %v, want ErrTaskOpBusy — B still holds", err)
		}
	}()

	waitForParkedWaiters(t, reg, taskID, 1)
	a()
	a() // The idempotent second call: must be a no-op.

	select {
	case <-bDone:
	case <-time.After(5 * time.Second):
		t.Fatal("B was not granted after A's release")
	}

	if release, err := reg.AcquireTaskOp(t.Context(), taskID, TaskOpTry); err != nil {
		t.Fatalf("Try after both holders left = %v, want the freed lease", err)
	} else {
		release()
	}
}

// Entries do not accumulate: after every holder and waiter of an id has
// left — plain cycles, handoffs, skipped cancellations — the table holds
// nothing. Pinned on the table itself, because no behaviour can observe
// an absent entry.
func TestTaskOpEntriesDoNotAccumulate(t *testing.T) {
	reg := NewRegistry()

	// Plain cycles over several ids.
	for range 10 {
		for _, taskID := range []string{"tsk_x", "tsk_y", "tsk_z"} {
			release := acquireOrFail(t, reg, taskID)
			release()
		}
	}
	if got := len(reg.leases); got != 0 {
		t.Fatalf("table holds %d entries after plain cycles, want 0", got)
	}

	// A full handoff: A, then B behind it, then both gone.
	const taskID = "tsk_handoff"
	a := acquireOrFail(t, reg, taskID)
	bDone := make(chan struct{})
	go func() {
		defer close(bDone)

		release, err := reg.AcquireTaskOp(t.Context(), taskID, TaskOpWait)
		if err != nil {
			t.Errorf("handoff waiter: %v", err)

			return
		}
		release()
	}()
	waitForParkedWaiters(t, reg, taskID, 1)
	a()
	<-bDone
	if got := len(reg.leases); got != 0 {
		t.Fatalf("table holds %d entries after a handoff, want 0", got)
	}

	// A cancelled waiter skipped by the handoff: the holder releases past
	// it, its goroutine leaves the queue, and the entry is gone.
	holder := acquireOrFail(t, reg, taskID)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, _ = reg.AcquireTaskOp(ctx, taskID, TaskOpWait)
	}()
	waitForParkedWaiters(t, reg, taskID, 1)
	cancel()
	waitForParkedWaiters(t, reg, taskID, 0)
	holder()
	if got := len(reg.leases); got != 0 {
		t.Fatalf("table holds %d entries after a skipped cancellation, want 0", got)
	}
}
