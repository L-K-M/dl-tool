# T132 — Withdraw readiness and stop HTTP before the runtime drain

| Field | Value |
|---|---|
| **ID** | T132 |
| **Milestone** | M7 |
| **Status** | done |
| **Depends on** | T130 |
| **Blocks** | — |
| **Parallel-safe** | no — it edits `cmd/dl-tool/main.go`, the composition root's shutdown path |
| **Implements** | [docs/17-operations-and-runbook.md §2](../17-operations-and-runbook.md#2-shutdown) steps 1–2 — readiness withdrawal and ingress close ahead of the worker drain |
| **Decisions** | [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md) — one process owns shutdown ordering |
| **Est. size** | ~90 LOC including tests |

## Goal

The audit confirmed two deviations from the documented shutdown order:

- `/readyz` can never return 503 during shutdown — `Health.MarkReady` is one-way
  (`internal/obs/health.go:42`) and nothing withdraws readiness, so a proxy keeps routing traffic
  until the listener itself dies.
- The runtime drain runs before ingress closes: `OnStop` calls `cancelRun()` and `runDone.Wait()`
  first, so the cron scheduler and job workers finish draining while the HTTP listener still accepts
  requests — the inverse of doc 17 §2's order (stop accepting HTTP and report 503 first, then drain).

Wire the documented sequence inside `shutdownDrain`: mark draining, begin closing ingress, run the
runtime drain, then join the listener close before the existing loop/engine/store tail.

## Context you need

1. [`docs/17-operations-and-runbook.md` §2](../17-operations-and-runbook.md#2-shutdown) — the ordered
   sequence this lands: readiness/ingress first, scheduler and jobs second, engines and store last.
2. [`internal/obs/health.go`](../../internal/obs/health.go) — `MarkReady` is one-way; `Ready` answers
   `problem+json` 503 with a `detail` distinguishing the cause.
3. [`cmd/dl-tool/main.go`](../../cmd/dl-tool/main.go) — `stopped`/`drained` are the happens-before
   edges between `OnStop` and the `OnStart` goroutine that owns `httpServer`, `server`, `db`,
   `cancelRun` and `runDone`. Reading them from `OnStop` is a racy cross-goroutine access; keep the
   whole sequence in the owning goroutine.
4. `http.Server.Shutdown` closes the listener immediately and then waits for in-flight connections —
   an open SSE stream (`internal/api/sse.go` `Events` parks on the request context) can hold it for
   the full `shutdownTimeout` budget, so the close runs beside the runtime drain, not serialized
   ahead of it.

## Files

| Path | Action | Purpose |
|---|---|---|
| `internal/obs/health.go` | modify | `draining` flag + `MarkDraining()`; `Ready` answers 503 with a draining detail once marked. |
| `internal/obs/health_test.go` | modify | `MarkDraining` yields 503 `problem+json` with the draining detail even while the database answers. |
| `cmd/dl-tool/main.go` | modify | `OnStop` reduces to `close(stopped)` + `<-drained`; `shutdownDrain` gains the runtime-drain step and orders it: `MarkDraining` → begin `httpServer.Shutdown` → runtime drain → join listener close → join live conn goroutines → `server.Shutdown()` → `Engines.CloseAll()` → `db.Close()`. A `ConnState` counter on the server supplies the conn join. |
| `cmd/dl-tool/main_test.go` | modify | Drive the drain with an injected runtime-drain step that probes mid-drain state: readiness is already withdrawn and the listener already refuses new connections; overrun and wedged-handler cases pin the escalation and bounded conn join. |
| `docs/tasks/T132-readyz-drain-and-shutdown-order.md` | modify | Set Status to `done` and paste the verification output under Evidence (step 5). |
| `docs/tasks/00-task-index.md` | modify | Flip both of this task's status cells (the M7 row and the roster row) to `done` (step 5); touch no other row except the identifier-order note's M7 enumeration. |

No other file may be modified.

## Steps

1. Extend `shutdownDrain` to take the runtime drain as a parameter and call it first — this keeps
   today's effective order (runtime drain before ingress close) while `OnStop` shrinks to
   `close(stopped)` + `<-drained`. Behavior-preserving refactor; existing drain tests updated only
   for the signature.
2. Write the ordering test and observe it FAIL: the injected runtime-drain step probes
   `server.Health.Ready` and the live listener mid-drain. Today readiness is never withdrawn (the
   probe answers not-ready for the wrong reason — the nil-db failure, not draining) and the listener
   still accepts connections, so both assertions fail deterministically.
3. Add `draining`/`MarkDraining`/`detailDraining` to `obs.Health` and mark it first inside
   `shutdownDrain`; the drain-step probe then sees the draining 503.
4. Start `httpServer.Shutdown` on a helper goroutine immediately after `MarkDraining`, run the
   runtime drain, then join the listener close before `server.Shutdown()`. The listener refuses new
   connections as soon as `Shutdown` begins, so ingress truly stops before workers finish draining;
   in-flight SSE connections still get the existing `shutdownTimeout` budget, joined before the loop
   teardown. Keep the T130 tail unchanged: `server.Shutdown()` → `Engines.CloseAll()` → `db.Close()`.
5. After Verification passes, paste its output under Evidence, set this file's Status to `done`,
   and flip both status cells in [`00-task-index.md`](00-task-index.md). Commit them with the work.

## Acceptance criteria

- [ ] During the drain, `GET /readyz` answers 503 `problem+json` with the draining detail — observed
      mid-drain in the drain test, not just after.
- [ ] The listener refuses new connections before the runtime drain returns — probed from inside the
      injected drain step.
- [ ] Runtime workers (scheduler, job pool, metrics) still drain to completion before
      `server.Shutdown()`, engine close and `db.Close()` run — the join order is unchanged.
- [ ] In-flight SSE connections keep the existing bounded `shutdownTimeout` drain; no new timers or
      grace windows are invented.
- [ ] A handler that overruns the `Shutdown` budget is force-cancelled and its conn goroutine joined
      before engines close — `TestShutdownDrainForceClosesOverrunningHandler` fails
      deterministically when the `httpServer.Close()` escalation is removed.
- [ ] A handler that ignores its request context cannot hang the drain: the conn join is bounded by
      one more `shutdownTimeout` and teardown proceeds with an explicit error log —
      `TestShutdownDrainBoundedJoinOfWedgedHandler`.
- [ ] The ordering test failed deterministically before the fix (step 2 output in Evidence).

## Verification

```bash
make lint && make vet && make test PKG=./cmd/... && make test PKG=./internal/obs/...
```

Expected: lint and vet clean; `ok` for `cmd/dl-tool` and `internal/obs`, no `FAIL`, no `DATA RACE`.

## Out of scope — do NOT

- Do NOT add a dedicated task-state/`engine_ref` flush (doc step 4): the reconciler writes
  continuously and boot reconciliation recovers stranded rows; the audit found no lost-state
  evidence.
- Do NOT close SSE subscriber channels on hub exit or otherwise redesign SSE teardown; preserve the
  existing bounded drain.
- Do NOT move `Engines.CloseAll` ahead of `server.Shutdown()`: the reconciler and admission pass
  must be dead before adapters read as closed (T130's ordering).
- Do NOT pause or remove engine-side transfers at shutdown.
- Do NOT change the runbook; the implementation follows doc 17 §2.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under
  "Blocked".

## Forbidden shortcuts

- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add a lint-suppression directive or assign an error to `_`; fix the cause.
- Do NOT use `time.Sleep` to order goroutines — the injected drain step and channel joins are the
  synchronization.

## Evidence

Step 2, deterministic red — with the drain keeping today's order and no `MarkDraining` existing, the
mid-drain probe sees the pre-migration 503, never the draining one:

```
--- FAIL: TestShutdownDrainStopsIngressBeforeRuntimeDrain (0.36s)
    main_test.go:144:
        Error:       "{\"type\":\"/problems/not-ready\",\"title\":\"Not ready\",\"status\":503,\"detail\":\"migrations have not completed\"}" does not contain "shutting down"
        Test:        TestShutdownDrainStopsIngressBeforeRuntimeDrain
        Messages:    readiness must be withdrawn before the runtime drain runs
FAIL	github.com/L-K-M/dl-tool/cmd/dl-tool	1.719s
```

Steps 3–4, the task's Verification block (`make lint && make vet && make test PKG=./cmd/... &&
make test PKG=./internal/obs/...`), run on the final tree including the conn-join:

```
test -z "$(gofmt -l cmd internal)"          # clean
golangci-lint run ./...
0 issues.
cd web && npm run lint                       # clean
cd web && npx prettier --check .             # clean
go vet ./...                                 # clean
go test -race -count=1 ./cmd/...
ok  	github.com/L-K-M/dl-tool/cmd/dl-tool	3.602s
go test -race -count=1 ./internal/obs/...
ok  	github.com/L-K-M/dl-tool/internal/obs	1.345s
```

The HTTP-overrun case — an in-flight handler parked on its request context outliving the
`Shutdown` budget — is pinned by `TestShutdownDrainForceClosesOverrunningHandler`. `Server.Close`
cancels request contexts but never joins the conn goroutines handlers run on, so the drain carries
a `ConnState`-driven `liveConns` counter (Add at `StateNew`, Done at `StateClosed`/`StateHijacked`)
and joins it — bounded by one more `shutdownTimeout` — between the listener close and
`server.Shutdown()`. The engine's `Close` therefore asserts the handler exited with a non-blocking
check, not a wait.

Red observed on the final shape by removing the `httpServer.Close()` escalation — the conn never
closes, the bounded join expires, and the handler is still parked at engine close:

```
ERROR http shutdown failed err="context deadline exceeded"
ERROR live connections outlived force-close; teardown proceeds with stragglers
FAIL	github.com/L-K-M/dl-tool/cmd/dl-tool	0.317s
```

(The earlier pre-join variant of the same test failed at 2.5s inside the engine's bounded wait;
the failure is identical in kind.)

The wedged-handler bound is pinned by `TestShutdownDrainBoundedJoinOfWedgedHandler`: a handler that
ignores its request context costs the drain exactly one more `shutdownTimeout` (50 ms in the test,
~0.47s total) with an explicit error log, and teardown completes — the process is never held hostage
by a misbehaving handler.

A full `go test -count=1 ./...` also passed — every package `ok`, no `FAIL`, no `DATA RACE`.
