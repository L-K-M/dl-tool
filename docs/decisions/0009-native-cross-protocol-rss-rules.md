# 0009 - A native cross-protocol RSS rule engine

> **Status:** accepted
> **Date:** 2026-09-01
> **Deciders:** repository owner

## Context and Problem Statement

Download Station's RSS automation is a two-field filter — Matches and Does-not-match, with a regex toggle
and a destination. It is case-sensitive, has no dry run, no episode awareness and no retroactive apply, and
it deduplicates on the item's `download_uri`, so a feed that rotates its download URLs re-downloads the same
release forever. qBittorrent's auto-downloader is the de-facto standard rule model, but it lives inside one
BitTorrent client. dl-tool has to decide whose rule engine it runs, and where.

## Decision Drivers

- A feed item may be a magnet, an HTTP URL or a media-site URL. A rule must be able to land in whichever
  engine the item needs, which is the entire point of a cross-protocol queue.
- [ADR-0017](0017-exclusive-control-of-engines.md) means dl-tool assumes exclusive control of its engines.
  Two RSS engines polling one feed produce duplicate grabs and irreproducible bug reports.
- The dry run is what makes an RSS rule UI usable and is exactly what Download Station lacks, and rules are
  user data that must survive an engine being swapped out.

## Considered Options

- **Option A** — Pass rules through to qBittorrent's `rss/*` API and let it do the work.
- **Option B** — A native rule engine in dl-tool, modelled on qBittorrent's rule semantics, targeting any engine.
- **Option C** — Embed or shell out to FlexGet for feed handling and matching.
- **Option D** — No RSS in v1.

## Decision Outcome

Chosen option: **Option B**, because a passthrough rule can only ever produce a torrent, and a rule that
cannot grab an HTTP release is not a feature of this product. Modelling on qBittorrent keeps the semantics
users already know while fixing the traps its implementation carries.

Fixed here; the schema and the algorithm live in [`../08-rss-automation.md`](../08-rss-automation.md):

- Match expressions are real arrays (`any_of`, `none_of`), never qBittorrent's `"a|b"` string split on `|`.
  An empty entry in `none_of` must be rejected at save time: in qBittorrent an empty expression matches
  everything, so a trailing `|` silently rejects every article.
- Matching is case-insensitive by default, like qBittorrent and unlike Download Station.
- `episode.filter` is validated when the rule is saved, not silently at match time, and the season number is
  normalised: qBittorrent interpolates the raw captured season, so `01x05;` cannot match `Show 1x05` while
  `1x05;` matches both.
- URL extraction follows qBittorrent's *actual* parser: an `application/x-bittorrent` enclosure and a
  `magnet:` `<link>` both write the torrent URL directly and **the last one in document order wins**; then
  an enclosure with empty or absent `type`; then `<link>`. The type is matched by prefix, so Torznab's
  `application/x-bittorrent;x-scheme-handler/magnet` is accepted — qBittorrent compares exactly, and drops it.
- Deduplication is a ladder: feed-item GUID, then infohash (v1 and v2), then a normalised content key.
  Every rejection carries a machine-readable reason code; those codes are the whole value of the dry run.
- Polling defaults to 30 minutes with ±10 % jitter, uses conditional GET, treats `<ttl>` and
  `sy:updatePeriod` as a floor, and backs off on `0, 60, 300, 900, 1800, 3600, 10800, 21600, 43200, 86400`
  seconds.

### Consequences

- Good, because one rule can target any engine, and `POST /rules/{id}/run` applies a new rule to items
  already fetched — Download Station's single most-reported RSS complaint.
- Good, because the dry run explains every non-match with a reason code instead of silently doing nothing.
- Bad, because dl-tool now owns feed parsing, episode parsing and deduplication, which is where every
  competitor's bug reports live; golden-file fixtures under `internal/rss/testdata/` are mandatory.
- Bad, because qBittorrent's own RSS processing must be forced off at conformance time; an operator who
  re-enables it in the qBittorrent UI gets duplicate grabs, and the runbook has to say so.
- Neutral, because release-name parsing is not attempted at Sonarr's depth in v1: the episode parser falls
  back to qBittorrent's four default regexes and reports `unparseable_episode` rather than guessing.

### Confirmation

```bash
make test PKG=./internal/rss/...
```

Expected: exit 0. The suite pins the four inherited traps by name —
`TestURLExtractionLastWinsInDocumentOrder`, `TestEnclosureTypeMatchedByPrefix`,
`TestEmptyNoneOfEntryRejectedAtSave` and `TestSeasonLeadingZeroNormalised` — over golden fixtures in
`internal/rss/testdata/` covering RSS 2.0, Atom and Torznab. `web/e2e/rss-dry-run.spec.ts` fails if any
evaluated item is shown without a reason code.

## Pros and Cons of the Options

### Option A - passthrough to qBittorrent's rss/\*

- Good, because it is nearly free, and it inherits a rule engine debugged for a decade.
- Bad, because a rule can then only produce a torrent, so an RSS feed of HTTP releases is unusable, and the
  rules live in qBittorrent's configuration — invisible to dl-tool's users, permissions and backup.

### Option B - native cross-protocol rule engine

- Good, because rules are dl-tool data — exported, restored and owned by a user — and the traps above are
  fixed once, in one place, with a test each.
- Bad, because it is the largest logic surface in the product with no upstream to inherit fixes from.

### Option C - FlexGet

- Good, because its `series` and `seen` plugins are genuinely more capable than anything written here.
- Bad, because it is a Python application with its own YAML configuration and its own scheduler, which means
  a second runtime in the image and a second source of truth for automation.

### Option D - no RSS in v1

- Good, because it removes the largest logic surface from the first release.
- Bad, because RSS automation is a headline Download Station feature, so shipping without it fails the
  acceptance test the product is measured against.

## More Information

- Research: `rss.md` §1.2, §2, §5.6, §8 and its fact-check corrections — summarised in
  [`../16-prior-art-and-research.md`](../16-prior-art-and-research.md).
- Depends on this decision: [`../08-rss-automation.md`](../08-rss-automation.md).
