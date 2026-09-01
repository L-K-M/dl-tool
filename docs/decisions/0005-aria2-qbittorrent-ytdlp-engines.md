# 0005 - aria2, qBittorrent and yt-dlp as the v1 engines

> **Status:** accepted
> **Date:** 2026-09-01
> **Deciders:** repository owner

## Context and Problem Statement

[ADR-0001](0001-control-plane-over-existing-engines.md) settled that dl-tool delegates transferring to
existing daemons. This decision picks them, and must be argued on transfer capability alone:
[ADR-0008](0008-torznab-first-declarative-yaml-second.md) and
[ADR-0009](0009-native-cross-protocol-rss-rules.md) mean dl-tool never calls qBittorrent's `search/*` or
`rss/*` endpoints, so "qBittorrent gives us search and RSS for free" — the research report's argument — is
void. What remains is which stack moves bytes correctly for each protocol family.

## Decision Drivers

- BitTorrent v2 and hybrid torrents (BEP 52) are common enough in 2026 that "some magnets just fail" is a
  support nightmare, and `tasks.infohash_v2` cannot be filled by an engine that does not compute it.
- The task model's file vocabulary is qBittorrent's: `0` skip, `1` normal, `6` high, `7` maximum, plus
  categories and tags.
- HTTP, HTTPS, FTP, SFTP and Metalink need multi-source segmented downloading; qBittorrent has no HTTP lane.
- Media-site URLs need a tool that ships fixes weekly against site breakage; no daemon worth running exists.

## Considered Options

- **Option A** — aria2 alone: one container, one JSON-RPC endpoint, one token, one adapter.
- **Option B** — aria2 for HTTP/FTP/SFTP/Metalink, qBittorrent-nox for BitTorrent, yt-dlp for media sites.
- **Option C** — Link libtorrent-rasterbar into the process instead of running qBittorrent.
- **Option D** — Transmission instead of qBittorrent for the BitTorrent lane.

## Decision Outcome

Chosen option: **Option B**, because **libtorrent 2.x** is the reason: it is the only BitTorrent stack in
the candidate set implementing BEP 52 v2 and hybrid torrents and µTP, and qBittorrent-nox exposes it over a
documented HTTP API together with real seeding behaviour, per-file priorities, categories and tags — the
exact vocabulary the task model and the UI are built on.

aria2 must therefore **not** do BitTorrent. It claims BEP 3, 5, 6, 7, 9, 10, 11, 12, 14, 15, 19, 27, 32 and
MSE/PSE; absent from that list and from the whole 4,455-line manual:

| Missing from aria2 | Consequence for dl-tool |
|---|---|
| BEP 52 (BitTorrent v2) | Cannot open v2-only torrents or magnets; cannot compute `infohash_v2` |
| BEP 29 (µTP), BEP 47 (padding files) | No µTP transport; poor interop with v2 and hybrid torrents |
| Per-file numeric priority; categories and tags | Only `--select-file`, a selection, and no 0/1/6/7 scale; seeding is `--seed-ratio` and `--seed-time` only |

The counter-evidence against aria2 is stated rather than hidden: the last tagged release is **1.37.0 on
2023-11-15**, issue **#2337** ("New release?", opened 2026-01-02) has no maintainer response, and every
distro package ships 1.37.0 — while `master` still receives commits, the most recent read being
**2026-06-25**. Low-maintenance mode, not death, and the reason aria2's scope is confined to the lane where
1.37.0 is complete and stable. Its image is built in-repo (`alpine:3.22` + `apk add aria2`) and published
as `ghcr.io/l-k-m/dl-tool-aria2`; `p3terx/aria2-pro` is never referenced.

### Consequences

- Good, because v2 and hybrid magnets work, `infohash_v1`/`infohash_v2` come straight out of
  `torrents/info`, and a wedged aria2 does not stop torrents.
- Bad, because there are three adapters, three credential sets and three status-normalisation tables.
- Bad, because qBittorrent 5.x wire quirks must be carried explicitly: `torrents/add` takes **`stopped`**,
  not `paused`; dl-tool sends both `skip_checking` (every shipping 5.x) and `seedMode` (master only) and
  accepts both `stoppedDL`/`stoppedUP` and `pausedDL`/`pausedUP`; from 5.2.0 login answers 204 or 401, so
  success is keyed off the `SID` cookie, never the status code.
- Bad, because aria2 pushes notifications over WebSocket only and each carries just a GID
  (`"params":[{"gid":"2089b05ecca3d829"}]`), so the adapter polls `aria2.tellActive` instead.
- Neutral, because the `Engine` interface does not preclude a fourth adapter; Usenet and ed2k stay out of v1.

### Confirmation

```bash
make test-integration PKG=./internal/engine/...
```

Expected: exit 0. `internal/engine/enginetest.RunContract` runs one contract against real `aria2` and
`qbittorrent` containers started by `testcontainers-go`, including a BEP 52 hybrid-torrent fixture, and
`TestRouterNeverSendsBitTorrentToAria2` fails if a `magnet:` or `.torrent` input reaches the aria2 adapter.

## Pros and Cons of the Options

### Option A - aria2 alone

- Good, because it is one container, one RPC, one token and one adapter, and its RPC is the cleanest here.
- Bad, because it lacks BEP 52, µTP, per-file priorities, categories and tags, and because "one engine does
  everything" is not even true: no Usenet, no ed2k, no media-site extraction.

### Option B - aria2 + qBittorrent + yt-dlp

- Good, because each protocol family is handled by the tool that is best at it, each updates on its own
  cadence — which matters most for yt-dlp — and a user can point dl-tool at a qBittorrent they already run.
- Bad, because three daemons must be configured, and `compose.yaml` has to hide that.

### Option C - link libtorrent-rasterbar in-process

- Good, because it gives one process, no IPC, no polling, and the full alert stream instead of 1 Hz polls.
- Bad, because it means writing queueing, resume-data persistence, fastresume corruption handling, storage
  moves, rechecking and share limits — and an assertion takes the web UI down with it.

### Option D - Transmission for BitTorrent

- Good, because 4.1.x has a clean JSON-RPC 2.0 protocol and 4.1.3 carries a real CSRF-nonce security fix.
- Bad, because 4.1 changed the wire protocol, so an adapter must support two formats, and there are no
  categories, no tags and a weaker file-priority model than the task model uses.

## More Information

- Research: `engines.md` §1.1, §1.2, §6, §9.1 and fact-check items FC-1, FC-5, FC-6, FC-7, FC-9 —
  summarised in [`../16-prior-art-and-research.md`](../16-prior-art-and-research.md).
- Depends on this decision: [`../06-download-engines.md`](../06-download-engines.md),
  [`../10-deployment-and-compose.md`](../10-deployment-and-compose.md),
  [ADR-0017](0017-exclusive-control-of-engines.md), [ADR-0018](0018-pin-ytdlp-by-version-and-hash.md).
