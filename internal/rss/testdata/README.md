# RSS feed fixtures

Feed bodies captured for `parse_test.go` (task T067), five of the eight
documented in `docs/08-rss-automation.md` section 9. All verbatim. The
`.golden.json` file of the same stem is the expected `ParseFeed` output —
regenerate with `go test ./internal/rss/... -update`.

| File | Capture command | Fetched |
|---|---|---|
| `arch_releases.xml` | `curl -sS "https://archlinux.org/feeds/releases/"` | 2026-09-19 |
| `academic_torrents.xml` | `curl -sSL "https://academictorrents.com/rss.xml"` | 2026-09-19 |
| `distrowatch_torrents.xml` | `curl -sSL "https://distrowatch.com/news/torrents.xml"` | 2026-09-19 |
| `linuxtracker.xml` | `curl -sSL "https://linuxtracker.org/rss.php"` (301 → `https://rss.linuxtracker.org/feed.xml`) | 2026-09-19 |
| `gutenberg_today.rss` | `curl -sSL "https://www.gutenberg.org/cache/epub/feeds/today.rss"` | 2026-09-19 |

Upstream drift since the doc's 2026-09-01 probe:

- `linuxtracker.xml` no longer matches the hostile-feed description in doc 08
  section 9: the feed moved to `rss.linuxtracker.org/feed.xml` and now emits
  RFC-822 `<pubDate>` values, `application/x-bittorrent` enclosures and
  info-hash `<guid isPermaLink="false">` values. The permissive
  `02/01/2006 15:04:05` date layout is still covered by
  `TestPermissiveDateFallback` against the documented emission.
- `academic_torrents.xml` rotates its items; the doc's golden item
  (`dcb9178653b651c7ca4526e11fa8e22f74e2fd7a`, `size 71000122956`) is not in
  today's body and is reproduced inline in `TestSynthesiseMagnetFromInfohash`.
- `gutenberg_today.rss` items are plain ebook pages: under the tier-D
  download gate every item is discarded, so its golden carries an empty
  `items` array.
