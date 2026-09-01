# T021 — List, filter and sort tasks

| Field | Value |
|---|---|
| **ID** | T021 |
| **Milestone** | M1 |
| **Status** | todo |
| **Depends on** | T017, T020 |
| **Blocks** | T022, T025, T039 |
| **Parallel-safe** | no — extends the shared `internal/api/tasks.go` |
| **Implements** | [FR-012](../02-requirements.md#fr-012-list-and-filter-tasks), [FR-013](../02-requirements.md#fr-013-resolve-the-sidebar-filter-sets) |
| **Decisions** | [ADR-0003](../decisions/0003-chi-huma-code-first-openapi.md), [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md) |
| **Est. size** | 1 new file, ~330 LOC |

## Goal
`GET /api/v1/tasks` returns cursor-paginated Task objects filtered by state, sidebar filter, category, tag,
owner and a name substring, sorted by any documented column. `GET /api/v1/tasks/{id}` returns one Task and
`404` for an unknown id or another user's task.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §5.1 `GET /tasks`](../05-api-contract.md#51-get-tasks)
2. [`docs/05-api-contract.md` §5.4 `GET /tasks/{id}`](../05-api-contract.md#54-get-tasksid)
3. [`docs/05-api-contract.md` §1.4 Cursor pagination](../05-api-contract.md#14-cursor-pagination)
4. [`docs/04-data-model.md` §4.1 `tasks.state`](../04-data-model.md#41-tasksstate)
5. [`docs/14-conventions.md` §2.4 SQL and sqlx](../14-conventions.md#24-sql-and-sqlx)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/store/tasks_list.go` | create | `ListTasks`, the filter struct, the sort allowlist and the cursor codec. |
| `internal/store/tasks_test.go` | modify | Filter, sort and pagination cases over a seeded database. |
| `internal/api/tasks.go` | modify | The `GET /tasks` and `GET /tasks/{id}` handlers. |
| `internal/api/tasks_test.go` | modify | `humatest` cases for each sidebar filter and for a stale cursor. |
| `internal/api/server.go` | modify | Register `list-tasks` and `get-task`. |

No other file may be modified.

## Interface contract

```go
package store

// TaskFilter is the parsed query of GET /tasks. An empty field means "no constraint"; Category and
// Tag set to the empty string with the matching Has flag select uncategorised and untagged tasks.
type TaskFilter struct {
	State       string // a canonical state, or a sidebar filter name
	Category    string
	HasCategory bool
	Tag         string
	HasTag      bool
	OwnerID     string
	Query       string // case-insensitive substring of name
	Sort        string // a column from the allowlist, optionally prefixed with '-'
	Limit       int    // 1..500, default 100
	Cursor      string // opaque; bound to the filter and sort that produced it
}

// ListTasks returns one page plus the next cursor and the total matching the filter, ignoring the
// cursor. ErrStaleCursor is returned when the cursor was issued for a different filter or sort.
func (s *TaskStore) ListTasks(ctx context.Context, f TaskFilter) (items []Task, nextCursor string, total int, err error)

// ErrStaleCursor is returned when a cursor does not belong to the supplied filter and sort.
var ErrStaleCursor = errors.New("store: cursor does not match this filter")
```

Sidebar filter sets, resolved in SQL and nowhere else:

| Filter | States |
|---|---|
| `all` | every state except `removed` |
| `downloading` | `downloading` |
| `completed` | `completed`, `seeding` |
| `active` | `downloading`, `seeding` |
| `inactive` | `error`, `queued`, `paused` |
| `stopped` | `paused` |
| `error` | `error` |

Sort allowlist, exactly these and no others, each accepting a leading `-`:
`added_at`, `completed_at`, `name`, `total_bytes`, `progress`, `state`, `download_rate`, `upload_rate`,
`eta_seconds`, `ratio`, `queue_position`. The default is `-added_at`.

## Steps
1. Create `internal/store/tasks_list.go` with `TaskFilter`, `ErrStaleCursor` and `ListTasks`.
2. Build the `WHERE` clause from bound `?` placeholders only; never concatenate a value into SQL.
3. Encode the sidebar filter sets as a `map[string][]string` and expand the selected one into an
   `IN (?, ?, …)` clause with `sqlx.In`.
4. Validate `Sort` against the allowlist before it reaches the query and return
   `/problems/validation-failed` upstream for anything else. `progress` sorts on
   `CAST(completed_bytes AS REAL) / NULLIF(total_bytes, 0)`.
5. Encode the cursor as base64 JSON holding the last row's sort value, its `id`, and a hash of the filter
   and sort; reject a mismatching hash with `ErrStaleCursor`.
6. Return `total` from a second `COUNT(*)` over the same filter without the cursor predicate.
7. Add the `GET /tasks` and `GET /tasks/{id}` handlers to `internal/api/tasks.go`, mapping
   `store.ErrNotFound` to `404` `/problems/not-found` and `store.ErrStaleCursor` to `422`
   `/problems/validation-failed`.
8. Force `OwnerID` to the caller for a non-admin, and ignore a supplied `owner` query for them, so another
   user's task returns `404` rather than `403`.
9. Register both operations in `internal/api/server.go` as `list-tasks` and `get-task`.
10. Extend `internal/store/tasks_test.go`: seed one task in each of the ten states and assert the
    membership of all seven filters; seed 5 000 tasks and assert the cursor walk returns every row exactly
    once with no duplicate and no gap.
11. Extend `internal/api/tasks_test.go` with a `422` case for `sort=owner`, a `422` case for a cursor
    reused under a different filter, and a `404` case for another user's task.

## Acceptance criteria
- [ ] Each of the seven sidebar filters returns exactly the states in the table above.
- [ ] A 5 000-row cursor walk returns every id exactly once.
- [ ] `total` counts the filter and ignores the cursor.
- [ ] `sort=owner` returns `422` `/problems/validation-failed`.
- [ ] A cursor replayed under a different filter returns `422`, never a wrong page.
- [ ] A non-admin requesting another user's task id receives `404` `/problems/not-found`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then `ok` lines for
`github.com/L-K-M/dl-tool/internal/store` and `github.com/L-K-M/dl-tool/internal/api`, with
`TestSidebarFilterSets`, `TestCursorWalksEveryRowOnce` and `TestListTasksRejectsUnknownSort` all running.
No `FAIL`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT add `PATCH /tasks/{id}` or `POST /tasks/actions`; T022 owns them.
- Do NOT add `DELETE /tasks/{id}`; T023 owns it.
- Do NOT add the `files`, `trackers` or `peers` sub-resources; T032, T034 and T035 own them.
- Do NOT add `offset` paging anywhere.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
