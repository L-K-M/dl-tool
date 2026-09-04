package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
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

// taskStates is the ten-state vocabulary of docs/04-data-model.md section
// 4.1. The DDL CHECK constraint enforces the same set; Create consults it
// to answer a typo with a legible error instead of a constraint dump.
var taskStates = []string{
	"queued", "downloading", "checking", "paused", "seeding",
	"completed", "extracting", "moving", "error", "removed",
}

// universalTransitionTargets are reachable from every non-terminal state:
// any task may be paused, fail or be deleted at any time.
var universalTransitionTargets = []string{"paused", "error", "removed"}

// TaskPatch carries the PATCH-able columns of docs/05-api-contract.md
// section 5.5. A nil field leaves its column untouched. Tags are absent
// because they live in task_tags, and destination is absent because the
// cross-filesystem move is owned by T076.
type TaskPatch struct {
	Name             *string
	CategoryID       *string
	DLLimit          *int64
	ULLimit          *int64
	RatioLimit       *float64
	SeedingTimeLimit *int64
	Sequential       *bool
}

// Empty reports whether the patch carries no column at all — a tags-only
// PATCH, whose set lives in task_tags and never reaches Update.
func (p TaskPatch) Empty() bool {
	return p.Name == nil && p.CategoryID == nil && p.DLLimit == nil &&
		p.ULLimit == nil && p.RatioLimit == nil && p.SeedingTimeLimit == nil &&
		p.Sequential == nil
}

// QueueMove names one of the four queue rewrites of docs/05-api-contract.md
// section 5.7.
type QueueMove int

const (
	QueueMoveTop QueueMove = iota
	QueueMoveUp
	QueueMoveDown
	QueueMoveBottom
)

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

	// The tombstone of docs/05-api-contract.md 5.6 step 6: state plus the
	// cleared liveness columns, guarded by the same compare-and-swap.
	// queue_position is cleared too so the task leaves dl-tool's queue —
	// queue membership is exactly a non-NULL queue_position (doc 05
	// section 3: "null for tasks not in a queue"). Positions are
	// ordering-only, so the gap a mid-queue removal leaves is harmless:
	// ReorderQueue renumbers the whole queue densely from 1 on the next
	// queue action, and the admission pass reads order, never density.
	queryMarkTaskRemoved = `UPDATE tasks
SET state = 'removed', engine_ref = NULL, download_rate = 0, upload_rate = 0,
    eta_seconds = NULL, queue_position = NULL, updated_at = ?
WHERE id = ? AND state = ?`

	queryListTaskFiles = `SELECT id, task_id, file_index, path, size_bytes, completed_bytes, selected, priority, created_at, updated_at
FROM task_files
WHERE task_id = ?
ORDER BY file_index`

	queryQueueMembers = `SELECT id FROM tasks
WHERE queue_position IS NOT NULL
ORDER BY queue_position, id`

	querySetQueuePosition = `UPDATE tasks
SET queue_position = ?, updated_at = ?
WHERE id = ?`
)

// TaskStore persists tasks rows and enforces the task state machine.
type TaskStore struct{ db *sqlx.DB }

// NewTaskStore returns a TaskStore over db.
func NewTaskStore(db *sqlx.DB) *TaskStore {
	return &TaskStore{db: db}
}

// Create inserts one task, generating the id when empty, defaulting an
// empty state to the state machine's entry state queued
// (docs/03-architecture.md section 8.1) and stamping added_at, created_at
// and updated_at with the same Unix millisecond. It returns the row as
// stored. It writes no event: fixtures and internal seeds use it, and the
// create path uses CreateLogged so a task row never exists without its
// task.created row (FR-150).
func (s *TaskStore) Create(ctx context.Context, t Task) (Task, error) {
	if err := prepareNewTask(&t); err != nil {
		return Task{}, err
	}

	if err := insertTaskRow(ctx, s.db, t); err != nil {
		return Task{}, fmt.Errorf("store: create task %q: %w", t.Name, err)
	}

	return t, nil
}

// CreateLogged inserts one task and its task.created event in a single
// transaction, so a task row can never persist without the first entry of
// its event log. The row semantics are Create's.
func (s *TaskStore) CreateLogged(ctx context.Context, t Task) (Task, error) {
	if err := prepareNewTask(&t); err != nil {
		return Task{}, err
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return Task{}, fmt.Errorf("store: create task %q: %w", t.Name, err)
	}
	// Rolls back on any early return; after Commit this is sql.ErrTxDone,
	// which is the expected outcome and not worth a warning.
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "store: rollback of task creation failed", "task_id", t.ID, "error", err)
		}
	}()

	if err := insertTaskRow(ctx, tx, t); err != nil {
		return Task{}, fmt.Errorf("store: create task %q: %w", t.Name, err)
	}

	if err := insertTaskEvent(ctx, tx, t.ID, "info", CodeTaskCreated, messageTaskCreated, nil, t.CreatedAt); err != nil {
		return Task{}, fmt.Errorf("store: create task %q: %w", t.Name, err)
	}

	if err := tx.Commit(); err != nil {
		return Task{}, fmt.Errorf("store: create task %q: commit: %w", t.Name, err)
	}

	return t, nil
}

// insertTaskRow writes one tasks row through the executor both creation
// paths share, so the column list and its argument list are maintained in
// exactly one place and cannot drift between them. ext is *sqlx.DB or a
// *sqlx.Tx, the same contract insertTaskEvent has.
func insertTaskRow(ctx context.Context, ext sqlx.ExtContext, t Task) error {
	_, err := ext.ExecContext(
		ctx,
		queryCreateTask,
		t.ID, t.Engine, t.EngineRef, t.SourceKind, t.SourceURI, t.Name,
		t.InfohashV1, t.InfohashV2, t.State, t.ErrorCode, t.ErrorMessage,
		t.Destination, t.ContentPath, t.CategoryID, t.TotalBytes, t.CompletedBytes,
		t.UploadedBytes, t.DownloadRate, t.UploadRate, t.ETASeconds,
		t.Sequential, t.QueuePosition, t.AddedAt, t.StartedAt, t.CompletedAt,
		t.CreatedAt, t.UpdatedAt,
	)

	return err
}

// prepareNewTask fills a task row's generated defaults: the id when empty,
// the state machine's entry state when empty (validated against the
// vocabulary otherwise) and the added/created/updated stamps.
func prepareNewTask(t *Task) error {
	if t.ID == "" {
		t.ID = NewID(PrefixTask)
	}
	if t.State == "" {
		t.State = "queued"
	} else if !slices.Contains(taskStates, t.State) {
		return fmt.Errorf("store: create task %q: unknown state %q, want one of %q", t.Name, t.State, taskStates)
	}

	now := time.Now().UnixMilli()
	t.AddedAt = now
	t.CreatedAt = now
	t.UpdatedAt = now

	return nil
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
// infohash, yt-dlp job id) once the engine accepted the task, writing the
// engine.accepted event in the same transaction, and bumps updated_at. A
// missing id is ErrNotFound.
func (s *TaskStore) SetEngineRef(ctx context.Context, id, engineRef string) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: set engine ref of task %q: %w", id, err)
	}
	// Rolls back on any early return; after Commit this is sql.ErrTxDone,
	// which is the expected outcome and not worth a warning.
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "store: rollback of engine ref failed", "task_id", id, "error", err)
		}
	}()

	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, querySetTaskEngineRef, engineRef, now, id)
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

	// The handle landing is the moment the engine took ownership of the
	// transfer (docs/14-conventions.md section 4); the event rides the same
	// transaction so a task row and its acceptance never disagree.
	if err := insertTaskEvent(ctx, tx, id, "info", CodeEngineAccepted, "engine accepted the task", nil, now); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: set engine ref of task %q: commit: %w", id, err)
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

// The task_events messages written beside the codes the events package
// owns (CodeTaskCreated, CodeTaskRemoved, CodeTaskDataDeleted).
const (
	messageTaskCreated     = "created by user request"
	messageTaskRemoved     = "removed by user request"
	messageTaskDataDeleted = "downloaded data deleted by user request"
)

// DeletedData carries what a delete_data removal unlinked, recorded as the
// detail of the task.data_deleted event (docs/05-api-contract.md 5.6
// step 5: the file count and the byte total).
type DeletedData struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// TaskFile is one row of the task_files table (docs/04-data-model.md
// section 3.3). Path is relative to tasks.destination; joining it onto the
// destination belongs to the caller, which owns the path-safety check.
type TaskFile struct {
	ID             string `db:"id"`
	TaskID         string `db:"task_id"`
	FileIndex      int    `db:"file_index"`
	Path           string `db:"path"`
	SizeBytes      int64  `db:"size_bytes"`
	CompletedBytes int64  `db:"completed_bytes"`
	Selected       int    `db:"selected"`
	Priority       *int   `db:"priority"`
	CreatedAt      int64  `db:"created_at"`
	UpdatedAt      int64  `db:"updated_at"`
}

// ListFiles returns the task's recorded files in file_index order, the
// enumerated targets of the delete path (docs/05-api-contract.md 5.6
// step 1). It reads task_files alone — never the filesystem.
func (s *TaskStore) ListFiles(ctx context.Context, taskID string) ([]TaskFile, error) {
	var files []TaskFile
	if err := s.db.SelectContext(ctx, &files, queryListTaskFiles, taskID); err != nil {
		return nil, fmt.Errorf("store: list files of task %q: %w", taskID, err)
	}

	return files, nil
}

// MarkRemoved tombstones a task, running steps 5 and 6 of the delete path
// (docs/05-api-contract.md 5.6) in one transaction: the task.removed event,
// plus the warn-level task.data_deleted event carrying data when the
// request unlinked files, then state="removed", engine_ref=NULL, both
// rates zeroed and eta_seconds and queue_position cleared, so the task
// leaves dl-tool's queue. It never deletes a row: the task and every
// child row stay so the event and file history remain queryable.
func (s *TaskStore) MarkRemoved(ctx context.Context, id string, data *DeletedData) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin removal of task %q: %w", id, err)
	}
	// Rolls back on any early return; after Commit this is sql.ErrTxDone,
	// which is the expected outcome and not worth a warning.
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "store: rollback of task removal failed", "task_id", id, "error", err)
		}
	}()

	var current string
	err = tx.GetContext(ctx, &current, queryTaskState, id)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: remove task %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("store: remove task %q: read state: %w", id, err)
	}

	if !transitionLegal(current, "removed") {
		return fmt.Errorf("store: remove task %q from %q: %w", id, current, ErrIllegalTransition)
	}

	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, queryMarkTaskRemoved, now, id, current)
	if err != nil {
		return fmt.Errorf("store: remove task %q: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: remove task %q: count rows: %w", id, err)
	}
	if affected == 0 {
		return errTransitionConflict(id, current, "removed")
	}

	if err := insertTaskEvent(ctx, tx, id, "info", CodeTaskRemoved, messageTaskRemoved, nil, now); err != nil {
		return err
	}
	if data != nil {
		if err := insertTaskEvent(ctx, tx, id, "warn", CodeTaskDataDeleted, messageTaskDataDeleted, data, now); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit removal of task %q: %w", id, err)
	}

	return nil
}

// transitionEventLevel records a move into the error state as an error
// event; every other move is a normal operator-visible fact.
func transitionEventLevel(next string) string {
	if next == "error" {
		return "error"
	}

	return "info"
}

// Update writes the columns a patch carries and bumps updated_at, in one
// UPDATE whose column list is built from the patch's non-nil fields alone,
// so an omitted field is never written (docs/05-api-contract.md 5.5). A
// missing id is ErrNotFound; an empty patch is a caller bug and errors
// rather than bumping updated_at for nothing.
func (s *TaskStore) Update(ctx context.Context, id string, patch TaskPatch) error {
	sets := make([]string, 0, 8)
	args := make([]any, 0, 9)

	if patch.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *patch.Name)
	}
	if patch.CategoryID != nil {
		sets = append(sets, "category_id = ?")
		args = append(args, *patch.CategoryID)
	}
	if patch.DLLimit != nil {
		sets = append(sets, "dl_limit = ?")
		args = append(args, *patch.DLLimit)
	}
	if patch.ULLimit != nil {
		sets = append(sets, "ul_limit = ?")
		args = append(args, *patch.ULLimit)
	}
	if patch.RatioLimit != nil {
		sets = append(sets, "ratio_limit = ?")
		args = append(args, *patch.RatioLimit)
	}
	if patch.SeedingTimeLimit != nil {
		sets = append(sets, "seeding_time_limit = ?")
		args = append(args, *patch.SeedingTimeLimit)
	}
	if patch.Sequential != nil {
		sets = append(sets, "sequential = ?")
		args = append(args, boolToInt(*patch.Sequential))
	}

	if len(sets) == 0 {
		return fmt.Errorf("store: update task %q: empty patch", id)
	}

	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().UnixMilli(), id)

	result, err := s.db.ExecContext(ctx, "UPDATE tasks SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return fmt.Errorf("store: update task %q: %w", id, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update task %q: count rows: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("store: update task %q: %w", id, ErrNotFound)
	}

	return nil
}

// boolToInt maps a boolean onto the 0/1 integer columns.
func boolToInt(b bool) int {
	if b {
		return 1
	}

	return 0
}

// ReorderQueue moves ids inside the queue — every task whose queue_position
// is not NULL — and renumbers the whole queue densely from 1 in one
// transaction. No engine is contacted: dl-tool owns the queue
// (docs/05-api-contract.md section 5.7). It returns the requested ids that
// are not part of the queue, in request order, so the caller can answer
// them per-id.
func (s *TaskStore) ReorderQueue(ctx context.Context, ids []string, move QueueMove) ([]string, error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: reorder queue: %w", err)
	}
	// Rolls back on any early return; after Commit this is sql.ErrTxDone,
	// which is the expected outcome and not worth a warning.
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "store: rollback of queue reorder failed", "error", err)
		}
	}()

	var order []string
	if err := tx.SelectContext(ctx, &order, queryQueueMembers); err != nil {
		return nil, fmt.Errorf("store: reorder queue: read members: %w", err)
	}

	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		selected[id] = true
	}

	now := time.Now().UnixMilli()
	for position, id := range reorderQueueMembers(order, selected, move) {
		if _, err := tx.ExecContext(ctx, querySetQueuePosition, position+1, now, id); err != nil {
			return nil, fmt.Errorf("store: reorder queue: write position of task %q: %w", id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: reorder queue: commit: %w", err)
	}

	inQueue := make(map[string]bool, len(order))
	for _, id := range order {
		inQueue[id] = true
	}

	absent := make([]string, 0, len(ids))
	for _, id := range ids {
		if !inQueue[id] {
			absent = append(absent, id)
		}
	}

	return absent, nil
}

// reorderQueueMembers computes the queue's new order. The selected members
// keep their relative order however far they move, and so do the
// unselected ones: a move rewrites the boundary between the two groups,
// never the order inside either.
func reorderQueueMembers(order []string, selected map[string]bool, move QueueMove) []string {
	next := slices.Clone(order)

	switch move {
	case QueueMoveTop:
		return stablePartition(next, func(id string) bool { return selected[id] })
	case QueueMoveBottom:
		return stablePartition(next, func(id string) bool { return !selected[id] })
	case QueueMoveUp:
		// Front to back: every selected task swaps with an unselected
		// predecessor, so a contiguous selected run advances one slot as a
		// block instead of jumping over itself.
		for i := 1; i < len(next); i++ {
			if selected[next[i]] && !selected[next[i-1]] {
				next[i-1], next[i] = next[i], next[i-1]
			}
		}
	case QueueMoveDown:
		// Back to front, the mirror of QueueMoveUp.
		for i := len(next) - 2; i >= 0; i-- {
			if selected[next[i]] && !selected[next[i+1]] {
				next[i], next[i+1] = next[i+1], next[i]
			}
		}
	}

	return next
}

// stablePartition returns the members for which keep holds followed by the
// rest, each group in its original order.
func stablePartition(order []string, keep func(string) bool) []string {
	head := make([]string, 0, len(order))
	tail := make([]string, 0, len(order))
	for _, id := range order {
		if keep(id) {
			head = append(head, id)
		} else {
			tail = append(tail, id)
		}
	}

	return append(head, tail...)
}
