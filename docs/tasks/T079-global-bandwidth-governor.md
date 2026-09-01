# T079 — Fan global rate limits out to every engine

| Field | Value |
|---|---|
| **ID** | T079 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T016, T019, T027, T037 |
| **Blocks** | T080, T081, T082, T110, T118 |
| **Parallel-safe** | no — it also edits the shared files `cmd/dl-tool/main.go`, `internal/store/settings.go` |
| **Implements** | [FR-090](../02-requirements.md#fr-090-enforce-global-rate-limits-in-bytes-per-second) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md), [ADR-0017](../decisions/0017-exclusive-control-of-engines.md) |
| **Est. size** | 2 new files, ~260 LOC |

## Goal
One `Governor` holds the current global download and upload limits in bytes per second and pushes them to
every registered engine through `Engine.SetRateLimits`, reading each engine back to confirm the value
landed. `0` means unlimited. No KB/s value exists anywhere in the code path.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §10 Bandwidth precedence and fan-out](../06-download-engines.md#10-bandwidth-precedence-and-fan-out)
2. [`docs/06-download-engines.md` §10.1 The fan-out call per engine](../06-download-engines.md#101-the-fan-out-call-per-engine)
3. [`docs/06-download-engines.md` §1 The `Engine` interface](../06-download-engines.md#1-the-engine-interface)
4. [`docs/11-config-reference.md` §5 Database-backed settings](../11-config-reference.md#5-database-backed-settings)
5. [`docs/02-requirements.md` FR-090](../02-requirements.md#fr-090-enforce-global-rate-limits-in-bytes-per-second)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/bandwidth.go` | create | `Governor`, `Limits`, `ApplyGlobal` and the read-back check. |
| `internal/engine/bandwidth_test.go` | create | Fan-out, unlimited, read-back-mismatch and partial-failure cases. |
| `internal/store/settings.go` | modify | Add typed `GetInt64` and `SetInt64` over the `settings` table. |
| `cmd/dl-tool/main.go` | modify | Construct the governor and apply the stored limits at boot. |

No other file may be modified.

## Interface contract

```go
package engine

// Limits is one direction pair in bytes per second. Zero means unlimited. There is no KB/s
// representation anywhere in dl-tool: doc 04 §1.4 makes bytes the only unit.
type Limits struct {
	Down int64
	Up   int64
}

// Mode is the active schedule cell's meaning. T081 sets it; T079 only needs Default.
type Mode string

const (
	ModeNoDownload  Mode = "no_download"
	ModeDefault     Mode = "default"
	ModeAlternative Mode = "alternative"
)

// Governor owns the global bandwidth state and is the only caller of Engine.SetRateLimits with an
// empty id. It is safe for concurrent use.
type Governor struct{ /* reg *Registry; store *store.Store; mu sync.Mutex; current Limits */ }

func NewGovernor(reg *Registry, st *store.Store) *Governor

// Current returns the limits last applied.
func (g *Governor) Current() Limits

// ApplyGlobal pushes l to every registered, enabled engine and then reads each engine back to
// confirm. A read-back mismatch is logged at warn with the requested and observed values and does
// not fail the call. An engine returning ErrNotSupported is skipped. Errors from individual
// engines are joined with errors.Join so one unreachable daemon never blocks the others.
func (g *Governor) ApplyGlobal(ctx context.Context, l Limits) error

// LoadAndApply reads download_rate_limit and upload_rate_limit from settings and calls ApplyGlobal.
// It is called once at boot and again after any settings write that touches either key.
func (g *Governor) LoadAndApply(ctx context.Context) error
```

Per-engine calls, reproduced from [`06-download-engines.md` §10.1](../06-download-engines.md#101-the-fan-out-call-per-engine):
`aria2.changeGlobalOption` with `max-overall-download-limit` and `max-overall-upload-limit`;
`POST /api/v2/transfer/setDownloadLimit` and `.../setUploadLimit` with the form field `limit`;
for yt-dlp the value is applied to the argument vector at the next spawn.

```go
package store

// GetInt64 reads one settings key as an integer, returning def when the row is absent.
func (s *SettingsStore) GetInt64(ctx context.Context, key string, def int64) (int64, error)

// SetInt64 upserts one settings key as an integer.
func (s *SettingsStore) SetInt64(ctx context.Context, key string, v int64) error
```

## Steps
1. Create `internal/engine/bandwidth.go` with `Limits`, `Mode`, `Governor`, `NewGovernor`, `Current`,
   `ApplyGlobal` and `LoadAndApply`.
2. Implement `ApplyGlobal` as a loop over `Registry` entries, calling `SetRateLimits(ctx, "", &down, &up)`
   and skipping an engine that returns `ErrNotSupported`.
3. Read each engine back — `GET /api/v2/transfer/info` for qBittorrent, `aria2.getGlobalStat` for aria2 —
   and log a warn with both numbers on a mismatch, without returning an error.
4. Join per-engine errors with `errors.Join` and wrap each with the engine name, so one unreachable daemon
   is reported without hiding the others.
5. Never call `transfer/toggleSpeedLimitsMode` or `transfer/speedLimitsMode`: dl-tool always pushes one
   absolute value it computed itself.
6. Add `GetInt64` and `SetInt64` to `internal/store/settings.go` with an explicit column list and an upsert.
7. Edit `cmd/dl-tool/main.go` to construct the governor after the registry and call `LoadAndApply` in
   `OnStart`, before the HTTP listener accepts traffic.
8. Create `internal/engine/bandwidth_test.go` with a fake `Engine`: assert `1048576` reaches every engine;
   assert `0` is pushed as unlimited rather than skipped; assert an `ErrNotSupported` engine is skipped and
   the others still receive the value; assert a read-back mismatch logs and does not error; assert one
   unreachable engine still lets the other be set and appears in the joined error.
9. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `ApplyGlobal` calls `SetRateLimits(ctx, "", …)` on every enabled engine, aria2 included.
- [ ] `0` is applied as unlimited on every engine, never silently skipped.
- [ ] Every value crossing the package boundary is bytes per second; no KB/s conversion exists.
- [ ] A read-back mismatch produces a warn log and no error.
- [ ] One unreachable engine does not prevent the others from being set.
- [ ] `LoadAndApply` runs at boot and the engines carry the stored limits before the listener starts.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/engine/... ./internal/store/..." && echo BANDWIDTH_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/engine` and `ok  github.com/L-K-M/dl-tool/internal/store`,
with `TestApplyGlobalReachesEveryEngine`, `TestZeroMeansUnlimited`, `TestNotSupportedEngineSkipped`,
`TestReadBackMismatchLogsOnly` and `TestUnreachableEngineJoinedError` each reported as `--- PASS`. The final
line of stdout is exactly `BANDWIDTH_OK`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement the 168-cell grid, its storage or its endpoints; T080 owns them.
- Do NOT implement the per-minute evaluation, the alternative-speed switch or the pause behaviour; T081
  owns them.
- Do NOT implement the `min()` precedence chain across schedule, global and per-task limits; T110 owns it.
- Do NOT apply a per-task limit; T082 owns `ApplyTask`.
- Do NOT add `GET`/`PATCH /settings`; T092 owns the settings endpoints.
- Do NOT touch a transfer dl-tool did not create; ADR-0017 makes foreign transfers invisible and unmanaged.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
