# T089 — Parse yt-dlp progress lines and exit codes

| Field | Value |
|---|---|
| **ID** | T089 |
| **Milestone** | M7 |
| **Status** | todo |
| **Depends on** | T087 |
| **Blocks** | T090 |
| **Parallel-safe** | yes — adds `internal/engine/ytdlp/parse.go` only |
| **Implements** | [FR-011](../02-requirements.md#fr-011-maintain-the-canonical-task-state-machine), [NFR-015](../02-requirements.md#nfr-015-never-interpolate-configuration-into-a-shell) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| **Est. size** | 2 new files, ~300 LOC |

## Goal
One `--progress-template` line becomes one `engine.TaskInfo` update and one exit code becomes a task
outcome. Normal stdout is never parsed: only the JSON emitted by the template and the info document written
by `--print-to-file` are read.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §7.3 Reading progress: files, not stdout](../06-download-engines.md#73-reading-progress-files-not-stdout) — the field table and the fallback chain.
2. [`docs/06-download-engines.md` §7.4 Exit codes](../06-download-engines.md#74-exit-codes) — the five codes and their treatment.
3. [`docs/06-download-engines.md` §1 The `Engine` interface](../06-download-engines.md#1-the-engine-interface) — `TaskInfo`, `TaskState`, `TaskEvent`.
4. [`docs/04-data-model.md` §4.2 `tasks.error_code`](../04-data-model.md#42-taskserror_code) — the closed error enum.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/ytdlp/parse.go` | create | Progress-line decoding, info-document decoding, exit-code mapping. |
| `internal/engine/ytdlp/parse_test.go` | create | Table tests over every field, every fallback and every exit code. |

No other file may be modified.

## Interface contract

```go
package ytdlp

import (
	"time"

	"github.com/L-K-M/dl-tool/internal/engine"
)

// Progress is one decoded --progress-template line. Every numeric field is a pointer because
// yt-dlp emits None for an unknown value; see docs/06-download-engines.md §7.3.
type Progress struct {
	Status        string `json:"status"`   // "downloading" | "finished" | "error"; ignore anything else
	Downloaded    *int64 `json:"downloaded"`
	Total         *int64 `json:"total"`
	Estimate      *int64 `json:"est"`
	Speed         *int64 `json:"speed"`
	ETA           *int64 `json:"eta"`
	FragmentIndex *int64 `json:"frag"`
	FragmentCount *int64 `json:"frags"`
	Filename      string `json:"file"` // always present
}

// ParseProgressLine decodes one line. ok is false for a blank line, a non-JSON warning line, or a
// status value outside the three documented ones; the caller then ignores the line.
func ParseProgressLine(line []byte) (p Progress, ok bool)

// Apply folds p into info. TotalBytes falls back Total -> Estimate -> nil; it is never set to 0
// as a guess. State follows: downloading -> StateDownloading, finished -> StateCompleted,
// error -> StateError.
func (p Progress) Apply(info *engine.TaskInfo)

// Info is the subset of the --print-to-file document dl-tool reads.
type Info struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Filename    string `json:"filename"`
	FileSize    *int64 `json:"filesize"`
	FileSizeApx *int64 `json:"filesize_approx"`
	Duration    *int64 `json:"duration"`
	Extractor   string `json:"extractor"`
	WebpageURL  string `json:"webpage_url"`
	Timestamp   *int64 `json:"timestamp"` // Unix seconds
}

// ParseInfoDocument decodes <SaveDir>/.dl-tool-info.json.
func ParseInfoDocument(raw []byte) (Info, error)

// Apply fills Name, ContentPath, TotalBytes and CreatedAt from the final info document.
func (i Info) Apply(info *engine.TaskInfo)

// Outcome is the normalised result of one exited process.
type Outcome struct {
	State        engine.TaskState
	ErrorCode    string // a tasks.error_code value; "" when State is not StateError
	ErrorMessage string
	Retryable    bool
}

// ClassifyExit maps an exit code plus the captured stderr tail to an Outcome, per doc 06 §7.4:
//
//	0   -> completed
//	101 -> completed  (stopped early on purpose, e.g. --break-on-existing)
//	2   -> error, unknown            (log the full argv)
//	100 -> error, engine_unavailable (never retry)
//	1   -> error, private_video when stderr says so, otherwise unknown
//	-1  -> paused    (the process was signalled by Runner.Cancel)
func ClassifyExit(exitCode int, stderrTail string) Outcome

// ScanProgress consumes a reader of newline-delimited template output and emits one TaskEvent per
// accepted line. It closes the returned channel when the reader is exhausted.
func ScanProgress(taskID string, r io.Reader) <-chan engine.TaskEvent

// PercentFallback returns FragmentIndex/FragmentCount as a 0..1 ratio when neither Total nor
// Estimate is known, and -1 when no fragment counters are present either.
func (p Progress) PercentFallback() float64

var _ = time.Second
```

## Steps
1. Create `internal/engine/ytdlp/parse.go` with the structs above and a package comment quoting the upstream
   rule that normal stdout is never parsed.
2. Implement `ParseProgressLine`: `json.Unmarshal` into `Progress`; return `ok=false` on any decode error so
   a yt-dlp warning printed to stdout is skipped instead of breaking the stream.
3. Check `Status` first and return `ok=false` for any value other than `downloading`, `finished` or `error`,
   exactly as the upstream field table instructs.
4. Implement `Progress.Apply` with the documented fallback chain: `Total`, then `Estimate`, then leave
   `TotalBytes` nil. Never write `0` for an unknown size.
5. Implement `PercentFallback` for live, HLS and DASH sources that report neither size.
6. Implement `ParseInfoDocument` and `Info.Apply`; set `ContentPath` from `Filename`, `CreatedAt` from
   `Timestamp` in Unix seconds, and `TotalBytes` from `FileSize` then `FileSizeApx`.
7. Implement `ClassifyExit` with the five documented codes plus `-1` for a signalled process. Map only to
   values that exist in [`docs/04-data-model.md` §4.2](../04-data-model.md#42-taskserror_code).
8. Implement `ScanProgress` over a `bufio.Scanner` with a 1 MiB buffer, emitting `engine.EventProgress`
   for `downloading`, `engine.EventCompleted` for `finished` and `engine.EventError` for `error`.
9. Create `internal/engine/ytdlp/parse_test.go` with inline fixture lines covering: a full line; a line with
   `"total":null` falling back to `est`; a line with both null falling back to fragments; a yt-dlp warning
   line that is skipped; an unknown status that is skipped; each of the six exit codes; and a stderr tail
   containing a private-video message mapping to `private_video`.

## Acceptance criteria
- [ ] A line whose `total` is null and whose `est` is set yields `TotalBytes` equal to `est`.
- [ ] A line with both null leaves `TotalBytes` nil and `PercentFallback` returns the fragment ratio.
- [ ] A non-JSON warning line on stdout is skipped and does not stop the scan.
- [ ] Exit code `101` classifies as `completed`, not as an error.
- [ ] Exit code `100` classifies as `error` with `engine_unavailable` and `Retryable == false`.
- [ ] Exit code `-1` classifies as `paused`.
- [ ] Every `ErrorCode` the mapper can produce appears in doc 04 §4.2.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/engine/ytdlp/...
```
Expected: `make lint` prints nothing, then `ok  	github.com/L-K-M/dl-tool/internal/engine/ytdlp` followed by
its elapsed time, with `TestProgressTotalFallbackChain`, `TestProgressSkipsWarningLine`,
`TestProgressSkipsUnknownStatus`, `TestClassifyExitTable` and `TestInfoDocumentFillsContentPath` all listed
as passing. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT spawn a process or touch `runner.go`; T087 owns process lifecycle.
- Do NOT implement the `engine.Engine` methods or the event loop wiring; T090 owns them.
- Do NOT read or write the `--download-archive` file: yt-dlp owns its contents.
- Do NOT invent an `error_code` value; only the enum in doc 04 §4.2 may be produced.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
