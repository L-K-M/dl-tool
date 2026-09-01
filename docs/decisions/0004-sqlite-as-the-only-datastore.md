# 0004 - SQLite as the only datastore

> **Status:** accepted
> **Date:** 2026-09-01
> **Deciders:** repository owner

## Context and Problem Statement

dl-tool manages downloads onto one machine's disks, so it is single-node by definition. Its write volume is
of the order of ten writes per second. The repository owner's acceptance test for the whole product is
*"I can host it wherever I want using docker compose"*, and every additional stateful service costs a
container, a healthcheck, a password, a volume, a backup story and a
`depends_on: condition: service_healthy` ordering problem. The question is whether any of that is ever
justified here.

## Decision Drivers

- Deployment must stay one `compose.yaml` with no service-ordering constraint the user can get wrong.
- `CGO_ENABLED=0` must hold ([ADR-0002](0002-go-for-the-backend.md)), so the driver has to be pure Go.
- SQLite's WAL mode does not work over a network filesystem — sqlite.org, verbatim: *"WAL does not work
  over a network filesystem."* NAS users bind-mount NFS and SMB by habit, so this is the single most
  likely production-corrupting mistake.
- The job queue ([ADR-0015](0015-db-backed-in-process-job-queue.md)) and the settings store write to the
  same file as the task poller, so writer/writer contention has to be designed for, not discovered.

## Considered Options

- **Option A** — SQLite only, `modernc.org/sqlite v1.57.0` (driver name `"sqlite"`), WAL, one file at
  `/config/dl-tool.db`.
- **Option B** — SQLite by default with an opt-in Postgres compose profile, as `deployment.md` proposed.
- **Option C** — Postgres only.
- **Option D** — SQLite for relational data plus an embedded key/value store (bbolt or badger) for the job
  queue and the event ring.

## Decision Outcome

Chosen option: **Option A, SQLite only**, because a second database is a second failure mode that a weaker
model cannot debug and that the product's stated deployment shape does not pay for. `modernc.org/sqlite` is
the SQLite C source transpiled to Go, so it needs no cgo and keeps the static binary that
[ADR-0011](0011-alpine-runtime-with-puid-pgid.md) puts into a roughly 2 MB base image.

Three configuration facts are part of the decision rather than of the implementation:

- WAL is set once and is persistent across reopen; `busy_timeout=5000`, `synchronous=NORMAL`,
  `foreign_keys=ON` and `_txlock=immediate` are set on the DSN. The DSN itself lives in
  [`../11-config-reference.md`](../11-config-reference.md).
- The pool is `SetMaxOpenConns(1)`. WAL removes reader/writer blocking, not writer/writer contention; at
  this write volume serialising every write removes `SQLITE_BUSY` outright. Optimise later, never first.
- dl-tool **refuses to start** when the directory holding the database is on `nfs`, `cifs`, `smb3` or
  `fuse.*`, and exits with a named error. There is no degraded `journal_mode=DELETE` fallback.

### Consequences

- Good, because `docker compose up -d` starts one stateful process, and backup is `VACUUM INTO` on a
  schedule plus a documented stop-copy-start restore
  ([`../17-operations-and-runbook.md`](../17-operations-and-runbook.md)).
- Bad, because there is exactly one writer: a long transaction in a job handler stalls the delta writer.
- Bad, because dl-tool cannot be run as two replicas against a shared store. Accepted: it manages one
  machine's disks.
- Bad, because the network-filesystem refusal is a hard failure the operator meets at first boot, and the
  error text is the only thing standing between them and silent corruption. `17` owns that message.
- Neutral, because Litestream is out of scope for v1: v0.5.x rewrote the format and there are reports of
  silent replication failure in v0.5.6/v0.5.7 (litestream#1083). A backup mechanism that fails silently is
  worse than none.

### Confirmation

```bash
make test PKG=./internal/store/... && ! grep -rqn "lib/pq\|jackc/pgx\|go-sql-driver/mysql" --include='*.go' cmd internal
```

Expected: exit 0. The store suite includes `TestJournalModeIsWAL`, `TestRefusesNetworkFilesystem` (a fake
mount table on `nfs` must produce the named startup error) and `TestMigrationsUpDown` over the embedded
`goose` migrations. The grep asserts no second SQL driver has been introduced.

## Pros and Cons of the Options

### Option A - SQLite only

- Good, because the whole datastore is one file that a user can copy, and the pure-Go driver keeps
  `CGO_ENABLED=0 go build` working.
- Good, because `VACUUM INTO '<path>'` produces a consistent snapshot of a live database, so backups need
  no external tooling. The destination must not already exist, or must be an empty file.
- Bad, because one writer means the concurrency ceiling is a design constant rather than a tuning knob.

### Option B - SQLite plus an opt-in Postgres profile

- Good, because a profile costs nothing when unused and would let a user reuse a Postgres they already run.
- Bad, because it doubles the schema dialect, the migration set and the integration matrix, and a profile
  that CI does not exercise on every commit is a profile that is already broken.

### Option C - Postgres only

- Good, because concurrent writers, real types and `pg_dump` are all better than SQLite's equivalents.
- Bad, because a mandatory second container contradicts the deployment shape the owner asked for, and
  `depends_on: condition: service_healthy` is a boot-ordering bug users report as "it does not start".

### Option D - SQLite plus an embedded key/value store

- Good, because the job queue and the event log are append-heavy and would fit a KV store naturally.
- Bad, because it produces two durability models, two backup stories and two failure modes inside one
  process — and the event ring is in memory anyway ([ADR-0006](0006-sse-with-rid-deltas.md)).

## More Information

- Research: `architecture.md` §2 and its fact-check — summarised in
  [`../16-prior-art-and-research.md`](../16-prior-art-and-research.md).
- Depends on this decision: [`../04-data-model.md`](../04-data-model.md),
  [`../10-deployment-and-compose.md`](../10-deployment-and-compose.md),
  [`../17-operations-and-runbook.md`](../17-operations-and-runbook.md).
