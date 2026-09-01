# T017 — Persist tasks and enforce the task state machine

| Field | Value |
|---|---|
| **ID** | T017 |
| **Milestone** | M1 |
| **Status** | todo |
| **Depends on** | T006, T016 |
| **Blocks** | T020, T021, T024, T050, T074, T098, T100 |
| **Parallel-safe** | yes — touches only `internal/store/` |
| **Implements** | infrastructure for [FR-011](../02-requirements.md#fr-011-maintain-the-canonical-task-state-machine); the engine half of FR-011 is verified by T018 |
| **Decisions** | [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md), [ADR-0015](../decisions/0015-db-backed-in-process-job-queue.md) |
| **Est. size** | 3 new files, ~400 LOC |

## Goal
`internal/store` inserts, reads and updates `tasks` rows through explicit column lists, and refuses any
state change that is not a legal transition of the ten-state machine. Every accepted transition writes one
`task_events` row in the same transaction.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/04-data-model.md` §3.3 Tasks](../04-data-model.md#33-tasks)
2. [`docs/04-data-model.md` §4.1 `tasks.state`](../04-data-model.md#41-tasksstate)
3. [`docs/04-data-model.md` §4.2 `tasks.error_code`](../04-data-model.md#42-taskserror_code)
4. [`docs/03-architecture.md` §8.1 Task state machine](../03-architecture.md#81-task-state-machine)
5. [`docs/14-conventions.md` §2.4 SQL and sqlx](../14-conventions.md#24-sql-and-sqlx)
6. [`docs/14-conventions.md` §4 The `task_events` code vocabulary](../14-conventions.md#4-the-task_events-code-vocabulary)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/store/tasks.go` | create | Task CRUD, the transition table and `Transition`. |
| `internal/store/events.go` | create | `AppendEvent` and `ListEvents` over `task_events`. |
| `internal/store/tasks_test.go` | create | Transition table test plus round-trip against a temporary database. |
| `internal/store/models.go` | modify | Add the `Task` and `TaskEvent` row structs. |

No other file may be modified.

## Interface contract

```go
package store

// Task is one row of the tasks table. Column names and types are owned by docs/04-data-model.md §3.3.
type Task struct {
	ID             string  `db:"id"              json:"id"`
	OwnerID        string  `db:"owner_id"        json:"owner_id"`
	Engine         string  `db:"engine"          json:"engine"`
	EngineRef      *string `db:"engine_ref"      json:"engine_ref"`
	SourceKind     string  `db:"source_kind"     json:"source_kind"`
	SourceURI      *string `db:"source_uri"      json:"source_uri"`
	Name           string  `db:"name"            json:"name"`
	InfohashV1     *string `db:"infohash_v1"     json:"infohash_v1"`
	InfohashV2     *string `db:"infohash_v2"     json:"infohash_v2"`
	State          string  `db:"state"           json:"state"`
	ErrorCode      *string `db:"error_code"      json:"error_code"`
	ErrorMessage   *string `db:"error_message"   json:"error_message"`
	Destination    string  `db:"destination"     json:"destination"`
	ContentPath    *string `db:"content_path"    json:"content_path"`
	CategoryID     *string `db:"category_id"     json:"category_id"`
	TotalBytes     *int64  `db:"total_bytes"     json:"total_bytes"`
	CompletedBytes int64   `db:"completed_bytes" json:"completed_bytes"`
	UploadedBytes  int64   `db:"uploaded_bytes"  json:"uploaded_bytes"`
	DownloadRate   int64   `db:"download_rate"   json:"download_rate"`
	UploadRate     int64   `db:"upload_rate"     json:"upload_rate"`
	ETASeconds     *int64  `db:"eta_seconds"     json:"eta_seconds"`
	Sequential     int     `db:"sequential"      json:"sequential"`
	QueuePosition  *int64  `db:"queue_position"  json:"queue_position"`
	AddedAt        int64   `db:"added_at"        json:"added_at"`
	StartedAt      *int64  `db:"started_at"      json:"started_at"`
	CompletedAt    *int64  `db:"completed_at"    json:"completed_at"`
	CreatedAt      int64   `db:"created_at"      json:"created_at"`
	UpdatedAt      int64   `db:"updated_at"      json:"updated_at"`
}

// ErrNotFound is returned when no task with the given id exists.
var ErrNotFound = errors.New("store: task not found")

// ErrIllegalTransition is returned when a state change is not in the transition table.
var ErrIllegalTransition = errors.New("store: illegal state transition")

// Progress carries the mutable transfer counters an engine poll produces.
type Progress struct {
	TotalBytes     *int64
	CompletedBytes int64
	UploadedBytes  int64
	DownloadRate   int64
	UploadRate     int64
	ETASeconds     *int64
}

type TaskStore struct{ db *sqlx.DB }

func NewTaskStore(db *sqlx.DB) *TaskStore

func (s *TaskStore) Create(ctx context.Context, t Task) (Task, error)
func (s *TaskStore) Get(ctx context.Context, id string) (Task, error)
func (s *TaskStore) UpdateProgress(ctx context.Context, id string, p Progress) error
func (s *TaskStore) SetEngineRef(ctx context.Context, id, engineRef string) error

// Transition moves a task to next, writing one task_events row with code and message in the same
// transaction. It returns ErrIllegalTransition and mutates nothing when the move is not permitted.
func (s *TaskStore) Transition(ctx context.Context, id, next, code, message string) error

// AppendEvent inserts one task_events row. level is "info", "warn" or "error"; code is a dotted
// vocabulary key from docs/14-conventions.md §4, never a formatted value.
func (s *TaskStore) AppendEvent(ctx context.Context, taskID, level, code, message string, detail any) error
```

Legal transitions — the table `Transition` consults, plus the three universal rules of
[`03-architecture.md` §8.1](../03-architecture.md#81-task-state-machine): every state may move to `paused`,
to `error` and to `removed`. The schedule's `No Download` cell pauses tasks in `checking` as well as in
`downloading` and `queued` (T081), so `checking` → `paused` must be legal.

| From | To |
|---|---|
| `queued` | `downloading` |
| `downloading` | `checking`, `seeding`, `extracting`, `moving` |
| `checking` | `downloading` |
| `paused` | `downloading`, `queued` |
| `seeding` | `completed` |
| `extracting` | `moving` |
| `moving` | `completed`, `seeding` |
| `error` | `queued` |

## Steps
1. Add `Task` and `TaskEvent` to `internal/store/models.go` with both `db` and `json` tags, exactly the
   columns of [§3.3](../04-data-model.md#33-tasks).
2. Create `internal/store/tasks.go` with `ErrNotFound`, `ErrIllegalTransition`, `TaskStore` and
   `NewTaskStore`.
3. Write `Create` as one `INSERT` with an explicit column list and `?` placeholders; set `created_at`,
   `updated_at` and `added_at` to the same Unix millisecond value.
4. Write `Get` with an explicit column list and map `sql.ErrNoRows` to `ErrNotFound` with `%w`.
5. Write `UpdateProgress` and `SetEngineRef` as targeted `UPDATE` statements that also bump `updated_at`.
6. Encode the transition table above as a `map[string][]string`, add the two universal rules, and implement
   `Transition` inside one `sqlx.Tx` with `defer tx.Rollback()` before the commit.
7. Create `internal/store/events.go` with `AppendEvent` and a cursor-paginated `ListEvents` ordered by
   `at DESC`, marshalling `detail` into `detail_json`.
8. Create `internal/store/tasks_test.go`: build a temporary database from the migrations, insert one task
   and assert every column round-trips.
9. Add a table test asserting each legal pair above succeeds, that `completed` to `downloading` returns
   `ErrIllegalTransition`, and that a rejected transition leaves `tasks.state` and the `task_events` count
   unchanged.
10. Assert that one accepted `Transition` writes exactly one `task_events` row carrying the supplied code.

## Acceptance criteria
- [ ] No statement in `internal/store/tasks.go` or `events.go` uses `SELECT *`.
- [ ] `Create` then `Get` returns every column unchanged, including the nullable ones as `nil`.
- [ ] Every pair in the transition table succeeds and every other pair returns `ErrIllegalTransition`.
- [ ] Any state may move to `paused`, to `error` and to `removed`.
- [ ] A rejected transition writes no `task_events` row.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/store/...
```
Expected: `make lint` prints nothing, then `ok  	github.com/L-K-M/dl-tool/internal/store` followed by its
elapsed time, with `TestTaskRoundTrip`, `TestTransitionTable` and `TestTransitionWritesEvent` all running.
No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add HTTP handlers; T020 owns `internal/api/tasks.go`.
- Do NOT add list, filter or cursor queries; T021 owns them.
- Do NOT add the `GET /tasks/{id}/events` endpoint; T024 owns it.
- Do NOT add a migration; the schema is created by M0 and changed only through a new goose file.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
