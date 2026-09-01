# T035 — List a task's connected peers

| Field | Value |
|---|---|
| **ID** | T035 |
| **Milestone** | M2 |
| **Status** | todo |
| **Depends on** | T029, T030, T034 |
| **Blocks** | — |
| **Parallel-safe** | no — extends `internal/api/tasks_swarm.go` from T034 |
| **Implements** | the peer half of [FR-018](../02-requirements.md#fr-018-manage-trackers-and-list-peers-for-bittorrent-tasks) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| **Est. size** | 2 new files, ~290 LOC |

## Goal
`GET /api/v1/tasks/{id}/peers` returns the peers currently connected to a BitTorrent task, with `client`,
`flags` and `country` null when the engine does not report them.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §5.9 Trackers and peers](../05-api-contract.md#59-trackers-and-peers)
2. [`docs/06-download-engines.md` §5.7 Files, priorities, trackers, peers, lifecycle](../06-download-engines.md#57-files-priorities-trackers-peers-lifecycle)
3. [`docs/06-download-engines.md` §5.4 `sync/maindata` — the delta protocol](../06-download-engines.md#54-syncmaindata--the-delta-protocol)
4. [`docs/05-api-contract.md` §1.6 Units, timestamps and nulls](../05-api-contract.md#16-units-timestamps-and-nulls)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/qbittorrent/peers.go` | create | `Peers` over `sync/torrentPeers` with its own per-torrent rid. |
| `internal/engine/qbittorrent/peers_test.go` | create | Full-update, delta and removal cases. |
| `internal/api/tasks_swarm.go` | modify | Add the peers handler and its DTO. |
| `internal/api/tasks_swarm_test.go` | modify | Cases for the peer listing and its rejections. |
| `internal/api/server.go` | modify | Register `list-task-peers`. |

No other file may be modified.

## Interface contract

```go
package qbittorrent

// PeerEntry is one connected peer, normalised onto the wire shape of 05 §5.9. Client, Flags and Country
// are nil when the engine does not report them. Rates are bytes per second, never KB/s.
type PeerEntry struct {
	Address      string   // "host:port"
	Client       *string
	Progress     float64  // 0.0 to 1.0
	DownloadRate int64
	UploadRate   int64
	Flags        *string
	Country      *string
}

// Peers lists the peers of one torrent through GET /api/v2/sync/torrentPeers, which is a rid-delta
// endpoint like sync/maindata: it takes hash and rid, answers full_update on a rid it does not
// recognise, and otherwise sends only changed peers plus peers_removed. The adapter holds one rid per
// torrent and merges exactly as the maindata cache does.
func (c *Client) Peers(ctx context.Context, id string) ([]PeerEntry, error)
```

<!-- UNVERIFIED: the per-peer JSON key names of GET /api/v2/sync/torrentPeers, and the exact name of its
     removal array, were not read verbatim from release-5.2.3. Capture one full response and one delta
     response from a live daemon and derive the struct tags and the merge from them; do not guess. -->

```go
package api

// peerEngine is implemented by an engine that exposes a BitTorrent swarm. It is declared here, at the
// consumer, so the Engine interface of 06 §1 stays unchanged.
type peerEngine interface {
	Peers(ctx context.Context, id string) ([]qbittorrent.PeerEntry, error)
}

type PeerDTO struct {
	Address      string  `json:"address"`
	Client       *string `json:"client"`
	Progress     float64 `json:"progress"`
	DownloadRate int64   `json:"download_rate"`
	UploadRate   int64   `json:"upload_rate"`
	Flags        *string `json:"flags"`
	Country      *string `json:"country"`
}

type ListTaskPeersOutput struct {
	Body struct {
		Peers []PeerDTO `json:"peers"`
	}
}
```

## Steps
1. Capture one `sync/torrentPeers` full response and one delta from a live 5.2.3 daemon with
   `curl -s "$QBT/api/v2/sync/torrentPeers?hash=<hash>&rid=0"`, then repeat with the returned `rid`.
   Paste both, redacted, under `## Evidence` and derive the struct tags from them.
2. Create `internal/engine/qbittorrent/peers.go` with `PeerEntry`, a per-torrent rid map guarded by the
   client mutex, and `Peers`.
3. Merge exactly as T030's maindata cache does: replace on a full update, deep-merge each per-peer
   partial object otherwise, then apply the removal array. Reuse the merge helper rather than copying it.
4. Convert the engine's percentage-style progress to `0.0`–`1.0`, and its rates to bytes per second with
   no unit conversion; leave `Client`, `Flags` and `Country` nil when the key is absent or empty.
5. Return `engine.ErrNotFound` for a hash the client does not hold, and `engine.ErrUnavailable` on a
   transport failure.
6. Drop the per-torrent rid when the torrent leaves the maindata cache, so a re-added torrent starts at
   `rid=0` instead of merging into a stale peer set.
7. Edit `internal/api/tasks_swarm.go` to add the peers handler: `200` with the list, `404` for an unknown
   or foreign task, `422` when the task's engine does not declare `bittorrent`, `503` when the engine is
   down. Never cache the result — the detail pane polls it.
8. Register `list-task-peers` in `internal/api/server.go` as `GET /tasks/{id}/peers`.
9. Create `internal/engine/qbittorrent/peers_test.go` with an `httptest` server driving a full response, a
   delta that changes one peer's rate, a delta that removes a peer, and a re-add resetting the rid.
10. Extend `internal/api/tasks_swarm_test.go` with a peer listing whose optional fields are null, and a
    peers request on an aria2 task returning `422`.

## Acceptance criteria
- [ ] The struct tags were derived from the captured responses pasted under Evidence, not from the wiki.
- [ ] A delta that changes one peer leaves every other peer's fields untouched.
- [ ] A removed peer disappears from the next `Peers` result.
- [ ] `client`, `flags` and `country` serialise as `null`, never as `""`.
- [ ] Rates are bytes per second and no code path divides or multiplies by 1024.
- [ ] `GET /tasks/{id}/peers` on an aria2 task returns `422`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then `ok` for
`github.com/L-K-M/dl-tool/internal/engine/qbittorrent` and `github.com/L-K-M/dl-tool/internal/api`, with
`TestPeersFullUpdate`, `TestPeersDeltaKeepsFields`, `TestPeersRemoval`, `TestPeersRidResetOnReadd` and
`TestPeersOnNonBitTorrentTask` all `PASS`. No `FAIL`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT add a peer to a torrent; `torrents/addPeers` is not in v1.
- Do NOT persist peers in the database; there is no `task_peers` table and there must not be one.
- Do NOT stream peers over SSE; the detail pane polls this endpoint while it is open.
- Do NOT resolve a peer address to a hostname or query any geolocation service.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
