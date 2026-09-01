# T123 — Build the SSRF-guarded outbound HTTP client

| Field | Value |
|---|---|
| **ID** | T123 |
| **Milestone** | M4 |
| **Status** | todo |
| **Depends on** | T005 |
| **Blocks** | T054, T122 |
| **Parallel-safe** | yes — creates `internal/secure/ssrf.go` and its test, nothing else |
| **Implements** | [NFR-017](../02-requirements.md#nfr-017-block-server-side-request-forgery) |
| **Decisions** | [ADR-0001](../decisions/0001-control-plane-over-existing-engines.md), [ADR-0008](../decisions/0008-torznab-first-declarative-yaml-second.md) |
| **Est. size** | 2 new files, ~380 LOC |

## Goal
`internal/secure/ssrf.go` is the only place in the repository that builds an outbound `*http.Client`. It
validates the resolved peer IP inside the dialer on every connection, re-validates every redirect hop, caps
hops, schemes, ports, body size and time, and lifts the private ranges only when
`DLTOOL_SSRF_ALLOW_PRIVATE` is set.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/12-security-and-threat-model.md` §2.1 The block list](../12-security-and-threat-model.md#21-the-block-list)
   — the 18 IPv4 prefixes and the IPv6 allow-list, verbatim; copy them, do not re-derive them.
2. [`docs/12-security-and-threat-model.md` §2.2 How to implement it](../12-security-and-threat-model.md#22-how-to-implement-it)
   — the `ControlContext` recipe and the nine numbered rules.
3. [`docs/12-security-and-threat-model.md` §2.3 Private addresses and the engine sidecars](../12-security-and-threat-model.md#23-private-addresses-and-the-engine-sidecars)
   — what `allowPrivate` lifts, and the two prefixes it never lifts.
4. [`docs/12-security-and-threat-model.md` §2.4 Caps and diagnosability](../12-security-and-threat-model.md#24-caps-and-diagnosability)
   — timeouts, hop count, the 8 MiB body cap and the one `warn` record per block.
5. [`docs/11-config-reference.md` §2 `DLTOOL_` variables](../11-config-reference.md#2-dltool_-variables-application)
   — `DLTOOL_SSRF_ALLOW_PRIVATE`, already parsed into `config.Config.SSRFAllowPrivate` by T005.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/secure/ssrf.go` | create | The prefix tables, `Guard`, `AllowAddr`, `Check`, `CheckRedirect`, `NewClient`, `ReadCapped`, `RedactURL`. |
| `internal/secure/ssrf_test.go` | create | The address, port, scheme, redirect, cap and redaction cases of the acceptance list. |

No other file may be modified.

## Interface contract

```go
package secure

// ErrSSRFBlocked is returned when a resolved peer address, port, scheme or hop count is not
// permitted. internal/api maps it to /problems/ssrf-blocked with status 403.
var ErrSSRFBlocked = errors.New("secure: ssrf blocked")

// ErrBodyTooLarge is returned by ReadCapped for an over-cap Content-Length or body.
var ErrBodyTooLarge = errors.New("secure: response body over cap")

// MetadataFetchCap is the body cap for a .torrent, feed or indexer fetch: 8 MiB.
const MetadataFetchCap int64 = 8 << 20

// BlockedError names the rule that fired so a support request ends in one round trip.
// Reason is one of "network", "port", "scheme", "address", "resolve" or "redirect_cap".
type BlockedError struct {
	Reason string
	IP     netip.Addr // zero when Reason is not "address"
	Prefix string     // the matched prefix, "" when Reason is not "address"
	Hop    int        // 0 for the original request
	URL    string     // already passed through RedactURL
}

func (e *BlockedError) Error() string { return "secure: ssrf blocked: " + e.Reason }
func (e *BlockedError) Unwrap() error { return ErrSSRFBlocked }

// Guard holds the prefix tables and the allow-private switch. It is safe for concurrent use.
type Guard struct{ /* denied4, denied6, allowed6, allowPrivate, log */ }

// NewGuard builds the guard from 12-security-and-threat-model.md §2.1. allowPrivate is
// config.Config.SSRFAllowPrivate for the global guard, or an indexer's own allow_private flag.
// It lifts 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 127.0.0.0/8, fc00::/7 and ::1/128 only;
// 169.254.0.0/16 and fe80::/10 stay denied under every switch.
func NewGuard(log *slog.Logger, allowPrivate bool) *Guard

// AllowAddr applies the §2.1 tables to one already-resolved address. It calls Is4In6 and Unmap
// before the IPv4 rules, and logs one warn record carrying url_redacted, resolved_ip,
// matched_prefix and hop on every denial.
func (g *Guard) AllowAddr(ip netip.Addr) error

// Check is wired into net.Dialer.ControlContext, never Control. network is "tcp4" or "tcp6";
// addr is always "ip:port", never a hostname. Ports 80 and 443 are the only ones permitted.
func (g *Guard) Check(ctx context.Context, network, addr string) error

// CheckRedirect caps hops at 5 and requires the scheme to stay http or https on every hop.
func (g *Guard) CheckRedirect(req *http.Request, via []*http.Request) error

// NewClient returns the one outbound client: dial timeout 10s, total timeout 120s,
// ForceAttemptHTTP2, ControlContext wired to Check and CheckRedirect wired to the guard.
// No other file in the repository may construct an http.Client or call http.Get or http.Post.
func NewClient(g *Guard) *http.Client

// ReadCapped rejects an over-cap declared Content-Length before reading a byte, then reads
// through an io.LimitReader of limit+1 bytes so a lying Content-Length cannot bypass the cap.
func ReadCapped(resp *http.Response, limit int64) ([]byte, error)

// RedactURL returns scheme://host/path with userinfo and the whole query string removed, for
// log records and problem details. An unparseable input returns "[unparseable url]".
func RedactURL(raw string) string
```

## Steps
1. Create `internal/secure/ssrf.go`. Declare the 18 IPv4 prefixes of doc 12 §2.1 as a package-level
   `var denied4 = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/8"), …}` in the order the document lists
   them, and the IPv6 allow-list as `allowed6` (`2000::/3`) plus `denied6` (the five prefixes inside it).
2. Declare `privateLift`, the six prefixes `allowPrivate` removes from the denied set, and never include
   `169.254.0.0/16` or `fe80::/10` in it.
3. Write `NewGuard`: store the tables and the logger, and when `allowPrivate` is true build `denied4` and
   `denied6` without the `privateLift` members rather than special-casing them at match time.
4. Write `AllowAddr`: call `ip.Is4In6()` then `ip.Unmap()`, apply the IPv4 tables to a 4-byte address, and
   for an IPv6 address require membership of `allowed6` and non-membership of `denied6`. Return a
   `*BlockedError` naming the matched prefix, and log exactly one `warn` record per denial.
5. Write `Check` exactly as doc 12 §2.2 shows it: reject any network other than `tcp4`/`tcp6`, reject any
   port other than `80` and `443`, parse the host with `netip.ParseAddr`, then delegate to `AllowAddr`.
6. Write `CheckRedirect`: `len(via) >= 5` returns a `*BlockedError` with reason `redirect_cap`; a
   `req.URL.Scheme` other than `http` or `https` returns reason `scheme` — a `301` to
   `file:///etc/passwd` must not pass. Set `Hop` to `len(via)`.
7. Write `NewClient` with `&net.Dialer{Timeout: 10 * time.Second, ControlContext: g.Check}` and
   `&http.Transport{DialContext: d.DialContext, ForceAttemptHTTP2: true}`, `Timeout: 120 * time.Second`.
   Never set `Dialer.Control`: it is ignored whenever `ControlContext` is set.
8. Write `ReadCapped` and `RedactURL`. `RedactURL` clears `u.User` and `u.RawQuery` before `u.String()`,
   so a tracker passkey never reaches a log line or a problem detail.
9. Create `internal/secure/ssrf_test.go` with the pure cases, all asserting
   `errors.Is(err, secure.ErrSSRFBlocked)`: `TestCheckBlocksLoopback` (`127.0.0.1:80`),
   `TestCheckBlocksLinkLocalMapped` (`::ffff:169.254.169.254` on `tcp6`),
   `TestCheckBlocksNonStandardPort` (`93.184.216.34:8080`), `TestCheckAllowsPublicAddress`,
   `TestAllowPrivateLiftsRFC1918`, `TestAllowPrivateKeepsLinkLocalDenied`,
   `TestCheckRedirectCapsAtFiveHops` and `TestCheckRedirectRejectsFileScheme`.
10. Add `TestClientBlocksRedirectToMetadata`: an `httptest` server reached through a guard built with
    `allowPrivate` true redirects to `http://169.254.169.254/latest/meta-data/`; the request fails with
    `ErrSSRFBlocked` and no packet leaves, because `ControlContext` runs before `connect`.
11. Add `TestReadCappedRejectsDeclaredLength` (a 9 MiB `Content-Length` fails before a byte is read),
    `TestReadCappedRejectsLyingLength` (a `Content-Length` of 10 with a 9 MiB body still fails) and
    `TestRedactURLDropsUserinfoAndQuery`.
12. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `Check` denies `127.0.0.1:80`, `[::ffff:169.254.169.254]:443` and `93.184.216.34:8080`, and allows `93.184.216.34:443`.
- [ ] With `allowPrivate` true, `10.1.2.3` is allowed and `169.254.169.254` is still denied.
- [ ] `CheckRedirect` denies a sixth hop and denies a `file:` target on hop one.
- [ ] `TestClientBlocksRedirectToMetadata` fails with `errors.Is(err, secure.ErrSSRFBlocked)`.
- [ ] `ReadCapped` rejects a declared 9 MiB `Content-Length` and a body that exceeds `limit` despite a small declared length.
- [ ] `RedactURL("https://u:p@x.example/a?apikey=k")` returns `https://x.example/a`.
- [ ] `grep -rn "Control:" internal/secure/ssrf.go` returns nothing: only `ControlContext` is set.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/secure/... && echo SSRF_GUARD_OK
```
Expected: `ok  	github.com/L-K-M/dl-tool/internal/secure`, every test named in steps 9 to 11 listed as
passing, no `FAIL` and no `SKIP` line, and the final line of stdout exactly `SSRF_GUARD_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the two paths in the Files table and nothing else. Use `git status`, not `git diff`: both
files are untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT touch the Torznab client, `internal/search/` or any fixture under it; T054 owns all of it.
- Do NOT call this guard from any handler, poller or notifier; T122 wires the task endpoints and T054,
  T066 and T077 wire their own call sites.
- Do NOT add the per-indexer `allow_private` column or flag; T055 owns the `indexers` row.
- Do NOT add a repository-wide `depguard` rule or a `.golangci.yml`; this task ships no lint configuration.
- Do NOT special-case `DLTOOL_ARIA2_URL` or `DLTOOL_QBITTORRENT_URL`: sidecar RPC never goes through this
  client, so the guard needs no exception for it (doc 12 §2.3).

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
