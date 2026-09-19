package rss

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The vocabularies of docs/08-rss-automation.md section 4.2: the three
// match modes of docs/04-data-model.md section 4.6, the content_layout enum
// and the matchable fields. Empty members take the ApplyDefaults values.
const (
	MatchModeWildcard = "wildcard"
	MatchModeRegex    = "regex"
	MatchModePlain    = "plain"

	contentLayoutOriginal    = "original"
	contentLayoutSubfolder   = "subfolder"
	contentLayoutNoSubfolder = "no_subfolder"

	matchFieldTitle       = "title"
	matchFieldDescription = "description"
	matchFieldCategory    = "category"
)

// The regex-safety caps of docs/08-rss-automation.md section 5.2: RE2
// removes catastrophic backtracking, so what remains is the pattern-length
// cap and the score.formats entry cap.
const (
	maxPatternBytes = 1024
	maxScoreFormats = 32
)

// RuleDoc is the rule document of docs/08-rss-automation.md section 4. It is
// JSON on the wire and in rules.definition_json, and YAML only in the
// editor. Zero values are filled by ApplyDefaults.
type RuleDoc struct {
	Name     string       `json:"name"`
	Enabled  *bool        `json:"enabled,omitempty"`
	Priority int          `json:"priority,omitempty"`
	Feeds    []string     `json:"feeds,omitempty"`
	Match    MatchSpec    `json:"match"`
	Episode  *EpisodeSpec `json:"episode,omitempty"`
	Score    *ScoreSpec   `json:"score,omitempty"`
	Action   ActionSpec   `json:"action"`
	Throttle ThrottleSpec `json:"throttle,omitempty"`
}

// MatchSpec is the match block of the rule document: the haystack selection
// and the include/exclude, size and date clauses.
type MatchSpec struct {
	Mode           string   `json:"mode,omitempty"`           // wildcard | regex | plain, default wildcard
	CaseSensitive  bool     `json:"case_sensitive,omitempty"` // default false
	Fields         []string `json:"fields,omitempty"`         // title | description | category, default [title]
	AnyOf          []string `json:"any_of,omitempty"`
	NoneOf         []string `json:"none_of,omitempty"`
	MinSize        string   `json:"min_size,omitempty"        doc:"IEC size such as 1GiB or 700MiB"`
	MaxSize        string   `json:"max_size,omitempty"        doc:"IEC size such as 1GiB or 700MiB"`
	PublishedAfter string   `json:"published_after,omitempty" format:"date-time" doc:"RFC 3339; only items published after it match"`
}

// EpisodeSpec is the optional episode block; omit it entirely for non-TV
// rules.
type EpisodeSpec struct {
	Smart             bool   `json:"smart,omitempty"`
	Filter            string `json:"filter,omitempty"`
	AllowRepackProper *bool  `json:"allow_repack_proper,omitempty"` // default true
}

// ScoreSpec is the optional Sonarr-style additive preference block.
type ScoreSpec struct {
	Minimum int           `json:"minimum,omitempty"`
	Formats []ScoreFormat `json:"formats,omitempty"`
}

// ScoreFormat is one weighted pattern of score.formats.
type ScoreFormat struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
	Weight  int    `json:"weight"`
}

// ActionSpec is what a matched item becomes: destination, category, queue
// state, layout and an optional engine pin.
type ActionSpec struct {
	Destination   string `json:"destination,omitempty"`
	Category      string `json:"category,omitempty"`
	Paused        bool   `json:"paused,omitempty"`
	ContentLayout string `json:"content_layout,omitempty"` // original | subfolder | no_subfolder
	Engine        string `json:"engine,omitempty"`
}

// ThrottleSpec is the per-rule rate limit: the cooldown against
// last_match_at and the per-run grab cap.
type ThrottleSpec struct {
	CooldownDays int `json:"cooldown_days,omitempty"`
	MaxPerRun    int `json:"max_per_run,omitempty"`
}

// FieldError names one rejected member with a JSON-pointer-style location
// such as "episode.filter", which the API renders as
// "body.definition.episode.filter" in errors[].location.
type FieldError struct {
	Location string
	Message  string
}

// ApplyDefaults fills mode=wildcard, fields=[title], enabled=true and
// allow_repack_proper=true — the doc 08 section 4.2 default column.
func (d *RuleDoc) ApplyDefaults() {
	if d.Enabled == nil {
		enabled := true
		d.Enabled = &enabled
	}
	if d.Match.Mode == "" {
		d.Match.Mode = MatchModeWildcard
	}
	if len(d.Match.Fields) == 0 {
		d.Match.Fields = []string{matchFieldTitle}
	}
	if d.Episode != nil && d.Episode.AllowRepackProper == nil {
		allow := true
		d.Episode.AllowRepackProper = &allow
	}
}

// Validate returns every problem at once, never only the first. It compiles
// each pattern with regexp (RE2), rejects an empty string inside any_of or
// none_of, a pattern longer than 1024 bytes, more than 32 score.formats
// entries, an unparseable size or published_after, and a content_layout,
// mode or fields entry outside its enum. Members left at their default —
// an empty mode, content_layout, min_size, max_size, published_after or
// episode.filter — are valid.
func (d RuleDoc) Validate() []FieldError {
	var errs []FieldError
	add := func(location, format string, args ...any) {
		errs = append(errs, FieldError{Location: location, Message: fmt.Sprintf(format, args...)})
	}

	if d.Name == "" {
		add("name", "the name is required")
	}

	switch d.Match.Mode {
	case "", MatchModeWildcard, MatchModeRegex, MatchModePlain:
	default:
		add("match.mode", "want one of wildcard, regex or plain, got %q", d.Match.Mode)
	}
	for i, field := range d.Match.Fields {
		switch field {
		case matchFieldTitle, matchFieldDescription, matchFieldCategory:
		default:
			add(fmt.Sprintf("match.fields[%d]", i), "want one of title, description or category, got %q", field)
		}
	}
	for i, entry := range d.Match.AnyOf {
		checkPattern(&errs, fmt.Sprintf("match.any_of[%d]", i), d.Match.Mode, entry)
	}
	for i, entry := range d.Match.NoneOf {
		checkPattern(&errs, fmt.Sprintf("match.none_of[%d]", i), d.Match.Mode, entry)
	}
	if d.Match.MinSize != "" {
		if _, err := parseIECSize(d.Match.MinSize); err != nil {
			add("match.min_size", "%v", err)
		}
	}
	if d.Match.MaxSize != "" {
		if _, err := parseIECSize(d.Match.MaxSize); err != nil {
			add("match.max_size", "%v", err)
		}
	}
	if d.Match.MinSize != "" && d.Match.MaxSize != "" {
		// An inverted window can never match; the cross-check runs only
		// when both members parsed, so their own errors are not doubled.
		minimum, minErr := parseIECSize(d.Match.MinSize)
		maximum, maxErr := parseIECSize(d.Match.MaxSize)
		if minErr == nil && maxErr == nil && minimum > maximum {
			add("match.max_size", "max_size %q is below min_size %q", d.Match.MaxSize, d.Match.MinSize)
		}
	}
	if d.Match.PublishedAfter != "" {
		if _, err := time.Parse(time.RFC3339, d.Match.PublishedAfter); err != nil {
			add("match.published_after", "want an RFC 3339 timestamp, got %q", d.Match.PublishedAfter)
		}
	}

	if d.Episode != nil && d.Episode.Filter != "" {
		if _, err := ParseEpisodeFilter(d.Episode.Filter); err != nil {
			add("episode.filter", "%v", err)
		}
	}

	if d.Score != nil {
		if len(d.Score.Formats) > maxScoreFormats {
			add("score.formats", "at most %d entries, got %d", maxScoreFormats, len(d.Score.Formats))
		}
		for i, format := range d.Score.Formats {
			checkPattern(&errs, fmt.Sprintf("score.formats[%d].pattern", i), MatchModeRegex, format.Pattern)
		}
	}

	if d.Throttle.CooldownDays < 0 {
		add("throttle.cooldown_days", "must be at least 0, got %d", d.Throttle.CooldownDays)
	}
	if d.Throttle.MaxPerRun < 0 {
		add("throttle.max_per_run", "must be at least 0, got %d", d.Throttle.MaxPerRun)
	}

	switch d.Action.ContentLayout {
	case "", contentLayoutOriginal, contentLayoutSubfolder, contentLayoutNoSubfolder:
	default:
		add("action.content_layout", "want one of original, subfolder or no_subfolder, got %q", d.Action.ContentLayout)
	}

	return errs
}

// checkPattern applies the doc 08 sections 4.3 and 5.2 guards to one
// pattern member: an empty string is rejected outright — the pipe-split
// typo it reproduces silently rejects or matches every article — and the
// entry must fit the byte cap and compile under the rule's match mode.
func checkPattern(errs *[]FieldError, location, mode, entry string) {
	if entry == "" {
		*errs = append(*errs, FieldError{Location: location, Message: "the pattern must not be empty"})

		return
	}
	if len(entry) > maxPatternBytes {
		*errs = append(*errs, FieldError{
			Location: location,
			Message:  fmt.Sprintf("at most %d bytes, got %d", maxPatternBytes, len(entry)),
		})

		return
	}
	if _, err := compileMatchPattern(mode, entry); err != nil {
		*errs = append(*errs, FieldError{Location: location, Message: err.Error()})
	}
}

// compileMatchPattern renders one match entry into the regular expression
// the matcher of doc 08 section 5 steps 5-6 would run: wildcard escapes the
// literal text and maps * to .* and ? to ., plain escapes it whole, and
// regex uses the entry as-is. Validation compiles every entry so a bad
// pattern can never reach the matcher; wildcard and plain entries always
// compile.
func compileMatchPattern(mode, entry string) (*regexp.Regexp, error) {
	switch mode {
	case MatchModeRegex:
		compiled, err := regexp.Compile(entry)
		if err != nil {
			return nil, fmt.Errorf("the pattern does not compile: %v", err)
		}

		return compiled, nil
	case MatchModeWildcard:
		var pattern strings.Builder
		for _, r := range entry {
			switch r {
			case '*':
				pattern.WriteString(".*")
			case '?':
				pattern.WriteString(".")
			default:
				pattern.WriteString(regexp.QuoteMeta(string(r)))
			}
		}

		return regexp.Compile(pattern.String())
	default:
		// Plain mode — and any enum value Validate would already have
		// rejected — escapes the whole entry literally.
		return regexp.Compile(regexp.QuoteMeta(entry))
	}
}

// iecUnits are the binary suffixes match.min_size and match.max_size
// accept (doc 08 section 4.2: "IEC size, e.g. 1GiB, 700MiB"), longest
// first so the prefix scan cannot stop short.
var iecUnits = []struct {
	suffix string
	mult   int64
}{
	{"eib", 1 << 60},
	{"pib", 1 << 50},
	{"tib", 1 << 40},
	{"gib", 1 << 30},
	{"mib", 1 << 20},
	{"kib", 1 << 10},
	{"b", 1},
}

// parseIECSize parses an IEC size such as "1GiB" or "700MiB" into bytes.
// The unit is case-insensitive and optional whitespace may separate the
// number from it; a bare number without a unit is not an IEC size and is
// rejected.
func parseIECSize(s string) (int64, error) {
	trimmed := strings.ToLower(strings.TrimSpace(s))
	for _, unit := range iecUnits {
		if !strings.HasSuffix(trimmed, unit.suffix) {
			continue
		}
		number := strings.TrimSpace(trimmed[:len(trimmed)-len(unit.suffix)])
		value, err := strconv.ParseFloat(number, 64)
		// !(...) is deliberate: NaN fails value >= 0 and +Inf fails the
		// upper bound, so both — and any product outside int64 — fall
		// through to the rejection rather than convert to a garbage count.
		if err != nil || !(value >= 0 && value*float64(unit.mult) < float64(math.MaxInt64)) {
			break
		}

		return int64(value * float64(unit.mult)), nil
	}

	return 0, fmt.Errorf("want an IEC size such as 1GiB, got %q", s)
}

// EpisodeFilter is a parsed episode.filter. Season leading zeros are
// stripped at parse time, which qBittorrent does not do; see
// docs/08-rss-automation.md section 6.1. An empty token list matches
// nothing — the "1x;" row of section 6.3.
type EpisodeFilter struct {
	Season int
	Tokens []EpisodeToken
}

// EpisodeToken is a single number, an inclusive range, or an open-ended
// range.
type EpisodeToken struct {
	From, To int // To is 0 for a single number, -1 for the open form "01-"
	Open     bool
}

// episodeFilterPattern is the whole-filter grammar of doc 08 section 6.1:
// the trailing ';' is mandatory, so "1x01" never reaches this far.
var episodeFilterPattern = regexp.MustCompile(`^(\d{1,4})x(.*;)$`)

// ParseEpisodeFilter enforces ^(\d{1,4})x(.*;)$ — the trailing ';' is
// mandatory. An empty filter parses to a zero EpisodeFilter that matches
// everything. Capture group 2 is split on ';' and empty tokens are skipped;
// a token that fits none of the three forms of section 6.2 — or an
// inverted range such as "14-12" — is skipped, not an error.
func ParseEpisodeFilter(s string) (EpisodeFilter, error) {
	if s == "" {
		return EpisodeFilter{}, nil
	}

	groups := episodeFilterPattern.FindStringSubmatch(s)
	if groups == nil {
		return EpisodeFilter{}, fmt.Errorf("want <season>x<token>;, got %q", s)
	}

	season, err := strconv.Atoi(stripLeadingZeros(groups[1]))
	if err != nil {
		return EpisodeFilter{}, fmt.Errorf("parse season of %q: %w", s, err)
	}
	filter := EpisodeFilter{Season: season}

	for _, token := range strings.Split(groups[2], ";") {
		if token == "" {
			continue
		}
		if parsed, ok := parseEpisodeToken(token); ok {
			filter.Tokens = append(filter.Tokens, parsed)
		}
	}

	return filter, nil
}

// parseEpisodeToken parses one token of capture group 2 into its section
// 6.2 form: "5" a single number, "12-14" an inclusive range, "01-" the open
// range. Every numeric part has leading zeros stripped, keeping at least
// one character. ok is false for a token that fits no form — including an
// inverted range — which is skipped rather than an error.
func parseEpisodeToken(token string) (parsed EpisodeToken, ok bool) {
	from, to, open := token, "", false
	if dash := strings.IndexByte(token, '-'); dash >= 0 {
		from, to, open = token[:dash], token[dash+1:], true
	}

	fromNumber, err := strconv.Atoi(stripLeadingZeros(from))
	if err != nil {
		return EpisodeToken{}, false
	}
	if !open {
		return EpisodeToken{From: fromNumber}, true
	}
	if to == "" {
		return EpisodeToken{From: fromNumber, To: -1, Open: true}, true
	}
	toNumber, err := strconv.Atoi(stripLeadingZeros(to))
	if err != nil || toNumber < fromNumber {
		return EpisodeToken{}, false
	}

	return EpisodeToken{From: fromNumber, To: toNumber}, true
}

// stripLeadingZeros removes the leading zeros of one numeric part, keeping
// at least one character (doc 08 section 6.1).
func stripLeadingZeros(s string) string {
	stripped := strings.TrimLeft(s, "0")
	if stripped == "" && s != "" {
		return "0"
	}

	return stripped
}
