# T020 — Create tasks from submitted URIs

| Field | Value |
|---|---|
| **ID** | T020 |
| **Milestone** | M1 |
| **Status** | todo |
| **Depends on** | T007, T008, T015, T016, T017, T019 |
| **Blocks** | T021, T022, T023, T098, T099 |
| **Parallel-safe** | no — adds the shared `internal/api/tasks.go` |
| **Implements** | [FR-001](../02-requirements.md#fr-001-add-tasks-from-a-batch-of-pasted-uris), [FR-009](../02-requirements.md#fr-009-supply-ftp-credentials-for-a-single-task), [FR-010](../02-requirements.md#fr-010-recurse-an-ftp-directory-when-the-uri-ends-in-a-slash) |
| **Decisions** | [ADR-0003](../decisions/0003-chi-huma-code-first-openapi.md), [ADR-0012](../decisions/0012-single-data-mount.md) |
| **Est. size** | 3 new files, ~400 LOC |

## Goal
`POST /api/v1/tasks` accepts up to 50 URIs, normalises and routes each one, resolves the destination inside
a configured data root, inserts one `tasks` row per accepted URI and hands it to its engine. Rejected URIs
come back in `rejected[]` while the accepted ones are still created.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §5.2 `POST /tasks`](../05-api-contract.md#52-post-tasks)
2. [`docs/05-api-contract.md` §3 The canonical Task object](../05-api-contract.md#3-the-canonical-task-object)
3. [`docs/05-api-contract.md` §1.3 Errors](../05-api-contract.md#13-errors--rfc-9457-applicationproblemjson)
4. [`docs/12-security-and-threat-model.md` §3.3 `safeJoin`](../12-security-and-threat-model.md#33-safejoinroot-string-segments-string-string-error)
5. [`docs/06-download-engines.md` §2 Routing table](../06-download-engines.md#2-routing-table)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/tasks.go` | create | The `POST /tasks` handler and its Huma input and output structs. |
| `internal/api/tasks_test.go` | create | `humatest` cases for acceptance, partial rejection and every status code below. |
| `internal/fsx/safepath.go` | create | `ResolveDestination` — root containment after symlink resolution. |
| `internal/api/server.go` | modify | Register the operation on the existing Huma API. |
| `internal/store/tasks.go` | modify | Add `CountBytesForOwner` for the storage-quota pre-check. |

No other file may be modified.

## Interface contract

```go
package fsx

// ErrPathRejected is returned when a path resolves outside every configured root, or outside the
// caller's jail. The API maps it to /problems/path-rejected with status 403.
var ErrPathRejected = errors.New("fsx: path rejected")

// ResolveDestination resolves requested against the configured roots, following symlinks, and
// returns the cleaned absolute path. roots is DLTOOL_DATA_ROOTS in order; jail is the caller's
// users.default_destination and is "" for an admin. An empty requested returns the first root, or
// the jail when one is set.
func ResolveDestination(roots []string, jail, requested string) (string, error)
```

```go
package api

// CreateTasksInput is the JSON body of POST /tasks. The multipart form is added by T033.
type CreateTasksInput struct {
	Body struct {
		URIs            []string        `json:"uris"             maxItems:"50"`
		Destination     string          `json:"destination,omitempty"`
		Category        string          `json:"category,omitempty"`
		Tags            []string        `json:"tags,omitempty"`
		Paused          bool            `json:"paused,omitempty"`
		Sequential      bool            `json:"sequential,omitempty"`
		CreateSubfolder bool            `json:"create_subfolder,omitempty"`
		FTPCredentials  *FTPCredentials `json:"ftp_credentials,omitempty"`
		ExtractPassword string          `json:"extract_password,omitempty"`
		Engine          string          `json:"engine,omitempty"     enum:"aria2,qbittorrent,ytdlp"`
	}
}

// FTPCredentials is used for this request's ftp, ftps and sftp URIs only and is never returned.
type FTPCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// RejectedURI is one entry of rejected[]; type is a slug from the registry in 05 §1.3.
type RejectedURI struct {
	URI    string `json:"uri"`
	Type   string `json:"type"`
	Detail string `json:"detail"`
}

// CreateTasksOutput carries HTTP 201 whenever at least one task was created.
type CreateTasksOutput struct {
	Status int `json:"-"`
	Body   struct {
		Created  []TaskDTO     `json:"created"`
		Rejected []RejectedURI `json:"rejected"`
	}
}

func (h *TaskHandlers) CreateTasks(ctx context.Context, in *CreateTasksInput) (*CreateTasksOutput, error)
```

Status codes, exactly these: `201` with at least one created task · `403` `/problems/path-rejected` ·
`403` `/problems/quota-exceeded` · `413` `/problems/payload-too-large` · `422`
`/problems/validation-failed` for an empty submission, more than 50 URIs or an unknown category · `422`
`/problems/unsupported-scheme` when **every** URI was rejected · `503` `/problems/engine-unavailable`.

## Steps
1. Create `internal/fsx/safepath.go` with `ErrPathRejected` and `ResolveDestination`; resolve symlinks
   with `filepath.EvalSymlinks`, compare the result against each resolved root plus a trailing separator,
   and never build the path by string concatenation of request input.
2. Apply the jail argument as a second containment check with the same comparison, so a non-admin is
   confined to the subtree of their `default_destination`.
3. Create `internal/api/tasks.go` with `TaskHandlers`, its constructor taking the store, the registry and
   the configured roots, and the input and output structs above.
4. In `CreateTasks`, reject an empty `uris` and a list longer than 50 with `/problems/validation-failed`
   before any other work.
5. Resolve the destination once per request: the body value, else the caller's `default_destination`, else
   the first configured root. Map `fsx.ErrPathRejected` to `403` `/problems/path-rejected`.
6. Pre-check the owner's storage quota with `CountBytesForOwner`; a breach rejects the whole request with
   `403` `/problems/quota-exceeded`, and the task is never created.
7. Per URI: call `uri.Normalize`, then `engine.Route`, then honour an explicit `engine` field only when
   that engine's `Accepts()` is true. Map `uri.ErrUnsupportedScheme` and `engine.ErrNoEngine` to a
   `rejected[]` entry of type `/problems/unsupported-scheme`, with the message
   `ed2k is not supported in v1` for the ed2k scheme.
8. Per accepted URI: insert the `tasks` row through `store.Create` in state `queued` (or `paused` when
   `paused` is true), then call `Engine.Add` and store the returned handle with `SetEngineRef`.
9. Pass `ftp_credentials` to the adapter through `engine.AddRequest.Extra` for `ftp`, `ftps` and `sftp`
   URIs only, and strip userinfo from `tasks.source_uri` so no password is persisted or logged.
10. Return `422` `/problems/unsupported-scheme` when every URI was rejected; otherwise `201`.
11. Register the operation in `internal/api/server.go` with `huma.Register` and the operation id
    `create-tasks`.
12. Create `internal/api/tasks_test.go` with `humatest` cases: the four-line FR-001 submission
    (`https:`, `ftp:`, `magnet:`, garbage) asserting three created and one rejected; an ed2k submission
    asserting `422` and the exact message; a destination of `/etc` asserting `403`
    `/problems/path-rejected`; and an FTP submission asserting the response body contains no password.

## Acceptance criteria
- [ ] A four-line submission returns `201` with three `created[]` entries and one `rejected[]` entry.
- [ ] An ed2k-only submission returns `422` `/problems/unsupported-scheme` and the message
      `ed2k is not supported in v1`.
- [ ] `destination: "/data/../etc"` returns `403` `/problems/path-rejected` and creates nothing.
- [ ] A 51-URI submission returns `422` `/problems/validation-failed`.
- [ ] No response body and no log line contains an FTP password.
- [ ] Every created task carries a `tsk_` ULID and state `queued`, or `paused` when requested.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/api/...
```
Expected: `make lint` prints nothing, then `ok  	github.com/L-K-M/dl-tool/internal/api` followed by its
elapsed time, with `TestCreateTasksMixedBatch`, `TestCreateTasksRejectsED2K`,
`TestCreateTasksRejectsDestination` and `TestCreateTasksHidesFTPPassword` all running. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT accept `multipart/form-data`, a `blob` field or an uploaded `.torrent` or `.txt`; T033 owns
  uploads.
- Do NOT implement `POST /tasks/inspect`; T031 owns it.
- Do NOT implement `sanitiseSegment`, `safeJoin` or the 30-row hostile-path table; T046 extends
  `internal/fsx/safepath.go` with them, and T109 wires the per-user jail across `/fs/*`.
- Do NOT enforce `max_active_*`; T098 owns admission control.
- Do NOT check free space; T099 owns the reservation gate.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
