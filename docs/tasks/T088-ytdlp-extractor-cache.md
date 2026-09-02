# T088 — Cache the yt-dlp extractor patterns for the router

| Field | Value |
|---|---|
| **ID** | T088 |
| **Milestone** | M7 |
| **Status** | deferred — see `## Blocked` |
| **Depends on** | T016, T087 |
| **Blocks** | — (T090 no longer depends on it; see `## Blocked`) |
| **Parallel-safe** | no — it edits `internal/engine/router.go` |
| **Implements** | [FR-002](../02-requirements.md#fr-002-route-each-uri-to-an-engine-by-scheme) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md), [ADR-0010](../decisions/0010-never-execute-third-party-definitions.md) |
| **Est. size** | 3 new files, ~250 LOC |

## Blocked
**This task is not implementable as written; do not start it.** Its premise is that yt-dlp can be asked to
print its extractor URL patterns and that those patterns compile with Go's `regexp`. Both were measured
against the pinned yt-dlp 2026.08.19 and both are false — see
[`docs/06-download-engines.md` §7.2](../06-download-engines.md#72-routing-check) for the numbers.
Step 1 ("identify the flag that enumerates extractors") has no answer, and step 4's drop-on-compile-failure
rule would discard YouTube.

Row 3 of the routing table still needs an offline, cheap answer. Choosing the replacement changes how the
media lane is routed, so it needs an ADR under [`docs/decisions/`](../decisions/), not an edit here. Until
that record exists this task stays `deferred` and `MediaMatcher` stays nil, which
[T016](T016-engine-interface-and-router.md) already defines as "fall through to rows 4-6": a yt-dlp URL is
then routed to aria2 rather than mis-routed, and nothing else in the plan breaks.

## Goal
Row 3 of the routing table answers from a cache built once at start-up: `Accepts(uri)` is a regexp match
against extractor URL patterns and never makes a network call or runs a metadata extraction. The `generic`
extractor is excluded, so an arbitrary HTTPS URL still routes to aria2.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §7.2 Routing check](../06-download-engines.md#72-routing-check) — the rule and its UNVERIFIED marker.
2. [`docs/06-download-engines.md` §2 Routing table](../06-download-engines.md#2-routing-table) — where row 3 sits and what happens above and below it.
3. [`docs/06-download-engines.md` §1 The `Engine` interface](../06-download-engines.md#1-the-engine-interface) — `Accepts`, `ErrUnavailable`.
4. [`docs/12-security-and-threat-model.md` §5.2 Selectors and regexes](../12-security-and-threat-model.md#52-selectors-and-regexes) — regex compilation limits.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/ytdlp/extractors.go` | create | `ExtractorCache`, `LoadExtractors`, `Match`. |
| `internal/engine/ytdlp/extractors_test.go` | create | Match, non-match, `generic` exclusion, degraded-mode cases. |
| `internal/engine/ytdlp/testdata/extractors.txt` | create | A recorded enumeration output used by the tests. |
| `internal/engine/router.go` | edit | Point row 3's `mediaMatch` hook at the cache. |

No other file may be modified.

## Interface contract

```go
package ytdlp

import (
	"context"
	"regexp"
)

// ExcludedExtractors never contribute patterns: "generic" matches every URL, which would
// steal rows 4-6 of the routing table in docs/06-download-engines.md §2.
var ExcludedExtractors = []string{"generic"}

// ExtractorCache holds compiled URL patterns enumerated once from the pinned binary.
// The zero value matches nothing, which is the degraded mode when enumeration failed.
type ExtractorCache struct {
	patterns []*regexp.Regexp
	names    []string // parallel to patterns, for logging only
	loaded   bool
}

// LoadExtractors shells out once to cfg.BinaryPath, parses the enumeration output and compiles
// every pattern except those in ExcludedExtractors. A failure returns a usable empty cache
// together with the error, so a missing binary disables the media lane instead of failing boot.
func LoadExtractors(ctx context.Context, cfg Config) (*ExtractorCache, error)

// ParseExtractors turns one enumeration output into named patterns. Exported for the golden test.
func ParseExtractors(raw []byte) (names []string, patterns []string, err error)

// Match reports whether any cached pattern matches uri. It never performs I/O.
func (c *ExtractorCache) Match(uri string) bool

// Loaded reports whether enumeration succeeded; false means the media lane is disabled.
func (c *ExtractorCache) Loaded() bool

// Len returns the number of compiled patterns.
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

<!-- UNVERIFIED: doc 06 §7.2 records that the exact enumeration flag was not verbatim-confirmed. Run
     `<DLTOOL_YTDLP_PATH> --help` against the build on the developer machine, choose the flag that lists
     extractors, and paste both the flag and its first ten output lines under "## Evidence". -->

## Steps
1. Run `<DLTOOL_YTDLP_PATH> --help` and identify the flag that enumerates extractors. Record the exact flag
   as a package constant `extractorListFlag` with a comment naming the yt-dlp version it was read from.
2. Capture that command's output into `internal/engine/ytdlp/testdata/extractors.txt` and record the
   command, the yt-dlp version and the capture date in the file's first commented line, per
   [`docs/13-testing-and-verification.md` §5](../13-testing-and-verification.md#5-golden-file-fixtures).
3. Create `internal/engine/ytdlp/extractors.go` with `ParseExtractors`: one extractor per line, name and
   pattern separated by the recorded delimiter; skip blank lines and every name in `ExcludedExtractors`.
4. Compile each pattern with `regexp.Compile`, cap the input at 4096 bytes per pattern, and drop — with one
   `warn` log line naming the extractor — any pattern that fails to compile. One bad pattern never fails the
   load.
5. Implement `LoadExtractors`: `exec.CommandContext` with a 30 s deadline, `ParseExtractors` on the output,
   and on any error return `&ExtractorCache{}` plus the error wrapped with `engine.ErrUnavailable`.
6. Implement `Match`, `Loaded` and `Len`. `Match` returns `false` immediately when `loaded` is false.
7. Edit `internal/engine/router.go`: add the `MediaMatcher` type and `SetMediaMatcher`, and have row 3
   consult it. A nil matcher falls through to rows 4-6.
8. Create `internal/engine/ytdlp/extractors_test.go` covering: a URL a cached pattern matches; a plain
   `https://releases.ubuntu.com/24.04/ubuntu-24.04.iso` that must NOT match; an `extractors.txt` containing
   a `generic` line whose pattern is excluded even though it would match everything; an unparsable pattern
   that is skipped without failing the load; and the zero-value cache reporting `Loaded() == false` and
   `Match() == false`.
9. Add a router test asserting that with the matcher installed the media URL routes to `ytdlp`, and with a
   nil matcher the same URL routes to `aria2`.

## Acceptance criteria
- [ ] `Match` performs no network or subprocess call.
- [ ] The `generic` extractor's pattern is never compiled into the cache.
- [ ] A failed enumeration returns a usable cache with `Loaded() == false` and does not fail the process.
- [ ] A pattern that fails to compile is skipped with one `warn` line and the remaining patterns still load.
- [ ] `https://releases.ubuntu.com/24.04/ubuntu-24.04.iso` routes to `aria2`, both with and without the matcher.
- [ ] The exact enumeration flag and its first ten output lines are pasted under `## Evidence`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/engine/...
```
Expected: `make lint` prints nothing, then `ok` lines for
`github.com/L-K-M/dl-tool/internal/engine` and `github.com/L-K-M/dl-tool/internal/engine/ytdlp`, with
`TestParseExtractorsSkipsGeneric`, `TestMatchIgnoresPlainHTTP`, `TestLoadFailureDegradesToEmpty` and
`TestRouterRow3UsesMediaMatcher` all listed as passing. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT change rows 1, 2 or 4-9 of the routing table; T016 owns them.
- Do NOT call yt-dlp to answer `Accepts`; the cache is the only permitted source.
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
