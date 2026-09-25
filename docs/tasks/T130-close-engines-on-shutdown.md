# T130 — Close registered engines before the database at shutdown

| Field | Value |
|---|---|
| **ID** | T130 |
| **Milestone** | M7 |
| **Status** | done |
| **Depends on** | T090 |
| **Blocks** | — |
| **Parallel-safe** | no — it edits `cmd/dl-tool/main.go`, the composition root's shutdown path |
| **Implements** | [docs/17-operations-and-runbook.md §2](../17-operations-and-runbook.md#2-shutdown) step 3 — engine teardown inside the ordered drain |
| **Decisions** | [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md) — one process owns shutdown ordering |
| **Est. size** | ~120 LOC including tests |

## Goal

`engine.Engine.Close` is a required interface member
(`internal/engine/engine.go`) with real teardown in all three adapters — yt-dlp kills live
subprocesses and closes subscriber channels, qBittorrent stops the sync/maindata poll goroutine and
closes its event channels, aria2 stops the Events polling loop and aborts an in-flight dial — but the
composition root never calls it. The drain in `cmd/dl-tool/main.go` runs `httpServer.Shutdown`,
`server.Shutdown()` and `db.Close()` and nothing else; the audit of ae072b5 confirmed no `Close()`
call site exists for any registered engine. This task wires the missing lifecycle step: after
`server.Shutdown()` has joined the loops that call into engines and before `db.Close()` releases the
store every adapter's `InfohashWriter`/event path could still touch, the registry closes every
registered engine exactly once and surfaces all of their errors.

## Context you need

1. [`docs/17-operations-and-runbook.md` §2](../17-operations-and-runbook.md) — the ordered shutdown
   sequence this lands inside.
2. [`internal/engine/engine.go`](../../internal/engine/engine.go) — the `Engine` interface contract;
   `Close` takes no context.
3. [`internal/engine/registry.go`](../../internal/engine/registry.go) — `Names()` gives sorted,
   deterministic iteration order.
4. [`internal/api/server.go`](../../internal/api/server.go) `Server.Shutdown` — joins the sync hub,
   reconciler and admission loops; engine calls cannot race `Close` after it returns.

## Files

| Path | Action | Purpose |
|---|---|---|
| `internal/engine/registry.go` | modify | `CloseAll` closes every registered engine once, in sorted-name order, returning `errors.Join` of all adapter errors so one failure never skips the rest. |
| `internal/engine/registry_test.go` | modify | Fake engines pin call-once, error aggregation and the empty registry. |
| `cmd/dl-tool/main.go` | modify | Extract the drain block (HTTP shutdown → `server.Shutdown()` → `db.Close()`) into a function; insert the engine close between the loop drain and the store close. |
| `cmd/dl-tool/main_test.go` | create | Drive the extracted drain with a real registry of recording fake engines and a real store: every `Close` runs while the database is still open, and the database is closed when it returns. |
| `docs/tasks/T130-close-engines-on-shutdown.md` | modify | Set Status to `done` and paste the verification output under Evidence (step 4). |
| `docs/tasks/00-task-index.md` | modify | Flip both of this task's status cells (the M7 row and the roster row) to `done` (step 4); touch no other row except the identifier-order note's M7 enumeration. |

No other file may be modified.

## Steps

1. Extract the drain block inline in `main`'s `OnStart` tail into a package-level function taking
   the HTTP server, the `*api.Server` and the `*sqlx.DB`. The move is strictly behavior-preserving:
   same sequence (`httpServer.Shutdown` → `server.Shutdown()` → `db.Close()`), same error logging,
   no engine interaction yet.
2. Write the close-order regression test against that function — the same one `OnStart` calls, not
   a copy — and observe it FAIL before touching `registry.go`: build `api.NewServer` with a nil db
   (starts no loops), register recording fake engines on `server.Engines`, open a real store on a
   temp path, and drive the drain. The test fails because no `Close` ever runs; it must not pass
   through a registry helper the drain never calls.
3. Add `func (r *Registry) CloseAll() error` — iterate `Names()` (already sorted), `Close` each,
   collect all errors with `errors.Join`, never abort the loop on a failure; take the mutex only to
   read the map, never across adapter calls. Call it between `server.Shutdown()` and `db.Close()`,
   log-and-continue on the joined error. Closing after the loop drain is deliberate: the reconciler
   and admission pass must be dead before any adapter reads as closed, or a mid-drain tick would
   stamp false engine errors on healthy daemon transfers. Daemon-side transfers are untouched — no
   engine `Pause` on shutdown (doc 17 §2 step 4). A fake `Close` in the drain test executes a
   trivial query on the still-open store, pinning the close-before-db ordering.
4. After Verification passes, paste its output under Evidence, set this file's Status to `done`,
   and flip both status cells in [`00-task-index.md`](00-task-index.md). Commit them with the work.

## Acceptance criteria

- [ ] Every engine registered on the composition root's registry receives exactly one `Close()` at
      shutdown, after `server.Shutdown()` joined the loops and before `db.Close()` releases the
      store — observed through the same function `OnStart` runs, not a test-only copy.
- [ ] One engine's `Close` error does not prevent the remaining engines from closing; the drain
      returns and logs the joined error and still closes the database.
- [ ] `CloseAll` on an empty or already-drained registry is a no-op returning nil.
- [ ] No engine `Pause`/`Remove` runs at shutdown; daemon transfers are unaffected.

## Verification

```bash
make lint && make vet && make test PKG=./cmd/... && make test PKG=./internal/engine/...
```

Expected: lint and vet clean; `ok` for `cmd/dl-tool` and `internal/engine`, no `FAIL`, no `DATA
RACE`.

## Out of scope — do NOT

The audit found three further deviations from doc 17 §2. This task deliberately does not fix them;
their impact and required proofs differ from the lifecycle gap, so each is recorded as follow-up
work rather than silently dropped:

- `/readyz` never returns 503 during shutdown — `Health.MarkReady` is one-way and no unmark exists.
  Impact: a health-checking reverse proxy cannot drain the instance early; connections die at
  `httpServer.Shutdown` instead of being refused during the job drain. Narrow fix: a
  `MarkNotReady`/`MarkDraining` flip called at `OnStop` entry, proven by a test asserting 503 while
  the drain is in progress (needs `internal/obs/health.go` + a drain-timing seam — outside this
  Files table).
- HTTP shutdown runs after the cron/worker drain, not first as step 1 orders. Impact: API requests
  — including `POST /tasks` — are still accepted while the job drain runs; such writes land safely
  and are picked up at next boot, so the deviation is observable but harmless. Folding it into the
  `/readyz` repair keeps the drain contract provable in one place.
- Doc step 4 "flush task state and engine_ref" has no dedicated implementation; the reconciler
  writes continuously and boot reconciliation recovers stranded rows. No evidence of lost state —
  not recorded as a defect.
- Do NOT change any adapter's `Close` implementation.
- Do NOT pause or remove engine-side transfers at shutdown.

## Forbidden shortcuts

- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add a lint-suppression directive or assign an error to `_`; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under
  "Blocked".

## Evidence

Step 2, red first — the drain test failed before `registry.go` was touched, because `Close` never
ran:

```
--- FAIL: TestShutdownDrainClosesEnginesBeforeDatabaseClose (0.03s)
    main_test.go:94: expected: 1 / actual: 0  ("Close must run exactly once in the drain")
--- FAIL: TestShutdownDrainClosesDatabaseWhenEngineCloseFails (0.03s)
    main_test.go:113: expected: 1 / actual: 0
--- FAIL: TestShutdownDrainStopsHTTPBeforeEnginesClose (0.03s)
    main_test.go:152: "the HTTP listener must be dead before engines close"
```

Step 3 onward, the task's Verification block (`make lint && make vet && make test PKG=./cmd/... &&
make test PKG=./internal/engine/...`):

```
test -z "$(gofmt -l cmd internal)"          # clean
golangci-lint run ./...
0 issues.
go vet ./...                                # clean
go test -race -count=1 ./cmd/...
ok  	github.com/L-K-M/dl-tool/cmd/dl-tool	2.351s
go test -race -count=1 ./internal/engine/...
ok  	github.com/L-K-M/dl-tool/internal/engine	33.420s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.294s
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	9.064s
ok  	github.com/L-K-M/dl-tool/internal/engine/ytdlp	1.384s
```

`make lint` additionally runs `cd web && npm run lint`, which cannot run in this worktree —
`web/node_modules` is not installed (`eslint: not found`). The web side was untouched by this task.
A full `go test ./...` across all packages also passed (all `ok`, no `FAIL`, no `DATA RACE`).
