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
Verification ran on this branch; it stops one row short of `FS_BROWSE_OK` because of the
doc 12 §3.4 row-8 contradiction recorded under `## Blocked`.

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
--- FAIL: TestSanitiseSegmentTable (0.00s)
    --- FAIL: TestSanitiseSegmentTable/row_08 (0.00s)
        safepath_test.go:62: SanitiseSegment("\\\\?\\C:\\x") = "____C__x", want "____C_x"
FAIL	github.com/L-K-M/dl-tool/internal/fsx	0.030s
ok  	github.com/L-K-M/dl-tool/internal/api	97.052s
FAIL
```

Twenty-nine of the thirty §3.4 rows pass — every row except 8 — as do all `internal/api`
tests, including the fs endpoint tests and `TestOpenAPIMatchesCommittedDocument` against
the regenerated `api/openapi.json` and `web/src/api/schema.d.ts`. `FS_BROWSE_OK` cannot
print until row 8 is resolved in the plan.

## Blocked

Doc 12 §3.4 row 8 cannot be implemented verbatim: its expected output contradicts §3.2
step 6 and row 7 of the same table.

- §3.2 step 6 replaces *each* of `/ \ : * ? " < > |` with `_`. Applied to row 8's input
  `\\?\C:\x` that yields `____C__x`; the table expects `____C_x`, collapsing the `:\`
  pair to one underscore.
- Row 7's `C:\Windows\system32.exe` → `C__Windows_system32.exe` maps the identical `C:\`
  substring to `C__`, i.e. per character. Rows 7 and 8 cannot both be produced by any
  rule consistent with step 6's "each", because the `C:\` substring is identical in
  both inputs.

Row 19 needed no plan fix. Its input is written `\|` only because a bare pipe inside a
table cell's code span is escaped in GitHub-flavoured markdown; the segment under test is
`"a<b>c|d?e*f:g"`, which step 6 maps exactly to the tabulated `_a_b_c_d_e_f_g_`. The test
carries that input with a comment.

The only rules satisfying all thirty rows — absorbing the colon of a leading `\\?\X:`
device prefix, or replacing the first illegal run per character while collapsing later
runs — appear nowhere in §3.2 and disagree with each other on inputs outside the table
(for example `z\\?\C:\x`), so picking one would fabricate undocumented behaviour on the
path-safety boundary. Either plan amendment unblocks the task:

1. Correct row 8's expected output to `____C__x`, matching step 6 and every other row; or
2. If `\\?\X:` device-prefix handling is intended (the row is labelled "Windows device
   path"), add it to §3.2 as an explicit step so the algorithm and the table agree.

The implementation is otherwise complete on this branch: `SanitiseSegment`, `SafeJoin`
with `openat2` plus the `ENOSYS` fallback, `Browse`/`Roots`, the two `/fs` operations,
`server.go` wiring, regenerated `api/openapi.json` and `web/src/api/schema.d.ts`, and the
full thirty-row §3.4 test table. Once the row is resolved, updating the expected value in
`internal/fsx/safepath_test.go` finishes the task. An earlier session's blocked record —
the missing `server.go` wiring scope — was resolved by pull request #160 and the plan
amendment on top of it.
