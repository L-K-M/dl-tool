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
- [x] `docker buildx build --platform linux/amd64,linux/arm64 .` succeeds and the Go and npm stages run on the build platform, not under QEMU.
- [x] The `ytdlp` stage prints `/yt-dlp: OK` on both platforms, and a one-character change to either hash fails the build at `sha256sum -c -`.
- [x] `docker image inspect` reports `org.opencontainers.image.source` as `https://github.com/L-K-M/dl-tool` and a non-empty `org.opencontainers.image.version`.
- [x] `docker run --rm --entrypoint sh <image> -c 'yt-dlp --version'` prints the pinned version, and the image still contains no `python3`.
- [x] The three `ARG YTDLP_` lines each occupy exactly one line starting with `ARG `, and appear nowhere else in the repository.
- [x] The entrypoint, `HEALTHCHECK`, `ENV` block and `apk add` line are unchanged from T124.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make docker-build VERSION=t093 && docker run --rm --entrypoint sh ghcr.io/l-k-m/dl-tool:t093 -c 'yt-dlp --version; ! command -v python3 && echo NO_PYTHON'
test "$(docker image inspect ghcr.io/l-k-m/dl-tool:t093 --format '{{index .Config.Labels "org.opencontainers.image.source"}}')" = "https://github.com/L-K-M/dl-tool"
test -n "$(docker image inspect ghcr.io/l-k-m/dl-tool:t093 --format '{{index .Config.Labels "org.opencontainers.image.version"}}')"
docker run --privileged --rm tonistiigi/binfmt --install arm64
docker buildx create --name t093 --driver docker-container --use --bootstrap
docker buildx build --platform linux/amd64,linux/arm64 --progress=plain .
sed -i 's/YTDLP_SHA256_AMD64="f/YTDLP_SHA256_AMD64="0/' Dockerfile
docker build --target ytdlp --progress=plain . 2>&1 | tee /tmp/t093-corrupt.log && { echo 'corrupted hash did not fail the build' >&2; exit 1; }
git checkout -- Dockerfile
grep -q 'did NOT match' /tmp/t093-corrupt.log
```
Expected: `make docker-build` prints `/yt-dlp: OK` in the `ytdlp` stage, ends with `naming to
ghcr.io/l-k-m/dl-tool:t093`, and `docker run` prints exactly two lines — the pinned yt-dlp version and
`NO_PYTHON`. The two `test` lines print nothing. `docker buildx build` prints `/yt-dlp: OK` once per
platform — amd64 natively, arm64 under QEMU — and exits 0. The corrupted-hash build fails inside the
`ytdlp` stage at `sha256sum -c -` (`did NOT match`), and `git checkout` restores `Dockerfile`.

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

**Sandbox limitation (same as T124; stated, not worked around):** this session runs as
uid 1000 in a container with no Docker daemon, no buildx plugin, no `sudo`, and seccomp
blocking `unshare`, so the verbatim Verification block cannot execute here:

```
$ make docker-build VERSION=t093
docker build -t ghcr.io/l-k-m/dl-tool:t093 .
Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?
make: *** [Makefile:57: docker-build] Error 1
$ docker buildx build --platform linux/amd64,linux/arm64 .
unknown flag: --platform                       # no buildx plugin; no daemon either way
$ unshare -U -r true
unshare: unshare failed: Operation not permitted
```

The verbatim Verification block **ran and passed on this PR's `verify` job**
(`.github/workflows/task-verification.yml` extracts this file's `## Verification`
bash and executes it on a Docker-capable runner; the `compose` job in `ci.yml`
independently ran `make docker-build`). Observed output from the verify job's
"Run task Verification" step at the final tree (run 36039822023, job 107768912736):

```
#20 [ytdlp 3/3] RUN set -eu; case "amd64" in amd64) file=yt-dlp_musllinux; ... esac; ...
#20 1.228 /yt-dlp: OK
#35 naming to ghcr.io/l-k-m/dl-tool:t093 done
$ docker run --rm --entrypoint sh ghcr.io/l-k-m/dl-tool:t093 -c 'yt-dlp --version; ! command -v python3 && echo NO_PYTHON'
2026.08.19
NO_PYTHON
# (the two docker image inspect asserts printed nothing — source and non-empty version held)

# binfmt registered linux/arm64, then the docker-container builder solved both platforms:
#24 [linux/amd64->arm64 ytdlp 3/3] RUN set -eu; case "arm64" in arm64) file=yt-dlp_musllinux_aarch64; ... esac; ...
#24 1.617 /yt-dlp: OK
#25 [linux/amd64 ytdlp 3/3]      RUN set -eu; case "amd64" in amd64) file=yt-dlp_musllinux; ... esac; ...
#25 1.454 /yt-dlp: OK
#39 [linux/amd64->arm64 build 7/7] CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath ...
#31 [linux/arm64 stage-4 2/6] RUN apk add --no-cache su-exec ca-certificates tzdata nodejs   (the only emulated layer)

# one-character hash corruption, built --target ytdlp:
#8 0.888 /yt-dlp: FAILED
#8 0.888 sha256sum: WARNING: 1 of 1 computed checksums did NOT match
#8 ERROR: process "... sha256sum -c -; chmod 0755 /yt-dlp" did not complete successfully: exit code: 1
# git checkout restored Dockerfile; grep 'did NOT match' on the log passed
```

The same job then ran `make ci` end-to-end green. The multi-arch log also shows the
criterion's intent: `web`, `build`, `ytdlp` and `sevenzip` all ran as `linux/amd64`
steps for both solves — Go and npm work natively, no QEMU — while only the runtime
`apk add` ran under `linux/arm64`.

**Pin resolution** — `GET https://api.github.com/repos/yt-dlp/yt-dlp/releases/latest`
returned tag `2026.08.19` (newest stable). Both musllinux assets were downloaded from
that release and hashed locally; these values are written into the `ARG YTDLP_`
defaults:

```
$ sha256sum yt-dlp-amd64 yt-dlp-arm64
f3dec9cfeaf304cec98290fe41c6ad465d4b747d302473559643e7af24929722  yt-dlp-amd64
17b164c4d258be92bb1ad146cb7c336b783aedb380814aabbcb7d52937f77e57  yt-dlp-arm64
```

**Stage logic replicated locally** — the `ytdlp` stage's `case` + `sha256sum -c` block
run against the real downloaded binaries:

```
TARGETARCH=amd64  -> /yt-dlp: OK        (file=yt-dlp_musllinux)
TARGETARCH=arm64  -> /yt-dlp: OK        (file=yt-dlp_musllinux_aarch64)
TARGETARCH=bogus  -> unsupported TARGETARCH=bogus; exit 1
one-char corrupted hash -> /yt-dlp: FAILED, "1 computed checksum did NOT match", exit 1
```

**`yt-dlp --version` runs the pinned binary** — `yt-dlp_musllinux` is dynamically
linked against musl (`libc.musl-x86_64.so.1`, interp `/lib/ld-musl-x86_64.so.1`), so it
cannot run on this glibc host directly, but it executed under Alpine 3.22's own loader:
`musl-1.2.5-r12` and `zlib-1.3.2-r0` extracted from `dl-cdn.alpinelinux.org/alpine/v3.22`
(the same repo the runtime stage's `apk add` uses), then
`ld-musl-x86_64.so.1 --library-path ... ./yt-dlp --version` printed `2026.08.19`. No
Python is involved — the binary is musl-linked and the unchanged `apk add` line carries
no Python package.

**`make ci` on the final tree** — run twice after `npm ci --prefix web`, green both
times it completed: `gofmt` clean, `golangci-lint` 0 issues, ESLint clean, Prettier
clean, `tsc --noEmit` clean, all Go packages `ok` with `-race`, 301/301 Vitest, both
`docker compose config -q` clean, `doclint` 0 errors. One intermediate re-run hit
`TestChainFanoutDeliversEvent` in `internal/jobs` (`handlers_notify_test.go:498`), a
pre-existing flake on `origin/main` — it passes and fails across identical trees
(F, F, ok on isolated reruns); `internal/jobs` is untouched by this diff and outside
this task's `## Files` table.

**Scope** — `git status --porcelain=v1 -uall -- . ':(exclude)docs'` prints exactly one
line: `Dockerfile`.

**Deliberate deviations from this file's quoted contract, each toward the newer
canonical text in doc 10 §5 (the "post-T093 end state" per T124's contract):**

- `org.opencontainers.image.licenses` uses §5's
  `Unlicense AND MIT AND MPL-2.0 AND LGPL-2.1-or-later AND LicenseRef-unRAR`. The
  contract block's `"Unlicense"` quote predates commit `9d4b3cc`, which added the
  unRAR term when the `7zzs` stage landed; shipping `Unlicense`-only would mislabel an
  image that contains the unRAR codec.
- A global `ARG VERSION=dev REVISION=unknown CREATED=1970-01-01T00:00:00Z` was added
  before the first `FROM`, so the contract's stage lines stay byte-identical (`ARG
  TARGETOS TARGETARCH VERSION REVISION`, `ARG VERSION REVISION CREATED`) while bare
  `ARG` re-declaration inherits the global defaults. This preserves the
  non-empty-fallback fix T124's review added in `8a3234c`, keeps
  `org.opencontainers.image.version` non-empty under `make docker-build` (which
  forwards no `--build-arg`), and gives `org.opencontainers.image.created` a valid
  RFC 3339 value — the epoch is the reproducible-builds convention for "unset" — since
  an empty `created` would violate the OCI annotation spec. The release workflow still
  overrides all three via `--build-arg` / metadata labels (doc 10 §10).
- The T124 `# NOTE: yt-dlp is NOT installed in this image yet ...` comment was removed:
  it described the pre-T093 gap this task closes and would now be false.
- The `ytdlp` stage sits directly above the runtime stage per step 5; §5 shows it
  before `sevenzip` — stage order is functionally irrelevant.
- `COPY --from=ytdlp /yt-dlp     /usr/local/bin/yt-dlp` is placed between the dl-tool
  and 7zz copies, matching §5's order; `DLTOOL_YTDLP_PATH` already pointed there.
- `COPY --chmod=755 deploy/entrypoint.sh` is kept from T124 (the contract does not
  name that line and step 7 forbids touching unnamed lines).

Note on the uniqueness criterion: `2026.08.19` also occurs in pre-existing prose in
`docs/06-download-engines.md`, `PLAN-REVIEW*.md` and task files T088/T090 as "the
version the plan's research measured against" — not a second copy of the pin. The three
`ARG YTDLP_` lines themselves exist only in `Dockerfile`; the hashes appear nowhere
else in the repository.

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
