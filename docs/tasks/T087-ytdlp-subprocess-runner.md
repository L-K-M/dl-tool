# T087 — Run yt-dlp as a supervised subprocess

| Field | Value |
|---|---|
| **ID** | T087 |
| **Milestone** | M7 |
| **Status** | todo |
| **Depends on** | T005, T016 |
| **Blocks** | T088, T089, T090, T113 |
| **Parallel-safe** | yes — creates `internal/engine/ytdlp/` and touches nothing else |
| **Implements** | [FR-002](../02-requirements.md#fr-002-route-each-uri-to-an-engine-by-scheme), [NFR-015](../02-requirements.md#nfr-015-never-interpolate-configuration-into-a-shell), [NFR-020](../02-requirements.md#nfr-020-execute-no-third-party-code) |
| **Decisions** | [ADR-0002](../decisions/0002-go-for-the-backend.md), [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md), [ADR-0018](../decisions/0018-pin-ytdlp-by-version-and-hash.md) |
| **Est. size** | 2 new files, ~330 LOC |

## Goal
`internal/engine/ytdlp` starts, tracks and cancels one `yt-dlp` OS process per task. The argument vector is
built as a Go slice and handed to `exec.CommandContext`; no shell is involved and the submitted URI is always
the last element. Nothing yet parses the process output.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §7.1 Subprocess only, never in-process](../06-download-engines.md#71-subprocess-only-never-in-process) — the verbatim argument list.
2. [`docs/06-download-engines.md` §7.5 Dedup with `--download-archive`](../06-download-engines.md#75-dedup-with---download-archive) — where the archive file lives.
3. [`docs/06-download-engines.md` §1 The `Engine` interface](../06-download-engines.md#1-the-engine-interface) — `AddRequest`, the id namespace, the error sentinels.
4. [`docs/11-config-reference.md` §2 `DLTOOL_` variables (application)](../11-config-reference.md#2-dltool_-variables-application) — `DLTOOL_YTDLP_PATH`, `DLTOOL_JS_RUNTIME_PATH`, `DLTOOL_CONFIG_DIR`.
5. [`docs/14-conventions.md` §2.3 Signatures](../14-conventions.md#23-signatures) and [§3 Logging](../14-conventions.md#3-logging).

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/ytdlp/runner.go` | create | `Runner`, `Config`, `Argv`, process registry, spawn and cancel. |
| `internal/engine/ytdlp/runner_test.go` | create | Argv table tests, injection cases, cancel and exit propagation. |

No other file may be modified.

## Interface contract

```go
package ytdlp

import (
	"context"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/L-K-M/dl-tool/internal/engine"
)

// DefaultOutputTemplate is the -o value used when AddRequest.Filename is empty.
const DefaultOutputTemplate = "%(title)s [%(id)s].%(ext)s"

// InfoJSONName is the per-task file --print-to-file writes the final info document to.
const InfoJSONName = ".dl-tool-info.json"

// Config is the resolved runtime configuration of the media lane.
type Config struct {
	BinaryPath    string        // DLTOOL_YTDLP_PATH, default /usr/local/bin/yt-dlp
	JSRuntimePath string        // DLTOOL_JS_RUNTIME_PATH, default /usr/bin/node
	ArchiveDir    string        // <DLTOOL_CONFIG_DIR>/archives
	SpawnTimeout  time.Duration // hard ceiling on one process; 0 means no ceiling
}

// Argv builds the argument vector for one submission. It never returns a shell string.
// The URI is always the final element and is never interpolated into any other argument.
// rateLimitBytesPerSecond of 0 means unlimited and adds no flag.
func Argv(cfg Config, req engine.AddRequest, archivePath, infoJSONPath string, rateLimitBytesPerSecond int64) []string

// Proc is one live yt-dlp process.
type Proc struct {
	ID        string // engine-namespaced, e.g. "ytdlp:01JB0Q7M8WQ0F1R2S3T4U5V6W7"
	Cmd       *exec.Cmd
	SaveDir   string
	InfoPath  string
	StartedAt time.Time
	cancel    context.CancelFunc
}

// Runner owns every live process. It is safe for concurrent use.
type Runner struct {
	cfg  Config
	log  *slog.Logger
	mu   sync.Mutex
	live map[string]*Proc
}

func New(cfg Config, log *slog.Logger) *Runner

// Spawn starts one process for req under the caller-supplied engine-namespaced id and returns
// immediately. Stdout is returned as a line reader for T089; stderr is captured into a bounded buffer.
func (r *Runner) Spawn(ctx context.Context, id string, req engine.AddRequest, rateLimitBytesPerSecond int64) (*Proc, error)

// Cancel kills the process for id via its context. It is idempotent and returns
// engine.ErrNotFound when id is unknown.
func (r *Runner) Cancel(id string) error

// Wait blocks until the process for id exits and returns its exit code. A signalled process
// reports code -1. It returns engine.ErrNotFound when id is unknown.
func (r *Runner) Wait(id string) (exitCode int, err error)

// Live returns the ids of every running process, sorted.
func (r *Runner) Live() []string

// ArchivePath returns <ArchiveDir>/<id>.txt with the engine prefix stripped from id.
func (r *Runner) ArchivePath(id string) string
```

`Argv` emits exactly these elements, in this order, per
[`docs/06-download-engines.md` §7.1](../06-download-engines.md#71-subprocess-only-never-in-process):

```
--no-colors --newline --no-playlist
--paths <req.SaveDir>
--output <req.Filename or DefaultOutputTemplate>
--download-archive <archivePath>
--progress-template <the download: template of doc 06 §7.3>
--print-to-file %()j <infoJSONPath>
--no-simulate
<uri>
```

## Steps
1. Create `internal/engine/ytdlp/runner.go` with the package comment naming
   [ADR-0018](../decisions/0018-pin-ytdlp-by-version-and-hash.md) and the rule that `-U` and `--update-to`
   are never invoked.
2. Implement `Argv` exactly as above. Build a `[]string`; never call `strings.Join`, `sh`, `bash` or
   `exec.Command` with a single composed string.
3. Take the progress template string verbatim from
   [`docs/06-download-engines.md` §7.3](../06-download-engines.md#73-reading-progress-files-not-stdout);
   T089 parses what it emits, so it must match character for character.
4. Implement `New` and `ArchivePath`: create `cfg.ArchiveDir` with mode `0777` so the process umask decides
   the result, per [`docs/10-deployment-and-compose.md` §4](../10-deployment-and-compose.md#4-puid-pgid-umask-tz).
5. Implement `Spawn`: derive a cancellable context, apply `SpawnTimeout` when non-zero, call
   `exec.CommandContext(ctx, cfg.BinaryPath, args...)`, set `Cmd.Dir` to `req.SaveDir`, attach a
   `StdoutPipe` and a bounded stderr buffer of 64 KiB, `Start()`, and record the `Proc` in `live`.
6. Set `Cmd.Env` to a minimal explicit slice carrying `PATH`, `HOME`, `TZ` and nothing else, so no
   inherited variable reaches the child.
7. Implement `Cancel`: call the stored `cancel`, wait for the process with a 10 s grace, delete the entry
   and log at `info` with the `task_id` attribute of [`docs/14-conventions.md` §3.1](../14-conventions.md#31-standard-attribute-keys).
8. Implement `Wait` and `Live`; remove the entry from `live` once `Wait` returns.
9. Create `internal/engine/ytdlp/runner_test.go` with: an `Argv` golden slice for a plain request; an
   `Argv` case where `Filename` is set; an `Argv` case asserting the URI is `args[len(args)-1]`; injection
   cases for the URIs `https://example.org/v?a=1;rm -rf /` and `$(id)` asserting they appear as one
   unmodified element and that no element contains `sh` or `-c`; a `Spawn` case against a stub script that
   sleeps, then `Cancel`, asserting `Wait` returns within 10 s and `Live()` is empty.

## Acceptance criteria
- [ ] `Argv` places the submitted URI last and leaves it byte-identical to the input.
- [ ] No source line in the package contains `sh -c`, `bash -c`, `exec.Command(` with a composed string, or `os/exec.LookPath` on a user value.
- [ ] `Spawn` returns before the process exits and registers the id in `Live()`.
- [ ] `Cancel` on a running id terminates it and is a no-op the second time.
- [ ] `Cancel` and `Wait` on an unknown id return `engine.ErrNotFound`.
- [ ] No source line invokes `-U` or `--update-to`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/engine/ytdlp/...
```
Expected: `make lint` prints nothing, then `ok  	github.com/L-K-M/dl-tool/internal/engine/ytdlp` followed by
its elapsed time, with `TestArgvPutsURILast`, `TestArgvRejectsShellComposition`, `TestSpawnAndCancel` and
`TestCancelUnknownIDIsNotFound` all listed as passing. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT parse stdout, the progress lines or the info JSON; T089 owns `parse.go`.
- Do NOT enumerate extractors or implement `Accepts`; T088 owns `extractors.go`.
- Do NOT implement the `engine.Engine` methods or register the engine; T090 owns that.
- Do NOT add the capability probe or emit `js_runtime_missing`; T113 owns them. The image pin is T093's.
- Do NOT add a `--limit-rate` style flag literal: the flag name is unconfirmed and T113 owns it. Accept the
  `rateLimitBytesPerSecond` argument and, for now, emit no flag for it.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
