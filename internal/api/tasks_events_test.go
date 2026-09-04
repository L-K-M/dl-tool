package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/L-K-M/dl-tool/internal/store"
)

// The lifecycle fixture's single submitted URI.
const lifecycleURI = "https://example.org/lifecycle.iso"

// getTaskEvents calls GET /tasks/{id}/events with the test credential.
func (e *tasksTestEnv) getTaskEvents(t *testing.T, id, query string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Get("/tasks/"+id+"/events"+query, "Authorization: Bearer "+e.bearer)
}

// decodeEventsBody decodes the cursor pagination envelope of doc 05 1.4
// carrying TaskEventDTO rows.
func decodeEventsBody(t *testing.T, recorder *httptest.ResponseRecorder) struct {
	Items      []TaskEventDTO `json:"items"`
	NextCursor *string        `json:"next_cursor"`
	Total      int            `json:"total"`
} {
	t.Helper()

	var body struct {
		Items      []TaskEventDTO `json:"items"`
		NextCursor *string        `json:"next_cursor"`
		Total      int            `json:"total"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body
}

// createLifecycleTask submits one URI through the create endpoint, so the
// task and its task.created row are written by the real code path.
func (e *tasksTestEnv) createLifecycleTask(t *testing.T) string {
	t.Helper()

	response := e.createTasks(t, map[string]any{"uris": []string{lifecycleURI}})
	if response.Code != http.StatusCreated {
		t.Fatalf("create: status %d body %s", response.Code, response.Body.String())
	}

	body := decodeCreateBody(t, response)
	if len(body.Created) != 1 {
		t.Fatalf("create returned %d tasks, want 1", len(body.Created))
	}

	return body.Created[0].ID
}

// TestEventLogCoversLifecycle pins FR-150's verify clause: a task taken
// from creation to completion has a non-empty, newest-first event list
// containing task.created and task.completed.
func TestEventLogCoversLifecycle(t *testing.T) {
	env := newTasksTestEnv(t)
	id := env.createLifecycleTask(t)

	// The completion the reconciler will report once the engine finishes
	// the transfer: the state move through the store, with the code the
	// poller passes (a user-forced completion writes task.force_completed
	// instead, and belongs to the actions tests).
	tasks := store.NewTaskStore(env.db)
	if err := tasks.Transition(t.Context(), id, "completed", store.CodeTaskCompleted, "download finished"); err != nil {
		t.Fatalf("complete task: %v", err)
	}

	// A detail-carrying row, so the decode path is covered against a
	// non-null detail_json too.
	if err := tasks.AppendEvent(t.Context(), id, "warn", "e.detail", "fixture detail", map[string]int{"n": 7}); err != nil {
		t.Fatalf("append detail event: %v", err)
	}

	response := env.getTaskEvents(t, id, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", response.Code, response.Body.String())
	}
	body := decodeEventsBody(t, response)

	if body.Total != 3 {
		t.Errorf("total = %d, want 3", body.Total)
	}
	if len(body.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(body.Items))
	}
	if body.NextCursor != nil {
		t.Errorf("next_cursor = %v, want null on the only page", *body.NextCursor)
	}

	// The page holds exactly the three logged codes; their order within
	// one shared millisecond is the ULID tiebreak, whose random part does
	// not order same-millisecond events by insertion — the store's
	// TestListEventsPagination pins that tiebreak with deterministic ids.
	// What is deterministic here is the at half: newest first.
	codes := map[string]TaskEventDTO{}
	for _, item := range body.Items {
		if _, repeated := codes[item.Code]; repeated {
			t.Fatalf("code %s returned twice", item.Code)
		}
		codes[item.Code] = item
	}
	for _, want := range []string{"e.detail", store.CodeTaskCompleted, store.CodeTaskCreated} {
		if _, ok := codes[want]; !ok {
			t.Fatalf("codes = %v, want %s among them", body.Items, want)
		}
	}

	created := codes[store.CodeTaskCreated]
	completed := codes[store.CodeTaskCompleted]
	if created.Level != "info" || completed.Level != "info" {
		t.Errorf("levels = %q, %q, want info, info", created.Level, completed.Level)
	}
	if created.Detail != nil {
		t.Errorf("task.created detail = %v, want null", created.Detail)
	}
	if detail, ok := codes["e.detail"].Detail.(map[string]any); !ok || detail["n"] != float64(7) {
		t.Errorf("detail = %v, want {\"n\": 7}", codes["e.detail"].Detail)
	}

	// at is RFC 3339 UTC and never increases down the page.
	var previous time.Time
	for i, item := range body.Items {
		stamp, err := time.Parse(time.RFC3339, item.At)
		if err != nil {
			t.Fatalf("at %q is not RFC 3339: %v", item.At, err)
		}
		if i > 0 && stamp.After(previous) {
			t.Errorf("at[%d] %v is newer than at[%d] %v; the page is not newest first", i, stamp, i-1, previous)
		}
		previous = stamp
	}
}

// TestEventCursorWalksEveryRowOnce pages through 250 events and asserts
// the cursor walk returns each id exactly once.
func TestEventCursorWalksEveryRowOnce(t *testing.T) {
	env := newTasksTestEnv(t)
	id := env.createLifecycleTask(t)

	// 249 more rows beside the create endpoint's task.created: 250 total.
	tasks := store.NewTaskStore(env.db)
	for i := 1; i < 250; i++ {
		if err := tasks.AppendEvent(t.Context(), id, "info", "e.seed", "fixture", nil); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
	}

	seen := map[string]bool{}
	query := "?limit=100"
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatalf("cursor walk exceeded 10 pages; the server keeps issuing cursors")
		}
		response := env.getTaskEvents(t, id, query)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body %s", response.Code, response.Body.String())
		}
		body := decodeEventsBody(t, response)

		if body.Total != 250 {
			t.Errorf("total = %d, want 250 on every page", body.Total)
		}
		if len(body.Items) > 100 {
			t.Errorf("page returned %d items, want at most limit=100", len(body.Items))
		}
		for _, item := range body.Items {
			if seen[item.ID] {
				t.Fatalf("event %s returned twice", item.ID)
			}
			seen[item.ID] = true
		}
		if body.NextCursor == nil {
			break
		}
		query = "?limit=100&cursor=" + url.QueryEscape(*body.NextCursor)
	}

	if len(seen) != 250 {
		t.Errorf("walk returned %d events, want 250", len(seen))
	}
}

// TestListTaskEventsUnknownTask pins the 404 /problems/not-found of an
// unknown task id (doc 05 section 5.10).
func TestListTaskEventsUnknownTask(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.getTaskEvents(t, unknownID, "")
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
}

// TestListTaskEventsRejectsStaleCursor pins the 422 of a cursor this
// endpoint never issued (doc 05 section 1.4). The body is asserted field
// by field so the hand-built model cannot drift from what Problem builds
// for the same slug and status.
func TestListTaskEventsRejectsStaleCursor(t *testing.T) {
	env := newTasksTestEnv(t)
	id := env.createLifecycleTask(t)

	response := env.getTaskEvents(t, id, "?cursor=not-a-token")
	problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	// Field by field, so the hand-built model cannot drift from what
	// Problem builds for the same slug and status.
	if problem.Title != http.StatusText(http.StatusUnprocessableEntity) {
		t.Errorf("title = %q, want %q", problem.Title, http.StatusText(http.StatusUnprocessableEntity))
	}
	if problem.Detail != staleCursorDetail {
		t.Errorf("detail = %q, want %q", problem.Detail, staleCursorDetail)
	}
	if len(problem.Errors) != 1 {
		t.Fatalf("errors = %+v, want exactly one", problem.Errors)
	}
	if problem.Errors[0].Message != staleCursorDetail || problem.Errors[0].Location != "query.cursor" {
		t.Errorf("errors[0] = %+v, want message %q at query.cursor", problem.Errors[0], staleCursorDetail)
	}
}
