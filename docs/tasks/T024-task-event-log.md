# T024 — Record and serve the per-task event log

| Field | Value |
|---|---|
| **ID** | T024 |
| **Milestone** | M1 |
| **Status** | todo |
| **Depends on** | T017, T020, T023 |
| **Blocks** | T025, T026, T048, T071, T074, T096, T098, T099 |
| **Parallel-safe** | no — extends the shared `internal/api/tasks.go` |
| **Implements** | [FR-150](../02-requirements.md#fr-150-record-a-per-task-event-log) |
| **Decisions** | [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md) |
| **Est. size** | 2 new files, ~250 LOC |

## Goal
Every state transition and every engine outcome writes one `task_events` row carrying a stable dotted code,
and `GET /api/v1/tasks/{id}/events` returns them newest first with cursor pagination.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §5.10 `GET /tasks/{id}/events`](../05-api-contract.md#510-get-tasksidevents)
2. [`docs/14-conventions.md` §4 The `task_events` code vocabulary](../14-conventions.md#4-the-task_events-code-vocabulary)
3. [`docs/04-data-model.md` §3.3 Tasks](../04-data-model.md#33-tasks)
4. [`docs/05-api-contract.md` §1.4 Cursor pagination](../05-api-contract.md#14-cursor-pagination)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/tasks_events.go` | create | The `GET /tasks/{id}/events` handler. |
| `internal/api/tasks_events_test.go` | create | Ordering, pagination and end-to-end coverage of the codes. |
| `internal/store/events.go` | modify | Add the code constants and the cursor query. |
| `internal/api/server.go` | modify | Register `list-task-events`. |

No other file may be modified.

## Interface contract

```go
package store

// Task-event codes emitted in M1. A code is <area>.<subject>[.<outcome>], lower case, ASCII letters,
// digits and dots only. Never rename or reuse a code: a rename orphans stored history rows.
const (
	CodeTaskCreated       = "task.created"
	CodeTaskPaused        = "task.paused"
	CodeTaskResumed       = "task.resumed"
	CodeTaskRemoved       = "task.removed"
	CodeTaskCompleted     = "task.completed"
	CodeTaskDataDeleted   = "task.data_deleted"
	CodeEngineAccepted    = "engine.accepted"
	CodeEngineRejected    = "engine.rejected"
	CodeEngineUnavailable = "engine.unavailable"
)

// TaskEvent is one row of task_events. At is Unix milliseconds; the API converts it to RFC 3339.
type TaskEvent struct {
	ID         string  `db:"id"          json:"id"`
	TaskID     string  `db:"task_id"     json:"task_id"`
	At         int64   `db:"at"          json:"at"`
	Level      string  `db:"level"       json:"level"`
	Code       string  `db:"code"        json:"code"`
	Message    string  `db:"message"     json:"message"`
	DetailJSON *string `db:"detail_json" json:"detail"`
}

// ListEvents returns one page of events for a task, newest first, with the same cursor envelope as
// every other list endpoint.
func (s *TaskStore) ListEvents(ctx context.Context, taskID string, limit int, cursor string) (items []TaskEvent, nextCursor string, total int, err error)
```

```go
package api

type ListTaskEventsInput struct {
	ID     string `path:"id"`
	Limit  int    `query:"limit"  default:"100" minimum:"1" maximum:"500"`
	Cursor string `query:"cursor"`
}

type ListTaskEventsOutput struct {
	Body struct {
		Items      []TaskEventDTO `json:"items"`
		NextCursor *string        `json:"next_cursor"`
		Total      int            `json:"total"`
	}
}

// TaskEventDTO is the wire shape. Detail is null when detail_json is NULL.
type TaskEventDTO struct {
	ID      string `json:"id"`      // evt_ + ULID
	At      string `json:"at"`      // RFC 3339 UTC
	Level   string `json:"level"`   // info | warn | error
	Code    string `json:"code"`    // a stable i18n key
	Message string `json:"message"`
	Detail  any    `json:"detail"`
}

func (h *TaskHandlers) ListTaskEvents(ctx context.Context, in *ListTaskEventsInput) (*ListTaskEventsOutput, error)
```

## Steps
1. Add the code constants above to `internal/store/events.go`, each with a doc comment naming the moment
   it is emitted.
2. Add `ListEvents` with an explicit column list, `ORDER BY at DESC, id DESC` and the same base64 cursor
   codec used by `ListTasks`.
3. Create `internal/api/tasks_events.go` with the structs above and the handler, converting `at` from Unix
   milliseconds to an RFC 3339 UTC string at the API boundary.
4. Return `404` `/problems/not-found` for an unknown task id.
5. Unmarshal `detail_json` into `Detail`, emitting `null` when the column is `NULL`.
6. Register the operation in `internal/api/server.go` as `list-task-events`.
7. Confirm that `store.Transition` writes one row per accepted transition and add the missing emission
   points: `task.created` at insert, `engine.accepted` after the engine returns a handle, and
   `engine.unavailable` when it does not.
8. Add one i18next key per new code to `web/src/locales/en/tasks.json` in the same commit — that file is
   created by T052, so record the codes in this task's Evidence instead when it does not yet exist.
9. Create `internal/api/tasks_events_test.go`: create a task, run it to `completed`, and assert the event
   list is non-empty, ordered newest first, and contains `task.created` and `task.completed`.
10. Add a pagination case over 250 seeded events asserting the cursor walk returns each id exactly once.

## Acceptance criteria
- [ ] A task taken from creation to completion has a non-empty event list containing `task.created` and
      `task.completed`.
- [ ] Events are returned newest first and `total` counts every row for the task.
- [ ] A cursor walk over 250 events returns every id exactly once.
- [ ] `detail` is `null` when `detail_json` is `NULL` and a decoded object otherwise.
- [ ] An unknown task id returns `404` `/problems/not-found`.
- [ ] No code contains an interpolated value.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then `ok` lines for
`github.com/L-K-M/dl-tool/internal/store` and `github.com/L-K-M/dl-tool/internal/api`, with
`TestEventLogCoversLifecycle` and `TestEventCursorWalksEveryRowOnce` running. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add the system log endpoint; T096 owns `GET /system/logs`.
- Do NOT add notification delivery from these codes; T077 and T106 own channels.
- Do NOT add post-processing codes such as `postprocess.extract.failed`; T074 owns them.
- Do NOT add a retention sweep over `task_events`.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked

**Step 7's emission points and the `ListEvents` envelope cannot be built from the Files table: T017
and T020 already shipped the pieces this task was written to add, against different shapes.**

1. **`ListEvents` already exists with a different signature.** T017 shipped
   `ListEvents(ctx, taskID, cursor EventCursor, limit int) ([]TaskEvent, error)` plus tests in
   `internal/store/tasks_test.go` that pin it at five call sites (`TestTransitionWritesEvent`,
   `TestListEventsPagination`). This task's interface contract requires the cursor-envelope shape
   `ListEvents(ctx, taskID, limit, cursor string) (items, nextCursor, total, err)`; migrating breaks
   the compilation of `internal/store/tasks_test.go`, which no Files-table row covers.
2. **`task.created` has no emission point in the table.** Step 7 and acceptance criterion 1 require
   one row at insert. The insert runs in `TaskStore.Create` (`internal/store/tasks.go`) called by
   `insertPlanned` (`internal/api/tasks.go`); neither file is in the table, and no in-table file sits
   on the create path.
3. **`engine.accepted` has no emission point either.** The store's handle-recording moment is
   `SetEngineRef` (`internal/store/tasks.go`, outside the table). `engine.unavailable`/`engine.rejected`
   have **no call site at all in M1**: POST /tasks deliberately contacts no engine (the admission pass
   T098 owns `Engine.Add`), so there is no engine refusal moment to hook. Those two codes ship as
   constants now and their emission points arrive with T098.

**Minimal amendment:** add to the Files table —
`internal/api/tasks.go | edit | Emit task.created (store.CodeTaskCreated) in insertPlanned after the
insert of each accepted URI.`, `internal/store/tasks.go | edit | Emit engine.accepted
(store.CodeEngineAccepted) inside SetEngineRef, transactionally with the handle write.` and
`internal/store/tasks_test.go | edit | Migrate the ListEvents call sites to the envelope signature;
cover the SetEngineRef emission.` Nothing else changes: the API-level `task.created` home matches the
conventions table ("`task.` — Task lifecycle in `internal/api` and `internal/store`") and keeps the
emission out of the store-level fixtures the actions and delete tests seed through `store.Create`.
