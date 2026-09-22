# T084 — Issue and revoke API tokens

| Field | Value |
|---|---|
| **ID** | T084 |
| **Milestone** | M6 |
| **Status** | done |
| **Depends on** | T008, T009 |
| **Blocks** | T106, T107, T120 |
| **Parallel-safe** | no — it also edits the shared files `internal/api/auth.go`, `internal/api/server.go`, `internal/store/users.go` |
| **Implements** | [FR-117](../02-requirements.md#fr-117-issue-and-revoke-api-tokens), [NFR-016](../02-requirements.md#nfr-016-keep-api-tokens-revocable-and-out-of-the-logs) |
| **Decisions** | [ADR-0013](../decisions/0013-mandatory-built-in-authentication.md) |
| **Est. size** | 2 new files, ~290 LOC |

## Goal
`POST /api-tokens` returns a bearer token exactly once; `GET /api-tokens` lists only prefixes and labels;
`DELETE /api-tokens/{id}` revokes immediately, so the next request carrying that token returns `401`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §12 The account and API tokens](../05-api-contract.md#12-the-account-and-api-tokens)
2. [`docs/05-api-contract.md` §2 Endpoint summary](../05-api-contract.md#2-endpoint-summary)
3. [`docs/04-data-model.md` §3.1 Identity and access](../04-data-model.md#31-identity-and-access)
4. [`docs/12-security-and-threat-model.md` §6.1 Sessions and cookies](../12-security-and-threat-model.md#61-sessions-and-cookies)
5. [`docs/02-requirements.md` FR-117](../02-requirements.md#fr-117-issue-and-revoke-api-tokens)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/tokens.go` | create | The three token operations and the create-once reveal. |
| `internal/api/tokens_test.go` | create | Reveal-once, listing and revocation cases. |
| `internal/store/users.go` | modify | Add `CreateAPIToken`, `ListAPITokens`, `RevokeAPIToken`, `TouchAPIToken`. |
| `internal/api/server.go` | modify | Register `list-api-tokens`, `create-api-token`, `revoke-api-token`. |

No other file may be modified.

## Interface contract

```go
package api

// TokenView is the only shape ever returned for an existing token. There is no secret member.
type TokenView struct {
	ID         string     `json:"id"`          // tok_ + ULID
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`      // first 8 characters, display only
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// CreateTokenOutput is the ONLY response that ever carries Token. The value is not stored in clear
// text, is never logged, and is never returned again by any endpoint.
type CreateTokenOutput struct {
	Body struct {
		TokenView
		Token string `json:"token"` // "dlt_" + 32 hex characters
	}
}

type ListTokensOutput struct {
	Body struct {
		Items      []TokenView `json:"items"`
		NextCursor *string     `json:"next_cursor"`
		Total      int         `json:"total"`
	}
}

func (h *TokenHandlers) List(ctx context.Context, in *ListTokensInput) (*ListTokensOutput, error)
func (h *TokenHandlers) Create(ctx context.Context, in *CreateTokenInput) (*CreateTokenOutput, error)
func (h *TokenHandlers) Revoke(ctx context.Context, in *RevokeTokenInput) (*struct{}, error)

```

```go
package store

// CreateAPIToken stores only the SHA-256 hex of the token value and its 8-character prefix.
func (s *UserStore) CreateAPIToken(ctx context.Context, userID, name string, expiresAt *int64) (id, prefix, hash string, err error)

// ListAPITokens returns every token, per doc 05 §12. dl-tool has one account.
func (s *UserStore) ListAPITokens(ctx context.Context, userID string, limit int, cursor string) ([]APIToken, string, error)

// RevokeAPIToken sets revoked_at. The row is kept so an audit trail survives.
func (s *UserStore) RevokeAPIToken(ctx context.Context, userID, id string) error

// TouchAPIToken writes last_used_at at most once a minute per token.
func (s *UserStore) TouchAPIToken(ctx context.Context, id string, at int64) error
```

Statuses: `200`/`201`/`204` · `401` for a revoked or expired token · `404`.

## Steps
1. Add the four token functions to `internal/store/users.go` with explicit column lists; never select
   `token_hash` into a struct that reaches an API response.
2. Create `internal/api/tokens.go` with `TokenView`, the three operations and the `dlt_` + 32 hex value
   generated from `crypto/rand`.
3. Return the clear-text token only from `Create`, and make it structurally impossible elsewhere by giving
   `TokenView` no secret member.
4. Edit `internal/api/auth.go` so bearer authentication rejects a token whose `revoked_at` is set or whose
   `expires_at` has passed with `401`, and calls `TouchAPIToken` at most once a minute.
5. Ensure the token value never reaches a log record: log the `tok_` id and the prefix, never the value or
   its hash.
6. Create `internal/api/tokens_test.go` with `humatest`: assert the creation response carries `token` and
   the list response carries none; assert a request with the token succeeds and the same request after
   `DELETE` returns `401`; assert an expired token returns `401`; assert no log line contains the token
   value.
7. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [x] The clear-text token appears in the creation response and in no other response, ever —
  `TokenView` has no secret member (`internal/api/tokens.go`), proven by `TestTokenRevealedOnce`.
- [x] `GET /api-tokens` returns `prefix` and `name` only, never a token value — `TestListHasNoSecret`
  also asserts neither the value nor its SHA-256 hash appears.
- [x] A revoked token's next request returns `401` — `TestRevokedTokenIs401` (seeded row) and
  `TestDeleteRevokesImmediately` (through `DELETE /api-tokens/{id}`).
- [x] An expired token returns `401` without being revoked — `TestExpiredTokenIs401` (seeded row) and
  `TestExpiredCreatedTokenIs401` (through `POST` with a past `expires_at`).
- [x] The token value appears in no log record and no error message — `TestTokenNeverLogged` scans
  every captured slog record across issue, use and revoke; the store never sees the clear value.
- [x] Review check — step 4's `auth.go` behaviour predates this task: T008's
  `queryUserByAPITokenHash` already filters `revoked_at`/`expires_at` in SQL, and its stamp now
  delegates to `UserStore.TouchAPIToken`, whose `WHERE` clause bounds `last_used_at` to one write per
  minute. `auth.go` is not in the Files table and needed no edit.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/api/... ./internal/store/..." && echo TOKENS_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/api` and `ok  github.com/L-K-M/dl-tool/internal/store`,
with `TestTokenRevealedOnce`, `TestListHasNoSecret`, `TestRevokedTokenIs401`, `TestExpiredTokenIs401` and
`TestTokenNeverLogged` each reported as `--- PASS`. The final line of stdout is exactly `TOKENS_OK`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT store the token value, a reversible encryption of it, or a hint beyond the 8-character prefix.
- Do NOT add a token scope, permission or role system; a token carries exactly the operator account's access.
- Do NOT delete a revoked row; `revoked_at` keeps the audit trail.
- Do NOT implement any third party's authentication protocol; a bearer token plus `POST /tasks` is the
  entire integration surface.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
`make lint && make test PKG="./internal/api/... ./internal/store/..." && echo TOKENS_OK`:

```
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
go test -race -count=1 ./internal/api/... ./internal/store/...
ok  	github.com/L-K-M/dl-tool/internal/api	158.370s
ok  	github.com/L-K-M/dl-tool/internal/store	76.866s
TOKENS_OK
```

The five named tests plus the endpoint-level additions, individually:

```
--- PASS: TestTokenRevealedOnce (0.45s)
--- PASS: TestListHasNoSecret (0.43s)
--- PASS: TestDeleteRevokesImmediately (0.38s)
--- PASS: TestExpiredCreatedTokenIs401 (0.35s)
--- PASS: TestTokenListPaginates (0.43s)
--- PASS: TestTokenNeverLogged (0.41s)
--- PASS: TestRevokedTokenIs401 (0.42s)
--- PASS: TestExpiredTokenIs401 (0.39s)
ok  	github.com/L-K-M/dl-tool/internal/api	4.521s
```

Scope (the Files-table paths plus the §7.1-implicit generated pair `api/openapi.json` and
`web/src/api/schema.d.ts`):

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
api/openapi.json
internal/api/server.go
internal/api/tokens.go
internal/api/tokens_test.go
internal/store/users.go
web/src/api/schema.d.ts
```

Interface-contract adaptations, same file scope: `UserStore.CreateAPIToken` takes the prepared
`APIToken` row (the `CreateSession`/`CreateCategory` convention) instead of the sketched
`(userID, name, expiresAt) → (id, prefix, hash)` — the sketch cannot hand the caller the reveal-once
value, and per step 2 the `dlt_` + 32-hex value is minted in `tokens.go` from `crypto/rand`, so only
its hash and prefix cross the store boundary. `ListAPITokens` also returns `total` (the §1.4
envelope requires it). The middleware caller of `UserByAPITokenHash` is `auth.go`, outside the Files
table, so its signature is unchanged and it delegates the throttled stamp to `TouchAPIToken`.

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
