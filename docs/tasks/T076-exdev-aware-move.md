# T076 — Move completed data across filesystems with progress

| Field | Value |
|---|---|
| **ID** | T076 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T074, T099 |
| **Blocks** | T111 |
| **Parallel-safe** | yes — adds `internal/fsx/move.go` and `internal/jobs/handlers_move.go` |
| **Implements** | [FR-103](../02-requirements.md#fr-103-move-completed-data-across-filesystems), [FR-045](../02-requirements.md#fr-045-pre-check-free-space-and-pause-on-exhaustion) |
| **Decisions** | [ADR-0012](../decisions/0012-single-data-mount.md), [ADR-0015](../decisions/0015-db-backed-in-process-job-queue.md) |
| **Est. size** | 3 new files, ~340 LOC |

## Goal
Moving a completed task's data to a path on another filesystem holds the task in `moving`, copies then
deletes rather than failing on `EXDEV`, verifies the copy before deleting the source, and reports progress.
A move onto the same filesystem stays a single `rename(2)`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/02-requirements.md` FR-103](../02-requirements.md#fr-103-move-completed-data-across-filesystems)
2. [`docs/10-deployment-and-compose.md`](../10-deployment-and-compose.md)
3. [`docs/04-data-model.md` §4.1 `tasks.state`](../04-data-model.md#41-tasksstate)
4. [`docs/tasks/T099-disk-space-reservation.md`](T099-disk-space-reservation.md)
5. [`docs/tasks/T074-auto-extract-archives.md`](T074-auto-extract-archives.md)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/fsx/move.go` | create | `Move`, the `EXDEV` copy-verify-delete fallback and its progress callback. |
| `internal/fsx/move_test.go` | create | Same-filesystem rename, `EXDEV` fallback, verify and abort cases. |
| `internal/jobs/handlers_move.go` | create | The `move` job handler: state, progress and error mapping. |
| `internal/jobs/postprocess.go` | modify | Enqueue `move` after `extract` when the destination differs. |

No other file may be modified.

## Interface contract

```go
package fsx

// Progress reports a running byte count during a cross-filesystem copy. It is called at most once
// per second and never with a decreasing copied value.
type Progress func(copied, total int64)

// Move relocates src to dst. Both paths must already have been resolved by ResolveDestination;
// Move performs no containment check of its own.
//
// It first attempts os.Rename. When that fails with EXDEV it falls back to copy-verify-delete:
//  1. walk src and sum the regular-file bytes into total;
//  2. copy every entry into a staging directory beside dst on dst's filesystem, preserving the
//     relative tree, files at 0644 and directories at 0755;
//  3. verify: every destination file exists with the same size as its source;
//  4. rename the staging directory onto dst, then remove src.
//
// Nothing under src is unlinked before step 3 succeeds. On any error the staging directory is
// removed and src is left byte-for-byte untouched.
func Move(ctx context.Context, src, dst string, onProgress Progress) error

// ErrVerifyFailed is returned when a copied file's size does not match its source. Nothing is deleted.
var ErrVerifyFailed = errors.New("fsx: copied tree does not match source")

// SameFilesystem reports whether a and b live on one filesystem, using FilesystemID from space.go.
func SameFilesystem(a, b string) (bool, error)
```

```go
package jobs

// MoveHandler runs job kind "move". Registered as worker.Register("move", h.Handle).
type MoveHandler struct{ /* store *store.Store */ }

func NewMoveHandler(st *store.Store) *MoveHandler

// Handle transitions the task to moving, calls fsx.Move, writes tasks.content_path to the new
// location on success and transitions back to completed. It is idempotent: a job whose src is
// already gone and whose dst already exists returns nil.
func (h *MoveHandler) Handle(ctx context.Context, job store.Job) error
```

Before the copy the handler calls `fsx.Reservation.Admits(total)` on the destination filesystem
([T099](T099-disk-space-reservation.md)). A refusal pauses the task with `error_code` `disk_full` and
enqueues nothing; a mid-copy `ENOSPC` does the same and deletes no partial data. `task_events` codes:
`postprocess.move.started`, `postprocess.move.completed`, `postprocess.move.failed`.

## Steps
1. Create `internal/fsx/move.go` with `Move`, `Progress`, `ErrVerifyFailed` and `SameFilesystem`.
2. Attempt `os.Rename` first and detect the cross-device case with `errors.Is(err, syscall.EXDEV)`; any
   other rename error is returned unchanged.
3. Implement the walk-and-sum step, then the copy into a staging directory named
   `.dl-tool-move-<ULID>` created beside `dst` so the final step is a rename on one filesystem.
4. Call `onProgress` at most once per second with the running copied byte count and the total.
5. Implement step 3's verification over every regular file, returning `ErrVerifyFailed` on the first
   mismatch and removing the staging directory before returning.
6. Delete `src` only after the staging rename has succeeded; on `ENOSPC` return `fsx.ErrDiskFull` and leave
   both trees in place.
7. Create `internal/jobs/handlers_move.go` with `MoveHandler`, the `moving` transition, the progress
   forwarding into the task row, the space pre-check and the error mapping above.
8. Edit `internal/jobs/postprocess.go` to enqueue a `move` job after `extract` when the resolved destination
   differs from the engine's save directory, and to skip it otherwise.
9. Create `internal/fsx/move_test.go`: assert a same-filesystem move is one rename and calls `onProgress`
   zero times; simulate `EXDEV` and assert the copy path runs, the destination bytes equal the source bytes
   and the source is gone; assert a truncated destination file yields `ErrVerifyFailed` with the source
   intact; assert a cancelled context leaves the source intact and removes the staging directory.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] A move within one filesystem is a single `os.Rename` and no bytes are copied.
- [ ] An `EXDEV` rename falls back to copy-verify-delete instead of failing.
- [ ] The source is deleted only after every destination file's size has been verified.
- [ ] `ErrVerifyFailed` leaves the source byte-for-byte untouched and removes the staging directory.
- [ ] The task passes through `moving` and returns to `completed` with `content_path` updated.
- [ ] A destination without room pauses the task with `disk_full` and deletes nothing.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/fsx/... ./internal/jobs/..." && echo MOVE_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/fsx` and `ok  github.com/L-K-M/dl-tool/internal/jobs`, with
`TestSameFilesystemUsesRename`, `TestEXDEVFallsBackToCopy`, `TestVerifyFailureKeepsSource`,
`TestCancelledMoveKeepsSource` and `TestTaskPassesThroughMoving` each reported as `--- PASS`. The final line
of stdout is exactly `MOVE_OK`. No `FAIL`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT re-implement free-space or reservation arithmetic; T099 owns `internal/fsx/space.go`.
- Do NOT unlink a task's data on request; T111 owns `delete_data`.
- Do NOT add a second bind mount or an "incomplete folder on another disk" default; ADR-0012 has exactly
  one `/data` mount and a same-filesystem move is the normal case.
- Do NOT copy or preserve ownership, setuid bits, extended attributes or symlinks; files land at `0644` and
  directories at `0755`.
- Do NOT hash file contents to verify; size equality per file is the documented check.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
