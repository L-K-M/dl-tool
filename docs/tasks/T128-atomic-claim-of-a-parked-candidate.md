# T128 — Claim a parked candidate before the engine resumes it

| Field | Value |
|---|---|
| **ID** | T128 |
| **Milestone** | M1 |
| **Status** | todo |
| **Depends on** | T099, T126 |
| **Blocks** | T127 |
| **Parallel-safe** | no — it edits the shared registry, admission release path and store task writes |
| **Implements** | [FR-048](../02-requirements.md#fr-048-never-destroy-partial-data-when-a-filesystem-fills) |
| **Decisions** | [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md) — one process and one SQLite writer |
| **Est. size** | 1 new file, ~300 LOC |

## Goal
The admission pass takes a task-operation lease before it revalidates a selected candidate and
holds that lease through the hold decision, stamp write, engine call, failure handling and release
writes. For a `paused` + `disk_full` candidate, one guarded no-op row write then claims the
persisted pair without clearing it. T127's operator pause will take the same lease with a bounded
wait: its clear lands before the claim, follows a completed release, or fails clearly when the wait
budget expires. No clear can land between a successful claim and `Engine.Resume`, and admission
never waits behind another operation. A process exit after the claim but before the first engine
call drops the in-memory lease while the unchanged pair remains selectable. This narrow recovery
claim adds no persisted half-state; it does not assume `Engine.Resume` or T126's re-submit paths
are idempotent across the separate, pre-existing engine-call/store-write crash window.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/02-requirements.md` FR-048](../02-requirements.md#fr-048-never-destroy-partial-data-when-a-filesystem-fills)
2. [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md) — one process, SQLite only and one writer
3. [`go.mod`](../../go.mod) — the SQLite driver pin
4. [`docs/tasks/T127-operator-pause-of-a-parked-task.md`](T127-operator-pause-of-a-parked-task.md) —
   the operator action that joins the lease and clears the hold stamp
5. [`docs/tasks/T099-disk-space-reservation.md`](T099-disk-space-reservation.md) — the pass's
   candidate selection, `PauseDiskFull`, `ClearHoldCode` and the guarded writes' refusal semantics
6. [`docs/tasks/T126-disk-full-pause-routing.md`](T126-disk-full-pause-routing.md) — the three
   release shapes the claim must keep: the plain unpause, the `ErrNotFound` re-submit and the
   stopped `disk_full` re-submit

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/registry.go` | modify | A fair, context-aware task-operation lease with waiting and non-blocking acquisition modes. |
| `internal/engine/registry_test.go` | create | Exclusion, independence, busy try, FIFO handoff, handoff/cancel race, cleanup and idempotent release. |
| `internal/store/tasks.go` | modify | A guarded no-op claim matching `state = 'paused' AND error_code = 'disk_full'` and reporting whether it took the row. |
| `internal/store/tasks_test.go` | modify | Execute the exact `UPDATE … RETURNING`; test unchanged take, cleared/moved decline and missing `ErrNotFound`. |
| `internal/engine/admission.go` | modify | Lease and revalidate every release; claim a parked candidate before any engine call. |
| `internal/engine/admission_test.go` | modify | The lease and claim races, vanished candidate, unchanged parked release shapes and unchanged queued releases. |
| `docs/tasks/T128-atomic-claim-of-a-parked-candidate.md` | modify | Set Status to `done` and paste the verification output under Evidence (step 7). |
| `docs/tasks/00-task-index.md` | modify | Flip both of this task's status cells (the M1-list row and the roster row) to `done` (step 7); touch no other row. |

No other file may be modified.

## Steps
1. Add a context-aware task-operation lease to `Registry`, the instance already shared by the
   admission pass and `TaskHandlers`. Give acquisition an enum with wait and non-blocking modes,
   not a boolean parameter. Operations for one task id exclude each other; different ids remain
   independent. A non-blocking attempt returns a named busy error. Waiting is FIFO, and a canceled
   waiter is removed and returns `ctx.Err()`. Release hands ownership directly to the oldest
   waiter, or deletes the map entry only when no holder or waiter can still refer to it; a new
   acquirer can never create a second live entry or overtake a parked waiter. A cancellation racing
   with handoff produces exactly one outcome and cannot leak ownership. The returned release
   operation is idempotent, and entries do not accumulate after their last holder leaves.
2. Add the store's claim write with SQLite's `UPDATE … RETURNING id`. `go.mod` is the source of
   truth for the `modernc.org/sqlite` pin, which must bundle SQLite 3.35.0 or newer. The store test
   executes the exact statement, so syntax support is feature-tested rather than trusted from a
   copied version number. Use `SET error_code = error_code` under the exact
   `state = 'paused' AND error_code = 'disk_full'` guard. `RETURNING` identifies a matched row
   directly; do not infer the answer from changed-row counts. If it returns no row, read by id in
   the same transaction to distinguish a present-but-changed row (declined) from `ErrNotFound`;
   never loosen the update's guard. As with the candidate query, its SQL comment pins the
   `disk_full` literal to `engine.ErrorCodeDiskFull`. The schema has no update trigger, and the
   self-assignment changes no state, stamp, timestamp or event. The registry lease owns in-process
   exclusion under ADR-0004's one-process deployment; the write atomically revalidates the
   persisted pair after selection. Never clear the stamp as the claim: a crash before the engine
   call would then look like an operator takeover and strand the row outside the candidate query.
   Keeping the pair unchanged makes the claim itself recoverable: before the first engine call it
   needs no restore and remains in that query. Do not infer idempotency across a later engine-call/store-write crash; that is
   T126's existing boundary, not state introduced by this claim. Ordinary release errors still
   take the existing `releaseFailed` path.
3. Try the task-operation lease without waiting at the start of each candidate iteration, before
   either hold gate or `stampHeld`. A busy task is skipped quietly, before any slot or reservation
   is spent, so one slow action cannot head-of-line block the pass. Hold at most one task lease at
   a time and release it before the next candidate. Under the lease, re-read the candidate:
   require `queued` for a queued snapshot and `paused` + `disk_full` for a parked snapshot. A
   moved, cleared or vanished row aborts quietly. Run the existing hold gates and stamp writes only
   after that check, and keep the lease through release failure handling and every row write. This
   prevents a holding pass from re-stamping `disk_full` after an operator cleared it.
4. Immediately before a parked candidate's first engine call, execute the guarded claim and gate
   every release shape on its positive answer. Change the candidate-processing helper to return
   `(released bool, err error)` and use the boolean to gate the released-id append and in-memory
   slot and reservation spend. A declined or `ErrNotFound` claim is an overtaken candidate, not an
   engine rejection: return `(false, nil)`, call no engine, and write no row or event. Keep queued
   release mechanics unchanged after revalidation: no claim write, and the same
   `Resume`/re-submit/Add behavior. The lease covers queued candidates too because T127 also clears
   a hold stamp after pausing a queued row. A waiting operator reloads after acquisition; admission
   skips a busy task and reselects current state on a later pass.
5. Keep T126's parked release shapes intact: a genuinely paused handle still gets one `Resume`
   and zero `Add`s; a handle the engine lost still re-submits with resume semantics; a stopped
   `disk_full` result still goes through the `Get`-confirmed re-submit. Their engine-call counts
   are unchanged; the lease and one claim write precede the first call.
6. Drive both pre-claim checks: clear the stamp once when candidate selection returns, proving the
   under-lease re-read aborts stale work, and once in a store wrapper immediately before it delegates
   the claim, proving the guarded write declines. The wrappers call the existing
   `SetErrorCodeIfState(ctx, id, "paused", "", "")`; neither calls `ClearHoldCode`, which refuses
   paused rows. In both cases assert the parked row stays `paused`, the engine sees no `Resume` or
   `Add`, no event is written, and no slot or reservation is spent. Separately hold the registry
   lease and prove the pass skips that busy task without blocking or spending capacity.
7. After Verification passes, paste its output under Evidence, set this file's Status to `done`,
   and flip both status cells in [`00-task-index.md`](00-task-index.md). Commit them with the work.

## Acceptance criteria
- [ ] The registry task-operation lease serialises the admission release and T127's later operator
  pause for one task, while operations on different task ids do not block each other. Waiting is
  context-aware and FIFO; non-blocking acquisition returns the named busy error.
- [ ] The pass claims a `paused` + `disk_full` candidate with one guarded no-op write conditioned
  on that pair before any engine call; the pair and its timestamp remain unchanged.
- [ ] A hold-stamp clear after selection is caught by the under-lease re-read or guarded claim:
  the row stays `paused`, the engine sees no `Resume` or `Add`, and no event row is written. Once
  the claim takes the row, the lease prevents the operator clear from landing until the engine
  call and row write finish.
- [ ] The claim itself needs no recovery write. A process exit after the claim and before the
  first engine call leaves `paused` + `disk_full` intact and selectable because no marker was
  stored. No criterion asserts post-engine-call idempotency; ordinary release errors retain their
  existing `releaseFailed` outcomes.
- [ ] T126's parked release shapes keep their outcomes and engine-call counts: the plain unpause,
  the `ErrNotFound` re-submit and the stopped `disk_full` re-submit.
- [ ] The lease covers hold decisions and stamp writes: a pass cannot re-stamp a parked row after
  an operator clear that won the lease.
- [ ] Admission uses non-blocking acquisition, holds no more than one task lease, and skips a busy
  candidate before spending capacity. A queued candidate moved after selection is not released;
  an unchanged queued candidate gets no claim write and otherwise releases unchanged.
- [ ] A declined or vanished candidate returns `(false, nil)`, with no engine call, row write,
  event, released id, or in-memory slot and reservation spend.
- [ ] The claim's three answers are pinned: taken, declined, `ErrNotFound`. Lease tests pin busy
  try, cancellation and its handoff race, idempotent release, and A→B handoff where a later C
  cannot overtake B.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then `ok` lines for every `internal/` package, the store and
engine packages among them. The `test-go` target already uses `-race -count=1`: no `FAIL` or
`DATA RACE`.

Also confirm scope while every task change is still uncommitted; step 7 makes the task's one
commit, after which the clean working tree cannot show these paths:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | cut -c4- | sort
```
Expected: exactly the six non-doc paths in the Files table, and nothing else; the two docs edits
are hidden by the `:(exclude)docs` pathspec.

## Out of scope — do NOT
- Do NOT add the operator-pause hold-stamp clear or the action-layer takeover; T127 owns them and
  runs after this task.
- Do NOT change the candidate query, the space gate, the concurrency gates, `PauseDiskFull` or
  `ClearHoldCode`.
- Do NOT add a backoff ladder or cycle counter for the resume→pause cycle; that is the owner
  decision recorded in the deferral register.
- Do NOT claim this lease makes an engine call and its later store write atomic. Post-call crash
  recovery is the existing T126/reconciler boundary; this prerequisite only adds a pre-call claim.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add a lint-suppression directive or assign an error to `_`; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Paste each Verification command, its full output and its exit status here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
