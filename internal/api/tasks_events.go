// The per-task event log of docs/05-api-contract.md section 5.10: one
// cursor-paginated page of task_events rows, newest first, every state
// transition and engine outcome the task logged.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/L-K-M/dl-tool/internal/store"
)

const operationListTaskEvents = "list-task-events"

// ListTaskEventsInput is the query of GET /tasks/{id}/events: the cursor
// pagination envelope of doc 05 section 1.4 and nothing else.
type ListTaskEventsInput struct {
	ID     string `path:"id"             doc:"The tsk_ id of the task"`
	Limit  int    `query:"limit" minimum:"1" maximum:"500" default:"100" doc:"Page size"`
	Cursor string `query:"cursor"        doc:"Opaque page token from a previous response"`
}

// ListTaskEventsOutput is the cursor pagination envelope of doc 05
// section 1.4, carrying TaskEventDTO rows.
type ListTaskEventsOutput struct {
	Body struct {
		Items      []TaskEventDTO `json:"items"      doc:"Event rows, newest first"`
		NextCursor *string        `json:"next_cursor" doc:"Token for the next page; null on the last page"`
		Total      int            `json:"total"      doc:"Rows logged for the task, ignoring the cursor"`
	}
}

// TaskEventDTO is the wire shape of one task_events row (doc 05 5.10).
// At is RFC 3339 UTC — the database stores Unix milliseconds and the
// conversion happens here, at the API boundary. Detail is null when
// detail_json is NULL and the decoded JSON value otherwise.
type TaskEventDTO struct {
	ID      string `json:"id"      doc:"The evt_ id of the event"`
	At      string `json:"at"      doc:"When the event was logged" format:"date-time"`
	Level   string `json:"level"   enum:"info,warn,error" doc:"info, warn or error"`
	Code    string `json:"code"    doc:"A stable i18n key; the UI translates it and falls back to message"`
	Message string `json:"message" doc:"Human-readable fallback for the code"`
	Detail  any    `json:"detail"  doc:"Structured payload of the event, or null"`
}

// ListTaskEvents serves GET /tasks/{id}/events: one page of the task's
// event log, newest first. An unknown task id is 404 /problems/not-found;
// a cursor that is not one of this endpoint's tokens is 422.
func (h *TaskHandlers) ListTaskEvents(ctx context.Context, in *ListTaskEventsInput) (*ListTaskEventsOutput, error) {
	// The list itself cannot tell an empty log from a missing task, so the
	// task is resolved first and the 404 answered before any page is read.
	task, err := h.tasks.Get(ctx, in.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, Problem(SlugNotFound, http.StatusNotFound, "the addressed task does not exist")
	}
	if err != nil {
		return nil, internalFailure(ctx, "get task for events", err)
	}

	rows, nextCursor, total, err := h.tasks.ListEvents(ctx, task.ID, in.Limit, in.Cursor)
	if errors.Is(err, store.ErrStaleCursor) {
		problem := Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "the cursor was not issued by this endpoint")
		var model *huma.ErrorModel
		errors.As(problem, &model)
		model.Errors = []*huma.ErrorDetail{{
			Message:  "the cursor was not issued by this endpoint",
			Location: "query.cursor",
		}}

		return nil, problem
	}
	if err != nil {
		return nil, internalFailure(ctx, "list task events", err)
	}

	items := make([]TaskEventDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, newTaskEventDTO(row))
	}

	output := &ListTaskEventsOutput{}
	output.Body.Items = items
	output.Body.Total = total
	if nextCursor != "" {
		output.Body.NextCursor = &nextCursor
	}

	return output, nil
}

// newTaskEventDTO renders one row: Unix milliseconds to RFC 3339 UTC, and
// detail_json decoded into a JSON value — nil stays null on the wire.
func newTaskEventDTO(e store.TaskEvent) TaskEventDTO {
	var detail any
	if e.DetailJSON != nil {
		if err := json.Unmarshal([]byte(*e.DetailJSON), &detail); err != nil {
			// detail_json is written by json.Marshal on this package's own
			// values, so a decode failure means a corrupted row; surface it
			// as null rather than failing the whole page.
			detail = nil
		}
	}

	return TaskEventDTO{
		ID:      e.ID,
		At:      time.UnixMilli(e.At).UTC().Format(time.RFC3339),
		Level:   e.Level,
		Code:    e.Code,
		Message: e.Message,
		Detail:  detail,
	}
}
