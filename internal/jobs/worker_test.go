package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/store"
)

// jobs.state vocabulary, docs/04-data-model.md section 4.4.
const (
	statePending = "pending"
	stateRunning = "running"
	stateDone    = "done"
	stateFailed  = "failed"
)

const (
	testPollInterval = 5 * time.Millisecond
	waitTimeout      = 5 * time.Second
	waitTick         = 5 * time.Millisecond
	// drainSettle gives the pool a few poll cycles to prove a row in a
	// terminal state is never reclaimed.
	drainSettle = 50 * time.Millisecond
)

const queryJobByID = `SELECT id, kind, task_id, payload_json, state, attempts, max_attempts, run_after, locked_at, last_error
FROM jobs
WHERE id = ?`

func TestClaimIsExactlyOnce(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()

	const (
		jobCount      = 16
		claimantCount = 2
	)
	for range jobCount {
		enqueue(t, db, "race")
	}
	// Captured after the enqueues so every row is eligible.
	now := time.Now().UnixMilli()

	// Two concurrent claimants must never return the same row: the claim is
	// one UPDATE ... RETURNING over the single writer connection.
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed = make(map[string]int, jobCount)
	)
	wg.Add(claimantCount)
	for range claimantCount {
		go func() {
			defer wg.Done()
			for {
				job, err := store.ClaimJob(ctx, db, now)
				if errors.Is(err, store.ErrNotFound) {
					return
				}
				if err != nil {
					t.Errorf("claim job: %v", err)

					return
				}

				mu.Lock()
				claimed[job.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	require.Len(t, claimed, jobCount)
	for id, times := range claimed {
		require.Equal(t, 1, times, "job %s claimed more than once", id)
	}
}

func TestBackoffLadder(t *testing.T) {
	const now = int64(1_700_000_000_000)

	require.Equal(t, now+5_000, Backoff(now, 0))
	require.Equal(t, now+10_000, Backoff(now, 1))
	require.Equal(t, now+20_000, Backoff(now, 2))
	require.Equal(t, now+600_000, Backoff(now, 7))
	require.Equal(t, now+600_000, Backoff(now, 8))
	require.Equal(t, now+600_000, Backoff(now, 30))
}

func TestEnqueuedJobRunsExactlyOnce(t *testing.T) {
	db := newTestDB(t)
	worker := newTestWorker(db)

	var runs atomic.Int32
	worker.Register("count", func(context.Context, store.Job) error {
		runs.Add(1)

		return nil
	})
	startWorker(t, worker)

	id := enqueue(t, db, "count")
	waitFor(t, "job done", func() bool {
		return jobRow(t, db, id).State == stateDone
	})

	// A done row is never reclaimed, so the handler never runs twice.
	time.Sleep(drainSettle)
	require.Equal(t, int32(1), runs.Load())
}

func TestFailingJobIsRescheduledWithBackoff(t *testing.T) {
	db := newTestDB(t)
	worker := newTestWorker(db)
	worker.Register("flop", func(context.Context, store.Job) error {
		return errors.New("boom")
	})
	startWorker(t, worker)

	before := time.Now().UnixMilli()
	id := enqueue(t, db, "flop")
	waitFor(t, "first failure rescheduled", func() bool {
		job := jobRow(t, db, id)

		return job.State == statePending && job.Attempts == 1 && job.LastError != nil
	})
	after := time.Now().UnixMilli()

	job := jobRow(t, db, id)
	require.Equal(t, "boom", *job.LastError)
	require.Nil(t, job.LockedAt)

	// The claim incremented attempts to 1, so the ladder value is 10 s.
	const firstRetryDelayMS = int64(10_000)
	require.GreaterOrEqual(t, job.RunAfter, before+firstRetryDelayMS)
	require.LessOrEqual(t, job.RunAfter, after+firstRetryDelayMS)
}

func TestTerminalFailureKeepsRow(t *testing.T) {
	db := newTestDB(t)
	worker := newTestWorker(db)
	worker.Register("doomed", func(context.Context, store.Job) error {
		return errors.New("nope")
	})

	id := enqueue(t, db, "doomed")
	// max_attempts 1 sends the first failure straight to the terminal state.
	_, err := db.ExecContext(t.Context(), `UPDATE jobs SET max_attempts = 1 WHERE id = ?`, id)
	require.NoError(t, err)

	startWorker(t, worker)
	waitFor(t, "terminal failure", func() bool {
		return jobRow(t, db, id).State == stateFailed
	})

	// The failed row is the dead-letter queue: it is kept, not deleted, and
	// never reclaimed.
	time.Sleep(drainSettle)
	job := jobRow(t, db, id)
	require.Equal(t, stateFailed, job.State)
	require.Equal(t, 1, job.Attempts)
	require.Equal(t, "nope", *job.LastError)

	var count int
	require.NoError(t, db.GetContext(t.Context(), &count, `SELECT COUNT(*) FROM jobs WHERE id = ?`, id))
	require.Equal(t, 1, count)
}

func TestRecoverRunningJobsAtBoot(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()

	// A row left 'running' by a crash: claimed, never finished.
	id := enqueue(t, db, "rescued")
	lockedAt := time.Now().UnixMilli()
	_, err := db.ExecContext(ctx, `UPDATE jobs SET state = ?, locked_at = ? WHERE id = ?`, stateRunning, lockedAt, id)
	require.NoError(t, err)

	recovered, err := store.RecoverRunningJobs(ctx, db)
	require.NoError(t, err)
	require.Equal(t, int64(1), recovered)

	job := jobRow(t, db, id)
	require.Equal(t, statePending, job.State)
	require.Nil(t, job.LockedAt)

	// End to end: a worker started after the crash picks the row up.
	worker := newTestWorker(db)
	var runs atomic.Int32
	worker.Register("rescued", func(context.Context, store.Job) error {
		runs.Add(1)

		return nil
	})
	startWorker(t, worker)
	waitFor(t, "recovered job done", func() bool {
		return jobRow(t, db, id).State == stateDone
	})
	require.Equal(t, int32(1), runs.Load())
}

func TestUnknownKindFailsImmediately(t *testing.T) {
	db := newTestDB(t)
	worker := newTestWorker(db) // no handlers registered
	startWorker(t, worker)

	id := enqueue(t, db, "ghost")
	waitFor(t, "unknown kind failed", func() bool {
		return jobRow(t, db, id).State == stateFailed
	})

	// One claim, straight to 'failed': an unknown kind is not retried.
	job := jobRow(t, db, id)
	require.Equal(t, 1, job.Attempts)
	require.NotNil(t, job.LastError)
	require.Contains(t, *job.LastError, "ghost")
}

func TestShutdownRecordsInFlightOutcome(t *testing.T) {
	db := newTestDB(t)
	worker := newTestWorker(db)

	started := make(chan struct{})
	worker.Register("blocks", func(ctx context.Context, _ store.Job) error {
		close(started)
		<-ctx.Done()

		return ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(ctx)
	}()

	id := enqueue(t, db, "blocks")
	<-started
	cancelTime := time.Now().UnixMilli()
	cancel()

	// Run drains: it returns even though a handler was in flight at cancel.
	require.NoError(t, <-done)

	// The bookkeeping writes are detached from the pool context, so the
	// cancelled handler's failure was recorded instead of stranding the row
	// in 'running' until the next boot.
	job := jobRow(t, db, id)
	require.Equal(t, statePending, job.State)
	require.Equal(t, 1, job.Attempts)
	require.NotNil(t, job.LastError)
	require.Contains(t, *job.LastError, "context canceled")
	require.Greater(t, job.RunAfter, cancelTime)
}

func TestRecoveryFailureDoesNotStopPool(t *testing.T) {
	db := newTestDB(t)
	worker := newTestWorker(db)
	worker.recoverJobs = func(context.Context, *sqlx.DB) (int64, error) {
		return 0, errors.New("injected recovery failure")
	}

	var runs atomic.Int32
	worker.Register("alive", func(context.Context, store.Job) error {
		runs.Add(1)

		return nil
	})
	startWorker(t, worker)

	id := enqueue(t, db, "alive")
	waitFor(t, "job done despite failed recovery", func() bool {
		return jobRow(t, db, id).State == stateDone
	})
	require.Equal(t, int32(1), runs.Load())
}

func TestPanicInHandlerIsContained(t *testing.T) {
	db := newTestDB(t)
	worker := newTestWorker(db)
	worker.Register("panics", func(context.Context, store.Job) error {
		panic("kaboom")
	})

	var survived atomic.Int32
	worker.Register("survivor", func(context.Context, store.Job) error {
		survived.Add(1)

		return nil
	})
	startWorker(t, worker)

	panickedID := enqueue(t, db, "panics")
	waitFor(t, "panic routed through FailJob", func() bool {
		job := jobRow(t, db, panickedID)

		return job.Attempts == 1 && job.LastError != nil
	})

	job := jobRow(t, db, panickedID)
	require.Equal(t, statePending, job.State)
	require.Contains(t, *job.LastError, "kaboom")

	// The pool kept claiming: an unrelated job still runs to completion.
	survivorID := enqueue(t, db, "survivor")
	waitFor(t, "pool still claims after panic", func() bool {
		return jobRow(t, db, survivorID).State == stateDone
	})
	require.Equal(t, int32(1), survived.Load())
}

func newTestDB(t *testing.T) *sqlx.DB {
	t.Helper()

	dir := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(dir, "dl-tool.db"), filepath.Join(dir, "backups"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	return db
}

func newTestWorker(db *sqlx.DB) *Worker {
	worker := NewWorker(db, slog.New(slog.NewTextHandler(io.Discard, nil)), 1)
	worker.poll = testPollInterval

	return worker
}

// startWorker runs the pool; cleanup cancels it and waits for a clean drain,
// which also exercises the OnStop contract.
func startWorker(t *testing.T, worker *Worker) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
}

func enqueue(t *testing.T, db *sqlx.DB, kind string) string {
	t.Helper()

	id, err := store.EnqueueJob(t.Context(), db, kind, nil, map[string]string{"probe": "1"}, time.Now().UnixMilli())
	require.NoError(t, err)

	return id
}

func jobRow(t *testing.T, db *sqlx.DB, id string) store.Job {
	t.Helper()

	var job store.Job
	require.NoError(t, db.GetContext(t.Context(), &job, queryJobByID, id))

	return job
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(waitTick)
	}

	t.Fatalf("timed out waiting for %s", what)
}
