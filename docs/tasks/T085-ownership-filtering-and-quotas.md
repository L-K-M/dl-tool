# T085 — Scope tasks to their owner and enforce the storage quota

| Field | Value |
|---|---|
| **ID** | T085 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T020, T021, T022, T084 |
| **Blocks** | T086, T109, T111 |
| **Parallel-safe** | no — extends `internal/api/tasks.go` and `internal/store/tasks_list.go` |
| **Implements** | [FR-119](../02-requirements.md#fr-119-filter-tasks-by-owner-for-non-admins), [FR-121](../02-requirements.md#fr-121-enforce-the-per-user-storage-quota), [FR-122](../02-requirements.md#fr-122-re-check-the-storage-quota-when-metadata-resolves) |
| **Decisions** | [ADR-0013](../decisions/0013-mandatory-built-in-authentication.md) |
| **Est. size** | 0 new files, ~330 LOC |

## Goal
A non-admin sees only their own tasks from every task read endpoint and gets `404` — never `403` — when
acting on someone else's. Creating a task that would push the owner's `SUM(total_bytes)` above
`users.quota_bytes` is rejected with `quota_exceeded`; a magnet whose resolved size does that afterwards is
**paused**, never deleted.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §5.11 Quotas and concurrency limits](../05-api-contract.md#511-quotas-and-concurrency-limits)
2. [`docs/04-data-model.md` §4.7 Storage quota versus concurrency limit](../04-data-model.md#47-storage-quota-versus-concurrency-limit)
3. [`docs/02-requirements.md` FR-119](../02-requirements.md#fr-119-filter-tasks-by-owner-for-non-admins)
4. [`docs/02-requirements.md` FR-122](../02-requirements.md#fr-122-re-check-the-storage-quota-when-metadata-resolves)
5. [`docs/04-data-model.md` §3.3 Tasks](../04-data-model.md#33-tasks)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/store/tasks.go` | modify | Add `RecheckQuota` and call it when `total_bytes` first resolves. |
| `internal/store/tasks_list.go` | modify | Force the owner predicate for a non-admin caller. |
| `internal/api/tasks.go` | modify | Pass the caller identity into every read; map a quota refusal. |
| `internal/api/tasks_actions.go` | modify | Return `404` for an action on another owner's task. |
| `internal/api/tasks_test.go` | modify | Isolation, quota and metadata-recheck cases. |

No other file may be modified.

## Interface contract

```go
package store

// Scope is the ownership predicate applied to every task read. An admin scope selects every row;
// a user scope selects only that user's rows. There is no "all" option for a non-admin.
type Scope struct {
	UserID string
	Admin  bool
}

// ListTasks (T021) takes Scope as its first filter and ANDs owner_id = :user_id when Admin is false.
// The predicate is applied in SQL, never by filtering a fetched slice.
func (s *TaskStore) ListTasks(ctx context.Context, sc Scope, f Filter) ([]Task, string, error)

// Get returns ErrNotFound — not a permission error — when the row exists but belongs to someone
// else, so a non-admin cannot probe for the existence of another user's task id.
func (s *TaskStore) Get(ctx context.Context, sc Scope, id string) (Task, error)

// RecheckQuota re-evaluates the owner's storage quota for one task and reports the outcome. It is
// called by the engine-progress applier the first time tasks.total_bytes changes from NULL to a
// value, and by nothing else.
//
// When the resolved size puts the owner over quota_bytes it sets state 'paused' and error_code
// 'quota_exceeded', writes a task_events row and returns exceeded true. It NEVER deletes the task
// and NEVER unlinks a byte: the downloaded data stays on disk and the owner or an admin resolves it.
func (s *TaskStore) RecheckQuota(ctx context.Context, taskID string) (exceeded bool, err error)

// UsedBytes is SUM(total_bytes) over the owner's tasks whose state is not 'removed'.
func (s *TaskStore) UsedBytes(ctx context.Context, ownerID string) (int64, error)
```

Quota arithmetic, restated once as code because it is the part that is easy to get wrong:

```
allowed := user.QuotaBytes == 0 || used+incoming <= user.QuotaBytes
```

| Situation | HTTP | Task |
|---|---|---|
| `POST /tasks` would exceed | `403` `/problems/quota-exceeded`; the URI appears in `rejected[]` | Never created |
| Resolved metadata exceeds | — | `state: "paused"`, `error_code: "quota_exceeded"`, data kept |
| `quota_bytes = 0` | — | Unlimited |
| Another owner's task, any verb | `404` | Untouched |

`quota_exceeded` is a **storage** outcome; `concurrency_limit` is the separate `max_active_*` outcome owned
by [T098](T098-concurrency-limiter.md). Never conflate the two codes.

## Steps
1. Add `Scope` to `internal/store/tasks_list.go` and thread it into `ListTasks`, ANDing
   `owner_id = :user_id` in SQL when `Admin` is false.
2. Add the same predicate to `Get`, `ListFiles`, `ListEvents`, trackers and peers reads in
   `internal/store/tasks.go`, returning `ErrNotFound` rather than a permission error.
3. Add `UsedBytes` and `RecheckQuota` to `internal/store/tasks.go`, with `RecheckQuota` writing
   `state='paused'`, `error_code='quota_exceeded'` and one `task_events` row in a single transaction.
4. Call `RecheckQuota` from the function that applies an engine `TaskInfo` to a row, exactly when
   `total_bytes` moves from `NULL` to a value; never on a later size change.
5. Edit `internal/api/tasks.go` to build `Scope` from the request identity installed by T008 and pass it to
   every task read, and to map a creation-time quota refusal to `403` `/problems/quota-exceeded` with the
   URI in `rejected[]`.
6. Edit `internal/api/tasks_actions.go` so a `PATCH`, `DELETE` or `POST /tasks/actions` id owned by someone
   else yields `404` for the single-id verbs and a per-id `ok:false` with `/problems/not-found` in the batch.
7. Extend `internal/api/tasks_test.go`: create tasks for two non-admins and assert each sees only their own
   while an admin sees both; assert a cross-owner pause is `404`; set `quota_bytes` to `1073741824`, create
   a 700 MiB task and assert the second is `403` `/problems/quota-exceeded`; assert `quota_bytes = 0` is
   unlimited; resolve a magnet's size above the quota and assert the task is `paused` with `quota_exceeded`,
   still present, and still present after a store reopen.
8. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] A non-admin's `GET /tasks` returns only their own rows; an admin's returns every row.
- [ ] A cross-owner action returns `404`, never `403`, from every task endpoint.
- [ ] The owner predicate is in the SQL, not applied to a fetched slice.
- [ ] A second task breaching `quota_bytes` is `403` `/problems/quota-exceeded` and is never created.
- [ ] `quota_bytes = 0` means unlimited.
- [ ] A magnet resolving above the quota is `paused` with `quota_exceeded`, survives a restart, and keeps
      every downloaded byte.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/api/... ./internal/store/..." && echo OWNERSHIP_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/api` and `ok  github.com/L-K-M/dl-tool/internal/store`,
with `TestNonAdminSeesOwnTasksOnly`, `TestCrossOwnerActionIs404`, `TestQuotaRejectsSecondTask`,
`TestZeroQuotaUnlimited`, `TestQuotaRecheckPausesNeverDeletes` and `TestPausedQuotaTaskSurvivesRestart`
each reported as `--- PASS`. The final line of stdout is exactly `OWNERSHIP_OK`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement the per-user filesystem jail; T109 owns it.
- Do NOT implement `max_active_per_user` or the `by_user_round_robin` process order; T098 and T086 own them.
- Do NOT delete, truncate or move any data when a quota is breached — ever, by any code path.
- Do NOT add `/users` CRUD; T086 owns it.
- Do NOT return `403` for another owner's task; that leaks the existence of the id.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
