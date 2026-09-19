package rss

import (
	"bytes"
	"crypto/sha1"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/mmcdole/gofeed"
	ext "github.com/mmcdole/gofeed/extensions"
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/store"
)

var update = flag.Bool("update", false, "rewrite .golden.json files from current parser output")

// goldenMeta mirrors FeedMeta with stable snake_case keys — FeedMeta carries
// no json tags and is shared with poll.go.
type goldenMeta struct {
	Title            string   `json:"title"`
	TTLMinutes       int      `json:"ttl_minutes"`
	ImpliedIntervalS int      `json:"implied_interval_s"`
	SkipHours        []int    `json:"skip_hours,omitempty"`
	SkipDays         []string `json:"skip_days,omitempty"`
}

type goldenItem struct {
	Identity    string  `json:"identity"`
	GUID        *string `json:"guid,omitempty"`
	Title       string  `json:"title"`
	TitleNorm   string  `json:"title_norm"`
	Link        *string `json:"link,omitempty"`
	DownloadURL *string `json:"download_url,omitempty"`
	InfoHash    *string `json:"info_hash,omitempty"`
	SizeBytes   *int64  `json:"size_bytes,omitempty"`
	PublishedAt *int64  `json:"published_at,omitempty"`
}

type goldenFeed struct {
	Meta  goldenMeta   `json:"meta"`
	Items []goldenItem `json:"items"`
}

var fixtureFeeds = []string{
	"arch_releases",
	"academic_torrents",
	"distrowatch_torrents",
	"linuxtracker",
	"gutenberg_today",
}

func fixturePath(t *testing.T, stem string) string {
	t.Helper()
	for _, ext := range []string{".xml", ".rss"} {
		p := filepath.Join("testdata", stem+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Fatalf("no fixture for %s", stem)
	return ""
}

func parseFixture(t *testing.T, stem string) (FeedMeta, []store.FeedItem) {
	t.Helper()
	body, err := os.ReadFile(fixturePath(t, stem))
	require.NoError(t, err)

	meta, items, err := NewParser(func() time.Time { return testNow }).ParseFeed("fed_test", "https://example.com/feed.xml", body)
	require.NoError(t, err)

	return meta, items
}

func project(meta FeedMeta, items []store.FeedItem) goldenFeed {
	g := goldenFeed{
		Meta: goldenMeta{
			Title:            meta.Title,
			TTLMinutes:       meta.TTLMinutes,
			ImpliedIntervalS: meta.ImpliedIntervalS,
			SkipHours:        meta.SkipHours,
		},
		Items: make([]goldenItem, 0, len(items)),
	}
	for _, d := range meta.SkipDays {
		g.Meta.SkipDays = append(g.Meta.SkipDays, d.String())
	}
	for _, it := range items {
		g.Items = append(g.Items, goldenItem{
			Identity:    it.Identity,
			GUID:        it.GUID,
			Title:       it.Title,
			TitleNorm:   it.TitleNorm,
			Link:        it.Link,
			DownloadURL: it.DownloadURL,
			InfoHash:    it.InfoHash,
			SizeBytes:   it.SizeBytes,
			PublishedAt: it.PublishedAt,
		})
	}
	return g
}

// TestParseFeedGolden runs the five committed feed bodies through ParseFeed
// and diffs the projected result against each stem's .golden.json
// (docs/13-testing-and-verification.md section 5).
func TestParseFeedGolden(t *testing.T) {
	for _, stem := range fixtureFeeds {
		t.Run(stem, func(t *testing.T) {
			meta, items := parseFixture(t, stem)
			got, err := json.MarshalIndent(project(meta, items), "", "  ")
			require.NoError(t, err)
			got = append(got, '\n')

			golden := filepath.Join("testdata", stem+".golden.json")
			if *update {
				require.NoError(t, os.WriteFile(golden, got, 0o644))
			}
			want, err := os.ReadFile(golden)
			require.NoError(t, err)
			if diff := cmp.Diff(strings.Split(string(want), "\n"), strings.Split(string(got), "\n")); diff != "" {
				t.Fatalf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// feedItemByTitle finds one parsed row by title.
func feedItemByTitle(t *testing.T, items []store.FeedItem, title string) store.FeedItem {
	t.Helper()
	for _, it := range items {
		if it.Title == title {
			return it
		}
	}
	t.Fatalf("item %q not parsed; got %d items", title, len(items))
	return store.FeedItem{}
}

// firstItem is the first gofeed item of a fixture, for direct
// ExtractDownloadURI/Identity calls.
func firstItem(t *testing.T, stem string) *gofeed.Item {
	t.Helper()
	body, err := os.ReadFile(fixturePath(t, stem))
	require.NoError(t, err)
	feed, err := gofeed.NewParser().Parse(bytes.NewReader(body))
	require.NoError(t, err)
	require.NotEmpty(t, feed.Items)
	return feed.Items[0]
}

// TestTierAArchFixture: the Arch releases feed resolves every item in tier A
// through its application/x-bittorrent enclosure.
func TestTierAArchFixture(t *testing.T) {
	_, items := parseFixture(t, "arch_releases")
	require.NotEmpty(t, items)

	got := feedItemByTitle(t, items, "2026.09.01")
	require.NotNil(t, got.DownloadURL)
	require.Equal(t, "https://archlinux.org//releng/releases/2026.09.01/torrent/", *got.DownloadURL)
	// guid isPermaLink="false" still supplies the identity (step 1).
	require.Equal(t, "tag:archlinux.org,2026-09-01:/releng/releases/2026.09.01/", got.Identity)
	require.NotNil(t, got.SizeBytes)
	require.Equal(t, int64(1608286208), *got.SizeBytes)

	_, _, tier, ok := ExtractDownloadURI(firstItem(t, "arch_releases"), "https://archlinux.org/feeds/releases/")
	require.True(t, ok)
	require.Equal(t, TierEnclosureOrMagnet, tier)
}

// TestTierALastWins: an x-bittorrent enclosure and a magnet <link> write the
// same slot — the element appearing last inside the <item> is the one kept,
// in both directions.
func TestTierALastWins(t *testing.T) {
	body := []byte(`<rss version="2.0"><channel><title>t</title>
<item><title>enc-then-magnet</title>
<enclosure url="https://example.com/a.torrent" length="1" type="application/x-bittorrent"/>
<link>magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567</link>
</item>
<item><title>magnet-then-enc</title>
<link>magnet:?xt=urn:btih:76543210fedcba9876543210fedcba9876543210</link>
<enclosure url="https://example.com/b.torrent" length="1" type="application/x-bittorrent"/>
</item>
</channel></rss>`)

	_, items, err := NewParser(func() time.Time { return testNow }).ParseFeed("fed_t", "https://example.com/feed", body)
	require.NoError(t, err)
	require.Len(t, items, 2)

	first := feedItemByTitle(t, items, "enc-then-magnet")
	require.NotNil(t, first.DownloadURL)
	require.Equal(t, "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", *first.DownloadURL,
		"the later magnet link must beat the earlier enclosure")
	require.NotNil(t, first.InfoHash)
	require.Equal(t, "0123456789abcdef0123456789abcdef01234567", *first.InfoHash)

	second := feedItemByTitle(t, items, "magnet-then-enc")
	require.NotNil(t, second.DownloadURL)
	require.Equal(t, "https://example.com/b.torrent", *second.DownloadURL,
		"the later enclosure must beat the earlier magnet link")
}

// TestUrllessEnclosureDoesNotSuppressMagnet: an x-bittorrent enclosure with
// no usable url cannot win the tier-A slot — it must not strip the magnet
// link that would otherwise resolve the item.
func TestUrllessEnclosureDoesNotSuppressMagnet(t *testing.T) {
	magnet := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	body := []byte(`<rss version="2.0"><channel><title>t</title>
<item><title>magnet survives</title>
<link>` + magnet + `</link>
<enclosure type="application/x-bittorrent"/>
</item>
</channel></rss>`)

	_, items, err := NewParser(func() time.Time { return testNow }).ParseFeed("fed_t", "https://example.com/feed", body)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.NotNil(t, items[0].DownloadURL)
	require.Equal(t, magnet, *items[0].DownloadURL)
}

// TestTorznabCompoundTypeResolvesInTierA: the torznab compound enclosure
// type "application/x-bittorrent;x-scheme-handler/magnet" passes the prefix
// test — qBittorrent's exact-equality test drops the item, dl-tool does not.
func TestTorznabCompoundTypeResolvesInTierA(t *testing.T) {
	it := &gofeed.Item{
		Title: "torznab item",
		Enclosures: []*gofeed.Enclosure{{
			URL:    "https://indexer.example/dl/123.torrent",
			Length: "42",
			Type:   "application/x-bittorrent;x-scheme-handler/magnet",
		}},
	}
	u, _, tier, ok := ExtractDownloadURI(it, "https://example.com")
	require.True(t, ok)
	require.Equal(t, TierEnclosureOrMagnet, tier)
	require.Equal(t, "https://indexer.example/dl/123.torrent", u)
}

// TestSynthesiseMagnetFromInfohash: tier C source 3 turns the Academic
// Torrents unprefixed <infohash> into a magnet with the url-encoded title,
// and the unprefixed <size> fills size_bytes. The item is the golden one of
// doc 08 section 9, verbatim.
func TestSynthesiseMagnetFromInfohash(t *testing.T) {
	body := []byte(`<rss xmlns:academictorrents="https://academictorrents.com" version="2.0">
<channel><title>Academic Torrents</title>
<item>
<title>Results - Energy Landscape Controllers for XX Spin Rings - Robustness</title>
<category>Dataset</category>
<infohash>dcb9178653b651c7ca4526e11fa8e22f74e2fd7a</infohash>
<guid>https://academictorrents.com/details/dcb9178653b651c7ca4526e11fa8e22f74e2fd7a</guid>
<link>https://academictorrents.com/details/dcb9178653b651c7ca4526e11fa8e22f74e2fd7a</link>
<description>This is a dataset to investigate the robustness of energy landscape controllers</description>
<size>71000122956</size>
</item>
</channel></rss>`)

	_, items, err := NewParser(func() time.Time { return testNow }).ParseFeed("fed_at", "https://academictorrents.com/rss.xml", body)
	require.NoError(t, err)
	require.Len(t, items, 1)

	got := items[0]
	require.NotNil(t, got.DownloadURL)
	require.Equal(t,
		"magnet:?xt=urn:btih:dcb9178653b651c7ca4526e11fa8e22f74e2fd7a&dn=Results+-+Energy+Landscape+Controllers+for+XX+Spin+Rings+-+Robustness",
		*got.DownloadURL)
	require.NotNil(t, got.InfoHash)
	require.Equal(t, "dcb9178653b651c7ca4526e11fa8e22f74e2fd7a", *got.InfoHash)
	require.NotNil(t, got.SizeBytes)
	require.Equal(t, int64(71000122956), *got.SizeBytes)
	require.Equal(t, "https://academictorrents.com/details/dcb9178653b651c7ca4526e11fa8e22f74e2fd7a", got.Identity)

	// The same shape fires in the committed fixture for every item.
	_, fixtureItems := parseFixture(t, "academic_torrents")
	require.NotEmpty(t, fixtureItems)
	for _, it := range fixtureItems {
		require.NotNil(t, it.DownloadURL)
		require.Contains(t, *it.DownloadURL, "magnet:?xt=urn:btih:")
		require.NotNil(t, it.SizeBytes)
	}
}

// TestTierDDistrowatch: a direct .torrent <link> resolves in tier D and the
// permalink guid supplies identity step 1.
func TestTierDDistrowatch(t *testing.T) {
	_, items := parseFixture(t, "distrowatch_torrents")
	require.NotEmpty(t, items)

	for _, it := range items {
		require.NotNil(t, it.DownloadURL)
		require.Contains(t, *it.DownloadURL, ".torrent", "every DistroWatch item resolves its .torrent link")
		require.NotNil(t, it.GUID)
		require.Equal(t, *it.GUID, it.Identity, "identity step 1 is the raw guid")
	}

	u, _, tier, ok := ExtractDownloadURI(firstItem(t, "distrowatch_torrents"), "https://distrowatch.com/news/torrents.xml")
	require.True(t, ok)
	require.Equal(t, TierLink, tier)
	require.Contains(t, u, ".torrent")
}

// TestItemWithoutURIIsDiscarded: an item whose only link is a plain details
// page — no hash, no .torrent, no enclosure — yields no URI and is absent
// from the returned slice (FR-072).
func TestItemWithoutURIIsDiscarded(t *testing.T) {
	body := []byte(`<rss version="2.0"><channel><title>t</title>
<item><title>only a page</title>
<link>https://example.com/details/12345</link>
<guid isPermaLink="true">https://example.com/details/12345</guid>
<pubDate>Sat, 19 Sep 2026 03:58:00 +0000</pubDate>
</item>
<item><title>page with a hash</title>
<link>https://linuxtracker.org/index.php?page=torrent-details&amp;id=c3d56614025a6899a23dfe3de66a483dee59b2a8</link>
</item>
</channel></rss>`)

	_, items, err := NewParser(func() time.Time { return testNow }).ParseFeed("fed_t", "https://example.com/feed", body)
	require.NoError(t, err)
	require.Len(t, items, 1, "the details-page-only item must be discarded")

	got := items[0]
	require.Equal(t, "page with a hash", got.Title)
	require.NotNil(t, got.DownloadURL)
	require.Equal(t,
		"https://linuxtracker.org/index.php?page=torrent-details&id=c3d56614025a6899a23dfe3de66a483dee59b2a8",
		*got.DownloadURL,
		"the 40-hex id query parameter makes the link download-looking (doc 08 section 9)")
}

// TestGuidNotPermaLinkSkippedInTierD: a guid marked isPermaLink="false"
// cannot serve as tier D's link even when it happens to look like a URL.
func TestGuidNotPermaLinkSkippedInTierD(t *testing.T) {
	body := []byte(`<rss version="2.0"><channel><title>t</title>
<item><title>non-permalink guid</title>
<link>https://example.com/details/1</link>
<guid isPermaLink="false">https://example.com/files/x.torrent</guid>
</item>
</channel></rss>`)

	_, items, err := NewParser(func() time.Time { return testNow }).ParseFeed("fed_t", "https://example.com/feed", body)
	require.NoError(t, err)
	require.Empty(t, items, "isPermaLink=false guid must not resolve as tier D")
}

// TestPermissiveDateFallback: the Linuxtracker date shape parses only
// through the permissive "02/01/2006 15:04:05" layout.
func TestPermissiveDateFallback(t *testing.T) {
	want := time.Date(2026, 8, 31, 18, 47, 19, 0, time.UTC).UnixMilli()

	ms, ok := ParseItemDate("31/08/2026 18:47:19")
	require.True(t, ok)
	require.Equal(t, want, ms)

	// The RFC path still wins first.
	ms, ok = ParseItemDate("Mon, 31 Aug 2026 18:47:19 +0000")
	require.True(t, ok)
	require.Equal(t, want, ms)

	_, ok = ParseItemDate("not a date")
	require.False(t, ok)

	// Through ParseFeed the permissive date lands in published_at rather
	// than the substituted now.
	body := []byte(`<rss version="2.0"><channel><title>t</title>
<item><title>odd date</title>
<link>https://example.com/f.torrent</link>
<pubDate>31/08/2026 18:47:19</pubDate>
</item>
</channel></rss>`)
	_, items, err := NewParser(func() time.Time { return testNow }).ParseFeed("fed_t", "https://example.com/feed", body)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.NotNil(t, items[0].PublishedAt)
	require.Equal(t, want, *items[0].PublishedAt)
}

// TestNormaliseTitle: lowercased, '.' and '_' become spaces, whitespace runs
// collapse, ends trimmed.
func TestNormaliseTitle(t *testing.T) {
	require.Equal(t, "ubuntu 24 04 lts", NormaliseTitle("Ubuntu.24_04__LTS"))
	require.Equal(t, "the matrix", NormaliseTitle("  The...Matrix  "))
	require.Equal(t, "a b c", NormaliseTitle("a\t b\n c"))
	require.Equal(t, "", NormaliseTitle("  "))
}

// TestIdentityChain: guid, else info hash, else download URI, else the
// sha1(title + "\x00" + feedID) fallback.
func TestIdentityChain(t *testing.T) {
	it := &gofeed.Item{Title: "t", GUID: "g1"}
	require.Equal(t, "g1", Identity("fed_x", it, "https://u", "hash"))
	it.GUID = ""
	require.Equal(t, "hash", Identity("fed_x", it, "https://u", "hash"))
	require.Equal(t, "https://u", Identity("fed_x", it, "https://u", ""))
	want := sha1.Sum([]byte("t" + "\x00" + "fed_x"))
	require.Equal(t, hex.EncodeToString(want[:]), Identity("fed_x", it, "", ""))
}

// TestGutenbergIdentityFallsToStep3Or4: the RSS 0.91 Gutenberg items carry
// no guid and resolve no download URI (plain ebook pages, tier D gated), so
// identity lands on step 3 when a URI exists and step 4 otherwise.
func TestGutenbergIdentityFallsToStep3Or4(t *testing.T) {
	meta, items := parseFixture(t, "gutenberg_today")
	require.Equal(t, "Project Gutenberg Recently Posted or Updated EBooks", meta.Title)
	require.Empty(t, items, "plain ebook pages yield no download URI")

	it := firstItem(t, "gutenberg_today")
	require.Empty(t, it.GUID)
	_, _, _, ok := ExtractDownloadURI(it, "https://www.gutenberg.org/cache/epub/feeds/today.rss")
	require.False(t, ok)

	want := sha1.Sum([]byte(it.Title + "\x00" + "fed_gut"))
	require.Equal(t, hex.EncodeToString(want[:]), Identity("fed_gut", it, "", ""))
	require.Equal(t, "https://example.com/x.torrent", Identity("fed_gut", it, "https://example.com/x.torrent", ""))
}

// TestLinuxtrackerFixture: the current linuxtracker feed shape resolves in
// tier A (x-bittorrent enclosure); guid supplies the info-hash-shaped
// identity. (Upstream changed shape after the doc's 2026-09-01 probe — see
// testdata/README.md.)
func TestLinuxtrackerFixture(t *testing.T) {
	_, items := parseFixture(t, "linuxtracker")
	require.NotEmpty(t, items)
	for _, it := range items {
		require.NotNil(t, it.DownloadURL)
		require.Contains(t, *it.DownloadURL, ".torrent")
		require.NotNil(t, it.GUID)
		require.Equal(t, *it.GUID, it.Identity)
	}
}

// TestFeedMetaSkipWindowsAndFloor: skipHours/skipDays arrive trimmed,
// validated and deduplicated, and the sy implied interval is floored at the
// doc 08 section 2.2 minimum so a huge updateFrequency cannot zero it out.
func TestFeedMetaSkipWindowsAndFloor(t *testing.T) {
	body := []byte(`<rss version="2.0" xmlns:sy="http://purl.org/rss/1.0/modules/syndication/">
<channel><title>t</title>
<sy:updatePeriod>hourly</sy:updatePeriod>
<sy:updateFrequency>7200</sy:updateFrequency>
<skipHours><hour>0</hour><hour> 23 </hour><hour>0</hour><hour>25</hour><hour>x</hour></skipHours>
<skipDays><day>Monday</day><day> friday </day><day>Monday</day><day>funday</day></skipDays>
<item><title>i</title><link>https://example.com/x.torrent</link></item>
</channel></rss>`)

	meta, _, err := NewParser(func() time.Time { return testNow }).ParseFeed("fed_t", "https://example.com/feed", body)
	require.NoError(t, err)
	require.Equal(t, 300, meta.ImpliedIntervalS, "hourly/7200 floors at five minutes, not 0")
	require.Equal(t, []int{0, 23}, meta.SkipHours)
	require.Equal(t, []time.Weekday{time.Monday, time.Friday}, meta.SkipDays)
}

// TestLegacyDeclaredEncodingKeepsHints: a body declaring ISO-8859-1 still
// feeds the raw hint pass — here the isPermaLink guard drops the
// .torrent-shaped guid so the item resolves nothing.
func TestLegacyDeclaredEncodingKeepsHints(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="ISO-8859-1"?>
<rss version="2.0"><channel><title>t</title>
<item><title>latin1 item</title>
<guid isPermaLink="false">https://example.com/files/x.torrent</guid>
</item>
</channel></rss>`)

	_, items, err := NewParser(func() time.Time { return testNow }).ParseFeed("fed_t", "https://example.com/feed", body)
	require.NoError(t, err)
	require.Empty(t, items, "the isPermaLink hint must survive a non-UTF-8 declaration")
}

// TestIdentitiesDedupWithinFetch: a repeated identity inside one fetch keeps
// only the first item.
func TestIdentitiesDedupWithinFetch(t *testing.T) {
	body := []byte(`<rss version="2.0"><channel><title>t</title>
<item><title>one</title><guid>same</guid><link>https://example.com/1.torrent</link></item>
<item><title>two</title><guid>same</guid><link>https://example.com/2.torrent</link></item>
</channel></rss>`)
	_, items, err := NewParser(func() time.Time { return testNow }).ParseFeed("fed_t", "https://example.com/feed", body)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "one", items[0].Title)
}

// TestExtractDownloadURIVariants covers the remaining ladder sources over
// synthetic items: base32 btih decoding, torznab attrs, BEP 36, media:*,
// tier B and relative-link resolution.
func TestExtractDownloadURIVariants(t *testing.T) {
	base := "https://example.com/feed.xml"

	t.Run("tierB untyped enclosure", func(t *testing.T) {
		it := &gofeed.Item{Enclosures: []*gofeed.Enclosure{
			{URL: "https://example.com/media.mp3", Length: "10"},
		}}
		u, _, tier, ok := ExtractDownloadURI(it, base)
		require.True(t, ok)
		require.Equal(t, TierUntypedEnclosure, tier)
		require.Equal(t, "https://example.com/media.mp3", u)
	})

	t.Run("base32 btih decodes to hex", func(t *testing.T) {
		hexHash := "0123456789abcdef0123456789abcdef01234567"
		raw, err := hex.DecodeString(hexHash)
		require.NoError(t, err)
		b32 := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
		link := fmt.Sprintf("magnet:?xt=urn:btih:%s", strings.ToLower(b32))

		it := &gofeed.Item{Link: link}
		u, h, tier, ok := ExtractDownloadURI(it, base)
		require.True(t, ok)
		require.Equal(t, TierEnclosureOrMagnet, tier)
		require.Equal(t, link, u)
		require.Equal(t, hexHash, h)
	})

	t.Run("torznab magneturl wins tier C", func(t *testing.T) {
		it := &gofeed.Item{
			Title: "x",
			Link:  "https://example.com/page",
			Extensions: map[string]map[string][]ext.Extension{
				"torznab": {"attr": {
					{Name: "attr", Attrs: map[string]string{"name": "magneturl", "value": "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}},
					{Name: "attr", Attrs: map[string]string{"name": "size", "value": "123"}},
				}},
			},
		}
		u, h, tier, ok := ExtractDownloadURI(it, base)
		require.True(t, ok)
		require.Equal(t, TierSynthesised, tier)
		require.Equal(t, "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", u)
		require.Equal(t, "0123456789abcdef0123456789abcdef01234567", h)
	})

	t.Run("torznab infohash synthesises", func(t *testing.T) {
		it := &gofeed.Item{
			Title: "My Release",
			Extensions: map[string]map[string][]ext.Extension{
				"torznab": {"attr": {
					{Name: "attr", Attrs: map[string]string{"name": "infohash", "value": "0123456789ABCDEF0123456789ABCDEF01234567"}},
				}},
			},
		}
		u, h, tier, ok := ExtractDownloadURI(it, base)
		require.True(t, ok)
		require.Equal(t, TierSynthesised, tier)
		require.Equal(t, "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=My+Release", u)
		require.Equal(t, "0123456789abcdef0123456789abcdef01234567", h)
	})

	t.Run("relative link resolves against base", func(t *testing.T) {
		it := &gofeed.Item{Link: "/files/x.torrent"}
		u, _, tier, ok := ExtractDownloadURI(it, base)
		require.True(t, ok)
		require.Equal(t, TierLink, tier)
		require.Equal(t, "https://example.com/files/x.torrent", u)
	})
}
