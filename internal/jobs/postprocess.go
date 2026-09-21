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
	db    *sqlx.DB
	tasks *store.TaskStore
}

// NewChain returns the post-processing chain over the shared database.
func NewChain(db *sqlx.DB, tasks *store.TaskStore) *Chain {
	return &Chain{db: db, tasks: tasks}
}

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

	return c.maybeAutoRemove(ctx, taskID)
}

// enqueueExtract records the extract job unless a live row already
// exists — the (kind, task_id) pair is dispatched at most once per
// ADR-0015. A failed row is reset rather than duplicated: the task
// reached completed again (an operator retry after the verdict was
// recorded), so the chain owes the new completion a fresh run.
func (c *Chain) enqueueExtract(ctx context.Context, taskID string) error {
	jobID, state, exists, err := c.extractJobState(ctx, taskID)
	if err != nil {
		return err
	}
	if exists && state != "failed" {
		return nil
	}
	if exists {
		_, err := c.db.ExecContext(
			ctx,
			`UPDATE jobs SET state = 'pending', attempts = 0, locked_at = NULL, last_error = NULL,
				run_after = ?, updated_at = ? WHERE id = ? AND state = 'failed'`,
			time.Now().UnixMilli(), time.Now().UnixMilli(), jobID,
		)
		if err != nil {
			return fmt.Errorf("jobs: postprocess task %q: reset extract job: %w", taskID, err)
		}

		return nil
	}

	_, err = store.EnqueueJob(
		ctx, c.db, JobKindExtract, &taskID,
		map[string]string{"task_id": taskID}, time.Now().UnixMilli(),
	)
	if err != nil {
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

// maybeAutoRemove is the chain's tail (FR-106): with
// auto_remove_on_complete the task row goes away once post-processing is
// done — the downloaded data stays on disk, and the row's final event is
// postprocess.autoremoved.
func (c *Chain) maybeAutoRemove(ctx context.Context, taskID string) error {
	remove, err := c.boolSetting(ctx, settingAutoRemoveOnComplete)
	if err != nil || !remove {
		return err
	}

	if err := c.tasks.AppendEvent(
		ctx, taskID, "info", eventAutoRemoved, "removed automatically after completion", nil,
	); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("jobs: postprocess task %q: %w", taskID, err)
	}

	err = c.tasks.Delete(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
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
func payloadStem(archivePath string) string {
	base := filepath.Base(archivePath)
	lower := strings.ToLower(base)
	for _, ext := range []string{".tar.gz", ".tar.bz2", ".tar.xz"} {
		if strings.HasSuffix(lower, ext) {
			return base[:len(base)-len(ext)]
		}
	}

	return strings.TrimSuffix(base, filepath.Ext(base))
}
