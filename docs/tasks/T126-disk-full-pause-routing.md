# T126 — Route an engine disk-full report into the pause

| Field | Value |
|---|---|
| **ID** | T126 |
| **Milestone** | M1 |
| **Status** | done |
| **Pending decision** | owner decision at M1 exit — bound the disk-full resume→pause cycle (deferral register; up to ~172,800 event rows/day at 1 Hz) |
| **Depends on** | T026, T099 |
| **Blocks** | — |
| **Parallel-safe** | yes — code edits stay in `internal/engine` (reconciler + its test) plus the `internal/api/server.go` construction block (a shared store hoist and the admitter constructed before the reconciler), and this task's own row in `00-task-index.md` |
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
`docs/tasks/00-task-index.md` edit is hidden by the `:(exclude)docs` pathspec. That exclusion
makes the code-side check structurally blind to out-of-scope docs edits, so the docs side gets
its own gate:
```bash
git status --porcelain=v1 -uall -- docs | cut -c4- | sort
```
Expected: exactly the two doc paths this task owns (`docs/tasks/00-task-index.md` and this
file), and nothing else. The gates assume a task-private working tree — if parallel tasks share
one, their doc edits appear here too; confirm each extra path belongs to another task's Files
table before treating it as a scope violation. Both gates read the working tree, so run them
before committing; once committed, use the branch-diff form recorded under Evidence
(`git diff --name-only origin/main...HEAD -- docs`).

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

### Review round 10 (commit 653cfba → this one)

No majors; five minors, two infos; triage and outcome:

- **Minor — the round-9 header overcounts the minors** — applied: the
  round-9 review's own header said six while its body carried five;
  recounted from the round-9 bullets below (1 major, 5 minors, 2 infos —
  the round-9 response enumerated the same five), the header now says
  five. No disposition was dropped.
- **Minor — refusals and transient store failures are conflated in
  `routeDiskFull` (5th raise of the Warn/Debug split)** — declined,
  grounds unchanged: the refusals are ad-hoc `fmt.Errorf` values from
  `PauseDiskFull` (`internal/engine/admission.go`, outside the Files
  table, whose Out-of-scope section forbids changing `PauseDiskFull`),
  so no sentinel exists to `errors.Is` without that edit — and
  propagating the untyped refusal instead would reclassify a designed
  drop as a per-task `Error` (the sweep's "reconcile write-back failed"
  log) on every sweep for the parked/queued shapes the tests pin. A
  refusal writes nothing; the recurring Warn is the registered, priced
  cycle (the deferral register row).
- **Minor — routing keyed on ErrorCode alone trusts the §4.4 invariant
  (2nd raise of the guard family)** — declined: aria2 — the only
  registered engine — carries an errorCode only for stopped/error
  statuses (§4.4; `toTaskInfo` copies it only when a status result has
  one), so a live transfer cannot carry code 9. A violating adapter is
  an adapter bug to fix in the adapter; falling through to generic
  adoption of a live state is not safer than parking a transfer whose
  disk just reported ENOSPC.
- **Minor — the race test can pass vacuously if Boot lists the store
  before the sweep's snapshot** — applied: the reconciler's pauser is a
  `diskFullSpy` counting `PauseDiskFull` calls, asserted exactly 1
  after Boot. Proven to bite: on the 653cfba test, inserting a pre-sweep
  listing (the simulated Boot refactor) left every assertion green —
  the vacuous pass, observed; with the spy, the same insertion fails
  `PauseDiskFull calls = 0, want 1`.
- **Minor — the panic-recovery switch has no `case nil`** — declined,
  false against the reviewed commit: `case nil` is present in
  `TestNewReconcilerRequiresItsDependencies` at 653cfba
  (`git show 653cfba:internal/engine/reconcile_test.go | grep -n 'case nil'`
  → line 1300); the review's diff anchor was stale.
- **Info — newSweepEnv's eager pair is discarded by the two wrapper
  tests (2nd raise)** — applied this time as the `wire` method: the
  wrapper tests rebuild `env.admit`/`env.rec` over the wrapper, so the
  stale raw-store pair no longer exists to reach for.
- **Info — the same fake fills both store-side roles, so the pauser is
  unasserted** — satisfied by the race test's spy: a `DiskFullPauser`
  distinct from the store is injected into `NewReconciler` and its
  invocation asserted exactly once. The double-duty fake in
  `TestCancelledListIsNotUnreachable` stays, on round 8's grounds.

`make lint && make test PKG=./internal/...` after the round-10 fixes —
lint clean, every internal package `ok`, no `FAIL`:

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
All matched files use Prettier code style!
go test -race -count=1 ./internal/...
ok  	github.com/L-K-M/dl-tool/internal/api	45.183s
ok  	github.com/L-K-M/dl-tool/internal/config	1.129s
ok  	github.com/L-K-M/dl-tool/internal/engine	17.335s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.156s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.032s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.415s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.177s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.262s
ok  	github.com/L-K-M/dl-tool/internal/store	63.325s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.357s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.047s
```

```
$ make doclint 2>&1 | grep Total
🔍 2361 Total (in 182ms) 🔗 552 Unique ✅ 2347 OK 🚫 0 Errors 👻 14 Excluded
```

The routing suite after the round-10 fixes:

```
$ go test ./internal/engine/ -run 'TestDiskFull|TestNewReconciler' -count=1 -race -v | grep -E '^(--- (PASS|FAIL)|PASS|FAIL|ok)'
--- PASS: TestDiskFullReportPausesThroughAdmission (0.39s)
--- PASS: TestDiskFullReportSurvivesRejectedEnginePause (0.36s)
--- PASS: TestDiskFullReportOnAPausedRowWritesNothing (0.69s)
--- PASS: TestDiskFullReportOnAQueuedRowIsDropped (0.34s)
--- PASS: TestDiskFullReportRacingAnOperatorPauseWritesNothing (0.35s)
--- PASS: TestNewReconcilerRequiresItsDependencies (0.00s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/engine	3.188s
```

The mutation check for the spy, both halves — the same inserted
pre-sweep listing simulating a Boot refactor that lists the store
before its sweep:

```
$ # on 653cfba's test (no spy): every assertion stays green — vacuous
$ go test ./internal/engine/ -run 'TestDiskFullReportRacingAnOperatorPauseWritesNothing' -count=1
ok  	github.com/L-K-M/dl-tool/internal/engine	0.064s
$ # with the spy: the never-routed report is caught
$ go test ./internal/engine/ -run 'TestDiskFullReportRacingAnOperatorPauseWritesNothing' -count=1
--- FAIL: TestDiskFullReportRacingAnOperatorPauseWritesNothing (0.05s)
    reconcile_test.go:1273: PauseDiskFull calls = 0, want 1 — the sweep must route the report before the refusal
FAIL
FAIL	github.com/L-K-M/dl-tool/internal/engine	0.068s
```

Scope, both gates (working-tree form, pre-commit):

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | cut -c4- | sort
internal/engine/reconcile_test.go
$ git status --porcelain=v1 -uall -- docs | cut -c4- | sort
docs/tasks/T126-disk-full-pause-routing.md
```

Exactly the Files table, both sides — this round touched only the test
file and this file.

### Review round 9 (commit 31370f3 → 653cfba)

One major, five minors, two infos (the review's own header said six;
its body carried five — recounted, see round 10); triage and outcome:

- **Major — the cycle ships unbraked while T127 is `todo` (3rd raise of
  the sequencing concern)** — applied as the register clause the finding
  now asks for: the M1 exit row's disposition cell reads "no
  operator-facing release may ship T126's routing before T127 (or the
  owner-decided counter) lands". Its verification steps check out: the
  pass rate is 1 Hz (NewReconciler's poll, 1s in production), each
  iteration writes both a `task.resumed` and a `task.paused` row (pinned
  by `TestDiskFullReportPausesThroughAdmission`), no backoff or
  coalescing exists on the path, and a manual pause on a `disk_full`
  row is resumed by the next pass (the T127 register row's own text).
- **Minor — T127's prerequisites omit T126** — declined: editing T127's
  rows and task file is outside this task's Files table, and the
  dependency is not real. The disk-full-parked state predates T126:
  T099's admission guard lands `PauseDiskFull` with the stamp from
  dl-tool's own ENOSPC detection; T126 only routes the *engine's* report
  into the same landing. T127 guards the auto-resume T099 created,
  however the stamp arrived, and its tests can stamp `disk_full`
  directly. Recorded here so T127's session sees the consideration.
- **Minor — Evidence narratives grow ~80 lines per round** — applied:
  rounds 1–8 collapse to summaries below; declined findings keep
  one-line grounds, since later rounds cite them by raise-count. The
  newest round keeps the single full transcript record.
- **Minor — the dropped report loses its payload in the warn trail** —
  applied: `cause` joined the refusal Warn between `snapshot_state` and
  `error`, so a drop logs what the engine actually said. No test asserts
  the attribute set.
- **Minor — the main-path test duplicates `assertPartialUntouched`
  inline** — applied: it calls the helper now; the uniform comparator
  claim in the helper's doc is true again.
- **Minor — the panic test couples to `string` panic values** — applied:
  the recover switch gained an `error` case with the same
  names-the-dependency assertion, and the default branch reports the
  value's type.
- **Info — the docs gate false-fails in a shared working tree** —
  applied as the assumption note: the gates now state they assume a
  task-private working tree and say how to treat a foreign path before
  crying scope violation.
- **Info — the same fake fills both constructor slots (the aliasing
  point, 2nd raise, now with a comment ask)** — applied as the comment:
  the call site now says the one fake satisfies both roles
  (TaskWriter and DiskFullPauser) as the test-side form of the
  production wiring.

Post-fix verdict: lint clean, every internal package `ok`, doclint 0
errors, routing suite 6/6 PASS — output at 653cfba, superseded as the
full record by round 10's transcript above. Scope: exactly the Files
table, both gates.

### Review round 8 (commit 0605ca4 → this one)

One major, six minors; triage and outcome:

- **Major — T127 is the interim brake and the register does not say so**
  (the round-7 gating ask, narrowed to sequencing) — applied: the M1 exit
  row now records that T127 is the only operator brake on the cycle until
  the owner decision lands, that both are M1 tasks landing in the same
  milestone, and that the exit review verifies that sequencing; the
  carried-by cell names "T127 as interim brake".
- **Minor — "removing the task" sits unqualified under the FR-048
  anchor** — applied: the lever now reads "removing the task only once
  its partial data is preserved", so the register cannot be read as
  sanctioning the data loss FR-048 exists to prevent.
- **Minor — the Pending-decision row restates the cycle figure without
  the ceiling qualifier** — applied: "up to ~172,800", matching the
  register row.
- **Minor — the refusal Warn logs the stale snapshot state under an
  unqualified key** — applied: the key is `snapshot_state` now; in the
  moved-row race the value is the pre-move state by construction, and
  the name says so.
- **Minor — the counting wrapper does not front the admitter's store
  handle** — applied: the parked subtests build a fresh admitter over the
  wrapper too, mirroring the race test, and the comment says the wrapper
  fronts both halves of the pair.
- **Minor — the race test can silently lose its window** — applied: a
  pre-Boot guard fails the test if the one-shot landing has already fired
  (a constructor listing the store would consume it).
- **Minor — the same fake is aliased for `NewReconciler`'s 2nd and 3rd
  parameters in `TestCancelledListIsNotUnreachable`** — declined: the
  finding misreads the signature. The parameters are `ts TaskWriter` and
  `admitter DiskFullPauser` — two distinct roles, not two stores — and
  the production wiring passes the task store and the admitter built over
  it. `fakeTasks` implements both interfaces, so passing it twice is the
  test-side form of the production shape, and the reviewer's own step 5
  covers this: leave it, note it.

Post-fix verdict: lint clean, every internal package `ok`, doclint 0
errors, routing suite 6/6 PASS — output identical to the single full
record (round 9's transcript) apart from timings; the run executed at the
round's fix tip, 31370f3. Scope: exactly the Files table, both gates.

### Review round 7 (commit a459aa4 → 0605ca4)

One major, four minors, three infos. Applied: the `writeBack` early return
that skips frozen progress writes on a parked disk-full row
(`UpdateProgress` has no equal-values early-out — its own comment says so),
pinned at a `progressCountingStore` boundary; the pre-commit caveat on the
scope gates; the fake `PauseDiskFull` panicking loudly; the race test's
snapshot-state capture; the ceiling qualifier on the cycle figure; the
Status/`Pending decision` split; `assertPartialUntouched` for the four
duplicated partial-file blocks. Declined: the T127 release-gating sentence
— superseded by round 8's narrower sequencing note, which was applied.
Verification clean at 0605ca4; scope exact.

### Review round 6 (commit a2cd06c → a459aa4)

Two majors, six minors, two infos, one outside-diff. Applied: the register
row's T127-interaction sentence; the `DiskFullPauser` concurrency contract
comment (verified: `Admitter` holds no mutable state); the cause naming
the engine with no dangling separator; the parked-row snapshot-inclusion
guard; the race-test wrapper comment; the fix-tip citations; the
docs-side Verification gate. Declined: the Warn/Debug split for refusals
(4th raise) — the typed refusal sentinel lives in
`internal/engine/admission.go`, outside the Files table, and the task
forbids changing `PauseDiskFull`; a refusal writes nothing. Verification
clean at a459aa4; scope exact.

### Review round 5 (commit c4cd620 → a2cd06c)

Two majors, six minors, three infos. Applied: the pending decision moved
into the Status cell (moved again to its own `Pending decision` header row
in round 7); the exit row anchors its subject by title; the docs-side
scope evidence; the refusal Warn naming the row state; the rejected-pause
test's no-remove/partial assertions; the stacked-comment consolidation
with the dead recording fields deleted; the transcript-collapse
convention (one full record, the latest). Closed as a documentation note:
the ErrorCode-only routing needs no state guard — aria2 exposes errorCode
only for stopped/completed downloads (§4.4), so a live transfer cannot
carry code 9. Declined: ineligible-refusal Warn every sweep (3rd raise,
as major) — same grounds as round 6's 4th raise; a refusal writes no
store UPDATE, the drop is the specification. Verification clean at
a2cd06c; scope exact.

### Review round 4 (commit 8294d6c → c4cd620)

One major, three minors, two infos, one outside-diff. Applied: the "M1
exit:" register row putting the owner decision structurally on the exit
review's agenda; the narrow `DiskFullPauser` interface replacing the
concrete `*Admitter` field; the table-driven constructor-panic test over
all three nils; the atomic `fired` flag; the Boot/Run share-one-sweep
comment. Declined: warn-every-sweep (3rd raise) — both suggested variants
verified against the task file (the sentinel needs `admission.go`, the
state pre-filter breaks the specified queued-row engine stop). Declined:
parked rows clobbered by non-disk-full engine errors — out of scope,
T026's generic adoption path and T127's park authority. Verification
clean at c4cd620; scope exact.

### Review round 3 (commit 247ac05 → 8294d6c)

One major, five minors, four infos. Applied: the carried-by cell naming
the surfacing mechanism ("surfaced at the M1 exit review"); the
constructor panicking on any of the three nils; the panic-message
assertion; the bare-`FAIL` stress evidence — `go test -race -count=20
./internal/engine/`, 20 consecutive clean full-package runs, 348s.
Declined: warn-every-sweep (2nd raise) — the state guard would break the
task file's own queued-row specification, terminal rows are unreachable
(`ListNonTerminalByEngine` excludes them), and the sentinel still needs
`admission.go`. Declined: `fakeEngine.Resume` error injection —
`admitEngine.resumeErr` already covers the rejected-resume path in
admission_test.go (T099's surface); an unused hook here is speculative.
Verification clean at 8294d6c; scope exact.

### Review round 2 (commit 0efe91d → 247ac05)

One major, three minors, one info. Declined: the resume→pause cycle ships
unbounded (2nd raise) — the out-of-scope bullet forbids adding backoff
here and forbids touching `PauseDiskFull` or the admission pass (T099
owns them), and T099's round-16 disposition forbids inventing a carrier
task; the owner decision was registered instead. The factual sub-claim
was applied: the cycle is up to ~172,800 rows/day at 1 Hz (the release
writes its own `task.resumed` row), and the register row says so.
Declined: expected refusals Warn once per second (1st raise) — the typed
sentinel needs `internal/engine/admission.go`, outside the Files table.
Applied: the `sweepPartialContent` constant; the parked subtests'
no-remove/partial assertions; the shared `taskStore` hoist in server.go.
One bare `FAIL` for `internal/engine` did not reproduce across ~15
reruns or CI; the documented T099 shared-tempfs flake class is the known
suspect — if it recurs, capture the full output before attributing.
Verification clean at 247ac05; scope exact.

### Review round 1 (commit f91b493 → 0efe91d)

One major, three minors, one info, one outside-diff. Declined: the
unbounded cycle with no carrier (1st raise) — same grounds as round 2's
2nd raise; the register row carries the magnitude, the detection signal
(T024's served event log) and the pending owner decision. Applied: the
constructor's named nil-admitter panic; the mutex-guarded fake accessors;
the pause-attempt pinning; the queued-drop test's no-unlink assertions.
The race-with-operator-pause question was answered with
`TestDiskFullReportRacingAnOperatorPauseWritesNothing`, confirming for
T127 that the refusal writes no event row of the guard's own.
Verification clean at 0efe91d; scope exact.

### Initial run (commit f91b493, before the review)

`make lint && make test PKG=./internal/...` clean, every internal package
`ok`, no `FAIL`; the four routing tests PASS, and all four were verified
to FAIL with the routing call disabled (the generic `error` adoption
restored), so none is vacuous. Scope: exactly the three code paths of the
Files table; doclint 0 errors.

## Blocked

None. The discharged deferral-register row ("An aria2 disk-full report
(errorCode 9) pauses the task instead of erroring it", carried by T126) was
removed with this task, and the backoff/cycle-counter follow-up of the
out-of-scope bullet was registered in its place.
