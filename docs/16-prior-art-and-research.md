# 16 — Prior Art and Research

> **Status:** draft
> **Last reviewed:** 2026-09-01
> **Audience:** implementing agent
> **Read this before:** [`01-vision-and-scope.md`](01-vision-and-scope.md), [`decisions/`](decisions/), T001

## Purpose

This file is the evidence record behind the plan: what already exists, what it covers, what it does not,
and which primary source proves each claim. It answers "why build dl-tool at all" and "where did this
number come from". It does not instruct anybody to build anything.

## Scope of this document

- In scope: the 2026-09-01 landscape survey, the Download Station feature-parity matrix, the three
  concrete voids, the corrections the adversarial fact-check produced, a per-topic source index, and the
  aggregated list of unverified claims.
- Out of scope (lives instead in): product scope and personas → [`01-vision-and-scope.md`](01-vision-and-scope.md);
  the `Engine` interface and engine wire protocols → [`06-download-engines.md`](06-download-engines.md);
  Torznab and `.dlm` details → [`07-search-and-indexers.md`](07-search-and-indexers.md);
  RSS rule semantics → [`08-rss-automation.md`](08-rss-automation.md);
  security requirements → [`12-security-and-threat-model.md`](12-security-and-threat-model.md);
  every *why* → [`decisions/`](decisions/).

---

## 1. Landscape survey (read 2026-09-01)

Download Station feature codes used throughout, condensed from the survey; the canonical parity list
lives in [`01-vision-and-scope.md`](01-vision-and-scope.md):

`F1` multi-protocol single queue · `F2` batch URL paste · `F3` server-side destination picker ·
`F4` BT search with pluggable engines · `F5` RSS with filters · `F6` file selection before download ·
`F7` bandwidth schedule · `F8` global + per-task speed limits · `F9` auto-extract · `F10` eMule/ed2k ·
`F11` NZB/Usenet · `F12` official mobile app · `F13` multi-user · `F14` statistics.

### 1.1 Generic / multi-protocol download managers

| Tool | What it is | Licence | Language | Docker image | Maintenance (dated) | Covers | Lacks |
|---|---|---|---|---|---|---|---|
| **pyLoad (pyload-ng)** | Plugin-driven hoster/HTTP download manager with captcha and premium-account support | AGPL-3.0-or-later | Python 3.9+ | `lscr.io/linuxserver/pyload-ng`; `pyload/pyload` | Still pre-1.0 (`pip install --pre`); LSIO `develop-1bf50726-ls5` built 2026-03-06 | F1 (HTTP part), F2, F9 | BitTorrent, NZB, ed2k, real multi-user, RSS filters. API unstable: `/api/login` disappeared in pyload-ng |
| **JDownloader 2** | Java Swing GUI in a container over noVNC | Closed-source freeware core | Java | `jlesage/jdownloader-2` | Actively published | Hoster coverage, link decryption, F9 | Native REST (control is the cloud-relayed MyJDownloader API); huge RAM footprint; port 3129 must map 1:1 |
| **aria2** | CLI daemon: HTTP(S), FTP, SFTP, BitTorrent, Metalink; JSON-RPC + XML-RPC | GPL-2.0 | C++ | none official; build from `alpine` + `apk add aria2` | Last release **1.37.0, 2023-11-15**; `master` still commits, newest **2026-06-25** | F1 (HTTP/FTP/SFTP/BT), F3 (`dir`), F8 | NZB, ed2k, RSS, search, auto-extract, multi-user, per-file priorities, BEP 52, uTP |
| **Gopeed** | Go + Flutter manager: HTTP, BitTorrent, Magnet, **ED2K**, REST API, JS extensions | GPL-3.0 | Go + Flutter | `liwei2633/gopeed` (community namespace), port 9999 | ~26.0k stars, active | F1 (three of four DS families), F2, F8, F10, F12 | NZB, RSS filters, BT search, multi-user, auto-extract |
| **Motrix / Motrix Next** | Electron (resp. Tauri 2 + Vue 3) desktop GUI over aria2 | MIT | JS / Rust | none | Motrix largely inactive since 2023 (`2.0.0-beta.27/28`) | UX reference only | Desktop-only; not deployable via Compose |
| **Persepolis** | Desktop manager; 5.0.0 dropped aria2 for its own downloader | GPL-3.0 | Python | none | Active, desktop-only | UX reference only | Not a server |
| **uGet** | GTK desktop manager, optional aria2 backend | LGPL | C | none | Release cadence very slow; current release **UNVERIFIED** | UX reference only | Not a server |
| **Xtreme Download Manager** | Browser-extension-driven desktop app; latest seen 8.0.26 stable / 8.0.29 beta | GPL-2.0 | Java/Go | none | Active, desktop-only | UX reference only | Not a server |

### 1.2 BitTorrent engines

| Tool | What it is | Licence | Language | Docker image | Maintenance (dated) | Covers | Lacks |
|---|---|---|---|---|---|---|---|
| **qBittorrent-nox** | libtorrent 2.x server with WebUI API v2 | GPL-2.0/3.0 | C++/Qt | `lscr.io/linuxserver/qbittorrent`; `hotio/qbittorrent` | **v5.2.3, 2026-07-07**; WebAPI `2.15.1` | F2, F4, F5, F6, F7, F8, F14 | Single WebUI user (issue #3327, opened **2015-06-29**, still open); no auto-extract; no NZB/ed2k/HTTP-hoster |
| **Transmission** | Long-lived BT daemon, JSON-RPC at `/transmission/rpc` | GPL-2.0 | C/C++ | official + community | **4.1.3, 2026-06-30** (CORS/anti-CSRF-nonce fix); 4.1.2 2026-06-02; 4.1.1 2026-02-20; 4.1.0 2026-01-27 | F6 (one-call `files_wanted`/`files_unwanted`), F7 (`alt_speed_time_*`), F8, F14 | Search, RSS, auto-extract, multi-user. 4.1 changed dialect: JSON-RPC 2.0 + snake_case |
| **Deluge** | Daemon + GTK/Web/Console clients, plugin ecosystem | GPL-3.0 | Python | community | GitHub is a mirror ("PRs only"); latest docs read `2.2.1.dev43` — **2.2 unreleased** | F5 (YaRSS2), F6, F7, F9 (Extractor), F14 | Slow releases, plugin fragility, single-user web UI |
| **rTorrent + ruTorrent** | ncurses daemon over XMLRPC/SCGI plus a PHP front end | GPL-2.0 | C++ / PHP | community | ruTorrent **v5.1.0-stable**; issues opened 2026-07-28 and 2026-08-28 | F5 (`rss`, `autodl-irssi`), F7, F8, F9 (`unpack`), F14 | PHP + SCGI + nginx wiring is brittle; no clean JSON API; single-user |
| **libtorrent-rasterbar / `anacrolix/torrent`** | Libraries, not products | BSD-3 / MPL-2.0 | C++ / Go | n/a | Active | n/a | Building on them means owning DHT, LSD, PEX, resume data, tracker etiquette, port mapping, encryption |

### 1.3 Usenet

| Tool | What it is | Licence | Language | Docker image | Maintenance (dated) | Covers | Lacks |
|---|---|---|---|---|---|---|---|
| **SABnzbd** | NZB downloader with par2 repair, unrar, categories, post-processing scripts | GPL-2.0 | Python | `lscr.io/linuxserver/sabnzbd` | 5.1.0 **2026-08-10**, 5.1.1 **2026-08-18**, 5.1.2 **2026-08-25**; three serious vulnerabilities fixed across 5.1.1/5.1.2 — pin ≥ 5.1.2 | F1 (NZB), F3 (categories→folders), F8, F9, F11, F14 | Everything non-Usenet |
| **NZBGet (nzbgetcom fork)** | C++ NZB downloader; the original `nzbget/nzbget` was archived **2022-11-18** (last stable `21.1`, 2021-06-03) | GPL-2.0 | C++ | `nzbgetcom/nzbget` (upstream); `lscr.io/linuxserver/nzbget` also maintained | **v26.3, 2026-08-27**; 737 stars | F1 (NZB), F8, F9, F11, F14 | Everything non-Usenet |

### 1.4 The *arr stack

| Tool | What it is | Licence | Language | Docker image | Maintenance (dated) | Covers | Lacks |
|---|---|---|---|---|---|---|---|
| **Sonarr / Radarr / Lidarr** | PVRs that decide *what* to grab and hand it to a download client | GPL-3.0 | C#/.NET + React | `lscr.io/linuxserver/<name>` | Active | F5 for their own media types | Not download engines; media-library-shaped, not link-shaped |
| **Readarr** | Book PVR | GPL-3.0 | C#/.NET | — | **Retired.** Servarr wiki titles every page "(Retired)" | — | Metadata source unusable; community mirror `rreading-glasses` unsupported |
| **Prowlarr** | Indexer manager: 24 Usenet indexers + 500+ torrent trackers, syncs config into the *arrs | GPL-3.0 | C#/.NET + React | `ghcr.io/linuxserver/prowlarr` | Active; 7.1k stars | F4 industrialised | No queue, no engine, *arr-centric |
| **Jackett** | Torznab + TorrentPotato proxy; **torrent-only, it does not speak Newznab** | GPL-2.0 | C# | `linuxserver/jackett` | Active; 16k stars; ≈601 trackers counted in the README on 2026-09-01 | F4 industrialised | Usenet; no queue |
| **autobrr** | Real-time IRC-announce monitoring (75+ trackers) + Torznab/Newznab/RSS with regex filters | GPL-2.0 **UNVERIFIED** | Go + React | `ghcr.io/autobrr/autobrr` | Active; ~2.9k stars | F5, far beyond DS | Not a queue or a UI for casual downloads |
| **Unpackerr** | Watches *arr queues or a watch folder and extracts rar/zip/7z/tar/gz/bz2 recursively | MIT **UNVERIFIED** | Go | `golift/unpackerr` | Active | F9 for the whole stack | Driven by *arrs or folders, never by a queue |
| **Bazarr / Recyclarr** | Subtitles; TRaSH config sync | GPL-3.0 / MIT **UNVERIFIED** | Python / C# | community | Active | — | Not downloaders |
| **Seerr** | Request front end; Overseerr (archived 2024) + Jellyseerr (deprecated 2026) merged, announced **2026-02-10** | MIT **UNVERIFIED** | TypeScript | image name **UNVERIFIED** | Active | — | Not a downloader |

### 1.5 Media grabbers (yt-dlp family)

| Tool | What it is | Licence | Language | Docker image | Maintenance (dated) | Covers | Lacks |
|---|---|---|---|---|---|---|---|
| **yt-dlp** | Extractor CLI/library for thousands of sites; channels stable (monthly), nightly, master | Unlicense | Python | n/a | Active; PyPI shows near-daily `.dev0` nightlies | F1 (media-site part) | Not a queue, not a server, no persistence |
| **MeTube** | Thin aiohttp web UI + queue over yt-dlp; socket.io live state | AGPL-3.0 **UNVERIFIED** | Python | `ghcr.io/alexta69/metube`, port 8081 | Active | F1 (media part), F2, F3 (free-text dir) | Multi-user, BT/NZB, RSS. Its `ALLOW_PRIVATE_ADDRESSES=false` default is the SSRF pattern worth copying |
| **Pinchflat** | Elixir/Phoenix rule-based channel archiver; serves podcast RSS; single container, no external deps | AGPL-3.0 **UNVERIFIED** | Elixir | `ghcr.io/kieraneglin/pinchflat` | Active; ~5k stars | F5 for YouTube | Generic downloading |
| **Tube Archivist** | Full YouTube media server, Elasticsearch + Redis; needs ~2 GB RAM minimum | GPL-3.0 **UNVERIFIED** | Python | `bbilly1/tubearchivist` | Active | F5, F14 for YouTube | Far too heavy; not a download manager |
| **ytdl-sub** | YAML-config CLI that prepares media for Kodi/Jellyfin/Plex/Emby | GPL-3.0 **UNVERIFIED** | Python | `ghcr.io/jmbannon/ytdl-sub` **UNVERIFIED** | Active | Scripted archiving | No UI |
| **YoutubeDL-Material** | Heavier yt-dlp UI with accounts and a built-in player | MIT **UNVERIFIED** | TypeScript | `tzahi12345/youtubedl-material` **UNVERIFIED** | Activity slowed; status **UNVERIFIED** | F13-ish accounts | Current maintenance unknown |
| **cobalt** | Stateless media-URL → direct-file API | AGPL-3.0 **UNVERIFIED** | TypeScript | `ghcr.io/imputnet/cobalt:11` (API only; no official frontend image) | Active | Paste-any-social-URL helper | Not a queue or manager |

### 1.6 Indexers and DHT crawlers

| Tool | What it is | Licence | Language | Docker image | Maintenance (dated) | Covers | Lacks |
|---|---|---|---|---|---|---|---|
| **bitmagnet** | Self-hosted DHT crawler + classifier with GraphQL, a Torznab endpoint and an Angular UI | MIT | Go | official compose in repo; **requires PostgreSQL** | Active but self-declared **alpha**; API and DB will change before 1.0 | F4 without any tracker | **No authentication or access control at all** — never expose it |
| **magnetico** | The original DHT crawler | AGPL-3.0 | Go | none maintained | **Archived**, last commit 2022; forks at 30/15/10 stars | — | Effectively dead; do not depend on it |
| **Torrust** | `torrust-index` + `torrust-tracker`: host *your own* torrents | AGPL-3.0 | Rust | official | Repo activity through May–June 2026; demo live since 2024-04-24 | — | Not a search engine over the public swarm — wrong tool for DS parity |

### 1.7 Aggregators and DS-like UIs

| Tool | What it is | Licence | Language | Docker image | Maintenance (dated) | Covers | Lacks |
|---|---|---|---|---|---|---|---|
| **Flood** | Monitoring service normalising rTorrent ✅, qBittorrent v4.1+ ✅, Transmission ✅, Deluge v2+ 🧪 behind one REST API with its own OpenAPI spec at `/api/openapi.json` | GPL-3.0 | TypeScript | `jesec/flood`, port 3000 | Alive but low-velocity; 60 open issues / 7 PRs; no 2026 tagged release could be verified | F13 (**the only torrent web UI with real multi-user**), F3-ish via `--allowedpath`, F14 | Torrents only; no search, RSS, auto-extract, NZB, HTTP |
| **qui** | Multi-instance qBittorrent UI with rule automations, backups, and a **transparent qBittorrent reverse proxy for external apps** | GPL-2.0-or-later | Go + TypeScript | `ghcr.io/autobrr/qui`, port 7476 | Active | F4/F5 pass-through, F14, 11 UI languages | qBittorrent only; single logical user |
| **VueTorrent** | Alternate qBittorrent WebUI surfacing RSS and search in one responsive UI | GPL-3.0 | Vue | shipped pre-packed in `hotio/qbittorrent` | **6.87k stars, last release 2026-07-09** | Closest thing to a DS-like UI today | qBittorrent-only, single-user, torrents-only |
| **decypharr** | Debrid client that **mimics the qBittorrent API** so the *arrs treat Real-Debrid/TorBox/AllDebrid as a torrent client | MIT | Go | `ghcr.io/sirrobot01/decypharr` | Active | Proof that the qBittorrent façade strategy works | Not self-contained downloading |
| **downite** | Explicit "unified torrent + HTTP download client" attempt; Swagger at `:9999/docs` | MIT | Go + React | none documented | **5 stars, 1 fork, 127 commits**; README: *"PROJECT IS UNDER HEAVY DEVELOPMENT. IT'S NOT READY FOR PRODUCTION!"* | The idea, unexecuted | Everything |
| **Sinedo** | "A Simple Network Downloader for your NAS" — sharehoster downloads, DEB/RPM, REST API, port 2222 | MIT | C#/.NET | none | Self-described beta; low activity | Hosters only | Narrow scope |
| **exatorrent** | Single-binary Go + Svelte torrent client | MIT | Go | community | Low activity | Torrents only | Everything else |
| **aMule** (`ngosang/docker-amule`) | The only live ed2k daemon; `amuled` + `amuleweb` | GPL-2.0 | C++ | `ngosang/amule` (unofficial, maintained by an aMule team member) | Active | F10 | Control is the **binary EC protocol on :4712**, not HTTP — a real integration cost |
| **DS browser extensions / clients** | `seansfkelley/nas-download-manager`, `dvcol/synology-download`, `graingert/synopy`, `N4S4/synology-api` | MIT / GPL | TS / Python | n/a | Active | Ready-made clients for anything speaking `SYNO.DownloadStation.*` | They drive DS; they are not DS |

---

## 2. Feature-parity matrix

Reproduced in full from the survey. Legend: ✅ native · 🟡 partial, needs a plugin or a workaround ·
❌ absent. The **Gap** column states what a composed stack still misses after wiring all of them together.

| DS feature | qBittorrent 5.2 | Transmission 4.1 | SABnzbd 5.1 | aria2 1.37 | pyLoad-ng | Gopeed | Flood | qui / VueTorrent | Deluge 2.1 + plugins | **Gap after composing all of them** |
|---|---|---|---|---|---|---|---|---|---|---|
| **F1** Multi-protocol single queue | ❌ BT only | ❌ BT only | ❌ NZB only | 🟡 HTTP/FTP/SFTP/BT | 🟡 HTTP/hosters | ✅ HTTP/BT/magnet/**ed2k** | ❌ BT only | ❌ BT only | ❌ BT only | **Nothing unifies BT + NZB + HTTP + ed2k + yt-dlp in one queue/list. This is the #1 gap.** |
| **F2** Batch URL paste | ✅ `urls` newline-separated | 🟡 one `filename` per call | 🟡 one `name` per call | 🟡 one call per URI | ✅ package of links | ✅ | ✅ | ✅ | 🟡 | Cross-protocol paste: user pastes 10 mixed URLs → must be routed per-protocol. **Missing everywhere.** |
| **F3** Server-side destination picker | 🟡 `savepath` string, no browse API | 🟡 `download_dir` string; `free-space` method exists | 🟡 categories→folders | 🟡 `dir` option | 🟡 | 🟡 | 🟡 | 🟡 | **No engine exposes a directory-listing API.** dl-tool must own a browse endpoint over the shared volume. |
| **F4** BT search, pluggable engines | ✅ `search/*` + Python plugins + `jackett.py` | ❌ | ❌ (indexers via *arrs) | ❌ | ❌ | ❌ | ❌ | 🟡 (surfaces qBt search) | ❌ | Covered *if* you use qBittorrent's search API or a Torznab client. Usenet search only via Prowlarr/NZBHydra. |
| **F5** RSS with filters | ✅ `rss/*` + full rule JSON | ❌ | 🟡 RSS in config | ❌ | 🟡 addon | ❌ | ❌ | 🟡 (surfaces qBt RSS) | 🟡 YaRSS2 | Cross-protocol RSS (one rule that can land in NZB *or* BT) — **missing**. autobrr covers the power-user case. |
| **F6** File selection before download | 🟡 two-step: add stopped → `torrents/files` → `filePrio` | ✅ one-step `files_wanted`/`files_unwanted` | n/a | 🟡 `--select-file` | n/a | 🟡 **UNVERIFIED** | ✅ (UI) | ✅ (UI) | ✅ | Fine. Needs a "preview metadata without committing" step — qBt/Transmission need the torrent added first. |
| **F7** Bandwidth schedule | ✅ `scheduler_enabled` + `schedule_*` + `scheduler_days` | ✅ `alt_speed_time_*` | 🟡 SAB has a scheduler in config | ❌ | 🟡 scheduler addon | ❌ **UNVERIFIED** | ❌ (passes through) | ❌ | ✅ | Each engine schedules itself independently. **One global schedule across engines is missing.** |
| **F8** Global + per-task speed limits | ✅ `transfer/setDownloadLimit`, `dlLimit`/`upLimit` per torrent | ✅ session + per-torrent + **bandwidth groups** | ✅ `mode=config&name=speedlimit` | ✅ `--max-download-limit` | ✅ | ✅ | 🟡 pass-through | 🟡 | Same: no cross-engine global cap. |
| **F9** Auto-extract | ❌ | ❌ | ✅ native unrar/par2 | ❌ | ✅ extractor addon | ❌ | ❌ | ❌ | 🟡 Extractor plugin | Solved for torrents by **Unpackerr** or ruTorrent's `unpack` — but Unpackerr is *arr-driven or watch-folder driven, not queue-driven. |
| **F10** eMule / ed2k | ❌ | ❌ | ❌ | ❌ | ❌ | ✅ | ❌ | ❌ | ❌ | Only Gopeed (REST) or aMule (binary EC protocol on :4712). **Nobody offers a modern ed2k web API.** |
| **F11** NZB / Usenet | ❌ | ❌ | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | Fully solved by SABnzbd / nzbgetcom. |
| **F12** Mobile app | 🟡 iQbit PWA, qBitController | 🟡 Transdroid | 🟡 nzb360 | 🟡 AriaNg PWA | ❌ | ✅ Flutter app | 🟡 responsive | ✅ responsive/PWA | 🟡 | **LunaSea is discontinued.** nzb360 (Android, closed-source, paid) is the only broad multi-service app. **A responsive PWA is the pragmatic answer.** |
| **F13** Multi-user | ❌ single WebUI user (issue #3327) | ❌ single Basic-auth user | 🟡 API key + NZB key | ❌ single RPC secret | 🟡 has user accounts | ❌ | ✅ **multi-user** | 🟡 qui has its own auth | ❌ | **Real multi-user with per-user destinations/quotas exists nowhere except Flood.** Strong differentiator. |
| **F14** Statistics | ✅ `transfer/info`, `sync/maindata` | ✅ `session_stats` | ✅ `mode=history` | ✅ `getGlobalStat` | ✅ | ✅ | ✅ | ✅ | ✅ | Fine. |

**Score.** The best *single* tool (qBittorrent + VueTorrent) covers roughly F2/F4/F5/F6/F7/F8/F14 = **7/14**.
A composed stack (qBittorrent + SABnzbd + Gopeed/aria2 + Prowlarr + Unpackerr) covers **13/14 at the daemon
level but 0/14 as a coherent user experience**, because the user has to know which of five web UIs to open.

---

## 3. The honest conclusion

**For a power user with an *arr stack, a better answer than Download Station already exists.**
qBittorrent + SABnzbd + Prowlarr + Sonarr/Radarr + Unpackerr + autobrr is strictly more capable than
Download Station in every dimension except unified UX and multi-user. dl-tool does not compete with that
stack and must not pretend to.

**For the actual Download Station use case, nothing covers it.** That use case is: *paste a link, pick a
folder, walk away; my flatmate does the same without seeing my downloads.* Three concrete voids, each with
its evidence:

| # | Void | Evidence |
|---|---|---|
| 1 | **One queue across all protocols** | No maintained project does BT + NZB + HTTP-hoster + ed2k + yt-dlp in one list. The only project that tried, `ft-aslan/downite`, has 5 stars, 1 fork, 127 commits and a README reading *"PROJECT IS UNDER HEAVY DEVELOPMENT. IT'S NOT READY FOR PRODUCTION!"* Gopeed reaches three of four families and has no NZB. |
| 2 | **Multi-tenancy** | qBittorrent issue #3327 *"Multiple users for WebUI"* was opened **2015-06-29** and is still open — eleven years. Transmission, Deluge, aria2, Gopeed, qui and VueTorrent are all single-user. Only Flood has real multi-user, and only for torrents. |
| 3 | **Server-side destination browsing** | Every engine accepts a destination as a *string*: qBittorrent `savepath`, Transmission `download_dir`, aria2 `dir`. None exposes "list the folders under my share". Transmission ships only a `free-space` method. DS's folder picker is genuinely absent from the OSS world. |

Secondary voids: a global bandwidth schedule binding *all* engines; a mobile client now that LunaSea is discontinued; auto-extract triggered by the *queue* rather than by an *arr.

**The unlock that makes the plan cheap.** Sonarr's `develop` branch already ships a *Download Station*
download client — `src/NzbDrone.Core/Download/Clients/DownloadStation/` with `DiskStationProxyBase.cs`,
`DownloadStationTaskProxyV1.cs` and friends, all returning HTTP 200 on `raw.githubusercontent.com` on
2026-09-01. Implementing roughly five Synology endpoints would therefore have made dl-tool a
zero-configuration download client for Sonarr, Radarr and Lidarr *and* for every existing DS mobile app
and browser extension, and `decypharr` and `qui` independently chose the sibling trick of impersonating
qBittorrent.

**This option was considered and rejected.** The repository owner scoped dl-tool to being superficially
identical to Download Station *from the user's point of view*, not functionally identical to it for other
software: "I just want a tool that basically does the same thing from the user's pov and that I can host
wherever I want using docker compose." dl-tool therefore serves `/api/v1` only, emulates nobody, and
carries no compatibility surface. ADR-0014 was withdrawn before it was written and its number is
permanently unused. The finding is recorded here because it is real and well-evidenced — if the scope
ever changes, this is where the work starts.

---

## 4. Corrections the adversarial fact-check produced

Every research report was re-verified against primary sources on 2026-09-01. These corrections are traps an
implementer would otherwise fall into. **IMPORTANT:** where a report body and its `## Fact-check` section
disagree, the fact-check wins, and the corrections below are the ones that change code.

| # | The trap | The verified fact | Primary source |
|---|---|---|---|
| 1 | `torrents/add` takes `paused=true` | **The parameter is `stopped`, not `paused`.** `release-5.2.3`'s `torrentscontroller.cpp` reads `parseBool(params()[u"stopped"_s])`; the string `paused` does not appear in the file. Code that posts `paused=true` silently adds the torrent **started**, which breaks the add-then-select-files sequence. dl-tool sends **both** names. | `https://raw.githubusercontent.com/qbittorrent/qBittorrent/release-5.2.3/src/webui/api/torrentscontroller.cpp` |
| 2 | The torrent `state` enum contains `pausedDL` / `pausedUP` | **5.x emits `stoppedDL` / `stoppedUP`** (and `forcedMetaDL`); `pausedUP`, `pausedDL` and `allocating` are absent. Verified twice, on `master` and at `release-5.2.3`. The wiki still documents the old names — the wiki is stale. dl-tool **accepts both** spellings. | `src/webui/api/serialize/serialize_torrent.cpp` at `release-5.2.3` |
| 3 | qBittorrent has an official Torznab search-engine plugin | **It does not exist.** `nova3/engines/torznab.py` → 404; `wiki/New-Torznab-search-engine.md` → 404; `searchpluginmanager.cpp` at `release-5.2.3` contains zero occurrences of "torznab". 5.2.3 still runs the Python `nova2.py`/`nova3` engine unchanged. Torznab reaches qBittorrent only through *unofficial* plugins (`jackett.py`, `prowlarr.py`). **DeepWiki is wrong here** — `deepwiki.com/qbittorrent/search-plugins` claims the Python engine was replaced by a Torznab client in 4.5.0; that is false and appears to be an AI summary of a deleted proposal page. | `qbittorrent/search-plugins` tree; `release-5.2.3/src/base/search/searchpluginmanager.cpp` |
| 4 | LinuxServer's nzbget image is unmaintained | **It is maintained.** `linuxserver/docker-nzbget` is not archived and its changelog shows `05.06.26: Rebase to Alpine 3.23` and `05.07.26: Rebase to Alpine 3.24`; the docs carry no deprecation notice. `nzbgetcom/nzbget` remains the right default because it is the upstream-published image, but the warning was wrong. | `https://github.com/linuxserver/docker-nzbget` |
| 5 | aria2 is dormant | **Release-dormant, not development-dormant.** No tagged release since `1.37.0` on 2023-11-15 (the previous gap, 1.36.0 in 2021-08-21, was similar), but `master` still receives commits — most recently **2026-06-25**. Issue #2337 "New release?" (opened 2026-01-02) has no maintainer response. Phrase the risk as "no release in ~3 years; upstream is in maintenance mode". | `https://github.com/aria2/aria2/releases` · `.../commits/master` · `.../issues/2337` |
| 6 | Transmission upstream is 4.1.1, and DS's 4.0.5 is merely two years old | **Upstream stable is 4.1.3 (2026-06-30)**, a security release fixing a CORS bug that leaked the anti-CSRF nonce plus a peer-code use-after-free — pin ≥ 4.1.3, not 4.1.1. And **Transmission 4.0.6 shipped 2024-05-29**, so Download Station's November-2025 bundle of 4.0.5 was already a patch behind *within its own 4.0 line*. | `https://github.com/transmission/transmission/releases` · Synology release-note API |

Four further corrections that also change code, recorded here so they are not lost:

- **File priorities are `0, 1, 6, 7`,** not `0, 1, 4, 7`. `downloadpriority.h` at `release-5.2.3`:
  `Ignored = 0, Normal = 1, High = 6, Maximum = 7, Mixed = -1`. `0,1,4,7` is libtorrent's internal scale,
  not the WebAPI vocabulary. `filePrio` also *accepts* `-1`, so do not treat it as a validation error —
  and do not send it.
- **`skip_checking` is still the parameter name in every shipping 5.x.** The rename to `seedMode` exists
  only on `master`, which is `5.3.0alpha1`. Send both.
- **qBittorrent ≥ 5.2.0 login returns `204` on success and `401` on bad credentials**, not `200 "Ok."` /
  `200 "Fails."`; the literal `"Fails."` was last present at `release-5.1.0`. Assert on the
  `Set-Cookie: SID` header, not on the status code. A `Referer`/`Origin` header is **not** required —
  `isCrossSiteRequest()` is permissive when both are absent, and rejects only a *mismatch*, with 401.
- **Synology error code 105 is "the logged-in session does not have permission", not "auth failure".**
  Sonarr treats 105/106/107/119 as session errors but raises an authentication exception only on 105;
  bad credentials at login time come back as Auth-namespace code 400.

---

## 5. Per-topic research index

Ten research reports were produced on 2026-09-01. The load-bearing findings, with the primary source that
proves each, so a future maintainer can re-verify without redoing the work.

### 5.1 Download Station (`downloadstation.md`)

- DS is a front end over four third-party engines: Transmission (BT), NZBGet (Usenet), aMule (ed2k) and
  wget/curl (HTTP/FTP), plus pyLoad for hosting sites — confirmed by Synology's own changelog, the
  AppArmor comment `#transmissiond`, and literal Transmission `settings.json` keys on disk.
  `https://www.synology.com/api/releaseNote/findChangeLog?identify=DownloadStation&lang=en-global`
- The BT engine went **2.93 (2018-04-16) → 4.0.5 (2025-11-25)**: two changelog lines in a 65-entry history,
  7½ years frozen. `https://www.synology.com/en-global/releaseNote/DownloadStation`
- The exact URI-scheme list the UI accepts is verbatim
  `ftps://, sftp://, magnet://, http://, https://, ftp://, thunder://, flashget://, qqdl://`; note that
  Synology's own `magnet://` is malformed — real magnet URIs are `magnet:?xt=urn:btih:…`.
  `https://kb.synology.com/en-global/DSM/help/DSget/Android`
- `thunder://`, `flashget://` and `qqdl://` are not protocols but base64 obfuscation wrappers:
  thunder = b64(`"AA" + url + "ZZ"`), flashget = b64(`"[FLASHGET]" + url + "[FLASHGET]"`), qqdl = b64(url).
  Verified by local round-trip. `https://www.simpleapples.com/en/posts/thunder-link-decoder/`
- The task-status vocabulary is 10 values and `error_detail` has 26 values (counted). Synology's own PDF
  contradicts itself on the field name — the schema says `error_detail`, Appendix B says `err_detail`;
  read `error_detail`.
  `https://global.download.synology.com/download/Document/Software/DeveloperGuide/Package/DownloadStation/All/enu/Synology_Download_Station_Web_API.pdf`
- A `.dlm` search module is a gzip tar containing `INFO` (JSON) plus a PHP class — but "exactly two files"
  is observed convention, not spec: `INFO.module` is documented as *"Usually It is search.php, but you can
  use other file names"*. A parser must read `INFO.module`.
  `.../DeveloperGuide/Package/DownloadStation/All/enu/DLM_Guide.pdf`

### 5.2 Alternatives (`alternatives.md`)

- Sonarr ships a Download Station download client on `develop`; the wire protocol was read directly from
  `DiskStationProxyBase.cs` and `DownloadStationTaskProxyV1.cs`, including `create` v2 (POST, multipart
  `file`), `create` v3 (GET, query `uri`), `list` v1 (`additional=detail,transfer`) and `delete` v1
  (`id`, `force_complete`). `https://wiki.servarr.com/sonarr/supported`
- The `SYNO.API.Info` `query=` list is **dynamic** — each Sonarr proxy queries `SYNO.API.Auth` plus its
  own API name, so an emulator would have had to answer for `SYNO.DownloadStation2.Task`,
  `SYNO.DownloadStation.Info` and `SYNO.FileStation.*` too, not only the two obvious names. Recorded for
  completeness; dl-tool builds no such emulator.
- Flood already proves the composition pattern: one REST API with its own OpenAPI spec at
  `/api/openapi.json` normalising four torrent RPCs. `https://flood.js.org`
- Prowlarr documents *"Usenet support for 24 indexers natively"* and *"Torrent support for over 500
  trackers"* verbatim; Jackett's README claims only Torznab and TorrentPotato — **Jackett is torrent-only**.
  `https://github.com/Prowlarr/Prowlarr` · `https://raw.githubusercontent.com/Jackett/Jackett/master/README.md`
- Torznab is *"an api specification based on the Newznab WebAPI … built around a simple xml/rss feed with
  filtering and paging capabilities."*
  `https://torznab.github.io/spec-1.3-draft/torznab/Specification-v1.3.html`

### 5.3 Indexers (`indexers.md`)

- Torznab/Newznab is the lingua franca; being a *client* buys 500+ maintained sites for the cost of one
  HTTP client. Wire format read from Sonarr's own `schemas/torznab.xsd` and its test fixtures.
  `https://raw.githubusercontent.com/Sonarr/Sonarr/develop/schemas/torznab.xsd`
- Cardigann YAML semantics were taken from `definitions/v11/schema.json` and Prowlarr's
  `CardigannRequestGenerator.cs`, not from the wiki, which is a JS SPA that could not be extracted.
  `https://github.com/Prowlarr/Indexers` (commit `5f2b3d9`, 2026-08-31)
- qBittorrent's plugin result line is exactly
  `link | name | size(bytes) | seeds | leech | engine_url | desc_link | pub_date`, verified in
  `novaprinter.py` at `release-5.2.3`.
  `https://raw.githubusercontent.com/qbittorrent/qBittorrent/master/src/searchengine/nova3/novaprinter.py`
- The four plug-in models compared (Torznab, Cardigann, nova3 Python, `.dlm` PHP) all rot at the per-site
  scraping layer; only Torznab pushes that cost onto somebody else.
- Legitimate default sources were HTTP-probed individually: `archive.org/advancedsearch.php`,
  `archlinux.org/feeds/releases/`, `academictorrents.com/rss.xml`, `releases.ubuntu.com/24.04/` and
  `cdimage.debian.org/debian-cd/current/amd64/bt-cd/`.

### 5.4 Engines (`engines.md`)

- aria2's JSON-RPC path is `/jsonrpc` (XML-RPC is `/rpc`), default port **6800**, and the secret is the
  first parameter as `token:SECRET` — with two documented exceptions: `system.listMethods` and
  `system.listNotifications` need no token, and `system.multicall` takes the token in each *nested* call.
  `https://raw.githubusercontent.com/aria2/aria2/master/doc/manual-src/en/aria2c.rst`
- aria2 `tellStatus` is **not** all-strings: `bittorrent.creationDate` is a JSON integer and is
  conditionally absent (`src/RpcMethodImpl.cc:700`). Parse defensively.
- aria2 has **no BEP 52, no uTP and no per-file numeric priority** — the negative claims were confirmed by
  searching the whole 4,455-line manual. This is why aria2 must not do BitTorrent. It does have PEX
  (BEP 11, default on) and LPD (BEP 14, default off).
- qBittorrent's real WebAPI version is `2.15.1` at `release-5.2.3`
  (`webapplication.h`: `inline const Utils::Version<3, 2> API_VERSION {2, 15, 1};`), not the 2.11.3 the
  wiki's newest heading suggests. Gating features on "≥ 2.11.3" compares against a stale number.
- qBittorrent can hand you an infohash *before* adding: `torrents/fetchMetadata`, `/parseMetadata`,
  `/saveMetadata` all exist at `release-5.2.3`. `torrents/info` also emits `infohash_v1` and `infohash_v2`;
  the `hash` field is the TorrentID and for a v2-only torrent is **not** the v1 infohash. There is no
  `isPrivate` field — the JSON key is `private`, and it is tri-state (`null` before metadata arrives).
- Anything read from qBittorrent `master` must be re-verified at `release-5.2.3`: `master` is
  `5.3.0alpha1` and already diverges (`seedMode`, `shareLimitsMode`, `API_VERSION` 2.16.2).

### 5.5 RSS (`rss.md`)

- qBittorrent's `AutoDownloadRule` is the de-facto standard rule model and is readable in ~760 lines of
  C++; its `rss/matchingArticles` endpoint is the dry-run feature users actually want.
  `https://raw.githubusercontent.com/qbittorrent/qBittorrent/master/src/base/rss/rss_autodownloadrule.cpp`
- Do **not** copy qBittorrent's `mustContain` string-splitting hack (`"a|b"` split on `|` only when
  `useRegex=false`). Use real arrays.
- URL extraction inside a feed item follows qBittorrent's parser, which is document-order last-wins across
  an `application/x-bittorrent` enclosure and a `magnet:` `<link>`, then a typeless enclosure, then
  `<link>`. `.../src/base/rss/rss_parser.cpp`
- Dedup on three keys in order: feed-item GUID, BitTorrent info-hash, then a normalised episode key.
- Sonarr's error backoff ladder is exactly `0, 60, 300, 900, 1800, 3600, 10800, 21600, 43200, 86400`
  seconds. `https://raw.githubusercontent.com/Sonarr/Sonarr/develop/src/NzbDrone.Core/ThingiProvider/Status/EscalationBackOff.cs`
- Etiquette: conditional GET works on most real feeds; default poll 30 min with ±10 % jitter; honour
  `<ttl>` and `sy:updatePeriod` as a floor.

### 5.6 Architecture (`architecture.md`)

- Go 1.26 + chi v5 + Huma v2: Huma emits the OpenAPI spec from the handler structs, so the documented
  contract cannot drift from the code. `https://huma.rocks/features/`
- SQLite via `modernc.org/sqlite` keeps `CGO_ENABLED=0` and the static-binary story intact; `sqlx`
  `StructScan` removes the mis-ordered `rows.Scan` bug class.
- SSE carrying qBittorrent-style `rid` deltas: server→client only, survives every reverse proxy,
  reconnects with `Last-Event-ID` for free, and `huma/v2/sse` types the payload into the spec.
  `https://html.spec.whatwg.org/multipage/server-sent-events.html`
- `@tanstack/react-table` **v8** specifically: v9 shipped 2026-08-04 and is outside essentially all
  training data. `https://tanstack.com/blog/announcing-tanstack-table-v9`
- Jobs: an in-process worker pool plus a `jobs` table plus `robfig/cron/v3`. The database is already the
  durable store; a broker buys nothing here.

### 5.7 Deployment (`deployment.md`)

- **A single `/data` bind mount.** Separate `/downloads` and `/media` mounts silently break hardlinks and
  turn instant moves into slow copies. `https://trash-guides.info/File-and-Folder-Structure/Hardlinks-and-Instant-Moves/`
- PUID/PGID/UMASK are the NAS convention users expect; the entrypoint drops privileges, the app never runs
  as root. `https://docs.linuxserver.io/general/understanding-puid-and-pgid/`
- App HTTP on `:8080` inside the container, published on host `:8091` by default — DSM occupies 5000/5001
  and host 8080 is extremely crowded.
- gluetun for VPN routing, with the engine container joining via `network_mode: service:gluetun`; the
  firewall and port-forwarding options are documented per-provider.
  `https://github.com/qdm12/gluetun-wiki/blob/main/setup/connect-a-container-to-gluetun.md`
- GHCR multi-arch `linux/amd64,linux/arm64` with OCI labels, SBOM and provenance attestations.
  `https://docs.docker.com/build/metadata/attestations/`
- `p3terx/aria2-pro:latest` was last pushed **2022-09-06** (confirmed via the Docker Hub API) — do not
  depend on it.

### 5.8 UI/UX (`ui-ux.md`)

- The reference systems were read as source, not screenshots: qBittorrent's `dynamicTable.js`,
  `addtorrent.html`, `rssDownloader.html`, and Flood's `TorrentListColumns.ts`.
  `https://raw.githubusercontent.com/jesec/flood/master/client/src/javascript/constants/TorrentListColumns.ts`
- The virtualized grid must report `aria-rowcount` as the **total** row count, not the number of rows
  currently in the DOM. `https://www.w3.org/WAI/ARIA/apg/patterns/grid/`
- Notification targets and their exact publish shapes: ntfy, Gotify, Apprise.
  `https://docs.ntfy.sh/publish/` · `https://gotify.net/docs/pushmsg` · `https://github.com/caronc/apprise-api`
- DS complaints that became requirements were sourced from third-party issue trackers rather than forums,
  because SynoForum returns 403 and Synology Community renders client-side.
  `https://github.com/rembo10/headphones/issues/3058`
- Long-open qBittorrent UI issues (#9796 opened 2018-10-31, still open; #17504 closed as not planned)
  document what the incumbent UI will not fix.

### 5.9 Security (`security.md`)

- 23 real incidents in this exact software class were catalogued and turned into numbered requirements.
  The dominant cause is default or absent authentication: **CVE-2023-30801** (qBittorrent
  `admin`/`adminadmin` plus "run external program" = unauthenticated RCE, exploited in the wild March 2023,
  CVSS 9.8). `https://github.com/qbittorrent/qBittorrent/issues/18731`
- **CVE-2018-5702** (Transmission, Project Zero issue 1447): DNS rebinding against the RPC port. The fix
  Transmission now ships — `X-Transmission-Session-Id`, HTTP 409 carrying the correct id, and
  `rpc_host_whitelist_enabled` defaulting true — is the design to copy.
  `https://github.com/transmission/transmission/blob/main/docs/rpc-spec.md`
- **CVE-2024-51774**: qBittorrent called `ignoreSslErrors` unconditionally for **14 years** (2010-04-06 →
  fixed in 5.0.1, 2024-10-28). TLS verification is always on, with no override switch.
- SSRF protection must survive redirects (**CVE-2025-68616**, WeasyPrint) and must get the *default*
  filter right (**GHSA-2r5c-gw76-rh3w**, Gitea). Re-resolve and re-check after every redirect hop.
- Archive extraction: zip-slip is still shipping in 2025 (**CVE-2025-3445**, `mholt/archiver` v3), and
  UnRAR's **CVE-2022-30333** was a pre-auth RCE via symlink traversal.
  `https://security.snyk.io/research/zip-slip-vulnerability`
- Licensing: the Unlicense has **no patent grant**, Google bars employee contributions to
  public-domain-equivalent licences, and public-domain dedication is not a recognised act in several
  European jurisdictions. Because the current code is already dedicated to the public domain, relicensing
  *this* snapshot needs nobody's permission — the window is now.
  `https://en.wikipedia.org/wiki/Unlicense` · `https://raw.githubusercontent.com/spdx/license-list-data/main/text/Apache-2.0.txt`

### 5.10 Plan writing (`planwriting.md`)

- EARS has six patterns, developed at Rolls-Royce plc and published at RE'09 (Mavin, Wilkinson, Harwood,
  Novak). "Complex" is a *class* of combinations, not one fixed shape — a linter hard-coding six templates
  will reject valid complex requirements. `https://alistairmavin.com/ears/`
- MADR: copy the template into `docs/decisions` as `nnnn-title.md`. The minimal template's section list
  was diffed against the raw file. `https://raw.githubusercontent.com/adr/madr/main/template/adr-template-minimal.md`
- **GitHub does not render Mermaid `C4Context`/`C4Container`**, and the `click` directive is blocked by
  GitHub's CSP. Use `flowchart` with subgraphs instead.
  `https://github.com/orgs/community/discussions/197898` · `https://github.com/orgs/community/discussions/46096`
- Claude Code reads `CLAUDE.md`, not `AGENTS.md`; the bridge is an `@AGENTS.md` import. Imports load at
  launch, so splitting files organises but does not reduce context.
  `https://code.claude.com/docs/en/memory`
- Doc-lint is enforceable: `lychee --offline --include-fragments` checks local anchors (verified flag), and
  a `grep` for hedging words catches "we could"/"maybe"/"TBD" before review.
  `https://raw.githubusercontent.com/lycheeverse/lychee/master/README.md`

---

## 6. What we could not verify

Nobody should later mistake any of the following for a settled fact. Each is carried forward as
`UNVERIFIED`; where a document depends on one, it must say so.

| Topic | Unverified claim | Consequence |
|---|---|---|
| Download Station | The current DS package version. `3.8.16-3566` came from a third-party blog; `4.1.2-5012` from the catalogue page's embedded JSON. Synology's release-notes page is JS-rendered. | Never quote a DS version as authoritative. |
| Download Station | The complete DSM 7 default BT-search engine list (only 1337x is confirmed); the RSS "Update interval" dropdown values; the PHP version inside the DLM sandbox; that eMule = aMule; the full task-list column set. | The `.dlm` import path must not assume a PHP version, and the UI must not claim DS-identical columns. |
| Download Station | Verbatim user complaints. SynoForum returns 403, Synology Community renders client-side, and `web.archive.org` was blocked. The abandonment case rests entirely on Synology's own dated release notes. | State the abandonment case from changelog evidence only. |
| Alternatives | Star counts for Sonarr/Radarr/Lidarr/Bazarr/Tube Archivist/ytdl-sub/cobalt/MeTube; licences for Recyclarr, autobrr, Seerr, cobalt, Pinchflat, Tube Archivist; Seerr's Docker image name; put.io API details; uGet's current release. | Do not cite these numbers in the README or in a decision. |
| Alternatives | Gopeed's file-selection and scheduling capabilities. | v1 has no Gopeed adapter, so nothing depends on it. |
| Indexers | Jackett's wiki "Definition format" was read via summarisation, not byte-exact; `.Query.IsTVSearch` / `.Query.IsImdbQuery` were not found in Prowlarr's source — treat as Jackett-only. | The Cardigann importer must degrade gracefully on unknown template variables. |
| Indexers | The qBittorrent plugin directory on Linux (`~/.local/share/qBittorrent/nova3/engines/`) is inferred from Qt conventions, not quoted. `accountsupport` in `INFO` is absent from the 2011 DLM guide. | Do not hardcode either path. |
| Indexers | Stable machine-readable torrent feeds for Linux Mint, Fedora and Kiwix. Prowlarr/Indexers has no LICENSE file and is Jackett-derived (GPL-2.0). | Ship no engine for those three; do not bulk-reuse Prowlarr definitions without a human legal decision. |
| Engines | The exact release *year* of aria2 1.37.0 could not be read from the rendered page (GitHub omits the year); 2023-11-15 is corroborated by issue #2337 saying "soon 2.5 years" in January 2026. | Quote the gap, not a precise anniversary. |
| Engines / security | Whether `yt-dlp -U` verifies release signatures. No evidence of signature verification was found. | Self-update is disabled at runtime; the image pins an exact version verified by SHA-256. |
| Architecture | Whether Huma's `sse.Message.Comment` exists at tag `v2.39.1` — it was read on `main`. Image size ≈ 25–35 MB is an inference, not a measurement. | Check the tag before relying on `Comment`. |
| Architecture | "Go is more reliable for a weak model than Python" is an inference argued against a benchmark that points the other way for single-shot synthesis. | It is a decision driver, not a measured fact. See [`decisions/0002-go-for-the-backend.md`](decisions/0002-go-for-the-backend.md). |
| Deployment | SABnzbd's HTTP API was not re-verified in the deployment pass; only the container image was checked. | Usenet is a v2 non-goal, so nothing in v1 depends on it. |
| UI/UX | The DS task-status enum in Appendix A of the API PDF could not be text-extracted in that pass (it was confirmed separately in the Download Station report). `@tanstack/react-table` v9 was ~4 weeks old and its API names differ from v8. | v8 is pinned; see the pinned-version list in [`03-architecture.md`](03-architecture.md). |
| Security | Project Zero issue 1447's original text could not be retrieved; Ormandy's wording is secondary-sourced. The Huntarr API-key-exposure report has no CVE. CVE-2024-45248 does **not** relate to the *arr apps. | Cite the technical substance, not the quotes; do not cite CVE-2024-45248 for the *arrs. |
| Security | Whether `openat2` is available in the target deployment (needs Linux ≥ 5.6 and a permissive seccomp profile). | Always implement the portable path-resolution fallback. |
| Plan writing | GitHub's currently deployed Mermaid version (a 2025 community answer said v11.4.1). | Restrict diagrams to the four syntaxes listed in the house style. |

---

## Decisions referenced

Files live in [`decisions/`](decisions/).

| ADR | Decision |
|---|---|
| ADR-0001 | Build a control plane over existing download engines |
| ADR-0005 | aria2, qBittorrent and yt-dlp as the v1 engines |
| ADR-0008 | Torznab first, declarative YAML engines second |
| ADR-0009 | A native cross-protocol RSS rule engine |
| ADR-0010 | Never execute third-party definition code |
| ADR-0013 | Mandatory built-in authentication |
| ADR-0016 | Relicense from the Unlicense to Apache-2.0 (proposed) |

## Open questions

- None. The ten research reports cited in §5 are deliberately **not** vendored into the repository: this
  document is their durable summary, and every claim above carries the primary-source URL it came from, so
  re-verification goes to the source rather than to a stale copy.

## Change log

| Date | Change |
|---|---|
| 2026-09-01 | Initial version |
