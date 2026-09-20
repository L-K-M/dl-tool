# 0021 - Ship upstream's RAR-capable 7zz, pinned by version and hash

> **Status:** accepted
> **Date:** 2026-09-20
> **Deciders:** repository owner

## Context and Problem Statement

[FR-100](../02-requirements.md#fr-100-auto-extract-the-supported-archive-formats) lists `.rar`
among the six formats auto-extract must handle, and T074's acceptance criterion runs `7zz i`
against the built image and requires RAR in its output. The image installs Alpine's `7zip`
package, which compiles the codec out: `7zz i` on `7zip-24.09-r0` lists 59 formats, and neither
`Rar` nor `Rar5` is among them. Alpine v3.22 ships no codec package to add — the licence on RAR
decompression forbids the repackaged builds distributions would produce, so they drop it.
Upstream's own `7z<ver>-linux-<arch>` tarballs carry RAR, verified against `7z2603-linux-x64`.

## Decision Drivers

- `.rar` is the incumbent archive format in the release ecosystem dl-tool serves; dropping it
  silently caps feature parity with the product this replaces.
- The decoder is a native binary parsing hostile input
  ([ADR-0010](0010-never-execute-third-party-definitions.md),
  [`../12-security-and-threat-model.md` §4](../12-security-and-threat-model.md)): any artefact in
  the image must come from a pinned, hash-verified download, never a runtime fetch.
- `DLTOOL_SEVENZIP_PATH`, the safe recipe and every test all refer to `7zz`; the decision is
  about where that binary comes from, not which CLI runs.

## Considered Options

- **Option A** — Install upstream's static `7zzs` from the pinned `7z<ver>-linux-<arch>` tarball,
  verified by SHA-256 at build time, as `/usr/local/bin/7zz`; drop Alpine's `7zip` package.
- **Option B** — Drop `.rar` from FR-100, `06-download-engines.md` and the T074/T075/T076
  criteria; keep the Alpine package and extract five formats.
- **Option C** — Switch extraction to libarchive's `bsdtar`, which is packaged in Alpine and
  reads RAR without a pinned binary.

## Decision Outcome

Chosen option: **Option A**, because it preserves FR-100's format set with the smallest change
to the plan's shape — same tool, same argv, same `DLTOOL_SEVENZIP_PATH` contract — while adding
the missing codec through exactly the pin-and-verify mechanism
[ADR-0018](0018-pin-ytdlp-by-version-and-hash.md) already establishes for yt-dlp.

Concretely: the Dockerfile fetches `7z${SEVENZIP_VERSION}-linux-${TARGETARCH}.tar.xz` from
upstream's release area, verifies it against `SEVENZIP_SHA256_AMD64`/`SEVENZIP_SHA256_ARM64`
with `sha256sum -c -`, and installs the tarball's static `7zzs` as `/usr/local/bin/7zz` — the
tarball's dynamic `7zz` is glibc-linked and cannot run on `alpine:3.22`. `7zip` leaves the apk
list, `DLTOOL_SEVENZIP_PATH` moves to `/usr/local/bin/7zz`, and `internal/config`'s default
follows. A `.rar` fixture must be checked in for tests: 7-Zip reads RAR but never writes it, so
`t.TempDir()` cannot synthesize one.

### Consequences

- Good, because FR-100's six formats all extract under one binary and the same safe recipe, and
  the pin keeps the image reproducible — a tag always holds the same 7-Zip bytes.
- Bad, because the image carries a second pinned upstream binary to keep fresh; the weekly
  rebuild that bumps yt-dlp's pin owns this one too. Shipping upstream's own tarball is the
  redistribution channel upstream publishes; the licence terms that kept RAR out of Alpine's
  build govern repackaged builds, which this is not.
- Neutral, because this partially supersedes [ADR-0011](0011-alpine-runtime-with-puid-pgid.md):
  `su-exec`, `ca-certificates`, `tzdata` and `nodejs` still come from one `apk add`; 7-Zip no
  longer does.

### Confirmation

The image's `7zz` must be the pinned upstream build and must carry the codec:

```bash
grep -E '^ARG SEVENZIP_(VERSION|SHA256_(AMD64|ARM64))=' Dockerfile
docker build -t dl-tool:dev . && docker run --rm --entrypoint 7zz dl-tool:dev i | grep -iE 'rar'
```

Expected: three `ARG` lines with non-empty values, and `7zz i` output listing `Rar` and `Rar5`.
T074's acceptance criterion pastes this output under its `## Evidence`.

## Pros and Cons of the Options

### Option A - upstream static `7zzs`, pinned

- Good, because every format including RAR extracts under the binary the docs and tests already
  describe, and the SHA-256 check is the only mechanism that detects a tampered download.
- Bad, because it adds a second pinned-binary download and update cadence to the image.

### Option B - drop `.rar`

- Good, because the image stays distro-packaged and the extraction surface shrinks by a format.
- Bad, because it breaks parity for the most common archive type in the tool's target
  ecosystem, and silently — users discover it only when a completed task refuses to extract.

### Option C - libarchive `bsdtar`

- Good, because `bsdtar` is packaged in Alpine and reads RAR/RAR5 without a pinned binary.
- Bad, because it replaces the extraction tool rather than the codec source: different argv,
  a different password interface and a different CVE surface than the safe recipe was written
  against, so every doc and test built on `7zz` semantics would have to be re-derived.

## More Information

- Verified 2026-09-20: `7z2603-linux-x64.tar.xz` SHA-256
  `dc99eff5008f1ab79bd7084c68513701547a808a89502bf4133683535ab3c695` — its `7zzs` is an ET_EXEC
  static binary (no `PT_INTERP`, so it runs under musl) and `7zzs i` lists `Rar` and `Rar5`
  formats with `Rar1`–`Rar5` codecs; `7z2603-linux-arm64.tar.xz` exists for the arm64 build.
  These values seed the Dockerfile pins; later bumps follow the ADR-0018 cadence.
- Resolves the RAR open question in
  [`../06-download-engines.md`](../06-download-engines.md#open-questions); partially supersedes
  the package choice inside [ADR-0011](0011-alpine-runtime-with-puid-pgid.md)'s runtime layer.
- Depends on this decision: [`../10-deployment-and-compose.md`](../10-deployment-and-compose.md)
  §5, [`../11-config-reference.md`](../11-config-reference.md) `DLTOOL_SEVENZIP_PATH`,
  [`../tasks/T074-auto-extract-archives.md`](../tasks/T074-auto-extract-archives.md) and its
  dependents T075–T078, T119.
