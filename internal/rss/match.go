package rss

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/store"
	"github.com/L-K-M/dl-tool/internal/uri"
)

// Reason codes. The set is closed: docs/08-rss-automation.md section 5.1
// owns it and the API repeats it.
const (
	ReasonCooldown           = "cooldown"
	ReasonExcluded           = "excluded"
	ReasonNoMatch            = "no_match"
	ReasonSize               = "size"
	ReasonEpisodeFilter      = "episode_filter"
	ReasonUnparseableEpisode = "unparseable_episode"
	ReasonDuplicateEpisode   = "duplicate_episode"
	ReasonDuplicateInfoHash  = "duplicate_infohash"
	ReasonAlreadyHave        = "already_have"
	ReasonBelowMinimumScore  = "below_minimum_score"
)

// millisPerDay converts throttle.cooldown_days to the millisecond grid every
// stored timestamp uses.
const millisPerDay = 24 * 60 * 60 * 1000

// Decision is one evaluated item. Matched decisions carry MatchedBy;
// rejected ones carry Reason and ReasonDetail. Steps 1 and 3 remove an item
// before evaluation and produce no Decision at all.
type Decision struct {
	ItemID       string
	Matched      bool
	Score        int
	MatchedBy    map[string]string // e.g. {"any_of": "ubuntu *desktop* amd64"}
	Reason       string
	ReasonDetail string // `none_of[0] = "daily"`, `any_of[1] token "amd64" not found`
	Highlight    [2]int // byte offsets of the matched substring in the title, {0,0} when none
}

// Candidate is an accepted item, ready for step 13 and then for the grab in
// T071.
type Candidate struct {
	Item         store.FeedItem
	FeedURL      string
	FeedPriority int // feeds.priority — step 13's second sort key
	Score        int
	ContentKey   string // "ep:<rule_id>:<episode key>" when the rule stages one, else the info hash, else the identity (08 section 7)
	EpisodeKey   string // "" when episode.smart is off or no key was parsed
	Engine       string // engine.Route result: aria2 | qbittorrent | ytdlp
	Norm         uri.Normalized
	MatchedBy    map[string]string

	// stagedKeys is every episode key the item must write to
	// rule_seen_episodes at commit (step 14): the key itself on a fresh
	// accept, or the unseen REPACK/PROPER variants on a re-grab — a title
	// that is both stages both. Unexported because only package rss
	// commits them (grab.go, T071); stagedKeys[0] is always EpisodeKey.
	stagedKeys []string
}

// FeedRef is what steps 1 and 13 need from a candidate's feed: the URL the
// rule's feed scope compares against, and feeds.priority for the group
// sort.
type FeedRef struct {
	URL      string
	Priority int
}

// State answers the dedup ladder's questions. T070 passes a no-op
// implementation when ignore_state is true; T071 passes the
// database-backed one.
type State interface {
	// HasInfoHash reports whether a 40- or 64-hex hash was already grabbed.
	// It never truncates a v2 hash to compare it with a v1 column.
	HasInfoHash(ctx context.Context, hash string) (bool, error)
	// SeenEpisode reports whether this rule already stored that episode key.
	SeenEpisode(ctx context.Context, ruleID, key string) (bool, error)
	// BestScoreForContentKey returns the score of the stored grab for a
	// content key, and ok=false when there is none.
	BestScoreForContentKey(ctx context.Context, key string) (score int, ok bool, err error)
}

// matchClause is one none_of/any_of entry after compilation: the raw entry
// for reason_detail, its whitespace-split token texts (the whole entry in
// regex mode) and the compiled pattern per token.
type matchClause struct {
	raw    string
	parts  []string
	tokens []*regexp.Regexp
}

// scoreClause is one compiled score.formats entry.
type scoreClause struct {
	re     *regexp.Regexp
	weight int
}

// compileClause renders one match entry into the clause the matcher runs.
// In wildcard and plain mode the entry splits on whitespace into AND-ed
// tokens (doc 08 section 5 step 6); in regex mode the entry is one pattern
// and tokenise is ignored by the caller for none_of anyway, since section
// 5 step 5 compiles each entry whole.
//
// Case-insensitive matching compiles (?i) into the pattern rather than
// lowercasing text: the outcome is the doc's "lowercase both haystack and
// patterns" rule, but a regex entry's \D/\S/\W classes survive — rewriting
// the pattern text would corrupt them — and the haystack keeps its original
// byte offsets for Highlight.
func compileClause(mode, entry string, caseSensitive, tokenise bool) (matchClause, error) {
	clause := matchClause{raw: entry}
	parts := []string{entry}
	if tokenise && mode != MatchModeRegex {
		parts = strings.Fields(entry)
	}
	for _, part := range parts {
		re, err := compileMatchPattern(mode, part)
		if err != nil {
			return matchClause{}, err
		}
		if !caseSensitive {
			re, err = regexp.Compile(`(?i)` + re.String())
			if err != nil {
				return matchClause{}, fmt.Errorf("compile %q case-insensitive: %w", part, err)
			}
		}
		clause.parts = append(clause.parts, part)
		clause.tokens = append(clause.tokens, re)
	}
	return clause, nil
}

// ruleEval is a rule document prepared for one Evaluate pass: every pattern
// compiled once, every bound parsed once, so the per-item loop is pure
// matching.
type ruleEval struct {
	doc  RuleDoc
	rule store.Rule

	noneOf []matchClause
	anyOf  []matchClause

	minSize, maxSize int64
	hasMin, hasMax   bool

	publishedAfter int64
	hasFloor       bool

	filter    EpisodeFilter
	filterSet bool
	smart     bool
	allowRep  bool

	scoreClauses []scoreClause
	scoreMinimum int

	cooldownMs int64
}

// newRuleEval compiles the document. Validation ran at save time (T068), so
// an error here means an unvalidated document slipped in — reported, never
// panicked on.
func newRuleEval(doc RuleDoc, rule store.Rule) (*ruleEval, error) {
	e := &ruleEval{doc: doc, rule: rule}

	// ApplyDefaults ran at save time; the guards keep a hand-built
	// document on the documented defaults rather than a different mode.
	mode := doc.Match.Mode
	if mode == "" {
		mode = MatchModeWildcard
	}
	fields := doc.Match.Fields
	if len(fields) == 0 {
		fields = []string{matchFieldTitle}
	}
	e.doc.Match.Mode, e.doc.Match.Fields = mode, fields

	// An unknown field would silently shrink the haystack; reject it here
	// rather than per item.
	for _, field := range fields {
		if field != matchFieldTitle {
			return nil, fmt.Errorf("match.fields: unsupported field %q", field)
		}
	}

	for i, entry := range doc.Match.NoneOf {
		clause, err := compileClause(mode, entry, doc.Match.CaseSensitive, false)
		if err != nil {
			return nil, fmt.Errorf("match.none_of[%d]: %w", i, err)
		}
		e.noneOf = append(e.noneOf, clause)
	}
	for i, entry := range doc.Match.AnyOf {
		clause, err := compileClause(mode, entry, doc.Match.CaseSensitive, true)
		if err != nil {
			return nil, fmt.Errorf("match.any_of[%d]: %w", i, err)
		}
		e.anyOf = append(e.anyOf, clause)
	}

	if doc.Match.MinSize != "" {
		size, err := parseIECSize(doc.Match.MinSize)
		if err != nil {
			return nil, fmt.Errorf("match.min_size: %w", err)
		}
		e.minSize, e.hasMin = size, true
	}
	if doc.Match.MaxSize != "" {
		size, err := parseIECSize(doc.Match.MaxSize)
		if err != nil {
			return nil, fmt.Errorf("match.max_size: %w", err)
		}
		e.maxSize, e.hasMax = size, true
	}
	if doc.Match.PublishedAfter != "" {
		floor, err := time.Parse(time.RFC3339, doc.Match.PublishedAfter)
		if err != nil {
			return nil, fmt.Errorf("match.published_after: %w", err)
		}
		e.publishedAfter, e.hasFloor = floor.UnixMilli(), true
	}

	if doc.Episode != nil {
		if doc.Episode.Filter != "" {
			filter, err := ParseEpisodeFilter(doc.Episode.Filter)
			if err != nil {
				return nil, fmt.Errorf("episode.filter: %w", err)
			}
			if filter.Season == 0 && len(filter.Tokens) == 0 {
				// A "0x;"-shaped filter parses to the zero EpisodeFilter,
				// which Match reads as match-everything — the worst
				// failure mode for a grabbing tool. Reject it here so a
				// non-empty filter can never silently match all.
				return nil, fmt.Errorf("episode.filter %q selects no season", doc.Episode.Filter)
			}
			e.filter, e.filterSet = filter, true
		}
		e.smart = doc.Episode.Smart
		e.allowRep = doc.Episode.AllowRepackProper == nil || *doc.Episode.AllowRepackProper
	}

	if doc.Score != nil {
		e.scoreMinimum = doc.Score.Minimum
		for i, format := range doc.Score.Formats {
			// score.formats patterns are always case-insensitive
			// (doc 08 section 4.2), whatever match.case_sensitive says.
			re, err := regexp.Compile(`(?i)` + format.Pattern)
			if err != nil {
				return nil, fmt.Errorf("score.formats[%d].pattern: %w", i, err)
			}
			e.scoreClauses = append(e.scoreClauses, scoreClause{re: re, weight: format.Weight})
		}
	}

	e.cooldownMs = int64(doc.Throttle.CooldownDays) * millisPerDay

	return e, nil
}

// Evaluate runs steps 1 to 12 of docs/08-rss-automation.md section 5 for
// one rule — decide, then collect and route; nothing is written. Items are
// the candidate set, already newest-first. feedByID is keyed on
// feed_items.feed_id. It is side-effect free and safe to call
// concurrently.
func Evaluate(ctx context.Context, doc RuleDoc, rule store.Rule, items []store.FeedItem,
	feedByID map[string]FeedRef, st State, now int64) ([]Decision, []Candidate, error) {
	// now is for the step-14 timestamps T071 owns; steps 1-12 and 13
	// read only stored values, so it is unused here.
	e, err := newRuleEval(doc, rule)
	if err != nil {
		return nil, nil, fmt.Errorf("rss: evaluate rule %s: %w", rule.ID, err)
	}

	decisions := make([]Decision, 0, len(items))
	candidates := make([]Candidate, 0, len(items))
	for _, item := range items {
		feed := feedByID[item.FeedID]

		// Step 1 — feed scope: an item outside rule.feeds leaves the set
		// with no Decision.
		if len(doc.Feeds) > 0 && !slices.Contains(doc.Feeds, feed.URL) {
			continue
		}

		decision, candidate, emitted, err := e.evaluateItem(ctx, item, feed, st)
		if err != nil {
			return nil, nil, err
		}
		if emitted {
			decisions = append(decisions, decision)
		}
		if candidate != nil {
			candidates = append(candidates, *candidate)
		}
	}

	return decisions, candidates, nil
}

// evaluateItem runs one in-scope item through steps 2-12. emitted is false
// when the item leaves the candidate set without a Decision: the step-3
// date floor and the unroutable-URI guard at step 12.
func (e *ruleEval) evaluateItem(ctx context.Context, item store.FeedItem, feed FeedRef, st State) (Decision, *Candidate, bool, error) {
	d := Decision{ItemID: item.ID}
	reject := func(reason, detail string) (Decision, *Candidate, bool, error) {
		d.Reason, d.ReasonDetail = reason, detail
		return d, nil, true, nil
	}

	// Step 2 — cooldown. qBittorrent's ignoreDays: items published within
	// cooldown_days of rules.last_match_at are skipped. A missing
	// published_at cannot be shown to fall inside the window.
	if e.cooldownMs > 0 && e.rule.LastMatchAt != nil && item.PublishedAt != nil &&
		*item.PublishedAt < *e.rule.LastMatchAt+e.cooldownMs {
		return reject(ReasonCooldown, fmt.Sprintf("cooldown_days=%d, last_match_at=%s",
			e.doc.Throttle.CooldownDays,
			time.UnixMilli(*e.rule.LastMatchAt).UTC().Format(time.RFC3339)))
	}

	// Step 3 — date floor: like step 1, removal produces no Decision. A
	// missing published_at is not "earlier", so the item stays.
	if e.hasFloor && item.PublishedAt != nil && *item.PublishedAt < e.publishedAfter {
		return Decision{}, nil, false, nil
	}

	// Step 4 — the haystack: the configured match.fields joined with \n.
	haystack := buildHaystack(item, e.doc.Match.Fields)

	// Step 5 — none_of, the cheap rejector, evaluated before any_of.
	for i, clause := range e.noneOf {
		if clause.tokens[0].MatchString(haystack) {
			return reject(ReasonExcluded, fmt.Sprintf("none_of[%d] = %q", i, clause.raw))
		}
	}

	// Step 6 — any_of: empty passes; otherwise the first entry whose
	// tokens all match wins, and the reason_detail of a miss names the
	// closest entry's first missing token.
	matchedBy := map[string]string{}
	var highlight [2]int
	if len(e.anyOf) > 0 {
		entry, ok := e.matchAny(haystack)
		if !ok {
			missEntry, missToken, _ := e.closestMiss(haystack)
			return reject(ReasonNoMatch, fmt.Sprintf("any_of[%d] token %q not found",
				missEntry, e.anyOf[missEntry].parts[missToken]))
		}
		matchedBy["any_of"] = e.anyOf[entry].raw
		// Highlight is a title offset, so the span is searched in the
		// title itself — not the joined haystack, which would shift the
		// offsets once match.fields carries more than title.
		if span := e.anyOf[entry].tokens[0].FindStringIndex(item.Title); span != nil {
			highlight = [2]int{span[0], span[1]}
		}
	}

	// Step 7 — size bounds apply only when the size is known; a NULL
	// size_bytes passes.
	if item.SizeBytes != nil {
		if e.hasMin && *item.SizeBytes < e.minSize {
			return reject(ReasonSize, fmt.Sprintf("size_bytes=%d < min_size=%s",
				*item.SizeBytes, e.doc.Match.MinSize))
		}
		if e.hasMax && *item.SizeBytes > e.maxSize {
			return reject(ReasonSize, fmt.Sprintf("size_bytes=%d > max_size=%s",
				*item.SizeBytes, e.doc.Match.MaxSize))
		}
	}

	// Step 8 — episode.filter.
	if e.filterSet && !e.filter.Match(item.Title) {
		return reject(ReasonEpisodeFilter, fmt.Sprintf("no token of %q matched", e.doc.Episode.Filter))
	}

	// Step 9 — episode.smart key dedup, the decision table of doc 08
	// section 6.4.
	var episodeKey string
	var staged []string
	if e.smart {
		key, ok := SmartKey(item.Title)
		if !ok {
			return reject(ReasonUnparseableEpisode, "smart filter found no season/episode in the title")
		}
		seen, err := st.SeenEpisode(ctx, e.rule.ID, key)
		if err != nil {
			return Decision{}, nil, false, fmt.Errorf("rss: evaluate rule %s: %w", e.rule.ID, err)
		}
		switch {
		case !seen:
			episodeKey, staged = key, []string{key}
		case !e.allowRep:
			return reject(ReasonDuplicateEpisode,
				fmt.Sprintf("episode_key %q already seen for this rule", key))
		default:
			// Only unseen variants stage: committing an already-stored
			// key would hit the (rule_id, episode_key) uniqueness. A
			// REPACK+PROPER title stages every unseen variant, so
			// neither can be grabbed later.
			variants := RepackVariants(key, item.Title)
			if len(variants) == 0 {
				return reject(ReasonDuplicateEpisode,
					fmt.Sprintf("episode_key %q already seen for this rule", key))
			}
			var pending []string
			for _, variant := range variants {
				variantSeen, err := st.SeenEpisode(ctx, e.rule.ID, variant)
				if err != nil {
					return Decision{}, nil, false, fmt.Errorf("rss: evaluate rule %s: %w", e.rule.ID, err)
				}
				if !variantSeen {
					pending = append(pending, variant)
				}
			}
			if len(pending) == 0 {
				return reject(ReasonDuplicateEpisode,
					fmt.Sprintf("episode_key %q already seen for this rule", variants[0]))
			}
			episodeKey, staged = pending[0], pending
		}
	}

	// The step-10 content-key rung compares on score, so the step-11 total
	// is computed here; the minimum check still runs at its own step.
	d.Score = e.score(haystack)

	// Step 10 — the dedup ladder of doc 08 section 7. The identity rung
	// has already run: the candidate set only holds items the poller
	// accepted as new. Info-hash first, then the content key.
	if item.InfoHash != nil && *item.InfoHash != "" {
		dup, err := st.HasInfoHash(ctx, *item.InfoHash)
		if err != nil {
			return Decision{}, nil, false, fmt.Errorf("rss: evaluate rule %s: %w", e.rule.ID, err)
		}
		if dup {
			return reject(ReasonDuplicateInfoHash,
				fmt.Sprintf("infohash %s already grabbed", *item.InfoHash))
		}
	}
	contentKey := contentKeyFor(e.rule.ID, episodeKey, item)
	best, ok, err := st.BestScoreForContentKey(ctx, contentKey)
	if err != nil {
		return Decision{}, nil, false, fmt.Errorf("rss: evaluate rule %s: %w", e.rule.ID, err)
	}
	if ok && d.Score <= best {
		return reject(ReasonAlreadyHave, fmt.Sprintf("content_key %q already grabbed with score %d",
			contentKey, best))
	}

	// Step 11 — the summed score must reach score.minimum.
	if d.Score < e.scoreMinimum {
		return reject(ReasonBelowMinimumScore, fmt.Sprintf("score=%d < minimum=%d", d.Score, e.scoreMinimum))
	}

	// Step 12 — collect, routing exactly like a pasted URI. An item whose
	// stored download_url cannot be normalised or routed is a
	// store-invariant breach (the parser only stores items with a
	// download URI), not a rule outcome — and no reason code of section
	// 5.1 covers it, so it aborts the pass with an explicit error instead
	// of vanishing silently. Never log the raw URL: it may carry userinfo.
	if item.DownloadURL == nil {
		return Decision{}, nil, false, fmt.Errorf("rss: evaluate rule %s: item %s: no download_url", e.rule.ID, item.Identity)
	}
	norm, err := uri.Normalize(*item.DownloadURL)
	if err != nil {
		return Decision{}, nil, false, fmt.Errorf("rss: evaluate rule %s: item %s: %w", e.rule.ID, item.Identity, err)
	}
	name, err := engine.Route(norm, nil)
	if err != nil {
		return Decision{}, nil, false, fmt.Errorf("rss: evaluate rule %s: item %s: route %s: %w",
			e.rule.ID, item.Identity, norm.URI, err)
	}

	d.Matched, d.MatchedBy, d.Highlight = true, matchedBy, highlight
	return d, &Candidate{
		Item:         item,
		FeedURL:      feed.URL,
		FeedPriority: feed.Priority,
		Score:        d.Score,
		ContentKey:   contentKey,
		EpisodeKey:   episodeKey,
		Engine:       name,
		Norm:         norm,
		MatchedBy:    matchedBy,
		stagedKeys:   staged,
	}, true, nil
}

// matchAny returns the index of the first any_of entry whose every token
// matched, or ok=false.
func (e *ruleEval) matchAny(haystack string) (entry int, ok bool) {
	for i, clause := range e.anyOf {
		matched := true
		for _, token := range clause.tokens {
			if !token.MatchString(haystack) {
				matched = false
				break
			}
		}
		if matched {
			return i, true
		}
	}
	return 0, false
}

// closestMiss picks the any_of entry that came nearest to passing — fewest
// unmatched tokens, earliest index on a tie — and the index of its first
// missing token, so reason_detail can name both. ok is false when every
// entry matched, which cannot happen at the only call site (it runs after
// matchAny failed) but keeps the signature honest.
func (e *ruleEval) closestMiss(haystack string) (entry, token int, ok bool) {
	fewest := -1
	for i, clause := range e.anyOf {
		misses, firstMiss := 0, -1
		for j, tokenRe := range clause.tokens {
			if !tokenRe.MatchString(haystack) {
				misses++
				if firstMiss < 0 {
					firstMiss = j
				}
			}
		}
		if firstMiss >= 0 && (fewest < 0 || misses < fewest) {
			fewest, entry, token, ok = misses, i, firstMiss, true
		}
	}
	return entry, token, ok
}

// score sums the weights of every score.formats pattern matching the
// haystack (doc 08 section 5 step 11).
func (e *ruleEval) score(haystack string) int {
	total := 0
	for _, clause := range e.scoreClauses {
		if clause.re.MatchString(haystack) {
			total += clause.weight
		}
	}
	return total
}

// buildHaystack concatenates the configured match.fields with \n (doc 08
// section 5 step 4). title is the only field the store carries — the enum
// grows only when the store does — so today the haystack is the title.
func buildHaystack(item store.FeedItem, fields []string) string {
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		if field == matchFieldTitle {
			parts = append(parts, item.Title)
		}
	}
	return strings.Join(parts, "\n")
}

// contentKeyFor builds the step-10/13 key of doc 08 section 7: a staged
// episode key namespaced per rule, else the info hash, else the identity.
func contentKeyFor(ruleID, episodeKey string, item store.FeedItem) string {
	if episodeKey != "" {
		return "ep:" + ruleID + ":" + episodeKey
	}
	if item.InfoHash != nil && *item.InfoHash != "" {
		return *item.InfoHash
	}
	return item.Identity
}

// Resolve is step 13: group by ContentKey, sort each group by (score DESC,
// feed priority ASC, published_at DESC) and return the winner of each
// group first — winners sorted among themselves by the same key so
// throttle.max_per_run spends its slots on the best. Losers keep their
// ranked order and become rule_matches rows with status 'fallback' in
// T071.
func Resolve(cands []Candidate) (winners, losers []Candidate) {
	groups := make(map[string][]Candidate, len(cands))
	order := make([]string, 0, len(cands))
	for _, cand := range cands {
		if _, ok := groups[cand.ContentKey]; !ok {
			order = append(order, cand.ContentKey)
		}
		groups[cand.ContentKey] = append(groups[cand.ContentKey], cand)
	}

	better := func(a, b Candidate) int {
		if c := cmp.Compare(b.Score, a.Score); c != 0 {
			return c
		}
		if c := cmp.Compare(a.FeedPriority, b.FeedPriority); c != 0 {
			return c
		}
		return cmp.Compare(publishedAt(b), publishedAt(a))
	}

	for _, key := range order {
		group := groups[key]
		slices.SortStableFunc(group, better)
		winners = append(winners, group[0])
		losers = append(losers, group[1:]...)
	}
	slices.SortStableFunc(winners, better)

	return winners, losers
}

// publishedAt reads the sortable published_at of a candidate; a NULL sorts
// oldest.
func publishedAt(c Candidate) int64 {
	if c.Item.PublishedAt == nil {
		return 0
	}
	return *c.Item.PublishedAt
}
