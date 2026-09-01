# T115 — Build and publish the aria2 image

| Field | Value |
|---|---|
| **ID** | T115 |
| **Milestone** | M7 |
| **Status** | todo |
| **Depends on** | T093, T094, T097 |
| **Blocks** | — |
| **Parallel-safe** | no — it edits `.github/workflows/release.yml` |
| **Implements** | [NFR-005](../02-requirements.md#nfr-005-publish-a-multi-architecture-image), [NFR-025](../02-requirements.md#nfr-025-run-unprivileged-with-the-operators-uid-and-gid), [FR-143](../02-requirements.md#fr-143-list-engines-and-test-connectivity) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md), [ADR-0011](../decisions/0011-alpine-runtime-with-puid-pgid.md), [ADR-0012](../decisions/0012-single-data-mount.md) |
| **Est. size** | 2 new files, ~150 LOC |

## Goal
`deploy/aria2/Dockerfile` builds an `alpine:3.22` image running `aria2c` with JSON-RPC enabled, the same
PUID/PGID/UMASK/TZ drop as the application image, and a mandatory RPC secret. The release workflow publishes
it as `ghcr.io/l-k-m/dl-tool-aria2` for `linux/amd64` and `linux/arm64`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/10-deployment-and-compose.md` §5.1 `deploy/aria2/Dockerfile`](../10-deployment-and-compose.md#51-deployaria2dockerfile) — the Dockerfile and the `aria2c` argument list, verbatim.
2. [`docs/10-deployment-and-compose.md` §4 PUID, PGID, UMASK, TZ](../10-deployment-and-compose.md#4-puid-pgid-umask-tz) — the nine entrypoint steps this image repeats.
3. [`docs/10-deployment-and-compose.md` §1 Service inventory](../10-deployment-and-compose.md#1-service-inventory) — the `aria2` profile, its port and why nothing is published.
4. [`docs/10-deployment-and-compose.md` §10 Release workflow](../10-deployment-and-compose.md#10-release-workflow) — the matrix entry this task adds.
5. [`docs/06-download-engines.md` §4.1 Daemon flags](../06-download-engines.md#41-daemon-flags) — which flags the adapter depends on.

## Files
| Path | Action | Purpose |
|---|---|---|
| `deploy/aria2/Dockerfile` | edit | T028 created the two-line build for the contract test; add `su-exec`, `ca-certificates`, `tzdata`, `curl`, the `ENTRYPOINT` and the `HEALTHCHECK`. |
| `deploy/aria2/entrypoint.sh` | create | Privilege drop, then `exec su-exec … aria2c` with the documented flags. |
| `.github/workflows/release.yml` | edit | Add the `dl-tool-aria2` entry to the build matrix. |

No other file may be modified.

## Interface contract

`deploy/aria2/Dockerfile` is reproduced **verbatim** from
[`docs/10-deployment-and-compose.md` §5.1](../10-deployment-and-compose.md#51-deployaria2dockerfile),
including the `LABEL` block, `EXPOSE 6800` and `ENTRYPOINT ["/entrypoint.sh"]`.

`deploy/aria2/entrypoint.sh`, mode `0755`:

```sh
#!/bin/sh
# aria2 sidecar entrypoint for dl-tool. Same identity handling as deploy/entrypoint.sh.
set -eu

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"
UMASK="${UMASK:-002}"
TZ="${TZ:-Etc/UTC}"

if [ -z "${ARIA2_RPC_SECRET:-}" ]; then
  echo "entrypoint: ARIA2_RPC_SECRET is empty; refusing to start an unauthenticated RPC endpoint" >&2
  exit 1
fi

if [ -f "/usr/share/zoneinfo/$TZ" ]; then
  ln -snf "/usr/share/zoneinfo/$TZ" /etc/localtime
  printf '%s\n' "$TZ" > /etc/timezone
fi

umask "$UMASK"

mkdir -p /config
: > /config/aria2.conf
touch /config/aria2.session

if [ "$(id -u)" -eq 0 ] && [ "$PUID" != "0" ]; then
  getent group  "$PGID" >/dev/null 2>&1 || addgroup -g "$PGID" aria2
  getent passwd "$PUID" >/dev/null 2>&1 || adduser -D -H -u "$PUID" -G "$(getent group "$PGID" | cut -d: -f1)" aria2
  chown -R "$PUID:$PGID" /config
  # NEVER chown /data recursively.
  set -- su-exec "$PUID:$PGID" aria2c "$@"
else
  set -- aria2c "$@"
fi

exec "$@" \
  --enable-rpc --rpc-listen-all --rpc-listen-port=6800 \
  --rpc-secret="$ARIA2_RPC_SECRET" \
  --dir=/data --continue=true --disk-cache=64M \
  --conf-path=/config/aria2.conf --save-session=/config/aria2.session \
  --input-file=/config/aria2.session --save-session-interval=30
```

The matrix entry added to `.github/workflows/release.yml`, alongside the existing `dl-tool` entry:

```yaml
          - name: dl-tool-aria2
            context: ./deploy/aria2
            dockerfile: ./deploy/aria2/Dockerfile
            image: ghcr.io/l-k-m/dl-tool-aria2
```

## Steps
1. Create `deploy/aria2/Dockerfile` exactly as doc 10 §5.1 specifies. `curl` is present only so the compose
   healthcheck can post `aria2.getVersion`; it fetches nothing at build time.
2. Create `deploy/aria2/entrypoint.sh` as above and `chmod 0755` it. The final line must be an `exec`, so
   `SIGTERM` reaches `aria2c` and the session file is written on stop.
3. Refuse to start when `ARIA2_RPC_SECRET` is empty. The aria2 manual is explicit that the secret token is
   strongly recommended; an unauthenticated RPC endpoint on a shared network is a remote-control surface.
4. Keep `--save-session` and `--input-file` pointing at the same `/config/aria2.session` file, so a restart
   reloads what was in flight; `internal/engine/aria2` depends on that.
5. Keep `--dir=/data`: it must match the single data mount, or moves become copies.
6. Edit `.github/workflows/release.yml` to add the matrix entry above, and delete the comment T097 left
   saying this task adds it.
7. Confirm `docker compose --profile aria2 config` still resolves both the `image:` and the `build:` keys for
   the `aria2` service; the published image is pulled and `docker compose build aria2` rebuilds it locally
   under the same tag.
8. Build the image locally for both platforms and confirm `aria2c --version` runs.
9. Start the container with a secret set and confirm an unauthenticated `aria2.getVersion` post is answered
   at the transport level while an unauthorised method call is refused.

## Acceptance criteria
- [ ] `docker build ./deploy/aria2` succeeds on `linux/amd64` and `linux/arm64`.
- [ ] The image is based on `alpine:3.22` and contains `aria2c`, `su-exec` and `curl`.
- [ ] Starting the container with `ARIA2_RPC_SECRET` unset exits non-zero with the named message.
- [ ] With `PUID=1001 PGID=1001`, the `aria2c` process runs as uid `1001` and `/data` is not chowned.
- [ ] `/config/aria2.session` survives a stop and start, and in-flight transfers resume.
- [ ] `.github/workflows/release.yml` builds both images from one matrix.
- [ ] Nothing in the repository references `p3terx/aria2-pro`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
docker buildx build --platform linux/amd64,linux/arm64 --output=type=cacheonly ./deploy/aria2 && docker build -t dl-tool-aria2:t115 ./deploy/aria2 && docker run --rm --entrypoint aria2c dl-tool-aria2:t115 --version | head -1
```
Expected: both builds complete with no error, and the final line begins with `aria2 version `.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT edit `compose.yaml`; T094 already declares the `aria2` service with both `image:` and `build:`.
- Do NOT change the aria2 adapter or its contract test; T019 and T028 own `internal/engine/aria2/`.
- Do NOT publish the RPC port to the host: only `dl-tool` reaches it, over the compose network.
- Do NOT depend on `p3terx/aria2-pro`; its last push was 2022-09-06.
- Do NOT give aria2 any BitTorrent role: it has no BEP 52 and no uTP, so torrents go to qBittorrent.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
