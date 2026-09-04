# T008 — Authenticate every request with a session cookie or a bearer token

| Field | Value |
|---|---|
| **ID** | T008 |
| **Milestone** | M0 |
| **Status** | done |
| **Depends on** | T006, T007 |
| **Blocks** | T009, T020, T046, T055, T065, T068, T084 |
| **Parallel-safe** | no — it edits `internal/api/server.go` |
| **Implements** | [FR-116](../02-requirements.md#fr-116-authenticate-with-a-session-cookie-or-a-bearer-token), [NFR-012](../02-requirements.md#nfr-012-protect-against-csrf-with-a-synchroniser-token) |
| **Decisions** | [ADR-0013](../decisions/0013-mandatory-built-in-authentication.md) |
| **Est. size** | 3 new source files, 1 test file, ~380 LOC |

## Goal
Every request under `/api/v1` carries either a `dltool_session` cookie or `Authorization: Bearer dlt_…`.
A cookie-authenticated mutation without a matching `X-DLTOOL-CSRF` header is rejected with 403
`/problems/csrf-token-missing`; a bearer-authenticated mutation needs no header. An absent or expired
credential is 401 `/problems/unauthenticated`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §1.2 Authentication and authorisation](../05-api-contract.md#12-authentication-and-authorisation)
   — the two credentials, the cookie attributes and every status code.
2. [`docs/12-security-and-threat-model.md` §6.1 Sessions and cookies](../12-security-and-threat-model.md#61-sessions-and-cookies)
   and [§6.2 CSRF](../12-security-and-threat-model.md#62-csrf) — the three CSRF layers and the cookie table.
3. [`docs/04-data-model.md` §3.1 Identity and access](../04-data-model.md#31-identity-and-access) — the
   `users`, `sessions` and `api_tokens` columns.
4. [`docs/14-conventions.md` §2.4 SQL and sqlx](../14-conventions.md#24-sql-and-sqlx).

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/store/users.go` | create | Row structs and queries for the single `users` row, `sessions` and `api_tokens`. |
| `internal/secure/session.go` | create | Token minting, SHA-256 hashing, constant-time CSRF comparison. |
| `internal/api/auth.go` | create | The authentication and CSRF middleware and the request identity. |
| `internal/api/auth_test.go` | create | `humatest` coverage of 401, 403 and 2xx for both credentials. |
| `internal/api/server.go` | edit | Install the middleware on the `/api/v1` sub-router. |

No other file may be modified.

## Interface contract

```go
package store

type User struct {
	ID           string `db:"id" json:"id"`
	Username     string `db:"username" json:"username"`
	PasswordHash string `db:"password_hash" json:"-"`
	Enabled      bool   `db:"enabled" json:"enabled"`
	Locale       string `db:"locale" json:"locale"`
	LastLoginAt  *int64 `db:"last_login_at" json:"-"`
	CreatedAt    int64  `db:"created_at" json:"-"`
	UpdatedAt    int64  `db:"updated_at" json:"-"`
}

type Session struct {
	ID         string `db:"id"`
	UserID     string `db:"user_id"`
	TokenHash  string `db:"token_hash"`   // SHA-256 hex of the cookie value
	CSRFToken  string `db:"csrf_token"`
	ExpiresAt  int64  `db:"expires_at"`   // unix ms
	LastSeenAt int64  `db:"last_seen_at"`
}

func CountUsers(ctx context.Context, db *sqlx.DB) (int64, error)
func UserByID(ctx context.Context, db *sqlx.DB, id string) (User, error)
func CreateSession(ctx context.Context, db *sqlx.DB, s Session) error
func SessionByTokenHash(ctx context.Context, db *sqlx.DB, hash string) (Session, User, error)
func TouchSession(ctx context.Context, db *sqlx.DB, id string, now int64) error
func DeleteSession(ctx context.Context, db *sqlx.DB, id string) error
func DeleteExpiredSessions(ctx context.Context, db *sqlx.DB, now int64) (int64, error)
func UserByAPITokenHash(ctx context.Context, db *sqlx.DB, hash string) (User, error)
```

```go
package secure

// NewToken returns 32 bytes from crypto/rand, base64url-encoded without padding.
func NewToken() (string, error)

// HashToken returns the lowercase SHA-256 hex of a session cookie value or a bearer token.
func HashToken(token string) string

// EqualToken compares two tokens in constant time.
func EqualToken(a, b string) bool
```

```go
package api

// Identity is what the middleware puts on the request context.
type Identity struct {
	User   store.User
	Method string // "session" | "token"
	CSRF   string // the session's csrf_token; empty for token authentication
}

// IdentityFrom returns the caller's identity, or ok == false when unauthenticated.
func IdentityFrom(ctx context.Context) (Identity, bool)

// Authenticate resolves the cookie or the bearer token, enforces CSRF on
// POST, PUT, PATCH and DELETE made with cookie authentication, and answers
// 401 /problems/unauthenticated or 403 /problems/csrf-token-missing.
func Authenticate(db *sqlx.DB, cfg *config.Config) func(http.Handler) http.Handler
```

Cookie, exactly as doc 05 §1.2 and doc 12 §6.1 specify:

```
Set-Cookie: dltool_session=<opaque>; Path=<base>/; HttpOnly; SameSite=Lax[; Secure]
```

## Steps
1. Create `internal/store/users.go` with the structs and functions above. Write an explicit column list in
   every statement; `SELECT *` is forbidden. Return `ErrNotFound` wrapped for `sql.ErrNoRows`.
2. `SessionByTokenHash` joins `sessions` to `users`, rejects a row whose `expires_at` is in the past, and
   rejects a user whose `enabled` is 0.
3. `UserByAPITokenHash` rejects a token whose `revoked_at` is set or whose `expires_at` has passed, and
   updates `last_used_at`.
4. Create `internal/secure/session.go` with `NewToken`, `HashToken` and `EqualToken`
   (`crypto/subtle.ConstantTimeCompare`). The cookie value itself is never stored, only its hash.
5. Create `internal/api/auth.go` with `Authenticate`. Order: bearer header first, then cookie. A bearer
   value must carry the `dlt_` prefix; hash it and look it up.
6. For cookie authentication on `POST`, `PUT`, `PATCH` or `DELETE`, require `X-DLTOOL-CSRF` and compare it
   with the session's `csrf_token` using `EqualToken`. Bearer requests skip the check entirely.
7. Add the second CSRF layer: when `Origin` is present, or absent and `Referer` is present, its host must
   equal the request host; a mismatch is `403` `/problems/csrf-token-missing`.
8. Call `TouchSession` at most once a minute per session so the write rate stays bounded.
9. Edit `internal/api/server.go` to install `Authenticate` on the `/api/v1` sub-router only, so `/healthz`,
   `/readyz` and the SPA stay reachable without it.
10. Write `internal/api/auth_test.go` covering: no credential gives 401; a valid cookie with no
    `X-DLTOOL-CSRF` on `POST` gives 403 with `type` `/problems/csrf-token-missing`; the same `POST` with the
    header gives 2xx; a bearer token with no header on `POST` gives 2xx; an expired session gives 401; a
    revoked token gives 401; a forged `Referer` alone never grants access.
11. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [x] `TestCookieMutationWithoutCSRFIs403` asserts the status and the `type` slug.
- [x] `TestBearerMutationWithoutCSRFIs2xx` passes.
- [x] `TestExpiredSessionIs401` and `TestRevokedTokenIs401` pass.
- [x] `TestForgedRefererAloneIsRejected` passes; no code path treats `Referer` as sufficient.
- [x] The session cookie is set with `HttpOnly`, `SameSite=Lax` and `Path` equal to the base path plus `/`.
- [x] No session cookie value and no bearer token appears in any log record or error string.
- [x] `/healthz` and the SPA routes are not behind `Authenticate`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make test PKG="./internal/api/... ./internal/secure/... ./internal/store/..." && echo AUTH_OK
```
Expected: three `ok  	github.com/L-K-M/dl-tool/internal/…` lines, no `FAIL`, and a final line of exactly
`AUTH_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement `POST /auth/setup`, `POST /auth/login`, `POST /auth/logout` or `GET /auth/me`; T009 owns
  them and edits `internal/api/auth.go` again.
- Do NOT implement password hashing; T009 owns `internal/secure/hash.go`.
- Do NOT implement `/api-tokens` CRUD; T084 owns it.
- Do NOT create `internal/secure/csrf.go`. The CSRF token is a `sessions` column
  ([`04-data-model.md`](../04-data-model.md#31-identity-and-access)); it is minted in `session.go` and
  checked in `auth.go`.
- Do NOT add the login rate limiter; T009 owns the backoff and the `429`.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
```text
$ make test PKG="./internal/api/... ./internal/secure/... ./internal/store/..." && echo AUTH_OK
go test -race -count=1 ./internal/api/... ./internal/secure/... ./internal/store/...
ok  	github.com/L-K-M/dl-tool/internal/api	8.472s
?   	github.com/L-K-M/dl-tool/internal/secure	[no test files]
ok  	github.com/L-K-M/dl-tool/internal/store	7.444s
AUTH_OK
```

Deviation from the predicted output: the Verification block predicts three `ok` lines, but the Files
table allows exactly one test file (`internal/api/auth_test.go`), so `internal/secure` has no tests and
`go test` reports `?   	github.com/L-K-M/dl-tool/internal/secure	[no test files]` for it. Observed: two
`ok` lines, no `FAIL`, and a final line of exactly `AUTH_OK`.

Scope confirmation (run with every change staged for the single task commit):
```text
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
internal/api/auth.go
internal/api/auth_test.go
internal/api/server.go
internal/secure/session.go
internal/store/users.go
```
Exactly the paths in the Files table, and nothing else.

Audit regression: proxy TLS and cookie lifetime, verified through `Server.Router`.

```text
$ go test -race ./internal/api -run 'TestSessionCookie|TestRealIP|TestLogout' -count=1
ok  	github.com/L-K-M/dl-tool/internal/api	5.589s
$ make test PKG='./internal/api/... ./internal/secure/... ./internal/store/...' && echo AUTH_OK
go test -race -count=1 ./internal/api/... ./internal/secure/... ./internal/store/...
ok  	github.com/L-K-M/dl-tool/internal/api	45.001s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.106s
ok  	github.com/L-K-M/dl-tool/internal/store	59.765s
AUTH_OK
```

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
