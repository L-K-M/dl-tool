# T096 — Redact secrets in logs and serve the system log

| Field | Value |
|---|---|
| **ID** | T096 |
| **Milestone** | M7 |
| **Status** | done |
| **Depends on** | T004, T010, T024, T092 |
| **Blocks** | T121 |
| **Parallel-safe** | no — it edits `internal/obs/log.go` and `internal/api/server.go` |
| **Implements** | [FR-151](../02-requirements.md#fr-151-expose-system-logs-with-secrets-redacted), [NFR-016](../02-requirements.md#nfr-016-keep-api-tokens-revocable-and-out-of-the-logs) |
| **Decisions** | [ADR-0013](../decisions/0013-mandatory-built-in-authentication.md), [ADR-0002](../decisions/0002-go-for-the-backend.md) |
| **Est. size** | 2 new files, ~400 LOC |

## Goal
Every `log/slog` record is redacted **before it is stored**, so stdout, the file under `<CONFIG_DIR>/logs/`
and `GET /api/v1/system/logs` all show the same redacted text. The endpoint returns the newest records
first, cursor-paginated and filterable by level and time.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/17-operations-and-runbook.md` §6 Log redaction](../17-operations-and-runbook.md#6-log-redaction) — the eight never-logged classes and the before-storage rule.
2. [`docs/05-api-contract.md` §13 System endpoints](../05-api-contract.md#13-system-endpoints) — the `GET /system/logs` query, body and redaction placeholder.
3. [`docs/14-conventions.md` §3 Logging](../14-conventions.md#3-logging) and [§3.1 Standard attribute keys](../14-conventions.md#31-standard-attribute-keys).
4. [`docs/11-config-reference.md` §6 Secrets](../11-config-reference.md#6-secrets) — `secure.Secret` and the `"__redacted__"` placeholder.
5. [`docs/05-api-contract.md` §1.4 Cursor pagination](../05-api-contract.md#14-cursor-pagination).

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/obs/logsink.go` | create | `RedactAttr`, `RedactURL`, the recording handler and the file sink. |
| `internal/obs/logsink_test.go` | create | Redaction cases per class, ring wraparound and level filtering. |
| `internal/obs/log.go` | edit | Wire `ReplaceAttr`, wrap the handler in the recorder, tee to the file sink. |
| `internal/api/system.go` | edit | Add the `GET /system/logs` handler to T091's file. |
| `internal/api/server.go` | edit | Register `get-system-logs`. |

No other file may be modified.

## Interface contract

```go
package obs

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"
)

// Placeholder is the literal that replaces every redacted value, in logs and in API responses.
const Placeholder = "__redacted__"

// RedactedAttrKeys are attribute keys whose value is always replaced, whatever its type.
var RedactedAttrKeys = []string{
	"authorization", "cookie", "x-api-key", "api_key", "apikey", "token", "passkey",
	"password", "secret", "session_key", "csrf_key",
}

// RedactAttr is the slog.HandlerOptions.ReplaceAttr function. It replaces any attribute whose key
// is in RedactedAttrKeys, any value of type secure.Secret, and rewrites any string value that
// parses as a URL through RedactURL.
func RedactAttr(groups []string, a slog.Attr) slog.Attr

// RedactURL strips userinfo and rewrites the query parameters apikey, api_key, token and passkey
// to Placeholder. A value that does not parse as a URL is returned unchanged.
//
//	RedactURL("https://indexer.example.org/api?t=search&apikey=abc123")
//	  == "https://indexer.example.org/api?t=search&apikey=__redacted__"
//	RedactURL("ftp://user:pw@host/f.iso") == "ftp://__redacted__@host/f.iso"
func RedactURL(raw string) string

// Record is one stored log line, already redacted.
type Record struct {
	At    time.Time      `json:"at"`
	Level string         `json:"level"` // debug | info | warn | error
	Msg   string         `json:"msg"`
	Attrs map[string]any `json:"attrs"`
}

// Recorder is a slog.Handler that forwards to next and keeps the newest capacity records in a ring.
type Recorder struct {
	next slog.Handler
	mu   sync.RWMutex
	ring []Record
	head int
	n    int
}

func NewRecorder(next slog.Handler, capacity int) *Recorder

// Since returns up to limit records newer than the cursor, newest first, at or above minLevel.
// cursor is the At value of the last record of the previous page; a zero cursor starts at the newest.
// next is the cursor to pass for the following page, or the zero time when the page is the last.
func (r *Recorder) Since(minLevel slog.Level, after time.Time, cursor time.Time, limit int) (recs []Record, next time.Time)

// NewLogWriter returns a writer that tees to stdout and to <configDir>/logs/dl-tool.jsonl,
// truncating the file when it passes maxBytes so an operator's /config cannot fill up.
func NewLogWriter(stdout io.Writer, configDir string, maxBytes int64) (io.Writer, func() error, error)

var _ slog.Handler = (*Recorder)(nil)
var _ = context.Background
```

```go
package api

type SystemLogsInput struct {
	Level  string `query:"level" enum:"debug,info,warn,error" default:"info"`
	Since  string `query:"since"` // RFC 3339
	Limit  int    `query:"limit"  minimum:"1" maximum:"500" default:"100"`
	Cursor string `query:"cursor"`
}

type SystemLogsOutput struct {
	Body struct {
		Items      []obs.Record `json:"items"`
		NextCursor *string      `json:"next_cursor"`
		Total      int          `json:"total"`
	}
}

func (h *SystemHandlers) GetSystemLogs(ctx context.Context, in *SystemLogsInput) (*SystemLogsOutput, error)
```

Worked response:

```json
{"items":[{"at":"2026-09-01T09:41:52Z","level":"info","msg":"engine accepted task",
           "attrs":{"task_id":"tsk_01JKQ8Z9YV6M3P0R2S4T6V8W0X","engine":"qbittorrent",
                    "url":"https://indexer.example.org/api?apikey=__redacted__"}}],
 "next_cursor":null,"total":1}
```

## Steps
1. Create `internal/obs/logsink.go` with `Placeholder`, `RedactedAttrKeys`, `RedactAttr` and `RedactURL`.
   Compare attribute keys case-insensitively.
2. In `RedactAttr`, replace a `secure.Secret` value with `Placeholder` by type switch, not by key name, so a
   secret carried under an unexpected key is still caught.
3. In `RedactURL`, drop `url.Userinfo` entirely and rewrite the four query parameters. Return the input
   unchanged when `url.Parse` fails, so a non-URL string is never corrupted.
4. Implement `Recorder` as a `slog.Handler` wrapper: `Enabled`, `Handle`, `WithAttrs`, `WithGroup`. `Handle`
   materialises the already-redacted record into the ring and then forwards to `next`.
5. Implement `Since` with a level floor, a `since` filter and a `cursor` that is the `At` of the previous
   page's last record. Records are stored newest-last and returned newest-first.
6. Implement `NewLogWriter` returning an `io.MultiWriter` over stdout and the file, plus a close function.
   Create the file with mode `0666` so the container umask decides the result.
7. Edit `internal/obs/log.go`: set `ReplaceAttr: RedactAttr` on the handler options, wrap the handler in
   `NewRecorder(h, 5000)`, and build the writer with `NewLogWriter`.
8. Edit `internal/api/system.go` to add `GetSystemLogs` reading the recorder, and edit
   `internal/api/server.go` to register `get-system-logs`.
9. Create `internal/obs/logsink_test.go` covering: an `Authorization` attribute; a `Cookie` attribute; a
   `secure.Secret` under the key `engine_password`; an indexer URL with `passkey=`; an FTP URL with
   userinfo; a non-URL string left unchanged; a ring of capacity 3 fed 5 records returning the newest 3; and
   `Since` at `warn` excluding `info` records.

## Acceptance criteria
- [x] A log line carrying an indexer URL with `passkey=` stores the marker and never the passkey.
- [x] A `secure.Secret` value is redacted by type even under an unrecognised attribute key.
- [x] Redaction happens before the ring and before the file write, so all three surfaces agree.
- [x] A non-URL string attribute is returned byte-identical.
- [x] The ring never grows past its capacity and returns records newest-first.
- [x] `GET /system/logs?level=warn` excludes `info` and `debug` records.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then `ok` lines for `github.com/L-K-M/dl-tool/internal/obs` and
`.../internal/api`, with `TestRedactAttrByKey`, `TestRedactSecretByType`, `TestRedactURLPasskey`,
`TestRedactURLUserinfo`, `TestRedactLeavesPlainStringAlone`, `TestRecorderRingWraps` and
`TestSystemLogsLevelFilter` all listed as passing. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add the diagnostics-bundle command; doc 17 §7 describes it and no M7 task owns it.
- Do NOT implement `GET /tasks/{id}/events`; T024 owns the per-task event log.
- Do NOT create `.github/copilot-instructions.md`; T097 owns the contributor documentation.
- Do NOT redact at read time: a record must never be stored in clear and cleaned later.
- Do NOT add log rotation beyond the single size cap; a second file and a rotation policy are not in v1.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

`make lint && make test PKG=./internal/...` (2026-09-25, on the task branch):

```
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
go test -race -count=1 ./internal/...
ok  	github.com/L-K-M/dl-tool/internal/api	210.889s
ok  	github.com/L-K-M/dl-tool/internal/config	1.169s
ok  	github.com/L-K-M/dl-tool/internal/engine	35.349s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.226s
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	8.797s
ok  	github.com/L-K-M/dl-tool/internal/engine/ytdlp	1.435s
ok  	github.com/L-K-M/dl-tool/internal/fsx	3.027s
ok  	github.com/L-K-M/dl-tool/internal/jobs	41.610s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.270s
ok  	github.com/L-K-M/dl-tool/internal/rss	20.089s
ok  	github.com/L-K-M/dl-tool/internal/search	8.025s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.436s
ok  	github.com/L-K-M/dl-tool/internal/store	85.501s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.409s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.080s
```

Named tests, verbose (`go test -race -v -run <names> ./internal/obs/`):

```
--- PASS: TestRedactAttrByKey (0.00s)
--- PASS: TestRedactSecretByType (0.00s)
--- PASS: TestRedactURLPasskey (0.00s)
--- PASS: TestRedactURLUserinfo (0.00s)
--- PASS: TestRedactLeavesPlainStringAlone (0.00s)
--- PASS: TestRecorderRingWraps (0.00s)
--- PASS: TestSystemLogsLevelFilter (0.00s)
--- PASS: TestLoggerSurfacesAgree (0.00s)
--- PASS: TestRecorderWithAttrsSharesRing (0.00s)
--- PASS: TestLogWriterTruncatesAtCap (0.00s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/obs	1.078s
```

Scope (`git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort`):

```
api/openapi.json
internal/api/server.go
internal/api/system.go
internal/obs/log.go
internal/obs/logsink.go
internal/obs/logsink_test.go
web/src/api/schema.d.ts
```

The two generated files are the standing §7.1 exception: `get-system-logs` is a new Huma
operation, so `api/openapi.json` and `web/src/api/schema.d.ts` were regenerated with `make gen`.

Two deviations from the interface contract, both forced by the Files table:

- `NewLogger`'s signature is fixed by the `cmd/dl-tool/main.go` call sites, which the table does
  not list, so `log.go` resolves `DLTOOL_CONFIG_DIR` itself (same env var and `/config` default
  `config.Load` owns — `internal/config` exports no accessor) and degrades to the caller's writer
  alone when the file cannot be opened: construction has no error return and the `openapi`
  subcommand's stdout must stay a pure document.
- `Recorder` stores the ring behind a `*recordRing` pointer rather than as value fields, because
  `WithAttrs`/`WithGroup` clones must append to the same store or request-scoped records would
  never reach `GET /system/logs`. `Count` (exported) reports the envelope's `total`, which the
  contract's `Since` signature cannot return.

### Review round (PR #295, GLM 5.3 on `2e62328`)

Applied:

- Ring stored `rec.Message` raw while `ReplaceAttr` redacted the `msg` attribute on stdout and the
  file — `GET /system/logs` would have served credentials stdout never shows. `Handle` now stores
  `RedactURL(rec.Message)`; `TestLoggerSurfacesAgree` logs a credential URL as the message.
- `Since`/`Count` took two separate lock reads, so `items` and `total` could disagree under load;
  the handler now calls the new `Page`, which computes both in one locked pass (`Since` and `Count`
  wrap it — the contract's `Since` signature is unchanged).
- Timestamp-only cursors silently skipped records sharing the boundary instant. `recordRing.push`
  now nudges a tied or backward `At` forward by a nanosecond so stored instants strictly increase
  with arrival; `TestSincePaginatesWithCursor` pages a five-record ring two at a time over two
  shared-tick pairs.
- `MkdirAll(dir, 0o777)` made the log directory world-writable under a permissive umask (symlink
  pre-placement); now `0o755`. The file keeps `0666` — step 6 prescribes that mode so the container
  umask decides, so the reviewer's `0600` suggestion was declined as contradicting the contract.
- `slog.Any` containers bypassed redaction (a `headers` map carrying `Authorization`). `RedactAttr`
  now resolves `LogValuer`s and `redactAny` deep-copies `map[string]any`, `map[string]string`,
  `map[string][]string` (http.Header's shape), `[]any` and `[]string`, applying the same rules at
  every leaf; `error` and `fmt.Stringer` leaves render through `RedactURL`.
- A single write larger than the cap would leave the file over `maxBytes` until the next write;
  `cappedWriter` now drops it. `TestLogWriterDropsOversizedRecord` pins the policy.
- `format=text` wrote tint bytes into `dl-tool.jsonl`. `NewLogger` now pairs the console handler
  with a file-only JSON handler via `fanoutHandler`; `NewLogWriter` keeps its contract on the
  shared `logFileWriter`. `TestLogFileStaysJSONInTextFormat` asserts every file line parses.

Declined:

- `Recorder.Enabled` returning the wrapped level (info suggestion): the ring mirrors what the
  process emits; storing records the operator's level disabled would fill it with invisible spam.
- `logConfigDir` exporting from `internal/config` (info suggestion): already a documented
  deviation; `internal/config` is outside this task's Files table.
- Handler-level tests for `GET /system/logs` (outside-diff suggestion): a new
  `internal/api/*_test.go` is outside the Files table; the criterion is covered one layer down by
  `TestSystemLogsLevelFilter` plus the end-to-end smoke check below. Worth a follow-up task.

`make lint && make test PKG=./internal/...` re-run on the review-fix commit (2026-09-25):

```
0 issues.
ok  	github.com/L-K-M/dl-tool/internal/api	217.719s
ok  	github.com/L-K-M/dl-tool/internal/config	1.755s
ok  	github.com/L-K-M/dl-tool/internal/engine	39.564s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.261s
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	8.903s
ok  	github.com/L-K-M/dl-tool/internal/engine/ytdlp	1.421s
ok  	github.com/L-K-M/dl-tool/internal/fsx	2.897s
ok  	github.com/L-K-M/dl-tool/internal/jobs	44.826s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.234s
ok  	github.com/L-K-M/dl-tool/internal/rss	21.920s
ok  	github.com/L-K-M/dl-tool/internal/search	8.217s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.549s
ok  	github.com/L-K-M/dl-tool/internal/store	88.243s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.405s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.084s
```

Named tests, verbose (`go test -race -v -run <names> ./internal/obs/`):

```
--- PASS: TestRedactAttrByKey (0.00s)
--- PASS: TestRedactNestedContainer (0.00s)
--- PASS: TestRedactSecretByType (0.00s)
--- PASS: TestRedactURLPasskey (0.00s)
--- PASS: TestRedactURLUserinfo (0.00s)
--- PASS: TestRedactLeavesPlainStringAlone (0.00s)
--- PASS: TestRecorderRingWraps (0.00s)
--- PASS: TestSincePaginatesWithCursor (0.00s)
--- PASS: TestSystemLogsLevelFilter (0.00s)
--- PASS: TestLoggerSurfacesAgree (0.00s)
--- PASS: TestRecorderWithAttrsSharesRing (0.00s)
--- PASS: TestLogFileStaysJSONInTextFormat (0.00s)
--- PASS: TestLogWriterTruncatesAtCap (0.01s)
--- PASS: TestLogWriterDropsOversizedRecord (0.00s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/obs	1.080s
```

`make vet` and `make gen` clean — no `api/openapi.json` / `web/src/api/schema.d.ts` drift.

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
