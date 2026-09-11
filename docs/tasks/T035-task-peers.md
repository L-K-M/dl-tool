# T035 — List a task's connected peers

| Field | Value |
|---|---|
| **ID** | T035 |
| **Milestone** | M2 |
| **Status** | done |
| **Depends on** | T029, T030, T034 |
| **Blocks** | T048 |
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
- [x] The struct tags were derived from the captured responses pasted under Evidence, not from the wiki.
- [x] A delta that changes one peer leaves every other peer's fields untouched.
- [x] A removed peer disappears from the next `Peers` result.
- [x] `client`, `flags` and `country` serialise as `null`, never as `""`.
- [x] Rates are bytes per second and no code path divides or multiplies by 1024.
- [x] `GET /tasks/{id}/peers` on an aria2 task returns `422`.

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
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

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

### The 5.2.3 capture (step 1)

No Docker daemon exists in this environment, so the live daemon is the official-source static build of
the pinned release — `x86_64-qbittorrent-nox` from `userdocs/qbittorrent-nox-static` tag
`release-5.2.3_v2.0.14`, which builds qBittorrent `release-5.2.3` verbatim. Two instances ran locally
(seeder on WebUI 8080, leecher on 8081, isolated profiles under per-instance `XDG_CONFIG_HOME`), each
logged in with its one-time temp password (redacted; the daemons were killed after the capture). A
locally generated one-tracker 16 MiB fixture (`payload.bin`, infohash `bbdc90144a480f8763a8b327ef4e2dd728b2ebe0`,
announce `udp://tracker.example.org:6969/announce`, never contacted) was added to both; DHT was disabled
on both so only the local pair joined, and the pair connected through LSD and PEX — no `torrents/addPeers`
call was made, so nothing outside dl-tool's own vocabulary drove the daemon. The leecher was throttled to
2 MiB/s so the transfer window was observable. Nothing was committed as a fixture; the commands and
responses follow.

```
$ curl -s http://127.0.0.1:8080/api/v2/app/version   (and :8081)
v5.2.3

$ curl -s -F "torrents=@fixture.torrent" -F "savepath=/tmp/qbt35/a/data" http://127.0.0.1:8080/api/v2/torrents/add
{"added_torrent_ids":["bbdc90144a480f8763a8b327ef4e2dd728b2ebe0"],"failure_count":0,"pending_count":0,"success_count":1}
(seeder state: stalledUP, progress 1; leecher added the same way, state: downloading)

$ curl -s "http://127.0.0.1:8081/api/v2/sync/torrentPeers?hash=bbdc…2ebe0&rid=0"
{"full_update":true,"peers":{"127.0.0.1:53941":{"client":"qBittorrent/5.2.3","connection":"μTP","country":"N/A","country_code":"","dl_speed":695525,"downloaded":4487452,"files":"payload.bin","flags":"D X L P","flags_desc":"D = Interested (local) and unchoked (peer)\nX = Peer from PEX\nL = Peer from LSD\nP = μTP","host_name":"","ip":"127.0.0.1","peer_id_client":"-qB5230-","port":53941,"progress":1,"relevance":1,"up_speed":0,"uploaded":0},"172.26.0.3:53941":{"client":"qBittorrent/5.2.3","connection":"μTP","country":"N/A","country_code":"","dl_speed":535645,"downloaded":4552962,"files":"payload.bin","flags":"D X L P","flags_desc":"…same…","host_name":"","ip":"172.26.0.3","peer_id_client":"-qB5230-","port":53941,"progress":1,"relevance":1,"up_speed":0,"uploaded":0}},"rid":1,"show_flags":true}

$ curl -s "http://127.0.0.1:8081/api/v2/sync/torrentPeers?hash=bbdc…2ebe0&rid=1"
{"peers":{"127.0.0.1:53941":{"dl_speed":903247,"downloaded":6780302},"172.26.0.3:53941":{"dl_speed":531294,"downloaded":6780302}},"rid":2}

$ curl -s "http://127.0.0.1:8081/api/v2/sync/torrentPeers?hash=bbdc…2ebe0&rid=2"
{"peers_removed":["172.26.0.3:53941","127.0.0.1:53941"],"rid":3}    # the download completed; both peers disconnected

$ curl -s "http://127.0.0.1:8081/api/v2/sync/torrentPeers?hash=bbdc…2ebe0&rid=3"
{"rid":4}                                                            # an empty delta carries no peers key at all
```

An earlier exploratory run of the same pair with DHT still enabled captured one real internet peer the
GeoIP database could name — the only observed non-nil spelling of `country`:

```
"113.87.163.6:16619":{"client":"","connection":"μTP","country":"China","country_code":"cn","dl_speed":0,"downloaded":0,"files":"","flags":"H P","flags_desc":"H = Peer from DHT\nP = μTP","host_name":"","ip":"113.87.163.6","peer_id_client":"","port":16619,"progress":0,"relevance":0,"up_speed":0,"uploaded":0}
```

Derived from the captures and cross-checked against `release-5.2.3`
`src/webui/api/synccontroller.cpp` (`torrentPeersAction`, `generateSyncData`, `processHash` — the same
diff machinery `sync/maindata` uses) and `src/base/bittorrent/peerinfo.cpp`:

- Envelope: `rid`, `full_update`, `show_flags`, `peers` (an object keyed by the peer's `"ip:port"`, the
  daemon's own `PeerAddress::toString`) and, in a delta only, `peers_removed` (an array of those keys).
  A delta reports only changed peers, as per-peer **partial objects** — the captured rid=1 delta carries
  only `dl_speed` and `downloaded` — so the adapter merges key by key, never decoding into the struct first.
- Per-peer keys observed: `client`, `peer_id_client`, `progress` (0.0–1.0), `dl_speed`, `up_speed`
  (bytes per second — no conversion), `downloaded`, `uploaded`, `connection`, `flags`, `flags_desc`,
  `relevance`, `files`, `host_name`, `ip`, `port`, `country`, `country_code`. `peers.go` decodes
  `client`, `progress`, `dl_speed`, `up_speed`, `flags`, `country`; the rest are ignored by the decoder.
- `flags` is a **letter string** (`"D X L P"`), not a number — the wiki's integer claim does not match
  5.2.3's wire. It maps straight onto `PeerEntry.Flags`.
- `client` is empty until the handshake completes (the DHT stranger above shows it); `country` is
  `"N/A"` with an empty `country_code` for an address the GeoIP database cannot name. Both empty
  strings and the `"N/A"` sentinel map to nil, per doc 05 §1.6's "unknown → null, never a sentinel"
  rule — the same treatment T034 gave the tracker rows' `-1` counts.
- The rid state is per WebUI session and shared across torrents (`m_lastAcceptedPeersResponse` is one
  member), so switching torrents degrades to a full response — safe with one rid held per torrent,
  which is what the adapter does. A login resets the session and therefore the rid, which the stale
  rid then cannot match: the daemon answers a full response, the self-repair of a lost update.

### Verification block

`make lint`:

```
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
cd web && npm run lint
> lint
> eslint .
cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
```

`make test PKG=./internal/...`:

```
ok  	github.com/L-K-M/dl-tool/internal/api	80.109s
ok  	github.com/L-K-M/dl-tool/internal/config	1.227s
ok  	github.com/L-K-M/dl-tool/internal/engine	22.901s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.215s
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	5.301s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.034s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.692s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.208s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.486s
ok  	github.com/L-K-M/dl-tool/internal/store	71.686s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.374s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.109s
```

with the named tests all PASS (re-run on the final tree, after the review round's edits):

```
$ go test ./internal/engine/qbittorrent/ ./internal/api/ -run 'TestPeersFullUpdate|TestPeersDeltaKeepsFields|TestPeersRemoval|TestPeersRidResetOnReadd|TestPeersOnNonBitTorrentTask' -count=1 -v
--- PASS: TestPeersFullUpdate
--- PASS: TestPeersDeltaKeepsFields
--- PASS: TestPeersRemoval
--- PASS: TestPeersRidResetOnReadd
--- PASS: TestPeersOnNonBitTorrentTask
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent
ok  	github.com/L-K-M/dl-tool/internal/api
```

Scope. The work was committed before this paste, so `git status` shows a clean tree; the branch's own
diff is the authoritative scope list, and it is exactly the Files table plus the two generated files
`docs/13` §7.1 adds implicitly to any task that registers a Huma operation:

```
$ git diff --name-only origin/main...HEAD | sort
api/openapi.json
internal/api/server.go
internal/api/tasks_swarm.go
internal/api/tasks_swarm_test.go
internal/engine/qbittorrent/peers.go
internal/engine/qbittorrent/peers_test.go
web/src/api/schema.d.ts

$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
(no output)
```

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
