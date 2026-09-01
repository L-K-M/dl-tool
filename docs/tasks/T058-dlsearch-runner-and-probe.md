# T058 — Execute `rss` and `json` engines and probe an indexer

| Field | Value |
|---|---|
| **ID** | T058 |
| **Milestone** | M4 |
| **Status** | todo |
| **Depends on** | T054, T055, T056, T057 |
| **Blocks** | T061, T062, T105 |
| **Parallel-safe** | no — extends T055's `internal/api/search.go` |
| **Implements** | [FR-056](../02-requirements.md#fr-056-test-an-indexer-on-demand), [FR-051](../02-requirements.md#fr-051-load-declarative-dlsearchv1-engines) |
| **Decisions** | [ADR-0008](../decisions/0008-torznab-first-declarative-yaml-second.md), [ADR-0010](../decisions/0010-never-execute-third-party-definitions.md) |
| **Est. size** | 2 new files, ~470 LOC |

## Goal
`search.Runner` turns a validated `Definition` of kind `rss` or `json` plus a query into `[]SearchResult`,
expanding only the closed placeholder set and applying only the twelve transform ops, under every hard limit
of doc 07 §3.5. `POST /indexers/{id}/test` performs exactly one probe — `t=caps` for a Torznab indexer, one
definition request for a `dlsearch` engine — and reports the outcome as data.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/07-search-and-indexers.md` §3.3 Template placeholders](../07-search-and-indexers.md#33-template-placeholders--a-fixed-closed-set)
   — the token table, the two control constructs and the six functions.
2. [`docs/07-search-and-indexers.md` §3.4 Transform ops](../07-search-and-indexers.md#34-transform-ops--a-fixed-closed-set)
   — the twelve ops and their arguments.
3. [`docs/07-search-and-indexers.md` §3.5 Hard limits](../07-search-and-indexers.md#35-hard-limits) — every
   row is enforced here.
4. [`docs/07-search-and-indexers.md` §3.6 Worked example — `kind: rss`](../07-search-and-indexers.md#36-worked-example--kind-rss)
   and [§3.7 `kind: json`](../07-search-and-indexers.md#37-worked-example--kind-json) — the two shapes the
   runner must reproduce, including the browse-style client-side keyword filter.
5. [`docs/05-api-contract.md` §9.1 Indexer CRUD](../05-api-contract.md#91-indexer-crud) — the test response,
   and why a reachable-but-broken indexer is `200` with `ok:false`.
6. [`docs/12-security-and-threat-model.md` §5.2 Selectors and regexes](../12-security-and-threat-model.md#52-selectors-and-regexes).

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/search/dlsearch.go` | create | `Runner`, `Scope`, `Expand`, `ApplyTransforms`, the row extractors and `Probe`. |
| `internal/search/dlsearch_test.go` | create | Expansion, transforms, both fetchers and the limit cases. |
| `internal/api/search.go` | modify | Add the `POST /indexers/{id}/test` handler. |
| `internal/search/testdata/` | modify | Add `archlinux_releases.xml` and `archive_advancedsearch.json`, recorded per doc 13 §5. |

No other file may be modified.

## Interface contract

```go
package search

// Scope is the only data a template can see. There is no reflection over anything else.
type Scope struct {
	Keywords   string
	Page       int
	Limit      int
	Offset     int
	Categories []string          // site-side values mapped from the requested newznab ids
	Query      map[string]string // Q, Season, Ep, Year, Genre, IMDBID, IMDBIDShort, TMDBID,
	                             // TVDBID, TVMazeID, Artist, Album, Label, Track, Author,
	                             // Title, Publisher
	Config     map[string]string // declared settings[].name values; a checkbox is "true" or ""
	Result     map[string]string // fields already resolved for the current row
	TodayYear  string
}

// Expand walks the template with an explicit switch over the closed token set. It never
// calls text/template. Output is capped at 64 KiB, loops at 1000 iterations and nesting
// at depth 8; exceeding any of them returns an error naming the limit.
func Expand(tmpl string, s Scope) (string, error)

// ApplyTransforms runs the ordered op list. Every op is dispatched by an explicit
// switch; an unknown op is an error, never a no-op.
func ApplyTransforms(v string, ops []TransformOp, s Scope) (string, error)

// Coerce converts a raw string to the declared field type. bytes accepts "1.4 GiB",
// "1400000000" and a plain integer; datetime accepts iso8601, rfc1123, unix, relative
// and strptime:<fmt>, and returns RFC 3339.
func Coerce(raw, typ, format string) (string, error)

// Runner executes a definition against the network through the single guarded client.
// The limiter is a hand-rolled token bucket per engine id; no new dependency.
type Runner struct{ /* hc, log, userAgent, mu, limiter map[string]*bucket */ }

func NewRunner(hc *http.Client, log *slog.Logger, userAgent string) *Runner

// Search fetches and maps one page. cfg holds the engine's stored settings values.
// Limits applied per call: 8 MiB body, 2 MiB parsed document, 5 redirects, ports 80 and
// 443, http and https only, the definition's rate limit, and a 15 s total deadline
// covering redirects and parsing.
func (r *Runner) Search(ctx context.Context, def *Definition, cfg map[string]string, q Query) ([]SearchResult, error)

// Probe issues exactly one request and reports what came back. A reachable indexer that
// answers with an error document yields Ok=false and a populated Error, never a Go error.
func (r *Runner) Probe(ctx context.Context, def *Definition, cfg map[string]string) (ProbeResult, error)

type ProbeResult struct {
	Ok              bool
	ElapsedMS       int64
	CategoriesFound int
	Server          string
	Error           string
}

// ErrRateLimited is returned when the engine's own rate limit would be exceeded.
var ErrRateLimited = errors.New("search: engine rate limit exceeded")
```

```go
package api

// TestIndexerOutput is 200 whether or not the probe succeeded, mirroring
// 05-api-contract.md section 9.1.
type TestIndexerOutput struct {
	Body struct {
		Ok              bool    `json:"ok"`
		ElapsedMS       int64   `json:"elapsed_ms"`
		CategoriesFound int     `json:"categories_found"`
		Server          string  `json:"server"`
		Error           *string `json:"error"`
	}
}

func (h *SearchHandlers) TestIndexer(ctx context.Context, in *IndexerIDInput) (*TestIndexerOutput, error)
```

Operation id: `test-indexer`, `POST /indexers/{id}/test`, admin only, added inside T055's
`RegisterSearchRoutes`. `internal/api/server.go` is not touched again.

## Steps
1. Create `internal/search/dlsearch.go` with `Scope`, `Expand`, `ApplyTransforms`, `Coerce`, `Runner`,
   `NewRunner`, `Search` and `Probe`.
2. Implement `Expand` as a hand-written scanner: read `{{`, trim, dispatch on the first word through a
   `switch` over `Keywords`, `Page`, `Limit`, `Offset`, `Categories`, `Query.<F>`, `Config.<name>`,
   `Result.<field>`, `Today.Year`, `if`, `else`, `end`, `range`, `join`, `eq`, `ne`, `and`, `or`, `not`, and
   return an error for anything else. An `if` without an `else` is an error.
3. Implement all twelve transform ops in one `switch`: `trim`, `lower`, `upper`, `html_decode`,
   `url_decode`, `prepend`, `append`, `replace`, `regex_capture` (capture group 1 only), `split` (negative
   index counts from the end), `query_param` and `strip_html`.
4. Build the request: `base_url` joined with `path`, `request.query` values expanded, `request.headers`
   expanded, a `User-Agent` naming dl-tool and its version, and `GET` only.
5. Fetch through the injected `*http.Client` from `secure.NewClient`, read with `secure.ReadCapped(resp,
   8<<20)`, and refuse a parsed document over 2 MiB. Honour `Retry-After` on 429 and 503, and enforce the
   per-engine rate limit before the request, returning `ErrRateLimited` rather than sleeping past the
   deadline.
6. Extract `rss` rows by walking a generic `encoding/xml` element tree with the `response.rows` element path;
   read a field by child element name, or by `attr` on that element. Validate the document is a feed with
   `github.com/mmcdole/gofeed` first; the raw tree is required because `attr` access and extension elements
   such as `<infohash>` are outside gofeed's model.
7. Extract `json` rows with an in-repo evaluator supporting exactly `$`, `.field`, `[n]` and `[*]`. Add no
   JSONPath dependency.
8. Resolve fields in declaration order so `{{ .Result.<field> }}` can see earlier ones, drop `_`-prefixed
   temporaries from the output, apply transforms, then `Coerce`, then `Finalise`. Set `Seeders` and
   `Leechers` to nil whenever `caps.seeders_unknown` is true.
9. When no expanded `request.query` value referenced `{{ .Keywords }}`, filter rows in process by
   case-insensitive substring of `Title` — the browse-style behaviour of doc 07 §3.6.
10. Implement `Probe`: `t=caps` through `TorznabClient.Caps` for a Torznab or Newznab indexer, one
    `request` fetch for `rss`, `json` and `html`, and definition validation only for `static`; count
    categories, fill `Server` from the caps `<server title>` where present.
11. Add `TestIndexer` to `internal/api/search.go`, calling `RecordTest` with the outcome, returning `200`
    with `ok:false` for an upstream error and `503` `/problems/engine-unavailable` only when the probe could
    not be attempted; add `test-indexer` inside `RegisterSearchRoutes`.
12. Create `internal/search/dlsearch_test.go` with `TestExpandClosedSet`, `TestExpandRejectsUnknownToken`,
    `TestExpandOutputCap`, `TestTransformOpsTable`, `TestRSSRowExtraction` against the arch-linux fixture,
    `TestJSONRowExtraction` against an internet-archive fixture, `TestBrowseEngineFiltersByKeyword`,
    `TestBodyCapEnforcedWhileStreaming` and `TestProbeReportsUpstreamErrorAsData`.
13. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestExpandRejectsUnknownToken` asserts `{{ .Env.HOME }}` and `{{ printf "x" }}` both fail with the token named.
- [ ] `TestTransformOpsTable` covers all twelve ops, and an unknown op returns an error rather than the input unchanged.
- [ ] `TestBrowseEngineFiltersByKeyword` asserts an engine with no `{{ .Keywords }}` in its query returns only rows whose title contains the query, case-insensitively.
- [ ] `TestBodyCapEnforcedWhileStreaming` asserts a response declaring 1 MiB but sending 9 MiB is refused.
- [ ] `POST /indexers/{id}/test` against a stub returning `HTTP 503` responds `200` with `ok:false` and the upstream status in `error`.
- [ ] No `text/template`, `html/template`, `os/exec` or JSONPath dependency appears in `internal/search`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/search/... ./internal/api/..." && echo RUNNER_OK
```
Expected: `ok  	github.com/L-K-M/dl-tool/internal/search`, `ok  	github.com/L-K-M/dl-tool/internal/api`, every
test named in step 12 listed as passing, and the final line of stdout exactly `RUNNER_OK`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT implement `kind: static` execution; T105 owns `entries[]` filtering.
- Do NOT implement `kind: html` row extraction beyond the probe; no bundled engine may use it, and the
  goquery path is only reachable for a user-supplied definition.
- Do NOT start a search job or write `search_results` rows; T061 and T062 own the job model.
- Do NOT add a second request path, `request.paths[]`, or a directory-index walker; both are v2.
- Do NOT implement `POST /indexers/{id}/test` as "results were non-empty"; a Prowlarr self-test indexer
  makes that check meaningless.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
