# T030 — Track qBittorrent state through `sync/maindata` rid deltas

| Field | Value |
|---|---|
| **ID** | T030 |
| **Milestone** | M2 |
| **Status** | done |
| **Depends on** | T026, T029 |
| **Blocks** | T032, T035, T037, T038, T100 |
| **Parallel-safe** | no — extends `internal/engine/qbittorrent/client.go` |
| **Implements** | the qBittorrent half of [FR-148](../02-requirements.md#fr-148-ignore-engine-tasks-dl-tool-did-not-create); infrastructure for [FR-016](../02-requirements.md#fr-016-stream-task-changes-as-rid-deltas-over-sse) |
| **Decisions** | [ADR-0006](../decisions/0006-sse-with-rid-deltas.md), [ADR-0017](../decisions/0017-exclusive-control-of-engines.md) |
| **Est. size** | 3 new files, ~380 LOC |

## Goal
The adapter holds a merged torrent cache fed by `GET /api/v2/sync/maindata?rid=N` polled every second, and
serves `List`, `Get` and `Events` from it. A transfer dl-tool did not create never enters the cache.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §5.4 `sync/maindata` — the delta protocol](../06-download-engines.md#54-syncmaindata--the-delta-protocol)
2. [`docs/06-download-engines.md` §5.5 `torrents/info`](../06-download-engines.md#55-torrentsinfo)
3. [`docs/06-download-engines.md` §8 Engine ownership](../06-download-engines.md#8-engine-ownership)
4. [`docs/06-download-engines.md` §1 The Engine interface](../06-download-engines.md#1-the-engine-interface)
5. [`docs/13-testing-and-verification.md` §5 Golden-file fixtures](../13-testing-and-verification.md#5-golden-file-fixtures)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/qbittorrent/sync.go` | create | The rid cache, the merge, `List`, `Get`, `Events`. |
| `internal/engine/qbittorrent/sync_test.go` | create | Merge, removal, rid-reset and ownership cases. |
| `internal/engine/qbittorrent/testdata/qb_maindata_full_5.2.3.json` | create | One captured `full_update` response. |
| `internal/engine/qbittorrent/client.go` | modify | Start and stop the poll goroutine from `Connect`/`Close`. |
| `internal/engine/reconcile.go` | modify | Add `Reconciler.OwnedRefs` and install it on every engine that accepts an ownership filter. |

No other file may be modified.

## Interface contract

```go
package qbittorrent

// maindata is the GET /api/v2/sync/maindata?rid=N envelope. Torrents values are partial objects, so
// they are held as raw JSON and merged key by key — never decoded into torrentJSON before merging.
type maindata struct {
	Rid             int                        `json:"rid"`
	FullUpdate      bool                       `json:"full_update"`
	Torrents        map[string]json.RawMessage `json:"torrents"`
	TorrentsRemoved []string                   `json:"torrents_removed"`
	ServerState     json.RawMessage            `json:"server_state"`
}

// cache is the merged view. fields[hash] holds the accumulated JSON object for one torrent.
type cache struct {
	rid    int
	fields map[string]map[string]any
}

// merge applies one response. On FullUpdate it replaces fields wholesale; otherwise it deep-merges each
// per-hash object and then applies TorrentsRemoved. It returns the hashes whose value changed and the
// hashes that disappeared, both sorted.
func (c *cache) merge(m maindata) (changed, removed []string)

// SetOwnershipFilter installs the predicate deciding whether a qBittorrent hash belongs to a dl-tool
// task; it is supplied by the Reconciler of T026. Hashes it rejects are dropped from the cache, from
// List, from Get and from every TaskEvent. Until it is set, the cache holds nothing.
func (c *Client) SetOwnershipFilter(owned func(hash string) bool)

func (c *Client) List(ctx context.Context) ([]engine.TaskInfo, error)
func (c *Client) Get(ctx context.Context, id string) (engine.TaskInfo, error) // ErrNotFound when absent

// Events polls sync/maindata every second and emits one TaskEvent per changed hash: EventRemoved with a
// nil Info for a removed hash, otherwise the kind implied by the new state. The channel closes when ctx
// is cancelled or Close is called.
func (c *Client) Events(ctx context.Context) (<-chan engine.TaskEvent, error)
```

Poll loop rules, exactly these:

| Rule | Behaviour |
|---|---|
| Interval | 1 s, from a `time.Ticker` owned by the goroutine `Connect` starts. |
| First call | `rid=0`, which the server answers with `full_update: true`. |
| Subsequent calls | The `rid` from the previous response, held in adapter memory only. |
| `full_update: true` | Replace the cache; re-run the ownership filter over every hash. |
| `full_update` absent or false | Deep-merge each per-hash object, then apply `torrents_removed`. |
| Transport failure | Log at warn, keep the last cache, retry on the next tick; never reset `rid` to 0 unless the daemon says `full_update`. |
| Relation to dl-tool's SSE rid | None. This `rid` is the engine's own and never leaves the adapter ([ADR-0006](../decisions/0006-sse-with-rid-deltas.md)). |

## Steps
1. Create `internal/engine/qbittorrent/sync.go` with `maindata`, `cache` and a mutex guarding both the
   cache and the current `rid`.
2. Implement `cache.merge`: on `FullUpdate` replace `fields` from the response; otherwise, for each hash,
   decode the raw partial into `map[string]any` and copy its keys over the stored map, then delete every
   hash in `TorrentsRemoved`. Return the changed and removed hash lists sorted.
3. Apply the ownership filter inside `merge`, before storing: a hash the predicate rejects is dropped from
   `fields`, is never returned in `changed`, and is not counted anywhere.
4. Implement `SetOwnershipFilter` and default the predicate to one that accepts nothing, so a caller that
   forgets to install it sees an empty queue rather than foreign transfers. Then edit
   `internal/engine/reconcile.go` to add `OwnedRefs(engineName string) func(string) bool`, backed by
   `ListNonTerminalByEngine`, and have `NewReconciler` call
   `SetOwnershipFilter(r.OwnedRefs("qbittorrent"))` on every registered engine that exposes the method, so
   production runs the installed predicate and not the default-deny one.
5. Implement `List` and `Get` by re-marshalling one `fields` entry into `torrentJSON` and calling
   `toTaskInfo` from T029; `Get` splits the `"qbittorrent:"` prefix and returns `engine.ErrNotFound` for a
   hash the cache does not hold.
6. Implement `Events` as a buffered channel fed by the poll goroutine, emitting `EventRemoved` with a nil
   `Info` for removed hashes and otherwise `EventProgress`, `EventPaused`, `EventCompleted` or
   `EventError` from the new state; drop an event rather than block when the channel is full, and log it.
7. Edit `internal/engine/qbittorrent/client.go` so `Connect` starts the poll goroutine with a context
   derived from the client's own, and `Close` cancels it and waits for it to exit.
8. Capture `internal/engine/qbittorrent/testdata/qb_maindata_full_5.2.3.json` from a real 5.2.3 daemon with
   `curl -s "$QBT/api/v2/sync/maindata?rid=0"`, redact absolute paths and any token, and record the exact
   capture command and its date under `## Evidence` in this file.
9. Create `sync_test.go` covering: the captured full update populating the cache; the literal partial
   `{"rid":15,"torrents":{"8c2127…":{"state":"pausedUP"}}}` from 06 §5.4 changing only `state`;
   `torrents_removed` deleting a hash and emitting `EventRemoved`; a foreign hash never appearing in
   `List`, `Get` or the event channel; and a transport failure leaving the previous cache intact.

## Acceptance criteria
- [x] A partial delta merges into the cache without clearing any field the delta omitted.
- [x] `full_update: true` replaces the cache and re-applies the ownership filter.
- [x] A hash the ownership predicate rejects appears in no `List`, no `Get` and no `TaskEvent`.
- [x] `TestForeignHashIsInvisible` exercises a predicate installed by `NewReconciler`, not the default.
- [x] The engine `rid` is never sent to any dl-tool client and never stored in the database.
- [x] A failed poll leaves the previous cache and the previous `rid` unchanged.
- [x] `Close` returns only after the poll goroutine has exited; `go test -race` is clean.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/engine/qbittorrent/...
```
Expected: `make lint` prints nothing, then
`ok  github.com/L-K-M/dl-tool/internal/engine/qbittorrent` with `TestMergeFullUpdate`,
`TestMergePartialKeepsUntouchedFields`, `TestTorrentsRemovedEmitsEventRemoved`,
`TestForeignHashIsInvisible` and `TestPollFailureKeepsCache` all `PASS`. No `FAIL`, no data-race report.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add an adopt mode, an import path or any setting about transfers dl-tool did not create; there is
  one rule and it has no options.
- Do NOT publish these events to the SSE hub; T025 owns `internal/sync` and T026 owns the reconciler that
  bridges the two.
- Do NOT poll `torrents/info` on a timer; `sync/maindata` is the only polling loop this adapter runs.
- Do NOT add `Files`, trackers or peers to the cache; T032, T034 and T035 fetch those on demand.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

`make lint && make test PKG=./internal/engine/qbittorrent/...`:

```
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
go test -race -count=1 ./internal/engine/qbittorrent/...
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	4.656s
```

Named tests (`go test -count=1 -v -run … ./internal/engine/qbittorrent/...`):

```
=== RUN   TestMergeFullUpdate
--- PASS: TestMergeFullUpdate (0.00s)
=== RUN   TestMergePartialKeepsUntouchedFields
--- PASS: TestMergePartialKeepsUntouchedFields (0.00s)
=== RUN   TestTorrentsRemovedEmitsEventRemoved
--- PASS: TestTorrentsRemovedEmitsEventRemoved (0.02s)
=== RUN   TestForeignHashIsInvisible
--- PASS: TestForeignHashIsInvisible (0.03s)
=== RUN   TestPollFailureKeepsCache
2026/09/08 11:11:16 WARN qbittorrent: sync/maindata poll failed; keeping the last cache and rid engine=qbittorrent rid=8 error="qbittorrent: sync/maindata: status 500: boom"
2026/09/08 11:11:16 WARN qbittorrent: sync/maindata poll failed; keeping the last cache and rid engine=qbittorrent rid=8 error="qbittorrent: sync/maindata: status 500: boom"
2026/09/08 11:11:16 WARN qbittorrent: sync/maindata poll failed; keeping the last cache and rid engine=qbittorrent rid=8 error="qbittorrent: sync/maindata: status 500: boom"
--- PASS: TestPollFailureKeepsCache (0.03s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	0.085s
```

Rid locality
(`go test -count=1 -v -run TestEngineRidStaysInsideCache ./internal/engine/qbittorrent/...`):

```
=== RUN   TestEngineRidStaysInsideCache
--- PASS: TestEngineRidStaysInsideCache (0.00s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	0.009s
```

The test serializes every adapter output (`List`, `Get`, `TaskEvent`) and
proves a unique engine rid is absent. `Client` has no store writer, and the
scope check below contains no database, API or generated-contract path.

Full suite (the Reconciler change touches `internal/engine` too):
`make test` — every Go package `ok`, vitest 13 passed. The
race-sensitive tests, including ownership refresh and poll lifecycle races,
also pass `go test -race -count=5 ./internal/engine/qbittorrent/...`
(`ok … 19.163s`).

Scope check:

```
internal/engine/qbittorrent/client.go
internal/engine/qbittorrent/sync.go
internal/engine/qbittorrent/sync_test.go
internal/engine/qbittorrent/testdata/qb_maindata_full_5.2.3.json
internal/engine/reconcile.go
```

Fixture capture — 2026-09-08, from a live qBittorrent **5.2.3** daemon (no
Docker on the capturing machine; `qbittorrent-nox --version` reported
`qBittorrent v5.2.3`, the static official-source build
`x86_64-qbittorrent-nox` of userdocs/qbittorrent-nox-static
`release-5.2.3_v2.0.14`). The WebUI listened on 127.0.0.1:8080 with the
loopback subnet whitelisted; two Ubuntu 24.04.3 iso torrents were mid-download:

```sh
QBT=http://127.0.0.1:8080
curl -s "$QBT/api/v2/torrents/add" -F "torrents=@ubuntu-24.04.3-desktop-amd64.iso.torrent"
curl -s "$QBT/api/v2/torrents/add" -F "torrents=@ubuntu-24.04.3-live-server-amd64.iso.torrent"
curl -s "$QBT/api/v2/sync/maindata?rid=0" > testdata/qb_maindata_full_5.2.3.json
```

Redaction per docs/13-testing-and-verification.md §5: the local save-path
prefix was replaced with `/data`, and the capture machine's public IP in
`server_state.last_external_address_v4` with `198.51.100.7` (TEST-NET-2).
Everything else is byte-for-byte what the daemon emitted, including the
top-level `trackers` object §5.4 does not document — the envelope ignores
unknown keys, and the fixture proves it.

Behaviour verified against the live daemon beyond the fixture: within one
session (`SID` cookie) `rid` advances per served response, `full_update` is
**absent** — not false — on a partial, and a no-change poll is a bare
`{"rid":2}`. Sessionless requests (no cookie) always answer `full_update:
true`, which is why the adapter's poll rides the login session of T029.

Divergence recorded: 06 §5.4 also says to reset the rid to 0 after a failed
poll and after a five-minute full-sync interval; this task's rule table
says the opposite ("never reset `rid` to 0 unless the daemon says
`full_update`"), and the table governs — the no-reset rule is what
TestPollFailureKeepsCache asserts. Local ownership changes are the only
other reset: installing a predicate, or finding that a previously rejected
hash is now owned, requests a complete object under the new ownership
snapshot. A stale-response guard preserves that reset. The
transport-failure rule does not cover either ownership resync.

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
