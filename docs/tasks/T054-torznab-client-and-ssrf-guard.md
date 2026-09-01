# T054 — Fetch and parse Torznab responses through an SSRF-guarded client

| Field | Value |
|---|---|
| **ID** | T054 |
| **Milestone** | M4 |
| **Status** | todo |
| **Depends on** | T005 |
| **Blocks** | T055, T058, T062, T066, T077 |
| **Parallel-safe** | yes — creates `internal/secure/ssrf.go` and `internal/search/` |
| **Implements** | [FR-050](../02-requirements.md#fr-050-query-torznab-and-newznab-indexers), [FR-059](../02-requirements.md#fr-059-report-unknown-result-fields-as-null), [NFR-017](../02-requirements.md#nfr-017-block-server-side-request-forgery) |
| **Decisions** | [ADR-0008](../decisions/0008-torznab-first-declarative-yaml-second.md) |
| **Est. size** | 5 new files, ~620 LOC. Above the usual budget because FR-050, FR-059 and NFR-017 are all pinned to T054 by `02-requirements.md`, and the Torznab client cannot make a request without the guard. |

## Goal
`internal/secure/ssrf.go` is the one outbound HTTP client in the repository, validating the resolved peer IP
inside the dialer on every hop. `internal/search/torznab.go` uses it to fetch `t=caps` and `t=search` from a
Torznab or Newznab base URL and to parse both documents into `Caps` and `[]SearchResult`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/12-security-and-threat-model.md` §2.1 The block list](../12-security-and-threat-model.md#21-the-block-list)
   — the 18 IPv4 prefixes and the IPv6 allow-list, verbatim.
2. [`docs/12-security-and-threat-model.md` §2.2 How to implement it](../12-security-and-threat-model.md#22-how-to-implement-it)
   — the `ControlContext` recipe and the nine numbered rules.
3. [`docs/12-security-and-threat-model.md` §2.4 Caps and diagnosability](../12-security-and-threat-model.md#24-caps-and-diagnosability)
   — timeouts, hop count, the 8 MiB body cap and the one `warn` record per block.
4. [`docs/07-search-and-indexers.md` §2.1 Endpoint shape](../07-search-and-indexers.md#21-endpoint-shape) and
   [§2.2 `t=caps` document](../07-search-and-indexers.md#22-tcaps-document).
5. [`docs/07-search-and-indexers.md` §2.4 Search response and `torznab:attr`](../07-search-and-indexers.md#24-search-response-and-torznabattr)
   — the parser rule table, and [§2.5 Errors](../07-search-and-indexers.md#25-errors) including the Prowlarr deviations.
6. [`docs/07-search-and-indexers.md` §5 The normalised `SearchResult`](../07-search-and-indexers.md#5-the-normalised-searchresult)
   — the struct and the five mandatory rules.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/secure/ssrf.go` | create | `Guard`, the prefix tables, `Check`, `CheckRedirect`, `NewClient`, `ReadCapped`. |
| `internal/search/normalize.go` | create | `SearchResult` and the normalisation rules of doc 07 §5. |
| `internal/search/torznab.go` | create | Request building, `t=caps` and feed parsing, the Torznab error document. |
| `internal/search/torznab_test.go` | create | Fixture parsing, the null rules and the guard's own cases. |
| `internal/search/testdata/` | create | `torznab_hdaccess.xml`, `torznab_tpb.xml`, `torznab_caps.xml`, `academic_torrents_rss.xml`, `torznab_error.xml` and a `README.md` recording the source and date of each. |

No other file may be modified.

## Interface contract

```go
package secure

// ErrSSRFBlocked is returned when a resolved peer address, port, scheme or hop count is
// not permitted. internal/api maps it to /problems/ssrf-blocked.
var ErrSSRFBlocked = errors.New("secure: ssrf blocked")

// ErrBodyTooLarge is returned by ReadCapped for an over-cap Content-Length or body.
var ErrBodyTooLarge = errors.New("secure: response body over cap")

// Guard validates the resolved peer IP inside the dialer, never the hostname in the URL.
type Guard struct{ /* denied4, denied6, allowed6, allowedPorts, allowPrivate, log */ }

// NewGuard builds the guard from 12-security-and-threat-model.md section 2.1. allowPrivate
// lifts 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 127.0.0.0/8, fc00::/7 and ::1/128 only;
// 169.254.0.0/16 and fe80::/10 stay denied under every switch.
func NewGuard(log *slog.Logger, allowPrivate bool) *Guard

// Check is wired into net.Dialer.ControlContext, never Control. network is "tcp4" or
// "tcp6"; addr is always "ip:port".
func (g *Guard) Check(ctx context.Context, network, addr string) error

// CheckRedirect caps hops at 5 and re-checks the scheme on every hop.
func (g *Guard) CheckRedirect(req *http.Request, via []*http.Request) error

// NewClient returns the one outbound client: dial timeout 10s, total timeout 120s,
// ForceAttemptHTTP2, CheckRedirect wired to the guard.
func NewClient(g *Guard) *http.Client

// ReadCapped rejects an over-cap Content-Length up front, then reads through an
// io.LimitReader of limit+1 bytes. Indexer and feed fetches pass 8<<20.
func ReadCapped(resp *http.Response, limit int64) ([]byte, error)
```

```go
package search

// SearchResult is 07-search-and-indexers.md section 5 verbatim. Unknown numbers are nil,
// never -1 and never a fabricated 1.
type SearchResult struct {
	ID          string  `json:"id"`
	EngineID    string  `json:"engine_id"`
	Title       string  `json:"title"`
	DownloadURL string  `json:"download_url,omitempty"`
	MagnetURI   string  `json:"magnet_uri,omitempty"`
	Infohash    string  `json:"infohash,omitempty"`
	SizeBytes   int64   `json:"size_bytes"`
	Seeders     *int    `json:"seeders"`
	Leechers    *int    `json:"leechers"`
	Grabs       *int    `json:"grabs"`
	PublishedAt *string `json:"published_at"` // RFC 3339
	DetailsURL  string  `json:"details_url,omitempty"`
	CategoryIDs []int   `json:"category_ids"`
	CategoryDesc string `json:"category_desc,omitempty"`
	DownloadVolumeFactor float64  `json:"download_volume_factor"` // default 1.0
	UploadVolumeFactor   float64  `json:"upload_volume_factor"`   // default 1.0
	MinimumRatio         *float64 `json:"minimum_ratio"`
	MinimumSeedTimeSecs  *int     `json:"minimum_seed_time_seconds"`
	IMDBID string `json:"imdb_id,omitempty"`
	TMDBID string `json:"tmdb_id,omitempty"`
	TVDBID string `json:"tvdb_id,omitempty"`
	Year   *int   `json:"year"`
	Genre, Language, Publisher, Author, Album, Artist string // json: snake_case, omitempty
}

// Finalise applies rules 1, 2 and 5 of doc 07 section 5: leechers = peers - seeders,
// nil for unknown, and drop any row with no download URL, magnet or infohash. dropped is
// the count of rows removed, which the caller adds to that engine's error tally.
func Finalise(in []SearchResult) (out []SearchResult, dropped int)

// MagnetFromInfohash builds magnet:?xt=urn:btih:<infohash>&dn=<title> for an
// infohash-only result.
func MagnetFromInfohash(infohash, title string) string
```

```go
package search

type Category struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Subcategories []Category `json:"subcategories,omitempty"`
}

type Caps struct {
	ServerTitle   string
	LimitsMax     int                 // <limits max>, 100 when absent
	LimitsDefault int
	Modes         map[string][]string // "search"|"tv-search"|"movie-search"|"audio-search"|"book-search"
	Categories    []Category
}

// Query is one Torznab request. T is caps, search, tvsearch, movie, music or book.
type Query struct {
	T          string
	Q          string
	Categories []int
	Limit      int
	Offset     int
	Season     string
	Ep         string
	IMDBID     string
	NoCache    bool // adds &cache=false, for Jackett
}

type TorznabClient struct{ /* hc, base, apiKey, userAgent, engineID */ }

func NewTorznabClient(hc *http.Client, base string, apiKey secure.Secret, engineID, userAgent string) (*TorznabClient, error)

func (c *TorznabClient) Caps(ctx context.Context) (Caps, error)
func (c *TorznabClient) Search(ctx context.Context, q Query) ([]SearchResult, error)

// ParseCaps and ParseFeed are pure; they never touch the network.
func ParseCaps(doc []byte) (Caps, error)
func ParseFeed(doc []byte, engineID string) ([]SearchResult, error)

// TorznabError is <error code="200" description="Missing parameter (t)"/>. Conforming
// servers return it with HTTP 200; Prowlarr returns it with 400, 410 or 429.
type TorznabError struct {
	Code        int
	Description string
	RetryAfter  time.Duration // from the Retry-After header on 429, zero otherwise
	HTTPStatus  int
}

func (e *TorznabError) Error() string
```

## Steps
1. Create `internal/secure/ssrf.go` with the 18 IPv4 prefixes and the IPv6 allow-list of doc 12 §2.1 as
   package-level `[]netip.Prefix` values parsed once in an `init`-free `var` block.
2. Implement `Check` exactly as doc 12 §2.2 shows it: reject any network other than `tcp4`/`tcp6`, reject any
   port other than 80 and 443, call `Is4In6()` then `Unmap()` before the IPv4 rules, and log one `warn`
   record carrying `url_redacted`, `resolved_ip`, `matched_prefix` and `hop`.
3. Implement `CheckRedirect` (5 hops, scheme must stay `http` or `https`), `NewClient` wiring
   `net.Dialer.ControlContext` — never `Control` — and `ReadCapped`.
4. Create `internal/search/normalize.go` with `SearchResult`, `Finalise` and `MagnetFromInfohash`.
   `DownloadVolumeFactor` and `UploadVolumeFactor` default to `1.0` on every constructed row.
5. Create `internal/search/torznab.go`: build the query string from `Query` with `url.Values`, always sending
   `t` and `apikey`, joining `Categories` with commas, and clamping `Limit` to the caps `<limits max>`.
6. Implement `ParseCaps` with `encoding/xml`: flatten `category`/`subcat`, fall back to the newznab default
   `supportedParams` when the attribute is absent, treat `available="no"` as an unsupported mode, and default
   `LimitsMax` to 100.
7. Implement `ParseFeed` with `encoding/xml`, applying every row of the doc 07 §2.4 parser-rule table: attr
   size wins over `<size>`, repeated `category` collects into `CategoryIDs`, `peers`/`leechers`, the
   `magneturl` → `enclosure` → `link` download preference, `<comments>` → absolute `<guid>` for details, and
   `pubDate` with the Go layout `Mon, 02 Jan 2006 15:04:05 -0700` rendered as RFC 3339.
8. Detect `<error code=… description=…/>` before feed parsing and return `*TorznabError`, honouring
   `Retry-After` on 429 and carrying the real HTTP status for the Prowlarr 400/410/429 deviations.
9. Record the five fixtures under `internal/search/testdata/` with their source and date in the sibling
   `README.md` created inside that directory row.
10. Create `internal/search/torznab_test.go`: `TestParseTorznabItem` over `torznab_hdaccess.xml` asserting
    size, seeders, leechers, infohash and published date; `TestParseMagnetEnclosure` over `torznab_tpb.xml`;
    `TestParseCapsTree`; `TestSeedersNullWhenAbsent` over `academic_torrents_rss.xml`;
    `TestTorznabErrorDocument`; and the guard cases `TestGuardBlocksLoopback`,
    `TestGuardBlocksLinkLocalMapped` (`::ffff:169.254.169.254`) and `TestGuardBlocksRedirectToLoopback`
    against an `httptest` server, all asserting `errors.Is(err, secure.ErrSSRFBlocked)`.
11. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestParseTorznabItem` asserts `size_bytes=2538463390`, `seeders=7`, `leechers=0`, the 40-hex infohash and an RFC 3339 published date.
- [ ] `TestSeedersNullWhenAbsent` asserts `Seeders` and `Leechers` are nil pointers, not `-1` and not `1`.
- [ ] `Finalise` drops a row with no download URL, no magnet and no infohash and reports it in `dropped`.
- [ ] `TestGuardBlocksLoopback`, `TestGuardBlocksLinkLocalMapped` and `TestGuardBlocksRedirectToLoopback` all fail with `secure.ErrSSRFBlocked`.
- [ ] `ReadCapped` rejects a declared `Content-Length` of 9 MiB before reading a byte.
- [ ] `grep -rn "http.Get\|http.Post" internal/search` returns nothing.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/search/... ./internal/secure/..." && echo TORZNAB_OK
```
Expected: `ok  	github.com/L-K-M/dl-tool/internal/search`, `ok  	github.com/L-K-M/dl-tool/internal/secure`, every
test named in step 10 listed as passing, and the final line of stdout exactly `TORZNAB_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add the provider enumeration for Jackett or Prowlarr; T055 owns it.
- Do NOT add an `indexers` row, a handler or a route; T055 owns indexer storage and CRUD.
- Do NOT implement `details`, `getnfo`, `get`, `comments`, `register` or `user`; they are newznab-only.
- Do NOT dedupe results across engines; T062 owns `Dedup`.
- Do NOT store id `0` from a Prowlarr instance; it is a synthetic self-test indexer.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
