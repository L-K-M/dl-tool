package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/robfig/cron/v3"

	"github.com/L-K-M/dl-tool/internal/store"
)

// JobKindRSSPoll is the jobs.kind the feed-poller cron entry enqueues; the
// handler is rss.Poller.PollDue, registered by the composition root (T066).
const JobKindRSSPoll = "rss_poll"

// rssPollSchedule is the cadence of the poll-enqueue entry: one pass per
// minute, so a newly created feed (next_fetch_at = now) waits at most a
// minute for its first poll.
const rssPollSchedule = "@every 1m"

// queryPendingOfKind counts the pending rows of one jobs.kind; the cron
// entry enqueues only when this is zero, so a slow minute can never stack
// duplicate poll passes.
const queryPendingOfKind = `SELECT COUNT(*) FROM jobs WHERE kind = ? AND state = 'pending'`

// Scheduler owns the periodic job enqueues. Entries are code — the only
// clock-driven schedules — while the jobs table stays the queue
// (ADR-0015). cmd/dl-tool starts it in OnStart on the run context and
// cancels that context in OnStop.
type Scheduler struct {
	db  *sqlx.DB
	log *slog.Logger
}

func NewScheduler(db *sqlx.DB, log *slog.Logger) *Scheduler {
	return &Scheduler{db: db, log: log}
}

// Start runs the cron until ctx ends, then stops it and waits for any
// in-flight entry to finish so OnStop drains like the worker pool.
func (s *Scheduler) Start(ctx context.Context) {
	c := cron.New()
	if _, err := c.AddFunc(rssPollSchedule, func() { s.enqueueOnce(ctx, JobKindRSSPoll) }); err != nil {
		// A static schedule string cannot fail to parse; if it ever does,
		// say so rather than run silently without the entry.
		s.log.ErrorContext(ctx, "register cron entry failed", "schedule", rssPollSchedule, "err", err)
		return
	}

	c.Start()
	<-ctx.Done()
	// Stop returns a context that completes when running entries finish.
	<-c.Stop().Done()
}

// enqueueOnce inserts one pending job of kind when none is already waiting.
// Errors are logged, never returned — a missed minute is recovered by the
// next tick, and the cron entry has no retry channel of its own.
func (s *Scheduler) enqueueOnce(ctx context.Context, kind string) {
	var pending int
	if err := s.db.GetContext(ctx, &pending, queryPendingOfKind, kind); err != nil {
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "cron pending check failed", "kind", kind, "err", err)
		}

		return
	}
	if pending > 0 {
		return
	}

	if _, err := store.EnqueueJob(ctx, s.db, kind, nil, struct{}{}, time.Now().UnixMilli()); err != nil {
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "cron enqueue failed", "kind", kind, "err", err)
		}
	}
}
