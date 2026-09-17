# Search test fixtures

Real documents captured for `torznab_test.go` (task T054). Where a fixture was
trimmed, the edit is noted.

| File | Source | Fetched |
|---|---|---|
| `torznab_hdaccess.xml` | Sonarr repository, `src/NzbDrone.Core.Test/Files/Indexers/Torznab/torznab_hdaccess_net.xml` (develop branch), verbatim | 2026-09-17 |
| `torznab_tpb.xml` | Sonarr repository, `src/NzbDrone.Core.Test/Files/Indexers/Torznab/torznab_tpb.xml` (develop branch), verbatim | 2026-09-17 |
| `torznab_caps.xml` | Sonarr repository, `src/NzbDrone.Core.Test/Files/Indexers/Newznab/newznab_caps.xml` (develop branch), verbatim; the caps schema is shared between newznab and torznab | 2026-09-17 |
| `academic_torrents_rss.xml` | `https://academictorrents.com/rss.xml`, trimmed to the first three items with descriptions shortened; no pubDate or torznab attrs, as upstream emits | 2026-09-17 |
| `torznab_error.xml` | Sonarr repository, `src/NzbDrone.Core.Test/Files/Indexers/Newznab/unauthorized.xml` (develop branch); UTF-8 BOM stripped | 2026-09-17 |

Verbatim quirks kept on purpose: `torznab_tpb.xml` carries upstream's
misspelled `<lanuage>` element, and its magnet `dn=` parameters still contain
the original series name — Sonarr's own anonymisation of the fixture was
incomplete. Neither affects the parser tests.
