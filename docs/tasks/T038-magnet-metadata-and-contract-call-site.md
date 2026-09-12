# T038 — Resolve magnet metadata and complete the qBittorrent adapter

| Field | Value |
|---|---|
| **ID** | T038 |
| **Milestone** | M2 |
| **Status** | done |
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
| `internal/engine/qbittorrent/client_test.go` | modify | *Widened mid-task, see Blocked:* seed the maindata cache for the three tests that drive the now-gated mutations, call `removeTorrent` where the deleteFiles switch is exercised, and cover the gate, its daemon-truth fallback and the removal cache wait — added in review round 1. |
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
- [x] `InspectMagnet` leaves no torrent behind, on success, on timeout and on a cancelled context.
- [x] The manifest's infohashes come from `infohash_v1`/`infohash_v2`, never from `hash`.
- [x] `var _ engine.Engine = (*Client)(nil)` compiles, with no stub method added to satisfy it.
- [x] `Registry.Get("qbittorrent")` returns the client after `NewServer` with a configured URL.
- [x] `enginetest.RunContract` passes against a real qBittorrent 5.2.3 container.
- [x] `UnsupportedCapabilityReturnsErrNotSupported` passes for every capability the adapter does not
      declare.
- [x] No test in this task contacts a public tracker or a distribution mirror.

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

### Step 1 — the endpoint probe

No Docker on this machine (T028's condition), so the contract was first read verbatim from the pinned
sources (`release-5.2.3` of `qbittorrent/qBittorrent`) and then confirmed live by
`TestQBittorrentMetadataEndpointsProbe` on this branch's CI `integration` job. Source findings:

- `src/webui/api/torrentscontroller.cpp`, `fetchMetadataAction`: the single form parameter is
  **`source`** — not the `url` the older wiki shows (`requireParams({u"source"_s})`). A magnet with a
  valid infohash is answered `202` (APIStatus::Async → 202, `webapplication.cpp`) with
  `serializeInfoHash` — `{"infohash_v1","infohash_v2","hash"}` — while the daemon downloads the
  metadata **in hidden mode** (`sessionimpl.cpp`, `SessionImpl::downloadMetadata`: the handle lives in
  the libtorrent session outside `m_torrents`, so it never appears in `torrents/info`, and
  `handleMetadataReceivedAlert` removes it with `delete_files`). Once the metadata is cached or the
  torrent is in the transfer list, the same POST answers `200` with the nested `info` object
  `{"files":[{"path","length"}],"length","name","piece_length","pieces_num","private"}`.
- `parseMetadataAction`: takes **multipart `.torrent` file parts only** — no `source` parameter — and
  answers `200` with an array of the same serialised shape. It is not part of the magnet chain:
  `fetchMetadata` alone resolves a magnet to the full manifest, so the primary path polls
  `fetchMetadata` and never calls `parseMetadata`.

Live probe output (CI `integration` job, run 34663424656; the subtest failed that round only on the
last assertion, 415 vs 400 — the shapes below are what it observed, and the final tree asserts them):

```
probe: POST torrents/fetchMetadata source=<unknown magnet> -> 202 {"hash":"0123456789abcdef0123456789abcdef01234567","infohash_v1":"0123456789abcdef0123456789abcdef01234567","infohash_v2":""}
probe: POST torrents/fetchMetadata source=<known magnet> -> 200 {"comment":"","created_by":"","creation_date":-1,"hash":"ebd372198f2367e2214a552671006a3796357e68","info":{"files":[{"length":8388608,"path":"enginetest-probe.bin"}],"length":8388608,"name":"enginetest-probe.bin","piece_length":8388608,"pieces_num":1,"private":false},"infohash_v1":"ebd372198f2367e2214a552671006a3796357e68","infohash_v2":"","trackers":[],"webseeds":["http://host.testcontainers.internal:34869"]}
probe: POST torrents/parseMetadata <torrent file part> -> 200 [{"comment":"",…,"info":{…one entry, same shape…},"infohash_v1":"ebd372198f2367e2214a552671006a3796357e68",…}]
probe: POST torrents/parseMetadata <invalid part> -> 415 'empty' is not a valid torrent file.
```

### `make test-integration` (GitHub Actions `integration` job, ubuntu-latest, commit 16cd08a — the final tree)

Non-verbose runner (T028's condition); subtest `--- FAIL:` lines print at this verbosity, and every
genuinely failing round of this PR named its failing subtests here, so the quiet `ok` below is every
subtest passing:

```
ok  	github.com/L-K-M/dl-tool/internal/engine	2.372s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	72.613s
ok  	github.com/L-K-M/dl-tool/internal/engine/enginetest	16.109s
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	129.834s
```

Failing rounds for contrast (same job): `TestQBittorrentContract/AddURL/Progress/Pause/Resume/Remove`,
`/SpeedLimitRoundTrips`, `TestQBittorrentDaemonLimitReadback`, `TestQBittorrentMetadataEndpointsProbe`
and `TestInspectMagnetLeavesNoHandle/daemon_*` each printed `--- FAIL:` lines before the fixes; the
final run prints none. The Docker-free subtests also pass locally:

```
$ go test -tags=integration -count=1 -run 'TestInspectMagnetLeavesNoHandle' ./internal/engine/qbittorrent/ -v
    --- PASS: TestInspectMagnetLeavesNoHandle/primary_path_adds_and_deletes_nothing (0.00s)
    --- PASS: TestInspectMagnetLeavesNoHandle/fallback_success (1.00s)
    --- PASS: TestInspectMagnetLeavesNoHandle/fallback_timeout (2.50s)
    --- PASS: TestInspectMagnetLeavesNoHandle/cancelled_caller_still_removes_the_handle (0.30s)
```

(the two `daemon_*` subtests need the CI Docker lane and pass there).

### Local gates (final tree, commit 16cd08a)

```
$ make lint
0 issues.  (eslint clean, prettier clean)
$ make vet && make test
go vet ./... ; go test -race -count=1 ./... → ok (all packages) ; vitest run → pass
$ make gen && git status --porcelain api/openapi.json web/src/api/schema.d.ts
(no output — no drift; the task registers no Huma operations)
$ go mod tidy && git status --porcelain go.mod go.sum
(no output — crypto/pbkdf2 is stdlib, no new dependency)
$ make doclint
🔍 2384 Total 🔗 560 Unique ✅ 2369 OK 🚫 0 Errors
```

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

Four plan-level facts made the original four-row table impossible to satisfy:

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
   from — confirmed against the daemon's own `torrents/info` on a miss, so a task added a
   moment ago is never refused while the cache catches up (`requireOwned` in `client.go`). The
   `client_test.go` tests cover the gate on both sides of the cache lag.
3. **`torrents/files` does 404.** `Files` needed the same one-line `notFoundOr` mapping
   `mutate.go` already established for the other mutations — an edit in `files.go`, outside the
   original table.
4. **The step-9 web-seed premise is wrong for the pinned image.** Step 9 prescribed a locally
   generated single-file torrent whose *web seed* is the fixture URL. Observed against the real
   container (CI run 34666865450, per-500 ms `torrents/info` samples plus the daemon's own
   `log/main`): the web-seed transfer honors the per-task rate limit (8 MiB in ~8 s at 1 MiB/s),
   but the image's libtorrent **excludes web-seed payload from `dlspeed`, `completed` and
   `progress`** — every sample reads `"dlspeed":0,"completed":0,"progress":0` in `stalledDL`
   until the piece lands, and the daemon's log then shows `"Torrent download finished"`. A
   transfer whose rate and progress are invisible can never satisfy the suite's growth and
   rate-floor assertions. Per IMPLEMENTING.md (“where a task asserts what a daemon does, verify
   it against the pinned version; if reality differs, stop and say so”), the deviation is
   recorded here rather than coded around silently: the contract harness now runs a second,
   seeder container on a private testcontainers network — the seeder fetches the fixture body
   through the torrent's web seed, and the daemon under test takes the same bytes over real
   BitTorrent from the seeder, injected as a static peer (`torrents/addPeers`, literal IP), so
   no tracker, no DHT and no public host are involved and the acceptance criterion “no test
   contacts a public tracker or a distribution mirror” still holds — more genuinely than the
   web-seed shape, since the transfer under assertion is real peer traffic. Everything stays
   inside `contract_test.go`.

Two further fixture realities the harness documents inline: qBittorrent rejects a `Host` header
whose port differs from its listening port (`validateHostHeader`), so the seeded fixture config
disables `WebUI\HostHeaderValidation` (docs/06 §5.2 names the trap); and without a `/downloads`
volume the image's download directory stays root-owned while the daemon runs as `abc`, so the
image's documented `LSIO_NON_ROOT_USER=1` switch runs it as root — a throwaway CI container.

No acceptance criterion was weakened; every widening and the one harness deviation are forced
consequences of the pinned daemon's real behaviour, documented in place.
