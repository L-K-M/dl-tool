package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/robfig/cron/v3"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/store"
)

// JobKindRSSPoll is the jobs.kind the feed-poller cron entry enqueues; the
// handler is rss.Poller.PollDue, registered by the composition root (T066).
const JobKindRSSPoll = "rss_poll"

// rssPollSchedule is the cadence of the poll-enqueue entry: one pass per
// minute, so a newly created feed (next_fetch_at = now) waits at most a
// minute for its first poll.
const rssPollSchedule = "@every 1m"

// scheduleEvalSpec is the cadence of the bandwidth-schedule entry
// (T081): once a minute, the granularity the 168-cell grid is defined
// at — a cell spans a whole hour, so no faster tick can observe a
// boundary the minute tick misses.
const scheduleEvalSpec = "* * * * *"

// queryPendingOfKind counts the pending rows of one jobs.kind; the cron
// entry enqueues only when this is zero. The check collapses bursts of
// ticks but is not an atomic guard: a kind stays unenqueued only while a
// row sits pending, and the poller's own in-flight claim — not this count —
// keeps overlapping passes from double-fetching a feed.
const queryPendingOfKind = `SELECT COUNT(*) FROM jobs WHERE kind = ? AND state = 'pending'`

// Scheduler owns the periodic job enqueues. Entries are code — the only
// clock-driven schedules — while the jobs table stays the queue
// (ADR-0015). cmd/dl-tool starts it in OnStart on the run context and
// cancels that context in OnStop.
type Scheduler struct {
	db       *sqlx.DB
	log      *slog.Logger
	settings *store.SettingsStore
	// gov is set by WithGovernor and now is the injectable clock —
	// time.Now by default, a test's fixed clock in cron_test.go.
	gov *engine.Governor
	now func() time.Time
}

func NewScheduler(db *sqlx.DB, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}

	return &Scheduler{db: db, log: log, settings: store.NewSettingsStore(db), now: time.Now}
}

// WithGovernor attaches the bandwidth governor and arms the "* * * * *"
// entry that calls EvaluateSchedule — Start registers the entry for any
// attached governor, whether or not the schedule is enabled; the
// evaluator itself is the no-op while the schedule_enabled settings key
// is false. The field is set before the cron goroutine can observe it,
// so the attach needs no timing argument.
func (s *Scheduler) WithGovernor(gov *engine.Governor) *Scheduler {
	s.gov = gov
	return s
}

// Start runs the cron until ctx ends, then stops it and waits for any
// in-flight entry to finish so OnStop drains like the worker pool.
func (s *Scheduler) Start(ctx context.Context) {
	// cron entries run in bare goroutines; Recover keeps an entry panic —
	// a driver failure or a future edit — from taking the process down.
	c := cron.New(cron.WithChain(cron.Recover(cronLogger{log: s.log})))
	if _, err := c.AddFunc(rssPollSchedule, func() { s.enqueueOnce(ctx, JobKindRSSPoll) }); err != nil {
		// A static schedule string cannot fail to parse; if it ever does,
		// say so rather than run silently without the entry.
		s.log.ErrorContext(ctx, "register cron entry failed", "schedule", rssPollSchedule, "err", err)
		return
	}
	if s.gov != nil {
		if _, err := c.AddFunc(scheduleEvalSpec, func() { s.evaluateOnce(ctx) }); err != nil {
			s.log.ErrorContext(ctx, "register cron entry failed", "schedule", scheduleEvalSpec, "err", err)
			return
		}
	}

	c.Start()
	// Apply the active cell at once rather than waiting for the first
	// minute boundary — a restart inside a No Download window would
	// otherwise leave running transfers live for up to a minute.
	if s.gov != nil {
		s.evaluateOnce(ctx)
	}
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

// evaluateOnce runs the minute tick of the bandwidth schedule. Errors
// are logged, never returned — a missed minute is recovered by the next
// tick, and the cron entry has no retry channel of its own.
func (s *Scheduler) evaluateOnce(ctx context.Context) {
	if err := s.EvaluateSchedule(ctx, s.now()); err != nil && ctx.Err() == nil {
		s.log.ErrorContext(ctx, "schedule evaluation failed", "err", err)
	}
}

// EvaluateSchedule reads the cell for now — day*24+hour with Monday as
// day 0, in time.Local — resolves the mode and calls Governor.ApplyMode.
// It is a no-op while schedule_enabled is false and idempotent inside
// one cell: ApplyMode changes nothing at the engines when the resolved
// mode is the one already applied.
func (s *Scheduler) EvaluateSchedule(ctx context.Context, now time.Time) error {
	if s.gov == nil {
		return errors.New("jobs: schedule evaluation has no governor")
	}

	cells, enabled, err := s.settings.ScheduleSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("jobs: read bandwidth schedule snapshot: %w", err)
	}
	if !enabled {
		return nil
	}

	local := now.In(time.Local)
	// Weekday counts Sunday as 0; the grid counts Monday as 0.
	day := (int(local.Weekday()) + 6) % 7
	idx := day*24 + local.Hour()
	var mode engine.Mode
	switch cells[idx] {
	case store.ScheduleNoDownload:
		mode = engine.ModeNoDownload
	case store.ScheduleDefault:
		mode = engine.ModeDefault
	case store.ScheduleAlternative:
		mode = engine.ModeAlternative
	default:
		return fmt.Errorf("jobs: bandwidth schedule cell %d holds unknown mode %q", idx, cells[idx])
	}

	if err := s.gov.ApplyMode(ctx, mode); err != nil {
		return fmt.Errorf("jobs: apply schedule mode %s: %w", mode, err)
	}

	return nil
}

// cronLogger bridges robfig/cron's Logger onto slog so recovered panics
// land in the same log stream as everything else; the cron Info level
// (job scheduling noise) is dropped — the scheduler logs its own events.
type cronLogger struct {
	log *slog.Logger
}

func (cronLogger) Info(string, ...any) {}

func (l cronLogger) Error(err error, msg string, kv ...any) {
	l.log.Error("cron "+msg, append(kv, "err", err)...)
}
