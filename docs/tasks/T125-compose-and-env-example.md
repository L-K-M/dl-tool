# T125 — Write `compose.yaml` and `.env.example`

| Field | Value |
|---|---|
| **ID** | T125 |
| **Milestone** | M0 |
| **Status** | todo |
| **Depends on** | T124 |
| **Blocks** | T094 |
| **Parallel-safe** | yes — creates three root-level deployment files |
| **Implements** | — (foundation: the M0 `docker compose up -d` checkpoint; `stop_grace_period` gives [NFR-004](../02-requirements.md#nfr-004-shut-down-gracefully-on-sigterm) its 60 s window) |
| **Decisions** | [ADR-0012](../decisions/0012-single-data-mount.md), [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md), [ADR-0011](../decisions/0011-alpine-runtime-with-puid-pgid.md) |
| **Est. size** | 3 new files, ~150 LOC |

## Goal
`cp .env.example .env && docker compose -f compose.yaml -f compose.dev.yaml up -d --build` starts `dl-tool`
and `qbittorrent`, publishes the app on `${DLTOOL_PORT:-8091}`, and `curl -s localhost:8091/healthz` answers
`{"status":"ok"}`. `make compose-check` validates the base file and the dev overlay.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/10-deployment-and-compose.md` §2 `compose.yaml`](../10-deployment-and-compose.md#2-composeyaml) — the file and the three notes after it.
2. [`docs/10-deployment-and-compose.md` §1 Service inventory](../10-deployment-and-compose.md#1-service-inventory) — service names, profiles, and which ports are published.
3. [`docs/10-deployment-and-compose.md` §6 `.env.example`](../10-deployment-and-compose.md#6-envexample) — the interpolation template.
4. [`docs/10-deployment-and-compose.md` §3.1 The two mounts](../10-deployment-and-compose.md#31-the-two-mounts) — why `/data` is one mount at the same path everywhere.
5. [`docs/11-config-reference.md` §4 Compose-level variables](../11-config-reference.md#4-compose-level-variables-interpolation-only) — the five interpolated names and where each lands.

## Files
| Path | Action | Purpose |
|---|---|---|
| `compose.yaml` | create | The reference stack: `dl-tool`, `qbittorrent`, and `aria2` behind its profile. |
| `compose.dev.yaml` | create | Overlay that builds the image from source and turns on text logs; `make compose-check` validates it too. |
| `.env.example` | create | The interpolation template of doc 10 §6, without the `vpn` block. |

No other file may be modified.

## Interface contract

`compose.yaml` — doc 10 §2 with the `gluetun` and `caddy` services omitted (T094 adds them):

```yaml
# compose.yaml — dl-tool reference stack. `docker compose up -d` starts dl-tool and
# qbittorrent; COMPOSE_PROFILES=aria2 adds the HTTP/FTP engine.

x-common-env: &common-env
  PUID: "${PUID:-1000}"
  PGID: "${PGID:-1000}"
  TZ: "${TZ:-Etc/UTC}"
  UMASK: "${UMASK:-002}"

x-service-defaults: &service-defaults
  restart: unless-stopped
  security_opt:
    - no-new-privileges:true

services:

  dl-tool:
    <<: *service-defaults
    image: ghcr.io/l-k-m/dl-tool:1
    container_name: dl-tool
    environment:
      <<: *common-env
      DLTOOL_HTTP_ADDR: ":8080"
      DLTOOL_CONFIG_DIR: "/config"
      DLTOOL_DATA_ROOTS: "/data"
      DLTOOL_DB_PATH: "/config/dl-tool.db"
      DLTOOL_BASE_PATH: "${DLTOOL_BASE_PATH:-}"
      DLTOOL_TRUSTED_PROXIES: "${DLTOOL_TRUSTED_PROXIES:-}"
      DLTOOL_LOG_LEVEL: "${DLTOOL_LOG_LEVEL:-info}"
      DLTOOL_LOG_FORMAT: "${DLTOOL_LOG_FORMAT:-json}"
      # An engine lane is enabled by setting its URL; leave the URL empty to
      # disable the lane (11-config-reference.md §8 makes a set URL with
      # missing credentials a fatal config_missing).
      DLTOOL_QBITTORRENT_URL: "${DLTOOL_QBITTORRENT_URL:-}"
      DLTOOL_QBITTORRENT_USERNAME: "${QBT_USERNAME:-}"
      DLTOOL_QBITTORRENT_PASSWORD: "${QBT_PASSWORD:-}"
      DLTOOL_ARIA2_URL: "${DLTOOL_ARIA2_URL:-}"
      DLTOOL_ARIA2_SECRET: "${ARIA2_RPC_SECRET:-}"
    volumes:
      - ${CONFIG_DIR:-./config}/dl-tool:/config
      - ${DATA_DIR:-/srv/data}:/data          # ONE mount — see doc 10 section 3
    ports:
      - "${DLTOOL_PORT:-8091}:8080"
    healthcheck:
      test: ["CMD", "/usr/local/bin/dl-tool", "healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 20s
    stop_grace_period: 60s
    depends_on:
      qbittorrent:
        condition: service_started

  qbittorrent:
    <<: *service-defaults
    image: lscr.io/linuxserver/qbittorrent:latest   # pin at implementation time
    container_name: qbittorrent
    environment:
      <<: *common-env
      WEBUI_PORT: "${QBT_WEBUI_PORT:-8080}"
      TORRENTING_PORT: "6881"
    volumes:
      - ${CONFIG_DIR:-./config}/qbittorrent:/config
      - ${DATA_DIR:-/srv/data}:/data          # same host path, same container path
    ports:
      - "6881:6881"
      - "6881:6881/udp"
      # WebUI is deliberately NOT published: only dl-tool talks to it.
    ulimits:
      nofile:
        soft: 65535
        hard: 65535
    stop_grace_period: 120s

  aria2:
    <<: *service-defaults
    profiles: ["aria2"]
    image: ghcr.io/l-k-m/dl-tool-aria2:1
    build:
      context: ./deploy/aria2
      dockerfile: Dockerfile
    container_name: aria2
    environment:
      <<: *common-env
      # Not the `:?` form: Compose resolves required variables for every
      # service before profile filtering, so `:?` breaks a core-only `up`.
      # The entrypoint refuses an empty secret when this profile is active.
      ARIA2_RPC_SECRET: "${ARIA2_RPC_SECRET:-}"
    volumes:
      - ${CONFIG_DIR:-./config}/aria2:/config
      - ${DATA_DIR:-/srv/data}:/data
    healthcheck:
      # Liveness only, no token: credentials are checked by POST /engines/{id}/test.
      test: ["CMD-SHELL", "curl -fsS -X POST -d '{\"jsonrpc\":\"2.0\",\"id\":\"hc\",\"method\":\"aria2.getVersion\",\"params\":[]}' http://127.0.0.1:6800/jsonrpc >/dev/null"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
    stop_grace_period: 30s
```

`compose.dev.yaml` — the only overlay, applied second, and the one `make compose-check` validates:

```yaml
# compose.dev.yaml — developer overlay
#   docker compose -f compose.yaml -f compose.dev.yaml up -d --build
services:
  dl-tool:
    image: ghcr.io/l-k-m/dl-tool:dev
    build:
      context: .
      dockerfile: Dockerfile
      args:
        VERSION: dev
    environment:
      DLTOOL_LOG_LEVEL: debug
      DLTOOL_LOG_FORMAT: text
      DLTOOL_METRICS_ADDR: "0.0.0.0:9090"
    ports:
      - "127.0.0.1:9090:9090"
```

`.env.example`:

```dotenv
# ---- identity and permissions (container-level, handled by the entrypoint) ----
PUID=1000
PGID=1000
TZ=Etc/UTC
UMASK=002

# ---- host paths and published port (compose-level) ----
# CONFIG_DIR gets one subdirectory per service. DATA_DIR is ONE filesystem.
# 8091 avoids DSM's 5000/5001 and the crowded 8080.
CONFIG_DIR=./config
DATA_DIR=/srv/data
DLTOOL_PORT=8091

# ---- engines (compose-level) ----
# An engine lane is disabled until its URL is set; a set URL without its
# credentials is a fatal config_missing at boot. For qBittorrent, also set the
# same username/password in its own WebUI — the image does not read these vars.
#   DLTOOL_QBITTORRENT_URL=http://qbittorrent:8080
QBT_USERNAME=admin
QBT_PASSWORD=
# Required when the aria2 profile is active. Generate: openssl rand -hex 32
ARIA2_RPC_SECRET=
#   DLTOOL_ARIA2_URL=http://aria2:6800/jsonrpc
QBT_WEBUI_PORT=8080

# ---- app settings surfaced through compose ----
# Leave DLTOOL_BASE_PATH empty for a subdomain; set /dl-tool to serve under a subfolder.
DLTOOL_BASE_PATH=
DLTOOL_TRUSTED_PROXIES=
DLTOOL_LOG_LEVEL=info
DLTOOL_LOG_FORMAT=json
```

## Steps
1. Create `compose.yaml` with the three services above. Add no top-level `version:` key: Compose V2 warns
   that the attribute is obsolete.
2. Publish only `${DLTOOL_PORT:-8091}:8080` and qBittorrent's `6881` tcp and udp. The qBittorrent WebUI and
   the aria2 RPC port stay unpublished; only `dl-tool` reaches them, over the compose network.
3. Bind `${DATA_DIR:-/srv/data}:/data` in every service at that identical container path, so `rename(2)`
   stays atomic and hardlinks keep working.
4. Keep `ARIA2_RPC_SECRET: "${ARIA2_RPC_SECRET:-}"` in the aria2 service. Do **not** use the `:?` form:
   Compose resolves required variables for every service in the file before profile filtering, so `:?`
   would fail even a core-only `docker compose up -d` with a fresh `.env`. The empty-secret refusal lives
   in the aria2 entrypoint ([`docs/10-deployment-and-compose.md` §5.1](../10-deployment-and-compose.md#51-deployaria2dockerfile)),
   which exits before `aria2c` starts an unauthenticated RPC endpoint.
5. Leave `aria2` behind `profiles: ["aria2"]` and `dl-tool` profile-less, with `depends_on` naming only
   `qbittorrent`: Compose refuses a dependency that sits behind a profile the dependant is not in.
6. Keep the `aria2` `build:` stanza pointing at `deploy/aria2`, which T115 creates; `docker compose config`
   does not require the context to exist. Do not run `docker compose build aria2` here.
7. Create `compose.dev.yaml` as above; it may add keys only, so `docker compose -f compose.yaml -f
   compose.dev.yaml config -q` succeeds. Create `.env.example` as above, with no real secret.
8. Start the stack with the dev overlay and confirm `http://<host>:8091/healthz` answers before any release
   image exists.

## Acceptance criteria
- [ ] `make compose-check` exits `0` for the base file and for the base plus the dev overlay.
- [ ] `docker compose config` emits no `version` warning, shows `dl-tool` and `qbittorrent` with no profile and `aria2` under `aria2`, and succeeds with `ARIA2_RPC_SECRET` unset or empty (the entrypoint, not Compose, refuses the empty secret when the profile is active).
- [ ] `docker compose config` lists exactly three published host ports: `8091`, `6881/tcp` and `6881/udp`.
- [ ] `dl-tool` and `qbittorrent` both bind `${DATA_DIR}` to `/data`, and no service binds a second data path.
- [ ] `stop_grace_period` is `60s`, `120s` and `30s`, and all three services carry `no-new-privileges:true`.
- [ ] `.env.example` contains no secret value, and `compose.yaml` has no `gluetun`, `caddy` or Postgres service.
- [ ] Both `DLTOOL_*_URL` engine variables interpolate from `.env` with an empty default; with a fresh
      `.env` the dl-tool container boots with both engine lanes disabled instead of exiting `config_missing`.

  The runtime counterpart is not a T125 criterion: an empty `ARIA2_RPC_SECRET` with the `aria2` profile
  active must exit non-zero before `aria2c` starts, and that proof is owned by
  [T115](T115-aria2-image-build-and-publish.md)'s "unset secret exits non-zero" criterion — T125 ships no
  aria2 image (`deploy/aria2/` does not exist yet) and owns only the config-level behaviour above.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
cp .env.example .env && make compose-check && echo COMPOSE_OK
```
Expected: both `docker compose config -q` invocations print nothing at all, then a final line of exactly
`COMPOSE_OK`. A `version` obsolete warning, a `variable is not set` message or an empty-value `:?` failure
is a failure: the fresh `.env` must start the core stack without extra variables. No registry access is
needed: `config` never pulls an image.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, and nothing else; `.env` is ignored by `.gitignore` and must
not appear. Use `git status`, not `git diff`: `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add the `gluetun` or `caddy` services, the `vpn` or `proxy` profiles, or their `.env` block; T094
  owns them when it hardens this stack for release.
- Do NOT create `deploy/unraid/dl-tool.xml` or edit `README.md` (T094), and do not create
  `deploy/caddy/Caddyfile.example`, `deploy/traefik/labels.md` (T095) or `deploy/aria2/Dockerfile` (T115).
- Do NOT create or edit `Dockerfile`, `.dockerignore` or `deploy/entrypoint.sh`; T124 owns them.
- Do NOT add a Postgres service or profile: SQLite is the only datastore.
- Do NOT commit `.env`, and do not put a real secret into `.env.example`.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
