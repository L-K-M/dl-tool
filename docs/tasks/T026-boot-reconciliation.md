# T026 — Reconcile tasks with the engines at boot and on every poll

| Field | Value |
|---|---|
| **ID** | T026 |
| **Milestone** | M1 |
| **Status** | done |
| **Depends on** | T019, T024, T025 |
| **Blocks** | T030, T098 |
| **Parallel-safe** | no — it also edits the shared files `internal/engine/aria2/client.go`, `internal/store/tasks.go` and `internal/api/server.go` |
| **Implements** | [NFR-003](../02-requirements.md#nfr-003-resume-every-task-after-a-restart); the aria2 half of [FR-148](../02-requirements.md#fr-148-ignore-engine-tasks-dl-tool-did-not-create), whose qBittorrent half is T030 |
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
| `internal/store/tasks.go` | modify | Add `ListNonTerminalByEngine` and `SetEngineRef`. |
| `internal/api/server.go` | modify | Construct the reconciler, run `Boot` before the listener opens and start `Run`. |
| `internal/engine/aria2/client_test.go` | modify | Teach `fakeServer` the WebSocket upgrade (answer 101 and push notifications) so the notification transport is testable. |

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
	// ListNonTerminalByEngine returns engine_ref -> task for one engine, skipping completed,
	// removed and error. Added to internal/store/tasks.go by this task.
	ListNonTerminalByEngine(ctx context.Context, engineName string) (map[string]Reconcilable, error)
	UpdateProgress(ctx context.Context, id string, p Progress) error
	SetEngineRef(ctx context.Context, id, engineRef string) error
	Transition(ctx context.Context, id, next, code, message string) error
}

// Reconcilable is the non-terminal task the sweep needs: enough to write state back and, when the
// handle has vanished, to re-submit it.
type Reconcilable struct {
	ID         string
	State      string // never completed, removed or error
	SourceURI  *string
	InfohashV1 *string
}

// Boot runs one full sweep before the HTTP listener opens, over the non-terminal tasks only (never
// completed, removed or error): every known engine_ref is written back, a task in downloading,
// seeding or checking whose handle has vanished is re-submitted from its stored source with resume
// semantics, a queued or paused task is left alone, and every unknown handle is ignored.
// See 17-operations-and-runbook.md section 1.6.
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
| dl-tool holds a `downloading`, `seeding` or `checking` task whose handle the engine no longer knows | Re-submit from `source_uri` (or the infohash, for qBittorrent) with resume semantics, `SetEngineRef` to the new handle and emit `task.reconciled`. An aria2 GID never survives a daemon restart, so this is the expected path, not a failure. |
| The re-submission itself fails | Transition to `error` with `error_code` `engine_unavailable` and one `engine.unavailable` event. |
| dl-tool holds a `completed`, `removed` or `error` task | Out of scope: a terminal task is never listed and never touched. |
| dl-tool holds a task with `engine_ref` still `NULL` | Leave it in `queued`; T098's admission pass owns starting it. |
| The engine is unreachable | Log at `warn`, retry on the next tick, change no task state. |

## Steps
1. Create `internal/engine/reconcile.go` with `Reconciler`, `TaskWriter`, `NewReconciler`, `Boot` and
   `Run`.
2. Implement `Boot` as one `Engine.List` per registered engine, joined against
   `ListNonTerminalByEngine` on the `engine_ref` value, so a terminal task is never a candidate.
3. Drop every listed handle that is absent from the map, and add the comment that this is the whole of
   [ADR-0017](../decisions/0017-exclusive-control-of-engines.md): one rule, no options and no setting.
4. Write back the counters with `UpdateProgress`, and call `Transition` only when the normalised state
   differs from the stored one, so an unchanged task produces no event and no delta.
5. For a vanished handle in `downloading`, `seeding` or `checking`, re-submit through `Engine.Add` with
   resume semantics, call `SetEngineRef` with the new handle and write one `task.reconciled` event; leave
   `queued` and `paused` alone. Only a failed re-submission becomes `error` with `engine_unavailable` and
   one `engine.unavailable` event.
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
    unknown handle creates nothing and touches nothing; a vanished handle for a `downloading` task is
    re-submitted and adopts the new `engine_ref`; a vanished handle for a `completed` or `removed` task
    changes nothing; a re-submission that fails marks its task `error` with `engine_unavailable`; an
    unreachable engine changes no state.
11. Edit `internal/api/server.go` to build `engine.NewReconciler(reg, taskStore, time.Second)` on the
    registry T027 step 8 created, call `Boot` once before the HTTP listener opens — logging a failure at
    `warn` without stopping `NewServer` — and start `Run` in a goroutine under the server context, stopped
    when that context is cancelled.

## Acceptance criteria
- [ ] `Boot` writes every engine-reported state back for handles dl-tool knows.
- [ ] A handle dl-tool did not create produces no `tasks` row and no `task_events` row.
- [ ] A vanished handle for a non-terminal task is re-submitted and the new handle is stored.
- [ ] A vanished handle for a `completed` or `removed` task changes nothing.
- [ ] Only a failed re-submission leaves the task in `error` with `error_code` `engine_unavailable`.
- [ ] An unreachable engine logs one warning and changes no task state.
- [ ] `Run` returns when its context is cancelled and leaves no goroutine running.
- [ ] A server built by `NewServer` has reconciled once before it accepts its first request.
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
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

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

`make lint` — exit 0, no findings:

```
$ make lint
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
cd web && npm run lint
cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
```

`make test PKG=./internal/engine/...` — exit 0, both packages `ok`:

```
$ make test PKG=./internal/engine/...
go test -race -count=1 ./internal/engine/...
ok  	github.com/L-K-M/dl-tool/internal/engine	1.511s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	2.429s
```

Every named test, plus the two Events tests of the aria2 package:

```
$ go test -race -count=1 -v -run 'TestBootWritesKnownHandles|TestForeignTransferIsIgnored|TestVanishedHandleErrors|TestRunStopsWithContext' ./internal/engine/
=== RUN   TestBootWritesKnownHandles
--- PASS: TestBootWritesKnownHandles (0.00s)
=== RUN   TestForeignTransferIsIgnored
--- PASS: TestForeignTransferIsIgnored (0.00s)
=== RUN   TestVanishedHandleErrors
--- PASS: TestVanishedHandleErrors (0.00s)
=== RUN   TestRunStopsWithContext
--- PASS: TestRunStopsWithContext (0.07s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/engine	1.125s
=== RUN   TestEventsEmitsProgress
--- PASS: TestEventsEmitsProgress (1.01s)
=== RUN   TestEventsMapsWebSocketNotifications
--- PASS: TestEventsMapsWebSocketNotifications (0.01s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	2.062s
```

Scope check over the task's whole diff (the working tree is clean at the
final commit, so `git status` lists nothing; this is the same check over
`origin/main..HEAD`):

```
$ git diff --name-only origin/main..HEAD -- . ':(exclude)docs' | sort
go.mod
internal/api/server.go
internal/engine/aria2/client.go
internal/engine/aria2/client_test.go
internal/engine/reconcile.go
internal/engine/reconcile_test.go
internal/store/tasks.go
```

The six paths are the (amended) Files table; `go.mod` is the docs/13 §7.1
implicit addition — first import of the pinned `github.com/gorilla/websocket
v1.5.0`, whose only delta is the loss of the `// indirect` marker
(`go.sum` unchanged, no version changed). Full-repo `make test` is green:

```
$ make test | grep -E '^(ok|FAIL)'
ok  	github.com/L-K-M/dl-tool/internal/api	42.724s
ok  	github.com/L-K-M/dl-tool/internal/config	1.116s
ok  	github.com/L-K-M/dl-tool/internal/engine	1.527s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	2.385s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.456s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.168s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.301s
ok  	github.com/L-K-M/dl-tool/internal/store	63.568s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.356s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.030s
```

After the review round, the same gates — `make lint` clean, full-repo
`make test` green — and the named tests plus the two new ones:

```
$ go test -race -count=1 -v -run 'TestBootWritesKnownHandles|TestForeignTransferIsIgnored|TestVanishedHandleErrors|TestRunStopsWithContext|TestEvents|TestSweepSurvives' ./internal/engine/...
--- PASS: TestBootWritesKnownHandles (0.00s)
--- PASS: TestForeignTransferIsIgnored (0.00s)
--- PASS: TestVanishedHandleErrors (0.00s)
--- PASS: TestRunStopsWithContext (0.02s)
--- PASS: TestSweepSurvivesWriteBackFailure (0.00s)
ok  	github.com/L-K-M/dl-tool/internal/engine	1.078s
--- PASS: TestEventsEmitsProgress (1.01s)
--- PASS: TestEventsMapsWebSocketNotifications (0.01s)
--- PASS: TestEventsReconnectsPastSilentPeer (0.66s)
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	2.733s
```

The harness amendment also exposed one real defect step 9 had hidden: with
the WebSocket actually connected, a blocked `ReadMessage` observes neither a
cancelled context nor a closed client, so the `Events` channel never closed —
an acceptance criterion. `notifyEvents` now hands each connection to a
`closeOnDone` watchdog that owns `conn.Close()` on all three exits and is
waited on before the loop continues, so cancellation closes the channel
promptly and no goroutine outlives the loop.

### Review round

The GLM review's one critical finding — that real aria2 sends the gid as a
bare string `params:["<gid>"]` — is wrong, verified against the pinned
daemon's own source: `WebSocketSessionMan::addNotification` in
release-1.37.0 puts a `Dict` holding `gid` into the params list, i.e.
`params:[{"gid":"…"}]`, exactly what the manual documents and this
decoder expects. The wire shape stands; the fake now pins it through an
independent test-local type so a decoder regression cannot co-vary. The
adopted findings: sweep failure logs at warn (not debug), a per-task
write-back or re-submission failure no longer aborts the sweep, an
`AppendEvent` failure after the committed `SetEngineRef` is a warning, the
notification connection carries a read deadline and client pings so a
silent peer reconnects instead of hanging, the boot sweep inside
`NewServer` is bounded by a 10 s budget, and the two flaky-deadline test
loops were fixed. Rejected with evidence: adopt-orphan re-submission
(the task forbids an adopt mode; ADR-0017), progress change-detection
(the rules table writes counters on every report), duplicate-`engine_ref`
guarding (`idx_tasks_engine_ref` is a partial UNIQUE index, so the
duplicate is unrepresentable).

The follow-up round flagged two minors in the new test code, both taken:
the silent-peer handler no longer calls a testing.T method from a goroutine
that can outlive the test, and the read-idle window became a per-client
field snapshotted per connection (`c.readIdle`) instead of mutable package
state. Stability pinned with `go test -race -count=5 -run TestEvents`
(10 packages `ok` across the full `make test`).

## Blocked

None. An earlier session stopped here because
`internal/engine/aria2/client_test.go` was missing from the Files table:
step 8's WebSocket dial is an HTTP GET with Upgrade headers (RFC 6455
§1.3), which `fakeServer` rejected with its POST-only assertion, so no
implementation of step 8 could pass the suite. The amendment it proposed
is the `internal/engine/aria2/client_test.go` row above; the task then
proceeded with no other scope change — the upgrade branch in `fakeServer`,
the six-notification mapping test it makes possible, and the `closeOnDone`
watchdog that test forced (see Evidence).
