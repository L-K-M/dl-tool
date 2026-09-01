# T012 — Run the database-backed job worker pool

| Field | Value |
|---|---|
| **ID** | T012 |
| **Milestone** | M0 |
| **Status** | todo |
| **Depends on** | T006 |
| **Blocks** | T074, T076, T077 |
| **Parallel-safe** | no — it edits `cmd/dl-tool/main.go` |
| **Implements** | — (the mechanism behind [FR-100](../02-requirements.md#fr-100-auto-extract-the-supported-archive-formats) and [FR-103](../02-requirements.md#fr-103-move-completed-data-across-filesystems), both verified in M6) |
| **Decisions** | [ADR-0015](../decisions/0015-db-backed-in-process-job-queue.md) |
| **Est. size** | 2 new source files, 1 test file, ~300 LOC |

## Goal
An in-process pool claims rows from the `jobs` table one at a time with `UPDATE … RETURNING`, runs the
handler registered for the row's `kind`, and retries with the documented backoff until `max_attempts`, after
which the row is `failed` and stays as its own dead-letter record. Anything left `running` by a crash is
returned to `pending` at boot.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/04-data-model.md` §3.6 Jobs, schedule and preferences](../04-data-model.md#36-jobs-schedule-and-preferences)
   — the `jobs` DDL, the four worker rules and the exact claim statement.
2. [`docs/03-architecture.md` §8.5 Job queue and at-least-once semantics](../03-architecture.md#85-job-queue-and-at-least-once-semantics).
3. [`docs/04-data-model.md` §4.4 `jobs.state`](../04-data-model.md#44-jobsstate).
4. [`docs/14-conventions.md` §2.4 SQL and sqlx](../14-conventions.md#24-sql-and-sqlx).

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/store/jobs.go` | create | Enqueue, claim, complete, fail and boot recovery. |
| `internal/jobs/worker.go` | create | The pool, the handler registry and the backoff. |
| `internal/jobs/worker_test.go` | create | Claim, retry, backoff, terminal-failure and recovery tests. |
| `cmd/dl-tool/main.go` | edit | Start the pool in `OnStart`, drain it in `OnStop`. |

No other file may be modified.

## Interface contract

```go
package store

type Job struct {
	ID          string  `db:"id"`
	Kind        string  `db:"kind"`
	TaskID      *string `db:"task_id"`
	PayloadJSON string  `db:"payload_json"`
	State       string  `db:"state"`        // pending | running | done | failed
	Attempts    int     `db:"attempts"`
	MaxAttempts int     `db:"max_attempts"`
	RunAfter    int64   `db:"run_after"`    // unix ms
	LockedAt    *int64  `db:"locked_at"`
	LastError   *string `db:"last_error"`
}

// EnqueueJob inserts one pending row with id store.NewID(store.PrefixJob).
func EnqueueJob(ctx context.Context, db *sqlx.DB, kind string, taskID *string, payload any, runAfter int64) (string, error)

// ClaimJob runs the UPDATE ... RETURNING of 04-data-model.md section 3.6 against the
// oldest eligible row. It returns ErrNotFound when nothing is eligible.
func ClaimJob(ctx context.Context, db *sqlx.DB, now int64) (Job, error)

// CompleteJob sets state='done'.
func CompleteJob(ctx context.Context, db *sqlx.DB, id string, now int64) error

// FailJob records lastErr and either reschedules with run_after, or sets
// state='failed' once attempts >= max_attempts.
func FailJob(ctx context.Context, db *sqlx.DB, id string, attempts, maxAttempts int, lastErr string, now int64) error

// RecoverRunningJobs runs UPDATE jobs SET state='pending', locked_at=NULL WHERE state='running'.
func RecoverRunningJobs(ctx context.Context, db *sqlx.DB) (int64, error)
```

```go
package jobs

// Handler runs one job. It must be idempotent, keyed on (kind, task_id): a job may
// run twice and running twice must be harmless.
type Handler func(ctx context.Context, j store.Job) error

// Worker is the in-process pool over the jobs table.
type Worker struct{ /* db, log, handlers, size, poll */ }

func NewWorker(db *sqlx.DB, log *slog.Logger, size int) *Worker

// Register binds a handler to a jobs.kind. Registering a kind twice panics at boot.
func (w *Worker) Register(kind string, h Handler)

// Run recovers stranded rows, then claims and runs jobs until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error

// Backoff is run_after = now_ms + min(600000, 5000 * 2^attempts).
func Backoff(now int64, attempts int) int64
```

Claim statement, verbatim from doc 04 §3.6:

```sql
UPDATE jobs
   SET state = 'running', locked_at = :now, attempts = attempts + 1, updated_at = :now
 WHERE id = (SELECT id FROM jobs
              WHERE state = 'pending' AND run_after <= :now
              ORDER BY run_after LIMIT 1)
RETURNING id, kind, task_id, payload_json, attempts, max_attempts;
```

## Steps
1. Create `internal/store/jobs.go` with the struct and the six functions above, each using an explicit
   column list and a context-carrying method.
2. Implement `ClaimJob` with the statement above, unchanged. One claim per call keeps the single writer
   connection of doc 04 §1.3 uncontended.
3. Implement `FailJob`: when `attempts >= maxAttempts` set `state='failed'` and leave the row in place;
   otherwise set `state='pending'`, `locked_at=NULL` and `run_after=Backoff(now, attempts)`.
4. Create `internal/jobs/worker.go` with `NewWorker`, `Register`, `Run` and `Backoff`. `Backoff` caps at
   600 000 ms exactly.
5. `Run` first calls `RecoverRunningJobs` and logs the count at `info`, then starts `size` goroutines, each
   claiming in a loop and sleeping 1 second when `ClaimJob` returns `ErrNotFound`.
6. A job whose `kind` has no registered handler is failed immediately with a named error; it is not retried
   into a loop.
7. Recover from a handler panic, convert it into an error and route it through `FailJob`, so one bad handler
   cannot stop the pool. Log with the `task_id` attribute when the job carries one.
8. `Run` returns only after every in-flight handler has finished or `ctx` has expired, so `OnStop` drains
   cleanly.
9. Edit `cmd/dl-tool/main.go` to build the worker with size 2, start `Run` in a goroutine in `OnStart`, and
   cancel its context and wait in `OnStop`.
10. Write `internal/jobs/worker_test.go` against a temporary SQLite database from `store.Open`, covering: an
    enqueued job runs exactly once; a failing job is rescheduled with the expected `run_after`; the
    `attempts >= max_attempts` job ends `failed` and stays; a row stuck in `running` is recovered at boot; an
    unknown kind fails immediately; `Backoff(now, 8)` equals `now + 600000`; and a panicking handler does not
    stop the pool.
11. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestClaimIsExactlyOnce` asserts two concurrent workers never claim the same row.
- [ ] `TestBackoffLadder` asserts 5 000, 10 000, 20 000 ms and the 600 000 ms cap.
- [ ] `TestTerminalFailureKeepsRow` asserts `state='failed'` and that the row is not deleted.
- [ ] `TestRecoverRunningJobsAtBoot` asserts a stranded row returns to `pending` with `locked_at` NULL.
- [ ] `TestPanicInHandlerIsContained` asserts the pool keeps claiming afterwards.
- [ ] The claim statement in `internal/store/jobs.go` is byte-identical to doc 04 §3.6.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make test PKG="./internal/jobs/... ./internal/store/..." && echo JOBS_OK
```
Expected: `ok  	github.com/L-K-M/dl-tool/internal/jobs` and `ok  	github.com/L-K-M/dl-tool/internal/store`,
no `FAIL` and no `DATA RACE`, and a final line of exactly `JOBS_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT write any handler. `internal/jobs/handlers_extract.go` is T074, `handlers_move.go` is T076 and
  `handlers_notify.go` is T077.
- Do NOT create `internal/jobs/cron.go` or add a `robfig/cron/v3` schedule; T066 adds the first periodic
  entry when RSS polling needs one.
- Do NOT add retention pruning of `done` rows; T091 owns the nightly cron of doc 04 §7.
- Do NOT write a `task_events` row on terminal failure yet; `internal/store/events.go` is created by T017
  and connected to job failures by T024.
- Do NOT add a broker, a queue library or Redis; the table is the queue.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
