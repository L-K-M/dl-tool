# 0001 - Build a control plane over existing download engines

> **Status:** accepted
> **Date:** 2026-09-01
> **Deciders:** repository owner

## Context and Problem Statement

Synology Download Station's feature set is spread across roughly five separate daemons (qBittorrent,
SABnzbd/NZBGet, pyLoad/JDownloader, aMule, a yt-dlp wrapper), and no maintained self-hosted project unifies
them. Every unified attempt found in the survey is abandoned, desktop-only or pre-alpha — `ft-aslan/downite`
has 5 stars and a README that reads *"PROJECT IS UNDER HEAVY DEVELOPMENT. IT'S NOT READY FOR PRODUCTION!"*.
The question is therefore what dl-tool builds itself and what it delegates.

## Decision Drivers

- The engines already have stable, documented HTTP APIs: qBittorrent WebAPI v2, aria2 JSON-RPC,
  Transmission RPC, SABnzbd `?mode=`, pyLoad OpenAPI. Re-implementing any of them is a mistake.
- Four capabilities are missing from every engine and are the reason to build anything at all: one queue
  across all protocols, real multi-user with per-user destination and quota, a server-side destination
  browser, and a global bandwidth governor with a schedule that binds every engine.
- Multi-user is not a gap that will close by waiting: qBittorrent issue **#3327** has been open for roughly
  a decade. Transmission, Deluge, aria2, Gopeed, qui and VueTorrent are all single-user.
- No engine exposes a directory-listing API. Every one of them takes a save path as an opaque **string**.
- The implementing agent is a weaker model. Its blast radius must be an adapter, not a protocol stack.

## Considered Options

- **Option A** — Write a new download engine: an in-process BitTorrent client, an NNTP/par2 client and
  hoster scrapers, all owned by dl-tool.
- **Option B** — A control plane: dl-tool owns the queue, users, destinations, search, RSS, scheduling and
  the UI, and delegates transferring to existing engine daemons behind one `Engine` interface.
- **Option C** — Write no software: publish a documented compose stack of qBittorrent + SABnzbd + aria2 +
  Prowlarr + Unpackerr + autobrr and a page explaining which web UI to open for which job.
- **Option D** — Fork an existing aggregator (`jesec/flood`, `autobrr/qui`) and extend it.

## Decision Outcome

Chosen option: **Option B, the control plane**, because it is the only option that can own the four missing
capabilities while inheriting twenty years of engine behaviour that nobody should rewrite. The composition
pattern is proven: Flood drives four torrent clients, and qui drives N qBittorrent instances and even
exposes a transparent qBittorrent reverse proxy so external apps keep working.

### Consequences

- Good, because dl-tool's own code is HTTP clients, a state machine, a SQLite store and an SSE fan-out —
  all of which a weaker model writes reliably.
- Good, because a wedged aria2 does not stop torrents and a crashed yt-dlp process does not stop the web UI.
- Good, because users can point dl-tool at a qBittorrent they already run.
- Bad, because the deployment is multi-container and every engine adds configuration and credentials.
  [`../10-deployment-and-compose.md`](../10-deployment-and-compose.md) hides this behind one `compose.yaml`.
- Bad, because the "one queue" claim holds only for tasks dl-tool created. A transfer added directly to an
  engine by some other means is ignored and never appears in the queue — see
  [`ADR-0017`](0017-exclusive-control-of-engines.md) and
  [`../06-download-engines.md`](../06-download-engines.md).
- Neutral, because Usenet, ed2k and hoster premium accounts stay out of v1 — the adapter interface must not
  preclude them, and it does not.

### Confirmation

No BitTorrent, NNTP or hoster engine may be linked into the binary. `github.com/anacrolix/torrent` is
permitted for `.torrent` and magnet **parsing only**, and only under `internal/uri/`:

```bash
grep -rn "anacrolix/torrent" --include='*.go' cmd internal | grep -v "anacrolix/torrent/metainfo"
```

Expected: no output, exit 1 from `grep`. The adapters are then proven against the real daemons by
`make test-integration`, which runs `internal/engine/enginetest.RunContract` for every registered engine.

## Pros and Cons of the Options

### Option A - write a new engine

- Good, because one process, no IPC, no second credential, and direct access to a full alert stream instead
  of 1 Hz polling.
- Bad, because it means writing queueing, resume-data persistence, fastresume corruption handling, storage
  moves, rechecking, disk-IO tuning, port mapping, tracker error surfacing and share-limit enforcement.
- Bad, because a library assertion or one malformed torrent takes down the whole web application.

### Option B - control plane over existing engines

- Good, because each protocol is handled by the tool that is best at it, and each engine updates on its own
  cadence — which matters most for yt-dlp.
- Good, because fault isolation is a process boundary rather than a `recover()`.
- Bad, because there is an adapter layer to test N ways; [`../13-testing-and-verification.md`](../13-testing-and-verification.md)
  answers that with one shared contract suite.

### Option C - compose the existing stack, write nothing

- Good, because at the daemon level a composed stack covers 13 of the 14 surveyed Download Station features
  and is strictly more capable than Download Station for a power user with an \*arr stack.
- Bad, because it covers 0 of 14 as a coherent user experience: the user must know which of five web UIs to
  open, and there is still no multi-user, no folder picker and no cross-engine schedule.

### Option D - fork Flood or qui

- Good, because Flood already has multi-user and qui already proxies the qBittorrent WebAPI behind its own UI.
- Bad, because both are BitTorrent-only by construction; adding HTTP, media URLs and a cross-protocol queue
  is a rewrite of the data model wearing someone else's licence and release cadence.

## More Information

- Research: `alternatives.md` §0, §9, §10 — summarised in
  [`../16-prior-art-and-research.md`](../16-prior-art-and-research.md).
- Depends on this decision: [`../01-vision-and-scope.md`](../01-vision-and-scope.md),
  [`../03-architecture.md`](../03-architecture.md), [`../06-download-engines.md`](../06-download-engines.md).
- Engine selection is [ADR-0005](0005-aria2-qbittorrent-ytdlp-engines.md). dl-tool exposes no
  compatibility surface: it serves `/api/v1` only and never presents itself as another product's API.
