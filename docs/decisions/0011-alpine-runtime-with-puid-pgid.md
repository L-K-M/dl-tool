# 0011 - Alpine runtime image with PUID/PGID privilege drop

> **Status:** accepted
> **Date:** 2026-09-01
> **Deciders:** repository owner

## Context and Problem Statement

Docker runs containers as `root`, so without a mapping every file dl-tool writes under `/data` is owned by
`root` and is unusable from a NAS file manager, over SMB, or by the qBittorrent container sharing the same
mount. The established answer is the linuxserver.io convention — `PUID`, `PGID`, `UMASK`, `TZ` read by an
entrypoint that runs as root, fixes ownership and then drops privileges. The research corpus disagreed with
itself about the base image: `architecture.md` recommended distroless, `deployment.md` recommended
`debian:trixie-slim`, and both predate the check of whether Alpine can carry yt-dlp. It can.

## Decision Drivers

- **Verified:** the Go binary is built `CGO_ENABLED=0` and is statically linked, so it has no libc
  dependency at all and musl-versus-glibc is irrelevant to it ([ADR-0002](0002-go-for-the-backend.md)).
- **Verified:** yt-dlp publishes `yt-dlp_musllinux` and `yt-dlp_musllinux_aarch64` (musl 1.2+) alongside the
  glibc builds, so the one non-Go executable in the image runs on Alpine
  ([ADR-0018](0018-pin-ytdlp-by-version-and-hash.md)).
- **Verified:** distroless images "do not contain package managers, shells or any other programs you would
  expect to find in a standard Linux distribution". PUID/PGID requires a shell, `chown` and a privilege-drop
  helper, so distroless and PUID/PGID are mutually exclusive.
- NAS users expect PUID/PGID; without it they get "permission denied" reports they cannot fix from the NAS
  UI, because the files are root-owned. The base must also package an extractor and a JavaScript runtime.

## Considered Options

- **Option A** — `alpine:3.22` plus `su-exec`, with an entrypoint that applies PUID/PGID/UMASK/TZ and drops.
- **Option B** — `gcr.io/distroless/static-debian13:nonroot`, no entrypoint, `user: "1000:1000"` in compose.
- **Option C** — `debian:trixie-slim` plus `gosu`, the same entrypoint as Option A.

## Decision Outcome

Chosen option: **Option A, `alpine:3.22` with `su-exec`**, because it is the only option that keeps the
PUID/PGID contract NAS users depend on while staying near the 2 MB base — and the two reasons previously
given against Alpine (musl breaking a cgo binary, musl breaking yt-dlp) are both verified not to apply here.

The runtime layer is exactly `apk add --no-cache su-exec ca-certificates tzdata 7zip nodejs`: `su-exec`
performs the privilege drop, `ca-certificates` for HTTPS indexers, `tzdata` for `TZ`, `7zip` for
`/usr/bin/7zz` used by auto-extract, and `nodejs` as the JavaScript runtime `yt-dlp-ejs` requires — deno is
upstream's recommendation but does not build reliably on musl. **Python is never installed.** Entrypoint
sequence and the `user:` escape hatch: [`../10-deployment-and-compose.md`](../10-deployment-and-compose.md).

### Consequences

- Good, because the operator sets `PUID`/`PGID` from `id $user` and their downloads are immediately visible
  and writable over SMB, which is the whole reason this convention exists.
- Good, because `UMASK=002` yields `775` directories and `664` files, matching the `chmod -R a=,a+rX,u+w,g+w`
  bootstrap the same guides publish, so dl-tool and the engine containers can write each other's files.
- Bad, because the image runs as root until the final `exec su-exec`, so the entrypoint is security-relevant
  code. It never recursively chowns `/data`, it skips the drop when the effective UID is already non-root,
  and it warns rather than crashing when `PUID=0`.
- Bad, because musl's allocator is reported slower than glibc's under multithreaded load. This is accepted:
  the reports are secondary and their magnitudes are **UNVERIFIED**, and dl-tool's hot path is network and
  disk I/O, not allocation.
- Neutral, because `/custom-cont-init.d` and `DOCKER_MODS` are not implemented even though the same
  ecosystem popularised them — both execute third-party code fetched at runtime
  ([ADR-0010](0010-never-execute-third-party-definitions.md)).

### Confirmation

Every dependency the image claims must be present, and the drop must actually happen:

```bash
make docker-build
docker run --rm --entrypoint sh ghcr.io/l-k-m/dl-tool:dev -c 'command -v su-exec 7zz node yt-dlp'
docker run -d --name dltool-uidcheck -e PUID=1234 -e PGID=1234 ghcr.io/l-k-m/dl-tool:dev && docker top dltool-uidcheck
```

Expected: four absolute paths, then a `dl-tool` process owned by UID `1234`. File ownership is asserted end
to end by [NFR-025](../02-requirements.md#nfr-025-run-unprivileged-with-the-operators-uid-and-gid), T093.

## Pros and Cons of the Options

### Option A - alpine:3.22 + su-exec

- Good, because `su-exec`, `7zip` and `nodejs` are all packaged, so one `apk add` line covers every runtime
  dependency, and Alpine's release cadence keeps them current.
- Good, because the static Go binary makes the libc question moot, so the usual Alpine objection does not
  apply to dl-tool's own code.
- Bad, because a future glibc-only dependency would force a base change, and no compile-time gate catches
  that before the image is run.

### Option B - distroless static, non-root by construction

- Good, because it is the smallest attack surface available: no shell, so no interactive post-exploitation
  primitive.
- Bad, because it cannot implement PUID/PGID at all — no shell, no `chown`, no `su-exec` — so the operator
  must `chown` host directories by hand before first start and again whenever they add one.
- Bad, because hosting yt-dlp's JavaScript runtime abandons the "static, nothing else" premise that
  justifies the image at all.

### Option C - debian:trixie-slim + gosu

- Good, because glibc removes every musl compatibility question, including for future non-Go dependencies,
  and it was the research corpus's own recommendation for a PUID-capable image.
- Bad, because it is roughly 30 MB before packages against Alpine's ~2 MB, for no benefit dl-tool can name:
  the binary is static and yt-dlp ships musl builds.
- Bad, because `apt-get` layers need explicit list cleanup to stay small — one more step to get wrong.

## More Information

- Research: `deployment.md` §2.1, §2.2, §7.4 — summarised in
  [`../16-prior-art-and-research.md`](../16-prior-art-and-research.md).
- Depends on this decision: [`../10-deployment-and-compose.md`](../10-deployment-and-compose.md) and
  [`../11-config-reference.md`](../11-config-reference.md).
- The mount the entrypoint deliberately never chowns is [ADR-0012](0012-single-data-mount.md).
