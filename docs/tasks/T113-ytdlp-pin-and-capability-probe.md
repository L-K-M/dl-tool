# T113 — Enforce the yt-dlp pin and probe the runtime at boot

| Field | Value |
|---|---|
| **ID** | T113 |
| **Milestone** | M7 |
| **Status** | todo |
| **Depends on** | T090, T093, T097 |
| **Blocks** | — |
| **Parallel-safe** | no — it edits the shared `Dockerfile` |
| **Implements** | [FR-143](../02-requirements.md#fr-143-list-engines-and-test-connectivity), [NFR-020](../02-requirements.md#nfr-020-execute-no-third-party-code), [NFR-028](../02-requirements.md#nfr-028-harden-the-release-supply-chain) |
| **Decisions** | [ADR-0018](../decisions/0018-pin-ytdlp-by-version-and-hash.md), [ADR-0011](../decisions/0011-alpine-runtime-with-puid-pgid.md) |
| **Est. size** | 0 new files, ~260 LOC |

## Goal
The image installs one exact `yt-dlp_musllinux` build verified by SHA-256, self-update is never invoked, and
a boot probe records the resolved yt-dlp and JavaScript-runtime versions. A missing JavaScript runtime raises
the `js_runtime_missing` task-event code, disables the media lane and shows a warning instead of failing
downloads silently.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §7.6 Packaging, pinning and the boot capability probe](../06-download-engines.md#76-packaging-pinning-and-the-boot-capability-probe) — the five packaging rules and the two-row probe table.
2. [`docs/10-deployment-and-compose.md` §5 `Dockerfile`](../10-deployment-and-compose.md#5-dockerfile) — the `ytdlp` stage, the three `ARG` lines and the `TARGETARCH` case.
3. [`docs/12-security-and-threat-model.md` §8.1 yt-dlp: freshness versus unreviewed code](../12-security-and-threat-model.md#81-yt-dlp-freshness-versus-unreviewed-code) — why `-U` is never invoked.
4. [`docs/11-config-reference.md` §2 `DLTOOL_` variables (application)](../11-config-reference.md#2-dltool_-variables-application) — `DLTOOL_YTDLP_PATH` and `DLTOOL_JS_RUNTIME_PATH`.
5. [`docs/06-download-engines.md` §10.1 The fan-out call per engine](../06-download-engines.md#101-the-fan-out-call-per-engine) — the rate limit applies at spawn time and its flag is confirmed here.

## Files
| Path | Action | Purpose |
|---|---|---|
| `Dockerfile` | edit | Refresh the three pin `ARG` values to the newest stable release. |
| `internal/engine/ytdlp/runner.go` | edit | Add `Probe`, add the confirmed rate-limit flag to `Argv`. |
| `internal/engine/ytdlp/engine.go` | edit | `Health` returns the probe result; a failed JS probe disables the lane. |
| `internal/engine/ytdlp/runner_test.go` | edit | Probe, degraded-lane and rate-limit-flag cases. |
| `web/src/locales/en/errors.json` | edit | The English string for the `js_runtime_missing` code. |

No other file may be modified.

## Interface contract

```go
package ytdlp

import "context"

// EventJSRuntimeMissing is the task-event code raised when the JavaScript runtime is absent.
// It is spelled exactly as docs/06-download-engines.md §7.6 and docs/11-config-reference.md name it,
// and it is deliberately not dotted; do not rename it.
const EventJSRuntimeMissing = "js_runtime_missing"

// Capabilities is the result of the boot probe, recorded into engines.version.
type Capabilities struct {
	YtdlpVersion string // e.g. "2026.08.24"; "" when the binary is missing
	JSRuntime    string // e.g. "v24.4.1";    "" when the runtime is missing
	MediaEnabled bool   // false when either probe failed
	Warning      string // human-readable, names the missing binary and its environment variable
}

// Probe runs "<BinaryPath> --version" and "<JSRuntimePath> --version" once, with a 10 s deadline each.
// It never returns an error for a missing binary: the absence is reported in Capabilities.
// It never invokes -U or --update-to.
func (r *Runner) Probe(ctx context.Context) Capabilities

// Caps returns the result of the last Probe. The zero value means the probe has not run.
func (r *Runner) Caps() Capabilities
```

```go
package ytdlp

// Health returns the probed yt-dlp version. It returns engine.ErrUnavailable when the binary is
// absent. A present binary with an absent JavaScript runtime is healthy but disabled: Accepts
// returns false and Add returns engine.ErrUnavailable with the js_runtime_missing warning.
func (e *Engine) Health(ctx context.Context) (version string, err error)
```

The `Dockerfile` change is exactly three lines, and nothing else in the file moves. T093 wrote the first
pin; this task refreshes it and proves the check is real:

```dockerfile
ARG YTDLP_VERSION="<the newest stable tag on the day this task runs>"
ARG YTDLP_SHA256_AMD64="<sha256 of yt-dlp_musllinux>"
ARG YTDLP_SHA256_ARM64="<sha256 of yt-dlp_musllinux_aarch64>"
```

`web/src/locales/en/errors.json` gains one key:

```json
{ "js_runtime_missing": "No JavaScript runtime was found at {{path}}. Media downloads are disabled. Set DLTOOL_JS_RUNTIME_PATH or install nodejs in the image." }
```

<!-- UNVERIFIED: doc 06 §10.1 records that the yt-dlp rate-limit flag was not read verbatim from the pinned
     build. Confirm it with `<binary> --help` at step 4 and paste the matching help line under "## Evidence". -->

## Steps
1. Resolve the newest stable `yt-dlp` release tag, download `yt-dlp_musllinux` and
   `yt-dlp_musllinux_aarch64` from that tag, and record both `sha256sum` outputs.
2. Edit the three `ARG` lines in `Dockerfile`. Change nothing else; the weekly job of T097 rewrites exactly
   these lines and a reformatted file would break its `sed`.
3. Build once with a deliberately wrong `--build-arg YTDLP_SHA256_AMD64` and confirm the build **fails** at
   `sha256sum -c -`; then build with the recorded values and confirm it prints `/yt-dlp: OK`. Paste both
   outcomes under `## Evidence`: a pin nobody has seen fail is not a pin.
4. Run `<binary> --help` and identify the flag that caps the download rate. Add it to `Argv` in
   `internal/engine/ytdlp/runner.go`, emitted only when the rate limit is non-zero, as two separate argv
   elements. Record the confirmed flag in a comment naming the version it was read from.
5. Add `Probe` and `Caps` to `runner.go`. Each probe is one `exec.CommandContext` with a 10 s deadline; a
   non-zero exit or a missing file is a recorded absence, never a returned error and never a panic.
6. Store the probe result on the `Runner` behind its mutex so `Caps()` is safe from the HTTP handlers.
7. Edit `internal/engine/ytdlp/engine.go`: call `Probe` from `Connect`, return the yt-dlp version from
   `Health`, and set `MediaEnabled` false when either binary is absent.
8. When `MediaEnabled` is false, make `Accepts` return false so the router falls through to aria2, and make
   `Add` return `engine.ErrUnavailable` wrapping the `Warning`.
9. Emit one `task_events` row with code `js_runtime_missing` and the runtime path in `detail_json` the first
   time a media submission is refused for that reason, and one `warn` log line at boot.
10. Add the English string for `js_runtime_missing` to `web/src/locales/en/errors.json`.
11. Extend `internal/engine/ytdlp/runner_test.go` with: a probe against two stub scripts printing versions;
    a probe with the JS runtime path pointing at a non-existent file asserting `MediaEnabled == false`, a
    non-empty `Warning` naming `DLTOOL_JS_RUNTIME_PATH`, and no error; an `Argv` case asserting the
    rate-limit flag appears for a non-zero limit and is absent for `0`; and a repository grep asserting no
    source line contains `-U` or `--update-to`.

## Acceptance criteria
- [ ] The `Dockerfile` names one exact yt-dlp tag and two SHA-256 values, and a wrong hash fails the build.
- [ ] The image contains no Python interpreter and the binary is the musl build selected by `TARGETARCH`.
- [ ] `Probe` records both versions and neither call is `-U` or `--update-to`.
- [ ] A missing JavaScript runtime leaves the process running, sets `MediaEnabled` false and produces a warning naming `DLTOOL_JS_RUNTIME_PATH`.
- [ ] With the media lane disabled, a media URL routes to aria2 rather than being rejected.
- [ ] `GET /engines` reports the probed yt-dlp version for the `ytdlp` entry.
- [ ] The confirmed rate-limit flag and its `--help` line are pasted under `## Evidence`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/engine/ytdlp/... && make docker-build VERSION=t113
```
Expected: `make lint` prints nothing; `ok  	github.com/L-K-M/dl-tool/internal/engine/ytdlp` with
`TestProbeRecordsBothVersions`, `TestMissingJSRuntimeDisablesLane`, `TestArgvRateLimitFlag` and
`TestNoSelfUpdateFlag` all passing; then the `ytdlp` build stage printing `/yt-dlp: OK` and the build ending
with `naming to ghcr.io/l-k-m/dl-tool:t113`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT rename `js_runtime_missing`; doc 06 §7.6 and doc 11 §2 spell it exactly this way.
- Do NOT install Python or `pip` in the image; the standalone musl binary is the only permitted form.
- Do NOT add an update button, an update job, or any call to `-U` or `--update-to`.
- Do NOT change the release workflow or the weekly bump job; T097 owns them.
- Do NOT restructure the `Dockerfile`: only the three `ARG` values change, one line each, each starting with `ARG `.
- Do NOT substitute deno or bun for `nodejs`; deno does not build reliably on musl.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
