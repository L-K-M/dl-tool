# T088 — Cache the yt-dlp extractor patterns for the router

| Field | Value |
|---|---|
| **ID** | T088 |
| **Milestone** | M7 |
| **Status** | todo |
| **Depends on** | T016, T087 |
| **Blocks** | — (T090 no longer depends on it; see `## Blocked`) |
| **Parallel-safe** | no — it edits `internal/api/tasks.go`, `server.go` and `internal/engine/ytdlp/engine.go` |
| **Implements** | [FR-002](../02-requirements.md#fr-002-route-each-uri-to-an-engine-by-scheme) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md), [ADR-0010](../decisions/0010-never-execute-third-party-definitions.md), [ADR-0022](../decisions/0022-generate-ytdlp-routing-table.md) |
| **Est. size** | 5 new files + 1 script + 7 edits, ~450 LOC |

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
1. [`docs/decisions/0022-generate-ytdlp-routing-table.md`](../decisions/0022-generate-ytdlp-routing-table.md) — the mechanism this task implements, with the measured corpus numbers. It also bounds ADR-0010 for this task: "never execute third-party definitions" scopes to the product at runtime; the generator's maintainer-time import of the hash-verified wheel is the sanctioned exception, per the ADR's Consequences.
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
| `internal/engine/ytdlp/engine.go` | edit | `Engine` gains a `cache *ExtractorCache` field; `Connect` loads it (already called at boot by main.go's probe — no main.go edit needed); the `Accepts` stub delegates to `cache.Match`. |
| `internal/api/tasks.go` | edit | `NewTaskHandlers` gains the `mediaMatch func(string) bool` param (stored on the struct); its two `engine.Route(n, nil)` call sites pass `h.mediaMatch` — T091-#277 class: the hook exists but the composition root cannot reach it from the task's own package. |
| `internal/api/tasks_inspect.go` | edit | The third `engine.Route(n, nil)` site passes `h.mediaMatch`; the `TaskHandlers` receiver is shared, so no second constructor change. |
| `internal/api/server.go` | edit | Pull the registered ytdlp engine from the `engines` registry and pass its `Accepts` bound method into `NewTaskHandlers` — one cache, loaded once by `Engine.Connect`; absent engine → nil → rows 4-6, unchanged from today. |
| `internal/api/tasks_files_test.go` | edit | Update the `NewTaskHandlers` call for the new param (pass nil — today's assertions unchanged). |
| `internal/api/tasks_actions_test.go` | edit | Same signature update, plus one new case proving the wiring: a `TaskHandlers` built with `cache.Match` accepts a YouTube URL onto the yt-dlp engine where nil would route it to aria2. |
| `Dockerfile` | edit | Add `ARG YTDLP_SHA256_WHEEL` beside the existing pins — the generator refuses to import a wheel whose hash differs. For the current pin it is `1d57897e94c6665a0a6f9bc54b34e584284e32c034ffab3a7df25d8f7b24eedf` (sha256 of `yt_dlp-2026.8.19-py3-none-any.whl`, measured during this repair). |

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
package api

// TaskHandlers.mediaMatch is the row-3 hook handed to every engine.Route call.
// NewTaskHandlers receives it already bound — typically (*ytdlp.ExtractorCache).Match —
// or nil, which makes row 3 fall through to rows 4-6 rather than rejecting the URI.
func NewTaskHandlers(db *sqlx.DB, engines *engine.Registry, roots []string,
	guard *secure.Guard, resolver secure.Resolver, mediaMatch func(string) bool) *TaskHandlers
```

```go
package ytdlp

// Engine owns the one cache instance. Connect — already invoked at boot by the
// main.go engine probe — loads it; a load error is logged and leaves a usable
// empty cache, so Connect still succeeds and row 3 degrades to nil semantics.
// Accepts replaces today's `return false` stub; a nil cache answers false, which
// keeps forced-engine submissions rejecting exactly as they do today.
func (e *Engine) Accepts(uri string) bool // e.cache.Match(uri)
```

`internal/api/server.go` pulls the registered ytdlp engine from the `engines` registry
(`engines.Get(engine.NameYtDlp)`) and passes its `Accepts` into `NewTaskHandlers` — one
cache, one load, one source of "does yt-dlp claim this". If no ytdlp engine is
registered (nil-db boots, tests), `mediaMatch` stays nil: rows 4-6, same as today.

## Steps
1. Create `scripts/gen-ytdlp-patterns.py` per ADR-0022. It reads `YTDLP_VERSION` and
   `YTDLP_SHA256_WHEEL` from the `Dockerfile`, downloads that version's `py3-none-any` wheel from
   PyPI with stdlib `urllib` + `zipfile` (no pip, no third-party Python) — PyPI normalizes the
   version, so query with `08` → `8`-style segments (`".".join(str(int(s)) for s in tag.split("."))`)
   not the raw pin — **verifies the wheel's SHA-256 against the pin before unpacking or importing
   anything** — abort on mismatch — and then
   also refuses to run when the imported `yt_dlp.version.__version__` does not match
   `YTDLP_VERSION`. Optionally it accepts `--yt-dlp <path>` pointing at an already-unpacked package
   or wheel (the hash gate still applies to a wheel path; an unpacked dir is trusted as
   maintainer-supplied). With the stdlib only it:
   - enumerates every extractor class and collects `_VALID_URL` / `_VALID_URLS`, skipping `Generic`;
   - transpiles each pattern (the verbose flag is pattern-initial in every corpus case): strip `x`
     from a leading flag group while **preserving co-flags** — `(?xi)` becomes `(?i)`, `(?x)`
     vanishes — then drop unescaped whitespace and `#`-to-EOL comments outside character classes,
     **only inside the verbose region** — the whole pattern for a bare `(?x)`, or the interior of
     a leading `(?x:…)` group that reaches end-of-pattern. A `(?x` in any other position, or a
     scoped group that leaves a tail (whose literal spaces and `#` the strip would corrupt), lands
     the pattern on the residual list rather than guessing. Rewrite every capturing group — `(…)`
     and `(?P<name>…)` — to
     `(?:…)`, skipping escaped `\(` and parens inside character classes exactly as the whitespace
     pass does (a naive rewrite still compiles yet silently changes what matches), since routing
     needs only a boolean match; leave `(?i)` alone — Go's `regexp` accepts it;
   - writes every surviving pattern to a temp file and pipes it through a `go run` probe that
     `regexp.Compile`s each line; patterns that fail are dropped and their owning extractor name is
     recorded. `go` must be on the maintainer's `PATH` **and the probe must run under the repo's
     pinned toolchain** — honour `go.mod`'s `toolchain` directive (`GOTOOLCHAIN` default behaviour)
     since which regexp syntax compiles can shift between Go releases;
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
   `ResidualOverrides` into a lowercase host list. The alternation is start-anchored — yt-dlp
   evaluates `_VALID_URL` with `re.match`, so an unanchored `MatchString` would substring-match
   e.g. `https://evil.example/?next=youtube.com/watch`.
   `Match(uri)`: parse with `net/url`, lowercase the hostname (no match on parse failure or empty
   host), suffix-check the override hosts, then `pattern.MatchString`. No I/O anywhere.
5. Wire the hook. `internal/engine/ytdlp/engine.go`: add the `cache` field, load it in `Connect`
   (a failed load logs and leaves a usable empty cache — `Connect` still succeeds, and a nil or
   unloaded cache makes `Accepts`/`Match` answer false, preserving today's semantics), delegate
   `Accepts` to `cache.Match`. `internal/api/tasks.go`: `NewTaskHandlers` gains
   `mediaMatch func(string) bool`, stored on `TaskHandlers`; both `engine.Route(n, nil)` sites pass
   `h.mediaMatch`. `internal/api/tasks_inspect.go`: the third site does the same (shared receiver).
   `internal/api/server.go`: `engines.Get(engine.NameYtDlp)` and pass the engine's `Accepts` — the
   cache loads once inside `Connect`, not twice. Update the two test call sites in
   `tasks_files_test.go`/`tasks_actions_test.go` to pass nil. `internal/engine/router.go` needs **no
   edit** — row 3 already consults its `mediaMatch` param (T090 shipped it); only the call sites'
   nil changes.
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
   - `Match` returns false for `https://evilyoutube.com/x` and `https://notyoutu.be/x` — the override
     lookup is a label-boundary suffix match (`host == suffix` or `host` ends in `"." + suffix`),
     never a raw `strings.HasSuffix`;
   - the zero-value cache reports `Loaded() == false` and `Match() == false`.
7. In `tasks_actions_test.go`, add the wiring assertion: a `TaskHandlers` built with a stub
   `mediaMatch` that matches youtube URLs routes a `https://www.youtube.com/watch?v=…` submission to
   the yt-dlp engine while the same handler built with nil falls through to aria2 (the engine-side
   row-3 mechanics are already covered by `internal/engine/router_test.go`, and the real `Match` is
   covered in the ytdlp package — this case only proves the param reaches `Route`).

## Acceptance criteria
- [ ] `Match` performs no network or subprocess call.
- [ ] `Engine.Accepts` answers from the same cache, so an engine-forced YouTube submission
      (`engine: "ytdlp"`) is no longer refused at the `tasks.go` accept gate.
- [ ] `generic` patterns are never generated into the table.
- [ ] `youtube.com/watch`, `youtu.be` and `vimeo.com` URLs route to `ytdlp` via the override hosts, and
      `x.com/…/status/…` via the generated table.
- [ ] `https://releases.ubuntu.com/24.04/ubuntu-24.04.iso` routes to `aria2`, with and without the matcher.
- [ ] Every line in `extractor_patterns.txt` compiles, and every `extractors_residual.txt` name has an
      override entry — both enforced by tests, so a stale regen or a dropped override fails CI.
- [ ] `extractor_patterns.txt` and `extractors_residual.txt` carry the wheel-version header and are
      byte-identical across reruns against the same pin.
- [ ] The generator refuses to run against a `yt_dlp` package whose version ≠ `YTDLP_VERSION`, and
      aborts before unpacking when the downloaded wheel's SHA-256 ≠ `YTDLP_SHA256_WHEEL`.

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
- Do NOT wire the generator into `make gen`, the lint/test CI, or the image build. The one sanctioned
  automated run is T097's pin-bump workflow (a runner with both Python and `go`); anywhere else it is
  maintainer-run only.
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
