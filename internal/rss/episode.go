package rss

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// The two title parsers of docs/08-rss-automation.md section 6.2, in the
// order the range tokens try them: the s01e05 form first, then the 1x05
// form. Both are case-insensitive — the section 6.3 worked examples carry
// uppercase S/E.
var (
	partialPattern1 = regexp.MustCompile(`(?i)\bs0?(\d{1,4})[ -_.]?e(0?\d{1,4})(?:\D|\b)`)
	partialPattern2 = regexp.MustCompile(`(?i)\b(\d{1,4})x(0?\d{1,4})(?:\D|\b)`)
)

// smartKeyPattern is the single case-insensitive alternation of
// docs/08-rss-automation.md section 6.4: the four formats joined under one
// (?:_|\b) wrapper, so a '_' separator counts as a boundary where \b would
// not fire.
var smartKeyPattern = regexp.MustCompile(
	`(?i)(?:_|\b)(?:s(\d+)e(\d+)|(\d+)x(\d+)|` +
		`(\d{4}[.\-]\d{1,2}[.\-]\d{1,2})|(\d{1,2}[.\-]\d{1,2}[.\-]\d{4}))(?:_|\b)`)

// Match applies the token semantics of docs/08-rss-automation.md section
// 6.2 to one title: any token matching is a match. A zero EpisodeFilter —
// what an empty filter string parses to — matches everything; a parsed
// filter whose tokens were all empty or invalid matches nothing (the "1x;"
// row of section 6.3).
func (f EpisodeFilter) Match(title string) bool {
	if f.Season == 0 && len(f.Tokens) == 0 {
		return true
	}
	for _, token := range f.Tokens {
		if f.tokenMatches(token, title) {
			return true
		}
	}
	return false
}

// tokenMatches evaluates one token of the filter against the title. The
// range and open forms parse (season, episode) out of the title and
// compare numerically; the single-number form compiles the regex section
// 6.2 prescribes, which deliberately matches "e05" but not "e005".
func (f EpisodeFilter) tokenMatches(token EpisodeToken, title string) bool {
	switch {
	case token.Open:
		season, episode, ok := ParseSeasonEpisode(title)
		return ok && (season > f.Season || (season == f.Season && episode >= token.From))
	case token.To != 0:
		season, episode, ok := ParseSeasonEpisode(title)
		return ok && season == f.Season && token.From <= episode && episode <= token.To
	default:
		return singleEpisodePattern(f.Season, token.From).MatchString(title)
	}
}

// singlePatternCache memoises the per-(season, episode) regexes:
// singleEpisodePattern runs once per single-number token per item, and
// compiling the same pattern every call would dominate an evaluation pass.
var singlePatternCache sync.Map // [2]int{season, episode} -> *regexp.Regexp

// singleEpisodePattern compiles the section 6.2 single-number form
// \b(?:s0?{S}[ -_\.]?e0?{E}|{S}x0?{E})(?:\D|\b) for a normalised season —
// ParseEpisodeFilter has already stripped its leading zeros.
func singleEpisodePattern(season, episode int) *regexp.Regexp {
	key := [2]int{season, episode}
	if cached, ok := singlePatternCache.Load(key); ok {
		return cached.(*regexp.Regexp)
	}
	s, e := strconv.Itoa(season), strconv.Itoa(episode)
	re := regexp.MustCompile(`(?i)\b(?:s0?` + s + `[ -_.]?e0?` + e + `|` + s + `x0?` + e + `)(?:\D|\b)`)
	singlePatternCache.Store(key, re)
	return re
}

// ParseSeasonEpisode is the title parser every range token uses: try
// partialPattern1, then partialPattern2, and report the first hit.
func ParseSeasonEpisode(title string) (season, episode int, ok bool) {
	for _, pattern := range []*regexp.Regexp{partialPattern1, partialPattern2} {
		groups := pattern.FindStringSubmatch(title)
		if groups == nil {
			continue
		}
		// Both patterns capture \d{1,4} runs, so the parses cannot fail.
		season, _ = strconv.Atoi(groups[1])
		episode, _ = strconv.Atoi(groups[2])
		return season, episode, true
	}
	return 0, 0, false
}

// SmartKey builds the dedup key of docs/08-rss-automation.md section 6.4:
// the first alternation hit's non-empty capture groups, integer-parsed
// (stripping leading zeros) when they are purely numeric and kept verbatim
// otherwise, joined with the literal 'x'. ok is false when no format
// matches — the caller rejects unparseable_episode.
func SmartKey(title string) (string, bool) {
	groups := smartKeyPattern.FindStringSubmatch(title)
	if groups == nil {
		return "", false
	}
	parts := make([]string, 0, len(groups)-1)
	for _, group := range groups[1:] {
		if group == "" {
			continue
		}
		if n, err := strconv.Atoi(group); err == nil {
			parts = append(parts, strconv.Itoa(n))
		} else {
			parts = append(parts, group)
		}
	}
	return strings.Join(parts, "x"), true
}

// RepackVariants returns the key variants a REPACK or PROPER title stages
// (docs/08-rss-automation.md section 6.4): a title that is both stages
// key+"-REPACK" and key+"-PROPER" — in that order — so neither can be
// grabbed later.
func RepackVariants(key, title string) []string {
	upper := strings.ToUpper(title)
	variants := make([]string, 0, 2)
	if strings.Contains(upper, "REPACK") {
		variants = append(variants, key+"-REPACK")
	}
	if strings.Contains(upper, "PROPER") {
		variants = append(variants, key+"-PROPER")
	}
	return variants
}
