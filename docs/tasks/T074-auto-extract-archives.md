# T074 — Extract completed archives with the safe recipe

| Field | Value |
|---|---|
| **ID** | T074 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T012, T017, T024 |
| **Blocks** | T075, T076, T077, T078 |
| **Parallel-safe** | yes — adds `internal/jobs/postprocess.go` and `internal/jobs/handlers_extract.go` |
| **Implements** | [FR-100](../02-requirements.md#fr-100-auto-extract-the-supported-archive-formats), [FR-102](../02-requirements.md#fr-102-report-extraction-state-progress-and-failures), [FR-106](../02-requirements.md#fr-106-remove-completed-tasks-automatically), [NFR-018](../02-requirements.md#nfr-018-extract-archives-safely) |
| **Decisions** | [ADR-0015](../decisions/0015-db-backed-in-process-job-queue.md), [ADR-0010](../decisions/0010-never-execute-third-party-definitions.md) |
| **Est. size** | 3 new files, ~380 LOC |

## Goal
A completed task whose payload is a `.zip`, `.tar`, `.gz`, `.tgz`, `.rar` or `.7z` archive is extracted by a
`7zz` subprocess run as an argument vector, in three passes, while the task shows `extracting` with a 0–100
progress value. Auto-extract is off by default and a failure sets one of the five `extract_failed*` codes.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/12-security-and-threat-model.md` §4.1 The safe recipe](../12-security-and-threat-model.md#41-the-safe-recipe)
2. [`docs/02-requirements.md` FR-100](../02-requirements.md#fr-100-auto-extract-the-supported-archive-formats)
3. [`docs/04-data-model.md` §3.6 Jobs, schedule and preferences](../04-data-model.md#36-jobs-schedule-and-preferences)
4. [`docs/04-data-model.md` §4.2 `tasks.error_code`](../04-data-model.md#42-taskserror_code)
5. [`docs/11-config-reference.md` §5 Database-backed settings](../11-config-reference.md#5-database-backed-settings)
6. [`docs/14-conventions.md` §4 The `task_events` code vocabulary](../14-conventions.md#4-the-task_events-code-vocabulary)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/jobs/postprocess.go` | create | `OnCompleted`: the extract → move → notify chain and auto-remove. |
| `internal/jobs/handlers_extract.go` | create | The three-pass `7zz` recipe and the `extract` job handler. |
| `internal/jobs/handlers_extract_test.go` | create | Six-format, progress, truncated-archive and zip-slip cases. |
| `internal/store/tasks.go` | modify | Add `SetExtractProgress` and `SetErrorCode`. |

No other file may be modified.

## Interface contract

```go
package jobs

// Chain is the post-processing pipeline. It is enqueued once per task that reaches completed and is
// idempotent on (kind, task_id) as required by ADR-0015.
type Chain struct{ /* store *store.Store; cfg *config.Config */ }

// OnCompleted enqueues the postprocess chain for one task. Auto-extract is skipped when the
// settings key auto_extract is false, which is its default.
func (c *Chain) OnCompleted(ctx context.Context, taskID string) error

// ExtractHandler runs job kind "extract". Registered with the T012 worker pool as
// worker.Register("extract", h.Handle).
type ExtractHandler struct{ /* store *store.Store; sevenzipPath string; passwords PasswordSource */ }

func NewExtractHandler(st *store.Store, sevenzipPath string) *ExtractHandler
func (h *ExtractHandler) Handle(ctx context.Context, job store.Job) error

// Member is one row of pass 1: `7zz l -slt -y -ba <archive>`.
type Member struct {
	Path       string // as declared by the archive
	Size       int64  // declared uncompressed size
	Attributes string // raw Attributes value; a link, device, FIFO, setuid or setgid member is rejected
}

// ListMembers runs pass 1 and returns the declared members. It writes nothing.
func ListMembers(ctx context.Context, sevenzipPath, archivePath string) ([]Member, error)

// Validate applies pass 1's rejection rules to members against the caps below.
// It returns ErrInvalidArchive for a bad member name and ErrCapExceeded for a cap breach.
func Validate(members []Member, archiveSize int64) error

// Caps are the limits of doc 12 §4.1. Zero means the documented default.
type Caps struct {
	TotalUncompressedBytes int64         // min(10 * archive size, 20 GiB)
	MemberCount            int           // 10000
	SingleMemberBytes      int64         // 4 GiB
	Depth                  int           // 32
	WallClock              time.Duration // 30 * time.Minute
}

var (
	ErrInvalidArchive = errors.New("jobs: archive member rejected")
	ErrCapExceeded    = errors.New("jobs: extraction cap exceeded")
	ErrWrongPassword  = errors.New("jobs: no candidate password opened the archive")
)
```

The pass-2 invocation is an exec array, never a shell string
([NFR-015](../02-requirements.md#nfr-015-never-interpolate-configuration-into-a-shell)):

```go
tmp := filepath.Join(root, ".dl-tool-extract-"+ulid.Make().String()) // same root, same filesystem
cmd := exec.CommandContext(ctx, h.sevenzipPath, "x", "-y", "-bd", "-o"+tmp, "-p"+pw, archivePath)
```

```go
package store

// SetExtractProgress writes tasks.unzip_progress (0-100) and holds state at 'extracting'.
func (s *TaskStore) SetExtractProgress(ctx context.Context, taskID string, percent int) error

// SetErrorCode sets tasks.error_code and tasks.error_message. code must be a value of
// docs/04-data-model.md §4.2; an unknown code is a programming error and panics in tests.
func (s *TaskStore) SetErrorCode(ctx context.Context, taskID, code, message string) error
```

Failure mapping: `ErrWrongPassword` → `extract_failed_wrong_password`; `ErrInvalidArchive` → 
`extract_failed_invalid_archive`; `ErrCapExceeded` → `extract_failed_quota_reached`; `fsx.ErrDiskFull` → 
`extract_failed_disk_full`; anything else → `extract_failed`.

`task_events` codes emitted: `postprocess.extract.started`, `postprocess.extract.completed`,
`postprocess.extract.failed`, `postprocess.autoremoved`.

## Steps
1. Create `internal/jobs/postprocess.go` with `Chain` and `OnCompleted`, enqueuing an `extract` job when the
   `auto_extract` settings key is true and the payload matches one of the six extensions.
2. In the same file, after the chain's last job, remove the task row when the `auto_remove_on_complete`
   settings key is true — read it with an explicit `false` default, because it is not seeded by
   `00001_init.sql` — leaving every downloaded file on disk and writing `postprocess.autoremoved`.
3. Create `internal/jobs/handlers_extract.go` with `ListMembers`, `Validate`, `Caps` and `ExtractHandler`.
4. Implement pass 1 exactly as doc 12 §4.1: reject an absolute name, a `..` element, a name failing
   `fsx.SanitiseSegment`, a declared link, device, FIFO, setuid or setgid member, and any cap breach.
5. Implement pass 2 into a fresh `tmp` directory inside the same data root, with the exec array above, a
   `context` deadline of `Caps.WallClock`, and the child's process group killed on expiry.
6. Poll the bytes written into `tmp` once per second; update `unzip_progress` from bytes-written over the
   declared total and abort with `ErrCapExceeded` on a breach. Never recurse into a nested archive.
7. Implement pass 3: `lstat` every entry under `tmp`, abort on any symlink, hardlink, device, FIFO or socket
   or on any path resolving outside `tmp`, force `0644` on files and `0755` on directories, then rename the
   tree into place. On any abort remove `tmp` recursively and leave the destination untouched.
8. Add `SetExtractProgress` and `SetErrorCode` to `internal/store/tasks.go` with explicit column lists.
9. Create `internal/jobs/handlers_extract_test.go`: build one fixture of each of the six formats in
   `t.TempDir()`; assert each extracts; assert `unzip_progress` advances and ends at `100`; assert a
   truncated archive yields `extract_failed_invalid_archive`; assert a member named `../escape` is rejected
   before any file is written; assert the default of `auto_extract` is `false`.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `auto_extract` defaults to `false` and no extraction runs until it is enabled.
- [ ] One archive of each of `.zip`, `.tar`, `.gz`, `.tgz`, `.rar` and `.7z` extracts successfully.
- [ ] The task holds `extracting` with `unzip_progress` advancing from `0` to `100`.
- [ ] A truncated archive sets `error_code` `extract_failed_invalid_archive` and writes nothing.
- [ ] A `../escape` member is rejected in pass 1; the destination directory is unchanged.
- [ ] `7zz` is invoked as an argument vector; the string `sh -c` appears nowhere in the package.
- [ ] With `auto_remove_on_complete` enabled the task row is gone and its files remain.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/jobs/... && echo EXTRACT_OK
```
Expected: `make lint` prints nothing, then `ok  github.com/L-K-M/dl-tool/internal/jobs`, with
`TestExtractsAllSixFormats`, `TestProgressReaches100`, `TestTruncatedArchiveIsInvalid`,
`TestZipSlipMemberRejectedInPassOne` and `TestAutoExtractDefaultsOff` each reported as `--- PASS`. The final
line of stdout is exactly `EXTRACT_OK`. No `FAIL`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT implement the password candidate loop; T075 owns `internal/jobs/passwords.go`.
- Do NOT implement the cross-filesystem move; T076 owns `internal/fsx/move.go`.
- Do NOT send a notification; T077 owns `internal/jobs/handlers_notify.go`.
- Do NOT run a completion hook; T078 owns it.
- Do NOT add `github.com/mholt/archiver/v3` or any other archive library — CVE-2025-3445 is a zip-slip in
  exactly this role. The extractor is the `7zz` subprocess and nothing else.
- Do NOT recurse into an archive found inside an archive.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
