# T026 — Reconcile tasks with the engines at boot and on every poll

| Field | Value |
|---|---|
| **ID** | T026 |
| **Milestone** | M1 |
| **Status** | todo |
| **Depends on** | T019, T024, T025 |
| **Blocks** | T098 |
| **Parallel-safe** | yes — touches only `internal/engine/reconcile*.go` |
| **Implements** | [NFR-003](../02-requirements.md#nfr-003-resume-every-task-after-a-restart); the aria2 half of [FR-148](../02-requirements.md#fr-148-ignore-engine-tasks-dl-tool-did-not-create), which T102 verifies against qBittorrent |
| **Decisions** | [ADR-0017](../decisions/0017-exclusive-control-of-engines.md), [ADR-0006](../decisions/0006-sse-with-rid-deltas.md) |
| **Est. size** | 2 new files, ~300 LOC |

## Goal
After a restart every task resumes in the state it held, because dl-tool re-reads each engine's live list
and writes the differences back into `tasks`. A transfer dl-tool did not create is dropped on sight and
never becomes a task.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §8 Engine ownership](../06-download-engines.md#8-engine-ownership)
2. [`docs/06-download-engines.md` §4.3 Methods dl-tool calls](../06-download-engines.md#43-methods-dl-tool-calls)
3. [`docs/06-download-engines.md` §4.5 WebSocket notifications](../06-download-engines.md#45-websocket-notifications)
4. [`docs/17-operations-and-runbook.md`](../17-operations-and-runbook.md)
5. [`docs/03-architecture.md` §8.1 Task state machine](../03-architecture.md#81-task-state-machine)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/reconcile.go` | create | `Reconciler`: the boot sweep, the poll loop and the foreign-transfer filter. |
| `internal/engine/reconcile_test.go` | create | Boot-sweep, foreign-transfer and orphan cases against a fake `Engine`. |
| `internal/engine/aria2/client.go` | modify | Swap the polling `Events` for the WebSocket notification transport. |

No other file may be modified.

## Interface contract

```go
package engine

// Reconciler keeps the tasks table in step with the engines dl-tool owns. It is the only writer of
// engine-sourced task state.
type Reconciler struct{ /* unexported */ }

// NewReconciler wires the registry to the task store. poll is the sweep interval; 1s in production.
func NewReconciler(reg *Registry, ts TaskWriter, poll time.Duration) *Reconciler

// TaskWriter is the store surface the reconciler needs. internal/store.TaskStore satisfies it.
type TaskWriter interface {
	ListByEngineRef(ctx context.Context, engineName string) (map[string]string, error) // engine_ref -> task id
	UpdateProgress(ctx context.Context, id string, p Progress) error
	Transition(ctx context.Context, id, next, code, message string) error
}

// Boot runs one full sweep before the HTTP listener opens: every engine is listed, every known
// engine_ref is written back, every task whose handle the engine no longer knows is marked error
// with the code engine_unavailable, and every unknown handle is ignored.
func (r *Reconciler) Boot(ctx context.Context) error

// Run drives Boot and then a poll loop until ctx is cancelled. It is owned by the component that
// started it and stops with that context.
func (r *Reconciler) Run(ctx context.Context) error
```

Reconciliation rules, all of them:

| Situation | Action |
|---|---|
| Engine reports a handle dl-tool knows | Write the normalised `TaskInfo` back: state, byte counters, rates, ETA. |
| Engine reports a handle dl-tool does not know | Ignore it. It never enters `tasks`, counts toward no limit, and is never paused, relocated or deleted. |
| dl-tool holds a task whose handle the engine no longer knows | Transition to `error` with `error_code` `engine_unavailable` and one `engine.unavailable` event. |
| dl-tool holds a task with `engine_ref` still `NULL` | Leave it in `queued`; T098's admission pass owns starting it. |
| The engine is unreachable | Log at `warn`, retry on the next tick, change no task state. |

## Steps
1. Create `internal/engine/reconcile.go` with `Reconciler`, `TaskWriter`, `NewReconciler`, `Boot` and
   `Run`.
2. Implement `Boot` as one `Engine.List` per registered engine, joined against
   `ListByEngineRef` on the `engine_ref` value.
3. Drop every listed handle that is absent from the map, and add the comment that this is the whole of
   [ADR-0017](../decisions/0017-exclusive-control-of-engines.md): one rule, no options and no setting.
4. Write back the counters with `UpdateProgress`, and call `Transition` only when the normalised state
   differs from the stored one, so an unchanged task produces no event and no delta.
5. Mark a task whose handle has vanished as `error` with `engine_unavailable` and one
   `engine.unavailable` event.
6. Implement `Run` as `Boot` followed by a ticker at the configured interval, returning `ctx.Err()` on
   cancellation and never leaking the goroutine.
7. Log every unreachable engine at `warn` with the `engine` attribute, and change no task state on that
   path.
8. In `internal/engine/aria2/client.go` replace the polling `Events` implementation with a WebSocket
   client on the same host and path, reading the six notifications and mapping them to `TaskEvent` kinds:
   `onDownloadStart` to `started`, `onDownloadPause` to `paused`, `onDownloadStop` to `removed`,
   `onDownloadComplete` to `completed`, `onDownloadError` to `error`, `onBtDownloadComplete` to
   `progress`.
9. Never reply to a notification — it carries no `id` — and reconnect with exponential backoff on drop,
   while continuing to emit a `progress` event from a 1 s `aria2.tellActive` batch so rates keep moving.
10. Create `internal/engine/reconcile_test.go` with a fake `Engine`: a known handle updates its task; an
    unknown handle creates nothing and touches nothing; a vanished handle marks its task `error` with
    `engine_unavailable`; an unreachable engine changes no state.

## Acceptance criteria
- [ ] `Boot` writes every engine-reported state back for handles dl-tool knows.
- [ ] A handle dl-tool did not create produces no `tasks` row and no `task_events` row.
- [ ] A vanished handle leaves its task in `error` with `error_code` `engine_unavailable`.
- [ ] An unreachable engine logs one warning and changes no task state.
- [ ] `Run` returns when its context is cancelled and leaves no goroutine running.
- [ ] The aria2 `Events` channel closes when its context is cancelled.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/engine/...
```
Expected: `make lint` prints nothing, then `ok` lines for
`github.com/L-K-M/dl-tool/internal/engine` and `github.com/L-K-M/dl-tool/internal/engine/aria2`, with
`TestBootWritesKnownHandles`, `TestForeignTransferIsIgnored`, `TestVanishedHandleErrors` and
`TestRunStopsWithContext` all running. No `FAIL`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT add an adopt mode, a policy column or any setting about transfers dl-tool did not create.
- Do NOT run the boot conformance probe or change an engine preference; T101 owns conformance.
- Do NOT start queued tasks; T098 owns admission control.
- Do NOT reconcile qBittorrent's `sync/maindata`; T030 owns that adapter's delta protocol.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
