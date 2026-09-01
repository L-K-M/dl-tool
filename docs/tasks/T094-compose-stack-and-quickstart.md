# T094 — Ship the compose stack, the env template and the quickstart

| Field | Value |
|---|---|
| **ID** | T094 |
| **Milestone** | M7 |
| **Status** | todo |
| **Depends on** | T093 |
| **Blocks** | T095, T097, T115 |
| **Parallel-safe** | no — it edits `README.md` |
| **Implements** | [NFR-009](../02-requirements.md#nfr-009-collect-and-transmit-no-telemetry), [NFR-005](../02-requirements.md#nfr-005-publish-a-multi-architecture-image) |
| **Decisions** | [ADR-0012](../decisions/0012-single-data-mount.md), [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md), [ADR-0011](../decisions/0011-alpine-runtime-with-puid-pgid.md) |
| **Est. size** | 4 new files, ~330 LOC |

## Goal
`cp .env.example .env && docker compose up -d` brings up `dl-tool` and `qbittorrent`, serves the first-run
wizard on `http://<host>:8091`, and `make compose-check` validates both the base file and the dev overlay.
The README quickstart matches what the files actually do.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/10-deployment-and-compose.md` §2 `compose.yaml`](../10-deployment-and-compose.md#2-composeyaml) — the file, verbatim, and the three notes after it.
2. [`docs/10-deployment-and-compose.md` §1 Service inventory](../10-deployment-and-compose.md#1-service-inventory) — service names, profiles and which ports are published.
3. [`docs/10-deployment-and-compose.md` §6 `.env.example`](../10-deployment-and-compose.md#6-envexample) — the file, verbatim.
4. [`docs/10-deployment-and-compose.md` §3.1 The two mounts](../10-deployment-and-compose.md#31-the-two-mounts) — why `/data` is one mount at the same path everywhere.
5. [`docs/10-deployment-and-compose.md` §11.3 Unraid (Community Applications)](../10-deployment-and-compose.md#113-unraid-community-applications) — the template's required elements and defaults.

## Files
| Path | Action | Purpose |
|---|---|---|
| `compose.yaml` | create | The reference stack of doc 10 §2, verbatim. |
| `compose.dev.yaml` | create | Overlay that builds the image from source and turns on text logs. |
| `.env.example` | create | The interpolation template of doc 10 §6, verbatim. |
| `deploy/unraid/dl-tool.xml` | create | Community Applications template. |
| `README.md` | edit | Replace the placeholder quickstart with the real one; drop the planning banner's "no runnable code" claim. |

No other file may be modified.

## Interface contract

`compose.yaml` is reproduced **verbatim** from
[`docs/10-deployment-and-compose.md` §2](../10-deployment-and-compose.md#2-composeyaml): the two YAML
anchors, the five services, the `aria2`, `vpn` and `proxy` profiles, `stop_grace_period` of `60s`/`120s`/`30s`,
and no top-level `version:` key.

`compose.dev.yaml` — the only overlay, applied second:

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

`deploy/unraid/dl-tool.xml` — one `<Config>` element per port, path and variable:

```xml
<?xml version="1.0"?>
<Container version="2">
  <Name>dl-tool</Name>
  <Repository>ghcr.io/l-k-m/dl-tool:1</Repository>
  <Registry>https://ghcr.io/l-k-m/dl-tool</Registry>
  <Network>bridge</Network>
  <Privileged>false</Privileged>
  <WebUI>http://[IP]:[PORT:8080]/</WebUI>
  <Overview>Self-hosted download manager: one queue for HTTP, FTP, BitTorrent and media sites.</Overview>
  <Category>Downloaders:</Category>
  <Config Name="WebUI Port" Target="8080" Default="8091" Mode="tcp" Type="Port" Required="true">8091</Config>
  <Config Name="Config" Target="/config" Default="/mnt/user/appdata/dl-tool" Type="Path" Required="true">/mnt/user/appdata/dl-tool</Config>
  <Config Name="Data" Target="/data" Default="/mnt/user/data" Type="Path" Required="true">/mnt/user/data</Config>
  <Config Name="PUID" Target="PUID" Default="99" Type="Variable" Required="true">99</Config>
  <Config Name="PGID" Target="PGID" Default="100" Type="Variable" Required="true">100</Config>
  <Config Name="UMASK" Target="UMASK" Default="002" Type="Variable" Required="false">002</Config>
  <Config Name="TZ" Target="TZ" Default="Etc/UTC" Type="Variable" Required="false">Etc/UTC</Config>
</Container>
```

<!-- UNVERIFIED: doc 10 §11.3 records that the 99/100 convention and the exact <Config> attribute list were
     not confirmed against Unraid's schema. Validate the template in the Unraid template editor before it is
     submitted to Community Applications and paste the result under "## Evidence". -->

## Steps
1. Create `compose.yaml` by copying doc 10 §2 exactly. Do not add a `version:` key, do not publish the
   qBittorrent WebUI port, and do not publish the aria2 RPC port.
2. Confirm both engine services mount `${DATA_DIR:-/srv/data}:/data` at the identical container path, so
   hardlinks and instant moves work.
3. Keep `ARIA2_RPC_SECRET: "${ARIA2_RPC_SECRET:?set ARIA2_RPC_SECRET in .env}"` in its `:?` form, so an empty
   value fails `up` with a named error instead of starting an unauthenticated RPC endpoint.
4. Create `.env.example` by copying doc 10 §6 exactly, including the comment lines. Add no real secret value.
5. Create `compose.dev.yaml` as above; it must add keys only, so `docker compose -f compose.yaml -f
   compose.dev.yaml config -q` succeeds.
6. Create `deploy/unraid/dl-tool.xml` as above.
7. Edit `README.md`: replace the body of `## Quickstart (once it exists)` with `## Quickstart` and the real
   three-command sequence, add the `dl-tool gen-secrets` step before the first `up`, and keep the existing
   statement that there are no default credentials.
8. Edit the README status banner so it no longer claims the repository contains no runnable code, and leave
   the "On content" section — zero piracy indexers, no telemetry, no phone-home update check — untouched.
9. Start the stack, exercise the UI for five minutes with outbound traffic captured, and confirm the only
   destinations are the hosts the operator configured. Paste the capture summary under `## Evidence`.

## Acceptance criteria
- [ ] `make compose-check` exits `0` for both the base file and the base plus dev overlay.
- [ ] `docker compose config` shows `dl-tool` and `qbittorrent` with no profile, and `aria2`, `gluetun` and `caddy` behind profiles.
- [ ] Unsetting `ARIA2_RPC_SECRET` and running `COMPOSE_PROFILES=aria2 docker compose config` fails with the named error.
- [ ] Neither the qBittorrent WebUI port nor the aria2 RPC port appears in `docker compose port` output.
- [ ] `docker compose up -d` followed by opening `http://<host>:8091` shows the first-run wizard.
- [ ] A five-minute capture shows no request to any host the operator did not configure.
- [ ] `.env.example` contains no secret value, only empty assignments and comments.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
cp .env.example .env && ARIA2_RPC_SECRET=checkonly make compose-check && echo COMPOSE_OK
```
Expected: `docker compose config -q` prints nothing for both invocations, and the final line of stdout is
exactly `COMPOSE_OK`. Any warning about an obsolete `version` attribute is a failure.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file. `.env` is untracked and must not appear.

## Out of scope — do NOT
- Do NOT create or edit the `Dockerfile`, `.dockerignore` or `deploy/entrypoint.sh`; T093 owns them.
- Do NOT create `deploy/aria2/Dockerfile`; T115 owns the aria2 image.
- Do NOT create `deploy/caddy/Caddyfile.example` or `deploy/traefik/labels.md`; T095 owns them.
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
