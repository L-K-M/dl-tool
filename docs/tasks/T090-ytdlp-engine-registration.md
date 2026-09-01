# T090 — Register the yt-dlp engine and run the contract suite

| Field | Value |
|---|---|
| **ID** | T090 |
| **Milestone** | M7 |
| **Status** | todo |
| **Depends on** | T028, T088, T089 |
| **Blocks** | T113 |
| **Parallel-safe** | no — it edits `internal/engine/registry.go` and `cmd/dl-tool/main.go` |
| **Implements** | [FR-002](../02-requirements.md#fr-002-route-each-uri-to-an-engine-by-scheme), [FR-143](../02-requirements.md#fr-143-list-engines-and-test-connectivity), [FR-096](../02-requirements.md#fr-096-combine-schedule-global-and-per-task-limits-by-minimum) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md), [ADR-0017](../decisions/0017-exclusive-control-of-engines.md) |
| **Est. size** | 2 new files, ~360 LOC |

## Goal
`ytdlp.Engine` satisfies `engine.Engine`, appears in the registry under the name `ytdlp`, and passes the
shared contract suite. A media URL submitted to `POST /tasks` reaches yt-dlp, streams progress and reaches
`completed`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §1 The `Engine` interface](../06-download-engines.md#1-the-engine-interface) — the exact method set and the capability table.
2. [`docs/06-download-engines.md` §7 yt-dlp adapter](../06-download-engines.md#7-yt-dlp-adapter-internalengineytdlp) — what the adapter may and may not do.
3. [`docs/06-download-engines.md` §10.1 The fan-out call per engine](../06-download-engines.md#101-the-fan-out-call-per-engine) — limits apply at spawn time only.
4. [`docs/06-download-engines.md` §8 Engine ownership](../06-download-engines.md#8-engine-ownership) — a process dl-tool did not start is never surfaced.
5. [`docs/13-testing-and-verification.md` §4 Adapter contract tests](../13-testing-and-verification.md#4-adapter-contract-tests) — the call-site shape and the skip rule.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/ytdlp/engine.go` | create | The `engine.Engine` implementation over `Runner`, `ExtractorCache` and the parsers. |
| `internal/engine/ytdlp/contract_test.go` | create | The `enginetest.RunContract` call site, `//go:build integration`. |
| `internal/engine/registry.go` | edit | One registration line for `ytdlp`. |
| `cmd/dl-tool/main.go` | edit | Construct the adapter from config and install the media matcher on the router. |

No other file may be modified.

## Interface contract

```go
package ytdlp

import (
	"context"
	"log/slog"
	"sync"

	"github.com/L-K-M/dl-tool/internal/engine"
)

// Engine is the media lane. It declares exactly three capabilities: yt-dlp has no per-file model,
// no categories, no tags, no sequential mode and no share limits.
type Engine struct {
	runner     *Runner
	extractors *ExtractorCache
	log        *slog.Logger

	mu      sync.RWMutex
	tasks   map[string]*engine.TaskInfo // keyed by the engine-namespaced id
	events  chan engine.TaskEvent
	limitBS int64 // global bytes/second applied to the next spawn; 0 means unlimited
}

func NewEngine(cfg Config, log *slog.Logger) *Engine

func (e *Engine) Name() string                      // always "ytdlp"
func (e *Engine) Capabilities() []engine.Capability // {CapMediaSite, CapRename, CapPushEvents}, sorted
func (e *Engine) Accepts(uri string) bool           // e.extractors.Match(uri)

func (e *Engine) Connect(ctx context.Context) error
func (e *Engine) Close() error
func (e *Engine) Health(ctx context.Context) (version string, err error)

func (e *Engine) Add(ctx context.Context, req engine.AddRequest) (id string, err error)
func (e *Engine) List(ctx context.Context) ([]engine.TaskInfo, error)
func (e *Engine) Get(ctx context.Context, id string) (engine.TaskInfo, error)
func (e *Engine) Files(ctx context.Context, id string) ([]engine.FileEntry, error)

func (e *Engine) Pause(ctx context.Context, id string) error
func (e *Engine) Resume(ctx context.Context, id string) error
func (e *Engine) Remove(ctx context.Context, id string, deleteData bool) error

// Unsupported optional methods. Each returns engine.ErrNotSupported and mutates nothing.
func (e *Engine) SetFiles(ctx context.Context, id string, selected []int, priorities map[int]int) error
func (e *Engine) SetLocation(ctx context.Context, id, path string) error
func (e *Engine) SetCategory(ctx context.Context, id, category string) error
func (e *Engine) SetShareLimits(ctx context.Context, id string, ratio *float64, seedMinutes *int64) error

// Rename is supported: it rewrites the -o template for the next spawn.
func (e *Engine) Rename(ctx context.Context, id, name string) error

// SetRateLimits stores the download value for the next spawn. A running process is never re-limited,
// per docs/06-download-engines.md §10.1. The upload direction is always ignored.
func (e *Engine) SetRateLimits(ctx context.Context, id string, down, up *int64) error

func (e *Engine) Events(ctx context.Context) (<-chan engine.TaskEvent, error)

var _ engine.Engine = (*Engine)(nil)
```

```go
//go:build integration

package ytdlp_test

// RunContract skips when DLTOOL_YTDLP_PATH names no executable, per doc 13 §4.
func TestYtdlpContract(t *testing.T) {
	enginetest.RunContract(t, func(t *testing.T) engine.Engine { /* ... */ })
}
```

## Steps
1. Create `internal/engine/ytdlp/engine.go` wrapping the `Runner` from T087, the `ExtractorCache` from T088
   and the parsers from T089. Assert `var _ engine.Engine = (*Engine)(nil)` at package scope.
2. Implement `Connect`: load the extractor cache, and log one `warn` line and leave the lane disabled when
   the load failed. `Connect` never returns an error for a missing binary — `Health` reports it instead.
3. Implement `Add`: mint an engine-namespaced id `ytdlp:<ULID>`, call `Runner.Spawn` with the stored
   `limitBS`, start one goroutine that runs `ScanProgress` over stdout into `e.events` and calls
   `ClassifyExit` when the process exits, and record the resulting `TaskInfo` in `e.tasks`.
4. Implement `List` and `Get` from `e.tasks` only. A process `e.tasks` has no entry for is never reported,
   which is how §8 ownership holds for the media lane.
5. Implement `Files` returning exactly one `FileEntry` with `Index: 0`, `Priority: nil` and `Selected: true`,
   sourced from the info document.
6. Implement `Pause` as `Runner.Cancel` plus `StatePaused`, and `Resume` as a fresh `Spawn` with the same
   `AddRequest`; the partial file plus the archive file make the respawn a resume.
7. Implement `Remove`: cancel the process, drop the map entry, and delete the recorded output file only when
   `deleteData` is true.
8. Return `engine.ErrNotSupported` from `SetFiles`, `SetLocation`, `SetCategory` and `SetShareLimits`, and
   `engine.ErrNotFound` from every method given an unknown id.
9. Edit `internal/engine/registry.go` to register the adapter under `ytdlp`, and edit `cmd/dl-tool/main.go`
   to construct it from `config.Config` and call `router.SetMediaMatcher(eng.Accepts)`.
10. Create `internal/engine/ytdlp/contract_test.go` with `//go:build integration`, calling
    `enginetest.RunContract`; `t.Skip` when the binary is absent and skip the per-file-priority subtest
    because the capability is not declared.

## Acceptance criteria
- [ ] `Capabilities()` returns exactly `media_site`, `rename` and `push_events`, sorted and stable.
- [ ] `SetFiles`, `SetLocation`, `SetCategory` and `SetShareLimits` return `engine.ErrNotSupported` and change nothing.
- [ ] Every method returns `engine.ErrNotFound` for an id the adapter did not mint.
- [ ] `SetRateLimits` on a running task changes no running process and takes effect on the next spawn.
- [ ] `make test-integration` runs `TestYtdlpContract` and it passes, or skips with a named reason when the binary is absent.
- [ ] `List()` never includes a yt-dlp process dl-tool did not start.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/... && make test-integration
```
Expected: `make lint` prints nothing; `ok` lines for `github.com/L-K-M/dl-tool/internal/engine` and
`.../internal/engine/ytdlp` with `TestCapabilitiesAreExactlyThree`, `TestUnsupportedMethodsReturnErrNotSupported`
and `TestUnknownIDIsNotFound` passing; then `TestYtdlpContract` reported as `PASS` or as `SKIP` naming the
missing binary. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT declare `per_file_select`, `per_file_priority`, `categories`, `tags`, `sequential` or `share_limits`.
- Do NOT add the boot capability probe, the media-lane disable switch or `js_runtime_missing`; T113 owns them.
- Do NOT add a rate-limit flag literal to the argv; T113 confirms and adds it.
- Do NOT change the aria2 or qBittorrent adapters, or `enginetest/contract.go`; T028 owns the suite.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
