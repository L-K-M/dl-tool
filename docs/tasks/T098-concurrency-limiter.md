# T098 — Admit tasks under the concurrency limits

| Field | Value |
|---|---|
| **ID** | T098 |
| **Milestone** | M1 |
| **Status** | done |
| **Depends on** | T017, T019, T020, T024, T026 |
| **Blocks** | T099 |
| **Parallel-safe** | no — it also edits the shared files `internal/api/tasks_actions.go`, `internal/store/tasks.go` |
| **Implements** | [FR-020](../02-requirements.md#fr-020-cap-the-number-of-concurrently-active-tasks), [FR-021](../02-requirements.md#fr-021-exclude-seeding-tasks-from-every-concurrency-limit), [FR-095](../02-requirements.md#fr-095-order-the-queue-by-creation-date) |
| **Decisions** | [ADR-0017](../decisions/0017-exclusive-control-of-engines.md), [ADR-0015](../decisions/0015-db-backed-in-process-job-queue.md) |
| **Est. size** | 2 new files, ~330 LOC |

## Goal
dl-tool is the only admission controller: it counts started tasks in total and per engine, and releases
queued tasks to their engine only while every applicable limit still has headroom. Tasks in state `seeding`
count toward neither limit.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/03-architecture.md` §6.4 Admission control](../03-architecture.md#64-admission-control--the-concurrency-limiter)
2. [`docs/05-api-contract.md` §5.11 Concurrency limits](../05-api-contract.md#511-concurrency-limits)
3. [`docs/04-data-model.md` §4.7 Concurrency limit versus disk space](../04-data-model.md#47-concurrency-limit-versus-disk-space)
4. [`docs/11-config-reference.md` §5 Database-backed settings](../11-config-reference.md#5-database-backed-settings)
5. [`docs/06-download-engines.md` §9.4 Why the engine queues are raised](../06-download-engines.md#94-why-the-engine-queues-are-raised-not-used)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/admission.go` | create | `Admitter`: the counts, the candidate query and the release loop. |
| `internal/engine/admission_test.go` | create | Headroom and seeding-exclusion cases against a fake `Engine`. |
| `internal/store/tasks.go` | modify | Add `CountActive` and `SelectQueuedCandidates`. |
| `internal/api/tasks_actions.go` | modify | Return `/problems/concurrency-limit` per id on a blocked `resume`. |

No other file may be modified.

## Interface contract

```go
package engine

// Limits are the two max_active_* settings keys of docs/11-config-reference.md §5. 0 means unlimited.
// The bandwidth pair is engine.RateLimits (T079); these two types are distinct on purpose.
type Limits struct {
	MaxActiveTotal     int
	MaxActivePerEngine int
}

// ActiveCounts is one snapshot of the counted set: tasks in state downloading, checking, extracting
// or moving. Tasks in state seeding are excluded from every count.
type ActiveCounts struct {
	Total    int
	ByEngine map[string]int
}

// Admitter releases queued tasks while every applicable limit has headroom.
type Admitter struct{ /* unexported */ }

func NewAdmitter(reg *Registry, ts AdmissionStore, tick time.Duration) *Admitter

// AdmissionStore is the store surface the admitter needs.
type AdmissionStore interface {
	CountActive(ctx context.Context) (ActiveCounts, error)
	// SelectQueuedCandidates returns queued tasks in process_order, oldest added_at first.
	SelectQueuedCandidates(ctx context.Context, limit int) ([]Candidate, error)
	Transition(ctx context.Context, id, next, code, message string) error
	SetErrorCode(ctx context.Context, id, errorCode, message string) error
	SetEngineRef(ctx context.Context, id, engineRef string) error
}

// Candidate is one queued task considered for release.
type Candidate struct {
	ID        string
	Engine    string
	EngineRef *string // nil when the task has never been handed to an engine
}

// Pass runs one admission pass and returns the ids it released. It is idempotent and safe to run
// concurrently with the reconciler.
func (a *Admitter) Pass(ctx context.Context, l Limits) ([]string, error)

// Run drives Pass on a ticker until ctx is cancelled.
func (a *Admitter) Run(ctx context.Context, load func(context.Context) (Limits, error)) error
```

The counted set, and the disk-space gate it must never be conflated with:

| | Concurrency limit | Disk space |
|---|---|---|
| Lives in | `settings` keys `max_active_total`, `max_active_per_engine` | `settings` key `min_free_space`, per data root |
| Counted set | tasks in `downloading`, `checking`, `extracting`, `moving`; `seeding` excluded | free bytes on the destination's filesystem |
| `POST /tasks` when breached | accepted, `201`, state `queued`, `error_code` `concurrency_limit` | accepted, `201`, state `queued`, `error_code` `disk_full` |
| `resume` when breached | per-id `/problems/concurrency-limit`, `409`, task stays `queued` | per-id `ok:false`, task stays `queued` |
| `0` means | unlimited | unlimited |

## Steps
1. Add `CountActive` to `internal/store/tasks.go` as one grouped query over the four counted states, with
   `seeding` excluded in SQL rather than in Go.
2. Add `SelectQueuedCandidates` ordered by `added_at ASC, id ASC`, returning at most the requested number.
3. Create `internal/engine/admission.go` with `Limits`, `ActiveCounts`, `Candidate`, `Admitter`,
   `NewAdmitter`, `Pass` and `Run`.
4. In `Pass`, read the counts once, then walk the candidates in order and release one only while
   `Total` and `ByEngine[engine]` both have headroom against a non-zero limit.
5. Release a candidate by calling `Engine.Add` when `EngineRef` is nil, or `Engine.Resume` when it is set,
   then transition to `downloading` with the code `task.resumed` and clear `error_code`.
6. Increment the in-memory counts after each release, so one pass cannot exceed a limit.
7. Leave every remaining candidate in `queued` and set `error_code` to `concurrency_limit` with
   `SetErrorCode`; never reject a task at creation time for concurrency alone.
8. Clear `concurrency_limit` on release, so a started task never carries a stale error code.
9. In `internal/api/tasks_actions.go` return a per-id `{"ok":false,"type":"/problems/concurrency-limit"}`
   when a `resume` has no headroom, leaving the task `queued` to start on its own once a slot frees.
10. Create `internal/engine/admission_test.go`: with `max_active_total=2` and `max_active_per_engine=1`,
    submit six tasks split across two engines and assert exactly two are released with at most one per
    engine; fill `max_active_total` with `seeding` tasks and assert a queued download still starts; with
    `max_active_total=1`, queue three tasks and assert exactly one starts, the rest stay `queued`, and
    every held task carries `concurrency_limit`.

## Acceptance criteria
- [ ] `max_active_total=2` with `max_active_per_engine=1` releases exactly two tasks, one per engine.
- [ ] Tasks in `seeding` are counted by neither limit.
- [ ] Every held task stays in `queued` with `error_code` `concurrency_limit`.
- [ ] A released task has its `error_code` cleared.
- [ ] `0` for any limit means unlimited for that dimension.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then `ok` lines for
`github.com/L-K-M/dl-tool/internal/store`, `github.com/L-K-M/dl-tool/internal/engine` and
`github.com/L-K-M/dl-tool/internal/api`, with `TestPassRespectsTotalAndPerEngine`,
`TestSeedingIsNotCounted` and `TestHeldTaskCarriesConcurrencyLimit` all running. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT check free space; T099 adds that gate to the same `Pass`.
- Do NOT invent any ordering other than creation date; this task orders by
  `added_at` only.
- Do NOT raise an engine's own queue limit; T101 owns conformance.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

`make lint && make test PKG=./internal/...` (the Verification block, run at the
final tree):

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
ok  	github.com/L-K-M/dl-tool/internal/api	45.270s
ok  	github.com/L-K-M/dl-tool/internal/config	1.108s
ok  	github.com/L-K-M/dl-tool/internal/engine	5.312s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.137s
?   	github.com/L-K-M/dl-tool/internal/fsx	[no test files]
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.320s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.191s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.222s
ok  	github.com/L-K-M/dl-tool/internal/store	63.229s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.354s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.021s
```

The three named tests ran and passed (with the five companion cases:
unlimited zeros, creation order, the resume path, the vanished-handle
re-add, the refusal and the unregistered engine):

```
$ go test -race -count=1 -run 'TestPassRespectsTotalAndPerEngine|TestSeedingIsNotCounted|TestHeldTaskCarriesConcurrencyLimit' -v ./internal/engine/
=== RUN   TestPassRespectsTotalAndPerEngine
--- PASS: TestPassRespectsTotalAndPerEngine (0.45s)
=== RUN   TestSeedingIsNotCounted
--- PASS: TestSeedingIsNotCounted (0.32s)
=== RUN   TestHeldTaskCarriesConcurrencyLimit
--- PASS: TestHeldTaskCarriesConcurrencyLimit (0.34s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/engine	2.169s
```

Scope check:

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
internal/api/tasks_actions.go
internal/engine/admission.go
internal/engine/admission_test.go
internal/store/tasks.go
```

Exactly the four paths of the Files table. No Huma operation or schema
changed, so `make gen` was not needed; no dependency was first-imported, so
`go.mod`/`go.sum` are untouched.

Review round 1 (PR [#91](https://github.com/L-K-M/dl-tool/pull/91)) — accepted:
a resume batch now consumes headroom per `ok:true` id
(`ActiveCounts.Reserve`), a failed stamp clear after a successful release
is a warning instead of routing the healthy task into `releaseFailed`,
failures of the pass's own writes (`storeWriteError`) stay queued instead
of being mislabeled `engine.rejected`, the staying-queued branches drop a
stale `concurrency_limit` stamp, `candidatesUnbounded` is `math.MaxInt`, a
negative settings value is rejected on read, `SetErrorCode` with an empty
code clears the message too, `resumeAction` guards its nil snapshot, and
the round-trip test pins the handle convention (Add returns the
namespaced id, the row stores the bare ref, Resume gets the namespaced
form again). Rejected, with the code as evidence: Resume already receives
the namespaced id (`internal/engine/aria2/client.go` returns it from `Add`
and `ref` strips both forms), the already-queued resume keeps its
idempotent success inside `transitionAction`'s own same-state early
return, and the deferred-transaction lock concern is already mitigated
store-wide by `_txlock=immediate` in the `store.Open` DSN. Re-run at the
final tree:

```
$ make lint && make test PKG=./internal/...
... (lint silent; golangci-lint: 0 issues; eslint and prettier clean)
ok  	github.com/L-K-M/dl-tool/internal/api	44.479s
ok  	github.com/L-K-M/dl-tool/internal/config	1.110s
ok  	github.com/L-K-M/dl-tool/internal/engine	6.132s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.156s
?   	github.com/L-K-M/dl-tool/internal/fsx	[no test files]
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.682s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.166s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.150s
ok  	github.com/L-K-M/dl-tool/internal/store	68.700s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.361s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.024s
```

## Blocked
The task did not stop, but one rule of [`docs/14-conventions.md`
§8.3](../14-conventions.md#83-wire-a-long-lived-component) could not be
satisfied: `Admitter.Run` has **no composition-root call site**, because
neither `internal/api/server.go` nor `cmd/dl-tool/main.go` is in this task's
Files table (“No other file may be modified”). Until a call site lands,
`Pass` is complete and correct — and the resume action answers headroom
from the same counts and limits — but nothing drives the pass on a ticker,
so a queued task still never reaches an engine and the M1 exit checkpoint
stays unreachable. The contract's `load func(context.Context) (Limits,
error)` parameter is the likely reason the wiring was left out: no
settings-key reader exists yet (`internal/store/settings.go` defers the
settings rows to T092), so the composition root has nothing to build
`load` from. T099 — the disk-space gate that joins this same `Pass` —
needs the same reader and is the natural carrier of both; if it does not
own them, this file's Files table needs `internal/api/server.go` added
and a follow-up that constructs the Admitter, builds `load` over the
settings rows and starts `Run` beside the reconciler's loop. The debt is
tracked structurally as a row of the task index's deferral register
(“M1 exit: the admission pass's ticker”), whose Carried-by cell binds the
wiring to T099 — including the Files-table amendment it needs — not only
to this paragraph.
