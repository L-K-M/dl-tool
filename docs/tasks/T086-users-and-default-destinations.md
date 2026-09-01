# T086 — Manage users, their default destinations and the process order

| Field | Value |
|---|---|
| **ID** | T086 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T084, T085, T098 |
| **Blocks** | T108, T109 |
| **Parallel-safe** | yes — adds `internal/api/users.go` |
| **Implements** | [FR-120](../02-requirements.md#fr-120-apply-a-per-user-default-destination), [FR-095](../02-requirements.md#fr-095-order-the-queue-by-creation-date-or-by-owner) |
| **Decisions** | [ADR-0013](../decisions/0013-mandatory-built-in-authentication.md) |
| **Est. size** | 2 new files, ~330 LOC |

## Goal
An admin creates, edits and deletes users, each with a `default_destination`, a `quota_bytes` and a
`locale`. A task created without a destination lands in its owner's `default_destination` in preference to
the global default. With `process_order = by_user_round_robin`, admission starts at most one task per owner
before any owner's second.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §12 Users and API tokens](../05-api-contract.md#12-users-and-api-tokens)
2. [`docs/04-data-model.md` §3.1 Identity and access](../04-data-model.md#31-identity-and-access)
3. [`docs/11-config-reference.md` §5 Database-backed settings](../11-config-reference.md#5-database-backed-settings)
4. [`docs/02-requirements.md` FR-120](../02-requirements.md#fr-120-apply-a-per-user-default-destination)
5. [`docs/tasks/T098-concurrency-limiter.md`](T098-concurrency-limiter.md)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/users.go` | create | The four user operations and their validation. |
| `internal/api/users_test.go` | create | CRUD, last-admin, conflict, destination-default and ordering cases. |
| `internal/store/users.go` | modify | Add `ListUsers`, `UpdateUser`, `DeleteUser`, `CountEnabledAdmins`. |
| `internal/engine/admission.go` | modify | Order candidates by `process_order`. |
| `internal/api/tasks.go` | modify | Insert the owner default into the destination resolution chain. |

No other file may be modified. `internal/api/server.go` is not edited: register the group from
`NewUserHandlers(...).Register(api)` called by the existing handler wiring.

## Interface contract

```go
package api

// UserView is the user object of doc 05 §12. password_hash is never a member of any shape here.
type UserView struct {
	ID                 string     `json:"id"`   // usr_ + ULID
	Username           string     `json:"username"`
	Role               string     `json:"role"` // "admin" | "user"
	Enabled            bool       `json:"enabled"`
	DefaultDestination *string    `json:"default_destination"`
	QuotaBytes         int64      `json:"quota_bytes"` // STORAGE quota in bytes, 0 = unlimited
	Locale             string     `json:"locale"`
	LastLoginAt        *time.Time `json:"last_login_at"`
	CreatedAt          time.Time  `json:"created_at"`
}

// CreateUserBody requires username, password (>= 12 characters) and role.
type CreateUserBody struct {
	Username           string  `json:"username" required:"true" minLength:"1"`
	Password           string  `json:"password" required:"true" minLength:"12"`
	Role               string  `json:"role" required:"true" enum:"admin,user"`
	Enabled            *bool   `json:"enabled,omitempty"`
	DefaultDestination *string `json:"default_destination,omitempty"`
	QuotaBytes         *int64  `json:"quota_bytes,omitempty" minimum:"0"`
	Locale             *string `json:"locale,omitempty"`
}

// PatchUserBody accepts every CreateUserBody field, all optional, plus password.
```

Rules that must be enforced, not assumed: every operation is admin-only; a request that would delete or
disable the **last enabled admin** is `403` `/problems/forbidden`; a duplicate username is `409`
`/problems/conflict`, case-insensitively; a `default_destination` outside `DLTOOL_DATA_ROOTS` is `422`;
`quota_bytes` is a storage quota and never a task count.

```go
package store

// CountEnabledAdmins is the guard behind the last-admin rule; it is evaluated inside the same
// transaction as the delete or the disable, never before it.
func (s *UserStore) CountEnabledAdmins(ctx context.Context) (int, error)
```

```go
package engine

// ProcessOrder is the settings key process_order of docs/11-config-reference.md §5.
type ProcessOrder string

const (
	OrderByDateCreated      ProcessOrder = "by_date_created"
	OrderByUserRoundRobin   ProcessOrder = "by_user_round_robin"
)

// orderCandidates sorts the queued candidates the Admitter is about to start.
//
//	OrderByDateCreated    → added_at ascending
//	OrderByUserRoundRobin → one task per owner in added_at order, then a second pass for owners'
//	                        second tasks, and so on; ties broken by added_at
//
// Ordering is applied before every limit check, so the limits decide how many start and this
// decides which.
func orderCandidates(in []store.Task, order ProcessOrder) []store.Task
```

Destination resolution order for a task created with no `destination`: the task's category `save_path`, then
the owner's `users.default_destination`, then the `default_destination` settings key. The resolved value is
`tasks.destination`; when it differs from what the client asked for, the request value is reported in
`requested_destination`.

## Steps
1. Add `ListUsers`, `UpdateUser`, `DeleteUser` and `CountEnabledAdmins` to `internal/store/users.go` with
   explicit column lists; never select `password_hash` into a struct that reaches an API response.
2. Create `internal/api/users.go` with `UserView`, the four operations and the validation rules above,
   guarded by `RequireAdmin` from T084.
3. Hash a supplied password with the existing argon2id helper in `internal/secure`; never store or echo it.
4. Evaluate the last-admin rule inside the same transaction as the delete or the disable.
5. Validate `default_destination` with `fsx.ResolveDestination` and return `422` when it resolves outside
   every configured root.
6. Edit the destination resolution chain in `internal/api/tasks.go` so the owner's `default_destination`
   wins over the global default and loses to a category `save_path`, changing nothing else in that file.
7. Edit `internal/engine/admission.go` to read `process_order` and apply `orderCandidates` before the limit
   checks; `by_date_created` remains the default.
8. Create `internal/api/users_test.go`: assert CRUD round-trips without `password_hash`; assert a duplicate
   username differing only in case is `409`; assert deleting and disabling the last enabled admin are both
   `403`; assert a `default_destination` of `/etc` is `422`; assert a task created by user B with no
   destination lands in `/data/b`; assert three tasks for A and one for B under `by_user_round_robin` start
   B's before A's second.
9. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] Every `/users` verb is admin-only and a non-admin receives `403` with nothing changed.
- [ ] `password_hash` appears in no response of any shape.
- [ ] Deleting or disabling the last enabled admin is `403`.
- [ ] A duplicate username differing only in case is `409`.
- [ ] A task created with no destination by a user with `default_destination` lands there.
- [ ] Under `by_user_round_robin`, user B's first task starts before user A's second.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/api/... ./internal/engine/..." && echo USERS_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/api` and `ok  github.com/L-K-M/dl-tool/internal/engine`,
with `TestUserCrudRoundTrip`, `TestNoPasswordHashInResponses`, `TestLastAdminProtected`,
`TestDuplicateUsernameCaseInsensitive`, `TestPerUserDefaultDestination` and `TestRoundRobinOrdering` each
reported as `--- PASS`. The final line of stdout is exactly `USERS_OK`. No `FAIL`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT implement the filesystem jail derived from `default_destination`; T109 owns it.
- Do NOT change the concurrency limits themselves; T098 owns `max_active_total`,
  `max_active_per_engine` and `max_active_per_user`, and this task only orders the candidates.
- Do NOT add API-token endpoints; T084 owns them.
- Do NOT add a group, team, permission or per-endpoint role model; there are exactly two roles.
- Do NOT allow a user to change their own role, quota or `default_destination`.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
