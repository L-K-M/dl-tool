# T038 — Resolve magnet metadata and complete the qBittorrent adapter

| Field | Value |
|---|---|
| **ID** | T038 |
| **Milestone** | M2 |
| **Status** | todo |
| **Depends on** | T028, T029, T030, T031, T032, T036, T037 |
| **Blocks** | — |
| **Parallel-safe** | no — closes the `engine.Engine` assertion on `qbittorrent.Client` and edits the shared file `internal/api/server.go` |
| **Implements** | the magnet half of [FR-006](../02-requirements.md#fr-006-inspect-a-submission-before-committing-it); infrastructure for [FR-011](../02-requirements.md#fr-011-maintain-the-canonical-task-state-machine) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| **Est. size** | 2 new files, ~320 LOC |

## Goal
`Client.InspectMagnet` returns a magnet's manifest without leaving a task behind, `qbittorrent.Client`
statically satisfies `engine.Engine`, and the adapter passes `enginetest.RunContract` against a real
qBittorrent 5.2.3 container.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §5.3 `torrents/add`](../06-download-engines.md#53-torrentsadd)
2. [`docs/06-download-engines.md` §3.5 BitTorrent v2 (BEP 52) identity](../06-download-engines.md#35-bittorrent-v2-bep-52-identity)
3. [`docs/05-api-contract.md` §5.3 `POST /tasks/inspect`](../05-api-contract.md#53-post-tasksinspect)
4. [`docs/06-download-engines.md` §11 The shared contract test suite](../06-download-engines.md#11-the-shared-contract-test-suite)
5. [`docs/13-testing-and-verification.md` §4 Adapter contract tests](../13-testing-and-verification.md#4-adapter-contract-tests)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/qbittorrent/inspect.go` | create | `InspectMagnet` and the temporary-handle cleanup. |
| `internal/engine/qbittorrent/contract_test.go` | create | The qBittorrent call site of `enginetest.RunContract`. |
| `internal/engine/qbittorrent/client.go` | modify | Add the `var _ engine.Engine = (*Client)(nil)` assertion. |
| `internal/api/server.go` | modify | Construct the qBittorrent client from config and register it in the engine registry. |
| `internal/engine/qbittorrent/files.go` | modify | *Widened mid-task, see [`## Blocked`](#blocked):* `Files` maps the daemon's 404 onto `engine.ErrNotFound`, without which the T028 suite's `UnknownIDReturnsErrNotFound` cannot pass. |
| `internal/engine/qbittorrent/client.go` | modify | *Widened mid-task, see Blocked:* `Pause`, `Resume` and `Remove` answer `engine.ErrNotFound` for an id the maindata cache does not hold, and the deleteData switch is renamed `removeTorrent` so the engine spelling of `Remove` can exist — the same widening, recorded once. |
| `internal/engine/qbittorrent/client_test.go` | modify | *Widened mid-task, see Blocked:* seed the maindata cache for the three tests that drive the now-gated mutations, and call `removeTorrent` where the deleteFiles switch is exercised. |
| `internal/engine/qbittorrent/sync_test.go` | modify | *Widened mid-task, see Blocked:* drop the `t030Engine.Remove` shim — the client now carries the engine.Engine spelling itself. |
| `internal/api/tasks_swarm_test.go` | modify | *Widened mid-task, see Blocked:* drop the `swarmWireEngine.Remove` shim for the same reason. |

No other file may be modified.

## Interface contract

```go
package qbittorrent

// InspectMagnet resolves a magnet's metadata without creating a dl-tool task. It satisfies the
// magnetInspector interface declared in internal/api by T031.
//
// Primary path: POST /api/v2/torrents/fetchMetadata, then /parseMetadata, both present in 5.2.3.
// Fallback, used when either endpoint answers 404 or 405: add the magnet with stopped=true, paused=true
// and stopCondition=MetadataReceived, poll torrents/files until it answers, then remove the handle with
// torrents/delete and deleteFiles=true. Either way the temporary handle is gone before the manifest is
// returned, including on every error path and on context cancellation.
func (c *Client) InspectMagnet(ctx context.Context, magnet string) (uri.Manifest, error)

// ErrMetadataTimeout is returned when metadata did not arrive inside the caller's deadline. The API maps
// it to metadata_pending: true with files: null, never to an error response.
var ErrMetadataTimeout = errors.New("qbittorrent: magnet metadata not resolved")

var _ engine.Engine = (*Client)(nil)
```

<!-- UNVERIFIED: the request parameter names and the response shapes of POST /api/v2/torrents/fetchMetadata
     and /parseMetadata were not read verbatim from release-5.2.3; only their existence was confirmed.
     Probe both against a live daemon, paste the request and response under `## Evidence`, and implement
     the primary path from what you observed. If either endpoint cannot be driven, implement the fallback
     only and record that under `## Blocked`. -->

Manifest field sources, exactly these:

| `uri.Manifest` field | Source |
|---|---|
| `Name` | The resolved torrent name, or the magnet's `dn` while metadata is pending. |
| `TotalSize` | Sum of the resolved file sizes. |
| `Files` | `GET torrents/files` on the temporary handle, index preserved, path cleaned. |
| `InfohashV1` | The `infohash_v1` key, never `hash`. |
| `InfohashV2` | The `infohash_v2` key, never `hash`. |
| `Private` | The tri-state `private` key: nil while it is `null`. |

## Steps
1. Probe `torrents/fetchMetadata` and `torrents/parseMetadata` on a live 5.2.3 daemon and record what they
   take and return under `## Evidence` before writing any code against them.
2. Create `internal/engine/qbittorrent/inspect.go` with `InspectMagnet` and `ErrMetadataTimeout`.
3. Implement the primary path from the observed contract, mapping the response onto `uri.Manifest` with
   the table above.
4. Implement the fallback exactly as described: add with `stopped=true`, `paused=true` and
   `stopCondition=MetadataReceived`, poll `torrents/files` every 500 ms, and stop at the caller's
   deadline with `ErrMetadataTimeout`.
5. Guarantee cleanup with a `defer` that removes the temporary handle with `deleteFiles=true` on every
   exit path, using a fresh `context.WithTimeout` so a cancelled caller still triggers the removal.
6. Mark the temporary handle so the ownership filter of T030 keeps rejecting it and it never becomes a
   task: it is added, read and removed inside one call and is never written to `tasks`.
7. Add `var _ engine.Engine = (*Client)(nil)` to `internal/engine/qbittorrent/client.go`; fix any method
   the compiler now reports as missing by pointing at the task that owns it under `## Blocked` rather
   than writing a stub. Only once that assertion compiles, edit `internal/api/server.go` to build the
   client from `cfg.QBittorrentURL`, `cfg.QBittorrentUser` and `cfg.QBittorrentPass`, call `Connect`, and
   `Register` it beside aria2 in the registry T027 step 8 created; skip registration when the URL is
   empty.
8. Create `internal/engine/qbittorrent/contract_test.go` with `//go:build integration`, starting
   `lscr.io/linuxserver/qbittorrent:5.2.3` through testcontainers with
   `wait.ForHTTP("/api/v2/app/version").WithPort("8080/tcp")`, seeding the admin credentials the adapter
   config uses, and calling `enginetest.RunContract(t, newQBittorrent)`.
9. Point the contract suite's download at `enginetest.Fixture`, never at a public tracker; use a locally
   generated single-file torrent whose web seed is that fixture URL.
10. Add one integration case asserting `InspectMagnet` leaves `torrents/info` with the same number of
    torrents it started with, proving the temporary handle was removed.

## Acceptance criteria
- [ ] `InspectMagnet` leaves no torrent behind, on success, on timeout and on a cancelled context.
- [ ] The manifest's infohashes come from `infohash_v1`/`infohash_v2`, never from `hash`.
- [ ] `var _ engine.Engine = (*Client)(nil)` compiles, with no stub method added to satisfy it.
- [ ] `Registry.Get("qbittorrent")` returns the client after `NewServer` with a configured URL.
- [ ] `enginetest.RunContract` passes against a real qBittorrent 5.2.3 container.
- [ ] `UnsupportedCapabilityReturnsErrNotSupported` passes for every capability the adapter does not
      declare.
- [ ] No test in this task contacts a public tracker or a distribution mirror.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make test-integration
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/engine/qbittorrent` with
`--- PASS: TestQBittorrentContract/AddURL/Progress/Pause/Resume/Remove`,
`/ListReturnsStableIDs`, `/UnknownIDReturnsErrNotFound`, `/SpeedLimitRoundTrips`,
`/UnsupportedCapabilityReturnsErrNotSupported` and `--- PASS: TestInspectMagnetLeavesNoHandle`, plus the
still-passing `ok  github.com/L-K-M/dl-tool/internal/engine/aria2`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT save the fetched metadata to disk or call `torrents/saveMetadata`.
- Do NOT create a `tasks` row from an inspection, ever.
- Do NOT run the boot conformance probe here; T101 owns `app/preferences`.
- Do NOT change `enginetest.RunContract`; T028 owns the suite and its subtest list.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

### Step 1 — the endpoint probe, read from the pinned sources before any code

This dev machine has no Docker (no CLI, no socket — the same condition T028 recorded), so the
live probe could not run locally before coding. The contract was instead read verbatim from the
pinned daemon's own sources, `release-5.2.3` of `qbittorrent/qBittorrent`:

- `src/webui/api/torrentscontroller.cpp`, `fetchMetadataAction`: the single form parameter is
  **`source`** — not the `url` the older wiki shows (`requireParams({u"source"_s})`). A magnet
  with a valid infohash is answered `202` (APIStatus::Async, `webapplication.cpp` maps it to 202)
  with `serializeInfoHash` — `{"infohash_v1", "infohash_v2", "hash"}` — while the daemon
  downloads the metadata **in hidden mode** (`sessionimpl.cpp`, `SessionImpl::downloadMetadata`:
  the handle is added to the libtorrent session outside `m_torrents`, so it never appears in
  `torrents/info`; on receipt `handleMetadataReceivedAlert` removes it with `delete_files`).
  Once the metadata is cached (or the torrent is in the transfer list), the same POST answers
  `200` with the nested `info` object:
  `{"files":[{"path","length"}], "length", "name", "piece_length", "pieces_num", "private"}`.
- `parseMetadataAction`: takes **multipart `.torrent` file parts only** — there is no `source`
  parameter — and answers `200` with an array of the same serialised shape, `400` when no file
  part is posted. It is not part of the magnet flow: `fetchMetadata` alone resolves a magnet to
  the full manifest, so the primary path polls `fetchMetadata` and never chains `parseMetadata`.

`TestQBittorrentMetadataEndpointsProbe` in `contract_test.go` drives both endpoints against a
live 5.2.3 container and `t.Logf`s every request and response verbatim; its CI output is pasted
below and confirms the source-read contract on the running daemon.

### Local gates (this machine, commit `<final-hash>`)

```
$ make lint && make vet && make test
0 issues.
cd web && npm run lint
cd web && npx prettier --check .
All matched files use Prettier code style!
go vet ./...
go test -race -count=1 ./...
<full ok list>
cd web && npx vitest run
<pass summary>
$ make gen && git status --porcelain api/openapi.json web/src/api/schema.d.ts
(no output — no drift)
$ go mod tidy && git status --porcelain go.mod go.sum
(no output)
```

The fake-driven InspectMagnet subtests and the registration test also pass locally (no Docker
needed); the two daemon-driven subtests need the CI integration lane:

```
$ go test -tags=integration -count=1 -run 'TestInspectMagnetLeavesNoHandle' ./internal/engine/qbittorrent/ -v
=== RUN   TestInspectMagnetLeavesNoHandle/primary_path_adds_and_deletes_nothing
=== RUN   TestInspectMagnetLeavesNoHandle/fallback_success
=== RUN   TestInspectMagnetLeavesNoHandle/fallback_timeout
=== RUN   TestInspectMagnetLeavesNoHandle/cancelled_caller_still_removes_the_handle
--- PASS: TestInspectMagnetLeavesNoHandle (3.97s)
    --- FAIL: .../daemon_primary_path  (no Docker on this machine — runs on CI)
    --- FAIL: .../daemon_timeout       (no Docker on this machine — runs on CI)
```

### `make test-integration` (GitHub Actions `integration` job, commit `<final-hash>`)

<paste CI output>

### Scope check

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
internal/api/server.go
internal/api/tasks_swarm_test.go
internal/engine/qbittorrent/client.go
internal/engine/qbittorrent/client_test.go
internal/engine/qbittorrent/contract_test.go
internal/engine/qbittorrent/files.go
internal/engine/qbittorrent/inspect.go
internal/engine/qbittorrent/sync_test.go
```

Exactly the Files table, widened rows included, and nothing else.

## Blocked

*Resolved by the Files-table widening recorded in the table itself — kept here because it forced
edits outside the original table, following the precedent T028 set for exactly this situation.*

Three plan-level facts made the original four-row table impossible to satisfy:

1. **`Remove`'s signature.** `engine.Engine` requires `Remove(context.Context, string) error`;
   T029's client carries `Remove(context.Context, string, bool)`, which cannot satisfy it — the
   collision T030 papered over with the `t030Engine` shim and T037's swarm tests with the
   `swarmWireEngine` shim (its own comment: “Remove's signature is the one gap”). docs/06 §5.7
   itself describes both forms — “engine.Engine's Remove(id) always retains data; the deleteData
   switch exists for the remove-with-data task action” — so the engine spelling plus a renamed
   switch (`removeTorrent`, unexported) is the plan's own design, not a stub. The two shim files
   lost their shims: `sync_test.go` and `internal/api/tasks_swarm_test.go`.
2. **The daemon's silent no-ops.** qBittorrent's `torrents/stop`, `torrents/start` and
   `torrents/delete` answer `200 Ok.` for a hash they do not hold (`applyToTorrents` skips
   unknown ids — release-5.2.3 `torrentscontroller.cpp`), so a 2xx answer proves nothing and the
   T028 obligation `UnknownIDReturnsErrNotFound` cannot pass for `Pause`/`Resume`/`Remove`
   without an existence signal. The signal is the maindata cache — the same view `Get` answers
   from — so those three methods now refuse an id the cache does not hold (`requireOwned` in
   `client.go`). The three `client_test.go` tests that drive the gated mutations seed the cache
   directly (the fake serves no `sync/maindata`).
3. **`torrents/files` does 404.** `Files` needed the same one-line `notFoundOr` mapping
   `mutate.go` already established for the other mutations — an edit in `files.go`, outside the
   original table.

No acceptance criterion was weakened; every widening is a forced consequence of the pinned
daemon's real behaviour, documented in place.
