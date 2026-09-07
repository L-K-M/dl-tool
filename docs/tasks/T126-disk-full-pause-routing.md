# T126 — Route an engine disk-full report into the pause

| Field | Value |
|---|---|
| **ID** | T126 |
| **Milestone** | M1 |
| **Status** | todo |
| **Depends on** | T026, T099 |
| **Blocks** | — |
| **Parallel-safe** | yes — code edits stay in `internal/engine` (reconciler + its test) plus one constructor-argument line at the `internal/api/server.go` composition root, and this task's own row in `00-task-index.md` |
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
| `internal/api/server.go` | modify | Pass the `Admitter` to `NewReconciler` at the composition root. |
| `docs/tasks/00-task-index.md` | modify | Flip this task's own status row to `done` (step 4); touch no other row. |

No other file may be modified.

## Steps
1. In `Reconciler.writeBack`, before the generic `Transition` to the engine-reported state, detect
   `info.ErrorCode == ErrorCodeDiskFull` (the adapter maps aria2 errorCode 9 to it already; the
   constant lives in this same package, so no qualifier).
2. Call `Admitter.PauseDiskFull(ctx, task.ID, err)` for that outcome, with `err` carrying the
   engine-reported code/message so the warn trail keeps it. The reconciler and the admitter are
   the same package (`internal/engine`), so no interface or import cycle stands in the way — but
   `Reconciler` has no `Admitter` field today: add one, and its constructor call site
   (`internal/api/server.go`, which builds both) is in this task's Files table. Only substitute
   the store-level `TaskStore.PauseWithCode` if you can show it performs everything
   `PauseDiskFull` does besides the store write (see T099: the engine-side pause, the warn
   trail); otherwise step 3's resume criterion can fail silently. Note the engine download has
   already stopped with errorCode 9 by the time `writeBack` sees it, so aria2 may reject the
   engine-side pause as not active — `PauseDiskFull` treats that pause as best-effort (warn
   only) and lands the store pause regardless; pin that with a fake engine whose `Pause`
   fails, asserting the row still lands `paused` + the stamp. The row must land
   `paused` + `error_code = disk_full` + exactly one `task_events` row. Before routing, read the
   row's current stamp: if it is already `paused` — with or without a hold code — do NOT call
   `PauseDiskFull` and do NOT fall through to the generic `Transition`: write nothing. Such a
   row is operator-parked (`paused`, no code — see T127) or already guard-parked (`paused` +
   `disk_full`); neither may be re-routed, re-stamped, or un-paused here. If `PauseDiskFull` itself
   returns the allow-list refusal — whether the row moved between the read and the call or was
   never eligible (queued, seeding, completed, error, removed) — the same rule holds; log it and
   write nothing and do not fall through to the generic `Transition` on that path either, so a
   non-downloading row's `disk_full` report is dropped by choice, not by accident.
3. Assert in `reconcile_test.go`: a `disk_full` report on a downloading task leaves it paused with
   the stamp, keeps the recorded partial file byte-for-byte unchanged, and the next admission pass
   resumes the stored handle (one `Engine.Resume`, zero `Engine.Add`).
4. Flip this row to `done` in [`00-task-index.md`](00-task-index.md) in the same commit as the work.

## Acceptance criteria
- [ ] A `disk_full` engine report lands the row in `paused` with `error_code = disk_full`, not `error`.
- [ ] An engine-side pause the engine rejects (the download already stopped) does not stop the
  store-side landing: the row still reaches `paused` + the stamp.
- [ ] Exactly one `task_events` row is written for the pause.
- [ ] No partial data is unlinked; the file is byte-for-byte unchanged.
- [ ] The next admission pass resumes the stored handle; the engine sees no second `Add`.
- [ ] A `disk_full` report arriving on a row already `paused` — with or without a hold code —
  writes nothing; no re-stamp, no state change, no `task_events` row.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then an `ok` line for every internal package —
`internal/engine` and its `aria2` subpackage among them, and `internal/api`, whose call site
this task edits. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | cut -c4- | sort
```
Expected: exactly the three code paths in the Files table (`internal/engine/reconcile.go`,
`internal/engine/reconcile_test.go`, `internal/api/server.go`), and nothing else; the
`docs/tasks/00-task-index.md` edit is hidden by the `:(exclude)docs` pathspec.

## Out of scope — do NOT
- Do NOT change `PauseDiskFull` or the admission pass; T099 owns them and they are done.
- Do NOT handle any other engine error code specially; every non-`disk_full` outcome keeps the
  generic adoption path.
- Do NOT re-route rows already adopted as `error` by the pre-T126 path: `PauseDiskFull`'s state
  allow-list refuses `error` (T099), so a legacy errorCode-9 row stays `error` until an operator
  retries, and boot reconciliation (§1.6) is unchanged — record it so it is a choice, not a
  surprise.
- Do NOT add retry backoff for the routed report: the pass re-examines a parked task every tick,
  so an ENOSPC naming a volume no data root covers (an engine temp dir) cycles resume→pause
  until an operator frees space there — one `task_events` row per pause landing, at the pass
  rate (~86,400 rows/day per affected task at 1 Hz), the accepted cost of this scope. The cycle
  is operator-visible by design: each landing is a `task.paused` event in the served event log
  (T024), which is the detection signal. A backoff ladder or a cycle counter is a plan-level
  decision recorded here, not part of this task. Register the backoff/cycle-counter follow-up in
  the deferral register so the accepted cost has an owner, not only this file.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
