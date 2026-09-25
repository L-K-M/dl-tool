# T088 — Cache the yt-dlp extractor patterns for the router

| Field | Value |
|---|---|
| **ID** | T088 |
| **Milestone** | M7 |
| **Status** | todo |
| **Depends on** | T016, T087 |
| **Blocks** | — (T090 no longer depends on it; see `## Blocked`) |
| **Parallel-safe** | no — it edits `internal/engine/router.go` |
| **Implements** | [FR-002](../02-requirements.md#fr-002-route-each-uri-to-an-engine-by-scheme) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md), [ADR-0010](../decisions/0010-never-execute-third-party-definitions.md), [ADR-0022](../decisions/0022-generate-ytdlp-routing-table.md) |
| **Est. size** | 5 new files + 1 script, ~350 LOC |

## Blocked
Resolved 2026-09-25 (remedy: decision record + contract rewrite). The premise below was measured false —
enumeration has no flag and the patterns are Python `re` — so
[ADR-0022](../decisions/0022-generate-ytdlp-routing-table.md) replaced the mechanism with a committed,
generated pattern table plus a hand-maintained host-override map for the extractors that cannot transpile.
This task was restated to that contract and returned to `todo`.

> **Historical record.** Its premise was that yt-dlp could be asked to print its extractor URL patterns
> and that those patterns compile with Go's `regexp`. Both were measured against the pinned yt-dlp
> 2026.08.19 and both are false — see [`docs/06-download-engines.md` §7.2](../06-download-engines.md#72-routing-check)
> for the numbers. Row 3 of the routing table still needed an offline, cheap answer, and choosing the
> replacement changed how the media lane is routed, so it needed an ADR, not a patch.

## Goal
Row 3 of the routing table answers from a table **generated once, committed, and compiled at start-up**:
`Match(uri)` is a hostname check against a small override map plus one regexp alternation over the
generated patterns, and it never makes a network call, shells out, or runs a metadata extraction. The
`generic` extractor is never in the table, so an arbitrary HTTPS URL still falls through to aria2.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/decisions/0022-generate-ytdlp-routing-table.md`](../decisions/0022-generate-ytdlp-routing-table.md) — the mechanism this task implements, with the measured corpus numbers.
2. [`docs/06-download-engines.md` §7.2 Routing check](../06-download-engines.md#72-routing-check) — the rule and the failure the old mechanism hit.
3. [`docs/06-download-engines.md` §2 Routing table](../06-download-engines.md#2-routing-table) — where row 3 sits and what happens above and below it.
4. [`docs/06-download-engines.md` §1 The `Engine` interface](../06-download-engines.md#1-the-engine-interface) — `Accepts`, `ErrUnavailable`.
5. [`docs/12-security-and-threat-model.md` §5.2 Selectors and regexes](../12-security-and-threat-model.md#52-selectors-and-regexes) — regex compilation limits.
6. [`Dockerfile`](../../Dockerfile) — `YTDLP_VERSION` is the pin the generator must verify against.

## Files
| Path | Action | Purpose |
|---|---|---|
| `scripts/gen-ytdlp-patterns.py` | create | The maintainer-run generator per ADR-0022: reads the pinned wheel's `_VALID_URL`s, transpiles to RE2, verifies each pattern compiles, writes the two generated files below deterministically. |
| `internal/engine/ytdlp/extractor_patterns.txt` | create | GENERATED — one RE2 pattern per line, `generic` excluded; `//go:embed`'d. Never hand-edit. |
| `internal/engine/ytdlp/extractors_residual.txt` | create | GENERATED — extractor names whose patterns could not be made RE2-safe; drives `ResidualOverrides` coverage. Never hand-edit. |
| `internal/engine/ytdlp/overrides.go` | create | Hand-maintained `ResidualOverrides` map: residual extractor name → host suffixes. |
| `internal/engine/ytdlp/patterns.go` | create | `ExtractorCache`, `LoadExtractors`, `Match`, `Loaded`, `Len`. |
| `internal/engine/ytdlp/patterns_test.go` | create | Table compiles, residual↔override coverage, YouTube/Vimeo routing, non-match, `generic` exclusion. |
| `internal/engine/router.go` | edit | Point row 3's `mediaMatch` hook at the cache. |

No other file may be modified.

## Interface contract

```go
package ytdlp

// extractor_patterns.txt is written by scripts/gen-ytdlp-patterns.py — one RE2
// pattern per line, generic excluded. Never hand-edit.
//
//go:embed extractor_patterns.txt
var patternTable string

// ResidualOverrides maps each extractor name in extractors_residual.txt to the host
// suffixes that route its URIs (hostnames, lowercase, no port). It is hand-maintained:
// the generator tells you which names need entries on each regen, and the coverage
// test fails if any residual name lacks one. Hostname granularity is deliberate —
// row 3 answers "should yt-dlp see this URL"; yt-dlp picks its own extractor later.
var ResidualOverrides = map[string][]string{
	"Youtube": {"youtube.com", "youtu.be"},
	// … one entry per name in extractors_residual.txt …
}

// ExtractorCache holds the generated routing table compiled once at start-up.
// The zero value matches nothing.
type ExtractorCache struct {
	pattern  *regexp.Regexp // one alternation over the generated table
	hosts    []string       // flattened ResidualOverrides suffixes, lowercase
	patterns int            // table line count
	loaded   bool
}

// LoadExtractors compiles the embedded table into one alternation and flattens the
// override hosts. An empty table returns a usable empty cache with loaded == false;
// a line that fails regexp.Compile fails the load — the table is committed output and
// a bad line means the generator or a hand edit is broken, which should be loud.
func LoadExtractors() (*ExtractorCache, error)

// Match reports whether uri routes to yt-dlp: the URI's lowercase hostname is
// checked against the override suffixes (a suffix d matches h == d or
// strings.HasSuffix(h, "."+d)), then against the compiled alternation.
// It never performs I/O.
func (c *ExtractorCache) Match(uri string) bool

// Loaded reports whether the table compiled; false means the media lane is disabled.
func (c *ExtractorCache) Loaded() bool

// Len returns the number of table patterns plus override hosts.
func (c *ExtractorCache) Len() int
```

```go
package engine

// mediaMatch is row 3 of the routing table. It is nil until an adapter installs one, and a nil
// hook makes row 3 fall through to rows 4-6 rather than rejecting the URI.
type MediaMatcher func(uri string) bool

// SetMediaMatcher installs the yt-dlp extractor cache as row 3 of the routing table.
func (r *Router) SetMediaMatcher(m MediaMatcher)
```

## Steps
1. Create `scripts/gen-ytdlp-patterns.py` per ADR-0022. It reads `YTDLP_VERSION` from the
   `Dockerfile`, downloads that version's `py3-none-any` wheel from PyPI with stdlib `urllib` +
   `zipfile` (no pip, no third-party Python), imports `yt_dlp` from the unpacked wheel, and **refuses
   to run** when the package's `yt_dlp.version.__version__` does not match the pin. Optionally it
   accepts `--yt-dlp <path>` pointing at an already-unpacked package or wheel. With the stdlib only
   it:
   - enumerates every extractor class and collects `_VALID_URL` / `_VALID_URLS`, skipping `Generic`;
   - transpiles each pattern: when it starts with `(?x` (all such occurrences in the corpus are
     pattern-initial) strip the flag group, then drop unescaped whitespace and `#`-to-EOL comments
     outside character classes; leave `(?i)` and `(?P<name>…)` alone — Go's `regexp` accepts both;
   - writes every surviving pattern to a temp file and pipes it through a `go run` probe that
     `regexp.Compile`s each line; patterns that fail are dropped and their owning extractor name is
     recorded. `go` must be on the maintainer's `PATH`;
   - writes `internal/engine/ytdlp/extractor_patterns.txt` — a `#` comment header naming the wheel
     version (no date: output must be byte-identical across reruns on the same pin), then the patterns,
     sorted and deduplicated — and `internal/engine/ytdlp/extractors_residual.txt` with the same
     header and the sorted residual extractor names;
   - prints the residual list so the maintainer knows which names `ResidualOverrides` must cover.
2. Run it against the pinned wheel and commit both generated files unmodified.
3. Create `internal/engine/ytdlp/overrides.go`: one `ResidualOverrides` entry — lowercase host
   suffixes, no port, no leading dot — for **every** name in `extractors_residual.txt`. Youtube,
   Vimeo, Instagram, Soundcloud and Dailymotion are the load-bearing ones; a missing entry is a
   routing hole the coverage test catches.
4. Create `internal/engine/ytdlp/patterns.go` per the contract: `//go:embed` the table, skip `#`
   comment and blank lines, compile the rest as one `^(?:(?:p1)|(?:p2)|…)` alternation inside
   `LoadExtractors` (measured ≈163 ms on this corpus — do it once, never per `Match`), flatten
   `ResidualOverrides` into a lowercase host list.
   `Match(uri)`: parse with `net/url`, lowercase the hostname (no match on parse failure or empty
   host), suffix-check the override hosts, then `pattern.MatchString`. No I/O anywhere.
5. Edit `internal/engine/router.go`: add the `MediaMatcher` type and `SetMediaMatcher`, and have row 3
   consult it. A nil matcher falls through to rows 4-6.
6. Create `internal/engine/ytdlp/patterns_test.go` covering:
   - every non-comment line of `extractor_patterns.txt` compiles with `regexp.Compile` (the test is
     the permanent re-verification of generated output, so it does not silently rot);
   - every name in `extractors_residual.txt` has at least one `ResidualOverrides` entry;
   - `Match` returns true via the override hosts for
     `https://www.youtube.com/watch?v=dQw4w9WgXcQ`, `https://youtu.be/dQw4w9WgXcQ` and
     `https://vimeo.com/123456789` (all three extractors are in the residual set — measured), and via
     the generated table for `https://x.com/nasa/status/1234567890` (Twitter's pattern transpiles);
   - `Match` returns false for `https://releases.ubuntu.com/24.04/ubuntu-24.04.iso` — this is also the
     `generic` check: only Generic's pattern would claim such a URL, so a non-match proves it was
     never generated into the table;
   - the zero-value cache reports `Loaded() == false` and `Match() == false`.
7. Add a router test asserting that with the matcher installed the media URL routes to `ytdlp`, and
   with a nil matcher the same URL routes to `aria2`.

## Acceptance criteria
- [ ] `Match` performs no network or subprocess call.
- [ ] `generic` patterns are never generated into the table.
- [ ] `youtube.com/watch`, `youtu.be` and `vimeo.com` URLs route to `ytdlp` via the override hosts, and
      `x.com/…/status/…` via the generated table.
- [ ] `https://releases.ubuntu.com/24.04/ubuntu-24.04.iso` routes to `aria2`, with and without the matcher.
- [ ] Every line in `extractor_patterns.txt` compiles, and every `extractors_residual.txt` name has an
      override entry — both enforced by tests, so a stale regen or a dropped override fails CI.
- [ ] `extractor_patterns.txt` and `extractors_residual.txt` carry the wheel-version header and are
      byte-identical across reruns against the same pin.
- [ ] The generator refuses to run against a `yt_dlp` package whose version ≠ `YTDLP_VERSION`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/engine/...
```
Expected: `make lint` prints nothing, then `ok` lines for
`github.com/L-K-M/dl-tool/internal/engine` and `github.com/L-K-M/dl-tool/internal/engine/ytdlp`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT change rows 1, 2 or 4-9 of the routing table; T016 owns them.
- Do NOT call yt-dlp — or any subprocess — to answer `Match`; the committed table is the only
  permitted source.
- Do NOT hand-edit `extractor_patterns.txt` or `extractors_residual.txt`; change the generator or
  `ResidualOverrides` instead.
- Do NOT wire the generator into `make gen`, CI or the image build; it is maintainer-run only, on a
  machine that has both Python and `go`. Scheduling it belongs to T097.
- Do NOT implement the `engine.Engine` methods or register the adapter; T090 owns that.
- Do NOT add the capability probe or the media-lane disable switch; T113 owns them.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
