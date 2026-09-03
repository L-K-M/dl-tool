package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/store"
)

// errNoHandler marks a job whose kind has no registered handler — a boot-time
// wiring bug, so the job is failed terminally, not retried into a loop.
var errNoHandler = errors.New("jobs: no handler registered for kind")

// Handler runs one job. It must be idempotent, keyed on (kind, task_id): a
// job may run twice and running twice must be harmless.
type Handler func(ctx context.Context, j store.Job) error

// Worker is the in-process pool over the jobs table.
type Worker struct {
	db       *sqlx.DB
	log      *slog.Logger
	handlers map[string]Handler
	size     int
	poll     time.Duration
	// recoverJobs is the boot-recovery seam; tests replace it to inject a
	// failure.
	recoverJobs func(ctx context.Context, db *sqlx.DB) (int64, error)
}

// defaultPollInterval is how long an idle worker sleeps between claims.
const defaultPollInterval = time.Second

func NewWorker(db *sqlx.DB, log *slog.Logger, size int) *Worker {
	return &Worker{
		db:          db,
		log:         log,
		handlers:    make(map[string]Handler),
		size:        size,
		poll:        defaultPollInterval,
		recoverJobs: store.RecoverRunningJobs,
	}
}

// Register binds a handler to a jobs.kind. Registering a kind twice panics at
// boot, because duplicate wiring is a programming error.
func (w *Worker) Register(kind string, h Handler) {
	if _, exists := w.handlers[kind]; exists {
		panic(fmt.Sprintf("jobs: handler for kind %q already registered", kind))
	}

	w.handlers[kind] = h
}

// Run recovers rows stranded by a crash, then claims and runs jobs until ctx
// is cancelled; it returns only once every in-flight handler has finished, so
// OnStop drains cleanly.
func (w *Worker) Run(ctx context.Context) error {
	recovered, err := w.recoverJobs(ctx, w.db)
	if err != nil {
		// Keep the pool alive: a failed recovery strands old 'running' rows
		// until the next boot, and claimLoop already tolerates DB errors —
		// returning here would leave a dead pool behind a healthy server.
		w.log.ErrorContext(ctx, "recover running jobs failed", "err", err)
	} else {
		w.log.InfoContext(ctx, "recovered stranded jobs", "count", recovered)
	}

	var wg sync.WaitGroup
	wg.Add(w.size)
	for range w.size {
		go func() {
			defer wg.Done()
			w.claimLoop(ctx)
		}()
	}
	wg.Wait()

	return nil
}

// Backoff is run_after = now_ms + min(600000, 5000 * 2^attempts). It defers
// to store.Backoff so the ladder has one home, next to the jobs table.
func Backoff(now int64, attempts int) int64 {
	return store.Backoff(now, attempts)
}

// claimLoop claims one job per pass and sleeps between passes when no row is
// eligible or the claim itself failed, so an error cannot hot-spin.
func (w *Worker) claimLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		job, err := store.ClaimJob(ctx, w.db, time.Now().UnixMilli())
		if err == nil {
			w.execute(ctx, job)
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			w.log.ErrorContext(ctx, "job claim failed", "err", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(w.poll):
		}
	}
}

// execute runs the registered handler and routes the outcome: success
// completes the row, an error reschedules or fails it, an unknown kind fails
// terminally on the spot.
func (w *Worker) execute(ctx context.Context, job store.Job) {
	handler, ok := w.handlers[job.Kind]
	if !ok {
		err := fmt.Errorf("%w %q", errNoHandler, job.Kind)
		w.log.ErrorContext(ctx, "job failed permanently", append(jobLogAttrs(job), "err", err)...)
		w.fail(ctx, job, job.MaxAttempts, err)

		return
	}

	if err := runSafely(ctx, handler, job); err != nil {
		w.fail(ctx, job, job.Attempts, err)

		return
	}

	writeCtx, cancelWrite := detachedWrite(ctx)
	defer cancelWrite()
	if err := store.CompleteJob(writeCtx, w.db, job.ID, time.Now().UnixMilli()); err != nil {
		// The row stays 'running' and is recovered — and re-run, harmlessly —
		// at the next boot.
		w.log.ErrorContext(ctx, "job completion update failed", append(jobLogAttrs(job), "err", err)...)

		return
	}

	w.log.InfoContext(ctx, "job done", jobLogAttrs(job)...)
}

// fail routes a handler error into the store's retry-or-terminal bookkeeping.
// A reschedule is degraded but handled (warn); a terminal failure means a
// user-visible thing did not happen (error).
func (w *Worker) fail(ctx context.Context, job store.Job, attempts int, cause error) {
	attrs := append(jobLogAttrs(job), "attempts", attempts, "err", cause)
	if attempts >= job.MaxAttempts {
		w.log.ErrorContext(ctx, "job failed permanently", attrs...)
	} else {
		w.log.WarnContext(ctx, "job failed, rescheduled", attrs...)
	}

	writeCtx, cancelWrite := detachedWrite(ctx)
	defer cancelWrite()
	if err := store.FailJob(writeCtx, w.db, job.ID, attempts, job.MaxAttempts, cause.Error(), time.Now().UnixMilli()); err != nil {
		w.log.ErrorContext(ctx, "job failure update failed", append(attrs, "err", err)...)
	}
}

// bookkeepingWriteTimeout bounds the detached outcome writes; a real SQLite
// write on the single writer connection takes microseconds, so 30 s means
// wedged, not slow.
const bookkeepingWriteTimeout = 30 * time.Second

// detachedWrite returns the context for the terminal bookkeeping writes:
// immune to the pool's cancellation — an OnStop cancel mid-handler must not
// strand the row in 'running' — but time-bounded, so a wedged write cannot
// stall the drain forever.
func detachedWrite(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), bookkeepingWriteTimeout)
}

// runSafely converts a handler panic into an error, so one bad handler cannot
// stop the pool; the error is routed through FailJob like any other.
func runSafely(ctx context.Context, handler Handler, job store.Job) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("jobs: handler for kind %q panicked: %v", job.Kind, recovered)
		}
	}()

	return handler(ctx, job)
}

// jobLogAttrs carries the standard keys of docs/14-conventions.md section
// 3.1: task_id only when the job is bound to one.
func jobLogAttrs(job store.Job) []any {
	attrs := []any{"job_id", job.ID, "kind", job.Kind}
	if job.TaskID != nil {
		attrs = append(attrs, "task_id", *job.TaskID)
	}

	return attrs
}
