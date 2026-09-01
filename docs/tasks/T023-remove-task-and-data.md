# T023 — Remove a task with or without its data

| Field | Value |
|---|---|
| **ID** | T023 |
| **Milestone** | M1 |
| **Status** | todo |
| **Depends on** | T020, T021, T022 |
| **Blocks** | T024, T044, T111 |
| **Parallel-safe** | no — extends the shared `internal/api/tasks.go` |
| **Implements** | [FR-015](../02-requirements.md#fr-015-remove-a-task-with-or-without-its-data) |
| **Decisions** | [ADR-0012](../decisions/0012-single-data-mount.md), [ADR-0017](../decisions/0017-exclusive-control-of-engines.md) |
| **Est. size** | 2 new files, ~300 LOC |

## Goal
`DELETE /api/v1/tasks/{id}` removes the task row and, with `delete_data=true`, unlinks exactly the files
recorded in `task_files` after re-checking every resolved path. The response reports what happened, so a
client never has to guess.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §5.6 `DELETE /tasks/{id}`](../05-api-contract.md#56-delete-tasksid)
2. [`docs/04-data-model.md` §3.3 Tasks](../04-data-model.md#33-tasks)
3. [`docs/12-security-and-threat-model.md` §3 Path safety](../12-security-and-threat-model.md#3-path-safety)
4. [`docs/14-conventions.md` §4 The `task_events` code vocabulary](../14-conventions.md#4-the-task_events-code-vocabulary)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/tasks_delete.go` | create | The `DELETE /tasks/{id}` handler and the six-step sequence. |
| `internal/api/tasks_delete_test.go` | create | Cases for both flags, an escaping path and an unreachable engine. |
| `internal/store/tasks.go` | modify | Add `Delete` and `ListFiles` for the recorded targets. |
| `internal/api/server.go` | modify | Register `delete-task`. |

No other file may be modified.

## Interface contract

```go
package api

// DeleteTaskInput carries the two mutually exclusive query flags of docs/05-api-contract.md §5.6.
type DeleteTaskInput struct {
	ID            string `path:"id"`
	DeleteData    bool   `query:"delete_data"`
	ForceComplete bool   `query:"force_complete"`
}

// DeleteTaskOutput reports the outcome. Missing counts files recorded in task_files that were
// already gone; that is not an error.
type DeleteTaskOutput struct {
	Body struct {
		Deleted       bool  `json:"deleted"`
		DeleteData    bool  `json:"delete_data"`
		FilesUnlinked int   `json:"files_unlinked"`
		BytesUnlinked int64 `json:"bytes_unlinked"`
		Missing       int   `json:"missing"`
	}
}

func (h *TaskHandlers) DeleteTask(ctx context.Context, in *DeleteTaskInput) (*DeleteTaskOutput, error)
```

The six steps, in this order and never approximated:

| # | Step | Rule |
|---|---|---|
| 1 | Stop the task at its engine | `Engine.Pause` then `Engine.Remove`; never unlink a file an engine still has open. |
| 2 | Enumerate the targets | Only `task_files` rows for this task, resolved against `tasks.destination`. Never a glob, never a directory walk, never `content_path` alone. |
| 3 | Re-check every path before unlinking any of them | Each resolved path, symlinks included, must lie inside `DLTOOL_DATA_ROOTS` and inside the caller's jail. One failing path aborts the whole request with `403` `/problems/path-rejected` and an `errors[]` entry naming it; nothing at all is unlinked. |
| 4 | Unlink | One `unlink(2)` per recorded file, then remove the task's own directory only while it is empty. |
| 5 | Record it | One `task_events` row, `level:"warn"`, `code:"task.data_deleted"`, with the file count and byte total in `detail`, written before the response. |
| 6 | Delete the row | `tasks`; `task_files`, `task_trackers` and `task_tags` go by cascade. |

Status codes: `200` with the body above · `404` `/problems/not-found` · `422`
`/problems/validation-failed` when both flags are true · `403` `/problems/path-rejected` from step 3 ·
`503` `/problems/engine-unavailable` when step 1 could not complete, in which case the task is **not**
deleted and no file is unlinked.

## Steps
1. Add `Delete` and `ListFiles` to `internal/store/tasks.go`, both with explicit column lists.
2. Create `internal/api/tasks_delete.go` with the input and output structs above.
3. Reject `delete_data=true` together with `force_complete=true` as `422` before any other work.
4. Implement step 1; on `engine.ErrUnavailable` return `503` and leave the row and every byte in place.
5. Implement step 2 by joining `task_files.path` onto `tasks.destination` with `filepath.Join`, and never
   by reading the filesystem.
6. Implement step 3 with `fsx.ResolveDestination` over every target, collecting failures first and
   aborting before the first unlink when any path fails.
7. Implement step 4, counting `files_unlinked`, `bytes_unlinked` and `missing`; a recorded file that no
   longer exists increments `missing` and is not an error.
8. Leave a non-empty task directory in place, and add the code comment that a hardlinked file's removal is
   expected and safe because the library copy survives.
9. Implement steps 5 and 6 in one transaction, with the `task_events` row written before the response.
10. Implement `force_complete=true` as a transition to `completed` with no unlink at all.
11. Create `internal/api/tasks_delete_test.go`: `delete_data=false` leaves every file on disk;
    `delete_data=true` removes exactly the recorded files; a `task_files` row escaping the root returns
    `403` and unlinks nothing; an unreachable engine returns `503` and deletes nothing; both flags returns
    `422`.

## Acceptance criteria
- [ ] `delete_data=false` removes the row and unlinks nothing.
- [ ] `delete_data=true` unlinks exactly the recorded files and reports the counts in the body.
- [ ] One escaping path aborts the request with `403` and leaves every file present.
- [ ] `503` from the engine leaves the task row present.
- [ ] Both flags together return `422`.
- [ ] A successful `delete_data=true` writes exactly one `task.data_deleted` event at level `warn`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/api/...
```
Expected: `make lint` prints nothing, then `ok  	github.com/L-K-M/dl-tool/internal/api` followed by its
elapsed time, with `TestDeleteKeepsData`, `TestDeleteUnlinksRecordedFiles`,
`TestDeleteRejectsEscapingPath` and `TestDeleteRefusesWhenEngineDown` all running. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add hardlink detection or a warning about it; the behaviour is intended.
- Do NOT delete a transfer dl-tool did not create; foreign transfers are invisible per
  [ADR-0017](../decisions/0017-exclusive-control-of-engines.md).
- Do NOT implement the retention sweep or automatic removal of completed tasks; T074 owns it.
- Do NOT extend the semantics beyond §5.6; T111 owns the full delete-data specification and its
  hardlink-safety tests.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
