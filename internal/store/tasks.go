package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jmoiron/sqlx"
)

// ErrIllegalTransition is returned when a state change is not in the
// transition table. ErrNotFound is deliberately not redeclared here: db.go
// owns it for the whole package.
var ErrIllegalTransition = errors.New("store: illegal state transition")

// ErrTransitionConflict is returned when the state a transition validated
// against changed before its update landed. Unlike ErrIllegalTransition the
// move may well be legal from the new state: the caller should re-read and
// decide again.
var ErrTransitionConflict = errors.New("store: task state changed since read")

// Progress carries the mutable transfer counters an engine poll produces.
type Progress struct {
	TotalBytes     *int64
	CompletedBytes int64
	UploadedBytes  int64
	DownloadRate   int64
	UploadRate     int64
	ETASeconds     *int64
}

// taskTransitions is the edge table of docs/03-architecture.md section 8.1
// without the universal rules. The first five rows let the reconciler adopt
// whatever the engine normalisation tables of docs/06-download-engines.md
// sections 4.6 and 5.6 report; extracting and moving are entered only by the
// post-processing chain, never by an engine. removed has no row: it is the
// only terminal state.
var taskTransitions = map[string][]string{
	"queued":      {"downloading", "checking", "seeding", "completed"},
	"downloading": {"queued", "checking", "seeding", "completed"},
	"checking":    {"queued", "downloading", "seeding", "completed"},
	"paused":      {"queued", "downloading", "checking", "seeding", "completed"},
	"seeding":     {"queued", "downloading", "checking", "completed", "extracting", "moving"},
	"completed":   {"checking", "seeding", "extracting", "moving"},
	"extracting":  {"moving", "completed"},
	"moving":      {"completed", "seeding"},
	"error":       {"queued", "completed"},
}

// universalTransitionTargets are reachable from every non-terminal state:
// any task may be paused, fail or be deleted at any time.
var universalTransitionTargets = []string{"paused", "error", "removed"}

// transitionLegal reports whether from -> next is a legal edge: a table row
// or a universal rule, and never a self-loop or an exit from removed.
func transitionLegal(from, next string) bool {
	if from == "removed" || from == next {
		return false
	}
	if slices.Contains(universalTransitionTargets, next) {
		return true
	}

	return slices.Contains(taskTransitions[from], next)
}

const (
	queryCreateTask = `INSERT INTO tasks
(id, engine, engine_ref, source_kind, source_uri, name, infohash_v1, infohash_v2, state,
 error_code, error_message, destination, content_path, category_id, total_bytes, completed_bytes,
 uploaded_bytes, download_rate, upload_rate, eta_seconds, sequential, queue_position,
 added_at, started_at, completed_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	queryGetTask = `SELECT id, engine, engine_ref, source_kind, source_uri, name, infohash_v1, infohash_v2,
 state, error_code, error_message, destination, content_path, category_id, total_bytes, completed_bytes,
 uploaded_bytes, download_rate, upload_rate, eta_seconds, sequential, queue_position,
 added_at, started_at, completed_at, created_at, updated_at
FROM tasks
WHERE id = ?`

	queryUpdateTaskProgress = `UPDATE tasks
SET total_bytes = ?, completed_bytes = ?, uploaded_bytes = ?,
    download_rate = ?, upload_rate = ?, eta_seconds = ?, updated_at = ?
WHERE id = ?`

	querySetTaskEngineRef = `UPDATE tasks
SET engine_ref = ?, updated_at = ?
WHERE id = ?`

	queryTaskState = `SELECT state FROM tasks WHERE id = ?`

	// The state guard makes the update a compare-and-swap: a concurrent
	// transition that committed after the read above turns this into a
	// no-op instead of a lost update.
	queryTransitionTask = `UPDATE tasks
SET state = ?, updated_at = ?
WHERE id = ? AND state = ?`
)

// TaskStore persists tasks rows and enforces the task state machine.
type TaskStore struct{ db *sqlx.DB }

// NewTaskStore returns a TaskStore over db.
func NewTaskStore(db *sqlx.DB) *TaskStore {
	return &TaskStore{db: db}
}

// Create inserts one task, generating the id when empty and stamping
// added_at, created_at and updated_at with the same Unix millisecond. It
// returns the row as stored.
func (s *TaskStore) Create(ctx context.Context, t Task) (Task, error) {
	if t.ID == "" {
		t.ID = NewID(PrefixTask)
	}

	now := time.Now().UnixMilli()
	t.AddedAt = now
	t.CreatedAt = now
	t.UpdatedAt = now

	_, err := s.db.ExecContext(
		ctx,
		queryCreateTask,
		t.ID, t.Engine, t.EngineRef, t.SourceKind, t.SourceURI, t.Name,
		t.InfohashV1, t.InfohashV2, t.State, t.ErrorCode, t.ErrorMessage,
		t.Destination, t.ContentPath, t.CategoryID, t.TotalBytes, t.CompletedBytes,
		t.UploadedBytes, t.DownloadRate, t.UploadRate, t.ETASeconds,
		t.Sequential, t.QueuePosition, t.AddedAt, t.StartedAt, t.CompletedAt,
		t.CreatedAt, t.UpdatedAt,
	)
	if err != nil {
		return Task{}, fmt.Errorf("store: create task %q: %w", t.Name, err)
	}

	return t, nil
}

// Get returns the task with the given id, or ErrNotFound.
func (s *TaskStore) Get(ctx context.Context, id string) (Task, error) {
	var task Task
	err := s.db.GetContext(ctx, &task, queryGetTask, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("store: task %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Task{}, fmt.Errorf("store: task %q: %w", id, err)
	}

	return task, nil
}

// UpdateProgress replaces the transfer counters of a task and bumps
// updated_at. A missing id is ErrNotFound: the poller reads that as the
// signal to stop polling the task.
func (s *TaskStore) UpdateProgress(ctx context.Context, id string, p Progress) error {
	result, err := s.db.ExecContext(
		ctx,
		queryUpdateTaskProgress,
		p.TotalBytes, p.CompletedBytes, p.UploadedBytes,
		p.DownloadRate, p.UploadRate, p.ETASeconds, time.Now().UnixMilli(),
		id,
	)
	if err != nil {
		return fmt.Errorf("store: update progress of task %q: %w", id, err)
	}

	// SQLite's changes() counts matched rows even when the written values
	// are identical, so a no-op progress write still reports 1 and 0 means
	// the task row is genuinely gone.
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update progress of task %q: count rows: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("store: update progress of task %q: %w", id, ErrNotFound)
	}

	return nil
}

// SetEngineRef records the engine-side identity (aria2 GID, qBittorrent
// infohash, yt-dlp job id) once the engine accepted the task, and bumps
// updated_at. A missing id is ErrNotFound.
func (s *TaskStore) SetEngineRef(ctx context.Context, id, engineRef string) error {
	result, err := s.db.ExecContext(ctx, querySetTaskEngineRef, engineRef, time.Now().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("store: set engine ref of task %q: %w", id, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set engine ref of task %q: count rows: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("store: set engine ref of task %q: %w", id, ErrNotFound)
	}

	return nil
}

// Transition moves a task to next, writing one task_events row with code and
// message in the same transaction. It returns ErrIllegalTransition and
// mutates nothing when the move is not permitted, and ErrNotFound when the
// task does not exist.
func (s *TaskStore) Transition(ctx context.Context, id, next, code, message string) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin transition of task %q: %w", id, err)
	}
	// Rolls back on any early return; after Commit this is sql.ErrTxDone,
	// which is the expected outcome and not worth a warning.
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "store: rollback of task transition failed", "task_id", id, "error", err)
		}
	}()

	var current string
	err = tx.GetContext(ctx, &current, queryTaskState, id)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: transition task %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("store: transition task %q: read state: %w", id, err)
	}

	if !transitionLegal(current, next) {
		return fmt.Errorf(
			"store: transition task %q from %q to %q: %w",
			id, current, next, ErrIllegalTransition,
		)
	}

	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, queryTransitionTask, next, now, id, current)
	if err != nil {
		return fmt.Errorf("store: transition task %q to %q: %w", id, next, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: transition task %q to %q: count rows: %w", id, next, err)
	}
	if affected == 0 {
		// The state validated above changed underneath this transaction;
		// the move must be re-evaluated against the new state.
		return errTransitionConflict(id, current, next)
	}

	if err := insertTaskEvent(ctx, tx, id, transitionEventLevel(next), code, message, nil, now); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit transition of task %q: %w", id, err)
	}

	return nil
}

// errTransitionConflict reports a compare-and-swap miss: expected is the
// stale state the legality check read, not the state the task is in now.
func errTransitionConflict(id, expected, next string) error {
	return fmt.Errorf(
		"store: transition task %q: expected state %q, attempted move to %q: %w",
		id, expected, next, ErrTransitionConflict,
	)
}

// transitionEventLevel records a move into the error state as an error
// event; every other move is a normal operator-visible fact.
func transitionEventLevel(next string) string {
	if next == "error" {
		return "error"
	}

	return "info"
}
