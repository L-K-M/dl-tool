# T093 — Harden the runtime image for a multi-arch release

| Field | Value |
|---|---|
| **ID** | T093 |
| **Milestone** | M7 |
| **Status** | todo |
| **Depends on** | T124 |
| **Blocks** | T094, T097, T113, T115 |
| **Parallel-safe** | yes — modifies `Dockerfile` only |
| **Implements** | [NFR-005](../02-requirements.md#nfr-005-publish-a-multi-architecture-image) |
| **Decisions** | [ADR-0011](../decisions/0011-alpine-runtime-with-puid-pgid.md), [ADR-0018](../decisions/0018-pin-ytdlp-by-version-and-hash.md) |
| **Est. size** | 1 modified file, ~60 net new lines |

## Goal
The `Dockerfile` T124 wrote becomes the release image: both build stages run on `$BUILDPLATFORM` and
cross-compile by `$TARGETARCH`, a fourth stage fetches the pinned `yt-dlp_musllinux` binary and verifies its
SHA-256, and the runtime stage carries the OCI label block. `docker buildx build --platform
linux/amd64,linux/arm64` succeeds with no QEMU emulation of the Go or npm work.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/10-deployment-and-compose.md` §5 `Dockerfile`](../10-deployment-and-compose.md#5-dockerfile) — the four stages, verbatim, including the `LABEL` block and the three pin `ARG`s.
2. [`docs/tasks/T124-runtime-dockerfile-and-entrypoint.md`](T124-runtime-dockerfile-and-entrypoint.md) — the three stages already in the file; this task edits them, it does not rewrite them.
3. [`docs/12-security-and-threat-model.md` §8.1 yt-dlp: freshness versus unreviewed code](../12-security-and-threat-model.md#81-yt-dlp-freshness-versus-unreviewed-code) — why the version and both hashes are pinned and never self-updated.
4. [`docs/13-testing-and-verification.md` §2 Makefile](../13-testing-and-verification.md#2-makefile) — the `docker-build` target this task's verification calls.

## Files
| Path | Action | Purpose |
|---|---|---|
| `Dockerfile` | modify | Add `--platform=$BUILDPLATFORM`, the `TARGETOS`/`TARGETARCH` cross-compile, the `ytdlp` fetch stage and the OCI `LABEL` block. |

No other file may be modified.

## Interface contract

The four edits to `Dockerfile`, each reproduced from
[`docs/10-deployment-and-compose.md` §5](../10-deployment-and-compose.md#5-dockerfile):

```dockerfile
FROM --platform=$BUILDPLATFORM node:24-alpine AS web

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH VERSION REVISION
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.revision=${REVISION}" \
      -o /out/dl-tool ./cmd/dl-tool
```

```dockerfile
# yt-dlp: fetched on the build platform, selected by TARGETARCH, verified by SHA-256.
FROM --platform=$BUILDPLATFORM alpine:3.22 AS ytdlp
ARG TARGETARCH
# The three defaults below ARE the pin, and the only place the version and hashes live.
ARG YTDLP_VERSION="<the newest stable yt-dlp tag on the day this task runs>"
ARG YTDLP_SHA256_AMD64="<sha256 of yt-dlp_musllinux at that tag>"
ARG YTDLP_SHA256_ARM64="<sha256 of yt-dlp_musllinux_aarch64 at that tag>"
RUN apk add --no-cache curl
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) file=yt-dlp_musllinux;         sum="${YTDLP_SHA256_AMD64}" ;; \
      arm64) file=yt-dlp_musllinux_aarch64; sum="${YTDLP_SHA256_ARM64}" ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /yt-dlp \
      "https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/${file}"; \
    echo "${sum}  /yt-dlp" | sha256sum -c -; \
    chmod 0755 /yt-dlp
```

```dockerfile
FROM alpine:3.22
ARG VERSION REVISION CREATED
LABEL org.opencontainers.image.title="dl-tool" \
      org.opencontainers.image.description="Self-hosted download manager: one queue for HTTP, FTP, BitTorrent and media sites" \
      org.opencontainers.image.url="https://github.com/L-K-M/dl-tool" \
      org.opencontainers.image.documentation="https://github.com/L-K-M/dl-tool/tree/main/docs" \
      org.opencontainers.image.source="https://github.com/L-K-M/dl-tool" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}" \
      org.opencontainers.image.vendor="L-K-M" \
      org.opencontainers.image.licenses="Unlicense" \
      org.opencontainers.image.base.name="docker.io/library/alpine:3.22"
COPY --from=ytdlp /yt-dlp     /usr/local/bin/yt-dlp
```

The three `ARG YTDLP_` lines are the whole pin. T097's weekly job rewrites exactly these three lines, so each
must stay on one line beginning with `ARG `, and no second copy of the version or a hash may exist anywhere
in the repository.

## Steps
1. Edit the `web` and `build` stages of `Dockerfile` to start `FROM --platform=$BUILDPLATFORM`, so npm and
   the Go compiler always run natively and only the final `apk add` layer is emulated.
2. Add `ARG TARGETOS TARGETARCH VERSION REVISION` to the `build` stage and set `GOOS=${TARGETOS}
   GOARCH=${TARGETARCH}` on the `go build` line, keeping `CGO_ENABLED=0` and `-trimpath` from T124.
3. Add `-X main.revision=${REVISION}` to the existing `-ldflags` string; leave `main.version` as it is.
4. Resolve the newest stable `yt-dlp` release tag, download `yt-dlp_musllinux` and
   `yt-dlp_musllinux_aarch64` from it, record both `sha256sum` outputs and write all three into the `ARG`
   defaults of the new `ytdlp` stage.
5. Insert the `ytdlp` stage above the runtime stage exactly as shown, and add
   `COPY --from=ytdlp /yt-dlp /usr/local/bin/yt-dlp` to the runtime stage beside the existing binary copy.
   `DLTOOL_YTDLP_PATH` already points at that path.
6. Add `ARG VERSION REVISION CREATED` and the `LABEL` block to the runtime stage, above the `apk add` line.
7. Leave every line T124 wrote that this contract does not name — the `apk add` set, the `ENV` defaults,
   `EXPOSE`, `HEALTHCHECK`, `ENTRYPOINT` and `CMD` — byte for byte unchanged.
8. Build both platforms once with `docker buildx build --platform linux/amd64,linux/arm64 .` and confirm the
   `ytdlp` stage prints `/yt-dlp: OK` for each.
9. Corrupt one hash by a single character, rebuild, and confirm the build fails at `sha256sum -c -`; restore
   the correct value.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `docker buildx build --platform linux/amd64,linux/arm64 .` succeeds and the Go and npm stages run on the build platform, not under QEMU.
- [ ] The `ytdlp` stage prints `/yt-dlp: OK` on both platforms, and a one-character change to either hash fails the build at `sha256sum -c -`.
- [ ] `docker image inspect` reports `org.opencontainers.image.source` as `https://github.com/L-K-M/dl-tool` and a non-empty `org.opencontainers.image.version`.
- [ ] `docker run --rm --entrypoint sh <image> -c 'yt-dlp --version'` prints the pinned version, and the image still contains no `python3`.
- [ ] The three `ARG YTDLP_` lines each occupy exactly one line starting with `ARG `, and appear nowhere else in the repository.
- [ ] The entrypoint, `HEALTHCHECK`, `ENV` block and `apk add` line are unchanged from T124.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make docker-build VERSION=t093 && docker run --rm --entrypoint sh ghcr.io/l-k-m/dl-tool:t093 -c 'yt-dlp --version; ! command -v python3 && echo NO_PYTHON'
```
Expected: the `ytdlp` stage prints `/yt-dlp: OK`, the build ends with `naming to ghcr.io/l-k-m/dl-tool:t093`,
then exactly two lines — the pinned yt-dlp version and `NO_PYTHON`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly one line, `Dockerfile`, and nothing else.

## Out of scope — do NOT
- Do NOT create `Dockerfile`, `.dockerignore` or `deploy/entrypoint.sh`, and do NOT change the privilege
  drop; T124 owns all four.
- Do NOT create `compose.yaml`, `compose.dev.yaml` or `.env.example`; T125 owns them.
- Do NOT create `.github/workflows/release.yml`, push an image, or attach an SBOM, provenance or signature;
  T097 owns publishing.
- Do NOT add the yt-dlp capability probe or the `js_runtime_missing` code; T113 owns them.
- Do NOT create `deploy/aria2/Dockerfile`; T115 owns the aria2 image.
- Do NOT enable `yt-dlp -U` or any self-update path ([ADR-0018](../decisions/0018-pin-ytdlp-by-version-and-hash.md)).

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
