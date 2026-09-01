# 01 — Vision and Scope

> **Status:** draft
> **Last reviewed:** 2026-09-01
> **Audience:** implementing agent
> **Read this before:** [`02-requirements.md`](02-requirements.md), [`09-web-ui-spec.md`](09-web-ui-spec.md), [`16-prior-art-and-research.md`](16-prior-art-and-research.md), T001

## Purpose

State the problem dl-tool solves, who it serves, and exactly which Synology Download Station (DS) behaviours v1 reproduces, improves, defers or refuses. This file is arc42 sections 1–2 (Introduction and Goals; Constraints) for this repository; it carries no requirement text, no architecture and no configuration.

## Scope of this document

- In scope: problem statement with dated evidence, personas, the DS parity table, the differentiators, v1 non-goals, quality goals, content/legal posture.
- Out of scope (lives instead in): requirement sentences and IDs → [`02-requirements.md`](02-requirements.md); components, engines and diagrams → [`03-architecture.md`](03-architecture.md); env vars → [`11-config-reference.md`](11-config-reference.md); compose services, ports and volumes → [`10-deployment-and-compose.md`](10-deployment-and-compose.md); the landscape survey and full parity matrix of other tools → [`16-prior-art-and-research.md`](16-prior-art-and-research.md); the one-time import of an existing Download Station → [`15-migration-and-import.md`](15-migration-and-import.md).

## 1. The problem

**What Download Station is.** DS is a DSM web application that presents one task list over four to five separate third-party engines: Transmission for BitTorrent, NZBGet for Usenet, aMule/eMule for ED2K, wget/curl for HTTP/FTP, and pyLoad for file-hosting sites. Synology's own description is: "a web-based download application which allows you to download files from the Internet through BT, FTP, HTTP, NZB, FlashGet, QQDL, and eMule, and subscribe to RSS feeds". It accepts the nine URI schemes `ftps:// sftp:// magnet:// http:// https:// ftp:// thunder:// flashget:// qqdl://`, browses DSM shared folders as destinations, runs a 24×7 bandwidth schedule, searches trackers through uploaded `.dlm` plug-ins, and auto-extracts archives. The current shipping version is **4.1.2-5012, published 2026-04-14**, DSM 7.2+.

**That it is semi-abandoned.** Synology's own release-note dataset shows fourteen DSM 7 releases in five years with repeated multi-quarter stalls: a 15-month gap over 2019-07-02 → 2020-10-20, a **17-month gap** over 2021-11-17 → 2023-04-25, and another **17-month gap** over 2024-05-07 → 2025-10-08. The version that ended the first 17-month gap, **`4.0.0-4708` (2023-04-25)**, is a major-version bump whose entire release note is the single compatibility line "Download Station 4.0.0 requires DSM 7.2 and above." The bundled BitTorrent engine was frozen at **Transmission 2.93 from 2018-04-16 until 2025-11-25**, when `4.1.0-5005` moved it to **4.0.5** — a build that was already one patch behind its own 4.0 line (4.0.6 shipped 2024-05-29) on the day it was bundled, against an upstream now at 4.1.3 (June 2026). Features were removed rather than added: KickassTorrents BT search dropped in `3.7.3-3409` (2016-08-31), Xunlei-Lixian maintenance ceased in `3.8.6-3481` (2017-10-05), and its UI and Web API were deleted in `3.8.11-3517` (2018-10-15). The one security feature shipped in 2024 — SSRF blocklisting, in `4.0.2-4718` and `3.9.4-4625` — has no UI at all: the KB article instructs the user to sign in as **root over SSH**, edit `/var/packages/DownloadStation/etc/og_block_list.conf` in **`vi`**, `chmod a+r` it, and `synopkg restart DownloadStation`. The documentation matches: the current DSM 7 help page for creating a task still tells users "If you have installed Flash Player 9 or later, you can make multiple selections", five years after Flash reached end of life on 2020-12-31; the official Web API PDF was last revised 2014-03-26 and the `.dlm` developer guide 2011-03-14.

**What a self-hoster is left with.** The engines DS wraps are individually excellent and expose stable HTTP APIs, but nothing composes them. No maintained project puts BitTorrent, Usenet, HTTP-hoster, ED2K and media-site URLs in one queue. No torrent engine has real multi-user: qBittorrent issue #3327, "Multiple users for WebUI", was opened **2015-06-29** and is still open eleven years later; Transmission, Deluge, aria2, Gopeed, qui and VueTorrent are all single-user. No engine exposes a directory-listing API, so every one of them takes a destination as an opaque string and the folder picker DS users rely on simply does not exist in the open-source world. A composed stack of qBittorrent + SABnzbd + aria2 + Prowlarr + Unpackerr covers 13 of 14 DS features at the daemon level and none of them as one coherent experience, because the user must know which of five web UIs to open. dl-tool is that missing layer, and DS's own defects — the 2048/256 task caps, DSM-local-users-only, seeding auto-stop needing ratio **and** time, KB/s in configuration versus B/s in reporting — are the list of things it fixes on the way.

| Dated evidence | Value |
|---|---|
| Current DS version | `4.1.2-5012`, 2026-04-14 |
| Longest release gaps | 2021-11-17 → 2023-04-25 (17 months); 2024-05-07 → 2025-10-08 (17 months) |
| Empty major version | `4.0.0-4708` (2023-04-25) — release note is one compatibility sentence |
| BT engine freeze | Transmission 2.93 (2018-04-16) → 4.0.5 (2025-11-25) |
| Feature removals | KickassTorrents `3.7.3-3409` 2016-08-31; Xunlei-Lixian UI + Web API `3.8.11-3517` 2018-10-15 |
| 2024 security feature with no UI | SSRF blocklist in `4.0.2-4718` / `3.9.4-4625`, configured only by root SSH + `vi` |
| Documentation rot | `download_create` still requires "Flash Player 9 or later"; API PDF revised 2014-03-26 |
| Same bug fixed twice | `3.9.5-4627` and `4.0.3-4720`, both 2024-05-07, both "error message 117 when download links contained spaces" |

## 2. Personas

**The household NAS owner (admin).** Runs one Docker host beside the TV, pastes a mixed blob of HTTPS links, a magnet and a `.torrent` file into one box, picks a destination by clicking through folders under `/data`, and expects the archive to be extracted and the seed to stop on its own. Their job to be done: one screen that shows every transfer regardless of protocol, and settings that survive a reboot without an SSH session.

**The flatmate (second user).** Gets an account, sees only their own tasks, downloads into their own default destination, and cannot exhaust the household's disk or bandwidth because a quota and the global governor bound them. Their job to be done: add and manage their own downloads without seeing, pausing or deleting anyone else's, and without the admin handing out a shared password.

**The Download Station migrant.** Arrives from a NAS holding live tasks, RSS feeds and filters, and wants that same screen on hardware they choose. Their job to be done: point dl-tool at the NAS once, review a dry run, have the tasks and feeds re-created locally, and then work in a UI whose sidebar, add-task dialog, detail tabs and settings tree sit exactly where Download Station put them — nothing to relearn → [`15-migration-and-import.md`](15-migration-and-import.md), [`09-web-ui-spec.md`](09-web-ui-spec.md).

## 3. Download Station parity table

`parity` = same observable behaviour · `improved` = dl-tool does strictly more · `deferred to v2` = designed for, not built in v1 · `out of scope` = deliberately never.

| Feature | DS behaviour (one line) | dl-tool v1 | Notes / link |
|---|---|---|---|
| Multi-protocol URL add | Accepts `ftps:// sftp:// magnet:// http:// https:// ftp:// thunder:// flashget:// qqdl://` in one dialog | improved | One queue also covers media-site URLs via yt-dlp; `ed2k://` parsed and rejected with a clear message → [`06-download-engines.md`](06-download-engines.md) |
| Batch URL paste cap | "You can enter up to 50 URLs in the box" | improved | No 50-URL cap; the blob is split, normalised and routed per URI → [`02-requirements.md`](02-requirements.md) FR-001–FR-029 |
| Upload file cap | "You can upload up to 50 files at a time"; `.torrent`, `.nzb`, `.txt` | improved | No 50-file cap; `.torrent` and `.txt` URL lists in v1, `.nzb` deferred |
| Obfuscated schemes | `thunder://`/`flashget://`/`qqdl://` are base64 wrappers, not protocols | parity | Decoded in normalisation before routing → [`06-download-engines.md`](06-download-engines.md) |
| FTP directory recursion | Trailing `/` on an `ftp://` link downloads the folder; "SFTP/FTPS folder downloading is not supported" | parity | Same rule, same restriction |
| Per-task FTP credentials | "Authentication required (for `ftp://` only)" checkbox with user/password | parity | `ftp_credentials` on `POST /tasks` → [`05-api-contract.md`](05-api-contract.md) |
| File-selection dialog | "Show dialog to select files for download"; per-URL pages; optional subfolder named after the list | improved | Modelled as inspect-then-create: `POST /tasks/inspect` returns the manifest without creating a task |
| Per-file priority / skip | File tab sets priority or skips files, BT and NZB only | parity | `PATCH /tasks/{id}/files` |
| Destination picker | Browses DSM shared folders per task | improved | Server-side browser over the configured roots, jailed; no engine anywhere exposes one → §4 |
| Watch folder | Torrent/NZB watched folder; optional "Delete loaded torrent/NZB files" | parity | `.torrent` only in v1; NZB with the v2 engine |
| Sidebar filters | All · Downloading · Completed · Active (downloading ∪ seeding) · Inactive (error ∪ waiting ∪ paused) · Stopped (paused by you) | improved | Identical semantics plus a new `Error` filter → [`09-web-ui-spec.md`](09-web-ui-spec.md) |
| Detail-pane tabs | General · Transfer · Tracker (BT) · Peers (BT) · File (BT/NZB) · Log (NZB) | parity | Same tab set; Log becomes a per-task event log for every protocol |
| Task actions | Pause, Resume, Remove (multi-select) | parity | `POST /tasks/actions` |
| Force-complete | "End incomplete or erroneous download tasks"; not resumable afterwards | parity | `DELETE /tasks/{id}?force_complete=true` |
| Clear completed | Manual "Clear completed items" button only | improved | Manual clear plus an auto-remove-on-complete setting DS never documented |
| Per-task limits | Edit sets auto-stop, max up/down rate, max peers; "cannot exceed the default setting" | parity | Same fields; the ceiling rule is kept |
| Per-task tracker list | The Edit dialog also customises "tracker lists for the current task" | parity | `GET`/`POST`/`DELETE /tasks/{id}/trackers`; DHT, PeX and LSD appear as pseudo-rows and cannot be removed → [`09-web-ui-spec.md`](09-web-ui-spec.md) |
| BT protocol settings | TCP port `16881` (settable 1–65535, 32 reserved ports), encryption `Disable`/`Auto`/`Always`, max peers per torrent, DHT toggle and DHT UDP port | parity | Same three-valued encryption, max-peers and DHT/PeX/LSD controls in the settings tree; the engine's listening port is published by the compose file → [`09-web-ui-spec.md`](09-web-ui-spec.md), [`10-deployment-and-compose.md`](10-deployment-and-compose.md) |
| Concurrent-task limits | Not exposed; parallelism is bounded only by `Process order` and the 2048/256 task caps | improved | `max_active_total`, `max_active_per_engine` and `max_active_per_user`, enforced by dl-tool because no engine can see another engine's queue; seeding counts toward none of them → [`06-download-engines.md`](06-download-engines.md) |
| Sorting | Column headers; "Default ordering is by the creation date of the download tasks" | parity | Same default sort |
| BT search with `.dlm` plug-ins | Uploaded `.dlm` tarballs run PHP `search.php` server-side | improved | Torznab/Newznab client plus declarative `dlsearch/v1` YAML; `.dlm` is statically analysed and converted, never executed → [ADR-0010](decisions/0010-never-execute-third-party-definitions.md), [`07-search-and-indexers.md`](07-search-and-indexers.md) |
| RSS feeds | Add feed, optional "Automatically download all items"; default poll interval 24 h | improved | Configurable interval with backoff → [`08-rss-automation.md`](08-rss-automation.md) |
| RSS download filters | `Name`, `Matches`, `Does not match`, `Destination`, `Parse with regular expressions`, `Test Filter`; one of Matches/Does-not-match required; applies to newly added items only | improved | Cross-protocol rules that can land in any engine, plus a dry run that reports a reason code for every evaluated item → [ADR-0009](decisions/0009-native-cross-protocol-rss-rules.md) |
| 24×7 bandwidth schedule | Grid painted with three states: No Download · Default Speed · Alternative Speed | parity | Same three states, `GET|PUT /settings/schedule` |
| Alternative speeds | Second pair of max rates; "available for BT tasks only" | improved | Applies to every engine, not just BitTorrent |
| Global + per-task limits | Set through server config and per-task edit | improved | One governor fans out to every engine's own limit API → §4 |
| Auto-extract | `.zip .tar .gz .tgz .rar .7z`; extract-to-current-or-fixed; create subfolders; overwrite-or-skip; delete archive on success | parity | Same option set; `extracting` state with 0–100 progress |
| Extraction password list | Shared list, max 30 entries, each ≤ 1024 characters, tried automatically | improved | Same auto-retry behaviour, no 30-entry cap |
| Seeding auto-stop | Share ratio and seeding interval; "Download Station will stop tasks when BOTH criteria are met" | improved | Ratio **or** time, whichever is reached first; DS's AND is the documented defect being fixed |
| Process order | `By date created` or `By user (one task at a time)` round robin across owners | parity | Kept verbatim as a scheduler mode |
| Task count caps | 2048 tasks for `admin`/`administrators`, 256 for ordinary users | improved | No hard cap; per-user limits are expressed as quotas instead |
| Multi-user | "Download Station is available for local DSM users only but not for domain users or LDAP users" | improved | Built-in local users, mandatory auth, per-user default destination, quota and ownership filtering → [ADR-0013](decisions/0013-mandatory-built-in-authentication.md) |
| Per-user destination jail | One global `Default Destination Folder`; Download Station itself confines no user to a subtree | improved | Every non-admin is jailed to the subtree of their `default_destination` for `/fs/browse`, `/fs/free-space`, `/fs/mkdir` and task destinations; admins get all configured roots → [`12-security-and-threat-model.md`](12-security-and-threat-model.md) |
| Statistics | Global download and upload rate for the whole service | parity | Reported in bytes/second everywhere; DS's KB/s-config vs B/s-reporting split is not copied |
| Notifications | Email + desktop, per-event checkboxes, requires DSM Control Panel SMTP | improved | Multiple configurable channels (`webhook`, `ntfy`, `gotify`, `apprise`) with a per-event × per-channel matrix and `POST /notifications/{id}/test`, which returns the raw upstream status line and body; no NAS-wide mail relay → [`05-api-contract.md`](05-api-contract.md), [`09-web-ui-spec.md`](09-web-ui-spec.md) |
| Backup / restore | Hyper Backup 3.0+ saves config, RSS feeds/items/filters, tasks, plug-ins; excludes accounts and stored passwords | parity | `POST /system/backup` produces a self-contained archive with no Hyper Backup dependency |
| Migration from a live NAS | Tasks, feeds and filters stay on the NAS; Hyper Backup restores them into DSM | improved | A one-time, read-only, dry-run-first wizard signs in to DSM as an ordinary client, re-creates tasks and imports RSS sites and feeds, then is never used again; dl-tool never reads Download Station's on-disk database → [`15-migration-and-import.md`](15-migration-and-import.md) |
| Mobile client | DS get (Android), documented version 1.11 | parity | Responsive layout below 640 px: card list, bottom-sheet actions, long-press multi-select → [`09-web-ui-spec.md`](09-web-ui-spec.md). dl-tool serves `/api/v1` only, so DS get and DS browser extensions do not work against it |
| Installable web app | None; the mobile client is a separate app-store download | improved | Web app manifest, maskable icons, `standalone` display, `theme-color` matching dark mode, and a service worker whose only jobs are the install criterion and caching static assets. Nothing works offline → [`09-web-ui-spec.md`](09-web-ui-spec.md) |
| NZB / Usenet | News server config, max connections, par2 repair and cleanup | deferred to v2 | The `Engine` interface must not preclude it → [ADR-0005](decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| eMule / ED2K | Separate service, own settings tree, own scheduler, TCP 4662 / UDP 4672 | deferred to v2 | `ed2k://` is parsed and rejected with a clear message in v1 |
| File-hosting premium accounts | Per-site credentials under Settings > File Hosting, backed by pyLoad | out of scope | No hoster scraping, no captcha solving |
| Xunlei-Lixian / FLVCD | Removed by Synology in 2017–2018 | out of scope | Media-site URLs are served by yt-dlp instead |

## 4. Differentiators

**The interface is the product claim.** The primary goal is UX parity with Download Station: its layout, its sidebar taxonomy and set semantics, its add-task dialog with the Destination field and its **Select** folder browser, its file-selection dialog, its detail-pane tabs, its settings tree and its 24×7 schedule grid are the specification, not an inspiration. A person who knows Download Station must learn nothing to use dl-tool, which makes [`09-web-ui-spec.md`](09-web-ui-spec.md) the centrepiece of this plan.

**Improvements are additive and appear where a DS user already looks.** The new `Error` node sits in the existing sidebar, the rule dry run sits inside the existing filter editor, dark mode sits under the existing settings tree. Nothing a DS user already knows is moved or renamed.

**Shapes DS taught its users are kept even where they are internal:** the async search job (start → poll until finished → clean), the create-list → manifest → download-selected file-selection flow, and the task, detail, transfer, file, tracker and peer record shapes.

The five capabilities below are what nothing else in the landscape offers; each one is reached through the Download Station screen above, never as a separate surface.

1. **One queue spanning HTTP/FTP/SFTP, BitTorrent and media-site URLs**, for every task dl-tool created. dl-tool assumes exclusive control of its engines: a task created directly on an engine appears only where `engines.foreign_task_policy` is `adopt` → [ADR-0017](decisions/0017-exclusive-control-of-engines.md), [`06-download-engines.md`](06-download-engines.md). No maintained project unifies these: qBittorrent, Transmission, Deluge and Flood are BitTorrent-only, SABnzbd is NZB-only, and the one project that attempted the union has five stars and a README stating it is not production-ready.
2. **Multi-user with per-user default destination, quota and task ownership.** qBittorrent issue #3327 "Multiple users for WebUI" was opened 2015-06-29 and remains open; Transmission, Deluge, aria2 and Gopeed are single-credential, and only Flood has multi-user — for torrents alone.
3. **A server-side destination browser.** Every engine takes the destination as a string (`savepath`, `download_dir`, `dir`) and **no engine exposes a directory-listing API**, so the folder picker DS users depend on has no open-source equivalent.
4. **Pluggable search as a first-class user feature.** Torznab/Newznab search exists in the wild only as an *arr automation surface or as unofficial Python plug-ins for qBittorrent; there is no official Torznab engine in qBittorrent 5.2.3, and Transmission, Deluge, aria2 and Gopeed have no search at all.
5. **A global bandwidth governor and 24×7 schedule that fans out to every engine.** Each engine schedules only itself (`scheduler_enabled`, `alt_speed_time_*`, SABnzbd's config scheduler), so one cross-engine cap and one grid exist nowhere.

## 5. Non-goals for v1

| Non-goal | Reason |
|---|---|
| Writing a BitTorrent, NNTP/par2 or hoster-scraping engine | The engines are mature and expose stable HTTP APIs; re-implementing one is the classic failure of this product class → [ADR-0001](decisions/0001-control-plane-over-existing-engines.md) |
| Usenet / NZB | Deferred to v2; the `Engine` interface is designed so adding SABnzbd or NZBGet needs no interface change |
| eMule / ED2K | Deferred to v2; no maintained engine offers a modern ED2K web API, so `ed2k://` is parsed and rejected with a clear message |
| File-hosting premium accounts and captcha solving | Out of scope entirely; it requires per-site credential scraping and defeated captchas, neither of which can be maintained |
| An embedded DHT crawler | Documented only as an optional external bitmagnet Torznab provider; indexing is not this product's job |
| Executing any third-party code | No PHP, no Python plug-ins, no scripting runtime; the entire `.dlm` ecosystem is a remote-code-execution surface → [ADR-0010](decisions/0010-never-execute-third-party-definitions.md) |
| A second datastore | SQLite only, no Postgres profile; a second database is a second failure mode → [ADR-0004](decisions/0004-sqlite-as-the-only-datastore.md) |
| Telemetry or a phone-home update check | Not present by default or otherwise |

## 6. Quality goals

arc42 section 10 lives in [`02-requirements.md`](02-requirements.md); every non-functional requirement is allocated the range **NFR-001 – NFR-030**. This table names the goal and the scenario that proves it; the requirement sentences and their `Verify:` lines are in that file.

| Goal | Scenario that proves it | NFR area in [`02-requirements.md`](02-requirements.md) |
|---|---|---|
| Responsiveness under load | 2 000 tasks in the grid scroll and update over SSE without the UI dropping frames | Performance (NFR-001–NFR-030) |
| Safe by default | A fresh container refuses every request until the first-run wizard creates an admin; there are no default credentials | Security (NFR-001–NFR-030) |
| Hostile-input resistance | A task URL pointing at a link-local address, a path escaping the configured roots, and a zip-slip archive are each rejected with a specific error code | Security (NFR-001–NFR-030) |
| One-artifact portability | `docker compose up -d` on amd64 and arm64 yields a working instance with no host dependency beyond Docker | Portability (NFR-001–NFR-030) |
| Operability without a shell | Every setting a user can change is changeable in the UI; nothing requires SSH or a text editor | Operability (NFR-001–NFR-030) |
| Accessibility | The task grid and every dialog are fully keyboard-operable and expose accessible names | Accessibility (NFR-001–NFR-030) |
| Localisability | All UI strings resolve through i18next; no string is hard-coded in a component | i18n (NFR-001–NFR-030) |

## 7. Content and legal posture

dl-tool ships **zero** piracy indexers — not disabled, not commented out, absent. Exactly four legitimate search engines are bundled and enabled by default: `internet-archive` (JSON), `arch-linux` (RSS), `academic-torrents` (RSS) and `linux-distributions` (HTML directory index); their definitions and endpoints are in [`07-search-and-indexers.md`](07-search-and-indexers.md). Adding any other indexer is a deliberate, documented user action, and every imported engine starts **disabled** and displays its provenance. dl-tool is protocol-neutral infrastructure in the same sense as curl or a web browser; the README states that once, and neither the UI nor these documents moralise or add warnings. There is no telemetry and no phone-home update check, by default or otherwise.

**IMPORTANT** No default indexer, sample configuration, test fixture or documentation example in this repository may name, link to or imply a piracy tracker.

## Decisions referenced

| ADR | Decision |
|---|---|
| [0001](decisions/0001-control-plane-over-existing-engines.md) | Build a control plane over existing download engines |
| [0004](decisions/0004-sqlite-as-the-only-datastore.md) | SQLite as the only datastore |
| [0005](decisions/0005-aria2-qbittorrent-ytdlp-engines.md) | aria2, qBittorrent and yt-dlp as the v1 engines |
| [0008](decisions/0008-torznab-first-declarative-yaml-second.md) | Torznab first, declarative YAML engines second |
| [0009](decisions/0009-native-cross-protocol-rss-rules.md) | A native cross-protocol RSS rule engine |
| [0010](decisions/0010-never-execute-third-party-definitions.md) | Never execute third-party definition code |
| [0013](decisions/0013-mandatory-built-in-authentication.md) | Mandatory built-in authentication |
| [0017](decisions/0017-exclusive-control-of-engines.md) | dl-tool assumes exclusive control of its engines |

## Open questions

- The quality-goal table names NFR *areas*, not individual NFR IDs, because [`02-requirements.md`](02-requirements.md) owns the allocation inside NFR-001 – NFR-030. Replace each area with the concrete ID range once that file is written.
- <!-- UNVERIFIED: DS's RSS "Update interval" dropdown values are never enumerated by Synology; community reports of 1 hour / 10 minutes are unconfirmed. dl-tool sets its own default in `11-config-reference.md`. -->
- <!-- UNVERIFIED: the complete DSM 7 default BT-search engine list. Only 1337x is confirmed, from the 4.1.0-5005 release note. It has no bearing on dl-tool's bundled set, which is fixed at the four legitimate engines above. -->
- <!-- INFERRED: that DS's 24×7 grid degrades to on/off for HTTP/FTP/NZB. Synology states only "Downloading or uploading files at alternative speeds is available for BT tasks only"; the degradation follows but is not quoted. -->

## Change log

| Date | Change |
|---|---|
| 2026-09-01 | Initial version |
| 2026-09-01 | Compatibility façades cut. Automation-user persona replaced by the Download Station migrant; §4 rebuilt around UX parity as the product claim and the one-queue claim conditioned on `foreign_task_policy`; parity table gains per-task trackers, BT protocol settings, concurrency limits, the per-user destination jail, migration import and PWA install, and loses every API-compatibility claim. |
