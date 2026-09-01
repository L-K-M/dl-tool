# T093 — Build the runtime image and the PUID/PGID entrypoint

| Field | Value |
|---|---|
| **ID** | T093 |
| **Milestone** | M7 |
| **Status** | todo |
| **Depends on** | T003, T004, T013 |
| **Blocks** | T094, T097, T113, T115 |
| **Parallel-safe** | yes — creates the image build files only |
| **Implements** | [NFR-004](../02-requirements.md#nfr-004-shut-down-gracefully-on-sigterm), [NFR-005](../02-requirements.md#nfr-005-publish-a-multi-architecture-image), [NFR-025](../02-requirements.md#nfr-025-run-unprivileged-with-the-operators-uid-and-gid) |
| **Decisions** | [ADR-0011](../decisions/0011-alpine-runtime-with-puid-pgid.md), [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md), [ADR-0012](../decisions/0012-single-data-mount.md) |
| **Est. size** | 3 new files, ~200 LOC |

## Goal
`make docker-build` produces one `alpine:3.22` image containing the static `dl-tool` binary with the SPA
embedded, `su-exec`, `ca-certificates`, `tzdata`, `7zip` and `nodejs`. The entrypoint drops to `PUID:PGID`,
applies `UMASK` and `TZ`, and `exec`s the binary so `SIGTERM` reaches PID 1.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/10-deployment-and-compose.md` §5 `Dockerfile`](../10-deployment-and-compose.md#5-dockerfile) — the four stages, verbatim, including the label block and the `ENV` defaults.
2. [`docs/10-deployment-and-compose.md` §4 PUID, PGID, UMASK, TZ](../10-deployment-and-compose.md#4-puid-pgid-umask-tz) — the nine entrypoint steps, in order.
3. [`docs/10-deployment-and-compose.md` §4.1 The `user:` alternative](../10-deployment-and-compose.md#41-the-user-alternative) — the non-root detection branch.
4. [`docs/17-operations-and-runbook.md` §2 Shutdown](../17-operations-and-runbook.md#2-shutdown) — why `exec` matters and what a clean stop leaves behind.
5. [`docs/13-testing-and-verification.md` §2 Makefile](../13-testing-and-verification.md#2-makefile) — the `docker-build` target.

## Files
| Path | Action | Purpose |
|---|---|---|
| `Dockerfile` | create | The four-stage build of doc 10 §5, verbatim. |
| `.dockerignore` | create | Keep `.git`, `docs/`, `node_modules/`, `bin/`, `config/` and `.env` out of the build context. |
| `deploy/entrypoint.sh` | create | The privilege drop of doc 10 §4. |

No other file may be modified.

## Interface contract

`Dockerfile` is reproduced **verbatim** from
[`docs/10-deployment-and-compose.md` §5](../10-deployment-and-compose.md#5-dockerfile), except that the three
yt-dlp pin arguments carry real values instead of the empty `pin at implementation time` defaults — an empty
`YTDLP_VERSION` makes the `ytdlp` stage unbuildable, so the image cannot exist without them:

```dockerfile
ARG YTDLP_VERSION="<the newest stable yt-dlp tag on the day this task runs>"
ARG YTDLP_SHA256_AMD64="<sha256 of yt-dlp_musllinux at that tag>"
ARG YTDLP_SHA256_ARM64="<sha256 of yt-dlp_musllinux_aarch64 at that tag>"
```

These three lines are the whole pin. T113 owns the policy around them: refreshing them, proving the hash
check fails on a wrong value, and the boot capability probe. T097's weekly job rewrites exactly these three
lines, so they must each stay on one line starting with `ARG `.

`deploy/entrypoint.sh`, mode `0755`:

```sh
#!/bin/sh
# dl-tool container entrypoint. Runs as root, drops to PUID:PGID, execs the binary.
# Order is fixed by docs/10-deployment-and-compose.md section 4.
set -eu

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"
UMASK="${UMASK:-002}"
TZ="${TZ:-Etc/UTC}"

if [ -f "/usr/share/zoneinfo/$TZ" ]; then
  ln -snf "/usr/share/zoneinfo/$TZ" /etc/localtime
  printf '%s\n' "$TZ" > /etc/timezone
fi

umask "$UMASK"

if [ "$(id -u)" -ne 0 ]; then
  echo "entrypoint: already running as $(id -u):$(id -g); skipping user creation and chown" >&2
  exec /usr/local/bin/dl-tool "$@"
fi

if [ "$PUID" = "0" ]; then
  echo "entrypoint: PUID=0, running dl-tool as root" >&2
  exec /usr/local/bin/dl-tool "$@"
fi

getent group  "$PGID" >/dev/null 2>&1 || addgroup -g "$PGID" dltool
getent passwd "$PUID" >/dev/null 2>&1 || adduser -D -H -u "$PUID" -G "$(getent group "$PGID" | cut -d: -f1)" dltool

mkdir -p "${DLTOOL_CONFIG_DIR:-/config}"
chown -R "$PUID:$PGID" "${DLTOOL_CONFIG_DIR:-/config}"
# NEVER chown /data recursively: it can hold terabytes and the operator owns its permissions.

for root in $(printf '%s' "${DLTOOL_DATA_ROOTS:-/data}" | tr ':' ' '); do
  su-exec "$PUID:$PGID" test -w "$root" || \
    echo "entrypoint: data_root_not_writable $root as $PUID:$PGID" >&2
done

exec su-exec "$PUID:$PGID" /usr/local/bin/dl-tool "$@"
```

`.dockerignore`:

```gitignore
.git
.github
docs
bin
config
node_modules
web/node_modules
web/dist
.env
*.db
*.db-wal
*.db-shm
```

## Steps
1. Create `Dockerfile` by copying the four stages of doc 10 §5 exactly: the `node:24-alpine` web stage, the
   `golang:1.26-alpine` cross-compiling build stage, the `ytdlp` fetch stage, and the `alpine:3.22` runtime.
2. Resolve the newest stable `yt-dlp` release tag, download `yt-dlp_musllinux` and
   `yt-dlp_musllinux_aarch64` from it, record both `sha256sum` outputs, and write all three into the `ARG`
   defaults. Keep each on one line beginning with `ARG `.
3. Keep the runtime `apk add --no-cache su-exec ca-certificates tzdata 7zip nodejs` line exactly as written.
   Python is never installed.
4. Keep the `LABEL` block, the `ENV` defaults, `EXPOSE 8080`, the `HEALTHCHECK` calling
   `/usr/local/bin/dl-tool healthcheck`, `ENTRYPOINT ["/entrypoint.sh"]` and `CMD ["serve"]`. Add no
   `VOLUME` instruction.
5. Create `.dockerignore` as above so the build context excludes the repository's own state.
6. Create `deploy/entrypoint.sh` exactly as above and mark it executable with `chmod 0755`.
7. Confirm the entrypoint's last line is an `exec`: without it the shell stays PID 1 and swallows `SIGTERM`.
8. Build once and confirm the image runs unprivileged: start it with `PUID=1001 PGID=1001`, write a file into
   a mounted data directory and check the owning uid.
9. Send `SIGTERM` to the running container and confirm it exits `0` within 20 seconds leaving no `-wal` file
   beside the database.

## Acceptance criteria
- [ ] `make docker-build` completes and the resulting image is based on `alpine:3.22`.
- [ ] `docker run --rm <image> sh -c 'command -v su-exec 7zz node'` prints three paths.
- [ ] The image contains no `python3` binary.
- [ ] The `ytdlp` stage's `sha256sum -c -` step prints `/yt-dlp: OK` during the build.
- [ ] With `PUID=1001 PGID=1001`, `docker exec <c> id -u` prints `1001` and files written to `/data` are owned by `1001`.
- [ ] With compose `user: "1000:1000"`, the entrypoint skips user creation and still applies `UMASK` and `TZ`.
- [ ] `docker stop` returns within 20 s, the container exit code is `0`, and no `dl-tool.db-wal` remains.
- [ ] `/data` is never chowned recursively by the entrypoint.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make docker-build VERSION=t093 && docker run --rm --entrypoint sh ghcr.io/l-k-m/dl-tool:t093 -c 'command -v su-exec 7zz node yt-dlp; ! command -v python3 && echo NO_PYTHON'
```
Expected: the `ytdlp` stage prints `/yt-dlp: OK`, the build ends with `naming to ghcr.io/l-k-m/dl-tool:t093`,
then five lines — `/sbin/su-exec`, `/usr/bin/7zz`, `/usr/bin/node`, `/usr/local/bin/yt-dlp`, `NO_PYTHON`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT create `compose.yaml`, `compose.dev.yaml` or `.env.example`; T094 owns them.
- Do NOT add the yt-dlp capability probe, the `js_runtime_missing` code or the rate-limit flag; T113 owns them.
- Do NOT create `deploy/aria2/Dockerfile`; T115 owns the aria2 image.
- Do NOT create `.github/workflows/release.yml`; T097 owns publishing, SBOM and signing.
- Do NOT add `/custom-cont-init.d` or `DOCKER_MODS`: both execute third-party code at runtime.
- Do NOT switch the base image to Debian or distroless: neither can perform the privilege drop.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
