package rss

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/store"
)

// dryRunItem builds a stored feed_items row with a routable download URL,
// so a matching rule turns it into a candidate instead of an error.
func dryRunItem(feedID, identity, title string, publishedAt int64) store.FeedItem {
	return store.FeedItem{
		FeedID:      feedID,
		Identity:    identity,
		Title:       title,
		TitleNorm:   title,
		DownloadURL: strPtr("https://example.com/t/" + identity + ".torrent"),
		PublishedAt: ptrInt64(publishedAt),
	}
}

// seedDryRunItems stores items with a fixed clock so repeated reads are
// byte-identical.
func seedDryRunItems(t *testing.T, db *sqlx.DB, items []store.FeedItem) {
	t.Helper()

	_, err := store.UpsertFeedItems(t.Context(), db, items, testNow.UnixMilli())
	require.NoError(t, err)
}

// dryRunDoc is one valid document; mutate the returned copy to build the
// rejection cases.
func dryRunDoc() RuleDoc {
	doc := RuleDoc{
		Name:   "dry-run",
		Match:  MatchSpec{AnyOf: []string{"*ubuntu*"}},
		Action: ActionSpec{Destination: "/data/iso", Category: "linux", Paused: true},
	}
	doc.ApplyDefaults()

	return doc
}

// TestDryRunReturnsEveryEvaluatedItem pins the contract defect this
// endpoint exists to fix: 200 stored items yield evaluated ==
// matched + rejected == len(results) — every item, matched and unmatched.
func TestDryRunReturnsEveryEvaluatedItem(t *testing.T) {
	db := newTestDB(t)
	feed := newFeed(t, db, "https://example.com/dryrun.xml")

	items := make([]store.FeedItem, 200)
	for i := range items {
		title := fmt.Sprintf("debian release %d", i)
		if i%2 == 0 {
			title = fmt.Sprintf("ubuntu release %d", i)
		}
		items[i] = dryRunItem(feed.ID, fmt.Sprintf("item-%03d", i), title, testNow.UnixMilli()+int64(i))
	}
	seedDryRunItems(t, db, items)

	report, err := DryRun(t.Context(), db, DryRunRequest{
		Rule:        dryRunDoc(),
		FeedIDs:     []string{feed.ID},
		Limit:       200,
		IgnoreState: true,
	})
	require.NoError(t, err)
	require.Equal(t, 200, report.Evaluated)
	require.Len(t, report.Results, 200)

	rejected := 0
	for _, row := range report.Results {
		if row.Matched {
			require.NotNil(t, row.WouldDo)
			require.NotEmpty(t, row.MatchedBy)
			require.Empty(t, row.Reason, "matched_by and reason are never both present")
			require.Equal(t, "/data/iso", row.WouldDo.Destination)
			require.Equal(t, "linux", row.WouldDo.Category)
			require.True(t, row.WouldDo.Paused)
		} else {
			rejected++
			require.NotEmpty(t, row.Reason)
			require.NotEmpty(t, row.ReasonDetail)
			require.Nil(t, row.WouldDo)
			require.Empty(t, row.MatchedBy, "reason and matched_by are never both present")
		}
		require.Equal(t, feed.ID, row.FeedID)
		require.NotEmpty(t, row.Feed)
		require.NotNil(t, row.PublishedAt)
		require.NotEmpty(t, row.DownloadURL)
	}
	require.Equal(t, report.Matched, report.Evaluated-rejected)
	require.Equal(t, 100, report.Matched)
}

// TestDryRunReasonEnumIsClosed fails on any code outside the ten of doc 08
// section 5.1 — the UI switches on the enum, so an eleventh string is a
// contract breach.
func TestDryRunReasonEnumIsClosed(t *testing.T) {
	db := newTestDB(t)
	feed := newFeed(t, db, "https://example.com/closed.xml")

	threeGiB := int64(3 << 30)
	items := []store.FeedItem{
		dryRunItem(feed.ID, "excluded", "ubuntu daily build", 1),
		dryRunItem(feed.ID, "nomatch", "debian netinst", 2),
		dryRunItem(feed.ID, "toobig", "ubuntu studio", 3),
		dryRunItem(feed.ID, "toolow", "ubuntu server", 4),
	}
	items[2].SizeBytes = &threeGiB
	seedDryRunItems(t, db, items)

	doc := dryRunDoc()
	doc.Match.NoneOf = []string{"daily"}
	doc.Match.MaxSize = "1GiB"
	doc.Score = &ScoreSpec{Minimum: 10}
	report, err := DryRun(t.Context(), db, DryRunRequest{
		Rule:        doc,
		FeedIDs:     []string{feed.ID},
		IgnoreState: true,
	})
	require.NoError(t, err)
	require.Len(t, report.Results, 4)

	closed := map[string]bool{
		ReasonCooldown: true, ReasonExcluded: true, ReasonNoMatch: true,
		ReasonSize: true, ReasonEpisodeFilter: true, ReasonUnparseableEpisode: true,
		ReasonDuplicateEpisode: true, ReasonDuplicateInfoHash: true,
		ReasonAlreadyHave: true, ReasonBelowMinimumScore: true,
	}
	require.Len(t, closed, 10)
	reasons := map[string]string{}
	details := map[string]string{}
	for _, row := range report.Results {
		require.False(t, row.Matched)
		require.True(t, closed[row.Reason], "reason %q is outside the closed enum", row.Reason)
		reasons[row.Title] = row.Reason
		details[row.Title] = row.ReasonDetail
	}
	require.Equal(t, ReasonExcluded, reasons["ubuntu daily build"])
	require.Equal(t, `none_of[0] = "daily"`, details["ubuntu daily build"])
	require.Equal(t, ReasonNoMatch, reasons["debian netinst"])
	require.Equal(t, ReasonSize, reasons["ubuntu studio"])
	require.Equal(t, ReasonBelowMinimumScore, reasons["ubuntu server"])
}

// TestDryRunIsReproducibleWithIgnoreState pins doc 08 section 8: with
// ignore_state the report bypasses rule_matches, so two calls around a
// real grab row are identical. The ignore_state=false twin proves the row
// is visible only when state is honoured.
func TestDryRunIsReproducibleWithIgnoreState(t *testing.T) {
	db := newTestDB(t)
	feed := newFeed(t, db, "https://example.com/repro.xml")

	hash := "dcb9178653b651c7ca4526e11fa8e22f74e2fd7a"
	item := dryRunItem(feed.ID, "hashed", "ubuntu release", 1)
	item.InfoHash = &hash
	seedDryRunItems(t, db, []store.FeedItem{item})

	req := DryRunRequest{Rule: dryRunDoc(), FeedIDs: []string{feed.ID}, IgnoreState: true}
	first, err := DryRun(t.Context(), db, req)
	require.NoError(t, err)
	require.Len(t, first.Results, 1)
	require.True(t, first.Results[0].Matched)

	require.NoError(t, store.CreateRule(t.Context(), db, store.Rule{
		ID: testRuleID, Name: "grabbed", Enabled: true, DefinitionJSON: "{}",
	}))
	_, err = db.ExecContext(t.Context(),
		`INSERT INTO rule_matches
		(id, rule_id, info_hash, title, status, score, matched_at, created_at, updated_at)
		VALUES ('rm_repro', ?, ?, 'ubuntu release', 'sent', 0, ?, ?, ?)`,
		testRuleID, hash, testNow.UnixMilli(), testNow.UnixMilli(), testNow.UnixMilli())
	require.NoError(t, err)

	second, err := DryRun(t.Context(), db, req)
	require.NoError(t, err)

	// elapsed_ms is wall time, not state; everything else must be a
	// byte-for-byte repeat.
	first.ElapsedMS, second.ElapsedMS = 0, 0
	require.Empty(t, cmp.Diff(first, second))

	stateful, err := DryRun(t.Context(), db, DryRunRequest{
		Rule: req.Rule, FeedIDs: req.FeedIDs, IgnoreState: false,
	})
	require.NoError(t, err)
	require.Len(t, stateful.Results, 1)
	require.False(t, stateful.Results[0].Matched)
	require.Equal(t, ReasonDuplicateInfoHash, stateful.Results[0].Reason)
}

// TestDryRunWritesNothing pins the side-effect-free contract: the grab and
// seen-episode tables hold the same row counts afterwards, with the run
// reading the real schema (ignore_state=false).
func TestDryRunWritesNothing(t *testing.T) {
	db := newTestDB(t)
	feed := newFeed(t, db, "https://example.com/writes.xml")
	seedDryRunItems(t, db, []store.FeedItem{
		dryRunItem(feed.ID, "w1", "ubuntu release", 1),
	})

	counts := func() map[string]int {
		out := map[string]int{}
		for _, table := range []string{"rule_matches", "rule_seen_episodes"} {
			var n int
			require.NoError(t, db.GetContext(t.Context(), &n,
				"SELECT COUNT(*) FROM "+table))
			out[table] = n
		}
		return out
	}
	before := counts()

	doc := dryRunDoc()
	doc.Episode = &EpisodeSpec{Smart: true}
	_, err := DryRun(t.Context(), db, DryRunRequest{
		Rule: doc, FeedIDs: []string{feed.ID}, IgnoreState: false,
	})
	require.NoError(t, err)
	require.Equal(t, before, counts())
}

// TestDryRunLimitIsPerFeed pins the limit semantics of doc 05 section
// 10.3: items per feed, newest first — two feeds at limit 5 return ten
// rows, the five newest of each.
func TestDryRunLimitIsPerFeed(t *testing.T) {
	db := newTestDB(t)
	feedA := newFeed(t, db, "https://example.com/a.xml")
	feedB := newFeed(t, db, "https://example.com/b.xml")

	for i := range 8 {
		seedDryRunItems(t, db, []store.FeedItem{
			dryRunItem(feedA.ID, fmt.Sprintf("a-%d", i), fmt.Sprintf("ubuntu a%d", i), int64(i)),
			dryRunItem(feedB.ID, fmt.Sprintf("b-%d", i), fmt.Sprintf("ubuntu b%d", i), int64(i)),
		})
	}

	report, err := DryRun(t.Context(), db, DryRunRequest{
		Rule:        dryRunDoc(),
		FeedIDs:     []string{feedA.ID, feedB.ID},
		Limit:       5,
		IgnoreState: true,
	})
	require.NoError(t, err)
	require.Equal(t, 10, report.Evaluated)

	perFeed := map[string][]string{}
	for _, row := range report.Results {
		perFeed[row.FeedID] = append(perFeed[row.FeedID], row.Title)
	}
	require.Equal(t, 5, len(perFeed[feedA.ID]))
	require.Equal(t, 5, len(perFeed[feedB.ID]))
	// The five newest of each feed: items a3..a7 and b3..b7.
	for _, suffix := range []string{"3", "4", "5", "6", "7"} {
		require.Contains(t, perFeed[feedA.ID], "ubuntu a"+suffix)
		require.Contains(t, perFeed[feedB.ID], "ubuntu b"+suffix)
	}
}

// TestDryRunUnknownFeedIsNotFound pins the 404 of doc 05 section 10.3: a
// named feed id that addresses no row is ErrNotFound.
func TestDryRunUnknownFeedIsNotFound(t *testing.T) {
	db := newTestDB(t)

	_, err := DryRun(t.Context(), db, DryRunRequest{
		Rule:        dryRunDoc(),
		FeedIDs:     []string{"fed_does_not_exist"},
		IgnoreState: true,
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, store.ErrNotFound))
}

// TestDryRunFeedSelection pins the default feed set of doc 05 section
// 10.3: no feeds member means the rule's own feed URLs, and an empty
// rule.feeds means every enabled feed — a disabled feed contributes
// nothing.
func TestDryRunFeedSelection(t *testing.T) {
	db := newTestDB(t)
	enabled := newFeed(t, db, "https://example.com/enabled.xml")
	disabled := newFeed(t, db, "https://example.com/disabled.xml")
	disabled.Enabled = false
	require.NoError(t, store.UpdateFeed(t.Context(), db, disabled))

	seedDryRunItems(t, db, []store.FeedItem{
		dryRunItem(enabled.ID, "e1", "ubuntu one", 1),
		dryRunItem(disabled.ID, "d1", "ubuntu two", 2),
	})

	// An empty rule.feeds reads every enabled feed only.
	report, err := DryRun(t.Context(), db, DryRunRequest{
		Rule: dryRunDoc(), IgnoreState: true,
	})
	require.NoError(t, err)
	require.Equal(t, 1, report.Evaluated)
	require.Equal(t, enabled.ID, report.Results[0].FeedID)

	// rule.feeds scopes by URL: the disabled feed is still excluded here
	// because its URL is not listed.
	doc := dryRunDoc()
	doc.Feeds = []string{enabled.URL}
	report, err = DryRun(t.Context(), db, DryRunRequest{
		Rule: doc, IgnoreState: true,
	})
	require.NoError(t, err)
	require.Equal(t, 1, report.Evaluated)
	require.Equal(t, enabled.ID, report.Results[0].FeedID)

	// A named feed id selects the feed directly — even a disabled one —
	// while the rule's own feed scope still applies at evaluation step 1.
	report, err = DryRun(t.Context(), db, DryRunRequest{
		Rule: dryRunDoc(), FeedIDs: []string{disabled.ID}, IgnoreState: true,
	})
	require.NoError(t, err)
	require.Equal(t, 1, report.Evaluated)
	require.Equal(t, disabled.ID, report.Results[0].FeedID)
}

// TestDryRunPagesPastTheStorePageLimit exercises the cursor continuation
// of newestFeedItems: a limit above the 200-row store page must collect
// across pages without losing or repeating an item.
func TestDryRunPagesPastTheStorePageLimit(t *testing.T) {
	db := newTestDB(t)
	feed := newFeed(t, db, "https://example.com/pages.xml")

	items := make([]store.FeedItem, 250)
	for i := range items {
		items[i] = dryRunItem(feed.ID, fmt.Sprintf("p-%03d", i),
			fmt.Sprintf("ubuntu p%d", i), int64(i))
	}
	seedDryRunItems(t, db, items)

	report, err := DryRun(t.Context(), db, DryRunRequest{
		Rule:        dryRunDoc(),
		FeedIDs:     []string{feed.ID},
		Limit:       250,
		IgnoreState: true,
	})
	require.NoError(t, err)
	require.Equal(t, 250, report.Evaluated)
	require.Len(t, report.Results, 250)
	// Every seeded item exactly once: a boundary skip or a mid-page
	// duplicate would both pass the endpoint-only assertions.
	seen := make(map[string]bool, len(report.Results))
	for _, row := range report.Results {
		require.False(t, seen[row.Title], "item %q appears more than once", row.Title)
		seen[row.Title] = true
	}
	require.Len(t, seen, 250)
	// Newest first across the merged pages.
	require.Equal(t, "ubuntu p249", report.Results[0].Title)
	require.Equal(t, "ubuntu p0", report.Results[249].Title)
}

// TestDryRunItemWithoutDownloadURL pins the store-invariant breach: the
// parser only stores items carrying a download URI, so a NULL download_url
// row aborts the run with an error rather than vanishing or matching —
// the same contract TestUnroutableItemErrors pins on Evaluate.
func TestDryRunItemWithoutDownloadURL(t *testing.T) {
	db := newTestDB(t)
	feed := newFeed(t, db, "https://example.com/nourl.xml")
	item := dryRunItem(feed.ID, "nourl", "ubuntu release", 1)
	item.DownloadURL = nil
	seedDryRunItems(t, db, []store.FeedItem{item})

	_, err := DryRun(t.Context(), db, DryRunRequest{
		Rule: dryRunDoc(), FeedIDs: []string{feed.ID}, IgnoreState: true,
	})
	require.Error(t, err)
}
