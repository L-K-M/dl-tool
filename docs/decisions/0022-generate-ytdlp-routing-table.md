# ADR-0022: Generate the yt-dlp routing table from the pinned wheel at maintenance time

- Status: accepted
- Date: 2026-09-25
- Decides: [T088](../tasks/T088-ytdlp-extractor-cache.md), the open mechanism in
  [06-download-engines.md §7.2](../06-download-engines.md#72-routing-check)

## Context

Routing-table row 3 (doc 06 §2) must answer "does a yt-dlp extractor claim this URI" cheaply, offline
and without running a metadata extraction — skipping `generic`, which matches everything and would
steal rows 4–6. The originally prescribed mechanism (shell out at start-up to enumerate extractor URL
patterns into a Go `regexp` cache) is impossible, as measured against the pinned yt-dlp 2026.08.19:

- No CLI flag enumerates patterns — `--list-extractors` prints 1 752 names, `--extractor-descriptions`
  prints prose; the patterns live only in each extractor class's `_VALID_URL`.
- The patterns are Python `re`, not RE2: 291 of 1 787 fail `regexp.Compile` verbatim.

## Decision

The pattern table is **generated at maintenance time by a checked-in script, and the output is
committed as source**. Nothing in the image or the running binary ever invokes yt-dlp to answer
`Accepts`.

`scripts/gen-ytdlp-patterns.py` runs on each yt-dlp pin bump (the weekly bump in
[ADR-0018](0018-pin-ytdlp-by-version-and-hash.md) / T097). It downloads the pinned wheel from PyPI,
verifies its SHA-256 against `ARG YTDLP_SHA256_WHEEL` in the `Dockerfile` **before unpacking or
importing anything** — the wheel's own `__version__` string is only a second gate, since the artifact
being validated cannot attest itself — then reads every extractor's `_VALID_URL`, transpiles the
Python-only syntax measured below, rewrites capture groups (including `(?P<name>…)`) to
non-capturing (routing needs only a boolean match, and stripping them keeps the merged alternation
from depending on Go's tolerance for duplicate group names), verifies each result with Go's
`regexp.Compile`, and writes two files:

- `internal/engine/ytdlp/extractor_patterns.txt` — one RE2 pattern per line, `generic` never emitted,
  embedded into the binary with `//go:embed`. A comment header records the wheel version — no date,
  so reruns on the same pin are byte-identical.
- `internal/engine/ytdlp/extractors_residual.txt` — the extractor names whose patterns still do not
  compile, so regeneration diffs show coverage drift instead of hiding it.

`internal/engine/ytdlp/overrides.go` is the hand-maintained counterpart: a map from each residual
extractor name to host suffixes (e.g. `Youtube: youtube.com, youtube-nocookie.com, youtubekids.com,
youtu.be`). Hostname granularity is
correct for routing — row 3 answers "should yt-dlp see this URL", not "which extractor claims it";
yt-dlp still picks its own extractor at download time. A test asserts every residual name has at
least one override host, so a regenerating pin bump that strands an extractor fails CI loudly.

`Match` is two lookups and no I/O: a hostname suffix-match against the overrides **on label
boundaries** — lowercase host, `host == suffix` or `host` ends in `"." + suffix`, so
`evilyoutube.com` never matches `youtube.com` — then a match against one compiled alternation of the
generated table (measured on this corpus: 163 ms to compile once at load, ≈0.5 ms per lookup).

## Measured basis (yt-dlp 2026.08.19 wheel, Go 1.26)

- 1 787 `_VALID_URL` patterns across 1 751 extractor classes; verbatim `regexp.Compile`: 1 496 pass,
  291 fail — 289 unsupported Perl syntax, 1 invalid named capture, 1 invalid escape. (This recounts
  the corpus the OPEN note surveyed — 1 702 patterns / 284 failures / 1 752 `--list-extractors`
  names: the recount expands multi-pattern `_VALID_URLS` tuples per pattern instead of per class and
  counts classes rather than printed names, which is why both totals moved.)
- Transpiling the verbose flag (all 214 `(?x` occurrences are pattern-initial: remove `x` from the
  flag set while preserving co-flags — `(?xi)` becomes `(?i)` — then strip unescaped whitespace and
  `#` comments outside character classes, within the verbose region only) brings the pass count to
  1 694 — 94.8 % of the corpus.
- The 93 residual patterns belong to 89 extractors — including Youtube, Vimeo, Instagram, Soundcloud,
  Dailymotion, CBC, Imgur, Rumble and Nebula — and fail on look-around, conditional and backreference
  constructs RE2 has no equivalent for. They are precisely the hosts that must not be dropped, which
  is why the override table exists.

## Alternatives considered

- **Hand-curated pattern list** — covers a dozen sites and rots immediately; upstream coverage comes
  free here.
- **Image-build or runtime enumeration** — the runtime image is Alpine with no Python, and no flag
  exists anyway; generation is a maintainer/CI step, so the committed output stays deterministic,
  reviewable and drift-checked.
- **Runtime `--simulate` probe per URI** — a subprocess and possible network call per `Accepts`;
  violates the offline-and-cheap rule outright.
- **Drop row 3** — media URIs fall through to aria2, which downloads the HTML page as a file: a
  silent mis-route for the media lane's core case.

## Consequences

- Regeneration runs Python over upstream extractor code on a maintainer machine — the same trust
  posture as updating the pinned binary; the committed output is plain text reviewed in the diff.
- `Accepts` precision is host-level for the residual set: any URL on an overridden host routes to
  yt-dlp, which arbitrates correctly. Nothing routes wrong; a few non-media pages on media hosts go
  to yt-dlp's generic extractor and fail there.
- If a future yt-dlp rewrites a residual extractor to RE2-safe syntax, the override entry becomes
  unreachable — the coverage test notes the surplus rather than failing.
