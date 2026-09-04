package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jmoiron/sqlx"
)

// Task-event codes emitted in M1. A code is <area>.<subject>[.<outcome>], lower case, ASCII letters,
// digits and dots only. Never rename or reuse a code: a rename orphans stored history rows.
const (
	// CodeTaskCreated is emitted once when the create endpoint inserts a
	// task row for an accepted URI.
	CodeTaskCreated = "task.created"
	// CodeTaskPaused is emitted when the pause action moves a task into
	// the paused state.
	CodeTaskPaused = "task.paused"
	// CodeTaskResumed is emitted when the resume action requeues a paused
	// or errored task.
	CodeTaskResumed = "task.resumed"
	// CodeTaskRemoved is emitted when the delete endpoint tombstones a
	// task.
	CodeTaskRemoved = "task.removed"
	// CodeTaskCompleted is emitted when a task reaches the completed
	// state on its own — an engine poll reporting the transfer finished.
	// A user-forced completion writes task.force_completed instead.
	CodeTaskCompleted = "task.completed"
	// CodeTaskDataDeleted is emitted when a removal also unlinked the
	// downloaded files.
	CodeTaskDataDeleted = "task.data_deleted"
	// CodeEngineAccepted is emitted when an engine returned a handle and
	// SetEngineRef recorded it — the moment the engine took ownership of
	// the transfer. It is emitted only when the handle changes: SetEngineRef
	// is its single writer, and a no-op re-set re-learns nothing. No caller
	// may also pass it as a Transition code, or one acceptance logs twice.
	CodeEngineAccepted = "engine.accepted"
	// CodeEngineRejected is emitted when an engine refused a task. No M1
	// code path reaches an engine refusal — POST /tasks contacts no engine
	// and the admission pass (T098) owns Engine.Add — so the emission
	// point arrives with that task.
	CodeEngineRejected = "engine.rejected"
	// CodeEngineUnavailable is emitted when a task's engine cannot be
	// reached at hand-off. Like engine.rejected, no M1 code path reaches
	// the moment; the admission pass (T098) wires it.
	CodeEngineUnavailable = "engine.unavailable"
)

// eventLevels is the level vocabulary of the task_events DDL CHECK.
// AppendEvent consults it to answer a typo with a legible error instead of
// a constraint dump.
var eventLevels = []string{"info", "warn", "error"}

const (
	queryInsertTaskEvent = `INSERT INTO task_events
(id, task_id, at, level, code, message, detail_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

	queryCountTaskEvents = `SELECT COUNT(*) FROM task_events WHERE task_id = ?`

	queryListTaskEvents = `SELECT id, task_id, at, level, code, message, detail_json, created_at, updated_at
FROM task_events
WHERE task_id = ?
ORDER BY at DESC, id DESC
LIMIT ?`

	// The cursor predicate is the keyset form of the page's ORDER BY: the
	// (at, id) tuple is total — two events may share a millisecond, and the
	// ULID id breaks the tie in the same order the index scans — so the
	// walk misses nothing and repeats nothing.
	queryListTaskEventsBefore = `SELECT id, task_id, at, level, code, message, detail_json, created_at, updated_at
FROM task_events
WHERE task_id = ? AND (at < ? OR (at = ? AND id < ?))
ORDER BY at DESC, id DESC
LIMIT ?`
)

// AppendEvent inserts one task_events row. level is "info", "warn" or
// "error"; code is a dotted vocabulary key from docs/14-conventions.md
// section 4, never a formatted value. detail is marshalled into
// detail_json; a nil detail stores NULL.
func (s *TaskStore) AppendEvent(ctx context.Context, taskID, level, code, message string, detail any) error {
	if !slices.Contains(eventLevels, level) {
		return fmt.Errorf("store: append event to task %q: unknown level %q, want one of %q", taskID, level, eventLevels)
	}

	return insertTaskEvent(ctx, s.db, taskID, level, code, message, detail, time.Now().UnixMilli())
}

// ListEvents returns one page of events for a task, newest first, with the
// same cursor envelope as every other list endpoint (docs/05-api-contract.md
// section 1.4): limit defaults to 100 and is re-checked against the 1..500
// range, total counts every row of the task ignoring the cursor, and
// nextCursor is empty exactly on the last page. cursor is the opaque token a
// previous page returned; a token that is not one is ErrStaleCursor. There
// is no filter or sort to bind a token to — the endpoint's filter is the
// task id and its sort is fixed — so a well-formed token of another task
// simply continues that task's walk.
func (s *TaskStore) ListEvents(
	ctx context.Context,
	taskID string,
	limit int,
	cursor string,
) ([]TaskEvent, string, int, error) {
	if limit == 0 {
		limit = taskListDefaultLimit
	}
	if limit < 1 || limit > taskListMaxLimit {
		return nil, "", 0, fmt.Errorf("store: list events of task %q: limit %d outside 1..%d", taskID, limit, taskListMaxLimit)
	}

	var page eventPageCursor
	if cursor != "" {
		decoded, err := decodeEventCursor(cursor)
		if err != nil {
			// Wrapped like every other failure of this function, with %w so
			// ErrStaleCursor stays detectable for the 422 mapping.
			return nil, "", 0, fmt.Errorf("store: list events of task %q: %w", taskID, err)
		}
		page = decoded
	}

	var total int
	if err := s.db.GetContext(ctx, &total, queryCountTaskEvents, taskID); err != nil {
		return nil, "", 0, fmt.Errorf("store: list events of task %q: count: %w", taskID, err)
	}

	// One row past the limit decides whether another page exists, so
	// nextCursor is empty exactly on the last page.
	var (
		events []TaskEvent
		err    error
	)
	if cursor == "" {
		err = s.db.SelectContext(ctx, &events, queryListTaskEvents, taskID, limit+1)
	} else {
		err = s.db.SelectContext(
			ctx,
			&events,
			queryListTaskEventsBefore,
			taskID, page.At, page.At, page.ID, limit+1,
		)
	}
	if err != nil {
		return nil, "", 0, fmt.Errorf("store: list events of task %q: read page: %w", taskID, err)
	}

	if len(events) <= limit {
		return events, "", total, nil
	}
	events = events[:limit]

	last := events[len(events)-1]
	nextCursor, err := encodeEventCursor(eventPageCursor{At: last.At, ID: last.ID})
	if err != nil {
		return nil, "", 0, fmt.Errorf("store: list events of task %q: encode cursor: %w", taskID, err)
	}

	return events, nextCursor, total, nil
}

// eventPageCursor is the decoded event page token: the (at, id) of the last
// event of the page that issued it.
type eventPageCursor struct {
	At int64  `json:"a"`
	ID string `json:"i"`
}

// encodeEventCursor renders an event page token as base64 JSON — the same
// codec every other list endpoint uses (docs/05-api-contract.md 1.4).
func encodeEventCursor(c eventPageCursor) (string, error) {
	encoded, err := json.Marshal(c)
	if err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

// decodeEventCursor parses an event page token. A token that is not base64
// JSON of the cursor shape is ErrStaleCursor: it belongs to no page, and
// the wire outcome is the same 422 as any other stale cursor.
func decodeEventCursor(token string) (eventPageCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return eventPageCursor{}, fmt.Errorf("%w: token is not valid base64", ErrStaleCursor)
	}

	var cursor eventPageCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		return eventPageCursor{}, fmt.Errorf("%w: token is not a page cursor", ErrStaleCursor)
	}
	if cursor.ID == "" {
		return eventPageCursor{}, fmt.Errorf("%w: token carries no row", ErrStaleCursor)
	}

	return cursor, nil
}

// insertTaskEvent is the single writer of task_events rows, shared by
// AppendEvent directly and by Transition inside its transaction.
func insertTaskEvent(
	ctx context.Context,
	ex sqlx.ExtContext,
	taskID, level, code, message string,
	detail any,
	now int64,
) error {
	var detailJSON *string
	if detail != nil {
		data, err := json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("store: marshal detail of task event %q: %w", code, err)
		}
		// A typed nil (nil pointer, map or slice in a non-nil interface)
		// marshals to the literal null; store SQL NULL for it too.
		if encoded := string(data); encoded != "null" {
			detailJSON = &encoded
		}
	}

	_, err := ex.ExecContext(
		ctx,
		queryInsertTaskEvent,
		NewID(PrefixTaskEvent), taskID, now, level, code, message, detailJSON, now, now,
	)
	if err != nil {
		return fmt.Errorf("store: insert task event %q for task %q: %w", code, taskID, err)
	}

	return nil
}
