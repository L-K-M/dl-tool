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
| `archlinux_releases.xml` | `https://archlinux.org/feeds/releases/`, verbatim — three release items with enclosure length/type/url, the shape `def_valid_rss.yaml` expects | 2026-09-18 |
| `archive_advancedsearch.json` | `https://archive.org/advancedsearch.php?q=format%3A%22Archive+BitTorrent%22+AND+mediatype%3Atexts&fl%5B%5D=identifier%2Ctitle%2Citem_size%2Cbtih%2Cpublicdate&rows=2&output=json`, verbatim — `response.numFound`/`response.docs[]` shape | 2026-09-18 |

Verbatim quirks kept on purpose: `torznab_tpb.xml` carries upstream's
misspelled `<lanuage>` element, and its magnet `dn=` parameters still contain
the original series name — Sonarr's own anonymisation of the fixture was
incomplete. Neither affects the parser tests.

## `.dlm` fixtures (task T059)

The three `.dlm` files are synthetic gzip-compressed tar archives authored
for `dlm_import_test.go`; they are not captured modules.

| File | Contents |
|---|---|
| `jackett.dlm` | `INFO` (name `jackett`, `accountsupport: true`) plus a `search.php` modelled on the third-party jackett.dlm v1.0.2 — `simplexml_load_string` plus `torznab:attr`, so it converts to `kind: torznab` |
| `rssmodule.dlm` | `INFO` (the guide's `mininova` example) plus a `search.php` that calls `addRSSResults` with one `http` URL literal ending `?search=`, so it converts to `kind: rss` |
| `hostile.dlm` | valid `INFO` and `search.php` plus three hostile members: a symlink, a name containing `..`, and a 5 MiB member |
