# 11 — Configuration Reference

> **Status:** draft
> **Last reviewed:** 2026-09-01
> **Audience:** implementing agent
> **Read this before:** T005, T006, T007, T099, T106, T113

## Purpose

Define every configuration input dl-tool accepts: the `DLTOOL_` environment variables, the container-level
variables the entrypoint consumes, the compose interpolation variables, and the database-backed settings.
This file does not describe compose service wiring, volumes or ports, and does not restate the HTTP shapes
of the settings endpoints.

## Scope of this document

- In scope: variable names, types, defaults, categories, effects, read sites, secret handling, `.env`
  examples, boot validation.
- Out of scope (lives instead in): compose services, volumes, published ports →
  [`10-deployment-and-compose.md`](10-deployment-and-compose.md) · the `settings` table DDL →
  [`04-data-model.md`](04-data-model.md) · `GET`/`PATCH /settings` request and response bodies →
  [`05-api-contract.md`](05-api-contract.md) · threat rationale for secret handling →
  [`12-security-and-threat-model.md`](12-security-and-threat-model.md).

---

## 1. Precedence rule

Two categories, one rule each. Every variable in this document is labelled with its category.

| Category | Covers | Winner | Changeable at runtime |
|---|---|---|---|
| `infrastructure` | Listen addresses, base path, filesystem paths, engine URLs, engine credentials, log sinks, security switches | **Environment wins at boot.** No API can change it. | No — restart the container |
| `preference` | Speed limits, bandwidth schedule, concurrency limits, RSS interval, extraction options, default destination, watch folder, notification target | **Database wins.** The env var seeds the row on first boot only and is ignored on every later boot. | Yes — `PATCH /settings` |

```mermaid
flowchart TD
  A["dl-tool boots"] --> B{"variable category"}
  B -->|infrastructure| C["read DLTOOL_* env"]
  C --> D{"valid?"}
  D -->|no| E["log named error, exit 1"]
  D -->|yes| F["hold in config.Config for process lifetime"]
  B -->|preference| G{"settings row exists?"}
  G -->|yes| H["use DB value, ignore env"]
  G -->|no| I["seed row from env, else from default"]
```

**IMPORTANT** A `preference` env var that is changed after the first boot has no effect and is not an error;
the operator must change it through `PATCH /settings` or the Settings screen.

Parsing rules for every variable below: booleans go through `strconv.ParseBool` (`true`, `false`, `1`, `0`,
`t`, `f`); durations through `time.ParseDuration` (`720h`, `15m`); path lists split on `:`; host lists split
on `,` after trimming spaces. An empty string means "unset", never "empty value".

---

## 2. `DLTOOL_` variables (application)

All names are read exactly as written, prefix included, by `internal/config/env.go` inside
`config.Load(ctx) (*config.Config, error)`, which is called once from `cmd/dl-tool/main.go`. The `Read by`
column names the package that consumes the parsed field.

| Name | Type | Default | Required | Category | Effect | Read by |
|---|---|---|---|---|---|---|
| `DLTOOL_HTTP_ADDR` | listen addr | `:8080` | no | infrastructure | Address of the main HTTP listener serving the SPA, `/api/v1`, `/healthz` and `/readyz`. | `internal/api/server.go` |
| `DLTOOL_BASE_PATH` | url path | *(empty)* | no | infrastructure | Sub-path prefix when reverse-proxied, e.g. `/dl-tool`. Must start with `/` and must not end with `/`. Empty means the app is served at the root. | `internal/api/server.go`, `internal/api/static.go` |
| `DLTOOL_CONFIG_LOCK` | bool | `false` | no | infrastructure | When `true`, rejects API mutations of operator configuration. It cannot be changed through the API. | `internal/api/security.go` |
| `DLTOOL_CONFIG_DIR` | dir path | `/config` | no | infrastructure | Directory holding the database, `secrets.env`, `backups/`, `logs/` and `torrents/`. Must exist and be writable by the dropped user. | `internal/config`, `internal/store/db.go` |
| `DLTOOL_DATA_ROOTS` | `:`-separated dir paths | `/data` | no | infrastructure | The only directories any destination, browse, mkdir, move or delete-data operation may touch. Order matters: the first root is the fallback default destination. | `internal/fsx/safepath.go` |
| `DLTOOL_DB_PATH` | file path | `/config/dl-tool.db` | no | infrastructure | SQLite database file. Its directory must be on a local filesystem. | `internal/store/db.go` |
| `DLTOOL_LOG_LEVEL` | enum `debug\|info\|warn\|error` | `info` | no | infrastructure | Minimum `log/slog` level. | `internal/obs/log.go` |
| `DLTOOL_LOG_FORMAT` | enum `json\|text` | `json` | no | infrastructure | `json` selects `slog.NewJSONHandler`; `text` selects the `tint` handler for local development. | `internal/obs/log.go` |
| `DLTOOL_TRUSTED_PROXIES` | `,`-separated CIDRs | *(empty)* | no | infrastructure | Sources whose `X-Forwarded-For` and `X-Forwarded-Proto` are honoured. Empty means no forwarded header is trusted and the peer address is used. | `internal/api/server.go` |
| `DLTOOL_ALLOWED_HOSTS` | `,`-separated hostnames | *(empty)* | no | infrastructure | Additional `Host` names accepted by the DNS-rebinding defence ([`12-security-and-threat-model.md`](12-security-and-threat-model.md) §6.5). `localhost`, `localhost.` and literal IP addresses are always allowed and need not be listed. Empty plus a reverse proxy means the proxy's hostname must be listed or requests are rejected with `421`. | `internal/api/server.go` |
| `DLTOOL_SESSION_TTL` | duration | `720h` | no | infrastructure | Lifetime of a session cookie and its `sessions` row. | `internal/secure/session.go` |
| `DLTOOL_METRICS_ADDR` | listen addr \| `off` | `127.0.0.1:9090` | no | infrastructure | Separate listener exposing `GET /metrics`. The exact lowercase value `off` disables metrics entirely — an empty string cannot express this, because empty means "unset" and falls back to the default. | `internal/obs/metrics.go` |
| `DLTOOL_ARIA2_URL` | url | *(empty)* | no | infrastructure | aria2 JSON-RPC endpoint, e.g. `http://aria2:6800/jsonrpc`. Empty disables the aria2 lane and every HTTP/FTP/SFTP/Metalink task fails with `engine_unavailable`. | `internal/engine/aria2/client.go` |
| `DLTOOL_ARIA2_SECRET` | **secret** string | *(empty)* | yes when `DLTOOL_ARIA2_URL` is set | infrastructure | Value sent as the aria2 `token:<secret>` first parameter. | `internal/engine/aria2/client.go` |
| `DLTOOL_QBITTORRENT_URL` | url | *(empty)* | no | infrastructure | qBittorrent WebUI base URL, e.g. `http://qbittorrent:8080`. Empty disables the BitTorrent lane. | `internal/engine/qbittorrent/client.go` |
| `DLTOOL_QBITTORRENT_USERNAME` | string | *(empty)* | yes when `DLTOOL_QBITTORRENT_URL` is set | infrastructure | Username for `POST /api/v2/auth/login` against the engine. | `internal/engine/qbittorrent/client.go` |
| `DLTOOL_QBITTORRENT_PASSWORD` | **secret** string | *(empty)* | yes when `DLTOOL_QBITTORRENT_URL` is set | infrastructure | Password for the same login call. | `internal/engine/qbittorrent/client.go` |
| `DLTOOL_YTDLP_PATH` | file path | `/usr/local/bin/yt-dlp` | no | infrastructure | Standalone `yt-dlp_musllinux` binary. Probed at boot for its version; self-update is never invoked. | `internal/engine/ytdlp/runner.go` |
| `DLTOOL_JS_RUNTIME_PATH` | file path | `/usr/bin/node` | no | infrastructure | JavaScript runtime required by `yt-dlp-ejs` for full YouTube support. Missing binary raises `js_runtime_missing` and disables the media lane with a visible warning. | `internal/engine/ytdlp/runner.go` |
| `DLTOOL_SEVENZIP_PATH` | file path | `/usr/bin/7zz` | no | infrastructure | Archive extractor used by the auto-extract job handler. | `internal/jobs/handlers_extract.go` |
| `DLTOOL_SSRF_ALLOW_PRIVATE` | bool | `false` | no | infrastructure | When `true`, outbound fetches of feeds, indexers and task URIs may resolve to loopback and RFC 1918 addresses and may use ports other than 80/443. Link-local stays denied in every case — that is where the cloud metadata endpoints live. Leave `false` unless an indexer or feed lives on the same LAN. | `internal/secure/ssrf.go` |
| `DLTOOL_WATCH_DIR` | dir path | *(empty)* | no | preference | Seeds one enabled row in `watch_folders` pointing at this directory. Must be inside `DLTOOL_DATA_ROOTS`. | `internal/jobs/cron.go` |
| `DLTOOL_NOTIFY_URL` | url | *(empty)* | no | preference | Seeds one enabled `notification_channels` row of kind `webhook` with this URL. | `internal/jobs/handlers_notify.go` |

Every variable marked **secret** also accepts a `_FILE` suffixed sibling — see §6.

One deliberate absence: there is **no** variable, settings key or API field for the completion hook
([FR-105](02-requirements.md#fr-105-run-a-completion-hook-installed-by-the-operator)). The hook is an
executable the operator installs at `<DLTOOL_CONFIG_DIR>/hooks/on-complete`; its existence is the switch —
absent means off, present but not executable by `PUID:PGID` also means off, with the warning emitted by
the same per-finished-task evaluation that would run it. The check runs per finished task, so installing
the hook takes effect on the next completion without a restart. The durable invariant behind the
security claim is **path control, not payload format**: no HTTP endpoint writes any file into
`<DLTOOL_CONFIG_DIR>` — uploads land in the data roots, settings import writes only database rows, and
the only writers into the config directory are the process itself and the local `dl-tool restore` CLI,
which needs host access — so a hijacked web session cannot plant or alter the command that runs. argv,
environment, timeout and tests are owned by task [T078](tasks/T078-completion-hook.md).

`DLTOOL_ALLOWED_HOSTS` contains ASCII DNS names only: no scheme, path, port or wildcard. Internationalised
names are supplied in their ASCII Punycode form. Parsing lowercases each name and removes one trailing root
dot. Request validation removes a syntactically valid port, applies the same normalisation, and requires an
exact match. It checks the received `Host`, never `X-Forwarded-Host`. An empty list accepts only the implicit
loopback names and literal IP addresses.

`DLTOOL_CONFIG_LOCK` is environment-only. Settings export and import neither contain nor change it.

---

## 3. Container-level variables (entrypoint, not the application)

Consumed by `/entrypoint.sh` in the runtime image before `exec su-exec` hands control to the binary. The Go
process never reads them, except that `TZ` reaches it through the standard library.

| Name | Type | Default | Effect |
|---|---|---|---|
| `PUID` | integer uid | `1000` | User id the application runs as. `/config` is `chown`ed to `PUID:PGID` at start. `PUID=0` skips the privilege drop and logs a warning. |
| `PGID` | integer gid | `1000` | Group id the application runs as. |
| `TZ` | IANA zone | `Etc/UTC` | Container time zone. The 24×7 bandwidth schedule is evaluated in this zone; the UI shows it beside the grid. |
| `UMASK` | octal mask | `002` | Applied with `umask` before `exec`. `002` yields `775` directories and `664` files; `022` yields `755`/`644`. |

Files are created with mode `0666` and directories with `0777` so that `UMASK` — not a hard-coded mode —
decides the result. See [ADR-0011](decisions/0011-alpine-runtime-with-puid-pgid.md).

---

## 4. Compose-level variables (interpolation only)

Read by Docker Compose from `.env` while expanding `compose.yaml`. They never reach the application as
environment; most of them produce values for other fields, and the two `DLTOOL_*_URL` rows pass straight
through to the application variables of the same name.

| Name | Default in `compose.yaml` | Interpolated into |
|---|---|---|
| `CONFIG_DIR` | `./config` | The host side of the `/config` bind mount for dl-tool and each engine. |
| `DATA_DIR` | `/srv/data` | The host side of the single `/data` bind mount, identical in every service ([ADR-0012](decisions/0012-single-data-mount.md)). |
| `DLTOOL_PORT` | `8091` | Published host port mapped to container `8080`. |
| `DLTOOL_QBITTORRENT_URL` | *(empty — lane disabled; an empty string counts as unset, §1)* | Passes straight through to the application variable of the same name (§2): a non-empty value enables the BitTorrent lane, and `QBT_USERNAME`/`QBT_PASSWORD` must then be set or boot fails with `config_missing`. |
| `QBT_USERNAME` | *(empty)* | `DLTOOL_QBITTORRENT_USERNAME`. The same credentials must be set in qBittorrent's own WebUI; the linuxserver image does not read them. |
| `QBT_PASSWORD` | *(empty)* | Sources the `qbt_password` Compose secret, mounted at `/run/secrets/qbt_password` mode `0400` and read through `DLTOOL_QBITTORRENT_PASSWORD_FILE`. It never becomes a service environment variable. |
| `DLTOOL_ARIA2_URL` | *(empty — lane disabled)* | Passes straight through to the application variable of the same name (§2). Set together with `ARIA2_RPC_SECRET` **and** the `aria2` profile (`COMPOSE_PROFILES=aria2`): a URL with an empty secret is a fatal `config_missing` at dl-tool's boot (§8), a URL without the profile enables a lane whose backend container is not running and fails at runtime with `engine_unavailable`. |
| `ARIA2_RPC_SECRET` | *(none)* | The aria2 service's RPC secret **and** dl-tool's `DLTOOL_ARIA2_SECRET`. One value, two consumers, guarded twice: dl-tool's boot fails with `config_missing` when `DLTOOL_ARIA2_URL` is set and this is empty, and the aria2 entrypoint refuses an empty value when that profile is active. |
| `QBT_WEBUI_PORT` | `8080` | qBittorrent's in-container WebUI port. The URL is no longer derived from it — changing the port means updating `DLTOOL_QBITTORRENT_URL` to match. |
| `PUID`, `PGID`, `TZ`, `UMASK` | `1000`, `1000`, `Etc/UTC`, `002` | The dropped user, group, zone and umask of every service ([ADR-0011](decisions/0011-alpine-runtime-with-puid-pgid.md)). |
| `DLTOOL_BASE_PATH`, `DLTOOL_ALLOWED_HOSTS`, `DLTOOL_CONFIG_LOCK`, `DLTOOL_TRUSTED_PROXIES`, `DLTOOL_LOG_LEVEL`, `DLTOOL_LOG_FORMAT` | as in §2 | Each passes straight through to the application variable of the same name (§2); `compose.yaml` interpolates it so an operator can set it in `.env` rather than editing the compose file. |

The `vpn` profile's gluetun variables (`VPN_SERVICE_PROVIDER`, `VPN_TYPE`, `SERVER_COUNTRIES`,
`WIREGUARD_PRIVATE_KEY`, `WIREGUARD_ADDRESSES`, `OPENVPN_USER`, `OPENVPN_PASSWORD`,
`FIREWALL_OUTBOUND_SUBNETS`, `FIREWALL_VPN_INPUT_PORTS`, `VPN_PORT_FORWARDING`) are **not** dl-tool
configuration and never reach the dl-tool process. They ship in `.env.example` and are owned by
[`10-deployment-and-compose.md`](10-deployment-and-compose.md) §8.

---

## 5. Database-backed settings

One row per key in `settings` (`key`, `value_json`) — DDL in [`04-data-model.md`](04-data-model.md). Keys are
flat and lowercase. This table is the authoritative key list referenced by
[`05-api-contract.md`](05-api-contract.md) §11.

| Key | Type | Default | Changed by |
|---|---|---|---|
| `download_rate_limit` | integer bytes/s, `0` = unlimited | `0` | `PATCH /settings` |
| `upload_rate_limit` | integer bytes/s, `0` = unlimited | `0` | `PATCH /settings` |
| `alt_download_rate_limit` | integer bytes/s | `5242880` | `PATCH /settings` |
| `alt_upload_rate_limit` | integer bytes/s | `1048576` | `PATCH /settings` |
| `schedule_enabled` | boolean | `false` | `PATCH /settings` |
| `default_destination` | absolute path inside a data root | first entry of `DLTOOL_DATA_ROOTS` | `PATCH /settings` |
| `min_free_space` | sparse object, absolute root path → bytes | `2147483648` for every root absent from the stored map | `PATCH /settings` |
| `max_active_total` | integer, `0` = unlimited | `5` | `PATCH /settings` |
| `max_active_per_engine` | integer, `0` = unlimited | `3` | `PATCH /settings` |
| `process_order` | enum `by_date_created` | `by_date_created` | `PATCH /settings` |
| `rss_enabled` | boolean | `true` | `PATCH /settings` |
| `rss_interval_s` | integer seconds, minimum `300` | `1800` | `PATCH /settings` — the global poll interval and its 5-minute floor are owned by [`08-rss-automation.md`](08-rss-automation.md) §2.1 |
| `auto_extract` | boolean | `false` | `PATCH /settings` |
| `extract_passwords` | **secret** array of strings, encrypted at rest (§6) | `[]` | `PATCH /settings` |
| `confirm_on_delete` | boolean | `true` | `PATCH /settings` |

The initial `min_free_space` map is empty, as defined by
[`04-data-model.md` §3.2](04-data-model.md#32-configuration); consumers apply the default above only to each
missing root. An explicit `0` disables the floor for that root. Entries for roots no longer present in
`DLTOOL_DATA_ROOTS` remain stored but are ignored when reservations are built. Seeding does not count toward
any `max_active_*` limit. The 168-cell bandwidth grid is not a
settings key: it lives in its own table and is replaced through `PUT /settings/schedule`. `locale` is not a
settings key either: it is a column on the single `users` row, changed through `PATCH /account`.

---

## 6. Secrets

| Secret | Carrier | Never |
|---|---|---|
| `DLTOOL_ARIA2_SECRET` | `DLTOOL_ARIA2_SECRET_FILE`; the shipped compose mounts `/run/secrets/aria2_rpc_secret` mode `0400` | logged, returned by any endpoint, or written to a backup export |
| `DLTOOL_QBITTORRENT_PASSWORD` | `DLTOOL_QBITTORRENT_PASSWORD_FILE`; the shipped compose mounts `/run/secrets/qbt_password` mode `0400` | logged, returned by any endpoint, or written to a backup export |
| `extract_passwords` (the shared list) | `settings` row, encrypted at rest with `DLTOOL_SECRET_KEY` (see below) | returned in clear by `GET /settings` — it renders as `"__redacted__"` |
| `tasks.extract_password` (per task) | `tasks` column, encrypted at rest with `DLTOOL_SECRET_KEY` | returned by any API at all — it is absent from every Task object ([`05-api-contract.md`](05-api-contract.md) §3) |
| indexer `api_key` | `indexers.api_key_enc`, encrypted at rest | returned by `GET /indexers`, or included in a log line or an error message |
| notification channel `secret_enc`, engine `secret_enc` | `notification_channels`/`engines` rows, encrypted at rest | returned by `GET /notifications` or `/engines` |
| at-rest secret key `DLTOOL_SECRET_KEY`, `ARIA2_RPC_SECRET` | `<CONFIG_DIR>/secrets.env`, mode `0600` | exported, or exposed through any API |

**One value, two names.** `ARIA2_RPC_SECRET` (the compose-level name) and `DLTOOL_ARIA2_SECRET` (the
application variable) hold the **same** value: `compose.yaml` interpolates the one `.env` entry into the
aria2 service and into dl-tool's `DLTOOL_ARIA2_SECRET`. The `.env` entry is the single source — an
operator who copies `ARIA2_RPC_SECRET` into `.env` per step 3 below has configured dl-tool's side too.
Provisioning `DLTOOL_ARIA2_SECRET` directly (inline or `_FILE`) reaches only dl-tool, so aria2's RPC
secret must then be made to match by hand; a mismatch shows up as aria2 rejecting every request and the
engine reading unhealthy.

**At-rest encryption.** Every `*_enc` column and the extraction passwords are sealed with
`DLTOOL_SECRET_KEY`: XChaCha20-Poly1305 with a 32-byte key and a fresh random nonce per value, the nonce
stored alongside the ciphertext. The key lives only in `secrets.env` and in process memory; it is never
logged, exported or returned. Sessions and CSRF tokens do **not** use it — session ids are opaque random
values stored hashed in `sessions`, and the CSRF token is a per-session random value
([`12-security-and-threat-model.md`](12-security-and-threat-model.md) §6.1–6.2) — so no session or CSRF
key exists.

Rules:

- **`_FILE` convention.** Every variable marked **secret** in §2 also accepts `<NAME>_FILE` holding a path,
  the Docker-secrets and linuxserver.io style: `DLTOOL_ARIA2_SECRET_FILE=/run/secrets/aria2_rpc_secret`. The
  file is read once at boot and its trailing newline stripped. Setting both forms is a fatal
  `config_conflict`.
- **Never baked into an image.** No `ARG` carrying a secret (it survives in `docker history`), no
  `COPY .env`. A build-time credential, if ever unavoidable, uses BuildKit `RUN --mount=type=secret,id=…`.
- **Never logged.** Secret fields are typed `secure.Secret`, whose `String`, `Format` and `MarshalJSON`
  return `[REDACTED]`. Request logging additionally redacts `Authorization`, `Cookie`, `X-Api-Key`, and the
  query or path parameters `apikey`, `token` and `passkey`, because indexer and tracker URLs routinely embed
  a tracker passkey.
- **Never returned.** No endpoint echoes a secret. Redacted placeholders are literal `"__redacted__"`, and
  `PATCH` of a field whose submitted value equals `"__redacted__"` is a no-op on that field.

### First-run secret generation

`ARIA2_RPC_SECRET` is one value shared by two containers, so it is generated once, before the stack starts:

1. Run `dl-tool gen-secrets` (or `openssl rand -base64 32`) before the first `docker compose up`. It writes
   `<CONFIG_DIR>/secrets.env` mode `0600`, owned by `PUID:PGID`, if that file does not already exist.
2. The file contains two values, each 32 bytes from `crypto/rand`, base64url-encoded:
   `ARIA2_RPC_SECRET` and `DLTOOL_SECRET_KEY` (the at-rest encryption key above).
3. Copy `ARIA2_RPC_SECRET` into `.env` so Compose can interpolate it into both services.
4. At every boot dl-tool regenerates either value that is missing from `secrets.env`. A regenerated
   `DLTOOL_SECRET_KEY` makes every encrypted value undecryptable, and every affected surface says so: the
   boot logs one `warn` record with event code `secret_key_regenerated`, indexers, channels and engines
   get `last_error = 'secret_lost'` (a row-level error string, not a `tasks.error_code`), the
   `extract_passwords` settings row is cleared and renders as the literal `"__lost__"` on
   `GET /settings` — distinct from `"__redacted__"`, so "wiped by key loss" is tellable from "none
   configured", and `PATCH` treats it like `"__redacted__"` (a no-op) — and each affected task's
   `extract_password` is nulled with one `task_events` row. Nothing is wiped silently. A regenerated
   `ARIA2_RPC_SECRET` does **not** reconfigure the running aria2 container, so dl-tool logs
   `aria2_secret_rotated` and marks the engine unhealthy until the operator restarts it.

---

## 7. Worked `.env` files

Minimal — everything else takes its default:

```dotenv
# .env — minimal
PUID=1000
PGID=1000
TZ=Europe/Zurich
UMASK=002

CONFIG_DIR=./config
DATA_DIR=/srv/data
DLTOOL_PORT=8091

ARIA2_RPC_SECRET=Zk8s1QmF3rT9vN2xJ7bH5cW0aY4pL6dE8gU1oI3sK5M
```

Full — every knob this document defines, at its documented default unless a comment says otherwise:

```dotenv
# .env — full
# --- container ---
PUID=1000
PGID=1000
TZ=Europe/Zurich
UMASK=002

# --- compose interpolation ---
CONFIG_DIR=./config
DATA_DIR=/srv/data
DLTOOL_PORT=8091
QBT_WEBUI_PORT=8080
ARIA2_RPC_SECRET=Zk8s1QmF3rT9vN2xJ7bH5cW0aY4pL6dE8gU1oI3sK5M

# --- dl-tool: infrastructure ---
DLTOOL_HTTP_ADDR=:8080
DLTOOL_BASE_PATH=
DLTOOL_ALLOWED_HOSTS=
DLTOOL_CONFIG_LOCK=false
DLTOOL_CONFIG_DIR=/config
DLTOOL_DATA_ROOTS=/data
DLTOOL_DB_PATH=/config/dl-tool.db
DLTOOL_LOG_LEVEL=info
DLTOOL_LOG_FORMAT=json
DLTOOL_TRUSTED_PROXIES=
DLTOOL_SESSION_TTL=720h
DLTOOL_METRICS_ADDR=127.0.0.1:9090
DLTOOL_ARIA2_URL=http://aria2:6800/jsonrpc
DLTOOL_ARIA2_SECRET_FILE=/run/secrets/aria2_rpc_secret
DLTOOL_QBITTORRENT_URL=http://qbittorrent:8080
DLTOOL_QBITTORRENT_USERNAME=admin
DLTOOL_QBITTORRENT_PASSWORD_FILE=/run/secrets/qbt_password
DLTOOL_YTDLP_PATH=/usr/local/bin/yt-dlp
DLTOOL_JS_RUNTIME_PATH=/usr/bin/node
DLTOOL_SEVENZIP_PATH=/usr/bin/7zz
DLTOOL_SSRF_ALLOW_PRIVATE=false

# --- dl-tool: preference seeds, first boot only ---
DLTOOL_WATCH_DIR=/data/watch
DLTOOL_NOTIFY_URL=
```

The block above is a **fully configured** `.env`, shown for reference. It is *not* `.env.example`: that file
is owned by [`10-deployment-and-compose.md`](10-deployment-and-compose.md) §6 and ships both engine URLs
unset, so a fresh `cp .env.example .env && docker compose up -d` boots with both engine lanes disabled.
`.env`, `config/`, `secrets.env`, `*.key` and `*.pem` are in `.gitignore`.

---

## 8. Boot validation

Checked in `config.Load` and `store.Open` before the listener binds. Fatal means: log one `error` record with
`err_code` set to the name below, then `os.Exit(1)`. Warn means: log one `warn` record and continue with the
stated fallback.

| Condition | Applies to | Behaviour | `err_code` |
|---|---|---|---|
| Variable unset | any `infrastructure` variable with a default | Use the default silently. | — |
| Variable unset | `DLTOOL_ARIA2_SECRET` while `DLTOOL_ARIA2_URL` is set | Fatal. | `config_missing` |
| Variable unset | `DLTOOL_QBITTORRENT_USERNAME` or `..._PASSWORD` while `DLTOOL_QBITTORRENT_URL` is set | Fatal. | `config_missing` |
| Both `X` and `X_FILE` set | any secret | Fatal. | `config_conflict` |
| `X_FILE` path unreadable | any secret | Fatal. | `config_secret_unreadable` |
| Unparseable value | bool, duration, integer, enum, CIDR list | Fatal, naming the variable and the received value. | `config_malformed` |
| Not a valid `host:port` | `DLTOOL_HTTP_ADDR`, `DLTOOL_METRICS_ADDR` | Fatal. `DLTOOL_METRICS_ADDR` additionally accepts the literal `off`. | `config_malformed` |
| Missing leading `/`, or trailing `/` | `DLTOOL_BASE_PATH` | Fatal. | `config_malformed` |
| URL, port, wildcard, non-ASCII or invalid DNS name | `DLTOOL_ALLOWED_HOSTS` | Fatal. | `config_malformed` |
| Not an absolute path | `DLTOOL_CONFIG_DIR`, `DLTOOL_DATA_ROOTS`, `DLTOOL_DB_PATH`, `DLTOOL_WATCH_DIR` | Fatal. | `config_malformed` |
| Directory missing or not writable | `DLTOOL_CONFIG_DIR`, directory of `DLTOOL_DB_PATH` | Fatal, after attempting one `MkdirAll`. | `config_path_unwritable` |
| Directory missing or not writable | any entry of `DLTOOL_DATA_ROOTS` | Warn. Boot continues so the UI can show the problem — a NAS data mount may simply be late — and operations touching that root fail per request with `path_rejected` until it appears. Writability is re-probed after every settings change and lazily on each operation, so recovery needs no restart. The root is never `MkdirAll`-ed: creating the directory would silently mask an unmounted volume. | `data_root_not_writable` |
| Filesystem of the database directory is `nfs`, `cifs`, `smb3` or `fuse.*` | `DLTOOL_DB_PATH` | Fatal. SQLite WAL requires shared memory, which network filesystems do not provide; there is no degraded fallback. | `config_network_fs` |
| `PRAGMA integrity_check` does not return exactly one row equal to `ok` | `DLTOOL_DB_PATH` | Fatal, naming the database path. | `integrity_check_failed` |
| Binary absent or not executable | `DLTOOL_YTDLP_PATH`, `DLTOOL_SEVENZIP_PATH` | Warn. Disable the media lane and auto-extract respectively, and surface the reason in `GET /system/info`. | `binary_missing` |
| Binary absent or not executable | `DLTOOL_JS_RUNTIME_PATH` | Warn. Disable the media lane and raise the `js_runtime_missing` task-event code. | `js_runtime_missing` |
| Engine URL unreachable at boot | `DLTOOL_ARIA2_URL`, `DLTOOL_QBITTORRENT_URL` | Warn. Mark the engine disconnected, retry in the background, fail affected task creations with `engine_unavailable`. | `engine_unreachable` |
| `DLTOOL_WATCH_DIR` outside every data root | seed value | Warn. Skip seeding the watch folder. | `config_out_of_root` |
| Malformed or out-of-range value | any `preference` seed | Warn. Use the documented default from §5. | `config_malformed` |
| Preference env var changed after first boot | any `preference` variable | Ignore silently; the database value stands. | — |

`config.Load` performs no network I/O; engine reachability is probed after the listener is up.

---

## Decisions referenced

| ADR | Decision |
|---|---|
| [ADR-0004](decisions/0004-sqlite-as-the-only-datastore.md) | SQLite as the only datastore |
| [ADR-0011](decisions/0011-alpine-runtime-with-puid-pgid.md) | Alpine runtime image with PUID/PGID privilege drop |
| [ADR-0012](decisions/0012-single-data-mount.md) | A single `/data` mount |
| [ADR-0018](decisions/0018-pin-ytdlp-by-version-and-hash.md) | Pin yt-dlp by version and hash; never self-update at runtime |

## Open questions

- `DLTOOL_JS_RUNTIME_PATH` is not in the brief's canonical variable list; it is added because §15.2 requires a
  JavaScript runtime probe for yt-dlp and the probe needs a configurable path.
- <!-- UNVERIFIED: the Alpine `7zip` package installing the binary as `/usr/bin/7zz` was not verified against
  the package manifest; the brief pins this default and T004 confirms it at build time. -->

## Change log

| Date | Change |
|---|---|
| 2026-09-01 | Initial version |
| 2026-09-01 | Consistency review: removed the withdrawn ADR-0014 row and the two remaining façade references (the `preference` category description and `DLTOOL_HTTP_ADDR`); corrected the ADR-0011, ADR-0012 and ADR-0018 links to the canonical filenames. |
| 2026-09-01 | §2 now records the completion hook's deliberate absence from every configuration surface: the fixed path `<DLTOOL_CONFIG_DIR>/hooks/on-complete` is the switch (FR-105, T078). |
| 2026-09-01 | Secrets corrected: `DLTOOL_SESSION_KEY`/`DLTOOL_CSRF_KEY` removed (opaque server-side sessions and per-session CSRF tokens need no key) and replaced by `DLTOOL_SECRET_KEY`, the previously unspecified key behind every "encrypted at rest" `*_enc` column and the extraction passwords; its loss-and-regeneration behaviour is specified. |
| 2026-09-01 | Review pass: the `DLTOOL_SECRET_KEY` loss story names every affected surface (row-level `secret_lost` markers — not a `tasks.error_code` — plus the cleared `extract_passwords` setting and one `task_events` row per nulled per-task password); the two extraction-password carriers are split into their own rows; NFR-023 names each secret's file and mode. |
| 2026-09-01 | Review pass 2: the `secret_key_regenerated` boot log is restored (the aria2 branch's `aria2_secret_rotated` keeps its twin), and a key-lost `extract_passwords` renders as the literal `"__lost__"` — distinct from `"__redacted__"` and a no-op on PATCH — so the wipe is assertable, not vague. |
| 2026-09-01 | Review pass 4: the `ARIA2_RPC_SECRET` ↔ `DLTOOL_ARIA2_SECRET` equivalence is stated once, explicitly — one `.env` value, interpolated by Compose into both consumers — instead of leaving two names for one secret across the table and step 3. |
| 2026-09-01 | Review pass 5: the equivalence names the `.env` entry as the single source — a directly set `DLTOOL_ARIA2_SECRET` (inline or `_FILE`) reaches only dl-tool and must be matched by hand on the aria2 side. |
| 2026-09-01 | Contradiction fix: `rss_interval_s` default 1800 and minimum 300, matching `08-rss-automation.md` §2.1 (30-minute default, 5-minute floor), which owns the value. |
| 2026-09-01 | §4 gains the engine-lane interpolation variables (`DLTOOL_QBITTORRENT_URL`, `QBT_USERNAME`, `QBT_PASSWORD`, `DLTOOL_ARIA2_URL`); all default to empty, matching the compose fix in [`10-deployment-and-compose.md`](10-deployment-and-compose.md) §2 — an empty URL disables the lane instead of making §8's missing-credential check fatal on a fresh boot. |
| 2026-09-01 | Review pass: the `ARIA2_RPC_SECRET` and `QBT_WEBUI_PORT` rows were rewritten too (single-source note; the URL is no longer port-derived), and the two `DLTOOL_*_URL` rows read as pass-through application config, not producers of other fields. |
| 2026-09-01 | Review pass: the aria2 rows state both guards — the fatal `config_missing` boot check when the URL is set and the secret is empty, and the entrypoint refusal under the profile — matching the qBittorrent row's failure model and doc 10's "guarded twice" paragraph. |
| 2026-09-01 | Added `DLTOOL_ALLOWED_HOSTS`, the previously unspecified knob behind the Host-allowlist defence in `12-security-and-threat-model.md` §6.5. |
| 2026-09-01 | Security review: added the Host allowlist and environment-only configuration lock; made the trusted-proxy example deny forwarded headers by default. |
| 2026-09-02 | Multi-user model dropped ([ADR-0019](decisions/0019-single-account-no-ownership.md)). |
| 2026-09-02 | Engine credentials are wired as Compose named secrets rather than service environment variables; the `_FILE` convention is now what the shipped compose uses, not a manual alternative. |
| 2026-09-02 | Single-account cleanup: dropped `by_user_round_robin` from the `process_order` enum and repaired the sentence that had merged the per-user default destination into the `locale` note ([ADR-0019](decisions/0019-single-account-no-ownership.md)). |
| 2026-09-02 | Review pass: removed the duplicated `DLTOOL_ALLOWED_HOSTS` row; `DLTOOL_CONFIG_LOCK` now names `internal/api/security.go`, the file a task actually creates; §4 gained the six `DLTOOL_*` pass-throughs and the note that the gluetun variables are not dl-tool configuration; the reference `.env` block is marked as not being the shipped `.env.example`, which ships both engine lanes disabled. |
| 2026-09-02 | Defined `min_free_space` as a sparse map whose missing roots use the 2 GiB default, whose explicit zero disables a root's floor and whose unconfigured roots remain stored but inactive, matching the host-independent initial seed. |
| 2026-09-02 | Defined `integrity_check_failed` for a fatal boot integrity result. |
