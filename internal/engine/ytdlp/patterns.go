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

// ExtractorCache holds the generated routing table compiled once at start-up.
// The zero value matches nothing.
type ExtractorCache struct {
	pattern  *regexp.Regexp // one alternation over the generated table
	hosts    []string       // flattened ResidualOverrides suffixes, lowercase
	patterns int            // table line count
	loaded   bool
}

// LoadExtractors compiles the embedded table into one alternation and flattens
// the override hosts. An empty table returns a usable empty cache with
// loaded == false; a line that fails regexp.Compile fails the load — the table
// is committed output and a bad line means the generator or a hand edit is
// broken, which should be loud.
func LoadExtractors() (*ExtractorCache, error) {
	cache := &ExtractorCache{hosts: flattenedHosts()}

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

// flattenedHosts is ResidualOverrides as one lowercase, sorted suffix list.
func flattenedHosts() []string {
	var hosts []string
	for _, suffixes := range ResidualOverrides {
		hosts = append(hosts, suffixes...)
	}
	for i, h := range hosts {
		hosts[i] = strings.ToLower(h)
	}
	sort.Strings(hosts)
	return hosts
}

// Match reports whether uri routes to yt-dlp: the URI's lowercase hostname is
// checked against the override suffixes (a suffix d matches h == d or
// strings.HasSuffix(h, "."+d)), then against the compiled alternation.
// It never performs I/O.
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
	for _, suffix := range c.hosts {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return c.pattern != nil && c.pattern.MatchString(uri)
}

// Loaded reports whether the table compiled; false means the media lane is
// disabled.
func (c *ExtractorCache) Loaded() bool {
	return c != nil && c.loaded
}

// Len returns the number of table patterns plus override hosts.
func (c *ExtractorCache) Len() int {
	if c == nil {
		return 0
	}
	return c.patterns + len(c.hosts)
}
