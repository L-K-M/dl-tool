# T111 — Delete downloaded data safely and prove hardlink survival

| Field | Value |
|---|---|
| **ID** | T111 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T023, T076 |
| **Blocks** | — |
| **Parallel-safe** | no — extends `internal/api/tasks_delete.go` and `internal/api/tasks_actions.go` |
| **Implements** | [FR-024](../02-requirements.md#fr-024-delete-downloaded-data-safely-and-only-on-request) |
| **Decisions** | [ADR-0012](../decisions/0012-single-data-mount.md), [ADR-0017](../decisions/0017-exclusive-control-of-engines.md) |
| **Est. size** | 2 new files, ~300 LOC |

## Goal
`delete_data=true` is the only irreversible operation in the product, so it runs one shared six-step
executor for both the single delete and the bulk action: stop at the engine, enumerate only `task_files`,
re-check every path, unlink, record, delete the row. A hardlinked library copy survives untouched.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §5.6 `DELETE /tasks/{id}`](../05-api-contract.md#56-delete-tasksid)
2. [`docs/05-api-contract.md` §5.7 `POST /tasks/actions`](../05-api-contract.md#57-post-tasksactions)
3. [`docs/02-requirements.md` FR-024](../02-requirements.md#fr-024-delete-downloaded-data-safely-and-only-on-request)
4. [`docs/05-api-contract.md` §7.2 Containment](../05-api-contract.md#72-containment)
5. [`docs/tasks/T023-remove-task-and-data.md`](T023-remove-task-and-data.md)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/fsx/delete.go` | create | The six-step executor and its result. |
| `internal/fsx/delete_test.go` | create | Hardlink, escape, missing-file and ordering cases. |
| `internal/api/tasks_delete.go` | modify | Call the executor instead of the inline sequence. |
| `internal/api/tasks_actions.go` | modify | Run the same executor per id for a bulk `remove`. |
| `internal/api/tasks_delete_test.go` | modify | Seeding-task, containment and event-row cases. |

No other file may be modified.

## Interface contract

```go
package fsx

// DeleteResult is the body of DELETE /tasks/{id}, doc 05 §5.6.
type DeleteResult struct {
	Deleted       bool  `json:"deleted"`
	DeleteData    bool  `json:"delete_data"`
	FilesUnlinked int   `json:"files_unlinked"`
	BytesUnlinked int64 `json:"bytes_unlinked"`
	Missing       int   `json:"missing"` // a recorded file that no longer exists is not an error
}

// Target is one recorded file: a task_files row resolved against tasks.destination. The executor
// is given targets; it never globs, never walks a directory and never uses content_path alone.
type Target struct {
	Path  string
	Bytes int64
}

// DeleteData performs steps 3 and 4 of doc 05 §5.6 and nothing else. Steps 1, 5 and 6 belong to
// the caller, in that order, and the caller must not reorder them.
//
// Step 3 runs to completion BEFORE any unlink: every target is resolved, symlinks included, and
// must lie inside one of the configured roots. One failing target aborts the whole request with
// ErrPathRejected naming it and NOTHING AT ALL is unlinked.
//
// Step 4 is one unlink(2) per recorded file, then a single attempt to remove the task's own
// directory, which succeeds only while it is empty. A non-empty directory is left in place.
//
// Hardlinks are expected and safe: a file dl-tool downloaded and then hardlinked into a media
// library is one inode with two names, so unlinking dl-tool's name leaves the library copy
// byte-for-byte intact. This is the normal outcome of the single /data mount. Do not warn about
// it, do not detect it, and do not refuse the delete because of it.
func DeleteData(ctx context.Context, roots []string, taskDir string, targets []Target) (DeleteResult, error)
```

The caller's obligations, in order, with the failure behaviour that must be tested:

| # | Step | On failure |
|---|---|---|
| 1 | Stop the task at its engine and drop the handle | `503` `/problems/engine-unavailable`; the task is **not** deleted and no file is unlinked |
| 2 | Enumerate `task_files` into `[]Target` | — |
| 3 | `DeleteData` re-checks every path | `403` `/problems/path-rejected`; nothing is unlinked |
| 4 | `DeleteData` unlinks | — |
| 5 | One `task_events` row, `level:"warn"`, `code:"task.data_deleted"`, file count and byte total in `detail` | Written **before** the response |
| 6 | Delete the `tasks` row; children cascade | — |

`delete_data=false`, the default, unlinks nothing at all: the row goes and every byte stays.

## Steps
1. Create `internal/fsx/delete.go` with `DeleteResult`, `Target` and `DeleteData`.
2. Implement step 3 as a complete pass over every target before the first unlink, reusing the containment
   check of `fsx.ResolveDestination` so the roots check is the same code everywhere.
3. Implement step 4: one `unlink(2)` per target, counting a missing file into `Missing` rather than failing,
   then one `os.Remove` of the task directory that is allowed to fail when the directory is not empty.
4. Edit `internal/api/tasks_delete.go` to call the executor between its existing step 1 and step 5, deleting
   the inline path logic it replaces, and to keep the six steps in the documented order.
5. Edit `internal/api/tasks_actions.go` so a bulk `remove` with `delete_data` runs the same executor for
   every id, reporting per-id outcomes and never failing the batch on one bad id.
6. Ensure a `seeding` task is stopped at its engine before any unlink, and that an unreachable engine
   aborts with `503` leaving the task and its files intact.
7. Create `internal/fsx/delete_test.go`: create a file, hardlink it into a second directory, delete through
   the executor and assert the hardlinked copy still opens with its original contents; assert a target
   resolving outside the roots aborts the whole call with `ErrPathRejected` and unlinks nothing, including
   the valid targets in the same batch; assert a missing recorded file is counted in `Missing`; assert a
   non-empty task directory survives; assert no directory walk or glob is performed by planting an
   unrecorded file in the task directory and asserting it survives.
8. Extend `internal/api/tasks_delete_test.go`: assert the engine stop precedes every unlink; assert an
   unreachable engine yields `503` with the task still present; assert the `task.data_deleted` event row
   exists before the response; assert `delete_data=false` unlinks nothing.
9. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] A hardlinked copy elsewhere opens with its original contents after the delete.
- [ ] One target outside the roots aborts the whole request; nothing at all is unlinked.
- [ ] Only paths recorded in `task_files` are unlinked; an unrecorded file in the same directory survives.
- [ ] A missing recorded file is counted in `missing` and is not an error.
- [ ] The engine stop happens before the first unlink; an unreachable engine yields `503` and deletes nothing.
- [ ] A `task_events` row with `code:"task.data_deleted"` is written before the response.
- [ ] `delete_data=false` unlinks nothing.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/fsx/... ./internal/api/..." && echo DELETE_DATA_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/fsx` and `ok  github.com/L-K-M/dl-tool/internal/api`, with
`TestHardlinkedCopySurvives`, `TestOneEscapingTargetAbortsAll`, `TestOnlyRecordedFilesUnlinked`,
`TestMissingFileCounted`, `TestEngineStoppedBeforeUnlink`, `TestUnreachableEngineDeletesNothing` and
`TestDataDeletedEventWritten` each reported as `--- PASS`. The final line of stdout is exactly
`DELETE_DATA_OK`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT glob, walk a directory, or delete from `content_path` alone. `task_files` is the only source of
  targets, and that is what keeps this operation bounded.
- Do NOT detect, warn about, or refuse a delete because a file has more than one link. A surviving library
  copy is the intended outcome of the single `/data` mount, not a partial deletion.
- Do NOT delete a transfer dl-tool did not create, or data it did not record; ADR-0017 leaves them alone.
- Do NOT remove a non-empty task directory recursively.
- Do NOT delete anything on `ENOSPC` or on an extraction failure; T099 and T074 both pause instead.
- Do NOT add a "delete all completed" or "empty trash" endpoint.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
