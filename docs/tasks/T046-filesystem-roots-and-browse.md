# T046 — Serve the filesystem roots and browse endpoints

| Field | Value |
|---|---|
| **ID** | T046 |
| **Milestone** | M3 |
| **Status** | todo |
| **Depends on** | T007, T008, T020 |
| **Blocks** | T047, T049, T053 |
| **Parallel-safe** | no — extends `internal/fsx/safepath.go` and `internal/api/server.go` |
| **Implements** | [FR-040](../02-requirements.md#fr-040-browse-the-server-filesystem-jailed-to-configured-roots), [FR-042](../02-requirements.md#fr-042-reject-a-destination-outside-the-configured-roots), [NFR-014](../02-requirements.md#nfr-014-never-build-a-filesystem-path-from-a-request-parameter) |
| **Decisions** | [ADR-0012](../decisions/0012-single-data-mount.md) |
| **Est. size** | 4 new files, ~400 LOC. Doc 12 §3.4 assigns the 30-row hostile-path table to this task, so it lands with the endpoints it protects. |

## Goal
`GET /fs/roots` and `GET /fs/browse` list directories only, jailed to `DLTOOL_DATA_ROOTS` and to the
caller's own subtree, with containment verified at the syscall layer. `sanitiseSegment` and `safeJoin`
exist and reject every hostile row of the doc 12 table.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §7.1 Endpoints](../05-api-contract.md#71-endpoints) — the two response shapes
   and every status code.
2. [`docs/05-api-contract.md` §7.2 The per-user jail](../05-api-contract.md#72-the-per-user-jail) — the
   three caller cases, and why a jailed caller sees `403`, never `404` and never a filtered listing.
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
| `internal/api/fs_test.go` | create | Handler cases including the jail and the symlink escape. |

`internal/api/server.go` is not edited: register the group from `NewFSHandlers(...).Register(api)` called by
the existing handler wiring. If that wiring does not yet exist, STOP and write it under "Blocked".

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
	Parent      *string `json:"parent"`     // nil at a root or at the caller's jail root
	Separator   string  `json:"separator"`  // always "/"
	Writable    bool    `json:"writable"`
	FreeBytes   int64   `json:"free_bytes"`
	TotalBytes  int64   `json:"total_bytes"`
	Directories []Entry `json:"directories"`
}

// Browse lists the subdirectories of path. roots is DLTOOL_DATA_ROOTS in order and jail is the
// caller's users.default_destination, empty for an admin. It returns ErrPathRejected outside the
// jail or the roots, and fs.ErrNotExist for a readable-root-relative path that does not exist.
func Browse(roots []string, jail, path string, showHidden bool) (Listing, error)

// Roots reports one entry per configured root, or exactly the jail for a jailed caller.
func Roots(roots []string, jail string) ([]Listing, error)
```

```go
package api

type FSHandlers struct{ /* roots []string; db *sqlx.DB */ }

func NewFSHandlers(roots []string, db *sqlx.DB) *FSHandlers
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
   with a case-insensitive comparison. Never follow a symlink out of the jail.
5. Resolve the caller's jail from `api.IdentityFrom(ctx)`: an admin gets all roots, a user with a
   `default_destination` gets exactly that subtree, and a user without one gets `403` from every call.
6. Create `internal/api/fs.go` with `FSHandlers`, the two operations and the error mapping above. Never
   concatenate a path from request input; every path goes through `fsx`.
7. Create `internal/api/fs_test.go` with `humatest`: a root lists; `/etc` is `403 /problems/path-rejected`;
   `/data/../etc` and `/data/ok/../../etc` are `403`; a symlink inside a root pointing at `/etc` is not
   traversed; a jailed user browsing above their jail gets `403`, and their `parent` is `null` at the jail
   root; no response body contains a file entry.
8. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestSanitiseSegmentTable` runs all thirty rows of doc 12 §3.4 and every one passes.
- [ ] `TestBrowseRejectsOutsideRoots` covers `/etc`, `/data/../etc` and `/data/ok/../../etc`.
- [ ] `TestBrowseDoesNotTraverseSymlink` passes with a symlink created inside the root during the test.
- [ ] `TestJailedCallerCannotWalkUp` asserts `403` and a `null` parent at the jail root.
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
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
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
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
