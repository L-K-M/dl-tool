# T067 — Parse feed items and extract a download URI with the four-tier ladder

| Field | Value |
|---|---|
| **ID** | T067 |
| **Milestone** | M5 |
| **Status** | todo |
| **Depends on** | T015, T066 |
| **Blocks** | T069, T072 |
| **Parallel-safe** | yes — creates `internal/rss/parse.go` and its fixtures only |
| **Implements** | [FR-072](../02-requirements.md#fr-072-extract-a-download-uri-from-each-item) |
| **Decisions** | [ADR-0009](../decisions/0009-native-cross-protocol-rss-rules.md), [ADR-0010](../decisions/0010-never-execute-third-party-definitions.md) |
| **Est. size** | 3 new files, ~380 LOC |

## Goal
`Parser.ParseFeed` implements T066's `ItemParser`: it decodes RSS 2.0, RSS 0.91 and Atom, resolves one
download URI per item through the four-tier ladder, resolves an identity, normalises the title, and
discards an item that yields no URI at all.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/08-rss-automation.md` §3 Item parsing](../08-rss-automation.md#3-item-parsing) — the ladder, the
   deviations table, the identity chain, the format table and the date rules. This is the specification.
2. [`docs/08-rss-automation.md` §10 Feeds shipped as examples and used in tests](../08-rss-automation.md#10-feeds-shipped-as-examples-and-used-in-tests)
   — the eight feeds, the golden Academic Torrents item and the expected magnet.
3. [`docs/06-download-engines.md` §3.5 BitTorrent v2 (BEP 52) identity](../06-download-engines.md#35-bittorrent-v2-bep-52-identity)
   — 40-hex versus 64-hex, and base32 decoding. Never truncate a v2 hash.
4. [`docs/tasks/T066-feed-poller-and-backoff.md`](T066-feed-poller-and-backoff.md) — the `ItemParser`
   interface and `FeedMeta`, which this task implements without changing.
5. [`docs/13-testing-and-verification.md` §5 Golden-file fixtures](../13-testing-and-verification.md#5-golden-file-fixtures)
   — fixture naming, the `-update` flag and the `testdata/README.md` capture record.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/rss/parse.go` | create | `Parser`, the ladder, the identity chain, dates and `title_norm`. |
| `internal/rss/parse_test.go` | create | Ladder, identity, date and discard cases over the fixtures. |
| `internal/rss/testdata/` | create | The five committed feed bodies, their `.golden.json` files and `README.md`. |

No other file may be modified.

## Interface contract

```go
package rss

// Parser decodes feed bodies with github.com/mmcdole/gofeed. The bytes are fetched by poll.go,
// never by this file: the timeout, the 16 MiB cap and the SSRF guard stay with the caller.
type Parser struct{ /* now func() time.Time */ }

func NewParser(now func() time.Time) *Parser

// ParseFeed satisfies ItemParser. Items that resolve no download URI are dropped, not returned.
func (p *Parser) ParseFeed(feedID, baseURL string, body []byte) (FeedMeta, []store.FeedItem, error)

// Tier names the branch of the extraction ladder that produced a URI, for tests and the dry run.
type Tier string

const (
	TierEnclosureOrMagnet Tier = "A" // x-bittorrent enclosure and magnet <link>: document order, LAST wins
	TierUntypedEnclosure  Tier = "B" // enclosure with an empty or absent type attribute
	TierSynthesised       Tier = "C" // torznab magneturl/infohash, <infohash>, BEP 36, media:*, atom enclosure
	TierLink              Tier = "D" // <link>, atom alternate href, then <guid> when isPermaLink != "false"
)

// ExtractDownloadURI applies tiers A to D in order and returns the winning URI, the lowercase-hex
// info hash when one is known (40 chars for v1, 64 for v2), and the tier that produced it.
// ok is false when the item yields nothing and must be discarded.
func ExtractDownloadURI(it *gofeed.Item, baseURL string) (uriStr, infoHash string, tier Tier, ok bool)

// Identity resolves feed_items.identity: <guid>/<id>, else the info hash, else the download URI,
// else hex(sha1(title + "\x00" + feedID)).
func Identity(feedID string, it *gofeed.Item, downloadURI, infoHash string) string

// NormaliseTitle lowercases, replaces '.' and '_' with a space, collapses whitespace runs and trims.
func NormaliseTitle(s string) string

// ParseItemDate tries RFC 1123/822 first, then the permissive form "02/01/2006 15:04:05"
// that Linuxtracker emits. ok is false when both fail and the caller substitutes now.
func ParseItemDate(raw string) (unixMS int64, ok bool)
```

Tier A is **document-order last-wins**, copied from qBittorrent's `rss_parser.cpp`: an
`application/x-bittorrent` enclosure and a `magnet:` `<link>` write the same slot, and two
`x-bittorrent` enclosures behave the same way. The enclosure `type` test is a **prefix** match, so
`application/x-bittorrent;x-scheme-handler/magnet` qualifies — qBittorrent's exact-equality test drops
that item entirely, and dl-tool deliberately does not copy that bug.

Tier C synthesises, in this order: `torznab:attr[@name="magneturl"]`, `torznab:attr[@name="infohash"]`,
an unprefixed `<infohash>` child, BEP 36 `<torrent>/<magneturi>` or `<torrent>/<infohash>`,
`media:content/@url` ending in `.torrent` or `media:hash[@algo="sha1"]`, and an Atom
`link[@rel="enclosure"]`. A synthesised magnet is
`magnet:?xt=urn:btih:<hash>&dn=<url-encoded title>`.

Size comes from `torznab:attr[@name="size"]` first, then an unprefixed `<size>`, then
`enclosure/@length`, which is a hint only.

## Steps
1. Create `internal/rss/parse.go` with `Parser`, `NewParser` and `ParseFeed`, decoding with
   `gofeed.Parser` over `bytes.NewReader(body)`.
2. Read `<infohash>` and `<size>` by **local name** through gofeed's extensions map: Academic Torrents
   declares `xmlns:academictorrents` yet emits both elements unprefixed.
3. Implement `ExtractDownloadURI` as tiers A, B, C, D, consulting a tier only when every earlier tier
   yielded nothing, and keeping the last document-order write inside tier A.
4. Normalise every recovered hash to lowercase hex, decoding a 32-character base32 `btih` value to 20
   bytes first, and keep a 64-hex v2 hash at 64 characters.
5. Implement `Identity` as the four-step chain and drop an item for which all four are empty; inside one
   fetch keep a `map[string]struct{}` of identities and drop repeats before returning.
6. Implement `ParseItemDate`, strip CDATA from every field first, and substitute `now` with a `debug` log
   line when both parsers fail.
7. Fill `FeedMeta` from `<title>`, `<ttl>` clamped to `[5,1440]` **minutes**, `sy:updatePeriod` and
   `sy:updateFrequency`, `<skipHours>` and `<skipDays>`.
8. Commit five fixtures under `internal/rss/testdata/`: `arch_releases.xml`, `academic_torrents.xml`,
   `distrowatch_torrents.xml`, `linuxtracker.xml` and `gutenberg_today.rss`, each with its
   `.golden.json`, and record the `curl` command and the capture date in
   `internal/rss/testdata/README.md`.
9. Create `internal/rss/parse_test.go` with the `-update` golden pattern of doc 13 §5 plus table cases:
   tier A from Arch Linux; tier A last-wins when an item carries an `x-bittorrent` enclosure followed by a
   `magnet:` link; a `application/x-bittorrent;x-scheme-handler/magnet` enclosure still resolves in tier A;
   tier C source 3 turns Academic Torrents' `<infohash>` into
   `magnet:?xt=urn:btih:dcb9178653b651c7ca4526e11fa8e22f74e2fd7a&dn=…` with `size_bytes = 71000122956`;
   tier D from DistroWatch; an item with only a details page and no hash is discarded; Linuxtracker's
   `31/08/2026 18:47:19` parses through the fallback; Gutenberg's RSS 0.91 items fall to identity step 3
   or 4.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestTierALastWins` asserts the magnet, not the earlier enclosure, is kept.
- [ ] `TestTorznabCompoundTypeResolvesInTierA` passes.
- [ ] `TestSynthesiseMagnetFromInfohash` asserts the exact magnet string above.
- [ ] `TestItemWithoutURIIsDiscarded` asserts the item is absent from the returned slice.
- [ ] `TestPermissiveDateFallback` and `TestNormaliseTitle` pass.
- [ ] Every golden file regenerates unchanged under `-update`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/rss/... && echo PARSE_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/rss`, every test named above reported as `--- PASS`, and
the final line of stdout is exactly `PARSE_OK`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table, expanded to the individual files under
`internal/rss/testdata/`.

## Out of scope — do NOT
- Do NOT fetch a feed, a `.torrent` file or anything else from this file; T066 owns every request.
- Do NOT evaluate rules or compute an episode key here; T068 and T069 own those.
- Do NOT copy qBittorrent's exact-equality enclosure test, and do NOT rank tier A's two sources against
  each other — document order decides.
- Do NOT dedup on the download URL; doc 08 §7 forbids it. Identity and info hash are the keys.
- Do NOT add a feed to the fixture set that is not one of the eight in doc 08 §10.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
