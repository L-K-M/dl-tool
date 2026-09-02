# T098 — Admit tasks under the concurrency limits

| Field | Value |
|---|---|
| **ID** | T098 |
| **Milestone** | M1 |
| **Status** | todo |
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
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
