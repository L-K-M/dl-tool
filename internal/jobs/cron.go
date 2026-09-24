package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
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

const (
	// backupSpec is the nightly database snapshot of doc 17 section 3.2.
	backupSpec = "0 3 * * *"
	// retentionSpec runs the nightly table prunes of doc 04 section 7 an
	// hour after the backup entry: robfig/cron dispatches each entry on
	// its own goroutine, so same-tick entries would race VACUUM INTO.
	retentionSpec = "0 4 * * *"
	// searchPruneSpec runs the hourly search_jobs prune at minute 30, off
	// the top of the hour so it never shares a tick with the nightly
	// entries.
	searchPruneSpec = "30 * * * *"
)

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
	// watcher is set by WithWatcher: the watch-folder loader Start runs
	// beside the cron entries (T083).
	watcher *Watcher
	// maintenance and backupDir are set by WithMaintenance: the nightly
	// backup, the retention prunes and the hourly search prune run only
	// while a store is attached (T091).
	maintenance *store.MaintenanceStore
	backupDir   string
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

// WithWatcher attaches the watch-folder loader; Start runs w.Run on the
// scheduler context beside the cron entries and joins it during the
// drain. The field is set before the cron goroutine can observe it — the
// same attach rule WithGovernor follows. The scheduler's logger is handed
// over so the loader reports in the same stream.
func (s *Scheduler) WithWatcher(w *Watcher) *Scheduler {
	if w == nil {
		return s
	}
	if w.log == nil {
		w.log = s.log
	}
	s.watcher = w
	return s
}

// WithMaintenance attaches the backup and retention store; Start arms the
// three maintenance entries — the 03:00 backup and prune, the 04:00
// retention prunes and the hourly search_jobs prune — while one is set.
// The composition root hands over the same instance POST /system/backup
// uses, so the ErrBackupRunning lock spans both. The field is set before
// the cron goroutine can observe it — the same attach rule WithGovernor
// follows.
func (s *Scheduler) WithMaintenance(m *store.MaintenanceStore, backupDir string) *Scheduler {
	if m == nil {
		return s
	}
	s.maintenance = m
	s.backupDir = backupDir
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
	if s.maintenance != nil {
		entries := []struct {
			spec string
			run  func()
		}{
			{backupSpec, func() { s.backupOnce(ctx) }},
			{retentionSpec, func() { s.retentionOnce(ctx) }},
			{searchPruneSpec, func() { s.searchPruneOnce(ctx) }},
		}
		for _, entry := range entries {
			if _, err := c.AddFunc(entry.spec, entry.run); err != nil {
				// A static schedule string cannot fail to parse; if it
				// ever does, say so rather than run silently without it.
				s.log.ErrorContext(ctx, "register cron entry failed", "schedule", entry.spec, "err", err)
				return
			}
		}
	}

	// Apply the active cell at once rather than waiting for the first
	// minute boundary — a restart inside a No Download window would
	// otherwise leave running transfers live for up to a minute.
	// Evaluated before c.Start so this call cannot overlap a tick.
	if s.gov != nil {
		s.evaluateOnce(ctx)
	}

	// The watch-folder loader runs beside the cron entries on the same
	// context and is joined during the drain, so Stop leaves no scan
	// running against a store the caller is about to close.
	var loops sync.WaitGroup
	if s.watcher != nil {
		loops.Add(1)
		go func() {
			defer loops.Done()
			for {
				if err := s.watcher.Run(ctx); err != nil && ctx.Err() == nil {
					s.log.ErrorContext(ctx, "watch folder loader stopped; restarting", "err", err)
				}
				// A returned Run — whether it errored or not — restarts
				// after a minute: the folder list failing once must not
				// leave the loader dead for the process's lifetime.
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Minute):
				}
			}
		}()
	}

	c.Start()
	<-ctx.Done()
	// Stop returns a context that completes when running entries finish.
	<-c.Stop().Done()
	loops.Wait()
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

// backupOnce runs the nightly snapshot and prune of doc 17 section 3.2.
// Errors are logged, never returned — a missed night is recovered by the
// next tick, and the cron entry has no retry channel of its own.
func (s *Scheduler) backupOnce(ctx context.Context) {
	result, err := s.maintenance.BackupInto(ctx, s.backupDir)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrBackupRunning):
			// A manual POST /system/backup holds the shared lock: the
			// tick is skipped, not failed — the in-flight backup is the
			// night's snapshot.
			s.log.InfoContext(ctx, "nightly backup skipped: another backup is running")
		case ctx.Err() == nil:
			s.log.ErrorContext(ctx, "nightly backup failed", "err", err)
		}

		return
	}
	s.log.InfoContext(ctx, "nightly backup written",
		"path", result.Path, "size_bytes", result.SizeBytes)

	if _, err := s.maintenance.PruneBackups(ctx, s.backupDir, store.BackupKeepCount); err != nil && ctx.Err() == nil {
		s.log.ErrorContext(ctx, "backup retention prune failed", "err", err)
	}
}

// retentionOnce runs the two nightly prunes of doc 04 section 7 —
// task_events past 90 days and done jobs past 7 days — on the 04:00
// entry, an hour after the backup so they never share a VACUUM INTO tick.
func (s *Scheduler) retentionOnce(ctx context.Context) {
	now := s.now()
	if _, err := s.maintenance.PruneTaskEvents(ctx, now); err != nil && ctx.Err() == nil {
		s.log.ErrorContext(ctx, "task event retention prune failed", "err", err)
	}
	if _, err := s.maintenance.PruneDoneJobs(ctx, now); err != nil && ctx.Err() == nil {
		s.log.ErrorContext(ctx, "done job retention prune failed", "err", err)
	}
}

// searchPruneOnce runs the hourly search_jobs prune of doc 04 section 7;
// the search_results rows follow through ON DELETE CASCADE.
func (s *Scheduler) searchPruneOnce(ctx context.Context) {
	if _, err := s.maintenance.PruneSearchJobs(ctx, s.now()); err != nil && ctx.Err() == nil {
		s.log.ErrorContext(ctx, "search job retention prune failed", "err", err)
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

// activeCell resolves the grid cell in force at now: the wall-clock
// hour and weekday of now in time.Local — the container's TZ — index
// the grid day*24+hour with Monday as day 0. Daylight saving needs no
// special case: on the repeated hour of a fall-back transition both
// wall-clock occurrences read the same cell, so it is applied twice,
// and on the skipped hour of a spring-forward transition no wall-clock
// instant reads the cell, so it is never applied.
func activeCell(cells [168]store.ScheduleMode, now time.Time) store.ScheduleMode {
	local := now.In(time.Local)
	// Weekday counts Sunday as 0; the grid counts Monday as 0.
	day := (int(local.Weekday()) + 6) % 7
	return cells[day*24+local.Hour()]
}

// EvaluateSchedule reads the cell for now through activeCell, resolves
// the mode and calls Governor.ApplyMode. It is a no-op while
// schedule_enabled is false and idempotent inside one cell: ApplyMode
// changes nothing at the engines when the resolved mode is the one
// already applied.
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

	cell := activeCell(cells, now)
	var mode engine.Mode
	switch cell {
	case store.ScheduleNoDownload:
		mode = engine.ModeNoDownload
	case store.ScheduleDefault:
		mode = engine.ModeDefault
	case store.ScheduleAlternative:
		mode = engine.ModeAlternative
	default:
		return fmt.Errorf("jobs: bandwidth schedule cell at %s holds unknown mode %q", now.In(time.Local), cell)
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
