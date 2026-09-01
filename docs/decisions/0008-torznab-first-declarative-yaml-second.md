# 0008 - Torznab first, declarative YAML engines second

> **Status:** accepted
> **Date:** 2026-09-01
> **Deciders:** repository owner

## Context and Problem Statement

Download Station shipped "BT Search", backed by `.dlm` modules — tar.gz archives containing a `search.php`.
Search is one of the five things dl-tool owns as a first-class user feature, so it must exist; the question
here is what a "search engine" *is* inside dl-tool: a URL, a declarative document, or a program.

## Decision Drivers

- [ADR-0010](0010-never-execute-third-party-definitions.md) forbids executing third-party code: `.dlm` is
  PHP and nova3 plugins are Python, so neither can be run.
- dl-tool ships zero piracy indexers. The Cardigann corpus is 547 definitions in `Prowlarr/Indexers` and
  553 in Jackett, mostly private or piracy trackers, and `Prowlarr/Indexers` carries no `LICENSE` file
  while its content is Jackett-derived GPLv2.
- Most users of a tool in this class already run Prowlarr or Jackett. A Torznab client inherits both, plus
  NZBHydra2 and bitmagnet, for the cost of one wire format.

## Considered Options

- **Option A** — A Torznab/Newznab client only; no native definitions at all.
- **Option B** — Torznab/Newznab client as the primary surface, plus a `dlsearch/v1` declarative YAML
  format, a strict Cardigann subset, for sites that do not speak Torznab.
- **Option C** — Implement Cardigann v11 in full, so the existing definition corpus loads unmodified.
- **Option D** — Proxy qBittorrent's `search/*` API and let it run nova3 Python plugins.
- **Option E** — Embed a DHT crawler so dl-tool needs no indexers at all.

## Decision Outcome

Chosen option: **Option B**, because the Torznab client is where ninety per cent of the engineering value
sits and it executes nothing, while a small declarative format covers the RSS, JSON and simple-HTML sites
no proxy fronts. Being a strict Cardigann subset means most v11 definitions convert mechanically.

Fixed by this decision; the schema and the client live in
[`../07-search-and-indexers.md`](../07-search-and-indexers.md):

- The client parses one `GET /api` with `t=caps|search|tvsearch|movie|music|book`, namespace
  `http://torznab.com/schemas/2015/feed`, and reads `<torznab:attr name= value=/>`. It **ignores the
  `<rss version>` attribute** (Jackett emits `2.0`, Prowlarr `1.0`), and treats `<torznab:attr name="size">`
  as authoritative because `size` and `category` are the only non-optional predefined attributes.
- Prowlarr indexer id `0` is a synthetic self-test indexer answering any search with one hard-coded release
  titled "Test Release", so it is never treated as a real indexer; Prowlarr also returns real HTTP status
  codes with non-spec error codes (`410` disabled, `429` + `Retry-After`), which the client honours.
- Exactly four engines are bundled and enabled by default: `internet-archive`, `arch-linux`,
  `academic-torrents` and `linux-distributions` (a curated static list, not a scraper); imported engines
  start **disabled** and show their provenance.

### Consequences

- Good, because a Prowlarr or Jackett user gets every indexer they have configured from one URL and one API
  key, and there is no code execution anywhere in the search path.
- Bad, because sites speaking neither Torznab nor a simple RSS, JSON or HTML shape are unreachable in v1
  (`dlsearch/v1` omits captcha, FlareSolverr, cookie login, `download.before` and `rows.dateheaders`), and
  there are now two engine surfaces to document, validate and version instead of one.
- Neutral, because `.dlm` and nova3 `.py` files are import-only: analysed statically, never executed.

### Confirmation

```bash
make test PKG=./internal/search/... && ls definitions/engines/
```

Expected: exit 0, and exactly `academic-torrents.yaml`, `arch-linux.yaml`, `internet-archive.yaml`,
`linux-distributions.yaml`. `internal/search/` carries golden-file tests over recorded Jackett and Prowlarr
`t=caps` and `t=search` responses in `testdata/`, plus `TestIgnoresRssVersionAttribute`,
`TestHonoursRetryAfter` and `TestBundledEnginesValidate`, which validates every bundled YAML against the
`dlsearch/v1` schema and fails if a fifth file appears.

## Pros and Cons of the Options

### Option A - Torznab client only

- Good, because it is the smallest surface: one spec, one parser, real fixtures, nothing to sandbox.
- Bad, because a user who runs no proxy gets nothing on first boot, failing the "same thing from the user's
  point of view" test.

### Option B - Torznab client plus dlsearch/v1

- Good, because the four bundled engines make search work on first boot with no external service, and a
  bad definition is a validation error surfaced in the UI rather than a crash.
- Bad, because a subset invites "why does my Jackett definition not load"; `07` lists what is unsupported.

### Option C - full Cardigann v11

- Good, because 547 definitions would load unmodified and Jackett's documentation would apply verbatim.
- Bad, because v11 carries captcha blocks, FlareSolverr hints, cookie login, a 25-name filter enum, a Go
  template dialect and a `download.before` chain — months of work on a schema Prowlarr bumps yearly, over a
  corpus whose licence status is unresolved.

### Option D - proxy qBittorrent's search plugins

- Good, because it is nearly free: the API exists and the plugin ecosystem is maintained by others.
- Bad, because it executes arbitrary Python inside the qBittorrent image and cannot route a result to aria2
  or yt-dlp — the plugins are BitTorrent-only by construction.

### Option E - embed a DHT crawler

- Good, because it needs no indexer configuration at all and indexes what the network actually has.
- Bad, because result quality is poor by the crawler's own documentation and it makes dl-tool an index
  rather than a client. bitmagnet stays an external Torznab provider.

## More Information

- Research: `indexers.md` §1, §2, §6, §7 and fact-check notes 1a, 4a, 4b — summarised in
  [`../16-prior-art-and-research.md`](../16-prior-art-and-research.md).
- Depends on this decision: [`../07-search-and-indexers.md`](../07-search-and-indexers.md).
