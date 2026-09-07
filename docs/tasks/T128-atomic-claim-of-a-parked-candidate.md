# T128 — Claim a parked candidate before the engine resumes it

| Field | Value |
|---|---|
| **ID** | T128 |
| **Milestone** | M1 |
| **Status** | done |
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
- [x] The registry task-operation lease serialises the admission release and T127's later operator
  pause for one task, while operations on different task ids do not block each other. Waiting is
  context-aware and FIFO; non-blocking acquisition returns the named busy error.
- [x] The pass claims a `paused` + `disk_full` candidate with one guarded no-op write conditioned
  on that pair before any engine call; the pair and its timestamp remain unchanged.
- [x] A hold-stamp clear after selection is caught by the under-lease re-read or guarded claim:
  the row stays `paused`, the engine sees no `Resume` or `Add`, and no event row is written. Once
  the claim takes the row, the lease prevents the operator clear from landing until the engine
  call and row write finish.
- [x] The claim itself needs no recovery write. A process exit after the claim and before the
  first engine call leaves `paused` + `disk_full` intact and selectable because no marker was
  stored. No criterion asserts post-engine-call idempotency; ordinary release errors retain their
  existing `releaseFailed` outcomes.
- [x] T126's parked release shapes keep their outcomes and engine-call counts: the plain unpause,
  the `ErrNotFound` re-submit and the stopped `disk_full` re-submit.
- [x] The lease covers hold decisions and stamp writes: a pass cannot re-stamp a parked row after
  an operator clear that won the lease.
- [x] Admission uses non-blocking acquisition, holds no more than one task lease, and skips a busy
  candidate before spending capacity. A queued candidate moved after selection is not released;
  an unchanged queued candidate gets no claim write and otherwise releases unchanged.
- [x] A declined or vanished candidate returns `(false, nil)`, with no engine call, row write,
  event, released id, or in-memory slot and reservation spend.
- [x] The claim's three answers are pinned: taken, declined, `ErrNotFound`. Lease tests pin busy
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

`make lint && make test PKG=./internal/...` after the last code change — lint clean, every
internal package `ok`, no `FAIL`, exit 0:

```
$ make lint && make test PKG=./internal/...
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier Code style!
go test -race -count=1 ./internal/...
ok  	github.com/L-K-M/dl-tool/internal/api	47.154s
ok  	github.com/L-K-M/dl-tool/internal/config	1.117s
ok  	github.com/L-K-M/dl-tool/internal/engine	22.128s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.180s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.018s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.403s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.147s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.163s
ok  	github.com/L-K-M/dl-tool/internal/store	68.344s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.349s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.020s
EXIT=0
```

The task's new tests, run verbosely (`-race -count=1`):

```
$ go test ./internal/engine/ ./internal/store/ -run 'TestTaskOp|TestSelectionTimeClear|TestClaimTimeClear|TestClaimParkedDiskFull|TestPassSkipsABusyTask|TestVanishedCandidate|TestQueuedReleaseClaims|TestLeaseIsHeld' -count=1 -race -v | grep -E '^(--- (PASS|FAIL)|ok |FAIL)'
--- PASS: TestTaskOpLeaseExcludesOneTaskID (0.01s)
--- PASS: TestTaskOpLeaseIndependentAcrossTaskIDs (0.00s)
--- PASS: TestTaskOpTryAnswersBusyWhileHeld (0.00s)
--- PASS: TestTaskOpWaitIsFIFOAndCannotBeOvertaken (0.00s)
--- PASS: TestTaskOpCanceledWaiterAnswersItsContext (0.00s)
--- PASS: TestTaskOpHandoffCancelRaceHasOneOutcome (0.12s)
--- PASS: TestTaskOpReleaseIsIdempotent (0.00s)
--- PASS: TestTaskOpEntriesDoNotAccumulate (0.00s)
--- PASS: TestSelectionTimeClearAbortsTheParkedRelease (0.42s)
--- PASS: TestClaimTimeClearDeclinesTheGuardedClaim (0.40s)
--- PASS: TestPassSkipsABusyTask (0.36s)
--- PASS: TestVanishedCandidateAbortsQuietly (0.37s)
--- PASS: TestQueuedReleaseClaimsNothingAndParkedClaimsOnce (0.35s)
--- PASS: TestLeaseIsHeldThroughTheReleaseWrites (0.33s)
ok  	github.com/L-K-M/dl-tool/internal/engine	3.465s
--- PASS: TestClaimParkedDiskFull (1.17s)
ok  	github.com/L-K-M/dl-tool/internal/store	2.235s
```

T126's parked release shapes (plain unpause, `ErrNotFound` re-submit, stopped `disk_full`
re-submit) are pinned unchanged by the existing `TestENOSPCPausesAndKeepsData`,
`TestPassReAddsVanishedHandle` and `TestDiskFullReport*` in `internal/engine/reconcile_test.go`,
which run in the suite above with their engine-call counts unedited.

Mutation checks — each defence was disabled in turn and its test observed to FAIL before the
feature was restored (all restorations verified by `go build ./...`):

- Revalidation read disabled (`processCandidate` never aborts on a stale snapshot) →
  `TestSelectionTimeClearAbortsTheParkedRelease` FAIL (the claim fired instead of the re-read,
  caught by the claims==0 assertion).
- Claim guard loosened to `state = 'paused'` alone → `TestClaimTimeClearDeclinesTheGuardedClaim`
  FAIL (the cleared row was wrongly released and its reservation held the sibling back).
- Pass lease acquisition pointed elsewhere → `TestPassSkipsABusyTask` and
  `TestLeaseIsHeldThroughTheReleaseWrites` FAIL (the busy task was released; the mid-write probe
  found the lease free).

Scope check. The work was committed in coherent steps (the PR rule), so the working-tree gate ran
empty by design once each step landed; the branch diff is the equivalent that names every touched
path. Both outputs verbatim, with docs changes excluded on the code side and included below:

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | cut -c4- | sort
(empty — every code change committed)

$ git diff --name-only origin/main...HEAD -- . ':(exclude)docs' | sort
internal/engine/admission.go
internal/engine/admission_test.go
internal/engine/registry.go
internal/engine/registry_test.go
internal/store/tasks.go
internal/store/tasks_test.go
```

Exactly the six non-doc paths of the Files table, nothing else; the docs side is this file and
`docs/tasks/00-task-index.md`.

### Review round 1 (commit 06eae21 → this one)

Two inline comments, both on `TestLeaseIsHeldThroughTheReleaseWrites`, both applied:

- **Assert at least one probe fired** — applied as a `probed` counter with a fatal on zero: an
  empty channel made the busy-check loop pass vacuously. Verified to bite: pointing the wrapper
  at a foreign id fails the test with `no probes recorded`.
- **Release the probe's lease when acquired** — applied: the probe's Try now releases a successful
  acquisition instead of discarding it, so an unexpected success (the bug the test hunts) cannot
  also leak the lease and blur the assertions after it.

`make lint && make test PKG=./internal/...` after the round-1 fixes — lint clean, every internal
package `ok`, no `FAIL`, exit 0:

```
$ make lint && make test PKG=./internal/...
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier Code style!
go test -race -count=1 ./internal/...
ok  	github.com/L-K-M/dl-tool/internal/api	45.318s
ok  	github.com/L-K-M/dl-tool/internal/config	1.120s
ok  	github.com/L-K-M/dl-tool/internal/engine	19.630s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.161s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.026s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.551s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.163s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.022s
ok  	github.com/L-K-M/dl-tool/internal/store	64.473s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.376s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.042s
EXIT=0
```

The mutation check for the vacuity guard (wrapper pointed at a foreign id, then restored):

```
$ go test ./internal/engine/ -run 'TestLeaseIsHeld' -count=1
--- FAIL: TestLeaseIsHeldThroughTheReleaseWrites (0.03s)
    admission_test.go:2167: no probes recorded: the release never transitioned tsk_01M1Z0M0Z5QDC49DTWTQ5D4M28 through the wrapped store, so the lease span went unobserved
FAIL
FAIL	github.com/L-K-M/dl-tool/internal/engine	0.031s
```

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
