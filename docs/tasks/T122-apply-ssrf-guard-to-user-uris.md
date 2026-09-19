# T122 — Apply the SSRF guard to user-submitted URIs

| Field | Value |
|---|---|
| **ID** | T122 |
| **Milestone** | M4 |
| **Status** | done |
| **Depends on** | T020, T031, T123 |
| **Blocks** | — |
| **Parallel-safe** | no — it edits the shared `internal/api/tasks.go` and `internal/api/server.go` |
| **Implements** | [NFR-017](../02-requirements.md#nfr-017-block-server-side-request-forgery) — the `POST /tasks` and `POST /tasks/inspect` half |
| **Decisions** | [ADR-0001](../decisions/0001-control-plane-over-existing-engines.md), [ADR-0003](../decisions/0003-chi-huma-code-first-openapi.md) |
| **Est. size** | 2 new files, 3 edits, ~260 LOC |

## Goal
`POST /api/v1/tasks` and `POST /api/v1/tasks/inspect` resolve every `http`, `https`, `ftp`, `ftps` and
`sftp` URI through the guard before an engine is asked to fetch it. A blocked URI is reported as
`/problems/ssrf-blocked`, leaves the task in `error` with `error_code = 'ssrf_blocked'`, and never reaches
aria2, qBittorrent or yt-dlp.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/12-security-and-threat-model.md` §2.3 Private addresses and the engine sidecars](../12-security-and-threat-model.md#23-private-addresses-and-the-engine-sidecars)
   — the guard governs URLs originating from a user; sidecar RPC bypasses it by construction.
2. [`docs/12-security-and-threat-model.md` §2.4 Caps and diagnosability](../12-security-and-threat-model.md#24-caps-and-diagnosability)
   — one `warn` record per block, and the `tasks.error_code` / `/problems/ssrf-blocked` pair.
3. [`docs/05-api-contract.md` §5.2 `POST /tasks`](../05-api-contract.md#52-post-tasks) — the `rejected[]`
   shape and the full status list, including `403` `/problems/ssrf-blocked`.
4. [`docs/05-api-contract.md` §5.3 `POST /tasks/inspect`](../05-api-contract.md#53-post-tasksinspect)
   — inspecting never creates a task, so a block there is status-only.
5. [`docs/05-api-contract.md` §1.3 Errors](../05-api-contract.md#13-errors--rfc-9457-applicationproblemjson).
6. [`docs/04-data-model.md` §4.2 `tasks.error_code`](../04-data-model.md#42-taskserror_code) — the closed
   enum containing `ssrf_blocked`.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/secure/preflight.go` | create | `Resolver` and `PreflightURI`: scheme, port and every resolved address. |
| `internal/api/tasks.go` | modify | Add the guard and resolver to `TaskHandlers`, the `preflight` helper, and the `CreateTasks` block path. |
| `internal/api/tasks_inspect.go` | modify | Preflight each submission before building a manifest. |
| `internal/api/server.go` | modify | Build one `secure.Guard` from `cfg.SSRFAllowPrivate` and pass it to `NewTaskHandlers`. |
| `internal/api/tasks_ssrf_test.go` | create | `humatest` cases for both endpoints plus the `PreflightURI` unit cases. |
| `internal/api/search.go` | modify | *Widened mid-task, see [`## Blocked`](#blocked):* swap its two `secure.NewGuard` call sites for the `newSSRFGuard` helper in `server.go` — the acceptance grep requires the constructor's literal name in `server.go` only, and these calls landed after this file was written. |
| `internal/api/search_test.go` | modify | *Widened mid-task:* same grep criterion — its two `secure.NewGuard` calls route through `newSSRFGuard`. |
| `internal/api/tasks_test.go` | modify | *Widened mid-task:* the shared env and the late-resolution env inject the test resolver, and the late task is seeded through the store — its `127.0.0.1:<port>` stub URL is unfixably blocked by the port rule. |
| `internal/api/tasks_inspect_test.go` | modify | *Widened mid-task:* the inspect env injects the test resolver. |
| `internal/api/submission_test.go` | modify | *Widened mid-task:* `newCapableQBTEnv` injects the test resolver, keeping every submission-capable env consistent. |
| `internal/api/tasks_actions_test.go` | modify | *Widened mid-task:* the direct `NewTaskHandlers` call gains the two new arguments (`nil, nil` — it never creates or inspects). |
| `internal/api/tasks_files_test.go` | modify | *Widened mid-task:* the direct `NewTaskHandlers` call gains the two new arguments (`nil, nil` — it never creates or inspects). |
|| `internal/api/tasks_actions.go` | modify | *Widened in review, see [`## Blocked`](#blocked):* `error_code` joins `actionTask`, and `applyAction` refuses every lifecycle action but `remove` on an `ssrf_blocked` row — otherwise `error → queued` via `resume` requeued the refused URI and the admission pass handed it to an engine. |

No other file may be modified.

## Interface contract

```go
package secure

// Resolver is satisfied by *net.Resolver. A test substitutes a static map so no DNS query is made.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// PreflightURI validates a user-submitted URI before any engine is asked to fetch it, because the
// engines dial for themselves and the dialer guard of ssrf.go never sees their connections.
//
// It returns nil for a scheme the guard does not govern: magnet, a bare infohash and the obfuscated
// thunder, flashget and qqdl forms are parsed in process and never fetched, and an unsupported
// scheme is already rejected by the router.
//
// For http and https an explicit port other than 80 or 443 is blocked, matching rule 5 of
// 12-security-and-threat-model.md §2.2. For ftp, ftps and sftp the port is not constrained; the
// address check is what protects the LAN there.
//
// A literal-IP host is checked without a lookup. Otherwise EVERY address the host resolves to must
// pass g.AllowAddr: one blocked answer blocks the URI, and an empty or failing lookup blocks it too.
func PreflightURI(ctx context.Context, g *Guard, r Resolver, rawURI string) error
```

```go
package api

// TaskHandlers gains two fields; NewTaskHandlers gains the two arguments. internal/api/server.go
// passes secure.NewGuard(log, cfg.SSRFAllowPrivate) and net.DefaultResolver.
type TaskHandlers struct {
	// … the store, registry and roots fields of T020 …
	guard    *secure.Guard
	resolver secure.Resolver
}

// preflight validates one submitted URI. It returns nil when the guard does not govern the scheme,
// and a *secure.BlockedError otherwise.
func (h *TaskHandlers) preflight(ctx context.Context, rawURI string) error

// ssrfRejection is the rejected[] entry for a blocked URI. The detail carries the redacted URL only:
// a resolved address or a matched prefix goes to the log record, never to an API response.
func ssrfRejection(rawURI string) RejectedURI {
	return RejectedURI{
		URI:    secure.RedactURL(rawURI),
		Type:   SlugSSRFBlocked,
		Detail: "the URL resolved to a blocked address range",
	}
}
```

Behaviour, exactly this:

| Endpoint | A URI is blocked | Every URI in the submission is blocked |
|---|---|---|
| `POST /tasks` | the row created for it moves to `error` with `error_code = 'ssrf_blocked'`, the engine is never called, and the URI appears in `rejected[]`, never in `created[]` | `403` `/problems/ssrf-blocked` |
| `POST /tasks/inspect` | the URI appears in `rejected[]`; no row is written | `403` `/problems/ssrf-blocked` |

## Steps
1. Create `internal/secure/preflight.go` with `Resolver` and `PreflightURI`. Parse with `net/url`, lower-case
   the scheme, and return `nil` immediately for any scheme outside `http`, `https`, `ftp`, `ftps`, `sftp`.
2. Reject an `http` or `https` URI whose explicit port is neither `80` nor `443` with a
   `*BlockedError` whose `Reason` is `port`; leave the port unconstrained for the three file-transfer schemes.
3. Resolve the host: `netip.ParseAddr` first for a literal, otherwise `r.LookupNetIP(ctx, "ip", host)`.
   A lookup error or an empty answer returns a `*BlockedError` with `Reason` `resolve`; otherwise call
   `g.AllowAddr` on every answer and return the first denial.
4. In `internal/api/tasks.go`, add the `guard` and `resolver` fields, extend `NewTaskHandlers` with the two
   arguments, and add `preflight`, which delegates to `secure.PreflightURI` and returns its error unchanged.
5. In `CreateTasks`, call `h.preflight` for each URI after `store.Create` and before `Engine.Add`. On a
   block, call `store.Transition(ctx, id, "error", "ssrf_blocked", "blocked by the SSRF guard")`, skip the
   `Engine.Add` call entirely, and append `ssrfRejection(uri)` to `rejected[]`.
6. Return `403` `/problems/ssrf-blocked` through `Problem(SlugSSRFBlocked, http.StatusForbidden, …)` when
   every URI of the submission was blocked; otherwise return `201` with the surviving `created[]`.
7. In `internal/api/tasks_inspect.go`, call `h.preflight` on each entry before the kind branch of T031 step
   8, append `ssrfRejection(uri)` for a block, write nothing to `tasks`, and apply the same
   all-blocked `403` rule. Leave the `413` and `422` branches untouched.
8. In `internal/api/server.go`, build one `secure.Guard` with `secure.NewGuard(log, cfg.SSRFAllowPrivate)`
   and pass it with `net.DefaultResolver` into `NewTaskHandlers`. Build it once per server, not per request.
9. Create `internal/api/tasks_ssrf_test.go` with a `staticResolver` mapping `blocked.example` to
   `169.254.169.254` and `public.example` to `93.184.216.34`, and a fake engine that records every `Add`.
10. Add the endpoint cases: `TestCreateTasksBlocksLoopbackURI` (`http://127.0.0.1/x` → `403`, fake engine
    recorded no `Add`), `TestCreateTasksMarksBlockedTaskError` (`SELECT state, error_code FROM tasks` is
    `error` and `ssrf_blocked`), `TestCreateTasksMixedSubmission` (one blocked and one public URI → `201`,
    one entry in `created[]` and one in `rejected[]` typed `/problems/ssrf-blocked`) and
    `TestInspectBlocksBlockedHost` (`403`, `SELECT count(*) FROM tasks` unchanged).
11. Add the unit cases: `TestPreflightIgnoresMagnet`, `TestPreflightBlocksNonStandardHTTPPort`,
    `TestPreflightAllowsSFTPOnPort2222`, `TestPreflightBlocksWhenOneAnswerIsPrivate` and
    `TestPreflightRedactsUserinfo` (the `rejected[]` URI of `ftp://u:p@blocked.example/f` carries no
    password).
12. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [x] `POST /tasks` with `http://127.0.0.1:8080/x` returns `403` and the body `type` is `/problems/ssrf-blocked`.
- [x] The task created for a blocked URI ends in state `error` with `error_code = 'ssrf_blocked'`, and the fake engine recorded no `Add` for it.
- [x] A submission of one blocked and one public URI returns `201` with exactly one `created[]` and one `rejected[]` entry.
- [x] `POST /tasks/inspect` with a blocked host returns `403` and leaves `SELECT count(*) FROM tasks` unchanged.
- [x] `PreflightURI` returns nil for `magnet:?xt=urn:btih:…` and for a bare 40-hex infohash.
- [x] No response body or `rejected[]` entry contains a resolved IP, a matched prefix, a userinfo password or a query string.
- [x] `grep -rn "secure.NewGuard" internal/api` matches only `internal/api/server.go`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/api/... ./internal/secure/..." && echo SSRF_WIRED_OK
```
Expected: `ok  	github.com/L-K-M/dl-tool/internal/api`, `ok  	github.com/L-K-M/dl-tool/internal/secure`,
every test named in steps 10 and 11 listed as passing, no `FAIL` and no `SKIP` line, and the final line of
stdout exactly `SSRF_WIRED_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table (widened rows included) and nothing else. Use `git status`,
not `git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT touch the Torznab client or `internal/search/`; T054 owns the indexer fetch path and its own guard
  call site.
- Do NOT change `internal/secure/ssrf.go`; T123 owns the guard, the prefix tables and the client.
- Do NOT wire the guard into the feed poller, the notifier or the dlsearch runner; T066, T077 and T058 own
  those call sites.
- Do NOT add a per-indexer `allow_private_network` flag or a per-request override of `DLTOOL_SSRF_ALLOW_PRIVATE`.
- Do NOT preflight `magnet:`, a bare infohash, `thunder://`, `flashget://` or `qqdl://`: they are parsed in
  process and never fetched, and a decoded inner URI is preflighted by the normal `http`/`ftp` path.
- Do NOT route `DLTOOL_ARIA2_URL` or `DLTOOL_QBITTORRENT_URL` through the guard; they are infrastructure
  configuration (doc 12 §2.3).

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

`make lint && make test PKG="./internal/api/... ./internal/secure/..." && echo SSRF_WIRED_OK`
(exact command of the Verification block, run on the task branch's final tree):

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
go test -race -count=1 ./internal/api/... ./internal/secure/...
ok  	github.com/L-K-M/dl-tool/internal/api	130.954s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.650s
SSRF_WIRED_OK
```

No `FAIL`, no `SKIP`. The nine named cases run under `internal/api` and pass:
`TestCreateTasksBlocksLoopbackURI`, `TestCreateTasksMarksBlockedTaskError`,
`TestCreateTasksMixedSubmission`, `TestInspectBlocksBlockedHost`,
`TestPreflightIgnoresMagnet`, `TestPreflightBlocksNonStandardHTTPPort`,
`TestPreflightAllowsSFTPOnPort2222`, `TestPreflightBlocksWhenOneAnswerIsPrivate`,
`TestPreflightRedactsUserinfo`. The two review-driven cases pass with them:
`TestBlockedTaskCannotResume` and `TestCreateTasksBlockedAndJunk`.

Scope check — the review fix landed on top of the task commit, so `git status` on the working tree
shows only its delta; the cumulative branch diff against `origin/main` is the Files table exactly:

```
internal/api/search.go
internal/api/search_test.go
internal/api/server.go
internal/api/submission_test.go
internal/api/tasks.go
internal/api/tasks_actions.go
internal/api/tasks_actions_test.go
internal/api/tasks_files_test.go
internal/api/tasks_inspect.go
internal/api/tasks_inspect_test.go
internal/api/tasks_ssrf_test.go
internal/api/tasks_test.go
internal/secure/preflight.go
```

Exactly the Files table (widened rows included) and nothing else. The `secure.NewGuard` grep matches
`internal/api/server.go` only:

```
internal/api/server.go:func newSSRFGuard(log *slog.Logger, allowPrivate bool) *secure.Guard {
internal/api/server.go:	return secure.NewGuard(log, allowPrivate)
internal/api/server.go:	taskGuard := secure.NewGuard(log, cfg.SSRFAllowPrivate)
```

## Blocked

*Resolved by the Files-table widening recorded in the table itself — kept here because it forced edits
outside the original five-row table, following the precedent T100 set for exactly this situation.*

**The written contract predates three pieces of the tree it edits.**

1. The `secure.NewGuard` grep criterion could not pass: `internal/api/search.go` gained two
   `secure.NewGuard` call sites under T055/T061 — after this file was written — and
   `internal/api/search_test.go` has two more. Deleting them would change search behavior (they build
   per-request origin-scoped guards), so they route through `newSSRFGuard` in `server.go`, which keeps
   the literal `secure.NewGuard(log, …)` in the composition root alone. Semantics are identical.
2. `NewTaskHandlers` gained the two contracted arguments, which breaks its two direct test callers —
   `tasks_actions_test.go` and `tasks_files_test.go`. Both only exercise actions/patches, so they pass
   `nil, nil`; `PreflightURI` fails closed on a nil pair, which those paths never reach.
3. `server.go` wires `net.DefaultResolver` as required, but the shared submission envs post fixture
   hostnames (`releases.example.com`, `mirror.example.org`, …) that resolve nowhere in a test
   environment — they were written before a lookup could run at all. The envs substitute a resolver
   answering every host with one public address. `newLateResolutionEnv`'s submission is a real
   `http://127.0.0.1:<port>` stub-daemon URL that the port rule refuses under every switch, so its
   late task is seeded through the store in the same shape the endpoint wrote — the daemon fetch,
   identity write-back and duplicate pause the test proves are unchanged.

**Two contract adaptations, no scope change.** Step 5's insert-then-`Transition` sketch predates the
admission pass owning `Engine.Add` (T098) and `Transition`'s current signature — its `code` argument
is a task_events code and it never writes `tasks.error_code`. The blocked row is instead created
directly in `error` with `error_code = 'ssrf_blocked'` via `CreateLogged` (which writes both at
insert), which is strictly stronger: the row never sits in `queued` where the 1 Hz pass could claim
it between two writes. Preflight scope is the `uris` family as the Steps specify; a
`search_result_ids` submission resolves server-stored provider sources, and whether those should also
preflight is a plan-level question the task does not answer — flagged here for the plan owner rather
than silently extrapolated.

**The error row could be requeued through the actions endpoint.** The diff-review pass found that
`error → queued` is a legal transition and `POST /tasks/actions` `resume` applied it unconditionally,
so a blocked row's stored `source_uri` would reach `Engine.Add` through the admission pass — the very
hole the guard exists to close. `tasks_actions.go` therefore loads `error_code` into `actionTask` and
`applyAction` refuses every lifecycle action but `remove` on an `ssrf_blocked` row (the per-id outcome
is `/problems/ssrf-blocked`). The `error_code` survives a `paused` stopover — only `disk_full` and
`concurrency_limit` are ever cleared — so pause-then-resume is covered too, and `force_complete` is a
dead end (`completed` has no edge back to `queued` and the row has no `engine_ref` for the reconciler).
`TestBlockedTaskCannotResume` pins the gate and `TestCreateTasksBlockedAndJunk` pins the 403's boundary:
it fires only when every submitted URI was blocked, matching "every URI in the submission is blocked"
in the contract table and doc 05 §5.2's "every URI was rejected → 422".
