# T095 — Harden the proxied deployment and ship the proxy snippets

| Field | Value |
|---|---|
| **ID** | T095 |
| **Milestone** | M7 |
| **Status** | todo |
| **Depends on** | T007, T013, T094 |
| **Blocks** | — |
| **Parallel-safe** | no — it edits `internal/api/server.go` |
| **Implements** | [NFR-006](../02-requirements.md#nfr-006-work-when-hosted-under-a-sub-path), [NFR-010](../02-requirements.md#nfr-010-always-verify-tls-certificates), [NFR-013](../02-requirements.md#nfr-013-reject-unexpected-host-headers), [NFR-021](../02-requirements.md#nfr-021-serve-strict-security-headers), [NFR-024](../02-requirements.md#nfr-024-validate-login-redirects-as-relative-paths), [NFR-030](../02-requirements.md#nfr-030-lock-configuration-from-the-environment) |
| **Decisions** | [ADR-0013](../decisions/0013-mandatory-built-in-authentication.md), [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md) |
| **Est. size** | 1 new source file, 1 test file and 2 proxy snippets, ~380 LOC |

## Goal
Every HTML response carries the eight documented security headers, an unexpected `Host` is answered `421`,
an operator-configuration mutation under `DLTOOL_CONFIG_LOCK` is answered `403`,
a login redirect is honoured only when it is a single-slash relative path, and the shipped Caddy and Traefik
snippets serve dl-tool at both a subdomain and a subfolder without buffering the event stream.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/12-security-and-threat-model.md` §6.6 Response headers](../12-security-and-threat-model.md#66-response-headers) — the header block, verbatim, and the HSTS rule.
2. [`docs/12-security-and-threat-model.md` §6.5 Host-header allowlist against DNS rebinding](../12-security-and-threat-model.md#65-host-header-allowlist-against-dns-rebinding) — the four allowlist rules.
3. [`docs/12-security-and-threat-model.md` §6.7 Open redirects, configuration lock, exposure](../12-security-and-threat-model.md#67-open-redirects-configuration-lock-exposure) — the redirect rule and `config_lock`.
4. [`docs/10-deployment-and-compose.md` §7.3 Base-path requirements](../10-deployment-and-compose.md#73-base-path-requirements) — the eight hard requirements.
5. [`docs/10-deployment-and-compose.md` §7.1 Caddy](../10-deployment-and-compose.md#71-caddy--deploycaddycaddyfileexample) and [§7.2 Traefik](../10-deployment-and-compose.md#72-traefik--deploytraefiklabelsmd) — the two snippets, verbatim.
6. [`docs/11-config-reference.md` §2](../11-config-reference.md#2-dltool_-variables-application) — the `DLTOOL_ALLOWED_HOSTS` and `DLTOOL_CONFIG_LOCK` rows and the two paragraphs after the table.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/security.go` | create | Host allowlist, security-header, configuration-lock and redirect-validation middleware. |
| `internal/api/security_test.go` | create | Header, `421`, HSTS, redirect and base-path cases. |
| `deploy/caddy/Caddyfile.example` | create | The subdomain and subfolder snippets of doc 10 §7.1. |
| `deploy/traefik/labels.md` | create | The label set of doc 10 §7.2. |
| `internal/api/server.go` | edit | Mount the three middlewares on the base sub-router, outermost first. |
| `internal/api/server_test.go` | edit | Give the shared router-request helper and direct requests an allowed Host. |
| `internal/api/auth_test.go` | edit | Give root-router authentication requests an allowed Host. |
| `internal/obs/health_test.go` | edit | Give main-router health requests an allowed Host. |
| `internal/engine/qbittorrent/contract_test.go` | edit | Give the integration-tagged contract suite's two router-driving requests an allowed Host. |

No other file may be modified.

## Interface contract

```go
package api

import "net/http"

// SecurityHeaders sets the block in docs/12-security-and-threat-model.md §6.6 on every response.
// Strict-Transport-Security is set only when the request arrived over HTTPS, decided from
// X-Forwarded-Proto when the peer is in DLTOOL_TRUSTED_PROXIES and from r.TLS otherwise.
func SecurityHeaders(next http.Handler) http.Handler

// ContentSecurityPolicy is the exact policy string, single-spaced, sent on every HTML response.
const ContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; connect-src 'self'; font-src 'self'; object-src 'none'; " +
	"base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

// ConfigLock answers 403 /problems/config-locked to every mutation under the operator-configuration
// prefixes of 05-api-contract.md section 1.2 when DLTOOL_CONFIG_LOCK is true. Reads, and every task,
// account, authentication, token and backup operation, pass through. Nothing changes the lock itself.
func ConfigLock(locked bool) func(http.Handler) http.Handler

// HostAllowlist answers 421 Misdirected Request when the request Host is neither implicitly
// allowed nor configured. There is no switch that turns it off.
//
// Implicitly allowed, port stripped: "localhost", "localhost.", and any literal IPv4 or IPv6 address.
// Additionally allowed: every name in extra, which is cfg.AllowedHosts — the parsed
// DLTOOL_ALLOWED_HOSTS of 11-config-reference.md section 2, lowercased with one trailing root dot
// removed. Passing nil makes every reverse-proxied hostname answer 421.
func HostAllowlist(extra []string, log *slog.Logger) func(http.Handler) http.Handler

// AllowedHost reports whether host (with or without a port) passes the allowlist. Exported for the
// table test.
func AllowedHost(host string, extra []string) bool

// SafeRedirect returns target when it is a relative path beginning with exactly one "/" and no
// second "/" or "\" in position 1, prefixed with base. Every other input returns base + "/".
//
//	SafeRedirect("/dl-tool", "/tasks")               == "/dl-tool/tasks"
//	SafeRedirect("/dl-tool", "//evil.example")       == "/dl-tool/"
//	SafeRedirect("/dl-tool", "https://evil.example") == "/dl-tool/"
//	SafeRedirect("", "/tasks?state=error")           == "/tasks?state=error"
func SafeRedirect(base, target string) string
```

The headers, exactly, on every HTML response:

```
Content-Security-Policy: <ContentSecurityPolicy>
X-Content-Type-Options: nosniff
Referrer-Policy: same-origin
X-Frame-Options: DENY
Permissions-Policy: geolocation=(), camera=(), microphone=(), interest-cohort=()
Cross-Origin-Opener-Policy: same-origin
Cross-Origin-Resource-Policy: same-origin
```

`deploy/caddy/Caddyfile.example` and `deploy/traefik/labels.md` reproduce doc 10 §7.1 and §7.2 verbatim,
including the `flush_interval -1` comment and the "do NOT add stripprefix" note.

## Steps
1. Create `internal/api/security.go` with `ContentSecurityPolicy`, `SecurityHeaders`, `HostAllowlist`,
   `AllowedHost` and `SafeRedirect`.
2. Send `Strict-Transport-Security` only when the request arrived over HTTPS; an unconditional HSTS header
   bricks plain-HTTP LAN access.
3. Use no `unsafe-inline`, no `unsafe-eval` and no CDN origin in the policy: every asset is embedded in the
   binary, so the UI must work with no internet access.
4. Implement `AllowedHost`: strip the port, accept `localhost`, `localhost.` and any literal IP parsed by
   `net.ParseIP`, then accept any exact match in `extra`. Everything else fails.
5. Implement `HostAllowlist` to answer `421` with a problem document and one `warn` log line carrying the
   offending `Host` value.
6. Implement `SafeRedirect` rejecting `//host`, `/\host`, any absolute URL and any value containing a
   control character, and always returning a path prefixed by the configured base.
7. Edit `internal/api/server.go` to mount `HostAllowlist(cfg.AllowedHosts, log)` first, then
   `SecurityHeaders`, then `ConfigLock(cfg.ConfigLock)` on the `/api/v1` sub-router, then the existing
   middleware chain, all on the base sub-router so a request outside the base still returns `404`.
   `cfg.AllowedHosts` and `cfg.ConfigLock` are the parsed `DLTOOL_ALLOWED_HOSTS` and `DLTOOL_CONFIG_LOCK`
   ([`11-config-reference.md`](../11-config-reference.md#2-dltool_-variables-application) §2) — they are
   the only sources either has. Update router-driving requests in the four listed test files to use
   an allowed Host — including the `//go:build integration` `TestConformBootCorrection` constructions —
   keep the middleware unconditional and preserve every existing assertion. The shared
   `do()` helper also covers `static_test.go`, which needs no edit.
8. Create `deploy/caddy/Caddyfile.example` and `deploy/traefik/labels.md` from doc 10 §7.1 and §7.2, carrying
   forward the UNVERIFIED note on the Traefik flush-interval label name.
9. Create `internal/api/security_test.go` with: each header asserted on an HTML response; HSTS absent over
   plain HTTP and present when `X-Forwarded-Proto: https` arrives from a trusted proxy; `Host: evil.example`
   returning `421`; `Host: localhost:8080` and `Host: 192.168.1.10` succeeding; `Host: dl.example.com`
   succeeding when `cfg.AllowedHosts` is `["dl.example.com"]` and `421` when it is empty; with
   `DLTOOL_CONFIG_LOCK=true` a `PATCH /settings` and a `POST /indexers` returning `403`
   `/problems/config-locked` with no side effect while `POST /tasks/{id}/pause` and a token revocation
   still succeed; `SafeRedirect` table cases
   for `//evil.example`, `https://evil.example`, `/tasks` and `\\evil.example`; and a repository grep
   asserting no `InsecureSkipVerify: true` outside `testdata/`.
10. Run the Playwright suite against dl-tool behind Caddy at `/dl-tool/` and confirm login, the grid and the
    event stream all work; paste the run summary under `## Evidence`.

## Acceptance criteria
- [ ] All seven headers plus the conditional HSTS behave exactly as doc 12 §6.6 specifies.
- [ ] `Host: evil.example` returns `421`; `Host: localhost:8080` and a literal IP succeed.
- [ ] A name in `DLTOOL_ALLOWED_HOSTS` succeeds, with a port and with one trailing root dot, proving
      `cfg.AllowedHosts` reaches `HostAllowlist`.
- [ ] `TestConfigLockRejectsOperatorMutations` shows a settings and an indexer mutation returning `403`
      `/problems/config-locked` without side effects, while task pause and token revocation still work.
- [ ] `//evil.example` and `https://evil.example` are both ignored and land on the application root.
- [ ] `GET /anything` outside the configured base returns `404`, not the SPA.
- [ ] A repository grep finds no `InsecureSkipVerify: true` outside test fixtures.
- [ ] The Caddy subfolder block uses `handle`, not `handle_path`, and sets `flush_interval -1`.
- [ ] The Traefik snippet attaches no `buffering` middleware and no `stripprefix` middleware.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
set -euo pipefail
make lint && go test -race -count=1 ./internal/api/... ./internal/obs/... \
  && go test -race -count=1 -v -run 'TestSecurityHeadersOnHTML|TestHSTSOnlyOverHTTPS|TestUnexpectedHostIs421|TestAllowedHostTable|TestSafeRedirectTable|TestNoInsecureSkipVerify' ./internal/api/... ./internal/obs/... | tee /tmp/t095-named-tests.log \
  && [ "$(grep '^--- PASS: Test' /tmp/t095-named-tests.log | awk '{print $3}' | sort -u | wc -l)" -ge 6 ] \
  && make test-integration
```
Expected: lint succeeds and both packages pass, with
`TestSecurityHeadersOnHTML`, `TestHSTSOnlyOverHTTPS`, `TestUnexpectedHostIs421`,
`TestAllowedHostTable`, `TestSafeRedirectTable` and `TestNoInsecureSkipVerify` all listed as passing —
the distinct-name count fails the run if `-run` matched nothing — and the integration suite passes.
No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: only paths allowed by the Files table, sorted lexically. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT change the base-path mechanism itself; T013 owns `internal/api/static.go` and the `<base href>` rewrite.
- Do NOT edit `compose.yaml` to add the `proxy` profile; T094 owns the compose file.
- Do NOT add a CDN origin, `unsafe-inline` or `unsafe-eval` to the policy to make a component work.
- Do NOT weaken the Host allowlist behind a configuration switch: it is always on.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked

Resolved by plan repairs: the Files table permits the request-construction fixes recorded below —
the three test helpers of the 2026-09-25 record (repair `3c2ea66`) and the integration-tagged
`internal/engine/qbittorrent/contract_test.go` of the 2026-09-26 record (this repair) — and the
Verification block now exercises `make test-integration`. The allowlist and existing assertions
remain unchanged. The task is eligible again; implementation and its Verification output are still
required.

### 2026-09-25 — the always-on host allowlist breaks 18 merged tests outside the Files table

Step 7 mounts `HostAllowlist(cfg.AllowedHosts, log)` outermost on the base sub-router, and the
interface contract makes the check unconditional: "There is no switch that turns it off", and nil
`extra` "makes every reverse-proxied hostname answer 421". Verified against this tree with the
middleware written exactly as the contract specifies — `AllowedHost` accepting `localhost`,
`localhost.` and literal IPs, `extra` exact-matching — and mounted on `base` ahead of
`SecurityHeaders`: every test that drives `server.Router` or `server.Base` through
`httptest.NewRequest` — whose default `Host` is `example.com`, a DNS name that is neither
implicitly allowed nor configured — now fails on the 421 instead of its own assertion:

```text
--- FAIL: TestSessionCookiesThroughRoot
    auth_test.go:763: /auth/setup: status 421:
    {"type":"/problems/validation-failed","title":"Misdirected Request","status":421,
     "detail":"the request host \"example.com\" is not an allowed name"}
```

The full fallout of `go test ./internal/api/... ./internal/obs/...` with the middleware mounted is:

- `internal/api/auth_test.go`: `TestSessionCookiesThroughRoot`, `TestBaseRoutesStayAnonymous` —
  real-store builds that drive `server.Router`.
- `internal/api/server_test.go`: `TestOpenAPIMatchesCommittedDocument`, `TestCDNBackedDocsAreDisabled`,
  `TestBasePathMountsEverything`, `TestUnknownRouteIsProblemJSON`, `TestHumaErrorsCarryRegistrySlug`,
  `TestValidationErrorsDoNotEchoCredentials`, `TestRealIPHonoursTrustedProxies`,
  `TestPanicIsLoggedAndConforming`, `TestRecovererRepanicsAbortHandler` — nil-db router tests
  through the `do()` helper and two direct `httptest.NewRequest` call sites.
- `internal/api/static_test.go`: `TestBaseHrefInjected`, `TestSPAFallbackInsideBase`,
  `TestAPIRouteNotShadowed`, `TestSPAMethodNotAllowed`, `TestReservedNamespacesStay404` — all reach
  the SPA through `do()`, so they need no edit once the helper carries an allowed `Host`.
- `internal/obs/health_test.go`: `TestHealthEndpointsRequireNoCredential`,
  `TestMetricsNotOnMainListener` — the `doMainRouterRequest` helper drives the base router's
  `/healthz`, `/readyz` and `/metrics` probes, which the allowlist also covers by design.

The fixes are test-only — give each router-driving request an allowed `Host` (e.g. `localhost`) —
but none of the four files appears in this task's `## Files` table and hard rule 1 forbids touching
them. Neither alternative satisfies the contract: gating the middleware on `db != nil` (the
auth-middleware precedent) weakens "no switch turns it off" for a build the plan otherwise treats as
production-shaped, and still fails the two real-db auth tests; letting `example.com` through invents
an implicit name doc 12 §6.5 does not allow.

**Remedy:** a contract repair of the class of `1c71587` ("Repair the T091 Files table"): extend this
file's `## Files` table with `internal/api/server_test.go`, `internal/api/auth_test.go` and
`internal/obs/health_test.go` — the three files whose request construction must carry an allowed
`Host` — and keep the middleware unconditional. `internal/api/static_test.go` needs no table row:
it only calls `do()` (verified as of the 2026-09-25 block record — no direct
`httptest.NewRequest`/`http.Request` construction in the file). The file that should answer the question "may tests set Host to satisfy an
always-on allowlist" is this task file's Files table.

### 2026-09-26 — the same allowlist also breaks the integration-tagged contract call site, again outside the Files table

The 2026-09-25 repair scoped its verification to `go test ./internal/api/... ./internal/obs/...` —
the Verification block still runs exactly that — and the fallout list it produced does not cover
`//go:build integration` code in other packages — and `make test-integration` does cover it, because
`.github/workflows/ci.yml` runs it in the `integration` job (green on main as of this record),
and under that tag
[`internal/engine/qbittorrent/contract_test.go`](../../internal/engine/qbittorrent/contract_test.go)
`TestConformBootCorrection` drives `server.Router.ServeHTTP` twice with `httptest.NewRequest`'s
default `Host: example.com` — once for `POST /api/v1/auth/setup` asserting `201 Created`
(line ~1491), once in the `call` helper asserting `200 OK` (line ~1506).

Verified against this tree with `HostAllowlist` written exactly as the contract specifies and
mounted on `base` ahead of `SecurityHeaders` — the construction `TestConformBootCorrection` uses,
replayed through `server.Router`:

```text
POST /api/v1/auth/setup: status 421: {"type":"/problems/validation-failed","title":"Misdirected Request","status":421,"detail":"the request host \"example.com\" is not an allowed name"}
GET /api/v1/engines: status 421: {"type":"/problems/validation-failed","title":"Misdirected Request","status":421,"detail":"the request host \"example.com\" is not an allowed name"}
```

The request path is the base sub-router's, so the always-on allowlist answers before any handler —
the `require.Equal(t, http.StatusCreated, setup.Code)` assertion can only see 421. The fix is again
test-only: give both request constructions an allowed `Host` (`localhost`). But
`internal/engine/qbittorrent/contract_test.go` is not in this task's `## Files` table, hard rule 1
forbids touching it, and no Files-table file can carry the fix — the test constructs its own
requests. The remaining fallouts were re-swept and are clean: every other `NewServer` caller either
never serves a request (rss, jobs, engine unit tests, search, secure), drives `server.API` through
humatest — which enters at the `v1` mux, below the base sub-router the allowlist guards — or listens
on a real socket, where the client sends a `127.0.0.1` host a literal IP accepts
(`cmd/dl-tool/main_test.go`, the e2e harness).

**Remedy:** a second contract repair of the class of `3c2ea66` ("Repair T095 host-test scope"):
extend this file's `## Files` table with `internal/engine/qbittorrent/contract_test.go` — the one
remaining file whose request construction must carry an allowed `Host` — and keep the middleware
unconditional, and extend this file's `## Verification` block with `make test-integration`, so the
next attempt exercises the suite that has now broken twice locally rather than discovering it in
CI. Verified as of this record: `grep -rn "go:build integration" --include="*.go" .`
lists five files, and only this one calls `server.Router.ServeHTTP`; the untagged sweep
`grep -rn "server\.Router" --include="*_test.go" .` adds only `cmd/dl-tool/main_test.go`, whose
requests ride real listeners and send a `127.0.0.1` host the literal-IP rule accepts.

#### Companion defect found while landing this record — the Verification block itself stalls `task-verification`

This deferral's own PR could not go through the `task-verification` workflow: every run of the
extracted script — `make lint && go test -race -count=1 -v ./internal/api/... ./internal/obs/...` —
stalled at "Run task Verification" and wedged the runner so completely that cancellation and the
job's own 60-minute `timeout-minutes` produced no effect for tens of minutes and no log blob was
ever uploaded. Observed across four consecutive runs (three `push` triggers, one
`workflow_dispatch`), each stuck ≥50 minutes on the step before being reaped. The same commands
pass individually on the same SHA — `make lint` in the `lint` job (6m16s), the full
`go test -race -count=1 ./...` in the `test` job (10m12s) — and the exact extracted script
completes locally under `bash -euo pipefail` in ~5.5 minutes. Every prior `task-verification`
run on other task branches succeeded; all of their scripts use `make test` — plain, non-verbose
`go test` — and none emits anywhere near the ~60 MB of step output that `-v` produces on
`internal/api` alone. Verbose output on the repo's largest test suite is the clearest variable
that distinguishes this script from everything that has ever passed in that workflow — though the
chained `make lint &&` prefix and the explicit package list differ as well — so it is the leading
suspect for the stall mechanism (runner resource or log-pipeline exhaustion); the runner died too
early to leave logs proving it.

**Resolution applied in this deferral:** the `## Verification` block now runs the package pass
without `-v`, then a `-v -run` pass naming the six required tests, so the gate keeps both halves
of the intent while shedding the output volume. The next task-verification run of the repaired
script doubles as the test of the `-v` hypothesis: green confirms it; another stall clears it
and points at the remaining variables.

**Confirmed 2026-09-26:** the `task-verification` job ran the repaired script on the deferral PR's
head (`453613d`) and passed in 14m36s — the `-v` output-volume hypothesis holds. The follow-up
repair additionally guards the `-v -run` pass against a vacuous green (a distinct-name count
of the `--- PASS` lines) and appends `make test-integration`, closing the coverage gap both
fallout records trace to.
