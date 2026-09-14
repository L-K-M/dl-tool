# T046 — Serve the filesystem roots and browse endpoints

| Field | Value |
|---|---|
| **ID** | T046 |
| **Milestone** | M3 |
| **Status** | todo |
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
- [ ] `TestSanitiseSegmentTable` runs all thirty rows of doc 12 §3.4 and every one passes.
- [ ] `TestBrowseRejectsOutsideRoots` covers `/etc`, `/data/../etc` and `/data/ok/../../etc`.
- [ ] `TestBrowseDoesNotTraverseSymlink` passes with a symlink created inside the root during the test.
- [ ] `TestCallerCannotWalkAboveRoot` asserts `403` and a `null` parent at a root.
- [ ] No response from either endpoint lists a regular file.

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
Not run — the task stopped before implementation (see ## Blocked), so there is no
verification output to paste. The wiring claims below were checked against the tree:

```text
$ git rev-parse --short HEAD
b0524cb
$ grep -rln 'huma.Register' internal/api --include='*.go' | grep -v _test.go | sort
internal/api/auth.go
internal/api/server.go
internal/api/settings.go
internal/api/sse.go
internal/api/tasks.go
$ grep -n 'registerOperations\|RegisterOperations' internal/api/server.go
299:	server.registerOperations()
390:// registerOperations mounts the placeholder operation that keeps the document
392:func (s *Server) registerOperations() {
393:	s.auth.registerOperations(s.API)
394:	s.tasks.registerOperations(s.API)
395:	s.settings.registerOperations(s.API)
396:	s.SSE.RegisterOperations(s.API)
$ grep -rn 'server\.registerOperations\|\.registerOperations()' . --include='*.go' \
    | grep -v _test.go
./internal/api/server.go:299:	server.registerOperations()
```

Every production `huma.Register` call lives in a file reached only through
`Server.registerOperations`, and `NewServer` is that method's only caller — server.go:299 is
the sole invocation of `registerOperations` on the `Server` anywhere outside tests. No file
in the Files table can install a registration for `/fs/roots` or `/fs/browse`.

## Blocked

None. An earlier session stopped here because `internal/api/server.go` — the registration
call site of the two `/fs` Huma operations — was missing from the Files table while the
note below it forbade editing that file. The amendment it proposed is the
`internal/api/server.go` row above (recorded in pull request #160); the task proceeds with
no other scope change.
