package rss

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/store"
)

// fakeState is the in-memory State for match tests: three lookup tables and
// an optional error to prove Evaluate propagates State failures.
type fakeState struct {
	hashes   map[string]bool
	episodes map[string]bool // ruleID + "\x00" + key
	best     map[string]int
	err      error
}

func (s fakeState) HasInfoHash(_ context.Context, hash string) (bool, error) {
	return s.hashes[hash], s.err
}

func (s fakeState) SeenEpisode(_ context.Context, ruleID, key string) (bool, error) {
	return s.episodes[ruleID+"\x00"+key], s.err
}

func (s fakeState) BestScoreForContentKey(_ context.Context, key string) (int, bool, error) {
	score, ok := s.best[key]
	return score, ok, s.err
}

func emptyState() fakeState {
	return fakeState{hashes: map[string]bool{}, episodes: map[string]bool{}, best: map[string]int{}}
}

const testRuleID = "rul_01TEST0000000000000000000"
const testFeedURL = "https://example.com/feed.xml"
const testFeedID = "fee_01TEST0000000000000000000"

var testFeedScope = map[string]FeedRef{testFeedID: {URL: testFeedURL}}

// matchItem builds a feed_items row for evaluation: always routable via a
// .torrent URL so an accepted item always becomes a candidate.
func matchItem(id, title string) store.FeedItem {
	return store.FeedItem{
		ID:          "itm_" + id,
		FeedID:      testFeedID,
		Identity:    "id-" + id,
		Title:       title,
		DownloadURL: strPtr("https://example.com/t/" + id + ".torrent"),
		PublishedAt: ptrInt64(testNow.UnixMilli()),
	}
}

// evalOne runs Evaluate over a single item and returns its Decision.
func evalOne(t *testing.T, doc RuleDoc, rule store.Rule, item store.FeedItem, st State) Decision {
	t.Helper()
	doc.ApplyDefaults()
	decisions, _, err := Evaluate(t.Context(), doc, rule, []store.FeedItem{item}, testFeedScope, st, testNow.UnixMilli())
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	return decisions[0]
}

// TestEveryReasonCodeIsProduced: one case per code of doc 08 section 5.1,
// and no eleventh string ever leaves Evaluate.
func TestEveryReasonCodeIsProduced(t *testing.T) {
	lastMatch := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC).UnixMilli()
	oldEnough := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC).UnixMilli()
	v1hash := "dcb9178653b651c7ca4526e11fa8e22f74e2fd7a"
	noRepack := false

	cases := []struct {
		name   string
		doc    RuleDoc
		rule   store.Rule
		item   store.FeedItem
		state  fakeState
		reason string
		detail string // substring the detail must carry
	}{
		{
			name:   "cooldown",
			doc:    RuleDoc{Throttle: ThrottleSpec{CooldownDays: 3}},
			rule:   store.Rule{ID: testRuleID, LastMatchAt: &lastMatch},
			item:   withPublished(matchItem("c1", "Ubuntu ISO"), oldEnough),
			state:  emptyState(),
			reason: ReasonCooldown,
			detail: "cooldown_days=3, last_match_at=2026-08-30T00:00:00Z",
		},
		{
			name:   "excluded",
			doc:    RuleDoc{Match: MatchSpec{NoneOf: []string{"daily"}}},
			rule:   store.Rule{ID: testRuleID},
			item:   matchItem("c2", "Ubuntu daily build"),
			state:  emptyState(),
			reason: ReasonExcluded,
			detail: `none_of[0] = "daily"`,
		},
		{
			name:   "no_match",
			doc:    RuleDoc{Match: MatchSpec{AnyOf: []string{"ubuntu *desktop* amd64", "kubuntu *amd64*"}}},
			rule:   store.Rule{ID: testRuleID},
			item:   matchItem("c3", "debian netinst"),
			state:  emptyState(),
			reason: ReasonNoMatch,
			detail: `any_of[1] token "kubuntu" not found`,
		},
		{
			name:   "size",
			doc:    RuleDoc{Match: MatchSpec{MaxSize: "1GiB"}},
			rule:   store.Rule{ID: testRuleID},
			item:   withSize(matchItem("c4", "Ubuntu ISO"), 2<<30),
			state:  emptyState(),
			reason: ReasonSize,
			detail: "size_bytes=2147483648 > max_size=1GiB",
		},
		{
			name:   "episode_filter",
			doc:    RuleDoc{Episode: &EpisodeSpec{Filter: "1x05;"}},
			rule:   store.Rule{ID: testRuleID},
			item:   matchItem("c5", "Show.S01E03.1080p"),
			state:  emptyState(),
			reason: ReasonEpisodeFilter,
			detail: `no token of "1x05;" matched`,
		},
		{
			name:   "unparseable_episode",
			doc:    RuleDoc{Episode: &EpisodeSpec{Smart: true}},
			rule:   store.Rule{ID: testRuleID},
			item:   matchItem("c6", "Ubuntu ISO"),
			state:  emptyState(),
			reason: ReasonUnparseableEpisode,
			detail: "smart filter found no season/episode in the title",
		},
		{
			name: "duplicate_episode",
			doc:  RuleDoc{Episode: &EpisodeSpec{Smart: true, AllowRepackProper: &noRepack}},
			rule: store.Rule{ID: testRuleID},
			item: matchItem("c7", "Show.S01E05.1080p"),
			state: fakeState{episodes: map[string]bool{
				testRuleID + "\x00" + "1x5": true,
			}},
			reason: ReasonDuplicateEpisode,
			detail: `episode_key "1x5" already seen for this rule`,
		},
		{
			name:   "duplicate_infohash",
			doc:    RuleDoc{},
			rule:   store.Rule{ID: testRuleID},
			item:   withHash(matchItem("c8", "Ubuntu ISO"), v1hash),
			state:  fakeState{hashes: map[string]bool{v1hash: true}},
			reason: ReasonDuplicateInfoHash,
			detail: "infohash " + v1hash + " already grabbed",
		},
		{
			name:   "already_have",
			doc:    RuleDoc{},
			rule:   store.Rule{ID: testRuleID},
			item:   matchItem("c9", "Ubuntu ISO"),
			state:  fakeState{best: map[string]int{"id-c9": 40}},
			reason: ReasonAlreadyHave,
			detail: `content_key "id-c9" already grabbed with score 40`,
		},
		{
			name: "below_minimum_score",
			doc: RuleDoc{Score: &ScoreSpec{Minimum: 10, Formats: []ScoreFormat{
				{Name: "prefer-64bit", Pattern: "amd64", Weight: 20},
			}}},
			rule:   store.Rule{ID: testRuleID},
			item:   matchItem("c10", "Ubuntu ISO"),
			state:  emptyState(),
			reason: ReasonBelowMinimumScore,
			detail: "score=0 < minimum=10",
		},
	}

	seen := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := evalOne(t, tc.doc, tc.rule, tc.item, tc.state)
			require.False(t, d.Matched)
			require.Equal(t, tc.reason, d.Reason)
			require.Equal(t, tc.detail, d.ReasonDetail)
			seen[d.Reason] = true
		})
	}

	closed := []string{
		ReasonCooldown, ReasonExcluded, ReasonNoMatch, ReasonSize,
		ReasonEpisodeFilter, ReasonUnparseableEpisode, ReasonDuplicateEpisode,
		ReasonDuplicateInfoHash, ReasonAlreadyHave, ReasonBelowMinimumScore,
	}
	require.Len(t, closed, 10)
	for _, code := range closed {
		require.True(t, seen[code], "reason %q was never produced", code)
	}
	require.Len(t, seen, 10, "an eleventh reason string was produced")
}

func withPublished(item store.FeedItem, ms int64) store.FeedItem {
	item.PublishedAt = ptrInt64(ms)
	return item
}

func withSize(item store.FeedItem, size int64) store.FeedItem {
	item.SizeBytes = &size
	return item
}

func withHash(item store.FeedItem, hash string) store.FeedItem {
	item.InfoHash = &hash
	return item
}

// TestNoneOfEvaluatedBeforeAnyOf: an item matching both clauses reports
// excluded — the cheap rejector runs first (doc 08 section 5 step 5).
func TestNoneOfEvaluatedBeforeAnyOf(t *testing.T) {
	doc := RuleDoc{Match: MatchSpec{
		AnyOf:  []string{"*ubuntu*"},
		NoneOf: []string{"daily"},
	}}
	d := evalOne(t, doc, store.Rule{ID: testRuleID}, matchItem("n1", "Ubuntu daily"), emptyState())
	require.Equal(t, ReasonExcluded, d.Reason)
	require.Equal(t, `none_of[0] = "daily"`, d.ReasonDetail)
}

// TestUnknownSizePasses: a NULL size_bytes is not rejected by size bounds
// (doc 08 section 5 step 7).
func TestUnknownSizePasses(t *testing.T) {
	doc := RuleDoc{Match: MatchSpec{MinSize: "1GiB", MaxSize: "8GiB"}}
	item := matchItem("s1", "Ubuntu ISO")
	item.SizeBytes = nil

	doc.ApplyDefaults()
	decisions, cands, err := Evaluate(t.Context(), doc, store.Rule{ID: testRuleID},
		[]store.FeedItem{item}, testFeedScope, emptyState(), testNow.UnixMilli())
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	require.True(t, decisions[0].Matched)
	require.Len(t, cands, 1)
}

// TestEpisodeFilterWorkedExamples: every row of doc 08 section 6.3.
func TestEpisodeFilterWorkedExamples(t *testing.T) {
	cases := []struct {
		filter string
		title  string
		match  bool
	}{
		{"1x01-;", "Show.S01E01.1080p", true},
		{"1x01-;", "Show.S03E07.1080p", true},
		{"1x01-;", "Show 1x00 Pilot", false},
		{"2x5;9;12-14;", "Show.S02E09", true},
		{"2x5;9;12-14;", "Show 2x13", true},
		{"2x5;9;12-14;", "Show.S03E13", false}, // same range, wrong season
		{"1x;", "Show.S01E01", false},
		{"01x05;", "Show 1x05", true}, // dl-tool normalises the season
		{"", "anything at all", true},
	}
	for _, tc := range cases {
		name := fmt.Sprintf("%q on %q", tc.filter, tc.title)
		t.Run(name, func(t *testing.T) {
			doc := RuleDoc{Episode: &EpisodeSpec{Filter: tc.filter}}
			d := evalOne(t, doc, store.Rule{ID: testRuleID}, matchItem("w1", tc.title), emptyState())
			if tc.match {
				require.True(t, d.Matched, "reason=%q detail=%q", d.Reason, d.ReasonDetail)
			} else {
				require.False(t, d.Matched)
				require.Equal(t, ReasonEpisodeFilter, d.Reason)
			}
		})
	}

	// The "1x01" row is a save-time 422: the filter must fail to parse.
	_, err := ParseEpisodeFilter("1x01")
	require.Error(t, err)
}

// TestSmartKeyCollapsesNotations: the three worked keys of doc 08 section
// 6.4 — S01E05 and 1x05 are the same episode.
func TestSmartKeyCollapsesNotations(t *testing.T) {
	for _, tc := range []struct{ title, key string }{
		{"Show.S01E05.1080p", "1x5"},
		{"Show 1x05 1080p", "1x5"},
		{"Show.2017.01.01", "2017.01.01"},
	} {
		key, ok := SmartKey(tc.title)
		require.True(t, ok, tc.title)
		require.Equal(t, tc.key, key)
	}
	_, ok := SmartKey("Ubuntu ISO")
	require.False(t, ok)
}

// TestRuleRoutesHTTPAndMagnet: a grabbed item follows the doc 06 section 2
// routing table exactly like a pasted URI — HTTP lands on aria2, magnet on
// qBittorrent.
func TestRuleRoutesHTTPAndMagnet(t *testing.T) {
	httpItem := matchItem("r1", "Ubuntu ISO")
	httpItem.DownloadURL = strPtr("https://example.com/ubuntu.iso")
	magnetItem := matchItem("r2", "Fedora ISO")
	magnetItem.DownloadURL = strPtr(
		"magnet:?xt=urn:btih:dcb9178653b651c7ca4526e11fa8e22f74e2fd7a&dn=Fedora")

	doc := RuleDoc{}
	doc.ApplyDefaults()
	_, cands, err := Evaluate(t.Context(), doc, store.Rule{ID: testRuleID},
		[]store.FeedItem{httpItem, magnetItem}, testFeedScope, emptyState(), testNow.UnixMilli())
	require.NoError(t, err)
	require.Len(t, cands, 2)
	require.Equal(t, engine.NameAria2, cands[0].Engine)
	require.Equal(t, engine.NameQBittorrent, cands[1].Engine)
}

// TestEvaluateWritesNothing: the acceptance criterion — every table a rule
// run could touch holds the same row count afterwards.
func TestEvaluateWritesNothing(t *testing.T) {
	db := newTestDB(t)

	counts := func() map[string]int {
		out := map[string]int{}
		for _, table := range []string{"rules", "rule_matches", "rule_seen_episodes", "feed_items", "tasks"} {
			var n int
			require.NoError(t, db.GetContext(t.Context(), &n,
				"SELECT COUNT(*) FROM "+table))
			out[table] = n
		}
		return out
	}
	before := counts()

	doc := RuleDoc{Match: MatchSpec{AnyOf: []string{"*"}}}
	doc.ApplyDefaults()
	_, _, err := Evaluate(t.Context(), doc, store.Rule{ID: testRuleID},
		[]store.FeedItem{matchItem("w1", "Ubuntu ISO")}, testFeedScope, emptyState(), testNow.UnixMilli())
	require.NoError(t, err)
	require.Equal(t, before, counts())
}

// TestResolveContest: doc 08 section 5 step 13 — the higher-scoring
// release wins the content_key group, the loser comes back in losers; a
// distinct key keeps its own winner.
func TestResolveContest(t *testing.T) {
	high := Candidate{Item: matchItem("h", "High"), ContentKey: "k1", Score: 40, FeedPriority: 0}
	low := Candidate{Item: matchItem("l", "Low"), ContentKey: "k1", Score: 10, FeedPriority: 0}
	other := Candidate{Item: matchItem("o", "Other"), ContentKey: "k2", Score: 5, FeedPriority: 0}

	winners, losers := Resolve([]Candidate{low, other, high})
	require.Len(t, winners, 2)
	require.Equal(t, "k1", winners[0].ContentKey)
	require.Equal(t, 40, winners[0].Score)
	require.Equal(t, "k2", winners[1].ContentKey)
	require.Len(t, losers, 1)
	require.Equal(t, "itm_l", losers[0].Item.ID)
	require.Equal(t, 10, losers[0].Score)
}

// TestResolveFeedPriorityAndRecency: the second and third sort keys —
// equal scores go to the lower-priority-number feed, then to the newer
// item.
func TestResolveFeedPriorityAndRecency(t *testing.T) {
	newer := Candidate{Item: withPublished(matchItem("n", "New"), testNow.UnixMilli()),
		ContentKey: "k", Score: 10, FeedPriority: 0}
	prioritised := Candidate{Item: withPublished(matchItem("p", "Pri"), testNow.UnixMilli()),
		ContentKey: "k", Score: 10, FeedPriority: -1}
	older := Candidate{Item: withPublished(matchItem("o", "Old"), testNow.Add(-time.Hour).UnixMilli()),
		ContentKey: "k", Score: 10, FeedPriority: 0}

	winners, losers := Resolve([]Candidate{older, newer, prioritised})
	require.Equal(t, "itm_p", winners[0].Item.ID)
	require.Len(t, losers, 2)
	require.Equal(t, "itm_n", losers[0].Item.ID) // newer of the two priority-0 losers
	require.Equal(t, "itm_o", losers[1].Item.ID)
}

// TestSmartDedupWithinRun: two notations of one episode in the same run
// share ep:<rule>:1x5, so step 13 picks one winner without any stored
// state.
func TestSmartDedupWithinRun(t *testing.T) {
	doc := RuleDoc{Episode: &EpisodeSpec{Smart: true}}
	doc.ApplyDefaults()
	items := []store.FeedItem{
		matchItem("e1", "Show.S01E05.1080p"),
		matchItem("e2", "Show 1x05 1080p"),
	}
	_, cands, err := Evaluate(t.Context(), doc, store.Rule{ID: testRuleID},
		items, testFeedScope, emptyState(), testNow.UnixMilli())
	require.NoError(t, err)
	require.Len(t, cands, 2)
	require.Equal(t, "ep:"+testRuleID+":1x5", cands[0].ContentKey)
	require.Equal(t, cands[0].ContentKey, cands[1].ContentKey)

	winners, losers := Resolve(cands)
	require.Len(t, winners, 1)
	require.Len(t, losers, 1)
}

// TestRepackVariantsDecision: the doc 08 section 6.4 table — a seen key
// rejects unless the title is a REPACK/PROPER, and a both-title stages both
// variants so neither can be grabbed later.
func TestRepackVariantsDecision(t *testing.T) {
	doc := RuleDoc{Episode: &EpisodeSpec{Smart: true}}
	doc.ApplyDefaults()
	rule := store.Rule{ID: testRuleID}
	seen := map[string]bool{testRuleID + "\x00" + "1x5": true}

	// REPACK of a seen episode: accepted, staging 1x5-REPACK.
	_, cands, err := Evaluate(t.Context(), doc, rule,
		[]store.FeedItem{matchItem("v1", "Show.S01E05.REPACK.1080p")},
		testFeedScope, fakeState{episodes: seen}, testNow.UnixMilli())
	require.NoError(t, err)
	require.Len(t, cands, 1)
	require.Equal(t, "1x5-REPACK", cands[0].EpisodeKey)
	require.Equal(t, []string{"1x5-REPACK"}, cands[0].stagedKeys)

	// Same variant already stored: duplicate_episode again.
	d := evalOne(t, doc, rule, matchItem("v2", "Show.S01E05.REPACK.1080p"),
		fakeState{episodes: map[string]bool{
			testRuleID + "\x00" + "1x5":        true,
			testRuleID + "\x00" + "1x5-REPACK": true,
		}})
	require.Equal(t, ReasonDuplicateEpisode, d.Reason)
	require.Equal(t, `episode_key "1x5-REPACK" already seen for this rule`, d.ReasonDetail)

	// A plain re-release of a seen episode: duplicate_episode.
	d = evalOne(t, doc, rule, matchItem("v3", "Show.S01E05.720p"), fakeState{episodes: seen})
	require.Equal(t, ReasonDuplicateEpisode, d.Reason)

	// REPACK+PROPER in one title stages both variants.
	_, cands, err = Evaluate(t.Context(), doc, rule,
		[]store.FeedItem{matchItem("v4", "Show.S01E05.REPACK.PROPER.1080p")},
		testFeedScope, fakeState{episodes: seen}, testNow.UnixMilli())
	require.NoError(t, err)
	require.Len(t, cands, 1)
	require.Equal(t, []string{"1x5-REPACK", "1x5-PROPER"}, cands[0].stagedKeys)
}

// TestFeedScopeAndDateFloorRemoveSilently: steps 1 and 3 produce no
// Decision at all.
func TestFeedScopeAndDateFloorRemoveSilently(t *testing.T) {
	doc := RuleDoc{
		Feeds: []string{"https://example.com/other.xml"},
		Match: MatchSpec{PublishedAfter: "2027-01-01T00:00:00Z"},
	}
	doc.ApplyDefaults()
	decisions, cands, err := Evaluate(t.Context(), doc, store.Rule{ID: testRuleID},
		[]store.FeedItem{matchItem("f1", "Ubuntu ISO")}, testFeedScope, emptyState(), testNow.UnixMilli())
	require.NoError(t, err)
	require.Empty(t, decisions)
	require.Empty(t, cands)

	// The date floor alone also removes without a Decision.
	doc = RuleDoc{Match: MatchSpec{PublishedAfter: "2027-01-01T00:00:00Z"}}
	doc.ApplyDefaults()
	decisions, _, err = Evaluate(t.Context(), doc, store.Rule{ID: testRuleID},
		[]store.FeedItem{matchItem("f2", "Ubuntu ISO")}, testFeedScope, emptyState(), testNow.UnixMilli())
	require.NoError(t, err)
	require.Empty(t, decisions)
}

// TestMatchedByAndHighlight: a matched decision names the winning any_of
// entry and the byte span of its first token in the title.
func TestMatchedByAndHighlight(t *testing.T) {
	doc := RuleDoc{Match: MatchSpec{AnyOf: []string{"kubuntu", "ubuntu *desktop* amd64"}}}
	d := evalOne(t, doc, store.Rule{ID: testRuleID},
		matchItem("m1", "Ubuntu 26.04 Desktop amd64"), emptyState())
	require.True(t, d.Matched)
	require.Equal(t, map[string]string{"any_of": "ubuntu *desktop* amd64"}, d.MatchedBy)
	require.Equal(t, [2]int{0, 6}, d.Highlight) // "Ubuntu"
}

// TestStateErrorPropagates: a State failure aborts the pass rather than
// silently passing or rejecting the item.
func TestStateErrorPropagates(t *testing.T) {
	doc := RuleDoc{}
	doc.ApplyDefaults()
	_, _, err := Evaluate(t.Context(), doc, store.Rule{ID: testRuleID},
		[]store.FeedItem{withHash(matchItem("x1", "Ubuntu ISO"), "dcb9178653b651c7ca4526e11fa8e22f74e2fd7a")},
		testFeedScope, fakeState{err: fmt.Errorf("db gone")}, testNow.UnixMilli())
	require.Error(t, err)
}
