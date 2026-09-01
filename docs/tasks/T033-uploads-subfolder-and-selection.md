# T033 — Accept uploaded files, the subfolder option and a create-time file selection

| Field | Value |
|---|---|
| **ID** | T033 |
| **Milestone** | M2 |
| **Status** | todo |
| **Depends on** | T020, T031, T032 |
| **Blocks** | — |
| **Parallel-safe** | no — extends `internal/api/tasks.go` and `internal/api/server.go` |
| **Implements** | [FR-005](../02-requirements.md#fr-005-add-tasks-from-an-uploaded-file), [FR-008](../02-requirements.md#fr-008-save-selected-files-in-a-subfolder-named-after-the-list), the create-time half of [FR-007](../02-requirements.md#fr-007-select-and-prioritise-individual-files) |
| **Decisions** | [ADR-0003](../decisions/0003-chi-huma-code-first-openapi.md), [ADR-0012](../decisions/0012-single-data-mount.md) |
| **Est. size** | 2 new files, ~360 LOC |

## Goal
`POST /api/v1/tasks` and `POST /api/v1/tasks/inspect` accept `multipart/form-data` carrying a JSON
`payload` part plus `.torrent`, `.metalink` and `.txt` file parts, honour `create_subfolder` by placing
content in `<destination>/<manifest name>/`, and apply a `select_files` list at creation.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §5.2 `POST /tasks`](../05-api-contract.md#52-post-tasks)
2. [`docs/05-api-contract.md` §5.3 `POST /tasks/inspect`](../05-api-contract.md#53-post-tasksinspect)
3. [`docs/12-security-and-threat-model.md` §3.2 `sanitiseSegment`](../12-security-and-threat-model.md#32-sanitisesegments-string-string)
4. [`docs/06-download-engines.md` §1.1 File priority vocabulary](../06-download-engines.md#11-file-priority-vocabulary)
5. [`docs/06-download-engines.md` §2 Routing table](../06-download-engines.md#2-routing-table)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/submission.go` | create | The shared multipart parser, the `.txt` expander and the subfolder resolver. |
| `internal/api/submission_test.go` | create | Cases for each part kind, each cap and the subfolder path. |
| `internal/api/tasks.go` | modify | Accept the form, apply `create_subfolder` and `select_files`. |
| `internal/api/tasks_inspect.go` | modify | Accept the same form for inspection. |
| `internal/api/server.go` | modify | Declare `multipart/form-data` on both operations. |

No other file may be modified.

## Interface contract

```go
package api

// Submission caps, from 05 §5.2. Exceeding any of them is 413 /problems/payload-too-large, except the
// URI count, which is 422 /problems/validation-failed.
const (
	MaxRequestBytes = 32 << 20 // whole multipart request
	MaxBlobBytes    = 10 << 20 // one decoded .torrent or .metalink
	MaxURIs         = 50       // uris[] plus every line of every .txt part
)

// UploadedFile is one "file" part of the multipart form.
type UploadedFile struct {
	Name  string // as sent by the client, used only for the display name
	Bytes []byte
}

// parseSubmission reads a multipart/form-data body: exactly one "payload" part holding the endpoint's
// JSON body without blob, and zero or more "file" parts. It returns ErrPayloadTooLarge above
// MaxRequestBytes and never buffers more than that.
func parseSubmission(r *http.Request) (payload []byte, files []UploadedFile, err error)

// expandTextList returns one entry per line of a .txt part, dropping empty lines and lines whose first
// non-space character is '#'. Order is preserved and duplicates are kept.
func expandTextList(b []byte) []string

// classifyUpload decides what one part becomes: "torrent", "metalink" or "text", from the sniffed bytes
// first and the filename extension only as a tie-break. An unrecognised part is rejected, never guessed.
func classifyUpload(f UploadedFile) (kind string, err error)

// subfolderDestination returns filepath.Join(destination, sanitiseSegment(manifestName)) when
// createSubfolder is set and the manifest holds more than one file, and destination otherwise. The
// result is re-checked with fsx.ResolveDestination before use.
func subfolderDestination(roots []string, jail, destination, manifestName string, createSubfolder bool) (string, error)

// FileSelectionRequest is one entry of the create body's select_files. It reuses the priority vocabulary
// of 06 §1.1 and is applied to the first multi-file manifest of the submission.
type FileSelectionRequest struct {
	Index    int     `json:"index"    minimum:"0"`
	Selected *bool   `json:"selected,omitempty"`
	Priority *string `json:"priority,omitempty" enum:"skip,normal,high,maximum"`
}

// applySelection turns select_files into the AddRequest fields. It returns 422 material when the routed
// engine does not declare per_file_select, or when an index is outside the manifest.
func applySelection(sel []FileSelectionRequest, fileCount int) (indices []int, priorities map[int]int, err error)
```

Part handling, exactly this table:

| Part | Becomes |
|---|---|
| `payload` | The endpoint's JSON body, without `blob`. Absent means an empty body, not an error. |
| `file` whose bytes parse as bencode | One BitTorrent task per part, routed to `qbittorrent`. |
| `file` whose bytes parse as Metalink | One task per part, routed to `aria2`. |
| `file` whose bytes are UTF-8 text | Every accepted line joins `uris` and is routed by scheme. |
| anything else | One `rejected[]` entry with `/problems/unsupported-media-type`. |

## Steps
1. Create `internal/api/submission.go` with the constants, `parseSubmission`, `expandTextList`,
   `classifyUpload`, `subfolderDestination` and `applySelection`.
2. Read the body with `http.MaxBytesReader(w, r.Body, MaxRequestBytes)` and `multipart.Reader.NextPart`,
   streaming part by part; never call `ParseMultipartForm`, which buffers to a temporary file.
3. Sniff each part: leading `d` plus a bencoded dictionary is a torrent; an XML root of `metalink` is a
   Metalink; otherwise valid UTF-8 with no NUL byte is text. Never trust `Content-Type` from the client.
4. Enforce the caps in order — request size, then per-blob size, then the 50-entry total across `uris`
   and every expanded `.txt` line — and map the first breach to its documented status.
5. Edit `internal/api/tasks.go` so `POST /tasks` accepts both `application/json` and
   `multipart/form-data`, merging file parts into the same submission list the JSON path already builds.
6. Apply `create_subfolder` after the manifest is known: call `subfolderDestination` and pass its result
   as `engine.AddRequest.SaveDir`; record the original request in `tasks.requested_destination` when it
   differs from `tasks.destination`.
7. Apply `select_files` to the first multi-file manifest of the submission: fill
   `AddRequest.SelectFiles`, and pass the priority map through `AddRequest.Extra` only when the routed
   engine declares `per_file_priority`; otherwise return `422` `/problems/validation-failed`.
8. Edit `internal/api/tasks_inspect.go` to call `parseSubmission` too, so the add dialog inspects exactly
   the bytes it will later submit; the inspect endpoint ignores every field except `uris`, `blob` and
   `filename`.
9. Declare the extra request media type on both operations in `internal/api/server.go` so
   `api/openapi.json` documents the form; run `make gen` and commit nothing else it changes.
10. Create `internal/api/submission_test.go` covering: a `.torrent` part becoming one BitTorrent task; a
    two-line `.txt` becoming two URI tasks; a comment line and a blank line being dropped; a 33 MiB
    request returning `413`; 51 lines returning `422`; a `.jpg` part landing in `rejected[]`; and
    `create_subfolder` producing `<destination>/<name>/` with a hostile manifest name sanitised.

## Acceptance criteria
- [ ] One uploaded `.torrent` yields exactly one task on `qbittorrent`.
- [ ] A two-line `.txt` yields two tasks routed by scheme; `#` and blank lines are ignored.
- [ ] A request above 32 MiB is refused without buffering the whole body.
- [ ] `create_subfolder=true` resolves to `<destination>/<sanitised manifest name>/` and the result still
      passes `fsx.ResolveDestination`.
- [ ] `select_files` on an engine without `per_file_select` returns `422`, and no task is created.
- [ ] `POST /tasks/inspect` accepts the identical form and still creates no task.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/api/...
```
Expected: `make lint` prints nothing, then `ok  github.com/L-K-M/dl-tool/internal/api` with
`TestUploadTorrentPart`, `TestUploadTextListExpands`, `TestRequestTooLarge`,
`TestCreateSubfolderSanitisesName` and `TestSelectFilesRejectedOnIncapableEngine` all `PASS`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, plus `api/openapi.json` if `make gen` changed it.
Use `git status`, not `git diff`: a file this task creates is untracked, and `git diff --name-only`
never lists an untracked file.

## Out of scope — do NOT
- Do NOT add a watch folder or scan a directory; T083 owns watch folders.
- Do NOT import anything from another download manager. There is no migration path in this product.
- Do NOT write the uploaded bytes to disk; they go to the engine in memory and are then discarded.
- Do NOT change `PATCH /tasks/{id}/files`; T032 owns post-creation selection.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
