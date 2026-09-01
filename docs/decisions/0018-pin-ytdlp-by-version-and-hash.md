# 0018 - Pin yt-dlp by version and hash; never self-update at runtime

> **Status:** accepted
> **Date:** 2026-09-01
> **Deciders:** repository owner

## Context and Problem Statement

yt-dlp is at once the component that must be updated most often and the one least safe to update
automatically. Upstream says so: stable is "released on a (mostly) monthly schedule" while nightly builds
"publish shortly before midnight UTC on any day that sees changes to the codebase … the recommended channel
for regular users". The corpus split. `engines.md` calls the cadence the dominant operational fact — "a
dl-tool container that pins a yt-dlp version will stop working within weeks" — and recommends daily
self-update. `security.md` concludes the opposite: **no signed release artefacts or signature verification
inside `yt-dlp -U` could be found (UNVERIFIED, likely absent)**, so `-U` is an unreviewed remote code fetch
into a process parsing hostile input.

## Decision Drivers

- `--update` is a code-fetching primitive anyone able to influence configuration can aim; qBittorrent's
  CVE-2024-51774 fetch-an-installer path became remote code execution exactly that way.
- Every artefact reaching an operator should have passed one signed, SBOM'd build; a self-rewriting binary
  has not. Extractor rot is real, so the replacement needs its own cadence.
- yt-dlp needs a JavaScript runtime for full YouTube support. Its README, under "Strongly recommended":
  "A JavaScript runtime/engine like deno (recommended), node.js, bun, or QuickJS is also required to run
  yt-dlp-ejs." That is a second moving dependency, and its absence must be diagnosable.

## Considered Options

- **Option A** — Pin an exact version by SHA-256 at build time, self-update off, weekly CI job bumps the pin.
- **Option B** — Upstream's own advice: install the nightly channel and run `yt-dlp -U` daily in-container.
- **Option C** — Install from PyPI in the image with a hash-pinned wheel, updated by a Renovate pull request.
- **Option D** — Run yt-dlp in its own container on its own release cadence, independent of dl-tool.

## Decision Outcome

Chosen option: **Option A, pin by version and SHA-256 with a weekly image rebuild**, because it answers
`security.md`'s objection without conceding `engines.md`'s point: freshness comes from the release pipeline
rather than the running container, so it stays weekly while every version shipped has passed a signed build.

Concretely: the `ytdlp` build stage downloads `yt-dlp_musllinux` or `yt-dlp_musllinux_aarch64` by
`TARGETARCH` from the pinned release tag and verifies it with `sha256sum -c -`. The `YTDLP_VERSION`,
`YTDLP_SHA256_AMD64` and `YTDLP_SHA256_ARM64` build arguments **are** the pin and the weekly job rewrites
exactly those lines. Python is never installed, dl-tool never invokes `-U` or `--update-to`, and there is
no update button: `docker compose pull` is the update mechanism.

The image ships Alpine's **`nodejs`** as that runtime, not deno, because deno does not build reliably on
musl. A boot probe runs `<DLTOOL_YTDLP_PATH> --version` and `<DLTOOL_JS_RUNTIME_PATH> --version` at
`Connect()` and records both in `engines.version`; a missing runtime raises **`js_runtime_missing`** and
disables the media lane behind a visible warning naming the binary and its variable.

### Consequences

- Good, because the image is reproducible: a tag always holds the same yt-dlp bytes, so "it worked
  yesterday" is checkable. yt-dlp also runs as a subprocess, so an extractor bug — in code evaluating remote
  JavaScript via `yt-dlp-ejs` — cannot take down the web app.
- Bad, because a user hit by an extractor break waits for the next weekly image where `yt-dlp -U` would have
  fixed it in seconds — the whole cost of this decision, accepted deliberately. A CI job that stops firing
  becomes a stale pin; the runbook names the symptom and the fix.
- Neutral, because v1 offers no opt-in nightly channel: it would reintroduce the unreviewed-code path, and
  can be added later without reopening this decision.
- Neutral, because yt-dlp's README states the PyInstaller-bundled executables include GPLv3+ code while the
  PyPI wheel is Unlicense-only: the image redistributes a GPLv3+ program run as a separate process, and the
  release notes must say so ([ADR-0016](0016-relicense-to-apache-2.md)).

### Confirmation

The pin must exist, the binary must match it, and no code path may update it:

```bash
grep -E '^ARG YTDLP_(VERSION|SHA256_(AMD64|ARM64))=' Dockerfile
docker run --rm ghcr.io/l-k-m/dl-tool:dev sh -c '/usr/local/bin/yt-dlp --version && node --version'
grep -rn -e '--update' -e '"-U"' --include='*.go' internal/engine/ytdlp
```

Expected: three `ARG` lines with non-empty values, a version string from each binary, and no output from the
last `grep`. The probe and `js_runtime_missing` are covered by `make test PKG=./internal/engine/ytdlp/...`.

## Pros and Cons of the Options

### Option A - pinned version and hash, weekly rebuild

- Good, because the SHA-256 check is the only mechanism here detecting a tampered download, and the update
  path is one operators know and can roll back: pull a tag.
- Bad, because the weekly rebuild is CI work the project must keep alive.

### Option B - nightly channel with runtime `yt-dlp -U`

- Good, because it is upstream's own advice and gives the fastest fix when an extractor breaks.
- Bad, because it executes new code in a running container with no signature to verify — the unreviewed
  remote fetch the CVE precedent warns about — and behaviour then depends on when the container last
  updated, so a bug report cannot be reproduced from its image tag.

### Option C - hash-pinned PyPI wheel

- Good, because the wheel is Unlicense-only, avoiding the GPLv3+ question the bundled executable raises.
- Bad, because it needs Python in the image, contradicting [ADR-0011](0011-alpine-runtime-with-puid-pgid.md).

### Option D - separate yt-dlp container

- Good, because it isolates the update cadence completely and was the research corpus's own suggestion.
- Bad, because it adds a container, a volume and a protocol for a well-behaved `os/exec` subprocess.

## More Information

- Research: `engines.md` §4.1, §4.5 and `security.md` §7.1 — summarised in
  [`../16-prior-art-and-research.md`](../16-prior-art-and-research.md).
- Depends on this decision: [`../06-download-engines.md`](../06-download-engines.md),
  [`../10-deployment-and-compose.md`](../10-deployment-and-compose.md),
  [`../17-operations-and-runbook.md`](../17-operations-and-runbook.md).
