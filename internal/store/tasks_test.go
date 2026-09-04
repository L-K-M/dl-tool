package store

import (
	"slices"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/require"
)

// allTaskStates is the ten-state vocabulary of docs/04-data-model.md
// section 4.1.
var allTaskStates = []string{
	"queued", "downloading", "checking", "paused", "seeding",
	"completed", "extracting", "moving", "error", "removed",
}

// expectedLegalTransitions restates the full edge set of
// docs/03-architecture.md section 8.1: the task-file table plus the
// universal rules (any non-terminal state to paused, error and removed;
// removed is terminal). It is written out independently of the
// implementation's table so a drift in either fails here. The
// engine-reported targets of docs/06-download-engines.md sections 4.6 and
// 5.6 (queued, downloading, checking, seeding, completed, paused, error,
// removed) are each covered from downloading, seeding and paused — for
// example downloading -> seeding, seeding -> downloading and paused ->
// checking. From completed the table admits only checking, seeding,
// extracting, moving, paused, error and removed; completed ->
// queued/downloading are illegal, so a completed task the engine reports
// as transferring again needs an intermediate hop: checking, seeding or
// paused reach downloading directly, error only through queued.
var expectedLegalTransitions = map[string][]string{
	"queued":      {"downloading", "checking", "seeding", "completed", "paused", "error", "removed"},
	"downloading": {"queued", "checking", "seeding", "completed", "paused", "error", "removed"},
	"checking":    {"queued", "downloading", "seeding", "completed", "paused", "error", "removed"},
	"paused":      {"queued", "downloading", "checking", "seeding", "completed", "error", "removed"},
	"seeding":     {"queued", "downloading", "checking", "completed", "extracting", "moving", "paused", "error", "removed"},
	"completed":   {"checking", "seeding", "extracting", "moving", "paused", "error", "removed"},
	"extracting":  {"moving", "completed", "paused", "error", "removed"},
	"moving":      {"completed", "seeding", "paused", "error", "removed"},
	"error":       {"queued", "completed", "paused", "removed"},
	"removed":     {},
}

func TestTaskRoundTrip(t *testing.T) {
	db, _, _ := openTestStore(t)
	tasks := NewTaskStore(db)

	t.Run("every column set", func(t *testing.T) {
		categoryID := NewID(PrefixCategory)
		_, err := db.ExecContext(
			t.Context(),
			`INSERT INTO categories (id, name, save_path, created_at, updated_at)
VALUES (?, 'linux', '/data/linux', 0, 0)`,
			categoryID,
		)
		require.NoError(t, err)

		input := Task{
			ID:             NewID(PrefixTask),
			Engine:         "qbittorrent",
			EngineRef:      ptr("gid-or-hash"),
			SourceKind:     "magnet",
			SourceURI:      ptr("magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"),
			Name:           "fixture",
			InfohashV1:     ptr(strings.Repeat("a", 40)),
			InfohashV2:     ptr(strings.Repeat("b", 64)),
			State:          "downloading",
			ErrorCode:      ptr("timeout"),
			ErrorMessage:   ptr("slow"),
			Destination:    "/data",
			ContentPath:    ptr("/data/fixture"),
			CategoryID:     &categoryID,
			TotalBytes:     ptr(int64(1_000_000)),
			CompletedBytes: 500_000,
			UploadedBytes:  250_000,
			DownloadRate:   1_024,
			UploadRate:     512,
			ETASeconds:     ptr(int64(488)),
			Sequential:     1,
			QueuePosition:  ptr(int64(3)),
			StartedAt:      ptr(int64(1_700_000_000_000)),
			CompletedAt:    ptr(int64(1_700_000_001_000)),
		}

		created, err := tasks.Create(t.Context(), input)
		require.NoError(t, err)

		// added_at, created_at and updated_at share one stamp.
		require.Equal(t, created.AddedAt, created.CreatedAt)
		require.Equal(t, created.AddedAt, created.UpdatedAt)

		expected := input
		expected.AddedAt = created.AddedAt
		expected.CreatedAt = created.CreatedAt
		expected.UpdatedAt = created.UpdatedAt
		require.Equal(t, expected, created)

		got, err := tasks.Get(t.Context(), created.ID)
		require.NoError(t, err)
		require.Equal(t, created, got)
	})

	t.Run("generated id and nullables stay nil", func(t *testing.T) {
		created, err := tasks.Create(t.Context(), Task{
			Engine:      "aria2",
			SourceKind:  "http",
			Name:        "bare",
			State:       "queued",
			Destination: "/data",
		})
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(created.ID, PrefixTask))

		got, err := tasks.Get(t.Context(), created.ID)
		require.NoError(t, err)
		require.Equal(t, created, got)

		require.Nil(t, got.EngineRef)
		require.Nil(t, got.SourceURI)
		require.Nil(t, got.InfohashV1)
		require.Nil(t, got.InfohashV2)
		require.Nil(t, got.ErrorCode)
		require.Nil(t, got.ErrorMessage)
		require.Nil(t, got.ContentPath)
		require.Nil(t, got.CategoryID)
		require.Nil(t, got.TotalBytes)
		require.Nil(t, got.ETASeconds)
		require.Nil(t, got.QueuePosition)
		require.Nil(t, got.StartedAt)
		require.Nil(t, got.CompletedAt)
	})

	t.Run("missing id is ErrNotFound", func(t *testing.T) {
		_, err := tasks.Get(t.Context(), "tsk_missing")
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestTaskMutators(t *testing.T) {
	db, _, _ := openTestStore(t)
	tasks := NewTaskStore(db)

	task := createTaskInState(t, tasks, "downloading")

	progress := Progress{
		TotalBytes:     ptr(int64(2_000)),
		CompletedBytes: 1_000,
		UploadedBytes:  100,
		DownloadRate:   200,
		UploadRate:     10,
		ETASeconds:     ptr(int64(5)),
	}
	require.NoError(t, tasks.UpdateProgress(t.Context(), task.ID, progress))

	got, err := tasks.Get(t.Context(), task.ID)
	require.NoError(t, err)
	require.Equal(t, progress.TotalBytes, got.TotalBytes)
	require.Equal(t, progress.CompletedBytes, got.CompletedBytes)
	require.Equal(t, progress.UploadedBytes, got.UploadedBytes)
	require.Equal(t, progress.DownloadRate, got.DownloadRate)
	require.Equal(t, progress.UploadRate, got.UploadRate)
	require.Equal(t, progress.ETASeconds, got.ETASeconds)
	require.GreaterOrEqual(t, got.UpdatedAt, task.UpdatedAt)
	afterProgress := got.UpdatedAt

	require.NoError(t, tasks.SetEngineRef(t.Context(), task.ID, "gid-1"))
	got, err = tasks.Get(t.Context(), task.ID)
	require.NoError(t, err)
	require.Equal(t, "gid-1", *got.EngineRef)
	require.GreaterOrEqual(t, got.UpdatedAt, afterProgress)

	// A mutator aimed at a missing task is ErrNotFound, never a silent
	// success: the poller reads that as the signal to stop polling.
	err = tasks.UpdateProgress(t.Context(), "tsk_missing", progress)
	require.ErrorIs(t, err, ErrNotFound)
	err = tasks.SetEngineRef(t.Context(), "tsk_missing", "gid-2")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestTransitionTable(t *testing.T) {
	db, _, _ := openTestStore(t)
	tasks := NewTaskStore(db)

	for _, from := range allTaskStates {
		for _, next := range allTaskStates {
			legal := slices.Contains(expectedLegalTransitions[from], next)
			t.Run(from+"_to_"+next, func(t *testing.T) {
				task := createTaskInState(t, tasks, from)
				err := tasks.Transition(t.Context(), task.ID, next, "test.transition", "test")

				if !legal {
					// A rejected transition mutates neither the
					// state nor the event log.
					require.ErrorIs(t, err, ErrIllegalTransition)
					got, gerr := tasks.Get(t.Context(), task.ID)
					require.NoError(t, gerr)
					require.Equal(t, from, got.State)
					require.Equal(t, 0, eventCount(t, db, task.ID))

					return
				}

				require.NoError(t, err)
				got, gerr := tasks.Get(t.Context(), task.ID)
				require.NoError(t, gerr)
				require.Equal(t, next, got.State)
				require.Equal(t, 1, eventCount(t, db, task.ID))
			})
		}
	}

	t.Run("missing task is ErrNotFound", func(t *testing.T) {
		err := tasks.Transition(t.Context(), "tsk_missing", "paused", "test.transition", "test")
		require.ErrorIs(t, err, ErrNotFound)
	})

	// The compare-and-swap guard against a concurrent transition
	// committing between Transition's read and its update: a stale
	// expected state matches no row and leaves the task untouched.
	// Transition maps that to ErrTransitionConflict; under this pool
	// (single connection, immediate transactions) the branch cannot be
	// reached through Transition itself, so it is pinned here at the SQL
	// level.
	t.Run("stale expected state updates nothing", func(t *testing.T) {
		task := createTaskInState(t, tasks, "queued")

		result, err := db.ExecContext(t.Context(), queryTransitionTask, "downloading", int64(0), task.ID, "paused")
		require.NoError(t, err)
		affected, err := result.RowsAffected()
		require.NoError(t, err)
		require.Equal(t, int64(0), affected)

		got, err := tasks.Get(t.Context(), task.ID)
		require.NoError(t, err)
		require.Equal(t, "queued", got.State)
	})

	// The branch itself is unreachable through Transition under this pool,
	// so pin the sentinel mapping on the error constructor directly.
	t.Run("conflict error wraps ErrTransitionConflict", func(t *testing.T) {
		err := errTransitionConflict("tsk_x", "queued", "paused")
		require.ErrorIs(t, err, ErrTransitionConflict)
		require.NotErrorIs(t, err, ErrIllegalTransition)
	})
}

func TestTransitionWritesEvent(t *testing.T) {
	db, _, _ := openTestStore(t)
	tasks := NewTaskStore(db)

	task := createTaskInState(t, tasks, "queued")
	require.NoError(t, tasks.Transition(t.Context(), task.ID, "downloading", "engine.accepted", "accepted by aria2"))

	events, err := tasks.ListEvents(t.Context(), task.ID, EventCursor{}, 10)
	require.NoError(t, err)
	require.Len(t, events, 1)

	event := events[0]
	require.True(t, strings.HasPrefix(event.ID, PrefixTaskEvent))
	require.Equal(t, task.ID, event.TaskID)
	require.Equal(t, "info", event.Level)
	require.Equal(t, "engine.accepted", event.Code)
	require.Equal(t, "accepted by aria2", event.Message)
	require.Nil(t, event.DetailJSON)
	require.Equal(t, event.At, event.CreatedAt)
	require.Equal(t, event.At, event.UpdatedAt)

	// A move into the error state is itself an error-level event.
	require.NoError(t, tasks.Transition(t.Context(), task.ID, "error", "engine.failed", "disk full"))

	events, err = tasks.ListEvents(t.Context(), task.ID, EventCursor{}, 10)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "error", events[0].Level)
	require.Equal(t, "engine.failed", events[0].Code)
}

func TestListEventsPagination(t *testing.T) {
	db, _, _ := openTestStore(t)
	tasks := NewTaskStore(db)
	task := createTaskInState(t, tasks, "downloading")

	// Deterministic stamps; the shared millisecond exercises the id
	// tiebreak inside one page boundary.
	base := int64(1_700_000_000_000)
	codes := []string{"e.one", "e.two", "e.three", "e.four", "e.five"}
	for i, code := range codes {
		stamp := base + int64(i/2)
		require.NoError(t, insertTaskEvent(t.Context(), db, task.ID, "info", code, code, map[string]int{"n": i}, stamp))
	}

	var order []string
	cursor := EventCursor{}
	for {
		page, err := tasks.ListEvents(t.Context(), task.ID, cursor, 2)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		for _, event := range page {
			order = append(order, event.Code)
		}
		last := page[len(page)-1]
		cursor = EventCursor{At: last.At, ID: last.ID}
	}

	// Newest first; within one millisecond, the later ULID sorts first.
	require.Equal(t, []string{"e.five", "e.four", "e.three", "e.two", "e.one"}, order)

	events, err := tasks.ListEvents(t.Context(), task.ID, EventCursor{}, 10)
	require.NoError(t, err)
	require.JSONEq(t, `{"n":0}`, *events[len(events)-1].DetailJSON)

	_, err = tasks.ListEvents(t.Context(), task.ID, EventCursor{}, 0)
	require.Error(t, err)
}

func createTaskInState(t *testing.T, tasks *TaskStore, state string) Task {
	t.Helper()

	task, err := tasks.Create(t.Context(), Task{
		Engine:      "aria2",
		SourceKind:  "http",
		Name:        "fixture",
		State:       state,
		Destination: "/data",
	})
	require.NoError(t, err)

	return task
}

func eventCount(t *testing.T, db *sqlx.DB, taskID string) int {
	t.Helper()

	var count int
	require.NoError(t, db.GetContext(t.Context(), &count, `SELECT COUNT(*) FROM task_events WHERE task_id = ?`, taskID))

	return count
}

func ptr[T any](v T) *T { return &v }
