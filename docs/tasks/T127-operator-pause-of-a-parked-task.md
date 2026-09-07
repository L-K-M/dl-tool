# T127 — Keep an operator pause authoritative over a disk-full auto-resume

| Field | Value |
|---|---|
| **ID** | T127 |
| **Milestone** | M1 |
| **Status** | todo |
| **Depends on** | T022, T099, T128 |
| **Blocks** | — |
| **Parallel-safe** | yes — code edits stay in the action layer and store; plan edits stay in this task file and its two status cells in `00-task-index.md` |
| **Implements** | [FR-048](../02-requirements.md#fr-048-never-destroy-partial-data-when-a-filesystem-fills) |
| **Decisions** | — |
| **Est. size** | 0 new files, ~180 LOC |

## Goal
An operator pause on a task the disk-space guard parked (paused + `error_code = disk_full`) takes
ownership of the row: the hold stamp is cleared, so the admission pass no longer selects the task
and cannot silently un-pause what the operator parked. Today the operator pause keeps the stamp
in both directions — landing on an already-paused row is an idempotent no-op that keeps it, and
on a queued row carrying a `disk_full` hold the stamp survives the transition — and the pass
resumes the task the moment space returns: the reverse hole of the one `PauseDiskFull` already
refuses. The pause action joins T128's task-operation lease before reloading the row, then holds it
through the existing engine call, state write and clear. Its lease wait is bounded: the clear lands
before the admission claim or after a timely release, never between claim and engine call; a wait
that expires returns a clear failed outcome without mutating the task, so the operator can retry.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/02-requirements.md` FR-048](../02-requirements.md#fr-048-never-destroy-partial-data-when-a-filesystem-fills)
2. [`docs/05-api-contract.md` §5.7](../05-api-contract.md) — the task action table
3. [`docs/tasks/T099-disk-space-reservation.md`](T099-disk-space-reservation.md) — the pass's
   candidate selection (`paused` + `disk_full`) and `PauseDiskFull`'s documented operator-pause
   stance
4. [`docs/tasks/T128-atomic-claim-of-a-parked-candidate.md`](T128-atomic-claim-of-a-parked-candidate.md) —
   the shared task-operation lease and the admission-side claim

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/store/tasks.go` | modify | A guarded clear that reports whether it wiped a hold stamp (`disk_full`, `concurrency_limit`) off a paused row. |
| `internal/store/tasks_test.go` | modify | Clear cases and result: paused+hold is true; other code or active state is false; missing id is `ErrNotFound`. |
| `internal/api/tasks_actions.go` | modify | Bounded lease and reload for pause; report a busy timeout, or clear the hold stamp after success. |
| `internal/api/tasks_actions_test.go` | modify | Operator-first, release-first, failed-release, timeout/retry and queued-stamp lease cases. |
| `docs/tasks/T127-operator-pause-of-a-parked-task.md` | modify | Clear this resolved Blocked record, set Status to `done` and paste Evidence (step 4). |
| `docs/tasks/00-task-index.md` | modify | Flip both of this task's status cells (the M1-list row and the roster row) to `done` (step 4); touch no other row. |

No other file may be modified.

## Steps
1. Add the store's guarded clear: one row write matching `state = 'paused'` and
   `error_code IN ('disk_full', 'concurrency_limit')`, clearing `error_code` and `error_message`
   to NULL, never `state`. Return whether the write cleared a stamp so the action can log only a
   real takeover. A row in any other state, or carrying any other code, returns false with no
   change; a missing id is `ErrNotFound`. Distinguish those zero-row outcomes with a read by id in
   the same transaction; never loosen the clear's guard. Do not touch `ClearHoldCode` — its
   paused-row refusal is the admission release's protection and stays.
2. Give pause its own action path. Before trusting the preloaded row, acquire T128's
   task-operation lease in waiting mode with a private, named five-second timeout; never wait on
   the lease with the unbounded request context. Five seconds is an operator-response budget,
   deliberately shorter than an adapter's worst-case RPC sequence: a slow healthy holder may make
   the action ask for a retry instead of keeping the request open. If acquisition times out,
   return this id as `ok:false` with `/problems/validation-failed` and the detail
   `another task operation is in progress; retry`, and make no engine call or mutating store call. This reuses the action layer's existing
   current-state failure type; do not misreport lease contention as `engine-unavailable`.
   Otherwise reload the row and hold the lease through the existing engine pause, state transition
   and hold-stamp clear. On an already-paused row, skip the engine call and pause event
   as before, then clear. On an active row, keep the existing engine-first behavior and one event,
   then clear. Log at info only when the clear reports a hold stamp was taken over; do not add a
   `task_events` row for the clear.
3. Assert every lease outcome. If the operator acquires first, its clear lands before the guarded
   admission claim, so a pass with space and a slot calls no `Resume` or `Add` and the row stays
   paused. If admission releases successfully inside the wait budget, the action reloads the
   released row, applies the ordinary engine pause and transition, and leaves engine and row
   paused. If admission returns `ErrUnavailable` inside the budget and leaves the row parked, the
   action reloads it, skips its own engine call and pause event, and clears the stamp. If the holder outlives the
   budget, the action returns the named failure without mutation; after the holder releases, a
   retry succeeds. Pin the five-second constant directly, but drive timeout behavior with a shorter
   parent context rather than sleeping five seconds. Also drive a queued row carrying either hold
   code: an operator-first pause
   clears the stamp, and the pass's under-lease revalidation aborts its stale queued release.
   An operator resume needs no counterpart: a queued row's hold stamp is re-evaluated by the pass
   every tick. `concurrency_limit` is the queued branch's other hold stamp; once paused it is inert,
   but the clear removes it for message accuracy.
4. After Verification passes, paste its output under Evidence, set this file's Status to `done`,
   restore the Blocked placeholder below because T128 resolved this recorded stop, and flip both
   status cells in [`00-task-index.md`](00-task-index.md). Commit them with the work.

## Acceptance criteria
- [ ] An operator pause on a paused + `disk_full` row (the idempotent branch) clears the stamp
  and writes no pause event; a fresh pause on an active task still writes exactly one.
- [ ] The admission pass does not select the row afterwards: state stays `paused` with space and
  a slot available, and the engine sees no `Resume`.
- [ ] The action and admission release use the same task-operation lease. An operator-first clear
  aborts the release. After an admission-first success or `ErrUnavailable` inside the five-second
  budget, the action reloads current state and leaves the row paused with no hold stamp; it calls
  the engine only when the release succeeded.
- [ ] A lease wait that reaches the named timeout returns a per-id `validation-failed` task-busy
  outcome, mutates nothing, and succeeds on retry after the holder releases.
- [ ] An operator pause on an active task keeps the existing engine-first behavior: state and one
  event land as before, and the hold stamp is cleared with the pause.
- [ ] A paused row carrying a non-hold code (e.g. an operator message) keeps it.
- [ ] An operator pause on a queued row carrying a hold stamp clears the stamp with the pause;
  admission's under-lease revalidation does not release the stale queued snapshot.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then `ok` lines for every `internal/` package. No `FAIL`.

Also confirm scope while every task change is still uncommitted; step 4 makes the task's one
commit, after which the clean working tree cannot show these paths:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | cut -c4- | sort
```
Expected: exactly the four non-doc paths in the Files table, and nothing else; the two docs edits
are hidden by the `:(exclude)docs` pathspec.

## Out of scope — do NOT
- Do NOT change the admission pass's selection or `PauseDiskFull`; T099 owns them and they are
  done.
- Do NOT change the engine-side pause calls of the action; the transfer of a parked task is
  already engine-paused.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add a lint-suppression directive or assign an error to `_`; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Paste each Verification command, its full output and its exit status here before marking done.>

## Blocked
Stopped at step 2, before any code: the required mid-pass atomic claim does not exist, so the
pre-rewrite step 2's STOP-and-register instruction was carried out — the claim is registered as
[T128](T128-atomic-claim-of-a-parked-candidate.md), the task that owns the task-operation lease,
the admission pass's files and the store's claim write; this row's `Depends on` now names it (the
T099→T126 pattern).

What was verified, at d3946ad (`internal/engine/admission.go`, `internal/store/tasks.go`):

- `Admitter.release` calls `Engine.Resume` before its first guarded row write — `markReleased`
  runs only after the engine call answered. Between `SelectQueuedCandidates` and the engine call
  there is no row write at all: the space gate and the limit gate are read-only.
- No store write both conditions on `state = 'paused' AND error_code = 'disk_full'` and reports
  whether it took the row: `SetErrorCodeIfState` guards the state alone and answers every declined
  write with success; `ClearHoldCode` matches `state <> 'paused'` and refuses paused rows (the
  refusal this task's own step 1 preserves); `PauseWithCode` lands pauses, it does not claim;
  `Transition` conditions on state legality alone.

PR review then exposed that a bare stamp-clear claim closes only the selection-to-claim half of
the race and creates a crash state indistinguishable from an operator pause. T128 therefore owns
a task-operation lease and a no-op guarded claim that keeps the parked pair intact.

The pre-rewrite criterion 3 was therefore uncheckable inside this task's Files table, and the
clear without the shared lease and claim would close only the next-tick hole while the mid-pass
race stayed open — a half-done task against the one-commit Definition of Done. No code was written;
the edits are this file's dependency and blocker record, its Goal/Context/Files/Steps/criteria
rewrite around T128, the new T128 task file, and the index registration and dependency cells,
risk-register row, and overflow note.
