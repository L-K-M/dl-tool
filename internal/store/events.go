package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jmoiron/sqlx"
)

// EventCursor marks a position in one task's event log: the last event of
// the page just read. The zero value starts at the newest event.
type EventCursor struct {
	At int64
	ID string
}

// eventLevels is the level vocabulary of the task_events DDL CHECK.
// AppendEvent consults it to answer a typo with a legible error instead of
// a constraint dump.
var eventLevels = []string{"info", "warn", "error"}

const (
	queryInsertTaskEvent = `INSERT INTO task_events
(id, task_id, at, level, code, message, detail_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

	queryListTaskEvents = `SELECT id, task_id, at, level, code, message, detail_json, created_at, updated_at
FROM task_events
WHERE task_id = ?
ORDER BY at DESC, id DESC
LIMIT ?`

	// The cursor is the (at, id) tuple of the last event seen: at alone is
	// not unique — two events may share a millisecond — and the ULID id
	// breaks the tie in the same order the index scans.
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

// ListEvents returns up to limit events of one task, newest first, starting
// after cursor. limit must be positive.
func (s *TaskStore) ListEvents(ctx context.Context, taskID string, cursor EventCursor, limit int) ([]TaskEvent, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list events of task %q: limit %d is not positive", taskID, limit)
	}

	var (
		events []TaskEvent
		err    error
	)
	if cursor == (EventCursor{}) {
		err = s.db.SelectContext(ctx, &events, queryListTaskEvents, taskID, limit)
	} else {
		err = s.db.SelectContext(
			ctx,
			&events,
			queryListTaskEventsBefore,
			taskID, cursor.At, cursor.At, cursor.ID, limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("store: list events of task %q: %w", taskID, err)
	}

	return events, nil
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
