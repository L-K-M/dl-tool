package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/store"
)

// JobKindExtract is the jobs.kind the post-processing chain enqueues for a
// completed task whose payload is a supported archive.
const JobKindExtract = "extract"

// JobKindMove is the jobs.kind the post-processing chain enqueues for a
// completed task whose payload sits outside its resolved destination (T076).
const JobKindMove = "move"

// Settings keys the chain reads (docs/11-config-reference.md section 5).
// Neither is seeded by 00001_init.sql, so a missing row means the
// documented default — false for both.
const (
	settingAutoExtract          = "auto_extract"
	settingAutoRemoveOnComplete = "auto_remove_on_complete"
)

// The post-processing task_events codes of docs/14-conventions.md section
// 4. Transition writes the three extract codes inside its own transaction;
// postprocess.autoremoved is appended by the chain before the row goes.
const (
	eventExtractStarted   = "postprocess.extract.started"
	eventExtractCompleted = "postprocess.extract.completed"
	eventExtractFailed    = "postprocess.extract.failed"
	eventAutoRemoved      = "postprocess.autoremoved"
)

// archiveExtensions is the FR-100 set; the .tar.gz spelling reaches the
// gzip branch through its .gz suffix.
var archiveExtensions = []string{".zip", ".tar", ".gz", ".tgz", ".rar", ".7z"}

// archiveExt returns the task's archive extension lower-cased, or "" when
// the payload is not one of the six supported formats.
func archiveExt(name string) string {
	lower := strings.ToLower(name)
	for _, ext := range archiveExtensions {
		if strings.HasSuffix(lower, ext) {
			return ext
		}
	}

	return ""
}

// gzipWrapped reports whether the archive's outer container is gzip — the
// one format where 7-Zip decompresses exactly one layer per pass, so a
// .tgz yields its inner .tar first and needs a second pass.
func gzipWrapped(name string) bool {
	ext := archiveExt(name)
	return ext == ".gz" || ext == ".tgz"
}

// Chain is the post-processing pipeline. It is enqueued once per task that
// reaches completed and is idempotent on (kind, task_id) as required by
// ADR-0015: a second call finds the extract job row and does not re-create
// it. The store fires OnCompleted on every transition into completed — the
// first call dispatches the extract job, and the extract handler's return
// leg to completed re-enters OnCompleted to run the steps after it.
type Chain struct {
	db        *sqlx.DB
	tasks     *store.TaskStore
	notify    *Notifier
	configDir string
}

// NewChain returns the post-processing chain over the shared database.
func NewChain(db *sqlx.DB, tasks *store.TaskStore) *Chain {
	return &Chain{db: db, tasks: tasks}
}

// SetNotifier attaches the T077 notifier the tail step fans out through.
// nil leaves the step a no-op — the tests that build a chain without one
// exercise the earlier legs alone.
func (c *Chain) SetNotifier(n *Notifier) { c.notify = n }

// SetConfigDir hands the chain the directory the T078 completion hook is
// discovered in. The chain keeps the directory, never a one-time verdict:
// the three-state switch is re-evaluated per finished task, so installing
// or fixing the hook mid-run takes effect on the next completion. An
// empty directory leaves the step off.
func (c *Chain) SetConfigDir(dir string) { c.configDir = dir }

// OnCompleted runs the post-processing chain for one task. Auto-extract is
// skipped when the settings key auto_extract is false, which is its
// default. The steps run in order — extract (T074), then move (T076) and
// notify (T077) land between the extraction check and the auto-remove tail
// — and each dispatched job re-enters this function when its task returns
// to completed, so the chain has exactly one call site.
func (c *Chain) OnCompleted(ctx context.Context, taskID string) error {
	task, err := c.tasks.Get(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		// The row vanished between the transition and the enqueue — the
		// removal already did everything post-processing would.
		return nil
	}
	if err != nil {
		return fmt.Errorf("jobs: postprocess task %q: %w", taskID, err)
	}

	if task.ContentPath != nil && archiveExt(*task.ContentPath) != "" {
		extract, err := c.boolSetting(ctx, settingAutoExtract)
		if err != nil {
			return err
		}
		if extract {
			settled, err := c.extractSettled(ctx, taskID)
			if err != nil {
				return err
			}
			if !settled {
				// No job row yet, or one is still pending: dispatch (once —
				// the row itself is the idempotence key) and stop here. The
				// handler's return leg to completed re-enters OnCompleted.
				return c.enqueueExtract(ctx, taskID)
			}
			// done, or running on the handler's own success leg: fall
			// through to the steps after extract.
		}
	}

	// The move step (T076): the payload belongs inside the resolved
	// destination, and the directory holding it — the engine's save
	// directory — differing from tasks.destination is the signal that a
	// relocation is owed. Same-dir means the engine already delivered the
	// payload where the task asked, and nothing is enqueued.
	if task.ContentPath != nil && *task.ContentPath != "" && task.Destination != "" &&
		filepath.Dir(*task.ContentPath) != filepath.Clean(task.Destination) {
		settled, err := c.moveSettled(ctx, taskID)
		if err != nil {
			return err
		}
		if !settled {
			src := *task.ContentPath
			return c.enqueueMove(ctx, taskID, src, filepath.Join(task.Destination, filepath.Base(src)))
		}
		// running on the handler's own success leg: fall through to the
		// steps after move.
	}

	// The notify step (T077): the chain's terminal event — the
	// task.completed this pass ran for — fans out to every enabled
	// channel whose mask selects it. The event's At is the task's
	// completed_at when the row carries it, so a re-entered pass rebuilds
	// the identical payload and the enqueue dedupe holds. The step must
	// run before the auto-remove tail: that tail deletes the task row.
	if c.notify != nil {
		at := time.Now()
		if task.CompletedAt != nil {
			at = time.UnixMilli(*task.CompletedAt)
		}
		if err := c.notify.Fanout(ctx, Event{
			Code:    store.CodeTaskCompleted,
			TaskID:  task.ID,
			Name:    task.Name,
			State:   task.State,
			Message: "download finished",
			At:      at,
		}); err != nil {
			return fmt.Errorf("jobs: postprocess task %q: notify fanout: %w", taskID, err)
		}
	}

	// The completion hook (T078) is the chain's last observing step: it
	// runs after notify, and its events must land before the auto-remove
	// tail deletes the row they attach to. Its verdict never changes the
	// task's state.
	if err := c.runHook(ctx, task); err != nil {
		return err
	}

	return c.maybeAutoRemove(ctx, taskID)
}

// enqueueExtract records the extract job unless a row already exists —
// the (kind, task_id) pair is dispatched at most once per ADR-0015. Both
// writes are single statements so a pair of overlapping OnCompleted
// calls cannot both observe "no row" and insert twice: SQLite serializes
// writers, and the second caller's UPDATE guard or NOT EXISTS sees the
// first row. A failed row is reset rather than duplicated: the task
// reached completed again (an operator retry after the verdict was
// recorded), so the chain owes the new completion a fresh run.
func (c *Chain) enqueueExtract(ctx context.Context, taskID string) error {
	now := time.Now().UnixMilli()

	if _, err := c.db.ExecContext(
		ctx,
		`UPDATE jobs SET state = 'pending', attempts = 0, locked_at = NULL, last_error = NULL,
			run_after = ?, updated_at = ? WHERE kind = ? AND task_id = ? AND state = 'failed'`,
		now, now, JobKindExtract, taskID,
	); err != nil {
		return fmt.Errorf("jobs: postprocess task %q: reset extract job: %w", taskID, err)
	}

	payload, err := json.Marshal(map[string]string{"task_id": taskID})
	if err != nil {
		return fmt.Errorf("jobs: postprocess task %q: %w", taskID, err)
	}
	if _, err := c.db.ExecContext(
		ctx,
		`INSERT INTO jobs (id, kind, task_id, payload_json, run_after, created_at, updated_at)
			SELECT ?, ?, ?, ?, ?, ?, ?
			WHERE NOT EXISTS (SELECT 1 FROM jobs WHERE kind = ? AND task_id = ?)`,
		store.NewID(store.PrefixJob), JobKindExtract, taskID, string(payload), now, now, now,
		JobKindExtract, taskID,
	); err != nil {
		return fmt.Errorf("jobs: postprocess task %q: %w", taskID, err)
	}

	return nil
}

// extractSettled reports whether the chain may advance past the extract
// step. A pending job still owes the task its run; a failed one already
// left the task in error, so neither lets the tail run. A running row on
// a completed task is the handler's own success leg — the worker marks
// the row done only after Handle returns, which is after this re-entry —
// and done is the plain finished case.
func (c *Chain) extractSettled(ctx context.Context, taskID string) (bool, error) {
	_, state, exists, err := c.extractJobState(ctx, taskID)
	if err != nil {
		return false, err
	}

	return exists && (state == "running" || state == "done"), nil
}

func (c *Chain) extractJobState(ctx context.Context, taskID string) (string, string, bool, error) {
	var row struct {
		ID    string `db:"id"`
		State string `db:"state"`
	}
	err := c.db.GetContext(
		ctx, &row,
		`SELECT id, state FROM jobs WHERE kind = ? AND task_id = ? ORDER BY created_at DESC LIMIT 1`,
		JobKindExtract, taskID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("jobs: postprocess task %q: read extract job: %w", taskID, err)
	}

	return row.ID, row.State, true, nil
}

// moveSettled reports whether the chain may advance past the move step.
// Only a running row settles the step — it is the handler's own success leg
// re-entering through the completed transition, and the worker marks it done
// after Handle returns. A pending row still owes the task its run — the
// chain stops on it like the extract step does, because letting the tail
// run would let auto-remove cascade-delete the job before the move ever
// ran. And done or failed rows are stale verdicts, not finished work: this
// pass is reachable only while the payload still sits outside the
// destination — the same-dir guard above sees to that — the disk-full park
// retried after the task's next completed transition, or a destination
// change under a finished move. enqueueMove re-arms those rows with a fresh
// payload instead of letting the tail skip a move that is still owed.
func (c *Chain) moveSettled(ctx context.Context, taskID string) (bool, error) {
	var state string
	err := c.db.GetContext(
		ctx, &state,
		`SELECT state FROM jobs WHERE kind = ? AND task_id = ? ORDER BY created_at DESC LIMIT 1`,
		JobKindMove, taskID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("jobs: postprocess task %q: read move job: %w", taskID, err)
	}

	return state == "running", nil
}

// enqueueMove records the move job with its durable src/dst payload. A done
// or failed row is reset in place — the only path here is one where the
// payload still owes relocation, so the row's earlier verdict is stale —
// and a pending row carrying a stale payload (a destination change after
// enqueue) is rewritten: pending is provably unclaimed because the worker's
// claim flips state to running atomically. A running row keeps the
// dispatch-at-most-once rule the extract step follows.
func (c *Chain) enqueueMove(ctx context.Context, taskID, src, dst string) error {
	now := time.Now().UnixMilli()

	payload, err := json.Marshal(movePayload{TaskID: taskID, Src: src, Dst: dst})
	if err != nil {
		return fmt.Errorf("jobs: postprocess task %q: %w", taskID, err)
	}

	// Reset and create are one transaction: the at-most-one-row-per-task
	// invariant moveSettled's LIMIT-1 read assumes is then enforced by the
	// write itself, not by SQLite's writer serialization happening to
	// interleave two overlapping chain passes benignly.
	tx, err := c.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("jobs: postprocess task %q: begin move job write: %w", taskID, err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "jobs: rollback of move job write failed", "task_id", taskID, "error", err)
		}
	}()

	if _, err := tx.ExecContext(
		ctx,
		`UPDATE jobs SET state = 'pending', attempts = 0, locked_at = NULL, last_error = NULL,
			payload_json = ?, run_after = ?, updated_at = ?
			WHERE kind = ? AND task_id = ? AND (state IN ('done', 'failed')
				OR (state = 'pending' AND payload_json != ?))`,
		string(payload), now, now, JobKindMove, taskID, string(payload),
	); err != nil {
		return fmt.Errorf("jobs: postprocess task %q: reset move job: %w", taskID, err)
	}

	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO jobs (id, kind, task_id, payload_json, run_after, created_at, updated_at)
			SELECT ?, ?, ?, ?, ?, ?, ?
			WHERE NOT EXISTS (SELECT 1 FROM jobs WHERE kind = ? AND task_id = ?)`,
		store.NewID(store.PrefixJob), JobKindMove, taskID, string(payload), now, now, now,
		JobKindMove, taskID,
	); err != nil {
		return fmt.Errorf("jobs: postprocess task %q: create move job: %w", taskID, err)
	}

	return tx.Commit()
}

// maybeAutoRemove is the chain's tail (FR-106): with
// auto_remove_on_complete the task row goes away once post-processing is
// done — the downloaded data stays on disk, and the row's final event is
// postprocess.autoremoved. Event and delete share one transaction so a
// crash cannot leave a live task whose log ends claiming it was removed.
func (c *Chain) maybeAutoRemove(ctx context.Context, taskID string) error {
	remove, err := c.boolSetting(ctx, settingAutoRemoveOnComplete)
	if err != nil || !remove {
		return err
	}

	tx, err := c.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("jobs: postprocess task %q: %w", taskID, err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "jobs: rollback of auto-remove failed", "task_id", taskID, "error", err)
		}
	}()

	var exists int
	if err := tx.GetContext(ctx, &exists, `SELECT COUNT(*) FROM tasks WHERE id = ?`, taskID); err != nil {
		return fmt.Errorf("jobs: postprocess task %q: %w", taskID, err)
	}
	if exists == 0 {
		// The row vanished between the chain's read and now — the
		// removal already did everything post-processing would.
		return nil
	}

	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO task_events (id, task_id, at, level, code, message, detail_json, created_at, updated_at)
			VALUES (?, ?, ?, 'info', ?, ?, NULL, ?, ?)`,
		store.NewID(store.PrefixTaskEvent), taskID, now, eventAutoRemoved,
		"removed automatically after completion", now, now,
	); err != nil {
		return fmt.Errorf("jobs: postprocess task %q: %w", taskID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, taskID); err != nil {
		return fmt.Errorf("jobs: postprocess task %q: %w", taskID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("jobs: postprocess task %q: %w", taskID, err)
	}

	return nil
}

// boolSetting reads one boolean key of the settings table; a missing row
// or a non-true value yields the documented default false.
func (c *Chain) boolSetting(ctx context.Context, key string) (bool, error) {
	var raw string
	err := c.db.GetContext(ctx, &raw, `SELECT value_json FROM settings WHERE key = ?`, key)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("jobs: read settings key %s: %w", key, err)
	}

	var value bool
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		// A malformed row falls back to the documented default rather
		// than wedging every completing task's post-processing.
		slog.Warn("jobs: settings key holds malformed JSON, using default", "key", key, "err", err)

		return false, nil
	}

	return value, nil
}

// ExtractHandler runs job kind "extract". Registered with the T012 worker
// pool as worker.Register(JobKindExtract, h.Handle).
type ExtractHandler struct {
	tasks        *store.TaskStore
	sevenzipPath string
	caps         Caps
}

// NewExtractHandler returns the extract handler; a zero caps uses the
// documented defaults of doc 12 section 4.1.
func NewExtractHandler(tasks *store.TaskStore, sevenzipPath string) *ExtractHandler {
	return &ExtractHandler{tasks: tasks, sevenzipPath: sevenzipPath}
}

// payloadStem names the directory the verified tree is renamed to: the
// archive's base name minus its extension, beside the archive itself.
// Only the archiveExtensions spellings ever reach it — .tar.gz counts as
// one extension so its stem drops both parts.
func payloadStem(archivePath string) string {
	base := filepath.Base(archivePath)
	if lower := strings.ToLower(base); strings.HasSuffix(lower, ".tar.gz") {
		return base[:len(base)-len(".tar.gz")]
	}

	return strings.TrimSuffix(base, filepath.Ext(base))
}
