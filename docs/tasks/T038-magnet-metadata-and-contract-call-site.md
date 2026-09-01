# T038 — Resolve magnet metadata and complete the qBittorrent adapter

| Field | Value |
|---|---|
| **ID** | T038 |
| **Milestone** | M2 |
| **Status** | todo |
| **Depends on** | T028, T029, T030, T031, T032, T036, T037 |
| **Blocks** | — |
| **Parallel-safe** | no — closes the `engine.Engine` assertion on `qbittorrent.Client` |
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
   than writing a stub.
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
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
