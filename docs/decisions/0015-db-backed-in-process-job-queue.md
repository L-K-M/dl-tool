# 0015 - DB-backed in-process job queue

> **Status:** accepted
> **Date:** 2026-09-01
> **Deciders:** repository owner

## Context and Problem Statement

dl-tool has three kinds of background work. Pollers ask each engine for state every one to two seconds and
need no persistence. One-shot retryable work — extract an archive, move a completed file, fire a webhook,
run a yt-dlp job — must survive a restart, because a container that dies mid-extract must not lose the task.
Periodic work — RSS polling, `VACUUM INTO`, retention pruning, the 24×7 schedule tick — must fire on a
clock. What runs that work, given that [ADR-0004](0004-sqlite-as-the-only-datastore.md) rules out a second
datastore and the acceptance test is a plain `docker compose up`?

## Decision Drivers

- Every additional container is a service the operator must run, upgrade and back up, and a component a
  weaker model must diagnose when it breaks. The durable store already exists: a `jobs` table costs one
  migration, a broker costs a process, and the workload is a handful of jobs per completed task.
- Post-processing must be crash-safe. Losing a "move to library" job after a power cut is data loss from the
  user's point of view, even though no bytes were lost.
- Restart semantics must be written down, not discovered: a job claimed when the process died must run
  again, and running twice must be harmless.

## Considered Options

- **Option A** — In-process worker pool over a `jobs` table in SQLite, plus `robfig/cron/v3` for schedules.
- **Option B** — River v0.47.0, a mature Go job queue, on its SQLite backend.
- **Option C** — asynq v0.26.0 with a Redis container.
- **Option D** — No queue: do post-processing inline on the goroutine that noticed the task completed.

## Decision Outcome

Chosen option: **Option A, a `jobs` table plus an in-process pool**, because the durable store is already
there and the workload does not justify a broker or an experimental backend. `robfig/cron/v3 v3.0.1` handles
the clock; cron entries are code, and the only DB-driven schedules are the RSS interval and the bandwidth
grid, which the tick reads on each fire.

Three rules are binding, and the DDL lives in [`../04-data-model.md`](../04-data-model.md):

1. **At-least-once.** Handlers are idempotent, keyed on `task_id` plus `kind`. Running twice is harmless.
2. **Boot recovery.** `UPDATE jobs SET state='pending', locked_at=NULL WHERE state='running';` runs at
   start-up, so anything claimed but unfinished when the process died is retried.
3. **Backoff and terminal failure.** `run_after = now + min(600, 5 * 2^attempts)` seconds; at
   `attempts >= max_attempts` the row becomes `state='failed'` and a `task_events` row is written. There is
   no dead-letter queue — the `failed` rows are the dead-letter queue.

Claiming is one `UPDATE ... RETURNING` against the oldest eligible row — SQLite has supported `RETURNING`
since 3.35 — so a claim cannot race a read-then-write.

### Consequences

- Good, because the compose file gains no service, and the queue survives restarts and backups because it
  lives in the file the operator already backs up. A job's history — attempts, `last_error`, `run_after` —
  is inspectable with ordinary SQL and surfaced through `GET /system/logs`.
- Bad, because at-least-once puts the idempotency burden on every handler author: a non-idempotent extract
  handler produces duplicate files rather than an error. The design is also single-process by construction —
  two replicas against one database would double-claim, making ADR-0004's foreclosure of scale-out concrete.
- Bad, because SQLite writes are serialised, so a long transaction in a handler stalls the claim loop;
  handlers do I/O outside the transaction and write results in one short statement.
- Neutral, because this rejects a better-engineered library: River has unique jobs, periodic jobs and a UI
  dl-tool does without. If its SQLite driver graduates, migrating is contained — the handler is the seam.

### Confirmation

The three binding rules are unit-testable without any container:

```bash
make test PKG=./internal/jobs/...
grep -rniE "redis|riverqueue|asynq|machinery" go.mod
```

Expected: exit 0 from `make test`, covering boot recovery (a `running` row is re-queued), the backoff ladder
(`5, 10, 20, 40 …` capped at 600 s), the terminal transition to `failed` with its `task_events` row, and a
handler invoked twice producing one outcome. `grep` prints nothing and exits 1, so no broker client is in
the dependency set.

## Pros and Cons of the Options

### Option A - jobs table plus in-process pool

- Good, because there is one process to run, one file to back up and one place to look when a job is stuck,
  and `robfig/cron/v3 v3.0.1` has a frozen API — last released 2020-01-04 — that does exactly one thing.
- Bad, because dl-tool owns the claim loop, backoff and recovery statement — the parts a queue library has
  already debugged.

### Option B - River on SQLite

- Good, because it is a well-engineered queue with unique jobs, periodic jobs, subscriptions and a web UI.
- Bad, because it is Postgres-first and River's own documentation calls its SQLite backend "experimental …
  needs more vetting to be considered fully production ready".

### Option C - asynq plus Redis

- Good, because it is mature, has a good dashboard, and its at-least-once semantics are proven at scale.
- Bad, because Redis is a second stateful container to run and back up for a single-user home application,
  which fails the `docker compose up` acceptance test.

### Option D - no queue, inline post-processing

- Good, because it is the least code and the shortest path from completion to extracted files.
- Bad, because a restart mid-extract loses the work with no record it was pending, and a 10-minute `EXDEV`
  copy blocks the poller that keeps the UI live.

## More Information

- Research: `architecture.md` §3.1–§3.2 — [`../16-prior-art-and-research.md`](../16-prior-art-and-research.md).
- Depends on this decision: [`../03-architecture.md`](../03-architecture.md),
  [`../04-data-model.md`](../04-data-model.md), [`../17-operations-and-runbook.md`](../17-operations-and-runbook.md).
