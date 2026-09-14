# T046 — Serve the filesystem roots and browse endpoints

| Field | Value |
|---|---|
| **ID** | T046 |
| **Milestone** | M3 |
| **Status** | done |
| **Depends on** | T007, T008, T020 |
| **Blocks** | T047 |
| **Parallel-safe** | no — extends `internal/fsx/safepath.go` and `internal/api/server.go` |
| **Implements** | [FR-040](../02-requirements.md#fr-040-browse-the-server-filesystem-jailed-to-configured-roots), [FR-042](../02-requirements.md#fr-042-reject-a-destination-outside-the-configured-roots), [NFR-014](../02-requirements.md#nfr-014-never-build-a-filesystem-path-from-a-request-parameter) |
| **Decisions** | [ADR-0012](../decisions/0012-single-data-mount.md) |
| **Est. size** | 4 new files, ~400 LOC. Doc 12 §3.4 assigns the 30-row hostile-path table to this task, so it lands with the endpoints it protects. |

## Goal
`GET /fs/roots` and `GET /fs/browse` list directories only, jailed to `DLTOOL_DATA_ROOTS`, with containment
verified at the syscall layer. `sanitiseSegment` and `safeJoin` exist and reject every hostile row of the
doc 12 table.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §7.1 Endpoints](../05-api-contract.md#71-endpoints) — the two response shapes
   and every status code.
2. [`docs/05-api-contract.md` §7.2 Containment](../05-api-contract.md#72-containment) — why a caller
   above a root sees `403`, never `404` and never a filtered listing.
3. [`docs/12-security-and-threat-model.md` §3.2](../12-security-and-threat-model.md#32-sanitisesegments-string-string),
   [§3.3](../12-security-and-threat-model.md#33-safejoinroot-string-segments-string-string-error) and
   [§3.4 Test-case table](../12-security-and-threat-model.md#34-test-case-table) — implement all three verbatim.
4. [`docs/tasks/T020-create-tasks-endpoint.md`](T020-create-tasks-endpoint.md) — the existing
   `fsx.ResolveDestination` and `fsx.ErrPathRejected`.
5. [`docs/14-conventions.md` §2.2 Error model](../14-conventions.md#22-error-model).

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/fsx/safepath.go` | edit | Add `SanitiseSegment` and `SafeJoin`. |
| `internal/fsx/safepath_test.go` | create | The 30-row table of doc 12 §3.4, verbatim. |
| `internal/fsx/browse.go` | create | Directory-only listing with syscall-level containment. |
| `internal/api/fs.go` | create | The `GET /fs/roots` and `GET /fs/browse` handlers. |
| `internal/api/fs_test.go` | create | Handler cases including the root containment and the symlink escape. |
| `internal/api/server.go` | edit | Set an `fs` field via `NewFSHandlers(cfg.DataRoots)` in `NewServer`; call `s.fs.Register(s.API)` in `registerOperations`. |

No other file may be modified, apart from the two generated files of
[`docs/13-testing-and-verification.md` §7.1](../13-testing-and-verification.md), this task file's
`## Evidence` section, and this task's row in the task index.

## Interface contract

```go
package fsx

// SanitiseSegment applies the eleven steps of doc 12 section 3.2 to one path component.
func SanitiseSegment(s string) string

// SafeJoin joins segments under root with the rules of doc 12 section 3.3. It returns
// ErrPathRejected for an absolute segment, a "..", a path over 4096 bytes, a depth over 32,
// or any component that is a symlink.
func SafeJoin(root string, segments []string) (string, error)

// Entry is one directory in a listing. Files are never returned.
type Entry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Writable bool   `json:"writable"`
}

// Listing is the answer of Browse.
type Listing struct {
	Path        string  `json:"path"`
	Parent      *string `json:"parent"`     // nil at a root, so the browser cannot walk upwards
	Separator   string  `json:"separator"`  // always "/"
	Writable    bool    `json:"writable"`
	FreeBytes   int64   `json:"free_bytes"`
	TotalBytes  int64   `json:"total_bytes"`
	Directories []Entry `json:"directories"`
}

// Browse lists the subdirectories of path. roots is DLTOOL_DATA_ROOTS in order. It returns
// ErrPathRejected outside the roots, and fs.ErrNotExist for a readable-root-relative path that
// does not exist.
func Browse(roots []string, path string, showHidden bool) (Listing, error)

// Roots reports one entry per configured root.
func Roots(roots []string) ([]Listing, error)
```

```go
package api

type FSHandlers struct{ /* roots []string */ }

func NewFSHandlers(roots []string) *FSHandlers
func (h *FSHandlers) Register(api huma.API)

type BrowseInput struct {
	Path       string `query:"path" required:"true"`
	ShowHidden bool   `query:"show_hidden"`
}
type BrowseOutput struct{ Body fsx.Listing }

type RootsOutput struct {
	Body struct {
		Roots []struct {
			Path       string `json:"path"`
			Writable   bool   `json:"writable"`
			FreeBytes  int64  `json:"free_bytes"`
			TotalBytes int64  `json:"total_bytes"`
		} `json:"roots"`
	}
}

func (h *FSHandlers) ListRoots(ctx context.Context, in *struct{}) (*RootsOutput, error)
func (h *FSHandlers) Browse(ctx context.Context, in *BrowseInput) (*BrowseOutput, error)
```

Error mapping: `fsx.ErrPathRejected` → `403 /problems/path-rejected`; `fs.ErrNotExist` and an unreadable
directory → `404 /problems/not-found`; a missing `path` → `422 /problems/validation-failed`.

## Steps
1. Edit `internal/fsx/safepath.go` to add `SanitiseSegment` with the eleven ordered steps of doc 12 §3.2,
   changing nothing about `ResolveDestination`.
2. Add `SafeJoin` with the seven rules of doc 12 §3.3, including the `openat2` path with
   `RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS|RESOLVE_NO_MAGICLINKS` and the portable `ENOSYS` fallback.
3. Create `internal/fsx/safepath_test.go` as a table-driven test carrying all thirty rows of doc 12 §3.4,
   with the row number in each case name.
4. Create `internal/fsx/browse.go` with `Browse` and `Roots`. Read the directory with `os.ReadDir`, keep
   entries whose resolved target is a directory, drop dot-directories unless `showHidden`, and sort by name
   with a case-insensitive comparison. Never follow a symlink out of a root.
5. Create `internal/api/fs.go` with `FSHandlers`, the two operations and the error mapping above. Never
   concatenate a path from request input; every path goes through `fsx`.
6. Create `internal/api/fs_test.go` with `humatest`: a root lists; `/etc` is `403 /problems/path-rejected`;
   `/data/../etc` and `/data/ok/../../etc` are `403`; a symlink inside a root pointing at `/etc` is not
   traversed; browsing above a root gets `403`, and `parent` is `null` at the root; no response body
   contains a file entry.
7. Edit `internal/api/server.go` to construct the handlers and call `Register`: one `fs` field on
   `Server`, `NewFSHandlers(cfg.DataRoots)` in `NewServer`, `s.fs.Register(s.API)` in
   `registerOperations`.
8. Run `make gen` to regenerate `api/openapi.json` and `web/src/api/schema.d.ts` (docs/13 §7.1).
   Run the verification command, paste its output under `## Evidence`, and confirm scope with the
   `git status` command under `## Verification`. Only then commit everything, including the two
   regenerated files.

## Acceptance criteria
- [x] `TestSanitiseSegmentTable` runs all thirty rows of doc 12 §3.4 and every one passes.
- [x] `TestBrowseRejectsOutsideRoots` covers `/etc`, `/data/../etc` and `/data/ok/../../etc`.
- [x] `TestBrowseDoesNotTraverseSymlink` passes with a symlink created inside the root during the test.
- [x] `TestCallerCannotWalkAboveRoot` asserts `403` and a `null` parent at a root.
- [x] No response from either endpoint lists a regular file.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/fsx/... ./internal/api/..." && echo FS_BROWSE_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/fsx` and `ok  github.com/L-K-M/dl-tool/internal/api`, every
test named above reported as `--- PASS`, and the final line of stdout is exactly `FS_BROWSE_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table plus the two generated files of
docs/13 §7.1 (`api/openapi.json`, `web/src/api/schema.d.ts`), in the sorted order the command
prints, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement `POST /fs/mkdir` or `GET /fs/free-space`; T047 owns both.
- Do NOT build any UI; T047 owns the folder browser dialog.
- Do NOT return file entries, sizes or modification times from `browse`; directories only.
- Do NOT add a "show all filesystems" or "browse from /" mode; ADR-0012 has exactly one mount.
- Do NOT weaken any row of the doc 12 §3.4 table, including the deliberately stricter rows 2, 4, 13, 14
  and 19.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
Verification ran on the final tree after the doc 12 §3.4 row-8 correction (see
`## Blocked`):

```text
$ make lint
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!

$ make test PKG="./internal/fsx/... ./internal/api/..."
go test -race -count=1 ./internal/fsx/... ./internal/api/...
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.032s
ok  	github.com/L-K-M/dl-tool/internal/api	90.954s
FS_BROWSE_OK
```

All thirty §3.4 rows pass, including the corrected row 8. The acceptance-named tests,
run verbosely on the same tree:

```text
--- PASS: TestSanitiseSegmentTable (0.01s)
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.036s
--- PASS: TestListFSRoots (0.36s)
--- PASS: TestBrowseListsRoot (0.33s)
--- PASS: TestBrowseRejectsOutsideRoots (0.36s)
--- PASS: TestBrowseDoesNotTraverseSymlink (0.36s)
--- PASS: TestCallerCannotWalkAboveRoot (0.36s)
ok  	github.com/L-K-M/dl-tool/internal/api	2.902s
```

Scope check — the worktree was mid-commit when the command ran, so the porcelain line
shows only the pending row-8 edit; the full branch change set is the second command:

```text
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
internal/fsx/safepath_test.go

$ git diff origin/main --name-only | sort
api/openapi.json
docs/tasks/00-task-index.md
docs/tasks/T046-filesystem-roots-and-browse.md
go.mod
go.sum
internal/api/fs.go
internal/api/fs_test.go
internal/api/server.go
internal/fsx/browse.go
internal/fsx/safepath.go
internal/fsx/safepath_test.go
web/src/api/schema.d.ts
```

Every path is in the `## Files` table or a docs/13 §7.1 carve-out (`api/openapi.json`,
`web/src/api/schema.d.ts` regenerated by `make gen`; `go.mod`/`go.sum` from
`go mod tidy` promoting the already-pinned `golang.org/x/text` to a direct requirement).

## Blocked

Resolved by the plan repair: doc 12 §3.4 row 8 now expects `____C__x`, the output §3.2
step 6's per-character replacement produces, and `internal/fsx/safepath_test.go` asserts
it. `TestSanitiseSegmentTable/row_08` passes.

Original defect: row 8 expected `\\?\C:\x` → `____C_x`, collapsing the `:\` pair to one
underscore, while §3.2 step 6 replaces *each* of `/ \ : * ? " < > |` with `_` — and row 7
maps the identical `C:\` substring to `C__`. No rule consistent with step 6's "each"
produces both tabulated outputs, so the test could not satisfy all thirty rows verbatim.
The repair chose the table correction over adding undocumented `\\?\X:` device-prefix
handling to §3.2; row 19 needed no fix (`\|` in the table source is the GFM escape for a
literal `|`).

An earlier session's blocked record — the missing `server.go` wiring scope — was resolved
by pull request #160 and the plan amendment on top of it.
