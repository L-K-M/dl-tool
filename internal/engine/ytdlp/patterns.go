package ytdlp

import (
	_ "embed"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// extractor_patterns.txt is written by scripts/gen-ytdlp-patterns.py — one RE2
// pattern per line, generic excluded. Never hand-edit.
//
//go:embed extractor_patterns.txt
var patternTable string

// overrideRoute is one ResidualOverrides spec decoded: a lowercase host
// suffix plus, when the spec carried a "/", a fragment the decoded path must
// contain — extractors that claim only specific paths on shared hosts must
// not be claimed wholesale. The fragment excludes the separating slash, so
// `web.archive.org/youtube.com/` also covers a www.youtube.com capture.
type overrideRoute struct {
	host string
	path string
}

// ExtractorCache holds the generated routing table compiled once at start-up.
// The zero value matches nothing.
type ExtractorCache struct {
	pattern   *regexp.Regexp // one alternation over the generated table
	overrides []overrideRoute
	patterns  int // table line count
	loaded    bool
}

// LoadExtractors compiles the embedded table into one alternation and flattens
// the override hosts. An empty table returns a usable empty cache with
// loaded == false; a line that fails regexp.Compile fails the load — the table
// is committed output and a bad line means the generator or a hand edit is
// broken, which should be loud.
func LoadExtractors() (*ExtractorCache, error) {
	cache := &ExtractorCache{overrides: overrideRoutes()}

	var alts []string
	for line := range strings.Lines(patternTable) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		alts = append(alts, "(?:"+line+")")
	}
	cache.patterns = len(alts)
	if len(alts) == 0 {
		return cache, nil
	}

	// yt-dlp applies _VALID_URL with re.match, so the routing answer is a
	// start-anchored match: an unanchored MatchString would substring-match
	// e.g. https://evil.example/?next=youtube.com/watch (ADR-0022).
	re, err := regexp.Compile("^(?:" + strings.Join(alts, "|") + ")")
	if err != nil {
		// The merged alternation compiles whenever every line does; walk the
		// lines only to name the offender in the error.
		for i, line := range alts {
			if _, lerr := regexp.Compile(line); lerr != nil {
				return nil, fmt.Errorf("ytdlp: extractor table line %d: %w", i+1, lerr)
			}
		}
		return nil, fmt.Errorf("ytdlp: extractor table: %w", err)
	}
	cache.pattern = re
	cache.loaded = true
	return cache, nil
}

// overrideRoutes decodes ResidualOverrides into sorted routes: a spec is
// "host" or "host/fragment"; the host part is lowercased and matched on
// label boundaries, and the fragment — everything after the first slash —
// must appear in the URI's decoded path, so `sharepoint.com/:v:/` claims
// video views without claiming document downloads on the same host.
func overrideRoutes() []overrideRoute {
	var routes []overrideRoute
	for _, specs := range ResidualOverrides {
		for _, spec := range specs {
			r := overrideRoute{host: spec}
			if i := strings.IndexByte(spec, '/'); i >= 0 {
				r.host = spec[:i]
				r.path = strings.ToLower(spec[i+1:])
			}
			routes = append(routes, r)
		}
	}
	for i := range routes {
		routes[i].host = strings.ToLower(routes[i].host)
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].host != routes[j].host {
			return routes[i].host < routes[j].host
		}
		return routes[i].path < routes[j].path
	})
	return routes
}

func isAlnum(b byte) bool {
	return '0' <= b && b <= '9' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
}

// pathClaims reports whether fragment occurs in path at a claim boundary:
// preceded by an ASCII non-alphanumeric (path separators, dots, scheme
// colons) and — when the fragment ends in an alphanumeric — followed by an
// ASCII non-letter, so `vk.com/video` claims video-123_456 but not the
// profile `videographer`, and `youtube.com/` claims an archived
// www.youtube.com URL but not an archived fakeyoutube.com one. Bytes >= 0x80
// may be UTF-8 letters, so they never count as a boundary — the conservative
// direction, since a miss falls through to the plain lane while a wrong claim
// misroutes.
func pathClaims(path, fragment string) bool {
	for i := 0; ; {
		j := strings.Index(path[i:], fragment)
		if j < 0 {
			return false
		}
		at := i + j
		end := at + len(fragment)
		if (at == 0 || (path[at-1] < 0x80 && !isAlnum(path[at-1]))) &&
			(end == len(path) ||
				!isAlnum(fragment[len(fragment)-1]) ||
				path[end] < 0x80 && (path[end] < 'a' || 'z' < path[end])) {
			return true
		}
		i = at + 1
	}
}

// Match reports whether uri routes to yt-dlp: the URI's lowercase hostname is
// checked against the override routes (host suffix on label boundaries, plus
// the path fragment when the spec carries one), then the URI — scheme and
// authority lowercased, matching yt-dlp's netloc normalization — against the
// compiled alternation. It never performs I/O.
func (c *ExtractorCache) Match(uri string) bool {
	if c == nil {
		return false
	}
	u, err := url.Parse(uri)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	path := strings.ToLower(u.Path)
	for _, r := range c.overrides {
		if host != r.host && !strings.HasSuffix(host, "."+r.host) {
			continue
		}
		if r.path == "" || pathClaims(path, r.path) {
			return true
		}
	}
	if c.pattern == nil {
		return false
	}
	// The patterns are lowercase; yt-dlp's re sees a URL whose scheme and
	// netloc are already lowercase, so the same normalization applies to the
	// authority prefix here while path and query keep their case.
	matchURI := uri
	if i := strings.Index(uri, "://"); i > 0 {
		end := len(uri)
		if j := strings.IndexAny(uri[i+3:], "/?#"); j >= 0 {
			end = i + 3 + j
		}
		matchURI = strings.ToLower(uri[:end]) + uri[end:]
	}
	return c.pattern.MatchString(matchURI)
}

// Loaded reports whether the table compiled; false means the media lane is
// disabled.
func (c *ExtractorCache) Loaded() bool {
	return c != nil && c.loaded
}

// Len returns the number of table patterns plus override routes.
func (c *ExtractorCache) Len() int {
	if c == nil {
		return 0
	}
	return c.patterns + len(c.overrides)
}
