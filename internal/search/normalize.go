package search

import (
	"net/url"
	"strconv"
	"strings"
	"unicode"
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

// NormaliseTitle lower-cases s, collapses every run of whitespace, dots and
// underscores into a single space and trims. It exists for dedup keys only,
// never for display (07-search-and-indexers.md section 5).
func NormaliseTitle(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pending := false
	for _, r := range strings.ToLower(s) {
		if r == '.' || r == '_' || unicode.IsSpace(r) {
			pending = b.Len() > 0
			continue
		}
		if pending {
			b.WriteByte(' ')
			pending = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Dedup collapses results that several engines returned: by Infohash when
// present, otherwise by (NormaliseTitle(Title), SizeBytes). The surviving row
// is the one with the highest non-nil Seeders; ties keep the first, so the
// caller's ordering decides. 05-api-contract.md section 9.2 fixes the result
// object, so the collapsed engines are not named in the response; their rows
// stay in search_results.
func Dedup(in []SearchResult) []SearchResult {
	keyOf := make([]string, len(in))
	best := make(map[string]int, len(in))
	for i, r := range in {
		k := dedupKey(r)
		keyOf[i] = k
		if j, ok := best[k]; !ok || seedersWin(r.Seeders, in[j].Seeders) {
			best[k] = i
		}
	}
	out := make([]SearchResult, 0, len(best))
	for i, r := range in {
		if best[keyOf[i]] == i {
			out = append(out, r)
		}
	}
	return out
}

// dedupKey is the collapse identity of doc 07 section 5: the infohash when the
// engine reported one — case-folded so two engines spelling the same swarm
// differently still merge — else the normalised title paired with the byte
// size.
func dedupKey(r SearchResult) string {
	if r.Infohash != "" {
		return "h\x00" + strings.ToLower(r.Infohash)
	}
	return "t\x00" + NormaliseTitle(r.Title) + "\x00" + strconv.FormatInt(r.SizeBytes, 10)
}

// seedersWin reports whether candidate outranks the incumbent: a real count
// always beats an unknown one, and a higher count beats a lower one. Ties —
// including two unknowns — keep the incumbent, so input order decides.
func seedersWin(candidate, incumbent *int) bool {
	if candidate == nil {
		return false
	}
	if incumbent == nil {
		return true
	}
	return *candidate > *incumbent
}

// MagnetFromInfohash builds magnet:?xt=urn:btih:<infohash>&dn=<title> for an
// infohash-only result. The dn value is percent-encoded — QueryEscape plus a
// "+"/"%20" fixup — because form-encoding's '+' would render literally in
// BitTorrent clients, while PathEscape would leave '&' and '=' unescaped.
func MagnetFromInfohash(infohash, title string) string {
	return "magnet:?xt=urn:btih:" + infohash + "&dn=" + strings.ReplaceAll(url.QueryEscape(title), "+", "%20")
}
