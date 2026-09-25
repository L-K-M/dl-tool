# T094 — Harden the compose stack and document release verification

| Field | Value |
|---|---|
| **ID** | T094 |
| **Milestone** | M7 |
| **Status** | todo |
| **Depends on** | T093, T125 |
| **Blocks** | T095, T097, T115 |
| **Parallel-safe** | no — it edits `compose.yaml`, `.env.example` and `README.md` |
| **Implements** | [NFR-009](../02-requirements.md#nfr-009-collect-and-transmit-no-telemetry) |
| **Decisions** | [ADR-0012](../decisions/0012-single-data-mount.md), [ADR-0011](../decisions/0011-alpine-runtime-with-puid-pgid.md) |
| **Est. size** | 1 new file, 3 modified, ~210 net new lines |

## Goal
The three-service stack T125 wrote gains the two optional profiles of doc 10 §1 — `vpn` (gluetun) and
`proxy` (caddy) — and the `.env.example` block they need. `deploy/unraid/dl-tool.xml` makes the image
installable from Community Applications, and the README quickstart carries the `cosign verify` and SBOM
inspection commands an operator runs before trusting a published image.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/10-deployment-and-compose.md` §2 `compose.yaml`](../10-deployment-and-compose.md#2-composeyaml) — the `gluetun` and `caddy` services, verbatim, and the three notes after the file.
2. [`docs/10-deployment-and-compose.md` §1 Service inventory](../10-deployment-and-compose.md#1-service-inventory) — which service sits behind which profile and which ports are published.
3. [`docs/10-deployment-and-compose.md` §8 VPN profile (`vpn`)](../10-deployment-and-compose.md#8-vpn-profile-vpn) and [§8.1 Attaching the engines](../10-deployment-and-compose.md#81-attaching-the-engines) — the killswitch, and why the engines join gluetun's network namespace.
4. [`docs/10-deployment-and-compose.md` §6 `.env.example`](../10-deployment-and-compose.md#6-envexample) — the `vpn profile only` block to append.
5. [`docs/10-deployment-and-compose.md` §10 Release workflow](../10-deployment-and-compose.md#10-release-workflow) — the `cosign verify` command and what `provenance: mode=max` and `sbom: true` attach.
6. [`docs/10-deployment-and-compose.md` §11.3 Unraid (Community Applications)](../10-deployment-and-compose.md#113-unraid-community-applications) — the template's required elements and defaults.

## Files
| Path | Action | Purpose |
|---|---|---|
| `compose.yaml` | modify | Append the `gluetun` (`vpn`) and `caddy` (`proxy`) services of doc 10 §2, and the `wireguard_private_key`/`wireguard_addresses` entries of §2's top-level `secrets:` block. |
| `.env.example` | modify | Append the `vpn profile only` block of doc 10 §6. |
| `deploy/unraid/dl-tool.xml` | create | Community Applications template. |
| `README.md` | modify | Replace the placeholder quickstart, drop the "no runnable code" claim, add the release-verification commands. |

No other file may be modified.

## Interface contract

The two services appended to `compose.yaml`, verbatim from
[`docs/10-deployment-and-compose.md` §2](../10-deployment-and-compose.md#2-composeyaml):

```yaml
  gluetun:
    <<: *service-defaults
    profiles: ["vpn"]
    image: qmcgaw/gluetun                          # pin at implementation time
    container_name: gluetun
    cap_add: [NET_ADMIN]
    devices: ["/dev/net/tun:/dev/net/tun"]
    # No `env_file: [.env]`: that hands the VPN container every value in the file,
    # including the qBittorrent password and the aria2 RPC secret it has no use for.
    # Name the VPN variables explicitly and mount the two credentials as secrets.
    environment:
      TZ: "${TZ:-Etc/UTC}"
      # No `:?` here: Compose interpolates every service before it filters by profile, so a
      # required variable in an inactive profile still fails a core-only `up` from a fresh `.env`
      # — the same trap §2 avoids for the aria2 secret. gluetun refuses to start without a
      # provider anyway, and only under the `vpn` profile.
      VPN_SERVICE_PROVIDER: "${VPN_SERVICE_PROVIDER:-}"
      VPN_TYPE: "${VPN_TYPE:-wireguard}"
      SERVER_COUNTRIES: "${SERVER_COUNTRIES:-}"
      OPENVPN_USER: "${OPENVPN_USER:-}"
      OPENVPN_PASSWORD: "${OPENVPN_PASSWORD:-}"
      FIREWALL_OUTBOUND_SUBNETS: "${FIREWALL_OUTBOUND_SUBNETS:-}"
      FIREWALL_VPN_INPUT_PORTS: "${FIREWALL_VPN_INPUT_PORTS:-}"
      VPN_PORT_FORWARDING: "${VPN_PORT_FORWARDING:-off}"
      WIREGUARD_PRIVATE_KEY_SECRETFILE: "/run/secrets/wireguard_private_key"
      WIREGUARD_ADDRESSES_SECRETFILE: "/run/secrets/wireguard_addresses"
    secrets:
      - source: wireguard_private_key
        target: wireguard_private_key
        mode: 0400
      - source: wireguard_addresses
        target: wireguard_addresses
        mode: 0400
    volumes:
      - ${CONFIG_DIR:-./config}/gluetun:/gluetun
    ports:
      - "6881:6881"
      - "6881:6881/udp"

  caddy:
    <<: *service-defaults
    profiles: ["proxy"]
    image: caddy:2
    container_name: caddy
    ports:
      - "80:80"
      - "443:443"
      - "443:443/udp"
    volumes:
      - ./deploy/caddy/Caddyfile.example:/etc/caddy/Caddyfile:ro
      - ${CONFIG_DIR:-./config}/caddy/data:/data
      - ${CONFIG_DIR:-./config}/caddy/config:/config
```

The `gluetun` service mounts two Compose named secrets; append their entries to the existing
top-level `secrets:` block (T125 wrote `aria2_rpc_secret` and `qbt_password` there — change nothing
it wrote):

```yaml
  wireguard_private_key:
    environment: "WIREGUARD_PRIVATE_KEY"
  wireguard_addresses:
    environment: "WIREGUARD_ADDRESSES"
```

The block appended to `.env.example`:

```dotenv
# ---- vpn profile only (doc 10 section 8) ----
VPN_SERVICE_PROVIDER=
VPN_TYPE=wireguard
WIREGUARD_PRIVATE_KEY=
WIREGUARD_ADDRESSES=
SERVER_COUNTRIES=
OPENVPN_USER=
OPENVPN_PASSWORD=
FIREWALL_OUTBOUND_SUBNETS=
FIREWALL_VPN_INPUT_PORTS=
VPN_PORT_FORWARDING=off
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

The verification commands the README gains, from
[`docs/10-deployment-and-compose.md` §10](../10-deployment-and-compose.md#10-release-workflow):

```bash
cosign verify ghcr.io/l-k-m/dl-tool:1.0.0 \
  --certificate-identity-regexp '^https://github\.com/L-K-M/dl-tool/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

docker buildx imagetools inspect ghcr.io/l-k-m/dl-tool:1.0.0   # lists both platforms, the SBOM and the provenance attestation
```

## Steps
1. Append the `gluetun` and `caddy` services to `compose.yaml` exactly as above, after `aria2`, reusing the
   existing `*service-defaults` anchor, and append the two `wireguard_*` entries to the top-level
   `secrets:` block. Change nothing T125 wrote.
2. Keep `VPN_SERVICE_PROVIDER: "${VPN_SERVICE_PROVIDER:-}"` in its `:-` form. Compose interpolates every
   service before it filters by profile, so a `:?` here would fail a core-only `up` from a fresh `.env`;
   a missing provider is rejected by gluetun at container start, which exists only under the `vpn`
   profile — no traffic routes outside the tunnel because the engines share gluetun's netns.
3. Record, as a comment beside `caddy`, that the `proxy` profile wants the app port bound to loopback
   (`"127.0.0.1:${DLTOOL_PORT:-8091}:8080"`) so Caddy is the only public entry point.
4. Append the `vpn profile only` block to `.env.example`. Every value stays empty except `VPN_TYPE` and
   `VPN_PORT_FORWARDING`; add no real credential.
5. Create `deploy/unraid/dl-tool.xml` as above and carry the `UNVERIFIED` note forward into the file as an
   XML comment.
6. Edit `README.md`: rename `## Quickstart (once it exists)` to `## Quickstart`, keep the three-command
   sequence and the "no default credentials" sentence, add the two verification commands above under it,
   and state the prerequisites line: Docker Compose v2.24 or newer — the top-level `secrets:` block uses
   the `environment:` source, which landed in v2.24.0.
7. Edit the README status banner so it no longer claims the repository contains no runnable code, and leave
   the "On content" section — zero piracy indexers, no telemetry, no phone-home update check — untouched.
8. Start the stack, exercise the UI for five minutes with outbound traffic captured, and confirm the only
   destinations are hosts the operator configured. Paste the capture summary under `## Evidence`.
9. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `make compose-check` exits `0` for the base file and for the base plus the dev overlay, with every profile selected.
- [ ] `docker compose config` shows `dl-tool` and `qbittorrent` with no profile, `aria2` under `aria2`, `gluetun` under `vpn` and `caddy` under `proxy`.
- [ ] With `VPN_SERVICE_PROVIDER` empty in `.env`, `docker compose config` succeeds with no profile
  selected and under `COMPOSE_PROFILES=vpn`; rejecting a missing provider is gluetun's job at container
  start, not Compose's at config time.
- [ ] Under `COMPOSE_PROFILES=vpn`, the rendered `gluetun` service has no `env_file`, sets
  `WIREGUARD_PRIVATE_KEY_SECRETFILE` and `WIREGUARD_ADDRESSES_SECRETFILE`, and the rendered top-level
  `secrets:` block carries `wireguard_private_key` and `wireguard_addresses` (verified on Compose
  v5.5.1 — it prunes unreferenced secrets from the render when the profile is inactive).
- [ ] `docker compose config` still emits no `version` warning and publishes no engine WebUI or RPC port.
- [ ] `.env.example` contains no secret value, only empty assignments, `wireguard`, `off` and comments.
- [ ] A five-minute capture shows no request to any host the operator did not configure.
- [ ] The README quickstart's commands run as written, and the banner no longer says the repository has no runnable code.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
cp .env.example .env && ARIA2_RPC_SECRET=checkonly VPN_SERVICE_PROVIDER=checkonly \
  COMPOSE_PROFILES=aria2,vpn,proxy make compose-check && echo COMPOSE_HARDENED_OK
```
Expected: both `docker compose config -q` invocations print nothing, no `version` warning appears, and the
final line of stdout is exactly `COMPOSE_HARDENED_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the four paths in the Files table and nothing else. `.env` is untracked and must not
appear; add it to `.gitignore` only if T125 did not.

## Out of scope — do NOT
- Do NOT create `compose.yaml`, `compose.dev.yaml` or `.env.example`, and do NOT change the three services
  T125 wrote; this task only appends to those files.
- Do NOT create or edit the `Dockerfile`, `.dockerignore` or `deploy/entrypoint.sh`; T124 wrote them and
  T093 hardens them.
- Do NOT write `.github/workflows/release.yml` or produce an SBOM, provenance or signature; T097 does, and
  this task only documents how to verify them.
- Do NOT create `deploy/caddy/Caddyfile.example` or `deploy/traefik/labels.md`; T095 owns them, and the
  `caddy` service mounts the file T095 creates.
- Do NOT create `deploy/aria2/Dockerfile`; T115 owns the aria2 image.
- Do NOT add a Postgres service or profile: SQLite is the only datastore.
- Do NOT commit `.env`, and do not put a real secret into `.env.example`.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked — resolved

**Remedy applied.** The `## Interface contract` now quotes doc 10 §2's current `gluetun` verbatim —
explicit VPN variables, the two `*_SECRETFILE` vars, the service `secrets:` list, `:-` for the
provider — and step 1 also appends `wireguard_private_key` and `wireguard_addresses` to the top-level
`secrets:` block. The §6 `.env.example` quote carries `SERVER_COUNTRIES=`. Step 2's `:?` instruction is
gone — §2's comment is the design — and the named-error acceptance criterion is replaced with the
guarantee the design makes: `docker compose config` succeeds on a fresh `.env` with and without the
`vpn` profile, and a missing provider is rejected by gluetun at container start, not by Compose at
config time. The deferral proposed in the record below was never merged — its PR was closed and this
repair applies its remedy instead; the status stays `todo`. The original record is preserved below.

---

### 2026-09-25 — the contract quotes a `gluetun` service doc 10 §2 deliberately rewrote

The `## Interface contract` and steps 1–2 transcribed
[`docs/10-deployment-and-compose.md`](../10-deployment-and-compose.md#2-composeyaml) §2 as of
2026-09-01. Two commits on 2026-09-02 rewrote that service on purpose:

- `3de51b7` removed `env_file: [.env]` — it handed the VPN container the qBittorrent password and
  the aria2 RPC secret it has no use for — and moved `WIREGUARD_PRIVATE_KEY`/`WIREGUARD_ADDRESSES`
  into Compose named secrets mounted `0400` behind the `*_SECRETFILE` variables.
- `36f3af6` replaced `VPN_SERVICE_PROVIDER: "${VPN_SERVICE_PROVIDER:?required for the vpn
  profile}"` with the `:-` form: Compose interpolates every service before it filters by profile, so
  a `:?` in the inactive `vpn` profile fails a core-only `up` from a fresh `.env`.

Verified with Docker Compose v5.5.1 — the contract's literal block appended to `compose.yaml`, `.env`
a fresh copy of `.env.example` (`VPN_SERVICE_PROVIDER` empty):

```bash
docker compose -f compose.yaml config -q
#   error while interpolating services.gluetun.environment.VPN_SERVICE_PROVIDER:
#   required variable VPN_SERVICE_PROVIDER is missing a value: required for the vpn profile
#   (exit 1 — with NO profile selected)
```

So the task as written was unsatisfiable: the `:?` form step 2 mandated — and the third acceptance
criterion required — made plain `docker compose -f compose.yaml config -q` fail for every operator
whose `.env` left `VPN_SERVICE_PROVIDER` empty; the `env_file: [.env]` the contract dictated
reintroduced the credential leak `3de51b7` removed; and following doc 10 §2 instead left the
named-error criterion unmeetable, since Compose has no profile-scoped interpolation. The §6
`.env.example` quote was stale the same way, lacking `SERVER_COUNTRIES=`.
