package search

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/secure"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err, "read fixture %s", name)
	return data
}

// TestParseTorznabItem parses the private-tracker fixture: .torrent enclosure,
// peers without leechers, and the full torznab:attr set.
func TestParseTorznabItem(t *testing.T) {
	results, err := ParseFeed(readFixture(t, "torznab_hdaccess.xml"), "idx_hdaccess")
	require.NoError(t, err)
	results, dropped := Finalise(results)
	require.Equal(t, 0, dropped)
	require.Len(t, results, 5)

	r := results[0]
	assert.Equal(t, "idx_hdaccess", r.EngineID)
	assert.Equal(t, "Better Call Saul S01E05 Alpine Shepherd 1080p NF WEBRip DD5.1 x264", r.Title)
	assert.Equal(t, int64(2538463390), r.SizeBytes)
	require.NotNil(t, r.Seeders)
	assert.Equal(t, 7, *r.Seeders)
	// peers=7, seeders=7 and no leechers attr: leechers = peers - seeders = 0.
	require.NotNil(t, r.Leechers)
	assert.Equal(t, 0, *r.Leechers)
	assert.Equal(t, "63e07ff523710ca268567dad344ce1e0e6b7e8a3", r.Infohash)
	assert.Equal(t, "https://hdaccess.net/download.php?torrent=11515&passkey=123456", r.DownloadURL)
	assert.Empty(t, r.MagnetURI)
	assert.Equal(t, "https://hdaccess.net/details.php?id=11515&hit=1#comments", r.DetailsURL)
	assert.Equal(t, []int{5000, 5040, 100009, 100036}, r.CategoryIDs)
	assert.Equal(t, "HDTV 1080p", r.CategoryDesc)
	require.NotNil(t, r.PublishedAt)
	assert.Equal(t, "2015-03-14T17:10:42-04:00", *r.PublishedAt)
	require.NotNil(t, r.MinimumRatio)
	assert.InDelta(t, 1.0, *r.MinimumRatio, 1e-9)
	require.NotNil(t, r.MinimumSeedTimeSecs)
	assert.Equal(t, 172800, *r.MinimumSeedTimeSecs)
	assert.Equal(t, "3032476", r.IMDBID)
	assert.Equal(t, "273181", r.TVDBID)
	assert.InDelta(t, 1.0, r.DownloadVolumeFactor, 1e-9)
	assert.InDelta(t, 1.0, r.UploadVolumeFactor, 1e-9)
}

// TestParseMagnetEnclosure parses the public-tracker fixture: the magneturl
// attr wins over the magnet-typed enclosure and link.
func TestParseMagnetEnclosure(t *testing.T) {
	results, err := ParseFeed(readFixture(t, "torznab_tpb.xml"), "idx_tpb")
	require.NoError(t, err)
	require.Len(t, results, 5)

	r := results[0]
	wantMagnet := "magnet:?xt=urn:btih:9fb267cff5ae5603f07a347676ec3bf3e35f75e1&dn=Game+of+Thrones+S05E02+HDTV+x264-Xclusive+%5Beztv%5D&tr=udp:%2F%2Fopen.demonii.com:1337&tr=udp:%2F%2Ftracker.coppersurfer.tk:6969&tr=udp:%2F%2Ftracker.leechers-paradise.org:6969&tr=udp:%2F%2Fexodus.desync.com:6969"
	assert.Equal(t, wantMagnet, r.MagnetURI)
	assert.Empty(t, r.DownloadURL)
	assert.Equal(t, "9fb267cff5ae5603f07a347676ec3bf3e35f75e1", r.Infohash)
	assert.Equal(t, int64(388895872), r.SizeBytes)
	// seeders=34128, peers=36724: leechers = 36724 - 34128.
	results, dropped := Finalise(results)
	require.Equal(t, 0, dropped)
	require.NotNil(t, results[0].Leechers)
	assert.Equal(t, 2596, *results[0].Leechers)
	assert.Equal(t, "https://thepiratebay.se/torrent/11811366/Series_Title_S05E02_HDTV_x264-Xclusive_%5Beztv%5D", r.DetailsURL)

	// dn is percent-encoded: spaces as %20, not '+', and '&'/'+' escaped.
	assert.Equal(t,
		"magnet:?xt=urn:btih:abc&dn=A%20B%26C%2BD",
		MagnetFromInfohash("abc", "A B&C+D"))
}

// TestParseCapsTree checks the flattened category/subcat tree and the
// <limits max> default of 100 when the element is absent.
func TestParseCapsTree(t *testing.T) {
	caps, err := ParseCaps(readFixture(t, "torznab_caps.xml"))
	require.NoError(t, err)

	require.Len(t, caps.Categories, 3)
	tv := caps.Categories[0]
	assert.Equal(t, 5000, tv.ID)
	assert.Equal(t, "TV", tv.Name)
	require.Len(t, tv.Subcategories, 8)
	assert.Equal(t, Category{ID: 5070, Name: "Anime"}, tv.Subcategories[0])
	assert.Equal(t, 7000, caps.Categories[1].ID)
	assert.Equal(t, Category{ID: 8020, Name: "Comics"}, caps.Categories[2].Subcategories[0])

	// The fixture declares <limits max="60" default="25"/>.
	assert.Equal(t, 60, caps.LimitsMax)
	assert.Equal(t, 25, caps.LimitsDefault)

	// supportedParams is absent in the fixture: the newznab defaults apply to
	// search and tv-search; book-search is not declared at all.
	assert.Equal(t, []string{"q"}, caps.Modes["search"])
	assert.Equal(t, []string{"q", "rid", "season", "ep"}, caps.Modes["tv-search"])
	_, hasBook := caps.Modes["book-search"]
	assert.False(t, hasBook)

	noLimits, err := ParseCaps([]byte(`<caps><server title="x"/><searching><search available="yes"/></searching></caps>`))
	require.NoError(t, err)
	assert.Equal(t, 100, noLimits.LimitsMax)
}

// TestSeedersNullWhenAbsent parses a plain RSS 2.0 feed with no torznab
// attrs: Seeders and Leechers stay nil pointers, not -1 and not 1.
func TestSeedersNullWhenAbsent(t *testing.T) {
	results, err := ParseFeed(readFixture(t, "academic_torrents_rss.xml"), "idx_academic")
	require.NoError(t, err)
	results, dropped := Finalise(results)
	require.Equal(t, 0, dropped)
	require.NotEmpty(t, results)

	for _, r := range results {
		assert.Nil(t, r.Seeders, "row %q must report null, not a sentinel", r.Title)
		assert.Nil(t, r.Leechers, "row %q must report null, not a sentinel", r.Title)
	}
	first := results[0]
	assert.Equal(t, "f447784b18231897c892dbe45d943924583eab64", first.Infohash)
	assert.Equal(t, int64(6169821184), first.SizeBytes)
	assert.Equal(t, "Course", first.CategoryDesc)
	assert.Nil(t, first.PublishedAt)
}

// TestTorznabErrorDocument covers both error transports: a conforming HTTP
// 200 error body, and Prowlarr's real-status deviation with Retry-After.
func TestTorznabErrorDocument(t *testing.T) {
	_, err := ParseFeed(readFixture(t, "torznab_error.xml"), "idx_test")
	require.Error(t, err)
	var te *TorznabError
	require.True(t, errors.As(err, &te), "error type %T, want *TorznabError", err)
	assert.Equal(t, 100, te.Code)
	assert.Equal(t, "Incorrect user credentials", te.Description)

	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`<error code="429" description="Indexer user limit reached"/>`))
	}))
	defer srv.Close()

	client, err := NewTorznabClient(srv.Client(), srv.URL+"/api", secure.Secret("k3y"), "idx_test", "dl-tool/test")
	require.NoError(t, err)
	_, err = client.Search(context.Background(), Query{T: "search", Q: "x", Categories: []int{5000, 5040}, Limit: 50, NoCache: true})
	require.Error(t, err)
	require.True(t, errors.As(err, &te), "error type %T, want *TorznabError", err)
	assert.Equal(t, 429, te.Code)
	assert.Equal(t, http.StatusTooManyRequests, te.HTTPStatus)
	assert.Equal(t, 30*time.Second, te.RetryAfter)

	// The request carried t, apikey, the joined cat list and Jackett's
	// cache=false bypass.
	assert.Equal(t, "search", gotQuery.Get("t"))
	assert.Equal(t, "k3y", gotQuery.Get("apikey"))
	assert.Equal(t, "5000,5040", gotQuery.Get("cat"))
	assert.Equal(t, "50", gotQuery.Get("limit"))
	assert.Equal(t, "false", gotQuery.Get("cache"))
}

// TestSearchClampsLimitToCaps checks that a cached caps <limits max> clamps
// Query.Limit on subsequent searches, and that a pre-Caps search sends the
// requested value unclamped.
func TestSearchClampsLimitToCaps(t *testing.T) {
	var gotLimit []string
	capsFetches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLimit = append(gotLimit, r.URL.Query().Get("limit"))
		if r.URL.Query().Get("t") == "caps" {
			capsFetches++
			_, _ = w.Write([]byte(`<caps><limits max="60"/></caps>`))
			return
		}
		_, _ = w.Write([]byte(`<rss version="2.0"><channel></channel></rss>`))
	}))
	defer srv.Close()

	c, err := NewTorznabClient(srv.Client(), srv.URL+"/api", secure.Secret("k"), "idx_test", "dl-tool/test")
	require.NoError(t, err)

	_, err = c.Search(context.Background(), Query{Q: "x", Limit: 500})
	require.NoError(t, err)
	assert.Equal(t, "500", gotLimit[len(gotLimit)-1], "no caps fetched yet: limit unclamped")

	_, err = c.Caps(context.Background())
	require.NoError(t, err)

	_, err = c.Search(context.Background(), Query{Q: "x", Limit: 500})
	require.NoError(t, err)
	assert.Equal(t, "60", gotLimit[len(gotLimit)-1], "limit clamps to caps <limits max>")
	assert.Equal(t, 1, capsFetches, "second search must reuse the cached caps document")
}

// TestParseRetryAfter pins the header parsing: zero and negative seconds are
// 0, an out-of-range value saturates instead of wrapping, and an HTTP date
// is accepted.
func TestParseRetryAfter(t *testing.T) {
	assert.Equal(t, time.Duration(0), parseRetryAfter(""))
	assert.Equal(t, time.Duration(0), parseRetryAfter("0"))
	assert.Equal(t, time.Duration(0), parseRetryAfter("-5"))
	assert.Equal(t, 30*time.Second, parseRetryAfter("30"))
	assert.Equal(t, time.Duration(math.MaxInt64), parseRetryAfter("10000000000"))
	assert.Equal(t, time.Duration(0), parseRetryAfter("not-a-number"))
	future := time.Now().UTC().Add(time.Hour).Format(http.TimeFormat)
	assert.Greater(t, parseRetryAfter(future), 30*time.Minute)
}

// TestFinaliseDropsUnusableRow covers section 5 rule 5: a row with no
// download URL, no magnet and no infohash is dropped and counted.
func TestFinaliseDropsUnusableRow(t *testing.T) {
	in := []SearchResult{
		{Title: "no acquisition handle"},
		{Title: "infohash only", Infohash: "9fb267cff5ae5603f07a347676ec3bf3e35f75e1"},
		{Title: "download only", DownloadURL: "https://example.org/t.torrent"},
	}
	out, dropped := Finalise(in)
	require.Equal(t, 1, dropped)
	require.Len(t, out, 2)
	assert.Equal(t, "infohash only", out[0].Title)
	assert.Equal(t, "download only", out[1].Title)
}
