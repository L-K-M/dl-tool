# T109 — Jail non-admins to their own destination subtree

| Field | Value |
|---|---|
| **ID** | T109 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T046, T047, T085, T086, T107 |
| **Blocks** | T111 |
| **Parallel-safe** | no — extends `internal/fsx/safepath.go` and `internal/api/fs.go` |
| **Implements** | [FR-124](../02-requirements.md#fr-124-jail-non-admins-to-their-own-destination-subtree) |
| **Decisions** | [ADR-0013](../decisions/0013-mandatory-built-in-authentication.md), [ADR-0012](../decisions/0012-single-data-mount.md) |
| **Est. size** | 0 new files, ~300 LOC |

## Goal
One containment check confines a non-admin to the subtree of their `users.default_destination` for
browsing, free space, `mkdir` **and** every task destination. A user with no `default_destination` reaches
nothing. No response ever enumerates another user's directory names.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §7.2 The per-user jail](../05-api-contract.md#72-the-per-user-jail)
2. [`docs/12-security-and-threat-model.md` §3 Path safety](../12-security-and-threat-model.md#3-path-safety)
3. [`docs/02-requirements.md` FR-124](../02-requirements.md#fr-124-jail-non-admins-to-their-own-destination-subtree)
4. [`docs/04-data-model.md` §3.1 Identity and access](../04-data-model.md#31-identity-and-access)
5. [`docs/tasks/T046-filesystem-roots-and-browse.md`](T046-filesystem-roots-and-browse.md)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/fsx/safepath.go` | modify | Add `Jail` and route every containment check through it. |
| `internal/api/fs.go` | modify | Apply the jail to `browse`, `roots`, `mkdir` and `free-space`. |
| `internal/api/fs_test.go` | modify | The isolation suite. |
| `internal/api/tasks.go` | modify | Apply the same jail to a task destination. |
| `internal/api/watchfolders.go` | modify | Apply it to `path` and `destination` against the named owner. |

No other file may be modified.

## Interface contract

```go
package fsx

// Jail is the containment rule for one caller. It is the ONLY place the rule is expressed: every
// path-accepting endpoint calls Contains, and no handler re-implements it.
type Jail struct {
	Roots []string // DLTOOL_DATA_ROOTS, in order
	Base  string   // users.default_destination; "" for an admin
	Admin bool
}

// Contains resolves p — symlinks included — and reports whether the caller may touch it.
//
//	Admin                      → p must lie inside one of Roots
//	Base set                   → p must lie inside Base, which must itself lie inside a root
//	Base empty and not Admin   → always false; the caller reaches nothing until an admin sets one
//
// The resolution runs before the comparison, so a symlink inside the jail pointing outside it is
// rejected exactly like a ".." path. A prefix comparison alone is not sufficient: "/data/alice2"
// must not match the jail "/data/alice".
func (j Jail) Contains(p string) (resolved string, err error)

// VisibleRoots reports what GET /fs/roots returns: every configured root for an admin, exactly the
// one Base entry for a jailed caller, and an empty slice for a caller with no Base.
func (j Jail) VisibleRoots() []string

// JailFor builds the Jail for a request identity.
func JailFor(roots []string, u store.User) Jail

// ErrPathRejected is the existing sentinel; a jail failure returns it, never a distinct error, so a
// caller cannot tell "outside the roots" from "outside my jail".
```

Behaviour that must hold at the HTTP layer, from doc 05 §7.2:

| Situation | Answer |
|---|---|
| Jailed caller browses above their jail | `403` `/problems/path-rejected` — never `404`, never a filtered listing |
| Jailed caller at their jail root | `parent` is `null`, so the browser cannot walk upwards |
| Jailed caller, `GET /fs/roots` | Exactly one entry: their own `default_destination` |
| Non-admin with no `default_destination` | `403` `/problems/path-rejected` from all four filesystem endpoints |
| Task destination outside the jail | `403` `/problems/path-rejected`; the task is not created |
| Category `save_path` outside the jail | The caller's own `default_destination` becomes `destination`, and the category path is reported in `requested_destination` |

## Steps
1. Add `Jail`, `Contains`, `VisibleRoots` and `JailFor` to `internal/fsx/safepath.go`, built on the existing
   `ResolveDestination` and `SafeJoin` so the symlink resolution is not duplicated.
2. Implement `Contains` with a path-component comparison after resolution, so `/data/alice2` never matches
   the jail `/data/alice`.
3. Return the existing `ErrPathRejected` for every failure, so an outside-the-roots rejection and an
   outside-my-jail rejection are indistinguishable to the caller.
4. Edit `internal/api/fs.go` so all four endpoints build the jail with `JailFor` and call `Contains` before
   any filesystem call; make `GET /fs/roots` return `VisibleRoots` and set `parent` to `null` at the jail
   root.
5. Edit `internal/api/tasks.go` so `POST /tasks` and `PATCH /tasks/{id}` validate `destination` through the
   same `Contains`, and so a category `save_path` outside the jail falls back to the caller's
   `default_destination` with the category path reported in `requested_destination`.
6. Edit `internal/api/watchfolders.go` so `path` and `destination` are checked against the **named owner's**
   jail, not the calling admin's.
7. Extend `internal/api/fs_test.go` with the isolation suite: assert a jailed caller browsing `/data` is
   `403`; assert `/data/alice2` is `403` for the jail `/data/alice`; assert a symlink inside the jail
   pointing at `/etc` is `403`; assert `GET /fs/roots` returns exactly one entry; assert `parent` is `null`
   at the jail root; assert a non-admin with no `default_destination` gets `403` from all four endpoints;
   assert a task destination outside the jail is `403` and no task exists afterwards; assert no response
   body in the whole suite contains another user's directory name.
8. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] A jailed caller cannot browse, stat, create in or download to any path outside their subtree.
- [ ] `/data/alice2` is rejected for the jail `/data/alice`.
- [ ] A symlink inside the jail pointing outside it is rejected after resolution.
- [ ] A jailed caller's `GET /fs/roots` has exactly one entry and its `parent` is `null`.
- [ ] A non-admin with no `default_destination` receives `403` from all four filesystem endpoints.
- [ ] No response in the suite enumerates another user's directory names.
- [ ] Admins reach every configured root, unchanged.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/fsx/... ./internal/api/..." && echo JAIL_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/fsx` and `ok  github.com/L-K-M/dl-tool/internal/api`, with
`TestJailedBrowseAboveIsForbidden`, `TestSiblingPrefixNotInJail`, `TestSymlinkOutOfJailRejected`,
`TestJailedRootsHasOneEntry`, `TestNoDefaultDestinationReachesNothing`,
`TestTaskDestinationOutsideJailRejected` and `TestNoCrossUserDirectoryNames` each reported as `--- PASS`.
The final line of stdout is exactly `JAIL_OK`. No `FAIL`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT weaken any row of the doc 12 §3.4 path-safety table; the jail is an additional check, never a
  replacement for the root check.
- Do NOT add a per-user root setting, a second data mount or a "browse from /" mode; ADR-0012 has exactly
  one `/data` mount and the jail is a subtree of it.
- Do NOT make a jail failure distinguishable from a root failure; both are `403` `/problems/path-rejected`.
- Do NOT return `404` for a path above the jail; that tells the caller the path exists.
- Do NOT implement ownership filtering of tasks; T085 owns it.
- Do NOT apply the jail to `delete_data`; T111 owns that call site.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
