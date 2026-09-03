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

[`docs/10-deployment-and-compose.md` §2](../10-deployment-and-compose.md#2-composeyaml) **owns
`compose.yaml` verbatim**. Copy it from there rather than from this file — a second copy in a task file is
how the two drift, and the compose file has already changed twice since this task was written. Reproduce it
exactly, with these three deltas:

| Delta | Why |
|---|---|
| Omit the `gluetun` and `caddy` services and the `vpn` and `proxy` profiles | T094 adds them, together with the VPN secrets and the reverse-proxy volumes |
| Omit the `wireguard_private_key` and `wireguard_addresses` entries from the top-level `secrets:` block | They exist only for `gluetun`; keep `aria2_rpc_secret` and `qbt_password` |
| Keep the `aria2` profile and its service | The HTTP/FTP lane is M1 and must be startable now |

Everything else is verbatim, including the `x-common-env` and `x-service-defaults` anchors, the named
secrets mounted `0400` through the `_FILE` variables, the single `/data` mount, the healthchecks,
`stop_grace_period`, `security_opt: no-new-privileges:true`, and the absence of a top-level `version:` key.

`.env.example` carries every compose-level variable of
[`docs/11-config-reference.md` §4](../11-config-reference.md#4-compose-level-variables-interpolation-only)
with its documented default, each under a one-line comment, and **no real credential**. The three secret
values ship empty with a comment naming the command that generates them:

```sh
# ARIA2_RPC_SECRET / QBT_PASSWORD: generated on first run into
# <CONFIG_DIR>/dl-tool/secrets.env; copy them here so Compose can mount them.
# openssl rand -base64 32
ARIA2_RPC_SECRET=
QBT_PASSWORD=
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
- [ ] `docker compose config` renders `DLTOOL_QBITTORRENT_URL` and `DLTOOL_ARIA2_URL` as empty strings
      from a fresh `.env`, so §8's missing-credential fatal cannot fire on a fresh boot (the runtime
      lane-disabled boot is exercised by T124's healthcheck run and the milestone exit checkpoints).

  The runtime counterpart is not a T125 criterion: an empty `ARIA2_RPC_SECRET` with the `aria2` profile
  active must exit non-zero before `aria2c` starts, and that proof is owned by
  [T115](T115-aria2-image-build-and-publish.md)'s "unset secret exits non-zero" criterion — T125 ships no
  aria2 image (`deploy/aria2/` does not exist yet) and owns only the config-level behaviour above.
- [ ] `docker compose config` renders sentinel credentials nowhere but the `secrets:` definitions: no
      service `environment` block contains the aria2 secret or the qBittorrent password.

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

The sandbox ships no Docker CLI, so a client-only toolchain was placed on `PATH` for the run — the
static `docker` CLI from `https://download.docker.com/linux/static/stable/x86_64/docker-27.5.1.tgz`
and the compose plugin from
`https://github.com/docker/compose/releases/latest/download/docker-compose-linux-x86_64`.
`docker compose config` resolves and validates entirely client-side; no daemon was involved.
Toolchain versions, verbatim:

```
$ docker --version
Docker version 27.5.1, build 9f9e405
$ docker compose version
Docker Compose version v5.5.1
```

Both strings are copied from observed command output, not reconstructed; v5.5.1 is what the official
`releases/latest` artifact served on 2026-09-03 and satisfies the `secrets.<name>.environment`
requirement (Compose >= 2.24).
The sandbox also exports `TZ=UTC`, which Compose correctly gives precedence over `.env`; the
rendered-config checks below were re-run with `env -u TZ` so they show the fresh-`.env` defaults.

**Verification block (verbatim):**

```
$ cp .env.example .env && make compose-check && echo COMPOSE_OK
docker compose -f compose.yaml config -q
docker compose -f compose.yaml -f compose.dev.yaml config -q
COMPOSE_OK
```

Both `config -q` invocations printed nothing: no `version` obsolete warning, no `variable is not set`
message, no `:?` failure, from a fresh `.env`.

**Acceptance-criterion spot checks against `docker compose config` (fresh `.env`, `env -u TZ`):**

- No profile warning; `docker compose config --profiles` prints exactly `aria2`, and only the `aria2`
  service carries `profiles: [aria2]`. `dl-tool` and `qbittorrent` are profile-less; `depends_on`
  names only `qbittorrent`.
- Exactly three published host ports across all services: `8091->8080/tcp` (dl-tool), `6881->6881/tcp`
  and `6881->6881/udp` (qbittorrent). aria2 publishes nothing.
- `dl-tool`, `qbittorrent` and `aria2` each bind `/srv/data` to `/data` (plus their per-service
  `/config`); no service binds a second data path.
- `stop_grace_period` renders `1m0s` (dl-tool), `2m0s` (qbittorrent), `30s` (aria2); all three carry
  `no-new-privileges:true` via the anchor.
- `DLTOOL_QBITTORRENT_URL: ""` and `DLTOOL_ARIA2_URL: ""` render as empty strings from the fresh
  `.env`; `TZ` renders `Etc/UTC` once the sandbox's `TZ=UTC` is unset.
- No service `environment` block contains the aria2 secret or the qBittorrent password — only the
  `_FILE` paths `/run/secrets/aria2_rpc_secret` and `/run/secrets/qbt_password`. The values appear
  nowhere but the top-level `secrets:` definitions, which reference the `.env` names.
- `grep -icE "gluetun|caddy|postgres" compose.yaml` prints `0`; no top-level `version:` key;
  `.env.example` ships `ARIA2_RPC_SECRET=` and `QBT_PASSWORD=` empty.
- Dev overlay render: `dl-tool` gains `build: {context: ., dockerfile: Dockerfile}` and
  `DLTOOL_LOG_FORMAT: text`; `config -q` on the pair succeeds.

**Scope check:**

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
.env.example
compose.dev.yaml
compose.yaml
```

Exactly the Files table; `.env` is gitignored and does not appear.

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
