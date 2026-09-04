# T023 — Remove a task with or without its data

| Field | Value |
|---|---|
| **ID** | T023 |
| **Milestone** | M1 |
| **Status** | done |
| **Depends on** | T020, T021, T022 |
| **Blocks** | T024, T044, T111 |
| **Parallel-safe** | no — extends the shared `internal/api/tasks.go` |
| **Implements** | [FR-015](../02-requirements.md#fr-015-remove-a-task-with-or-without-its-data) |
| **Decisions** | [ADR-0012](../decisions/0012-single-data-mount.md), [ADR-0017](../decisions/0017-exclusive-control-of-engines.md) |
| **Est. size** | 2 new files, ~300 LOC |

## Goal
`DELETE /api/v1/tasks/{id}` marks the task `removed` and, with `delete_data=true`, unlinks exactly the files
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
| `internal/store/tasks.go` | modify | Add `MarkRemoved` and `ListFiles` for the recorded targets. |
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
		Removed       bool  `json:"removed"`
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
| 1 | Enumerate the targets | Only `task_files` rows for this task, resolved against `tasks.destination`. Never a glob, never a directory walk, never `content_path` alone. |
| 2 | Validate every path before any side effect | Each resolved path, symlinks included, must lie inside `DLTOOL_DATA_ROOTS`. One failing path aborts the whole request with `403` `/problems/path-rejected` and an `errors[]` entry naming it; the engine and the filesystem stay untouched. |
| 3 | Remove the engine handle | `Engine.Pause` then `Engine.Remove`, which always instructs the engine to retain payload data; it must finish before any unlink so no file is still open. |
| 4 | Unlink | One `unlink(2)` per recorded file, then remove the task's own directory only while it is empty. |
| 5 | Record it | One `task_events` row, `level:"warn"`, `code:"task.data_deleted"`, with the file count and byte total in `detail`, written before the response. |
| 6 | Mark the tombstone | In the same transaction as step 5 set `state="removed"`, clear `engine_ref`, zero both rates and clear ETA. The task row and every child row are retained; only T091's retention sweep ever hard-deletes. |

Status codes: `200` with the body above · `404` `/problems/not-found` · `422`
`/problems/validation-failed` when both flags are true · `403` `/problems/path-rejected` from step 3 ·
`503` `/problems/engine-unavailable` when step 3 could not complete, in which case the task is **not**
deleted and no file is unlinked.

## Steps
1. Add `MarkRemoved` and `ListFiles` to `internal/store/tasks.go`, both with explicit column lists.
   `MarkRemoved` transitions to `removed` and never deletes a row.
2. Create `internal/api/tasks_delete.go` with the input and output structs above.
3. Reject `delete_data=true` together with `force_complete=true` as `422` before any other work.
4. Implement step 3; on `engine.ErrUnavailable` return `503` and leave the row and every byte in place.
5. Implement step 1 by joining `task_files.path` onto `tasks.destination` with `filepath.Join`, and never
   by reading the filesystem.
6. Implement step 2 with `fsx.ResolveDestination` over every target, collecting failures first and
   aborting before the first unlink when any path fails.
7. Implement step 4, counting `files_unlinked`, `bytes_unlinked` and `missing`; a recorded file that no
   longer exists increments `missing` and is not an error.
8. Leave a non-empty task directory in place, and add the code comment that a hardlinked file's removal is
   expected and safe because the library copy survives.
9. Implement steps 5 and 6 in one transaction, with the `task_events` row written before the response;
   the row is retained, so `GET /tasks/{id}/events` still answers after removal.
10. Implement `force_complete=true` as a transition to `completed` with no unlink at all.
11. Create `internal/api/tasks_delete_test.go`: `delete_data=false` leaves every file on disk;
    `delete_data=true` removes exactly the recorded files; a `task_files` row escaping the root returns
    `403` and unlinks nothing; an unreachable engine returns `503` and deletes nothing; both flags returns
    `422`.

## Acceptance criteria
- [x] `delete_data=false` leaves the task row in state `removed` and unlinks nothing.
- [x] `delete_data=true` unlinks exactly the recorded files and reports the counts in the body.
- [x] One escaping path aborts the request with `403` and leaves every file present.
- [x] `503` from the engine leaves the task row present.
- [x] Both flags together return `422`.
- [x] A successful `delete_data=true` writes exactly one `task.data_deleted` event at level `warn`.

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

`make lint && make test PKG=./internal/api/...`:

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
$ make test PKG=./internal/api/...
go test -race -count=1 ./internal/api/...
ok  	github.com/L-K-M/dl-tool/internal/api	36.417s
```

`go test -v -run 'TestDelete' ./internal/api/`:

```
=== RUN   TestDeleteKeepsData
--- PASS: TestDeleteKeepsData (0.06s)
=== RUN   TestDeleteUnlinksRecordedFiles
--- PASS: TestDeleteUnlinksRecordedFiles (0.03s)
=== RUN   TestDeleteRejectsEscapingPath
--- PASS: TestDeleteRejectsEscapingPath (0.02s)
=== RUN   TestDeleteRefusesWhenEngineDown
--- PASS: TestDeleteRefusesWhenEngineDown (0.03s)
=== RUN   TestDeleteRejectsBothFlags
--- PASS: TestDeleteRejectsBothFlags (0.04s)
=== RUN   TestDeleteForceComplete
--- PASS: TestDeleteForceComplete (0.02s)
ok  	github.com/L-K-M/dl-tool/internal/api	0.218s
```

Scope check:

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
api/openapi.json
internal/api/server.go
internal/api/tasks_delete.go
internal/api/tasks_delete_test.go
internal/store/tasks.go
web/src/api/schema.d.ts
```

The Files table's four paths plus `api/openapi.json` and `web/src/api/schema.d.ts`, the
implicitly-in-scope generated pair this task owes `make gen` for registering `delete-task`
(docs/13-testing-and-verification.md §7.1).

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
