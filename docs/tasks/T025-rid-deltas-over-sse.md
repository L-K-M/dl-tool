# T025 — Compute rid deltas and stream them over SSE

| Field | Value |
|---|---|
| **ID** | T025 |
| **Milestone** | M1 |
| **Status** | done |
| **Depends on** | T011, T021, T022, T024 |
| **Blocks** | T026, T041, T051 |
| **Parallel-safe** | no — extends T011's `internal/sync/hub.go` |
| **Implements** | [FR-016](../02-requirements.md#fr-016-stream-task-changes-as-rid-deltas-over-sse), [FR-017](../02-requirements.md#fr-017-serve-the-identical-delta-payload-by-polling) |
| **Decisions** | [ADR-0006](../decisions/0006-sse-with-rid-deltas.md) |
| **Est. size** | 3 new files, ~400 LOC |

## Goal
`GET /api/v1/events` pushes one `sync` event per second carrying only what changed, keyed by a monotonic
`rid` that is also the SSE `id:` line, and `GET /api/v1/sync?rid=N` returns the byte-identical JSON object
without event framing.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §6.1 `GET /events`](../05-api-contract.md#61-get-events)
2. [`docs/05-api-contract.md` §6.2 `GET /sync`](../05-api-contract.md#62-get-sync)
3. [`docs/03-architecture.md` §8.6 SSE ring buffer](../03-architecture.md#86-sse-ring-buffer)
4. [`docs/03-architecture.md` §6.2 The live-update loop](../03-architecture.md#62-the-live-update-loop)
5. [`docs/05-api-contract.md` §3 The canonical Task object](../05-api-contract.md#3-the-canonical-task-object)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/sync/delta.go` | create | `Snapshot`, `Diff` and the task-to-wire projection. |
| `internal/sync/delta_test.go` | create | Diff, projection and payload-equality tests. |
| `internal/api/sse.go` | create | Serve `GET /events` and `GET /sync` from T011's hub. |
| `internal/sync/hub.go` | modify | Add the 1 Hz diff loop that publishes deltas built by `Diff`. |
| `internal/api/server.go` | modify | Register `stream-events` and `get-sync`. |

No other file may be modified.

## Interface contract

`Delta`, `Stats`, `Ring` and `Hub` already exist from T011 and are not redeclared. This task adds the
diffing that feeds `Hub.Publish`, and the two endpoints that read from it.

```go
package sync

// Snapshot is the current wire state of every task, keyed by task id. The inner map holds the
// field names of the canonical Task object in docs/05-api-contract.md §3.
type Snapshot map[string]map[string]any

// Project turns one store row into its wire representation: bytes as integers, timestamps as
// RFC 3339 UTC strings, progress as completed_bytes / total_bytes and 0.0 when total_bytes is null.
func Project(t store.Task) map[string]any

// Diff returns, per task id, only the fields whose value changed between prev and next, plus the
// ids present in prev and absent from next. An unchanged task is absent from changed entirely.
func Diff(prev, next Snapshot) (changed map[string]json.RawMessage, removed []string)

// Aggregate computes the stats block: the summed rates and the counts of active and queued tasks.
func Aggregate(s Snapshot) Stats

// Loop builds a snapshot every tick, diffs it against the previous one and calls Publish only when
// changed, removed or Stats differ. It returns when ctx is cancelled.
func (h *Hub) Loop(ctx context.Context, tick time.Duration, snap func(context.Context) (Snapshot, error)) error
```

```go
package api

// SyncInput is the query of GET /sync. A rid of 0, or one outside the ring, forces a full update.
type SyncInput struct {
	RID int64 `query:"rid" minimum:"0"`
}

// SyncOutput is the identical JSON object an SSE data line carries, with no event framing.
type SyncOutput struct {
	Body sync.Delta
}

func (h *SSEHandlers) Sync(ctx context.Context, in *SyncInput) (*SyncOutput, error)
```

Server rules, all of them:

| Rule | Value |
|---|---|
| Push rate | at most once per second, and only when something changed |
| Ring depth | 300 deltas |
| Reconnect key | the `Last-Event-ID` request header **is** the rid |
| Miss behaviour | one message with `full_update: true` and `seq_gap: true` carrying every task |
| `retry` | `retry: 3000` on the first message of every connection |
| Heartbeat | one SSE comment every 15 seconds while idle, the wire bytes `: hb` |
| Restart | `rid` is monotonic per process and resets to `0`, so the first message after a restart carries `full_update: true` and `seq_gap: true` |

## Steps
1. Create `internal/sync/delta.go` with `Snapshot`, `Project`, `Diff` and `Aggregate`; compare field by
   field and emit only changed keys, so an unchanged task never appears in `tasks`.
2. Marshal each changed task once into `json.RawMessage`, so `GET /events` and `GET /sync` serve the same
   bytes and cannot drift.
3. Add `Hub.Loop` to `internal/sync/hub.go`: a one-second ticker that builds the next `Snapshot`, calls
   `Diff` and `Aggregate`, and calls the existing `Publish` with the result.
4. Skip the tick entirely when `changed` and `removed` are both empty and `Stats` is unchanged, so an idle
   system emits no rid at all.
5. Create `internal/api/sse.go` with `SSEHandlers` and register `GET /events` through the Huma `sse`
   adapter, declaring the single event name `sync` and emitting `retry: 3000` on the first message.
6. Pass the `Last-Event-ID` request header straight to `Hub.Subscribe`, so an absent, unparseable or
   evicted value seeds one message with `full_update: true` and `seq_gap: true`.
7. Send an SSE comment `hb` every 15 seconds while idle.
8. Add `GET /sync` in the same file over `Hub.Snapshot`: default `rid` to `0` and reject a negative value
   with `422` `/problems/validation-failed`.
9. Register both operations in `internal/api/server.go` as `stream-events` and `get-sync`; in the same
   file construct the hub with `sync.NewHub()`, build `snap` from `TaskStore.ListTasks` (every state except
   `removed`) through `Project`, start `hub.Loop(ctx, time.Second, snap)` in a goroutine under the server
   context, and connect `hub.Clients` to the `dltool_sse_clients` gauge T010 registered.
10. Create `internal/sync/delta_test.go`: mutate one task and assert the delta contains that id and no
    other; assert a removed task appears only in `tasks_removed`; assert an idle tick publishes nothing;
    assert `Project` renders bytes as integers and timestamps as RFC 3339.
11. Add a test comparing the `GET /events` data line and the `GET /sync` body for the same rid with
    `github.com/google/go-cmp/cmp` and asserting byte equality.

## Acceptance criteria
- [ ] Mutating one task produces a delta naming that id and no unchanged id.
- [ ] The SSE `id:` line equals the `rid` inside the payload on every message.
- [ ] A `Last-Event-ID` older than the ring yields `full_update: true` and `seq_gap: true`.
- [ ] The `GET /sync` body for a rid is byte-identical to the SSE `data:` payload for that rid.
- [ ] The first message of every connection carries `retry: 3000`.
- [ ] A negative `rid` returns `422`, and an idle second publishes no rid at all.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then `ok` lines for
`github.com/L-K-M/dl-tool/internal/sync` and `github.com/L-K-M/dl-tool/internal/api`, with
`TestDiffOnlyChangedTasks`, `TestIdleTickPublishesNothing` and `TestSSEAndSyncPayloadsAreIdentical` all
running. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT write the client reducer or the polling fallback in the SPA; T041 owns the store and T051 owns
  the reconnect behaviour.
- Do NOT add feed, rule or search deltas to the envelope.
- Do NOT redeclare `Delta`, `Stats`, `Ring` or `Hub`; T011 owns them and this task only adds `Loop`.
- Do NOT poll an engine here; T026 owns the engine poll that writes the rows `snap` reads.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

`make lint && make test PKG=./internal/...`:

```
$ make lint
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
$ make test PKG=./internal/...
go test -race -count=1 ./internal/...
ok  	github.com/L-K-M/dl-tool/internal/api	37.740s
ok  	github.com/L-K-M/dl-tool/internal/config	1.091s
ok  	github.com/L-K-M/dl-tool/internal/engine	1.034s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	2.078s
?   	github.com/L-K-M/dl-tool/internal/fsx	[no test files]
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.353s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.178s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.221s
ok  	github.com/L-K-M/dl-tool/internal/store	58.814s
ok  	github.com/L-K-M/dl-tool/internal/sync	3.383s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.038s
```

The named tests, isolated:

```
$ go test ./internal/sync/... -run 'TestDiffOnlyChangedTasks|TestIdleTickPublishesNothing|TestSSEAndSyncPayloadsAreIdentical' -v
=== RUN   TestDiffOnlyChangedTasks
--- PASS: TestDiffOnlyChangedTasks (0.00s)
=== RUN   TestIdleTickPublishesNothing
--- PASS: TestIdleTickPublishesNothing (0.06s)
=== RUN   TestSSEAndSyncPayloadsAreIdentical
--- PASS: TestSSEAndSyncPayloadsAreIdentical (0.60s)
pass
ok  	github.com/L-K-M/dl-tool/internal/sync	0.675s
```

Scope:

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
api/openapi.json
go.mod
internal/api/server.go
internal/api/sse.go
internal/sync/delta.go
internal/sync/delta_test.go
internal/sync/hub.go
web/src/api/schema.d.ts
```

The Files table's five paths, plus the three standing exceptions of doc 13
§7.1: `api/openapi.json` and `web/src/api/schema.d.ts` from `make gen` for the
two registered operations, and `go.mod` promoting the already-pinned
`github.com/google/go-cmp` to direct for the payload-equality test.

## Blocked

One clause of step 9 — "connect `hub.Clients` to the `dltool_sse_clients` gauge
T010 registered" — cannot be finished inside this task's Files table.
`obs.NewMetrics` builds a private registry per instance and hands it to each
component at the composition root, and that instance is constructed in
`cmd/dl-tool/main.go`, which is outside the table (T010 and T012 both listed it
when they needed it; this task does not). The API side is complete:
`Server.SSE.SetClientsGauge` maintains the gauge's Inc/Dec around every event
connection. The missing handoff is one line in `cmd/dl-tool/main.go`, next to
`server.Health.MarkReady()`: `server.SSE.SetClientsGauge(metrics.SSEClients)`.
Until it lands, `/metrics` reports `dltool_sse_clients 0`. Everything else in
the task is done and verified.
