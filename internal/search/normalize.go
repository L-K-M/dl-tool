package search

import (
	"net/url"
	"strings"
)

// SearchResult is the normalised row every search tier produces, per
// 07-search-and-indexers.md section 5. Fields declared as pointers are nil
// when the source does not report them — null in JSON — never -1 and never a
// fabricated 1. Producers set DownloadVolumeFactor and UploadVolumeFactor to
// 1.0 at construction; their zero value would mean freeleech, so a bare
// SearchResult{} literal is not a valid row.
//
// DownloadURL, MagnetURI and DetailsURL are acquisition handles: they can
// embed the operator's tracker passkey and must never be emitted by an API
// mapper (section 5 rule 6).
type SearchResult struct {
	ID          string `json:"id"`
	EngineID    string `json:"engine_id"`
	Title       string `json:"title"`
	DownloadURL string `json:"download_url,omitempty"`
	MagnetURI   string `json:"magnet_uri,omitempty"`
	Infohash    string `json:"infohash,omitempty"`

	SizeBytes   int64   `json:"size_bytes"`
	Seeders     *int    `json:"seeders"`
	Leechers    *int    `json:"leechers"`
	Grabs       *int    `json:"grabs"`
	PublishedAt *string `json:"published_at"` // RFC 3339

	DetailsURL   string `json:"details_url,omitempty"`
	CategoryIDs  []int  `json:"category_ids"`
	CategoryDesc string `json:"category_desc,omitempty"`

	DownloadVolumeFactor float64  `json:"download_volume_factor"` // default 1.0
	UploadVolumeFactor   float64  `json:"upload_volume_factor"`   // default 1.0
	MinimumRatio         *float64 `json:"minimum_ratio"`
	MinimumSeedTimeSecs  *int     `json:"minimum_seed_time_seconds"`

	IMDBID string `json:"imdb_id,omitempty"`
	TMDBID string `json:"tmdb_id,omitempty"`
	TVDBID string `json:"tvdb_id,omitempty"`
	Year   *int   `json:"year"`

	Genre     string `json:"genre,omitempty"`
	Language  string `json:"language,omitempty"`
	Publisher string `json:"publisher,omitempty"`
	Author    string `json:"author,omitempty"`
	Album     string `json:"album,omitempty"`
	Artist    string `json:"artist,omitempty"`

	// peers carries the Torznab peers attr (seeders + leechers) so Finalise can
	// derive Leechers when an engine reports peers but not leechers. It is
	// unexported: it is parser state, not a result field.
	peers *int
}

// Finalise applies rules 1, 2 and 5 of 07-search-and-indexers.md section 5:
// leechers = peers - seeders when leechers is unknown but both peers and
// seeders are reported, unknown numbers stay nil, and a row with no download
// URL, magnet or infohash is dropped. dropped is the count of rows removed,
// which the caller adds to that engine's error tally.
func Finalise(in []SearchResult) (out []SearchResult, dropped int) {
	out = make([]SearchResult, 0, len(in))
	for _, r := range in {
		if r.Leechers == nil && r.peers != nil && r.Seeders != nil {
			l := max(*r.peers-*r.Seeders, 0)
			r.Leechers = &l
		}
		if r.DownloadURL == "" && r.MagnetURI == "" && r.Infohash == "" {
			dropped++
			continue
		}
		out = append(out, r)
	}
	return out, dropped
}

// MagnetFromInfohash builds magnet:?xt=urn:btih:<infohash>&dn=<title> for an
// infohash-only result. The dn value is percent-encoded — QueryEscape plus a
// "+"/"%20" fixup — because form-encoding's '+' would render literally in
// BitTorrent clients, while PathEscape would leave '&' and '=' unescaped.
func MagnetFromInfohash(infohash, title string) string {
	return "magnet:?xt=urn:btih:" + infohash + "&dn=" + strings.ReplaceAll(url.QueryEscape(title), "+", "%20")
}
