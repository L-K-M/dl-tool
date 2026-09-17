package search

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/L-K-M/dl-tool/internal/secure"
)

// defaultLimitsMax is the caps <limits max> fallback of
// 07-search-and-indexers.md section 2.2: 100 when the element or attribute
// is absent.
const defaultLimitsMax = 100

// Category is one node of the caps category tree. Subcategories holds the
// flattened <subcat> children of a top-level <category>.
type Category struct {
	ID            int        `json:"id"`
	Name          string     `json:"name"`
	Subcategories []Category `json:"subcategories,omitempty"`
}

// Caps is the parsed t=caps document of 07-search-and-indexers.md section 2.2.
type Caps struct {
	ServerTitle   string
	LimitsMax     int // <limits max>, 100 when absent
	LimitsDefault int
	// Modes maps "search", "tv-search", "movie-search", "audio-search" or
	// "book-search" to its supportedParams. A mode with available="no" or one
	// the caps document does not declare is absent from the map.
	Modes      map[string][]string
	Categories []Category
}

// Query is one Torznab request. T is caps, search, tvsearch, movie, music or
// book.
type Query struct {
	T          string
	Q          string
	Categories []int
	Limit      int
	Offset     int
	Season     string
	Ep         string
	IMDBID     string
	NoCache    bool // adds &cache=false, for Jackett
}

// TorznabClient fetches t=caps and t=search from a Torznab or Newznab base
// URL. It holds the guarded client, never one of its own: hc always comes
// from secure.NewClient (T123), and every response body is read with
// secure.ReadCapped.
type TorznabClient struct {
	hc        *http.Client
	base      string
	apiKey    secure.Secret
	engineID  string
	userAgent string

	// limitsMax caches the caps <limits max> after a successful Caps call so
	// Search can clamp Query.Limit. Zero means caps has not been fetched yet.
	limitsMax atomic.Int64
}

// NewTorznabClient validates base (one of the provider URL shapes of
// 07-search-and-indexers.md section 2.6: an http or https URL) and stores the
// client it is given — it never builds one.
func NewTorznabClient(hc *http.Client, base string, apiKey secure.Secret, engineID, userAgent string) (*TorznabClient, error) {
	if hc == nil {
		return nil, fmt.Errorf("search: torznab client requires an *http.Client from secure.NewClient")
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("search: invalid torznab base url %q", secure.RedactURL(base))
	}
	return &TorznabClient{
		hc:        hc,
		base:      base,
		apiKey:    apiKey,
		engineID:  engineID,
		userAgent: userAgent,
	}, nil
}

// Caps fetches and parses the t=caps document and caches its <limits max> for
// later Search calls.
func (c *TorznabClient) Caps(ctx context.Context) (Caps, error) {
	body, err := c.fetch(ctx, Query{T: "caps"})
	if err != nil {
		return Caps{}, err
	}
	caps, err := ParseCaps(body)
	if err != nil {
		return Caps{}, err
	}
	c.limitsMax.Store(int64(caps.LimitsMax))
	return caps, nil
}

// Search runs one torznab query and returns the normalised rows. Rows with no
// download URL, magnet or infohash are dropped and logged against the engine's
// error tally (07-search-and-indexers.md section 5 rule 5).
func (c *TorznabClient) Search(ctx context.Context, q Query) ([]SearchResult, error) {
	if q.T == "" {
		q.T = "search"
	}
	body, err := c.fetch(ctx, q)
	if err != nil {
		return nil, err
	}
	results, err := ParseFeed(body, c.engineID)
	if err != nil {
		return nil, err
	}
	out, dropped := Finalise(results)
	if dropped > 0 {
		slog.Warn("torznab rows dropped: no download url, magnet or infohash",
			"engine_id", c.engineID, "dropped", dropped)
	}
	return out, nil
}

// buildURL renders the query string for q. It always sends t and apikey,
// joins Categories into cat, clamps Limit to the cached caps <limits max>
// when Caps has run, and adds cache=false for Jackett when NoCache is set.
func (c *TorznabClient) buildURL(q Query) string {
	v := url.Values{}
	v.Set("t", q.T)
	v.Set("apikey", c.apiKey.Reveal())
	if q.Q != "" {
		v.Set("q", q.Q)
	}
	if len(q.Categories) > 0 {
		parts := make([]string, len(q.Categories))
		for i, n := range q.Categories {
			parts[i] = strconv.Itoa(n)
		}
		v.Set("cat", strings.Join(parts, ","))
	}
	if q.Limit > 0 {
		limit := q.Limit
		if max := c.limitsMax.Load(); max > 0 && int64(limit) > max {
			limit = int(max)
		}
		v.Set("limit", strconv.Itoa(limit))
	}
	if q.Offset > 0 {
		v.Set("offset", strconv.Itoa(q.Offset))
	}
	if q.Season != "" {
		v.Set("season", q.Season)
	}
	if q.Ep != "" {
		v.Set("ep", q.Ep)
	}
	if q.IMDBID != "" {
		v.Set("imdbid", q.IMDBID)
	}
	if q.NoCache {
		v.Set("cache", "false")
	}
	sep := "?"
	if strings.Contains(c.base, "?") {
		sep = "&"
	}
	return c.base + sep + v.Encode()
}

// fetch issues the GET, enforces the 8 MiB metadata cap and maps an error
// document — conforming HTTP 200 or Prowlarr 400/410/429 — to *TorznabError.
func (c *TorznabClient) fetch(ctx context.Context, q Query) ([]byte, error) {
	u := c.buildURL(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		// url parse errors embed the raw URL, apikey included.
		return nil, fmt.Errorf("search: build torznab request: %w", secure.RedactError(err))
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// The error chain renders the request URL, apikey included.
		return nil, fmt.Errorf("search: torznab fetch: %w", secure.RedactError(err))
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("search: close torznab response body", "error", err)
		}
	}()
	body, err := secure.ReadCapped(resp, secure.MetadataFetchCap)
	if err != nil {
		return nil, err
	}
	if te := parseErrorDoc(body); te != nil {
		te.HTTPStatus = resp.StatusCode
		if resp.StatusCode == http.StatusTooManyRequests {
			te.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
		}
		return nil, te
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("search: torznab http %d from %s", resp.StatusCode, secure.RedactURL(u))
	}
	return body, nil
}

// TorznabError is <error code="200" description="Missing parameter (t)"/>.
// Conforming servers return it with HTTP 200; Prowlarr returns it with 400,
// 410 or 429, so HTTPStatus carries the real status.
type TorznabError struct {
	Code        int
	Description string
	RetryAfter  time.Duration // from the Retry-After header on 429, zero otherwise
	HTTPStatus  int
}

func (e *TorznabError) Error() string {
	if e.HTTPStatus >= http.StatusBadRequest {
		return fmt.Sprintf("torznab error %d (http %d): %s", e.Code, e.HTTPStatus, e.Description)
	}
	return fmt.Sprintf("torznab error %d: %s", e.Code, e.Description)
}

// capsXML mirrors the section 2.2 document: only the fields dl-tool uses.
type capsXML struct {
	XMLName xml.Name `xml:"caps"`
	Server  struct {
		Title string `xml:"title,attr"`
	} `xml:"server"`
	Limits struct {
		Max     int `xml:"max,attr"`
		Default int `xml:"default,attr"`
	} `xml:"limits"`
	Searching struct {
		Search      searchModeXML `xml:"search"`
		TVSearch    searchModeXML `xml:"tv-search"`
		MovieSearch searchModeXML `xml:"movie-search"`
		AudioSearch searchModeXML `xml:"audio-search"`
		BookSearch  searchModeXML `xml:"book-search"`
	} `xml:"searching"`
	Categories struct {
		List []categoryXML `xml:"category"`
	} `xml:"categories"`
}

type searchModeXML struct {
	Available       string `xml:"available,attr"`
	SupportedParams string `xml:"supportedParams,attr"`
}

type categoryXML struct {
	ID      int           `xml:"id,attr"`
	Name    string        `xml:"name,attr"`
	Subcats []categoryXML `xml:"subcat"`
}

// ParseCaps parses a t=caps document. It is pure; it never touches the
// network. available="no" modes and modes the document does not declare are
// left out of Modes; supportedParams falls back to the newznab defaults of
// 07-search-and-indexers.md section 2.2 when the attribute is absent — that
// document names defaults for search and tv-search only, so the other modes
// report no params rather than an invented set. LimitsMax defaults to 100.
func ParseCaps(doc []byte) (Caps, error) {
	var c capsXML
	if err := xml.Unmarshal(doc, &c); err != nil {
		return Caps{}, fmt.Errorf("search: parse torznab caps: %w", err)
	}
	out := Caps{
		ServerTitle:   c.Server.Title,
		LimitsMax:     c.Limits.Max,
		LimitsDefault: c.Limits.Default,
		Modes:         make(map[string][]string),
	}
	if out.LimitsMax <= 0 {
		out.LimitsMax = defaultLimitsMax
	}
	addMode := func(name string, m searchModeXML, fallback []string) {
		if !strings.EqualFold(m.Available, "yes") {
			return
		}
		params := fallback
		if m.SupportedParams != "" {
			params = splitParams(m.SupportedParams)
		}
		out.Modes[name] = params
	}
	addMode("search", c.Searching.Search, []string{"q"})
	addMode("tv-search", c.Searching.TVSearch, []string{"q", "rid", "season", "ep"})
	addMode("movie-search", c.Searching.MovieSearch, nil)
	addMode("audio-search", c.Searching.AudioSearch, nil)
	addMode("book-search", c.Searching.BookSearch, nil)
	for _, cat := range c.Categories.List {
		out.Categories = append(out.Categories, convertCategory(cat))
	}
	return out, nil
}

func splitParams(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func convertCategory(c categoryXML) Category {
	out := Category{ID: c.ID, Name: c.Name}
	for _, sub := range c.Subcats {
		out.Subcategories = append(out.Subcategories, convertCategory(sub))
	}
	return out
}

// feedXML is the RSS shape both newznab and torznab feeds share. Attr
// elements match on local name, so torznab:attr and newznab:attr both land in
// Attrs. The rss version attribute is ignored — Jackett emits 2.0, Prowlarr
// 1.0, and it carries no meaning for dl-tool.
type feedXML struct {
	Channel struct {
		Items []itemXML `xml:"item"`
	} `xml:"channel"`
}

type itemXML struct {
	Title      string   `xml:"title"`
	Link       string   `xml:"link"`
	Comments   string   `xml:"comments"`
	PubDate    string   `xml:"pubDate"`
	Categories []string `xml:"category"`
	Size       string   `xml:"size"`
	Infohash   string   `xml:"infohash"` // academictorrents plain element
	GUID       struct {
		Value string `xml:",chardata"`
	} `xml:"guid"`
	Enclosures []struct {
		URL  string `xml:"url,attr"`
		Type string `xml:"type,attr"`
	} `xml:"enclosure"`
	Attrs []struct {
		Name  string `xml:"name,attr"`
		Value string `xml:"value,attr"`
	} `xml:"attr"`
}

// ParseFeed parses a t=search document into normalised rows. It is pure; it
// never touches the network. Every row of the section 2.4 parser-rule table
// is applied in itemToResult. A Torznab error document is detected before
// feed parsing and returned as *TorznabError.
func ParseFeed(doc []byte, engineID string) ([]SearchResult, error) {
	if te := parseErrorDoc(doc); te != nil {
		return nil, te
	}
	var f feedXML
	if err := xml.Unmarshal(doc, &f); err != nil {
		return nil, fmt.Errorf("search: parse torznab feed: %w", err)
	}
	out := make([]SearchResult, 0, len(f.Channel.Items))
	for _, it := range f.Channel.Items {
		out = append(out, itemToResult(it, engineID))
	}
	return out, nil
}

func itemToResult(it itemXML, engineID string) SearchResult {
	r := SearchResult{
		EngineID:             engineID,
		Title:                strings.TrimSpace(it.Title),
		SizeBytes:            parseInt64(strings.TrimSpace(it.Size)),
		Infohash:             strings.TrimSpace(it.Infohash),
		CategoryDesc:         firstNonEmpty(it.Categories),
		DownloadVolumeFactor: 1.0,
		UploadVolumeFactor:   1.0,
	}
	var magnetURL string
	for _, a := range it.Attrs {
		v := strings.TrimSpace(a.Value)
		switch strings.ToLower(strings.TrimSpace(a.Name)) {
		case "size":
			// The attr is authoritative; <size> is the convenience copy.
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				r.SizeBytes = n
			}
		case "category":
			if n, err := strconv.Atoi(v); err == nil {
				r.CategoryIDs = append(r.CategoryIDs, n)
			}
		case "seeders":
			r.Seeders = parseIntPtr(v)
		case "leechers":
			r.Leechers = parseIntPtr(v)
		case "peers":
			r.peers = parseIntPtr(v)
		case "grabs":
			r.Grabs = parseIntPtr(v)
		case "infohash":
			r.Infohash = v
		case "magneturl":
			magnetURL = v
		case "minimumratio":
			r.MinimumRatio = parseFloatPtr(v)
		case "minimumseedtime":
			r.MinimumSeedTimeSecs = parseIntPtr(v)
		case "downloadvolumefactor":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				r.DownloadVolumeFactor = f
			}
		case "uploadvolumefactor":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				r.UploadVolumeFactor = f
			}
		case "imdb":
			r.IMDBID = v
		case "tmdbid":
			r.TMDBID = v
		case "tvdbid":
			r.TVDBID = v
		case "year":
			r.Year = parseIntPtr(v)
		case "genre":
			r.Genre = v
		case "language":
			r.Language = v
		case "publisher":
			r.Publisher = v
		case "author":
			r.Author = v
		case "album":
			r.Album = v
		case "artist":
			r.Artist = v
		}
	}

	// Download target: prefer magneturl, else <enclosure url>, else <link>.
	switch u := firstNonEmpty([]string{magnetURL, enclosureURL(it), it.Link}); {
	case strings.HasPrefix(u, "magnet:"):
		r.MagnetURI = u
	default:
		r.DownloadURL = u
	}

	// Details page: <comments> when present, else an absolute <guid>.
	if c := strings.TrimSpace(it.Comments); c != "" {
		r.DetailsURL = c
	} else if g := strings.TrimSpace(it.GUID.Value); isAbsoluteURL(g) {
		r.DetailsURL = g
	}

	if p := parsePubDate(it.PubDate); p != "" {
		r.PublishedAt = &p
	}
	return r
}

// enclosureURL picks the download enclosure: a magnet-typed or magnet:-URL
// enclosure first, then an application/x-bittorrent one, then the first
// enclosure with a URL at all.
func enclosureURL(it itemXML) string {
	best := ""
	for _, e := range it.Enclosures {
		u := strings.TrimSpace(e.URL)
		if u == "" {
			continue
		}
		if strings.Contains(e.Type, "x-scheme-handler/magnet") || strings.HasPrefix(u, "magnet:") {
			return u
		}
		if strings.Contains(e.Type, "x-bittorrent") {
			best = u
			continue
		}
		if best == "" {
			best = u
		}
	}
	return best
}

func firstNonEmpty(ss []string) string {
	for _, s := range ss {
		if t := strings.TrimSpace(s); t != "" {
			return t
		}
	}
	return ""
}

func isAbsoluteURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.IsAbs()
}

// pubDateLayouts covers the section 2.4 layout first, then the same-date
// variants real feeds emit (named zone, two-digit year).
var pubDateLayouts = []string{
	"Mon, 02 Jan 2006 15:04:05 -0700",
	time.RFC1123,
	time.RFC822Z,
	time.RFC822,
}

// parsePubDate renders pubDate as RFC 3339, or "" when it is absent or does
// not parse.
func parsePubDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, layout := range pubDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format(time.RFC3339)
		}
	}
	return ""
}

func parseInt64(s string) int64 {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	return 0
}

func parseIntPtr(s string) *int {
	if n, err := strconv.Atoi(s); err == nil {
		return &n
	}
	return nil
}

func parseFloatPtr(s string) *float64 {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return &f
	}
	return nil
}

type errorDocXML struct {
	XMLName     xml.Name `xml:"error"`
	Code        int      `xml:"code,attr"`
	Description string   `xml:"description,attr"`
}

// parseErrorDoc returns the Torznab error document a body carries, or nil.
// The XMLName tag makes the unmarshal fail on any other root element.
func parseErrorDoc(body []byte) *TorznabError {
	var e errorDocXML
	if err := xml.Unmarshal(body, &e); err != nil {
		return nil
	}
	return &TorznabError{Code: e.Code, Description: e.Description}
}

// parseRetryAfter reads the Retry-After header Prowlarr sends with a 429:
// integer seconds, or an HTTP date as a fallback. Unparseable or past values
// are zero.
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(h, 10, 64); err == nil {
		switch {
		case secs <= 0:
			return 0
		case secs > math.MaxInt64/int64(time.Second):
			// Saturate rather than wrap: a wrapped value could come out
			// near zero and report "retry immediately".
			return time.Duration(math.MaxInt64)
		default:
			return time.Duration(secs) * time.Second
		}
	}
	if t, err := http.ParseTime(h); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}

// DefaultCategories is the newznab tree of 07-search-and-indexers.md
// section 2.3, roots 1000..8000 with their documented subcategories. It is
// the fallback for GET /indexers/categories when no indexer has cached caps.
// Ids 9000-99999 are reserved and never assigned; ids >= 100000 are
// engine-scoped and arrive through caps, not this table.
func DefaultCategories() []Category {
	return []Category{
		{ID: 1000, Name: "Console", Subcategories: []Category{
			{ID: 1010, Name: "NDS"},
			{ID: 1020, Name: "PSP"},
			{ID: 1030, Name: "Wii"},
			{ID: 1040, Name: "XBox"},
			{ID: 1050, Name: "XBox 360"},
			{ID: 1060, Name: "Wiiware"},
			{ID: 1070, Name: "XBox 360 DLC"},
		}},
		{ID: 2000, Name: "Movies", Subcategories: []Category{
			{ID: 2010, Name: "Foreign"},
			{ID: 2020, Name: "Other"},
			{ID: 2030, Name: "SD"},
			{ID: 2040, Name: "HD"},
			{ID: 2045, Name: "UHD"},
			{ID: 2050, Name: "BluRay"},
			{ID: 2060, Name: "3D"},
		}},
		{ID: 3000, Name: "Audio", Subcategories: []Category{
			{ID: 3010, Name: "MP3"},
			{ID: 3020, Name: "Video"},
			{ID: 3030, Name: "Audiobook"},
			{ID: 3040, Name: "Lossless"},
		}},
		{ID: 4000, Name: "PC", Subcategories: []Category{
			{ID: 4010, Name: "0day"},
			{ID: 4020, Name: "ISO"},
			{ID: 4030, Name: "Mac"},
			{ID: 4040, Name: "Mobile-Other"},
			{ID: 4050, Name: "Games"},
			{ID: 4060, Name: "Mobile-iOS"},
			{ID: 4070, Name: "Mobile-Android"},
		}},
		{ID: 5000, Name: "TV", Subcategories: []Category{
			{ID: 5020, Name: "Foreign"},
			{ID: 5030, Name: "SD"},
			{ID: 5040, Name: "HD"},
			{ID: 5045, Name: "UHD"},
			{ID: 5050, Name: "Other"},
			{ID: 5060, Name: "Sport"},
			{ID: 5070, Name: "Anime"},
			{ID: 5080, Name: "Documentary"},
		}},
		{ID: 6000, Name: "XXX", Subcategories: []Category{
			{ID: 6010, Name: "DVD"},
			{ID: 6020, Name: "WMV"},
			{ID: 6030, Name: "XviD"},
			{ID: 6040, Name: "x264"},
			{ID: 6050, Name: "Pack"},
			{ID: 6060, Name: "ImgSet"},
			{ID: 6070, Name: "Other"},
		}},
		{ID: 7000, Name: "Books", Subcategories: []Category{
			{ID: 7010, Name: "Mags"},
			{ID: 7020, Name: "EBook"},
			{ID: 7030, Name: "Comics"},
		}},
		{ID: 8000, Name: "Other", Subcategories: []Category{
			{ID: 8010, Name: "Misc"},
		}},
	}
}

// FlattenCategories renders a caps category tree as the flat id-ordered list
// stored in indexers.categories_json: every root and every subcat becomes one
// entry, parents ahead of children, so a stored document never depends on the
// nesting of one provider's tree.
func FlattenCategories(roots []Category) []Category {
	var flat []Category
	var walk func(c Category)
	walk = func(c Category) {
		flat = append(flat, Category{ID: c.ID, Name: c.Name})
		for _, sub := range c.Subcategories {
			walk(sub)
		}
	}
	for _, c := range roots {
		walk(c)
	}
	return flat
}

// ProviderIndexer is one upstream indexer discovered on a Jackett or Prowlarr
// instance (07-search-and-indexers.md section 2.7).
type ProviderIndexer struct {
	RemoteID string // Jackett indexer id, or the Prowlarr integer id as text
	Name     string
	BaseURL  string // fully built per-indexer Torznab base URL
}

// jackettPathMarker is the path segment that distinguishes a Jackett instance
// URL from a Prowlarr one (07-search-and-indexers.md section 2.6).
const jackettPathMarker = "/api/v2.0/indexers/"

// EnumerateProvider detects the provider from the submitted URL's path and
// issues the enumeration request of 07-search-and-indexers.md section 2.7,
// returning one entry per configured indexer. The path marker selects
// Jackett; anything else is asked the Prowlarr question. hc is the guarded
// client the caller scoped for this origin — Prowlarr id 0 is skipped: it is
// a synthetic self-test indexer.
func EnumerateProvider(ctx context.Context, hc *http.Client, host string, apiKey secure.Secret) ([]ProviderIndexer, error) {
	if hc == nil {
		return nil, fmt.Errorf("search: enumerate provider requires an *http.Client from secure.NewClient")
	}
	u, err := url.Parse(host)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("search: invalid provider url %q", secure.RedactURL(host))
	}
	origin := u.Scheme + "://" + u.Host

	if strings.Contains(u.Path, jackettPathMarker) {
		return enumerateJackett(ctx, hc, origin, apiKey)
	}
	return enumerateProwlarr(ctx, hc, origin, apiKey)
}

// providerGet issues one enumeration GET, enforces the metadata cap and the
// redirect hop limit the client carries, and maps a torznab error document or
// a non-2xx status to an error. prowlarr selects the X-Api-Key header over
// the apikey query parameter.
func providerGet(ctx context.Context, hc *http.Client, rawURL string, apiKey secure.Secret, prowlarr bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("search: build provider request: %w", secure.RedactError(err))
	}
	if prowlarr {
		req.Header.Set("X-Api-Key", apiKey.Reveal())
	}
	resp, err := hc.Do(req)
	if err != nil {
		// The error chain renders the request URL, apikey included.
		return nil, fmt.Errorf("search: provider fetch: %w", secure.RedactError(err))
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("search: close provider response body", "error", err)
		}
	}()
	body, err := secure.ReadCapped(resp, secure.MetadataFetchCap)
	if err != nil {
		return nil, err
	}
	if te := parseErrorDoc(body); te != nil {
		te.HTTPStatus = resp.StatusCode
		return nil, te
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("search: provider http %d from %s", resp.StatusCode, secure.RedactURL(rawURL))
	}
	return body, nil
}

// jackettIndexersXML is the t=indexers document of section 2.7: one
// <indexer> child per configured indexer.
type jackettIndexersXML struct {
	XMLName  xml.Name `xml:"indexers"`
	Indexers []struct {
		ID         string `xml:"id,attr"`
		Configured string `xml:"configured,attr"`
		Title      string `xml:"title"`
	} `xml:"indexer"`
}

// enumerateJackett asks the aggregate endpoint for t=indexers&configured=true
// and builds each per-indexer base URL of section 2.6.
func enumerateJackett(ctx context.Context, hc *http.Client, origin string, apiKey secure.Secret) ([]ProviderIndexer, error) {
	v := url.Values{}
	v.Set("apikey", apiKey.Reveal())
	v.Set("t", "indexers")
	v.Set("configured", "true")
	body, err := providerGet(ctx, hc, origin+jackettPathMarker+"all/results/torznab/api?"+v.Encode(), apiKey, false)
	if err != nil {
		return nil, err
	}

	var doc jackettIndexersXML
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("search: parse jackett indexers: %w", err)
	}

	out := make([]ProviderIndexer, 0, len(doc.Indexers))
	for _, ix := range doc.Indexers {
		if ix.ID == "" || (ix.Configured != "" && ix.Configured != "true") {
			continue
		}
		name := strings.TrimSpace(ix.Title)
		if name == "" {
			name = ix.ID
		}
		out = append(out, ProviderIndexer{
			RemoteID: ix.ID,
			Name:     name,
			BaseURL:  origin + jackettPathMarker + url.PathEscape(ix.ID) + "/results/torznab/api",
		})
	}
	return out, nil
}

// prowlarrIndexerJSON is the one field pair of the /api/v1/indexer document
// the enumeration needs; every other member is ignored.
type prowlarrIndexerJSON struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// enumerateProwlarr asks {origin}/api/v1/indexer with the X-Api-Key header
// and builds each base URL as {origin}/<id>/api. Id 0 is the synthetic
// self-test indexer and never becomes a row.
func enumerateProwlarr(ctx context.Context, hc *http.Client, origin string, apiKey secure.Secret) ([]ProviderIndexer, error) {
	body, err := providerGet(ctx, hc, origin+"/api/v1/indexer", apiKey, true)
	if err != nil {
		return nil, err
	}

	var list []prowlarrIndexerJSON
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("search: parse prowlarr indexers: %w", err)
	}

	out := make([]ProviderIndexer, 0, len(list))
	for _, ix := range list {
		if ix.ID == 0 {
			continue
		}
		name := strings.TrimSpace(ix.Name)
		id := strconv.Itoa(ix.ID)
		if name == "" {
			name = id
		}
		out = append(out, ProviderIndexer{
			RemoteID: id,
			Name:     name,
			BaseURL:  origin + "/" + id + "/api",
		})
	}
	return out, nil
}
