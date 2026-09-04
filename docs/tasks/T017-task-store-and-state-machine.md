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

// ErrNotFound is deliberately absent here: db.go already declares it for the whole package (T006).
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
to `error` and to `removed` — except `removed` itself, which is terminal. The schedule's `No Download` cell pauses tasks in `checking` as well as in
`downloading` and `queued` (T081), so `checking` → `paused` must be legal.

| From | To |
|---|---|
| `queued` | `downloading`, `checking`, `seeding`, `completed` |
| `downloading` | `queued`, `checking`, `seeding`, `completed` |
| `checking` | `queued`, `downloading`, `seeding`, `completed` |
| `paused` | `queued`, `downloading`, `checking`, `seeding`, `completed` |
| `seeding` | `queued`, `downloading`, `checking`, `completed`, `extracting`, `moving` |
| `completed` | `checking`, `seeding`, `extracting`, `moving` |
| `extracting` | `moving`, `completed` |
| `moving` | `completed`, `seeding` |
| `error` | `queued`, `completed` |

The first five rows exist because the reconciler adopts whatever the engine reports: every target state of
the normalisation tables in [`06-download-engines.md`](../06-download-engines.md) §4.6 and §5.6 must be
reachable from every state those tables can follow. `extracting` and `moving` are never engine-reported —
the post-processing chain (T074, T076) enters them from `completed` or `seeding` and leaves back to
`completed`. `error` → `completed` exists for T022's `force_complete`. `removed` is terminal: it is the
only state with no outgoing edge.

## Steps
1. Add `Task` and `TaskEvent` to `internal/store/models.go` with both `db` and `json` tags, exactly the
   columns of [§3.3](../04-data-model.md#33-tasks).
2. Create `internal/store/tasks.go` with `ErrIllegalTransition`, `TaskStore` and `NewTaskStore`. Reuse the
   package-level `ErrNotFound` that T006 declares in `internal/store/db.go`; never declare a second one.
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
9. Add a table test asserting each legal pair above succeeds, that every transition out of `removed`
   returns `ErrIllegalTransition`, and that a rejected transition leaves `tasks.state` and the
   `task_events` count unchanged. Include one case per target state of
   [`06-download-engines.md`](../06-download-engines.md) §4.6 and §5.6 reached from `downloading`,
   `seeding`, `paused` and `completed`.
10. Assert that one accepted `Transition` writes exactly one `task_events` row carrying the supplied code.

## Acceptance criteria
- [ ] No statement in `internal/store/tasks.go` or `events.go` uses `SELECT *`.
- [ ] `Create` then `Get` returns every column unchanged, including the nullable ones as `nil`.
- [ ] Every pair in the transition table succeeds and every other pair returns `ErrIllegalTransition`.
- [ ] Any state except `removed` may move to `paused`, to `error` and to `removed`; `removed` has no
      outgoing edge.
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
`make lint && make test PKG=./internal/store/...`, re-run after the review fix (the
compare-and-swap guard in `Transition`) on the working tree of the HEAD commit:

```
$ make lint
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!

$ make test PKG=./internal/store/...
go test -race -count=1 ./internal/store/...
ok  	github.com/L-K-M/dl-tool/internal/store	9.292s
```

`make lint` printed no findings (`0 issues.`, eslint and prettier silent). One `ok` line, no `FAIL`.
With `-v`, `TestTaskRoundTrip`, `TestTransitionTable`, `TestTransitionWritesEvent`, `TestTaskMutators`
and `TestListEventsPagination` all run and pass (105 subtests: 3 round-trip cases, the exhaustive
100-pair transition matrix, the missing-task case and the stale-state guard case):

```
$ go test -race -count=1 -v ./internal/store/... 2>&1 | grep -Ec '^=== RUN[[:space:]]+Test(TaskRoundTrip|TransitionTable|TransitionWritesEvent|TaskMutators|ListEventsPagination)/'
105
```

Scope check: `git status` run with the four task files staged for the first commit, and the
branch-level check after the review fix:

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
internal/store/events.go
internal/store/models.go
internal/store/tasks.go
internal/store/tasks_test.go

$ git diff --name-only origin/main -- . ':(exclude)docs' | sort
internal/store/events.go
internal/store/models.go
internal/store/tasks.go
internal/store/tasks_test.go
```


## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
