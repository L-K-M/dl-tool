# T054 — Fetch and parse Torznab caps and search responses

| Field | Value |
|---|---|
| **ID** | T054 |
| **Milestone** | M4 |
| **Status** | todo |
| **Depends on** | T005, T123 |
| **Blocks** | T055, T058, T062, T066, T077 |
| **Parallel-safe** | yes — creates `internal/search/` only |
| **Implements** | [FR-050](../02-requirements.md#fr-050-query-torznab-and-newznab-indexers), [FR-059](../02-requirements.md#fr-059-report-unknown-result-fields-as-null) |
| **Decisions** | [ADR-0008](../decisions/0008-torznab-first-declarative-yaml-second.md) |
| **Est. size** | 3 new Go files plus a fixture directory, ~380 LOC |

## Goal
`internal/search/torznab.go` fetches `t=caps` and `t=search` from a Torznab or Newznab base URL through the
client T123 built, and parses both documents into `Caps` and `[]SearchResult`. Unknown numeric fields stay
nil, and a row with no download URL, magnet or infohash is dropped and counted.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/07-search-and-indexers.md` §2.1 Endpoint shape](../07-search-and-indexers.md#21-endpoint-shape) and
   [§2.2 `t=caps` document](../07-search-and-indexers.md#22-tcaps-document).
2. [`docs/07-search-and-indexers.md` §2.4 Search response and `torznab:attr`](../07-search-and-indexers.md#24-search-response-and-torznabattr)
   — the parser rule table, and [§2.5 Errors](../07-search-and-indexers.md#25-errors) including the Prowlarr deviations.
3. [`docs/07-search-and-indexers.md` §2.6 Provider URL shapes](../07-search-and-indexers.md#26-provider-url-shapes)
   — the base URL forms `NewTorznabClient` must accept.
4. [`docs/07-search-and-indexers.md` §5 The normalised `SearchResult`](../07-search-and-indexers.md#5-the-normalised-searchresult)
   — the struct and the five mandatory rules.
5. [`docs/12-security-and-threat-model.md` §2.4 Caps and diagnosability](../12-security-and-threat-model.md#24-caps-and-diagnosability)
   — the 8 MiB body cap this client passes to `secure.ReadCapped`.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/search/normalize.go` | create | `SearchResult` and the normalisation rules of doc 07 §5. |
| `internal/search/torznab.go` | create | Request building, `t=caps` and feed parsing, the Torznab error document. |
| `internal/search/torznab_test.go` | create | Fixture parsing and the null rules. |
| `internal/search/testdata/` | create | `torznab_hdaccess.xml`, `torznab_tpb.xml`, `torznab_caps.xml`, `academic_torrents_rss.xml`, `torznab_error.xml` and a `README.md` recording the source and date of each. |

No other file may be modified.

## Interface contract

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

// TorznabClient holds the guarded client, never one of its own: hc always comes from
// secure.NewClient (T123), and every response body is read with secure.ReadCapped.
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
1. Create `internal/search/normalize.go` with `SearchResult`, `Finalise` and `MagnetFromInfohash`.
   `DownloadVolumeFactor` and `UploadVolumeFactor` default to `1.0` on every constructed row.
2. Create `internal/search/torznab.go`. `NewTorznabClient` stores the `*http.Client` it is given and never
   builds one: no `http.Client{}` literal, no `http.Get` and no `http.Post` may appear in this package.
3. Build the query string from `Query` with `url.Values`, always sending `t` and `apikey`, joining
   `Categories` with commas, and clamping `Limit` to the caps `<limits max>`.
4. Read every response with `secure.ReadCapped(resp, secure.MetadataFetchCap)` so an 8 MiB body is the hard
   ceiling for a caps or search document.
5. Implement `ParseCaps` with `encoding/xml`: flatten `category`/`subcat`, fall back to the newznab default
   `supportedParams` when the attribute is absent, treat `available="no"` as an unsupported mode, and default
   `LimitsMax` to 100.
6. Implement `ParseFeed` with `encoding/xml`, applying every row of the doc 07 §2.4 parser-rule table: attr
   size wins over `<size>`, repeated `category` collects into `CategoryIDs`, `peers`/`leechers`, the
   `magneturl` → `enclosure` → `link` download preference, `<comments>` → absolute `<guid>` for details, and
   `pubDate` with the Go layout `Mon, 02 Jan 2006 15:04:05 -0700` rendered as RFC 3339.
7. Detect `<error code=… description=…/>` before feed parsing and return `*TorznabError`, honouring
   `Retry-After` on 429 and carrying the real HTTP status for the Prowlarr 400/410/429 deviations.
8. Record the five fixtures under `internal/search/testdata/` with their source and date in the sibling
   `README.md` created inside that directory row.
9. Create `internal/search/torznab_test.go`: `TestParseTorznabItem` over `torznab_hdaccess.xml` asserting
   size, seeders, leechers, infohash and published date; `TestParseMagnetEnclosure` over `torznab_tpb.xml`;
   `TestParseCapsTree` over `torznab_caps.xml`; `TestSeedersNullWhenAbsent` over
   `academic_torrents_rss.xml`; `TestTorznabErrorDocument` over `torznab_error.xml`; and
   `TestFinaliseDropsUnusableRow`.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestParseTorznabItem` asserts `size_bytes=2538463390`, `seeders=7`, `leechers=0`, the 40-hex infohash and an RFC 3339 published date.
- [ ] `TestSeedersNullWhenAbsent` asserts `Seeders` and `Leechers` are nil pointers, not `-1` and not `1`.
- [ ] `TestParseCapsTree` asserts a flattened `category`/`subcat` tree and `LimitsMax == 100` when `<limits>` is absent.
- [ ] `Finalise` drops a row with no download URL, no magnet and no infohash and reports it in `dropped`.
- [ ] `TestTorznabErrorDocument` returns a `*TorznabError` with `Code == 100` for an HTTP 200 body and keeps `HTTPStatus` for a Prowlarr 429.
- [ ] `grep -rn "http.Get\|http.Post\|http.Client{" internal/search` returns nothing: the client arrives from `secure.NewClient`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/search/... && echo TORZNAB_OK
```
Expected: `ok  	github.com/L-K-M/dl-tool/internal/search`, the six tests named in step 9 listed as passing,
no `FAIL` and no `SKIP` line, and the final line of stdout exactly `TORZNAB_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT create `internal/secure/ssrf.go`, a `Guard`, a dialer or an `http.Client`; T123 owns all of it and
  this task only calls `secure.NewClient`, `secure.ReadCapped` and `secure.MetadataFetchCap`.
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
