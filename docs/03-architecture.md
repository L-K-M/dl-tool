# 03 — Architecture

> **Status:** draft
> **Last reviewed:** 2026-09-01
> **Audience:** implementing agent
> **Read this before:** T001, T004, T015, T019, T028, T039, T065, T098, T101, T102 — and before adding any package under `internal/`

## Purpose

Define the static and runtime structure of dl-tool: which processes and Go packages exist, how a task
travels from a browser click to an engine daemon, and which concepts every package shares. It does **not**
define DDL, HTTP payload shapes, the `Engine` interface body, or the compose file.

## Scope of this document

- In scope: system context, container topology, package decomposition, four runtime scenarios including
  admission control, the task state machine, the ID scheme, the error model, the units rule, job semantics,
  the SSE ring, engine conformance, the foreign-task policy, risks.
- Out of scope, and where it lives instead: DDL and enums → [`04-data-model.md`](04-data-model.md);
  endpoints and payloads → [`05-api-contract.md`](05-api-contract.md); the `Engine` interface and engine
  status tables → [`06-download-engines.md`](06-download-engines.md); RSS rule schema →
  [`08-rss-automation.md`](08-rss-automation.md); compose, ports, volumes →
  [`10-deployment-and-compose.md`](10-deployment-and-compose.md); environment variables →
  [`11-config-reference.md`](11-config-reference.md); threat model →
  [`12-security-and-threat-model.md`](12-security-and-threat-model.md).

Headings follow [arc42](https://docs.arc42.org/home/) sections 3–8 and 11: section 9 lives in
[`decisions/`](decisions/), 10 in [`02-requirements.md`](02-requirements.md), 12 in
[`glossary.md`](glossary.md).

---

## 3. Context and Scope

dl-tool is one process that owns state and delegates transfers. Everything outside the box is a system it
talks to but does not control.

```mermaid
flowchart LR
    op["Operator<br/>web browser"]
    idx["Torznab and Newznab indexers<br/>Prowlarr, Jackett, bitmagnet"]
    feeds["RSS and Atom feeds"]
    subgraph sys["dl-tool — control plane"]
        dlt["dl-tool<br/>unified queue, users, destinations,<br/>search, RSS rules, bandwidth schedule, UI"]
    end
    eng["Download engines<br/>aria2, qBittorrent-nox, yt-dlp<br/>fetch from origin servers and the swarm into /data"]
    dsm["Synology NAS running Download Station<br/>one-time migration source only"]
    op -->|"HTTPS: REST /api/v1 plus SSE /api/v1/events"| dlt
    dlt -->|"one-time, read-only migration import"| dsm
    dlt -->|"HTTP GET: Torznab caps and search"| idx
    dlt -->|"conditional GET: If-None-Match, If-Modified-Since"| feeds
    dlt -->|"JSON-RPC, WebAPI v2, subprocess"| eng
```

| External interface | Direction | Protocol | Detailed in |
|---|---|---|---|
| Operator browser | inbound | REST `/api/v1` plus SSE | [`05-api-contract.md`](05-api-contract.md) |
| Download Station NAS | outbound | one-time, read-only migration client over `SYNO.*` | [`15-migration-and-import.md`](15-migration-and-import.md) |
| aria2 | outbound | JSON-RPC 2.0 over HTTP POST at `/jsonrpc`; notifications over WebSocket only | [`06-download-engines.md`](06-download-engines.md) |
| qBittorrent-nox, yt-dlp | outbound | WebAPI v2 with a `SID` cookie session; local subprocess with JSON on stdout | [`06-download-engines.md`](06-download-engines.md) |
| Indexers and feeds | outbound | Torznab/Newznab XML and RSS/Atom over HTTPS | [`07-search-and-indexers.md`](07-search-and-indexers.md) |

dl-tool serves `/api/v1` only. It exposes no compatibility surface for third-party clients: there is no
qBittorrent WebAPI façade and no Synology DownloadStation façade, and dl-tool never claims to be a
drop-in download client for other software. The goal is UX parity with Download Station, not API parity.

Non-goals: no BitTorrent, NNTP/par2 or hoster-scraping implementation, no DHT crawler, no interpreter for
third-party code, no compatibility façade. See [`01-vision-and-scope.md`](01-vision-and-scope.md).

---

## 4. Solution Strategy

**Control-plane thesis (D1).** dl-tool implements the layer no engine ships: one queue spanning
HTTP/FTP/SFTP, BitTorrent and media-site URLs; multi-user ownership with per-user destinations and quotas; a
server-side destination browser; pluggable search as a user feature; and a global bandwidth governor with a
24×7 schedule that fans out to every engine. Transferring bytes is delegated to existing daemons.

| # | Decision | ADR |
|---|---|---|
| D1 | Control plane over external engine daemons, not a new engine. | [ADR-0001](decisions/0001-control-plane-over-existing-engines.md) |
| D2 | Go 1.26 backend, single static binary built with `CGO_ENABLED=0`. | [ADR-0002](decisions/0002-go-for-the-backend.md) |
| D3 | chi v5 plus Huma v2; REST with OpenAPI 3.1 generated from the handler structs. | [ADR-0003](decisions/0003-chi-huma-code-first-openapi.md) |
| D4 | SQLite via `modernc.org/sqlite` at `/config/dl-tool.db` in WAL mode; no Postgres. | [ADR-0004](decisions/0004-sqlite-as-the-only-datastore.md) |
| D5 | Engines are aria2, qBittorrent-nox and yt-dlp behind one `Engine` interface. | [ADR-0005](decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| D6 | SSE at `GET /api/v1/events` carrying rid deltas, with `GET /api/v1/sync?rid=` as the polling fallback. | [ADR-0006](decisions/0006-sse-with-rid-deltas.md) |
| D7 | React 19 plus Vite plus TypeScript SPA, `//go:embed`-ed into the binary. | [ADR-0007](decisions/0007-react-spa-embedded-in-the-binary.md) |
| D8 | Search is a Torznab/Newznab client first, `dlsearch/v1` declarative YAML engines second. | [ADR-0008](decisions/0008-torznab-first-declarative-yaml-second.md) |
| D9 | A native cross-protocol RSS rule engine, not a passthrough to qBittorrent's `rss/*`. | [ADR-0009](decisions/0009-native-cross-protocol-rss-rules.md) |
| D10 | No third-party code execution; `.dlm` and qBittorrent `.py` plugins are statically analysed and converted. | [ADR-0010](decisions/0010-never-execute-third-party-definitions.md) |
| D11 | Alpine 3.22 runtime image with `su-exec` for PUID/PGID/UMASK privilege drop. | [ADR-0011](decisions/0011-alpine-runtime-with-puid-pgid.md) |
| D12 | A single `/data` bind mount plus `/config`, identical path in every container. | [ADR-0012](decisions/0012-single-data-mount.md) |
| D13 | Built-in local users and server-side sessions are mandatory; no anonymous mode, no default credentials. | [ADR-0013](decisions/0013-mandatory-built-in-authentication.md) |
| D15 | In-process worker pool plus a `jobs` table in SQLite plus `robfig/cron/v3`. | [ADR-0015](decisions/0015-db-backed-in-process-job-queue.md) |
| D16 | Declarative-only extensibility in v1; if v2 ever needs scripting it is Starlark. | [ADR-0010](decisions/0010-never-execute-third-party-definitions.md) |
| — | dl-tool assumes exclusive control of every engine it is configured with; see §8.7 and §8.8. | [ADR-0017](decisions/0017-exclusive-control-of-engines.md) |
| — | yt-dlp is pinned by version and SHA-256 at image build time and never self-updates. | [ADR-0018](decisions/0018-pin-ytdlp-by-version-and-hash.md) |

D14 (compatibility façades) is withdrawn: dl-tool serves `/api/v1` only, ADR-0014 does not exist, and the
number is never reused. ADR-0017 and ADR-0018 postdate the settled D-list, so they carry no `D` number.

---

## 5. Building Block View

### 5.1 Level 2 — containers and deployment units

Ports, volumes and profile names are owned by [`10-deployment-and-compose.md`](10-deployment-and-compose.md);
the annotations here let this diagram double as a cross-check against `compose.yaml`.

```mermaid
flowchart TB
    op["Operator browser"]
    subgraph host["Docker host — compose project dl-tool"]
        app["dl-tool<br/>image ghcr.io/l-k-m/dl-tool<br/>container port 8080, host port 8091<br/>Go binary with embedded React SPA<br/>yt-dlp runs here as a subprocess"]
        qbt["qbittorrent<br/>image lscr.io/linuxserver/qbittorrent<br/>WebUI 8080 on the internal network only<br/>BitTorrent 6881 tcp and udp"]
        aria["aria2<br/>built in-repo from alpine plus apk add aria2<br/>JSON-RPC 6800 at path /jsonrpc"]
        opt["optional profiles<br/>gluetun, image qmcgaw/gluetun, owns the engine network namespace<br/>caddy, image caddy:2, ports 80 and 443, TLS termination"]
        cfg[("/config — local volume<br/>dl-tool.db in WAL mode, definitions")]
        data[("/data — one bind mount<br/>identical path in every container")]
    end
    op -->|"HTTPS via caddy, or HTTP direct to host port 8091"| app
    app -->|"JSON-RPC POST plus WebSocket notifications"| aria
    app -->|"WebAPI v2 with SID cookie"| qbt
    app --> cfg
    app --> data
    qbt --> data
    aria --> data
    opt -.->|"network_mode service, or reverse proxy to 8080"| app
```

- Engines report absolute paths (`aria2.getFiles` → `path`; qBittorrent → `content_path`), so `/data` must
  be the identical string in every container or dl-tool cannot `stat` what they wrote.
- Publish only dl-tool's port and the BitTorrent listen port; never an engine's API port.
- yt-dlp is not a container: it is a subprocess of dl-tool at `DLTOOL_YTDLP_PATH`.

### 5.2 Level 3 — Go packages

```mermaid
flowchart TB
    main["cmd/dl-tool<br/>humacli wiring only"]
    api["internal/api<br/>chi, Huma, SPA static, SSE"]
    secure["internal/secure<br/>ssrf, hash, session, csrf"]
    engine["internal/engine<br/>Engine interface, registry, router"]
    uri["internal/uri<br/>normalize, obfuscated, magnet, metainfo"]
    search["internal/search"]
    rss["internal/rss"]
    jobs["internal/jobs<br/>worker pool and cron"]
    syncp["internal/sync<br/>hub, delta, ring"]
    fsx["internal/fsx<br/>browse, safepath, move, space"]
    a2["internal/engine/aria2"]
    qb["internal/engine/qbittorrent"]
    yt["internal/engine/ytdlp"]
    ct["internal/engine/enginetest"]
    store["internal/store<br/>sqlx, goose migrations, models"]
    cfgp["internal/config"]
    obs["internal/obs<br/>log, metrics, health"]
    main --> api & cfgp & obs & jobs
    api --> secure & uri & engine & syncp & search & rss & fsx
    engine --> a2 & qb & yt
    jobs --> engine & fsx
    rss --> jobs
    syncp --> engine
    search --> secure
    api & jobs & syncp & engine --> store
    ct -.->|"one contract suite every adapter must pass"| engine
```

| Package | Responsibility |
|---|---|
| `cmd/dl-tool` | `humacli` wiring: load config, open the store, build the router, start workers. No business logic. |
| `internal/config` | Parse `DLTOOL_*` environment variables into one struct and validate them at boot. |
| `internal/store` | All SQL: `sqlx` access, goose migrations, and row structs with `db:` and `json:` tags. |
| `internal/api` | chi router, Huma operations, session and CSRF middleware, embedded SPA, the SSE endpoint. |
| `internal/engine` | The `Engine` interface, the adapter registry, and the URI-to-engine routing table. |
| `internal/engine/aria2`, `.../qbittorrent` | Protocol client plus status and field normalisation, one package per daemon. |
| `internal/engine/ytdlp` | yt-dlp subprocess runner and stdout JSON parser. |
| `internal/engine/enginetest` | One shared conformance suite that every adapter must pass. |
| `internal/uri` | Normalise input URIs, decode `thunder`/`flashget`/`qqdl`, parse magnets and `.torrent` metainfo. |
| `internal/search` | Torznab client, `dlsearch/v1` YAML engines, `.dlm` import, result normalisation. |
| `internal/rss` | Feed polling, feed parsing, rule matching, episode filters. |
| `internal/jobs` | Worker pool over the `jobs` table, cron entries, and the extract, move and notify handlers. |
| `internal/sync` | The delta hub: snapshot diffing, the rid ring buffer, SSE fan-out. |
| `internal/fsx` | Destination browsing, path jailing, free-space queries, EXDEV-safe moves. |
| `internal/secure` | SSRF guard, argon2id password hashing, session tokens, CSRF tokens. |
| `internal/obs` | `log/slog` setup, the Prometheus registry, `/healthz` and `/readyz` handlers. |

Layering rule: `internal/store` imports nothing from `internal/api`, `internal/engine` or `internal/jobs`,
and adapters import `internal/engine` only. Adding an engine touches one directory plus one `registry.go` line.

---

## 6. Runtime View

### 6.1 Add a magnet with file selection

```mermaid
sequenceDiagram
    autonumber
    participant UI as Browser SPA
    participant API as internal/api
    participant URI as internal/uri
    participant ENG as internal/engine router
    participant QBT as qBittorrent WebAPI v2
    UI->>API: POST /api/v1/tasks/inspect with uris magnet
    API->>URI: normalise, then parse the magnet
    URI-->>API: manifest name, total_size, files
    API-->>UI: 200 manifest, no task created
    UI->>UI: operator ticks the files to keep
    UI->>API: POST /api/v1/tasks with select_files indices
    API->>ENG: route by scheme magnet
    ENG->>QBT: POST /api/v2/torrents/add with stopped true and paused true
    QBT-->>ENG: 200, empty body, no identifier returned
    ENG->>QBT: GET /api/v2/torrents/info to correlate the infohash
    QBT-->>ENG: torrent list including the new infohash
    ENG->>QBT: POST /api/v2/torrents/filePrio, pipe-separated ids, priority 0 for unticked files
    ENG->>QBT: POST /api/v2/torrents/start then POST /api/v2/torrents/resume
    ENG-->>API: engine_ref infohash
    API-->>UI: 201 with task id tsk_01J...
    Note over API,QBT: a magnet with no cached metainfo yields an empty files array,<br/>so inspect offers all-or-nothing selection instead
```

Send `stopped` and `paused` with the same value and call both `start` and `resume`, so one adapter serves
qBittorrent 4.x and 5.x. Detail in [`06-download-engines.md`](06-download-engines.md).

### 6.2 The live-update loop

```mermaid
sequenceDiagram
    autonumber
    participant POLL as Engine poll loop
    participant HUB as internal/sync hub
    participant RING as rid ring buffer, 300 entries
    participant SSE as GET /api/v1/events
    participant CL as Client reducer
    loop once per second
        POLL->>HUB: snapshot of every task from every engine
        HUB->>HUB: diff against the previous snapshot
        HUB->>RING: store the delta under rid N
        HUB->>SSE: event sync, id N, data delta
        SSE->>CL: text/event-stream frame
        CL->>CL: merge changed fields, delete tasks_removed
    end
    Note over SSE,CL: an SSE comment heartbeat goes out every 15 s, then the connection drops
    CL->>SSE: automatic reconnect carrying Last-Event-ID 41
    alt rid 41 is still inside the ring
        SSE-->>CL: coalesced delta 42 to N, full_update false, seq_gap false
    else rid 41 evicted, or no Last-Event-ID header
        SSE-->>CL: every task, full_update true, seq_gap true
        CL->>CL: replace the whole store
    end
    Note over CL: after 3 consecutive stream failures the client polls<br/>GET /api/v1/sync with rid N every 2 s for the identical payload
```

### 6.3 An RSS rule fires

```mermaid
sequenceDiagram
    autonumber
    participant CRON as robfig cron tick
    participant POLL as internal/rss poll
    participant FEED as Remote feed
    participant MATCH as internal/rss match
    participant JOBS as jobs table
    participant ENG as internal/engine router
    CRON->>POLL: fire feed_poll for fed_01J...
    POLL->>FEED: GET the feed URL with If-None-Match and If-Modified-Since
    alt 304 Not Modified
        FEED-->>POLL: 304, zero bytes
        POLL->>POLL: update last_checked_at only
    else 200 OK
        FEED-->>POLL: 200 body plus ETag and Last-Modified
        POLL->>POLL: parse with gofeed, upsert items keyed by guid, store both validators verbatim
        POLL->>MATCH: evaluate every enabled rule against the new items
        MATCH-->>POLL: matches, plus a reason code for every non-match
        POLL->>JOBS: insert job kind rss_download with payload item_id and rule_id
        JOBS->>ENG: handler resolves the enclosure or infohash and creates the task
        ENG-->>JOBS: tsk_01J...
    end
```

Send a weak validator `W/"..."` back exactly as received, never stripping the `W/`. Around half of real
feeds supply neither validator, so a poll that always receives 200 is normal.

### 6.4 Admission control — the concurrency limiter

Every engine keeps its own queue and none of them can see the others, so no engine can enforce a limit that
spans the stack. dl-tool is therefore the only admission controller: it holds tasks in `queued` and releases
them itself, and the engines' own queue limits are raised out of the way during the conformance check (§8.7).

| Setting | What it caps |
|---|---|
| `max_active_total` | Active tasks across every engine. |
| `max_active_per_engine` | Active tasks released to one engine. |
| `max_active_per_user` | Active tasks owned by one user; a concurrency limit, not the storage quota. |

**Seeding never counts toward any of the three.** Values, defaults and their env vars live in
[`11-config-reference.md`](11-config-reference.md); the columns live in [`04-data-model.md`](04-data-model.md).

```mermaid
sequenceDiagram
    autonumber
    participant TICK as Admission pass
    participant DB as tasks table
    participant ENG as internal/engine router
    participant E as aria2, qBittorrent or yt-dlp
    TICK->>DB: count active tasks in total, per engine and per user
    TICK->>DB: select queued candidates in process_order
    loop while every applicable limit still has headroom
        TICK->>ENG: release one candidate
        ENG->>E: add or resume
        E-->>ENG: engine_ref
    end
    TICK->>DB: leave the rest in queued, error_code concurrency_limit
    Note over TICK,DB: seeding tasks are excluded from all three counts,<br/>so a full seed list cannot starve new downloads
```

- Without dl-tool-side admission, `process_order` (by-date or by-user round-robin) is meaningless: every
  task would start at once because each engine only sees its own share of the queue.
- Concurrency exhaustion is the error code `concurrency_limit`, distinct from the storage-quota code
  `quota_exceeded`.
- A task held back by a limit stays `queued`; it is never rejected at creation time for concurrency alone.

<!-- INFERRED: the counted set is "released to an engine and not yet completed, error, paused or removed";
     the brief fixes only that seeding is excluded. -->

---

## 7. Deployment View

One Docker host, one compose project, two persistent paths. The §5.1 diagram is also the deployment
diagram; compose, profiles, PUID/PGID and NAS notes are in
[`10-deployment-and-compose.md`](10-deployment-and-compose.md).

| Node | Runs | Persistent state |
|---|---|---|
| `dl-tool` | Go binary, embedded SPA, worker pool, cron, yt-dlp subprocesses | `/config/dl-tool.db`, `/config/definitions` |
| `qbittorrent`, `aria2` | qBittorrent-nox and `aria2c --enable-rpc` | `/config/qbittorrent`, `/config/aria2` |
| `gluetun`, `caddy` (profiles) | VPN egress and TLS termination | certificate store only |

Rules that follow from D12:

- Mount one `/data`, never separate `/downloads` and `/media`: two bind mounts are two mount points, so
  `st_dev` differs, `link(2)` and `rename(2)` return `EXDEV`, and every move degrades to copy plus delete.
- At boot compare `st_dev` of the download and library roots, then actually create and unlink a test
  hardlink between them; warn in the UI on failure. The live test catches exFAT, CIFS and NFS.
- Never call `os.Rename` blindly: detect `EXDEV`, fall back to copy, fsync, rename, unlink, and report
  progress, because on a NAS a 40 GB copy takes minutes. Run every container with the same UID and GID.

---

## 8. Crosscutting Concepts

### 8.1 Task state machine

Ten canonical lowercase states, shared by the Go `TaskState` type, the `tasks.state` column and the API
`state` field.

```mermaid
stateDiagram-v2
    [*] --> queued
    queued --> downloading
    queued --> paused
    downloading --> checking
    checking --> downloading
    downloading --> paused
    paused --> downloading
    seeding --> paused
    downloading --> extracting
    downloading --> moving
    downloading --> seeding
    extracting --> moving
    moving --> completed
    moving --> seeding
    seeding --> completed
    downloading --> error
    moving --> error
    error --> queued
    completed --> removed
    removed --> [*]
    note right of removed
        Every state can reach error and removed.
        Only the common paths are drawn.
    end note
```

| From | To | Trigger |
|---|---|---|
| `queued` | `downloading` | The engine accepted the task and reports an active transfer. |
| `downloading` | `checking` | The engine reports hash checking or integrity verification. |
| `downloading` | `extracting` / `moving` / `seeding` | Payload complete: extract if enabled, else move if the destination differs, else seed. |
| `moving` / `seeding` | `completed` | The move finished and was verified, or the share limit was reached. |
| any | `paused` | User pause, or the bandwidth schedule paused the task. |
| any | `error` | Engine error, post-processing failure or guard rejection; `tasks.error_code` is set. |
| `error` | `queued` | User retry, or automatic retry while `attempts < max_attempts`. |
| any | `removed` | User delete, with or without `delete_data`. |

Engine-status normalisation tables are in [`06-download-engines.md`](06-download-engines.md); sidebar filter
semantics over these states are in [`09-web-ui-spec.md`](09-web-ui-spec.md).

### 8.2 Identifier scheme

Every identifier is a lowercase prefix, an underscore, and a ULID in Crockford base32, 26 characters — for
example `tsk_01J8ZC4Y7N3Q2R5V8W0X1Y2Z3A`.

| Prefix | Entity | Prefix | Entity |
|---|---|---|---|
| `tsk_` | task | `sch_` | search job |
| `usr_` | user | `job_` | queue job |
| `fed_` | feed | `evt_` | event |
| `rul_` | RSS rule | `tok_` | API token |
| `idx_` | indexer | | |

Engine-side identifiers — aria2 GID, qBittorrent infohash, yt-dlp job id — live in the separate `engine` and
`engine_ref` columns, never in a URL. Columns are defined in [`04-data-model.md`](04-data-model.md).

### 8.3 Error model

- Every API error is RFC 9457 `application/problem+json`, the Huma default; shapes and status codes are in
  [`05-api-contract.md`](05-api-contract.md).
- Every task failure also sets `tasks.error_code` from the canonical enum: Download Station's 26 values plus
  `ssrf_blocked`, `path_rejected`, `quota_exceeded`, `concurrency_limit`, `disk_full`, `engine_unavailable`
  and `unsupported_scheme`.
- Every state transition and job attempt writes one `task_events` row whose `code` is a stable machine
  string such as `engine.accepted` or `postprocess.extract.failed`, so the UI can translate it with i18next.
- Go errors wrap with `fmt.Errorf("...: %w", err)`; see [`14-conventions.md`](14-conventions.md).

### 8.4 Units and time

| Quantity | Representation | Applies to |
|---|---|---|
| Size | bytes, integer | DB, API, adapters, UI store |
| Rate | bytes per second, integer | DB, API, adapters, UI store |
| Timestamp | Unix milliseconds, integer | DB columns |
| Timestamp | RFC 3339 string, for example `2026-09-01T12:00:00Z` | API request and response bodies |
| Duration | seconds, integer | `eta`, `seeding_time`, in both DB and API |

Never store, transport or compute in KB/s; formatting to KiB/s or MiB/s happens in the browser only, which
deliberately fixes Download Station's split of KB/s for configuration and B/s for reporting.

### 8.5 Job queue and at-least-once semantics

The `jobs` table is the durable queue; an in-process pool claims rows with one `UPDATE ... RETURNING`
against the oldest eligible row, and `robfig/cron/v3` enqueues the periodic ones. No broker, no Redis. DDL
is in [`04-data-model.md`](04-data-model.md).

1. **At-least-once delivery.** Every handler must be idempotent, keyed off `task_id` plus `kind`. A job may
   run twice, and running twice must be harmless.
2. **Boot recovery.** On start-up run `UPDATE jobs SET state='pending', locked_at=NULL WHERE state='running';`
   so anything claimed but unfinished when the process died is retried.
3. **Backoff and terminal failure.** On failure set `run_after = now + min(600, 5 * 2^attempts)` seconds;
   once `attempts >= max_attempts` set `state='failed'` and write a `task_events` row. There is no
   dead-letter queue — the `failed` rows are the dead-letter queue.

Cron entries are code, not rows. The only DB-driven schedules are the RSS interval and the 24×7 bandwidth
grid, which the cron tick reads on each fire.

### 8.6 SSE ring buffer

| Parameter | Value | Reason |
|---|---|---|
| Ring depth | 300 deltas | About 5 minutes of history at the 1 Hz push rate. |
| Push rate | at most once per second | Bounds diff cost and client re-render cost. |
| Heartbeat | one SSE comment every 15 s | Defeats idle-timeout kills in reverse proxies. |
| `retry` | 3000 ms, sent on the first message | Client reconnect delay. |
| Reconnect key | the `Last-Event-ID` header is the `rid` | No client-side bookkeeping required. |
| Miss behaviour | `full_update: true` plus `seq_gap: true` | Client replaces the store instead of merging a partial view. |
| Fallback | `GET /api/v1/sync?rid=N` polled every 2 s after 3 stream failures | Identical envelope, identical reducer. |

The ring lives in `internal/sync/ring.go` and is memory-only; after a restart it is rebuilt from the current
task snapshot, which is exactly the `seq_gap` case every client already handles.

### 8.7 Engine conformance at boot

dl-tool assumes exclusive control of every engine it is configured with ([ADR-0017](decisions/0017-exclusive-control-of-engines.md)).
At boot, and after any engine configuration change, it asserts the settings below and, where the API allows
it, forces them. Two schedulers or two RSS engines against one feed produce irreproducible bugs.

| Engine setting | Asserted value | Reason |
|---|---|---|
| qBittorrent `rss_processing_enabled` | `false` | dl-tool owns RSS rules (D9); a second rule engine double-adds items. |
| qBittorrent `scheduler_enabled` | `false` | dl-tool owns the 24×7 bandwidth schedule and fans it out itself. |
| qBittorrent `auto_tmm_enabled` | `false` | Automatic Torrent Management silently relocates files by category and would override `tasks.destination`. |
| qBittorrent search plugins | none installed | D10: dl-tool never executes third-party code. |
| Every engine's internal queue limit | raised out of the way | dl-tool is the only admission controller (§6.4). |

A conformance failure is a visible warning carrying a "fix it for me" action. It is never a crash and never
blocks start-up. The probe also records the resolved engine, yt-dlp and JavaScript-runtime versions into
`engines.version`.

### 8.8 Foreign-task policy

`engines.foreign_task_policy` decides what happens to tasks that exist in an engine but that dl-tool did not
create — a torrent added directly in qBittorrent's own WebUI, or a queue predating installation. The
`engines` columns themselves are defined in [`04-data-model.md`](04-data-model.md).

| Value | Behaviour |
|---|---|
| `ignore` (default) | Foreign tasks never appear in the queue, the grid or an SSE delta. dl-tool leaves them running and untouched. |
| `adopt` | Foreign tasks are imported as dl-tool tasks and owned by the admin who configured that engine, preserving save path, category and tags. |

The "one queue" claim in [`01-vision-and-scope.md`](01-vision-and-scope.md) is conditional on this setting.
Bulk adoption of an existing qBittorrent is a one-time import, not a live sync; it is specified in
[`15-migration-and-import.md`](15-migration-and-import.md).

---

## 11. Risks and Technical Debt

| # | Risk | Mitigation |
|---|---|---|
| R1 | aria2 has shipped no tagged release since `1.37.0` on 2023-11-15, although `master` still receives commits, the most recent seen on 2026-06-25. Treat this as maintenance mode, not abandonment. | The `Engine` interface isolates aria2 in `internal/engine/aria2`; replacing it touches one directory plus one line of `registry.go`, and `enginetest` already defines what a replacement must satisfy. |
| R2 | yt-dlp breaks whenever a media site changes, so it needs updating far more often than the rest of the stack — but a self-updating binary is an unreviewed remote code path inside the container. | Install the standalone `yt-dlp_musllinux` binary at an exact version verified by SHA-256 at image build time; `yt-dlp -U` and every other self-update path is disabled at runtime. Freshness comes from a scheduled weekly CI job that bumps the pin and rebuilds the image. The boot probe records the resolved version in `engines.version` and `GET /system/info` reports it. See [ADR-0018](decisions/0018-pin-ytdlp-by-version-and-hash.md). |
| R3 | qBittorrent's upstream wiki is stale in two places that break adapters: `torrents/add` reads `stopped`, not the documented `paused`; and the `state` enum returns `stoppedDL`/`stoppedUP` in 5.x where the wiki says `pausedDL`/`pausedUP`. | Send both parameter names, accept both spellings on read, and always provide a `default` branch mapping unknown states to `queued` with a logged warning rather than a crash. The exact parameter list and state enum are reproduced in [`06-download-engines.md`](06-download-engines.md). |
| R4 | SQLite WAL mode does not work on a network filesystem, because WAL requires all processes to share memory and processes on separate hosts cannot. | **IMPORTANT:** `/config`, and therefore `dl-tool.db`, must be a local path or local Docker volume — never NFS or SMB. Download destinations under `/data` may be network mounts. At boot, detect `nfs`, `cifs`, `smb3` or `fuse.*` on the directory holding the database and **refuse to start** with a named error. There is no degraded fallback: `journal_mode=DELETE` on a network mount is not offered, because a silently slower and still-corruptible database is worse than a refusal the operator can read. |
| R5 | A wedged engine daemon stalls the poll loop and therefore the SSE stream for every task. | Poll each engine in its own goroutine with a per-call timeout; a failed poll marks that engine unhealthy in `/readyz` and leaves the last known snapshot in place instead of emptying the grid. |
| R6 | yt-dlp needs a JavaScript runtime for full YouTube support — its README lists deno, node.js, bun or QuickJS as strongly recommended, to run `yt-dlp-ejs`. That is a second moving dependency inside the runtime image. | The image installs Alpine's `nodejs`, because deno does not build reliably on musl; Python is never installed. The boot capability probe records the JS runtime version in `engines.version`; a missing runtime raises the `js_runtime_missing` task-event code and disables the media lane behind a visible warning rather than failing downloads silently. |

Accepted v1 debt: no Usenet engine, no ed2k engine, no in-process BitTorrent, no Litestream replication, no
all-in-one s6-overlay image — all listed as out of scope in [`01-vision-and-scope.md`](01-vision-and-scope.md).

---

## Decisions referenced

The D-list in §4 is the full map, D14 excluded. ADRs that shape this document directly:

| ADR | Decision |
|---|---|
| [ADR-0001](decisions/0001-control-plane-over-existing-engines.md) | Build a control plane over existing download engines |
| [ADR-0004](decisions/0004-sqlite-as-the-only-datastore.md) | SQLite as the only datastore |
| [ADR-0005](decisions/0005-aria2-qbittorrent-ytdlp-engines.md) | aria2, qBittorrent and yt-dlp as the v1 engines |
| [ADR-0006](decisions/0006-sse-with-rid-deltas.md) | Server-sent events with rid deltas for live updates |
| [ADR-0012](decisions/0012-single-data-mount.md) | A single `/data` mount |
| [ADR-0015](decisions/0015-db-backed-in-process-job-queue.md) | DB-backed in-process job queue |
| [ADR-0017](decisions/0017-exclusive-control-of-engines.md) | dl-tool assumes exclusive control of its engines |
| [ADR-0018](decisions/0018-pin-ytdlp-by-version-and-hash.md) | Pin yt-dlp by version and hash; never self-update at runtime |

## Open questions

- D16 (declarative-only extensibility) has no ADR of its own and is covered by ADR-0010; ADR-0016 is the
  licensing decision.
- [NEEDS CLARIFICATION: whether `extracting` and `moving` may run while a torrent is `seeding`, or whether
  seeding is suspended for their duration. The transition table assumes they are sequential.]

## Change log

| Date | Change |
|---|---|
| 2026-09-01 | Initial version |
| 2026-09-01 | Compatibility façades cut: removed D14, ADR-0014, `internal/compat`, the façade actor and its context row. Added §6.4 admission control, §8.7 engine conformance, §8.8 foreign-task policy, ADR-0017 and ADR-0018, and risk R6 (JS runtime). Hardened R2 (pinned yt-dlp) and R4 (refuse to start on a network filesystem). ADR links moved to the canonical slugs. |
