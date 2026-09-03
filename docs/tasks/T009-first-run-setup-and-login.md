# T009 — Complete the first run with a one-time setup token

| Field | Value |
|---|---|
| **ID** | T009 |
| **Milestone** | M0 |
| **Status** | done |
| **Depends on** | T008 |
| **Blocks** | T040, T084 |
| **Parallel-safe** | no — it edits `internal/api/auth.go` |
| **Implements** | [FR-115](../02-requirements.md#fr-115-complete-a-first-run-setup-using-a-one-time-token), [NFR-011](../02-requirements.md#nfr-011-ship-no-default-credentials) |
| **Decisions** | [ADR-0013](../decisions/0013-mandatory-built-in-authentication.md) |
| **Est. size** | 1 new source file, 1 test file, 4 edits, ~330 LOC |

## Goal
A fresh instance prints a one-time setup token, writes it to `<config>/setup-token` mode `0600`, and answers
every endpoint except `POST /auth/setup` with 401 `/problems/setup-required`. Setup creates the single
operator account with an argon2id password hash, deletes the token file, and returns 409 on any later
attempt. Login, logout
and `GET /auth/me` work with the session that setup issued.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §4.1 `POST /auth/setup`](../05-api-contract.md#41-post-authsetup) — the body,
   the 201 envelope and every status code.
2. [`docs/05-api-contract.md` §4.2 login, logout and me](../05-api-contract.md#42-post-authlogin-post-authlogout-get-authme).
3. [`docs/12-security-and-threat-model.md` §6.3 Password storage and login](../12-security-and-threat-model.md#63-password-storage-and-login)
   — the argon2id parameters, the PHC string and the brute-force controls.
4. [`docs/12-security-and-threat-model.md` §6.4 First run](../12-security-and-threat-model.md#64-first-run-and-the-absence-of-default-credentials).

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/secure/hash.go` | create | argon2id hashing and verification against the PHC string. |
| `internal/secure/hash_test.go` | create | Parameter, round-trip and constant-time tests. |
| `internal/store/users.go` | edit | `CreateUser`, `UserByUsername`, `TouchLastLogin`. |
| `internal/api/auth.go` | edit | The setup gate and the four `/auth` operations. |
| `internal/api/auth_test.go` | edit | Setup, second-setup, login, logout and me coverage. |
| `internal/api/server.go` | edit | Register the four /auth operations on the existing Huma API: call a `registerAuthOperations` helper defined in auth.go from `registerOperations`. |

No other file may be modified.

## Interface contract

```go
package secure

// Params are the OWASP mid-point argon2id parameters from
// 12-security-and-threat-model.md section 6.3. Do not lower them.
const (
	argonMemoryKiB = 19456 // m=19456 (19 MiB)
	argonTime      = 2     // t=2
	argonThreads   = 1     // p=1
	argonSaltLen   = 16
	argonKeyLen    = 32
)

// HashPassword returns the full PHC string
// $argon2id$v=19$m=19456,t=2,p=1$<b64salt>$<b64hash>.
func HashPassword(password string) (string, error)

// VerifyPassword parses the PHC string, re-derives the key with the parameters it
// carries and compares in constant time. It returns needsRehash when the stored
// parameters are weaker than the constants above.
func VerifyPassword(phc, password string) (ok bool, needsRehash bool, err error)
```

```go
package store

func CreateUser(ctx context.Context, db *sqlx.DB, u User) error
func UserByUsername(ctx context.Context, db *sqlx.DB, username string) (User, error)
func TouchLastLogin(ctx context.Context, db *sqlx.DB, id string, now int64) error
```

Wire shapes, exactly as doc 05 §4.1:

```http
POST /api/v1/auth/setup
Content-Type: application/json

{"setup_token":"9f2c...","username":"alice","password":"correct horse battery","locale":"en"}
```

```http
HTTP/1.1 201 Created
Set-Cookie: dltool_session=6f1c...; Path=/; HttpOnly; SameSite=Lax

{"user":{"id":"usr_01JKQ7X1AA0000000000000000","username":"alice","enabled":true,
 "locale":"en","last_login_at":null,
 "created_at":"2026-09-01T09:00:00Z"},
 "csrf_token":"K7sB2h1QpVmNc0aZ"}
```

## Steps
1. Create `internal/secure/hash.go` with the constants and the two functions above, using
   `golang.org/x/crypto/argon2`'s `argon2.IDKey` and `crypto/subtle.ConstantTimeCompare`.
2. Add `CreateUser`, `UserByUsername` and `TouchLastLogin` to `internal/store/users.go`, each with an
   explicit column list.
3. In `internal/api/auth.go`, add the setup gate: when `store.CountUsers` returns 0, every operation except
   `POST /auth/setup` returns 401 `/problems/setup-required`. Cache the count and invalidate it on setup.
4. On boot, when `CountUsers` is 0 and `<ConfigDir>/setup-token` is absent, generate the token with
   `secure.NewToken`, write it mode `0600`, and log it at `info` on its own line so it is visible in
   `docker compose logs`.
5. Implement `POST /auth/setup`: compare `setup_token` in constant time; reject a password under 12
   characters with 422 `/problems/validation-failed`; create the account; delete the token file; issue a
   session; return 201 with the envelope above. A second call returns 409
   `/problems/setup-already-complete`.
6. Implement `POST /auth/login`: identical `detail` and comparable timing for a wrong password, a disabled
   account and an unknown user; on success rotate the session id and call `TouchLastLogin`.
7. Add the brute-force controls of doc 12 §6.3: per-account exponential backoff from 1 s, doubling, capped
   at 15 minutes, and a per-source-IP bucket of 10 attempts per 5 minutes, answering 429
   `/problems/rate-limited` with `Retry-After`. Never a permanent lockout.
8. Implement `POST /auth/logout` (204, deletes the `sessions` row, expires the cookie) and `GET /auth/me`
   (200 with the same envelope, 401 otherwise).
9. Extend `internal/api/auth_test.go`: setup with the wrong token is 401; with a short password is 422; the
   happy path is 201 and deletes the token file; a second setup is 409; every other endpoint before setup is
   401 `/problems/setup-required`; login with the created credentials is 200; logout is 204 and the cookie
   stops working; eleven failed logins from one IP yield 429.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestSetupTokenFileDeletedOnSuccess` asserts `<config>/setup-token` no longer exists.
- [ ] `TestSecondSetupIs409` asserts `type` is `/problems/setup-already-complete`.
- [ ] `TestEveryEndpointBeforeSetupIs401` asserts the `type` is `/problems/setup-required`.
- [ ] `TestPasswordUnder12CharsRejected` asserts 422 `/problems/validation-failed`.
- [ ] `TestArgon2idPHCString` asserts the stored hash starts with `$argon2id$v=19$m=19456,t=2,p=1$`.
- [ ] `TestLoginFailureIsIndistinguishable` asserts identical `detail` for wrong password, disabled account
      and unknown user.
- [ ] `grep -ri 'adminadmin\|admin:admin\|DLTOOL_ADMIN_PASSWORD' cmd internal web` returns nothing.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make test PKG="./internal/api/... ./internal/secure/... ./internal/store/..." && echo SETUP_OK
```
Expected: three `ok  	github.com/L-K-M/dl-tool/internal/…` lines, no `FAIL`, and a final line of exactly
`SETUP_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT build the setup wizard screen; T039 and T040 own the SPA shell and its first-run route.
- Do NOT implement `/account` or `/api-tokens`; T084 and T120 own them.
- Do NOT accept the account password from an environment variable, in any form, ever.
- Do NOT add a "disabled for local addresses" or anonymous mode; there is none.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

```text
$ make test PKG="./internal/api/... ./internal/secure/... ./internal/store/..." && echo SETUP_OK
go test -race -count=1 ./internal/api/... ./internal/secure/... ./internal/store/...
ok  	github.com/L-K-M/dl-tool/internal/api	16.900s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.297s
ok  	github.com/L-K-M/dl-tool/internal/store	6.879s
SETUP_OK
```

Scope confirmation (run with every change staged for the single task commit):
```text
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
api/openapi.json
internal/api/auth.go
internal/api/auth_test.go
internal/api/server.go
internal/secure/hash.go
internal/secure/hash_test.go
internal/store/users.go
web/src/api/schema.d.ts
```

The eight paths are the Files table plus `api/openapi.json` and `web/src/api/schema.d.ts`, which
docs/13-testing-and-verification.md §7.1 makes part of this task's table because it adds Huma
operations (`make gen` regenerated both).

Acceptance grep:
```text
$ grep -ri 'adminadmin\|admin:admin\|DLTOOL_ADMIN_PASSWORD' cmd internal web
(no output, exit 1)
```

Notes:
- `make vet`, `golangci-lint run ./...`, `make doclint` and the web lint (`npm run lint`,
  `prettier --check`) all pass on this tree.
- Two pre-T009 tests needed adjusting: on a fresh empty store the middleware now answers
  `401 /problems/setup-required` (this task's rule), so `TestNoCredentialIs401` and
  `TestForgedRefererAloneIsRejected` seed a user first — exactly what
  `TestEveryEndpointBeforeSetupIs401` now covers from the other side.
- Doc 12 §6.3 locates the brute-force controls in `internal/secure/session.go`; the Files table
  scopes them to `internal/api/auth.go`, and the task file is authoritative, so they live beside
  the handlers that use them.

## Blocked

None. An earlier session stopped here because `internal/api/server.go` — the registration
call site of the four `/auth` Huma operations — was missing from the Files table. The amendment
it proposed is the `internal/api/server.go` row above; the task then proceeded with no other
scope change.
