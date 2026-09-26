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
| `cmd/dl-tool/main.go` | modify | `OnStop` reduces to `close(stopped)` + `<-drained`; `shutdownDrain` gains the runtime-drain step and orders it: `MarkDraining` → begin `httpServer.Shutdown` → await the listener-close boundary → runtime drain → join live conn goroutines → `server.Shutdown()` → `Engines.CloseAll()` → `db.Close()`. A `RegisterOnShutdown` callback supplies the ingress-stop boundary, a `ConnState` counter the conn join; an exhausted conn-join budget returns before the teardown tail. |
| `cmd/dl-tool/main_test.go` | modify | Drive the drain with an injected runtime-drain step and a listener whose `Close` can be held: the runtime step must not run until `Close` completes, and must then see the draining 503 and an immediately refused Dial; overrun and wedged-handler cases pin the escalation, the bounded conn join, and the fail-safe that leaves engines and the store open on a live handler. |
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
4. Start `httpServer.Shutdown` on a helper goroutine immediately after `MarkDraining`, and register a
   `RegisterOnShutdown` callback that closes an `ingressStopped` channel — callbacks run inside
   `Shutdown` after `closeListenersLocked` returns, so awaiting that channel is the hard barrier:
   the runtime drain cannot begin while the listener `Close` is still in flight. Run the runtime
   drain, then grant in-flight requests a fresh `shutdownTimeout` grace — measured after the drain,
   so a slow drain cannot shrink it — escalate an overrun to `httpServer.Close()`, and join the conn
   goroutines via `liveConns` before `server.Shutdown()`. If the conn join exhausts the same budget,
   the handler is still live: return without running `server.Shutdown()`, `Engines.CloseAll()` or
   `db.Close()` — process exit reaps what remains — rather than closing resources under a live
   request and claiming a safe teardown. Keep the T130 tail unchanged on the healthy path:
   `server.Shutdown()` → `Engines.CloseAll()` → `db.Close()`.
5. After Verification passes, paste its output under Evidence, set this file's Status to `done`,
   and flip both status cells in [`00-task-index.md`](00-task-index.md). Commit them with the work.

## Acceptance criteria

- [ ] During the drain, `GET /readyz` answers 503 `problem+json` with the draining detail — observed
      mid-drain in the drain test, not just after.
- [ ] The listener `Close` completes before the runtime drain starts — pinned by a listener whose
      `Close` is held during the drain: the injected drain step never runs while `Close` is blocked
      and observes the completed close plus an immediately refused Dial once it is.
- [ ] Runtime workers (scheduler, job pool, metrics) still drain to completion before
      `server.Shutdown()`, engine close and `db.Close()` run — the join order is unchanged.
- [ ] In-flight SSE connections keep the existing bounded `shutdownTimeout` drain; no new timers or
      grace windows are invented.
- [ ] A handler that overruns the `Shutdown` budget is force-cancelled and its conn goroutine joined
      before engines close — `TestShutdownDrainForceClosesOverrunningHandler` fails
      deterministically when the `httpServer.Close()` escalation is removed.
- [ ] A handler that ignores its request context cannot hang the drain, and the drain cannot close
      resources under it: after the bounded conn join exhausts, the drain returns before
      `server.Shutdown()`, engine close and `db.Close()` with an explicit incomplete-drain log —
      `TestShutdownDrainWedgedHandlerLeavesResourcesForProcessExit` asserts the engine's `Close` was
      never called and `db.PingContext` still succeeds while the handler is held.
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
ok  	github.com/L-K-M/dl-tool/cmd/dl-tool	4.084s
go test -race -count=1 ./internal/obs/...
ok  	github.com/L-K-M/dl-tool/internal/obs	1.312s
```

The HTTP-overrun case — an in-flight handler parked on its request context outliving the grace
budget — is pinned by `TestShutdownDrainForceClosesOverrunningHandler`. `httpServer.Shutdown` runs
with `context.Background()` so the listener closes at once but the in-flight grace window is
measured by a fresh `shutdownTimeout` after the runtime drain — the runtime drain cannot shrink it,
matching the pre-T132 budget semantics. On overrun the drain escalates to `Server.Close`
(reaping sockets, cancelling request contexts) and then joins the conn goroutines handlers run on
through a `ConnState`-driven `liveConns` counter (Add at `StateNew`, Done at
`StateClosed`/`StateHijacked`) — bounded by one more `shutdownTimeout` — before
`server.Shutdown()`. The engine's `Close` therefore asserts the handler exited with a non-blocking
check, not a wait.

Red observed on the final shape by removing the `httpServer.Close()` escalation: the parked conn
holds `Shutdown(context.Background())` forever and the drain wedges at `<-httpDone` — a `go test
-timeout 15s` dump shows `net/http.(*Server).Shutdown` parked in select at main.go's drain
goroutine:

```
goroutine 121 [select]:
net/http.(*Server).Shutdown(...)
github.com/L-K-M/dl-tool/cmd/dl-tool.shutdownDrain.func1()
	.../cmd/dl-tool/main.go:474
FAIL	github.com/L-K-M/dl-tool/cmd/dl-tool	15.023s
```

(The earlier pre-join/pre-restructure variant of the same test failed at 2.5s inside the engine's
bounded wait — the failure is identical in kind: the overrunning handler reaches engine teardown.)

The ingress-stop barrier is pinned by the held-listener shape in
`TestShutdownDrainStopsIngressBeforeRuntimeDrain`: `blockingListener.Close` signals entry, waits on
a release channel, closes the underlying listener, then signals completion. Running against the pre-
barrier drain — `go Shutdown(); drainRuntime()` — the injected step fired while `Close` was still
held, and the drain never finished after release because the failed `require` aborted the drain
goroutine:

```
main_test.go:234: the runtime drain ran while the listener close was still held
main_test.go:244: the drain never finished after the listener close released
--- FAIL: TestShutdownDrainStopsIngressBeforeRuntimeDrain (5.07s)
```

With the barrier — a `RegisterOnShutdown` callback closing `ingressStopped`, awaited before
`drainRuntime` — the held `Close` keeps the runtime step parked for the full 200 ms assertion
window, and after release the step observes `bl.completed` already closed, the draining 503, and a
single immediately refused Dial. The test exits in 0.24 s. An abort probe (`t.Fatal` while `Close`
is held) exits in 0.07 s: the release is a `defer`, so it runs before the `httpServer.Close`
cleanup that would otherwise block on the held listener.

The wedged-handler fail-safe is pinned by `TestShutdownDrainWedgedHandlerLeavesResourcesForProcessExit`:
against the pre-fix drain (join timeout logged, teardown proceeded) the regression failed with the
engine closed under a live handler:

```
main_test.go:356:
    Error:     Not equal: expected: 0 actual: 1
    Messages:  engines must not close while a live handler could still reach them
--- FAIL: TestShutdownDrainWedgedHandlerLeavesResourcesForProcessExit (0.16s)
```

After the fix the drain returns within two `shutdownTimeout` budgets (grace + join, 50 ms each in
the test) with an explicit incomplete-drain log; while the handler is still held, `engine.calls` is
0 and `db.PingContext` succeeds — `server.Shutdown()`, `Engines.CloseAll()` and `db.Close()` never
ran, and the process exit reaps them.

A full `go test -count=1 ./...` also passed — every package `ok`, no `FAIL`, no `DATA RACE`.
