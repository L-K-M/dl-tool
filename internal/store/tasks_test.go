package store

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/oklog/ulid/v2"
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

	t.Run("unknown state is rejected", func(t *testing.T) {
		_, err := tasks.Create(t.Context(), Task{
			Engine:      "aria2",
			SourceKind:  "http",
			Name:        "typo",
			State:       "queeud",
			Destination: "/data",
		})
		require.ErrorContains(t, err, "unknown state")
	})

	t.Run("empty state enters at queued", func(t *testing.T) {
		created, err := tasks.Create(t.Context(), Task{
			Engine:      "aria2",
			SourceKind:  "http",
			Name:        "stateless",
			Destination: "/data",
		})
		require.NoError(t, err)
		require.Equal(t, "queued", created.State)

		got, err := tasks.Get(t.Context(), created.ID)
		require.NoError(t, err)
		require.Equal(t, "queued", got.State)
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

	events, _, total, err := tasks.ListEvents(t.Context(), task.ID, 10, "")
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, 1, total)

	event := events[0]
	require.True(t, strings.HasPrefix(event.ID, PrefixTaskEvent))
	require.Equal(t, task.ID, event.TaskID)
	require.Equal(t, "info", event.Level)
	require.Equal(t, "engine.accepted", event.Code)
	require.Equal(t, "accepted by aria2", event.Message)
	require.Nil(t, event.DetailJSON)
	require.Equal(t, event.At, event.CreatedAt)
	require.Equal(t, event.At, event.UpdatedAt)

	require.NoError(t, tasks.Transition(t.Context(), task.ID, "error", "engine.failed", "disk full"))

	// A move into the error state is itself an error-level event. The
	// two transitions share a millisecond and NewID's random part does not
	// order same-millisecond ULIDs, so the page order is not asserted here:
	// TestListEventsPagination pins the ordering with deterministic ids.
	events, _, _, err = tasks.ListEvents(t.Context(), task.ID, 10, "")
	require.NoError(t, err)
	require.Len(t, events, 2)
	// The at half of the ordering contract is deterministic whatever the
	// ids do; pin it.
	require.GreaterOrEqual(t, events[0].At, events[1].At, "events are not newest-first")
	levels := map[string]string{}
	for _, event := range events {
		levels[event.Code] = event.Level
	}
	require.Equal(t, "info", levels["engine.accepted"])
	require.Equal(t, "error", levels["engine.failed"])
}

func TestCreateLoggedWritesEvent(t *testing.T) {
	db, _, _ := openTestStore(t)
	tasks := NewTaskStore(db)

	created, err := tasks.CreateLogged(t.Context(), Task{
		Engine:      "aria2",
		SourceKind:  "http",
		Name:        "evented-fixture",
		Destination: "/data",
	})
	require.NoError(t, err)

	// The row and its first event log entry persist together: the log of a
	// CreateLogged task starts at task.created (FR-150).
	events, _, total, err := tasks.ListEvents(t.Context(), created.ID, 10, "")
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, events, 1)
	require.Equal(t, CodeTaskCreated, events[0].Code)
	require.Equal(t, "info", events[0].Level)
	require.Equal(t, created.CreatedAt, events[0].At)
	require.Nil(t, events[0].DetailJSON)

	// The bare Create stays event-free — it backs fixtures and internal
	// seeds, whose event assertions would otherwise double-count.
	bare := createTaskInState(t, tasks, "queued")
	events, _, total, err = tasks.ListEvents(t.Context(), bare.ID, 10, "")
	require.NoError(t, err)
	require.Empty(t, events)
	require.Zero(t, total)
}

// TestCreateLoggedRollsBackWhenEventFails pins the transaction's reason to
// exist: when the event insert fails, the task insert dies with it, so no
// task row can persist without the first entry of its log.
func TestCreateLoggedRollsBackWhenEventFails(t *testing.T) {
	db, _, _ := openTestStore(t)

	// Make the event insert fail deterministically: with task_events gone,
	// the task INSERT still succeeds and only insertTaskEvent errors.
	if _, err := db.Exec(`DROP TABLE task_events`); err != nil {
		t.Fatalf("drop task_events: %v", err)
	}

	tasks := NewTaskStore(db)
	_, err := tasks.CreateLogged(t.Context(), Task{
		Engine:      "aria2",
		SourceKind:  "http",
		Name:        "rollback-fixture",
		Destination: "/data",
	})
	require.ErrorContains(t, err, "rollback-fixture")

	var count int
	require.NoError(t, db.GetContext(t.Context(), &count, `SELECT COUNT(*) FROM tasks`))
	require.Zero(t, count, "the task row survived the failed event insert")
}

func TestSetEngineRefWritesEvent(t *testing.T) {
	db, _, _ := openTestStore(t)
	tasks := NewTaskStore(db)
	task := createTaskInState(t, tasks, "queued")

	require.NoError(t, tasks.SetEngineRef(t.Context(), task.ID, "2089b05ecca3d829"))

	events, _, total, err := tasks.ListEvents(t.Context(), task.ID, 10, "")
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, events, 1)
	require.Equal(t, CodeEngineAccepted, events[0].Code)
	require.Equal(t, "info", events[0].Level)
	require.Nil(t, events[0].DetailJSON)
	require.Equal(t, events[0].At, events[0].CreatedAt)

	// A handle aimed at a missing task writes neither the handle nor the
	// event.
	err = tasks.SetEngineRef(t.Context(), "tsk_missing", "2089b05ecca3d829")
	require.ErrorIs(t, err, ErrNotFound)
	events, _, total, err = tasks.ListEvents(t.Context(), "tsk_missing", 10, "")
	require.NoError(t, err)
	require.Empty(t, events)
	require.Zero(t, total)
}

func TestListEventsPagination(t *testing.T) {
	db, _, _ := openTestStore(t)
	tasks := NewTaskStore(db)
	task := createTaskInState(t, tasks, "downloading")

	// Deterministic stamps; the shared millisecond exercises the id
	// tiebreak inside one page boundary. The ids are drawn from a
	// monotonic entropy rather than NewID, whose random part does not order
	// two ULIDs of one millisecond — the tiebreak assertion needs ids that
	// strictly increase with insertion order.
	base := int64(1_700_000_000_000)
	codes := []string{"e.one", "e.two", "e.three", "e.four", "e.five"}
	entropy := ulid.Monotonic(rand.Reader, 0)
	for i, code := range codes {
		stamp := base + int64(i/2)
		detail, err := json.Marshal(map[string]int{"n": i})
		require.NoError(t, err)
		id := PrefixTaskEvent + ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
		_, err = db.ExecContext(t.Context(),
			`INSERT INTO task_events (id, task_id, at, level, code, message, detail_json, created_at, updated_at)
VALUES (?, ?, ?, 'info', ?, ?, ?, ?, ?)`,
			id, task.ID, stamp, code, code, string(detail), stamp, stamp)
		require.NoError(t, err)
	}

	var order []string
	cursor := ""
	for {
		page, next, _, err := tasks.ListEvents(t.Context(), task.ID, 2, cursor)
		require.NoError(t, err)
		for _, event := range page {
			order = append(order, event.Code)
		}
		// next is empty exactly on the last page, so the walk stops on it
		// rather than on an empty page — a page that exactly fills the
		// limit is the last one and carries no cursor.
		if next == "" {
			break
		}
		cursor = next
	}

	// Newest first; within one millisecond, the later ULID sorts first.
	require.Equal(t, []string{"e.five", "e.four", "e.three", "e.two", "e.one"}, order)

	events, _, total, err := tasks.ListEvents(t.Context(), task.ID, 10, "")
	require.NoError(t, err)
	require.Equal(t, len(codes), total)
	require.JSONEq(t, `{"n":0}`, *events[len(events)-1].DetailJSON)

	// The envelope treats an absent limit (0) as the default page size and
	// still rejects an out-of-range one.
	events, _, _, err = tasks.ListEvents(t.Context(), task.ID, 0, "")
	require.NoError(t, err)
	require.Len(t, events, len(codes))
	_, _, _, err = tasks.ListEvents(t.Context(), task.ID, 501, "")
	require.Error(t, err)

	// A typed nil detail stores SQL NULL, not the JSON literal "null".
	require.NoError(t, tasks.AppendEvent(t.Context(), task.ID, "info", "e.typednil", "typed nil", (*struct{})(nil)))
	events, _, _, err = tasks.ListEvents(t.Context(), task.ID, 1, "")
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Nil(t, events[0].DetailJSON)

	// A typo'd level is a legible error naming the accepted vocabulary,
	// not a CHECK constraint dump.
	err = tasks.AppendEvent(t.Context(), task.ID, "warning", "e.badlevel", "bad level", nil)
	require.ErrorContains(t, err, "unknown level")
	require.ErrorContains(t, err, fmt.Sprintf("want one of %q", eventLevels))
}

func TestTaskStatesMatchTransitionTable(t *testing.T) {
	// Every state keys the transition table except terminal removed;
	// nothing pins the two vocabularies but this test, so drift fails CI.
	for _, state := range taskStates {
		if state == "removed" {
			require.NotContains(t, taskTransitions, state)

			continue
		}
		require.Contains(t, taskTransitions, state)
	}
	require.Len(t, taskTransitions, len(taskStates)-1)

	// Targets must be states too: a typo'd edge target would never match
	// in transitionLegal and surface as ErrIllegalTransition at runtime.
	for _, targets := range taskTransitions {
		for _, target := range targets {
			require.Contains(t, taskStates, target)
		}
	}
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

// expectedSidebarMembership restates the sidebar membership table of
// docs/04-data-model.md section 4.1 independently of sidebarFilterSets, so
// a drift in either fails here.
var expectedSidebarMembership = map[string][]string{
	"all":         {"queued", "downloading", "checking", "paused", "seeding", "completed", "extracting", "moving", "error"},
	"downloading": {"downloading"},
	"completed":   {"completed", "seeding"},
	"active":      {"downloading", "seeding"},
	"inactive":    {"error", "queued", "paused"},
	"stopped":     {"paused"},
	"error":       {"error"},
}

func TestSidebarFilterSets(t *testing.T) {
	db, _, _ := openTestStore(t)
	tasks := NewTaskStore(db)

	// One task per state: every filter's returned state set is then exactly
	// its membership, no more and no less.
	stateOf := map[string]string{} // id -> state
	for _, state := range allTaskStates {
		task := createTaskInState(t, tasks, state)
		stateOf[task.ID] = state
	}

	statesOf := func(page []Task) []string {
		states := make([]string, len(page))
		for i, task := range page {
			states[i] = stateOf[task.ID]
		}

		return states
	}

	for _, filter := range slices.Sorted(maps.Keys(expectedSidebarMembership)) {
		t.Run(filter, func(t *testing.T) {
			page, next, total, err := tasks.ListTasks(t.Context(), TaskFilter{State: filter, Limit: 100})
			require.NoError(t, err)
			require.Empty(t, next, "one page holds every seeded task")
			require.Equal(t, len(expectedSidebarMembership[filter]), total)
			require.ElementsMatch(t, expectedSidebarMembership[filter], statesOf(page))
		})
	}

	t.Run("no state behaves like all", func(t *testing.T) {
		page, _, total, err := tasks.ListTasks(t.Context(), TaskFilter{Limit: 100})
		require.NoError(t, err)
		require.Equal(t, len(allTaskStates)-1, total)
		require.ElementsMatch(t, expectedSidebarMembership["all"], statesOf(page))
	})

	t.Run("explicit removed lists tombstones", func(t *testing.T) {
		page, _, total, err := tasks.ListTasks(t.Context(), TaskFilter{State: "removed", Limit: 100})
		require.NoError(t, err)
		require.Equal(t, 1, total)
		require.Equal(t, "removed", stateOf[page[0].ID])
	})

	t.Run("unknown state is ErrUnknownFilterState", func(t *testing.T) {
		_, _, _, err := tasks.ListTasks(t.Context(), TaskFilter{State: "downloding"})
		require.ErrorIs(t, err, ErrUnknownFilterState)
	})
}

func TestListTasksCategoryTagAndQuery(t *testing.T) {
	db, _, _ := openTestStore(t)
	tasks := NewTaskStore(db)

	categoryID := NewID(PrefixCategory)
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO categories (id, name, save_path, created_at, updated_at) VALUES (?, 'linux', '/data/linux', 0, 0)`,
		categoryID)
	require.NoError(t, err)
	tagID := NewID(PrefixTag)
	_, err = db.ExecContext(t.Context(),
		`INSERT INTO tags (id, name, created_at, updated_at) VALUES (?, 'iso', 0, 0)`,
		tagID)
	require.NoError(t, err)

	create := func(name string, category *string, tagIDs ...string) Task {
		task, err := tasks.Create(t.Context(), Task{
			Engine: "aria2", SourceKind: "http", Name: name, State: "queued",
			Destination: "/data", CategoryID: category,
		})
		require.NoError(t, err)
		for _, id := range tagIDs {
			_, err := db.ExecContext(t.Context(), `INSERT INTO task_tags (task_id, tag_id) VALUES (?, ?)`, task.ID, id)
			require.NoError(t, err)
		}

		return task
	}

	ubuntu := create("Ubuntu ISO", &categoryID, tagID)
	debian := create("Debian netinst", nil)
	arch := create("Arch percent 100%", nil, tagID)
	underscore := create("under_score", nil)
	// Decoys that match the queries below only if % and _ reach SQLite as
	// LIKE wildcards instead of literals.
	create("underXscore", nil)
	create("1000 things", nil)

	page, _, total, err := tasks.ListTasks(t.Context(), TaskFilter{Category: "linux", HasCategory: true, Limit: 100})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Equal(t, ubuntu.ID, page[0].ID)

	_, _, total, err = tasks.ListTasks(t.Context(), TaskFilter{Category: "", HasCategory: true, Limit: 100})
	require.NoError(t, err)
	require.Equal(t, 5, total) // debian, arch, underscore + two decoys

	page, _, total, err = tasks.ListTasks(t.Context(), TaskFilter{Tag: "iso", HasTag: true, Limit: 100})
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.ElementsMatch(t, []string{ubuntu.ID, arch.ID}, []string{page[0].ID, page[1].ID})

	_, _, total, err = tasks.ListTasks(t.Context(), TaskFilter{Tag: "", HasTag: true, Limit: 100})
	require.NoError(t, err)
	require.Equal(t, 4, total) // debian, underscore + two decoys

	// Unknown names simply match nothing; a filter is not a reference.
	_, _, total, err = tasks.ListTasks(t.Context(), TaskFilter{Category: "no-such", HasCategory: true, Limit: 100})
	require.NoError(t, err)
	require.Equal(t, 0, total)

	_, _, total, err = tasks.ListTasks(t.Context(), TaskFilter{Query: "ISO", Limit: 100})
	require.NoError(t, err)
	require.Equal(t, 1, total) // case-insensitive substring

	// % and _ are literal substrings, not wildcards.
	_, _, total, err = tasks.ListTasks(t.Context(), TaskFilter{Query: "100%", Limit: 100})
	require.NoError(t, err)
	require.Equal(t, 1, total)

	_, _, total, err = tasks.ListTasks(t.Context(), TaskFilter{Query: "under_score", Limit: 100})
	require.NoError(t, err)
	require.Equal(t, 1, total)

	_ = debian
	_ = underscore
}

func TestListTasksSortAndTotal(t *testing.T) {
	db, _, _ := openTestStore(t)
	tasks := NewTaskStore(db)

	seed := []struct {
		name      string
		total     *int64
		completed int64
	}{
		{"half", ptr(int64(1000)), 500},
		{"quarter", ptr(int64(1000)), 250},
		{"unknown", nil, 700},
		{"zero", ptr(int64(0)), 0},
		{"done", ptr(int64(1000)), 1000},
	}
	for _, row := range seed {
		_, err := tasks.Create(t.Context(), Task{
			Engine: "aria2", SourceKind: "http", Name: row.name, State: "downloading",
			Destination: "/data", TotalBytes: row.total, CompletedBytes: row.completed,
		})
		require.NoError(t, err)
	}

	// progress: NULL totals (unknown, zero) sort as one cluster, then
	// quarter, half, done ascending.
	page, _, _, err := tasks.ListTasks(t.Context(), TaskFilter{Sort: "progress", Limit: 100})
	require.NoError(t, err)
	var names []string
	for _, task := range page {
		names = append(names, task.Name)
	}
	require.Equal(t, []string{"quarter", "half", "done"}, names[2:])
	require.ElementsMatch(t, []string{"unknown", "zero"}, names[:2])

	// name descending.
	page, _, _, err = tasks.ListTasks(t.Context(), TaskFilter{Sort: "-name", Limit: 100})
	require.NoError(t, err)
	require.Equal(t, "zero", page[0].Name)
	require.Equal(t, "done", page[len(page)-1].Name)

	// total counts the filter and ignores the cursor (doc 05 section 1.4).
	page, next, total, err := tasks.ListTasks(t.Context(), TaskFilter{Sort: "name", Limit: 2})
	require.NoError(t, err)
	require.Equal(t, 5, total)
	require.Len(t, page, 2)
	require.NotEmpty(t, next)
	_, next, total, err = tasks.ListTasks(t.Context(), TaskFilter{Sort: "name", Limit: 2, Cursor: next})
	require.NoError(t, err)
	require.Equal(t, 5, total)
	require.Len(t, page, 2)
	_ = next
}

func TestCursorWalksEveryRowOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 5000 rows")
	}

	db, _, _ := openTestStore(t)
	tasks := NewTaskStore(db)

	const seeded = 5000
	created := make(map[string]bool, seeded)
	for i := 0; i < seeded; i++ {
		task, err := tasks.Create(t.Context(), Task{
			Engine: "aria2", SourceKind: "http",
			Name: fmt.Sprintf("task-%05d", i), State: "queued", Destination: "/data",
		})
		require.NoError(t, err)
		created[task.ID] = true
	}

	walk := func(t *testing.T, sort string, descending bool) {
		t.Helper()

		seen := make(map[string]bool, seeded)
		cursor := ""
		var previousID string
		var previousAdded int64
		pages := 0
		for {
			page, next, total, err := tasks.ListTasks(t.Context(), TaskFilter{Sort: sort, Limit: 100, Cursor: cursor})
			require.NoError(t, err)
			require.Equal(t, seeded, total)
			require.LessOrEqual(t, len(page), 100)
			for _, task := range page {
				require.False(t, seen[task.ID], "row %s returned twice", task.ID)
				seen[task.ID] = true
				// added_at with the id tiebreak, in the walk's direction:
				// the seeded rows share milliseconds, so the tiebreak carries
				// most of the ordering.
				if previousID != "" {
					less := task.AddedAt < previousAdded ||
						(task.AddedAt == previousAdded && task.ID < previousID)
					require.Equal(t, descending, less, "page order broke the sort contract at %s", task.ID)
				}
				previousID, previousAdded = task.ID, task.AddedAt
			}
			pages++
			if next == "" {
				break
			}
			cursor = next
		}

		require.Len(t, seen, seeded)
		for id := range created {
			require.True(t, seen[id], "row %s never returned", id)
		}
		// 5000 rows in pages of 100: exactly 50 pages, no ragged tail.
		require.Equal(t, seeded/100, pages)
	}
	t.Run("default descending", func(t *testing.T) { walk(t, "", true) })
	t.Run("ascending", func(t *testing.T) { walk(t, "added_at", false) })
}

func TestCursorWalkNullableSort(t *testing.T) {
	db, _, _ := openTestStore(t)
	tasks := NewTaskStore(db)

	const seeded = 7
	ids := make([]string, seeded)
	for i := range ids {
		task := createTaskInState(t, tasks, "completed")
		ids[i] = task.ID
	}

	// Three distinct stamps, a shared stamp for ties, three NULLs: the
	// cluster a cursor must neither skip nor repeat.
	stamps := []any{int64(3000), int64(2000), int64(2000), nil, nil, nil, int64(1000)}
	for i, id := range ids {
		_, err := db.ExecContext(t.Context(), `UPDATE tasks SET completed_at = ? WHERE id = ?`, stamps[i], id)
		require.NoError(t, err)
	}

	for _, tc := range []struct {
		name string
		sort string
	}{
		{"ascending", "completed_at"},
		{"descending", "-completed_at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := map[string]bool{}
			cursor := ""
			for {
				page, next, total, err := tasks.ListTasks(t.Context(), TaskFilter{Sort: tc.sort, Limit: 2, Cursor: cursor})
				require.NoError(t, err)
				require.Equal(t, seeded, total)
				for _, task := range page {
					require.False(t, seen[task.ID], "row %s returned twice", task.ID)
					seen[task.ID] = true
				}
				if next == "" {
					break
				}
				cursor = next
			}
			require.Len(t, seen, seeded)
		})
	}
}

func TestListTasksRejectsUnknownSort(t *testing.T) {
	db, _, _ := openTestStore(t)
	tasks := NewTaskStore(db)
	_, err := tasks.Create(t.Context(), Task{
		Engine: "aria2", SourceKind: "http", Name: "fixture", State: "queued", Destination: "/data",
	})
	require.NoError(t, err)

	_, _, _, err = tasks.ListTasks(t.Context(), TaskFilter{Sort: "password", Limit: 10})
	require.ErrorIs(t, err, ErrInvalidSort)

	// A reversed key is still a key: only the prefix is special.
	_, _, total, err := tasks.ListTasks(t.Context(), TaskFilter{Sort: "-added_at", Limit: 10})
	require.NoError(t, err)
	require.Equal(t, 1, total)

	_, _, _, err = tasks.ListTasks(t.Context(), TaskFilter{Limit: 501})
	require.ErrorContains(t, err, "outside 1..500")

	_, _, _, err = tasks.ListTasks(t.Context(), TaskFilter{Limit: -1})
	require.ErrorContains(t, err, "outside 1..500")
}

func TestListTasksRejectsStaleCursor(t *testing.T) {
	db, _, _ := openTestStore(t)
	tasks := NewTaskStore(db)

	for i := 0; i < 3; i++ {
		_, err := tasks.Create(t.Context(), Task{
			Engine: "aria2", SourceKind: "http",
			Name: fmt.Sprintf("fixture-%d", i), State: "downloading", Destination: "/data",
		})
		require.NoError(t, err)
	}
	_, next, _, err := tasks.ListTasks(t.Context(), TaskFilter{Limit: 1})
	require.NoError(t, err)
	require.NotEmpty(t, next)

	// Same filter replays fine; a different filter or sort is 422 material.
	_, _, _, err = tasks.ListTasks(t.Context(), TaskFilter{Limit: 1, Cursor: next})
	require.NoError(t, err)

	_, _, _, err = tasks.ListTasks(t.Context(), TaskFilter{State: "downloading", Limit: 1, Cursor: next})
	require.ErrorIs(t, err, ErrStaleCursor)

	_, _, _, err = tasks.ListTasks(t.Context(), TaskFilter{Sort: "name", Limit: 1, Cursor: next})
	require.ErrorIs(t, err, ErrStaleCursor)

	_, _, _, err = tasks.ListTasks(t.Context(), TaskFilter{Limit: 1, Cursor: "not-a-cursor"})
	require.ErrorIs(t, err, ErrStaleCursor)
}

func ptr[T any](v T) *T { return &v }
