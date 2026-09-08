# T127 — Keep an operator pause authoritative over a disk-full auto-resume

| Field | Value |
|---|---|
| **ID** | T127 |
| **Milestone** | M1 |
| **Status** | done |
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
- [x] An operator pause on a paused + `disk_full` row (the idempotent branch) clears the stamp
  and writes no pause event; a fresh pause on an active task still writes exactly one.
- [x] The admission pass does not select the row afterwards: state stays `paused` with space and
  a slot available, and the engine sees no `Resume`.
- [x] The action and admission release use the same task-operation lease. An operator-first clear
  aborts the release. After an admission-first success or `ErrUnavailable` inside the five-second
  budget, the action reloads current state and leaves the row paused with no hold stamp; it calls
  the engine only when the release succeeded.
- [x] A lease wait that reaches the named timeout returns a per-id `validation-failed` task-busy
  outcome, mutates nothing, and succeeds on retry after the holder releases.
- [x] An operator pause on an active task keeps the existing engine-first behavior: state and one
  event land as before, and the hold stamp is cleared with the pause.
- [x] A paused row carrying a non-hold code (e.g. an operator message) keeps it.
- [x] An operator pause on a queued row carrying a hold stamp clears the stamp with the pause;
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
ok  	github.com/L-K-M/dl-tool/internal/api	54.054s
ok  	github.com/L-K-M/dl-tool/internal/config	1.246s
ok  	github.com/L-K-M/dl-tool/internal/engine	23.876s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.163s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.033s
ok  	github.com/L-K-M/dl-tool/internal/jobs	5.060s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.199s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.166s
ok  	github.com/L-K-M/dl-tool/internal/store	70.823s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.380s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.021s
EXIT=0
```

The task's new tests, run verbosely (`-race -count=1`):

```
$ go test ./internal/api/ ./internal/store/ -run 'TestOperatorPause|TestStaleQueuedReleaseAbortsUnderTheLease|TestClearPausedHoldCode' -count=1 -race -v | grep -E '(--- (PASS|FAIL)|^ok |^FAIL)'
--- PASS: TestOperatorPauseTakesOverAParkedRow (0.42s)
--- PASS: TestOperatorPauseKeepsANonHoldCode (0.33s)
--- PASS: TestOperatorPauseOnAnActiveRowKeepsEngineFirst (0.34s)
--- PASS: TestOperatorPauseClearsAQueuedRowHoldStamp (0.64s)
    --- PASS: TestOperatorPauseClearsAQueuedRowHoldStamp/disk_full (0.31s)
    --- PASS: TestOperatorPauseClearsAQueuedRowHoldStamp/concurrency_hold (0.32s)
--- PASS: TestOperatorPauseWaitsForTheAdmissionRelease (0.37s)
--- PASS: TestOperatorPauseAfterAFailedRelease (0.37s)
--- PASS: TestOperatorPauseTimesOutBehindTheLease (0.55s)
--- PASS: TestStaleQueuedReleaseAbortsUnderTheLease (0.35s)
ok  	github.com/L-K-M/dl-tool/internal/api	4.447s
--- PASS: TestClearPausedHoldCode (1.74s)
    --- PASS: TestClearPausedHoldCode/clears_a_hold_stamp_off_a_paused_row (0.70s)
    --- PASS: TestClearPausedHoldCode/a_declined_clear_writes_nothing (0.35s)
    --- PASS: TestClearPausedHoldCode/a_taken-over_row_is_invisible_to_the_admission_claim (0.33s)
    --- PASS: TestClearPausedHoldCode/reports_a_missing_id_as_not_found (0.36s)
ok  	github.com/L-K-M/dl-tool/internal/store	2.792s
```

Mutation checks — each defence was disabled in turn and its test observed to FAIL before
the feature was restored (every restoration verified by `go build ./...`):

- Reload under the lease replaced with the preloaded snapshot →
  `TestOperatorPauseWaitsForTheAdmissionRelease` FAIL (no operator `Pause` call, no
  `task.paused` event, the row left `downloading`).
- Lease acquisition switched from `TaskOpWait` to `TaskOpTry` →
  `TestOperatorPauseWaitsForTheAdmissionRelease` and `TestOperatorPauseAfterAFailedRelease`
  FAIL (the waiting action answered the retry outcome instead of joining the holder).
- Hold-stamp clear dropped from the action → `TestOperatorPauseTakesOverAParkedRow` FAIL
  (the stamp survived and the next pass released the row to `downloading`),
  `TestOperatorPauseOnAnActiveRowKeepsEngineFirst` FAIL and both
  `TestOperatorPauseClearsAQueuedRowHoldStamp` subtests FAIL
  (`/disk_full` and `/concurrency_hold`, re-observed with subtests visible after review round 1
  noted the first paste's grep hid them).
- Store guard loosened to `state = 'paused'` alone → `TestClearPausedHoldCode` FAIL
  (the non-hold-code decline case wiped the row's own code).

Scope check, run while every task change was still uncommitted:

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | cut -c4- | sort
internal/api/tasks_actions.go
internal/api/tasks_actions_test.go
internal/store/tasks.go
internal/store/tasks_test.go
```

Exactly the four non-doc paths of the Files table; the docs side is this file and the two
status cells of `00-task-index.md`.

### Review round 2 (commit 532804e → this one)

Three inline comments: two were round-1 leftovers whose suggestions had already landed (the
subtest-visible paste — line 202's "drop subtests wording" premise no longer holds, the paste
shows both `/disk_full` and `/concurrency_hold` — and the claim-decline subtest itself); one
applied: the claim-decline subtest now seeds a positive control — an identical untouched parked
row asserted still claimable — so the decline is the takeover's doing, not a claim that declines
everything.

`make lint && make test PKG=./internal/...` after the round-2 fix — lint clean, every internal
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
ok  	github.com/L-K-M/dl-tool/internal/api	48.366s
ok  	github.com/L-K-M/dl-tool/internal/config	1.106s
ok  	github.com/L-K-M/dl-tool/internal/engine	20.428s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.149s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.034s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.601s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.163s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.138s
ok  	github.com/L-K-M/dl-tool/internal/store	67.783s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.344s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.021s
EXIT=0
```

### Review round 1 (commit f678ef4 → 532804e)

Two majors — one applied as a test, one declined on a verified premise — four minors applied,
the rest declined with evidence.

**Majors**

- **The lease hold may be bounded by the five-second wait context** — premise verified false:
  `Registry.AcquireTaskOp` ties the hold to the returned release closure, never to the context —
  the context only gates the wait (a canceled waiter leaves the queue, and a handoff that already
  won still returns the lease; `TestTaskOpCanceledWaiterAnswersItsContext` and
  `TestTaskOpHandoffCancelRaceHasOneOutcome` pin both). Nothing watches `waitCtx` after the
  acquisition returns. Applied as a comment at the acquisition site stating the budget bounds
  only the wait, so the next reader need not re-derive it from the registry.
- **The takeover is never tested against the admission claim** — applied:
  `TestClearPausedHoldCode/a_taken-over_row_is_invisible_to_the_admission_claim` drives clear then
  `ClaimParkedDiskFull` on one row and pins the decline.

**Applied (minors)**

- `pauseEnv.pause` guards an empty `Results` slice before indexing.
- `pauseEnv.taskEventCodes` orders by `rowid`, not the ULID `id`: event ids carry same-millisecond
  random entropy, and the sequence assertions compare pairs that can land inside one millisecond.
- The verbose paste now shows the subtest lines (the first paste's `^(---` grep hid them); the
  mutation bullet's "both subtests" claim is re-observed with them visible.
- The two goroutine tests' `time.After` backstops widen to `pauseLeaseWait + 5s` so the backstop
  cannot race the action's own budget clock, and the waits-for-release test documents why the
  pause goroutine is deliberately not synchronised with the gate close (outcome-equivalent
  scheduling; the frozen preloaded snapshot makes the reload the only mover, and the
  waiting-not-failing half is pinned by the timeout test's busy outcome).

**Declined with evidence**

- **A busy/conflict slug instead of `/problems/validation-failed`** (raised twice) — the task's
  step 2 pins the slug and the detail verbatim and names the constraint: the action layer's
  existing current-state failure type, never `engine-unavailable`; §5.7's outcome vocabulary has
  no busy slug, and inventing one is outside this task's Files table.
- **Distinguish `AcquireTaskOp`'s errors** — verified total: in `TaskOpWait` the acquisition can
  answer only its context's error or nil (`ErrTaskOpBusy` is the Try mode's answer), so there is
  nothing to distinguish; a comment at the call site now says so. A canceled parent answering the
  retry outcome is harmless — the client is gone.
- **`reloaded` copies only four fields** — verified: `actionTask` has exactly `ID`, `Engine`,
  `EngineRef` and `State`.
- **`transitionAction`'s successful result discarded** — verified equivalent: on success it
  returns the identical `ActionResult{ID, Ok: true}` the pause path builds.
- **Cover the action's own engine Pause failing** — already pinned:
  `TestActionsEngineErrorMapping/engine_unavailable` sets `pauseErr` on an active task and asserts
  the per-id `engine-unavailable` outcome with the state unchanged, through the full pause path
  (the uncontended lease).
- **`ClearPausedHoldCode` answering not-found after the pause landed** — a vanished row cannot be
  resumed by the pass either; the not-found outcome is the store's plain answer, and deletion
  racing a mid-pause request is the pre-existing delete path's territory, outside this task.
- **`waitResumeRecorded` should surface a failed pass** — the pass goroutines already report their
  error with `t.Errorf`, so an early failure fails the test with the real cause; the channel
  wiring would only move the same message earlier.
- **The restored Blocked placeholder reads unfilled** — it is the file's own instruction (step 4)
  and the convention every done task keeps, T128's own Blocked section included.

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
ok  	github.com/L-K-M/dl-tool/internal/api	49.169s
ok  	github.com/L-K-M/dl-tool/internal/config	1.110s
ok  	github.com/L-K-M/dl-tool/internal/engine	20.793s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.180s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.029s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.335s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.184s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.236s
ok  	github.com/L-K-M/dl-tool/internal/store	65.974s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.367s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.041s
EXIT=0
```

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
