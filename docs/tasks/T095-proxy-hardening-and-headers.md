# T095 — Harden the proxied deployment and ship the proxy snippets

| Field | Value |
|---|---|
| **ID** | T095 |
| **Milestone** | M7 |
| **Status** | todo |
| **Depends on** | T007, T013, T094 |
| **Blocks** | — |
| **Parallel-safe** | no — it edits `internal/api/server.go` |
| **Implements** | [NFR-006](../02-requirements.md#nfr-006-work-when-hosted-under-a-sub-path), [NFR-010](../02-requirements.md#nfr-010-always-verify-tls-certificates), [NFR-013](../02-requirements.md#nfr-013-reject-unexpected-host-headers), [NFR-021](../02-requirements.md#nfr-021-serve-strict-security-headers), [NFR-024](../02-requirements.md#nfr-024-validate-login-redirects-as-relative-paths) |
| **Decisions** | [ADR-0013](../decisions/0013-mandatory-built-in-authentication.md), [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md) |
| **Est. size** | 4 new files, ~380 LOC |

## Goal
Every HTML response carries the eight documented security headers, an unexpected `Host` is answered `421`,
a login redirect is honoured only when it is a single-slash relative path, and the shipped Caddy and Traefik
snippets serve dl-tool at both a subdomain and a subfolder without buffering the event stream.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/12-security-and-threat-model.md` §6.6 Response headers](../12-security-and-threat-model.md#66-response-headers) — the header block, verbatim, and the HSTS rule.
2. [`docs/12-security-and-threat-model.md` §6.5 Host-header allowlist against DNS rebinding](../12-security-and-threat-model.md#65-host-header-allowlist-against-dns-rebinding) — the four allowlist rules.
3. [`docs/12-security-and-threat-model.md` §6.7 Open redirects, configuration lock, exposure](../12-security-and-threat-model.md#67-open-redirects-configuration-lock-exposure) — the redirect rule and `config_lock`.
4. [`docs/10-deployment-and-compose.md` §7.3 Base-path requirements](../10-deployment-and-compose.md#73-base-path-requirements) — the eight hard requirements.
5. [`docs/10-deployment-and-compose.md` §7.1 Caddy](../10-deployment-and-compose.md#71-caddy--deploycaddycaddyfileexample) and [§7.2 Traefik](../10-deployment-and-compose.md#72-traefik--deploytraefiklabelsmd) — the two snippets, verbatim.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/security.go` | create | Host allowlist, security-header and redirect-validation middleware. |
| `internal/api/security_test.go` | create | Header, `421`, HSTS, redirect and base-path cases. |
| `deploy/caddy/Caddyfile.example` | create | The subdomain and subfolder snippets of doc 10 §7.1. |
| `deploy/traefik/labels.md` | create | The label set of doc 10 §7.2. |
| `internal/api/server.go` | edit | Mount the three middlewares on the base sub-router, outermost first. |

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
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// HostAllowlist answers 421 Misdirected Request when the request Host is neither implicitly
// allowed nor configured. There is no switch that turns it off.
//
// Implicitly allowed, port stripped: "localhost", "localhost.", and any literal IPv4 or IPv6 address.
// Additionally allowed: every name in extra.
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
7. Edit `internal/api/server.go` to mount `HostAllowlist` first, then `SecurityHeaders`, then the existing
   middleware chain, all on the base sub-router so a request outside the base still returns `404`.
8. Create `deploy/caddy/Caddyfile.example` and `deploy/traefik/labels.md` from doc 10 §7.1 and §7.2, carrying
   forward the UNVERIFIED note on the Traefik flush-interval label name.
9. Create `internal/api/security_test.go` with: each header asserted on an HTML response; HSTS absent over
   plain HTTP and present when `X-Forwarded-Proto: https` arrives from a trusted proxy; `Host: evil.example`
   returning `421`; `Host: localhost:8080` and `Host: 192.168.1.10` succeeding; `SafeRedirect` table cases
   for `//evil.example`, `https://evil.example`, `/tasks` and `\\evil.example`; and a repository grep
   asserting no `InsecureSkipVerify: true` outside `testdata/`.
10. Run the Playwright suite against dl-tool behind Caddy at `/dl-tool/` and confirm login, the grid and the
    event stream all work; paste the run summary under `## Evidence`.

## Acceptance criteria
- [ ] All seven headers plus the conditional HSTS behave exactly as doc 12 §6.6 specifies.
- [ ] `Host: evil.example` returns `421`; `Host: localhost:8080` and a literal IP succeed.
- [ ] `//evil.example` and `https://evil.example` are both ignored and land on the application root.
- [ ] `GET /anything` outside the configured base returns `404`, not the SPA.
- [ ] A repository grep finds no `InsecureSkipVerify: true` outside test fixtures.
- [ ] The Caddy subfolder block uses `handle`, not `handle_path`, and sets `flush_interval -1`.
- [ ] The Traefik snippet attaches no `buffering` middleware and no `stripprefix` middleware.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/api/...
```
Expected: `make lint` prints nothing, then `ok  	github.com/L-K-M/dl-tool/internal/api` followed by its
elapsed time, with `TestSecurityHeadersOnHTML`, `TestHSTSOnlyOverHTTPS`, `TestUnexpectedHostIs421`,
`TestAllowedHostTable`, `TestSafeRedirectTable` and `TestNoInsecureSkipVerify` all listed as passing.
No `FAIL`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT change the base-path mechanism itself; T013 owns `internal/api/static.go` and the `<base href>` rewrite.
- Do NOT implement the `config_lock` switch; it is a settings concern and no M7 task owns it.
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
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
