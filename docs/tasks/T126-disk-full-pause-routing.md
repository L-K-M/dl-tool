# T126 — Route an engine disk-full report into the pause

| Field | Value |
|---|---|
| **ID** | T126 |
| **Milestone** | M1 |
| **Status** | todo |
| **Depends on** | T026, T099 |
| **Blocks** | — |
| **Parallel-safe** | yes — code edits stay in the reconciler's files; the only other edit is this task's own row in `00-task-index.md` |
| **Implements** | [FR-048](../02-requirements.md#fr-048-never-destroy-partial-data-when-a-filesystem-fills) |
| **Decisions** | [ADR-0017](../decisions/0017-exclusive-control-of-engines.md) |
| **Est. size** | 0 new files, ~60 LOC |

## Goal
An aria2 disk-full report (errorCode 9, already mapped to `TaskInfo.ErrorCode = "disk_full"` by T018)
pauses the task with `Admitter.PauseDiskFull` instead of adopting the engine's `error` state, so
FR-048's pause-keep-resume path is reached by the report that observes the condition, not only by
dl-tool's own write paths.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/02-requirements.md` FR-048](../02-requirements.md#fr-048-never-destroy-partial-data-when-a-filesystem-fills)
2. [`docs/17-operations-and-runbook.md` §1.6 Boot reconciliation](../17-operations-and-runbook.md)
3. [`docs/tasks/T099-disk-space-reservation.md`](T099-disk-space-reservation.md) — `PauseDiskFull`, its atomic landing and its tests

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/reconcile.go` | modify | In `writeBack`, route a `disk_full` engine outcome into `PauseDiskFull` instead of the generic state adoption. |
| `internal/engine/reconcile_test.go` | modify | A `disk_full` report pauses with the stamp and unlinks nothing; the release resumes the stored handle with zero re-Adds. |
| `docs/tasks/00-task-index.md` | modify | Flip this task's own status row to `done` (step 4); touch no other row. |

No other file may be modified.

## Steps
1. In `Reconciler.writeBack`, before the generic `Transition` to the engine-reported state, detect
   `info.ErrorCode == engine.ErrorCodeDiskFull` (the adapter maps aria2 errorCode 9 to it already).
2. Call `Admitter.PauseDiskFull(ctx, task.ID, nil)` for that outcome — the reconciler owns the
   `Admitter` reference it needs, or the store-level `TaskStore.PauseWithCode` directly with the
   same codes, whichever fits the reconciler's existing collaborators. The row must land
   `paused` + `error_code = disk_full` + exactly one `task_events` row.
3. Assert in `reconcile_test.go`: a `disk_full` report on a downloading task leaves it paused with
   the stamp, keeps the recorded partial file byte-for-byte unchanged, and the next admission pass
   resumes the stored handle (one `Engine.Resume`, zero `Engine.Add`).
4. Flip this row to `done` in [`00-task-index.md`](00-task-index.md) in the same commit as the work.

## Acceptance criteria
- [ ] A `disk_full` engine report lands the row in `paused` with `error_code = disk_full`, not `error`.
- [ ] Exactly one `task_events` row is written for the pause.
- [ ] No partial data is unlinked; the file is byte-for-byte unchanged.
- [ ] The next admission pass resumes the stored handle; the engine sees no second `Add`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/engine/...
```
Expected: `make lint` prints nothing, then `ok` lines for
`github.com/L-K-M/dl-tool/internal/engine` and its `aria2` subpackage. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the two code paths in the Files table (`internal/engine/reconcile.go`,
`internal/engine/reconcile_test.go`), and nothing else; the `docs/tasks/00-task-index.md` edit is
hidden by the `:(exclude)docs` pathspec.

## Out of scope — do NOT
- Do NOT change `PauseDiskFull` or the admission pass; T099 owns them and they are done.
- Do NOT handle any other engine error code specially; every non-`disk_full` outcome keeps the
  generic adoption path.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
