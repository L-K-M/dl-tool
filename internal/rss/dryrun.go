package rss

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/store"
)

// The bounds of docs/05-api-contract.md section 10.3: the handler enforces
// them through the request schema and DryRun re-checks them for every other
// caller. dryRunPageLimit is the widest page store.ListFeedItems serves
// (feedItemsMaxLimit), so a 500-item limit takes three pages.
const (
	dryRunDefaultLimit = 200
	dryRunMaxLimit     = 500
	dryRunPageLimit    = 200
)

// DryRunRequest mirrors the body of POST /rules/test. FeedIDs empty means
// the rule's own feeds, and an empty rule.feeds means every enabled feed.
// Limit is items per feed, newest first.
type DryRunRequest struct {
	Rule        RuleDoc
	FeedIDs     []string
	Limit       int  // default 200, range 1..500
	IgnoreState bool // default true
}

// DryRunItem is one row of results[]. MatchedBy is present only when
// Matched is true; Reason and ReasonDetail only when it is false.
type DryRunItem struct {
	FeedID       string            `json:"feed_id"`
	Feed         string            `json:"feed"`
	Title        string            `json:"title"`
	PublishedAt  *string           `json:"published_at"`
	DownloadURL  string            `json:"download_url"`
	Matched      bool              `json:"matched"`
	Score        int               `json:"score,omitempty"`
	MatchedBy    map[string]string `json:"matched_by,omitempty"`
	WouldDo      *WouldDo          `json:"would_do,omitempty"`
	Reason       string            `json:"reason,omitempty"`
	ReasonDetail string            `json:"reason_detail,omitempty"`
}

// WouldDo restates the rule's action for a matched item; it is the
// preview, never a promise.
type WouldDo struct {
	Destination string `json:"destination"`
	Category    string `json:"category"`
	Paused      bool   `json:"paused"`
}

// DryRunReport is the POST /rules/test body: every evaluated item, the
// counters and the evaluation wall time so a pathological regex is
// visible at once (docs/05-api-contract.md section 10.3).
type DryRunReport struct {
	Evaluated int          `json:"evaluated"`
	Matched   int          `json:"matched"`
	ElapsedMS int64        `json:"elapsed_ms"`
	Results   []DryRunItem `json:"results"`
}

// StatelessState is the State implementation used when IgnoreState is
// true: every lookup answers "not seen", so repeated calls over an
// unchanged item set are identical.
type StatelessState struct{}

// HasInfoHash always answers not-seen.
func (StatelessState) HasInfoHash(context.Context, string) (bool, error) { return false, nil }

// SeenEpisode always answers not-seen.
func (StatelessState) SeenEpisode(context.Context, string, string) (bool, error) {
	return false, nil
}

// BestScoreForContentKey always answers not-seen.
func (StatelessState) BestScoreForContentKey(context.Context, string) (int, bool, error) {
	return 0, false, nil
}

// dryRunState is the State of an ignore_state=false dry run: the real
// rule_matches and rule_seen_episodes tables, read-only. The rule under
// test is unsaved, so its SeenEpisode lookups carry an empty rule id and
// can never hit a committed row.
type dryRunState struct{ db *sqlx.DB }

// HasInfoHash reports whether the grab table already holds the hash.
func (s dryRunState) HasInfoHash(ctx context.Context, hash string) (bool, error) {
	var n int
	if err := s.db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM rule_matches WHERE info_hash = ?`, hash); err != nil {
		return false, fmt.Errorf("rss: dry run: lookup info hash: %w", err)
	}

	return n > 0, nil
}

// SeenEpisode reports whether the rule already stored the episode key.
func (s dryRunState) SeenEpisode(ctx context.Context, ruleID, key string) (bool, error) {
	var n int
	if err := s.db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM rule_seen_episodes WHERE rule_id = ? AND episode_key = ?`,
		ruleID, key); err != nil {
		return false, fmt.Errorf("rss: dry run: lookup episode key: %w", err)
	}

	return n > 0, nil
}

// BestScoreForContentKey returns the score of the newest stored grab for
// the key, ok=false when there is none.
func (s dryRunState) BestScoreForContentKey(ctx context.Context, key string) (int, bool, error) {
	var score int
	err := s.db.GetContext(ctx, &score,
		`SELECT score FROM rule_matches WHERE content_key = ? ORDER BY matched_at DESC LIMIT 1`, key)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("rss: dry run: lookup content key: %w", err)
	}

	return score, true, nil
}

// DryRun selects the items, runs Evaluate and builds the report. It opens
// no transaction and writes nothing. ErrNotFound is returned when a named
// feed id does not exist.
func DryRun(ctx context.Context, db *sqlx.DB, req DryRunRequest) (DryRunReport, error) {
	// Idempotent and safe on the value copy: a direct caller's bare
	// document gets the documented defaults the handler already applied.
	req.Rule.ApplyDefaults()

	limit := req.Limit
	if limit <= 0 {
		limit = dryRunDefaultLimit
	}
	if limit > dryRunMaxLimit {
		limit = dryRunMaxLimit
	}

	feeds, err := dryRunFeeds(ctx, db, req)
	if err != nil {
		return DryRunReport{}, err
	}

	feedByID := make(map[string]FeedRef, len(feeds))
	feedName := make(map[string]string, len(feeds))
	items := []store.FeedItem{}
	for _, feed := range feeds {
		feedByID[feed.ID] = FeedRef{URL: feed.URL, Priority: feed.Priority}
		if feed.Title != nil {
			feedName[feed.ID] = *feed.Title
		} else {
			// A feed that was never fetched has no title; the URL is the
			// only name the row can offer.
			feedName[feed.ID] = feed.URL
		}
		batch, err := newestFeedItems(ctx, db, feed.ID, limit)
		if err != nil {
			return DryRunReport{}, err
		}
		items = append(items, batch...)
	}

	// Merge the per-feed pages into one newest-first set, the order
	// ListFeedItems itself pages in, so the report is stable.
	slices.SortStableFunc(items, func(a, b store.FeedItem) int {
		if c := cmp.Compare(dryRunSortKey(b), dryRunSortKey(a)); c != 0 {
			return c
		}

		return cmp.Compare(b.ID, a.ID)
	})
	itemByID := make(map[string]store.FeedItem, len(items))
	for _, item := range items {
		itemByID[item.ID] = item
	}

	var state State = dryRunState{db: db}
	if req.IgnoreState {
		state = StatelessState{}
	}

	// The document is unsaved, so the synthesised rule carries no id and
	// no last_match_at: cooldown can never fire, episode keys namespace
	// under an empty rule id and a stored grab can only collide through
	// the info-hash and global content-key rungs — the honest answer to
	// "would this rule grab what an earlier run already took".
	start := time.Now()
	decisions, _, err := Evaluate(ctx, req.Rule, store.Rule{Name: req.Rule.Name},
		items, feedByID, state, start.UnixMilli())
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return DryRunReport{}, err
	}

	report := DryRunReport{
		ElapsedMS: elapsed,
		Results:   make([]DryRunItem, 0, len(decisions)),
	}
	for _, decision := range decisions {
		item := itemByID[decision.ItemID]
		row := DryRunItem{
			FeedID:      item.FeedID,
			Feed:        feedName[item.FeedID],
			Title:       item.Title,
			PublishedAt: unixMilliPtrRFC3339(item.PublishedAt),
			DownloadURL: stringOrEmpty(item.DownloadURL),
			Matched:     decision.Matched,
			Score:       decision.Score,
		}
		if decision.Matched {
			row.MatchedBy = decision.MatchedBy
			row.WouldDo = &WouldDo{
				Destination: req.Rule.Action.Destination,
				Category:    req.Rule.Action.Category,
				Paused:      req.Rule.Action.Paused,
			}
			report.Matched++
		} else {
			row.Reason = decision.Reason
			row.ReasonDetail = decision.ReasonDetail
		}
		report.Results = append(report.Results, row)
	}
	// Steps 1 and 3 of the algorithm remove items before evaluation, so
	// evaluated counts results only, not the stored item set.
	report.Evaluated = len(report.Results)

	return report, nil
}

// dryRunFeeds resolves the feed set of the request: the named ids — one
// ErrNotFound for an unknown id — else the rule's own feed URLs, else
// every enabled feed. A rule.feeds URL no stored feed carries selects
// nothing: step 1 of the algorithm scopes on URLs, so the dry run selects
// the same way rather than inventing an id lookup.
func dryRunFeeds(ctx context.Context, db *sqlx.DB, req DryRunRequest) ([]store.Feed, error) {
	feeds, err := store.ListFeeds(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("rss: dry run: list feeds: %w", err)
	}

	switch {
	case len(req.FeedIDs) > 0:
		byID := make(map[string]store.Feed, len(feeds))
		for _, feed := range feeds {
			byID[feed.ID] = feed
		}
		selected := make([]store.Feed, 0, len(req.FeedIDs))
		seen := make(map[string]bool, len(req.FeedIDs))
		for _, id := range req.FeedIDs {
			feed, ok := byID[id]
			if !ok {
				return nil, fmt.Errorf("rss: dry run: feed %s: %w", id, store.ErrNotFound)
			}
			// A repeated id would double-count the feed's items.
			if !seen[id] {
				seen[id] = true
				selected = append(selected, feed)
			}
		}

		return selected, nil
	case len(req.Rule.Feeds) > 0:
		wanted := make(map[string]bool, len(req.Rule.Feeds))
		for _, url := range req.Rule.Feeds {
			wanted[url] = true
		}
		var selected []store.Feed
		for _, feed := range feeds {
			if wanted[feed.URL] {
				selected = append(selected, feed)
			}
		}

		return selected, nil
	default:
		var enabled []store.Feed
		for _, feed := range feeds {
			if feed.Enabled {
				enabled = append(enabled, feed)
			}
		}

		return enabled, nil
	}
}

// newestFeedItems pages ListFeedItems until limit items are collected or
// the feed runs out — the list caps a page at dryRunPageLimit while a dry
// run may ask for 500.
func newestFeedItems(ctx context.Context, db *sqlx.DB, feedID string, limit int) ([]store.FeedItem, error) {
	items := []store.FeedItem{}
	cursor := ""
	for len(items) < limit {
		page := min(limit-len(items), dryRunPageLimit)
		batch, next, _, err := store.ListFeedItems(ctx, db, store.FeedItemFilter{
			FeedIDs: []string{feedID},
			Limit:   page,
			Cursor:  cursor,
		})
		if err != nil {
			return nil, fmt.Errorf("rss: dry run: list items of feed %s: %w", feedID, err)
		}
		items = append(items, batch...)
		if next == "" {
			break
		}
		cursor = next
	}

	return items, nil
}

// dryRunSortKey is the sortable timestamp of one item: published_at when
// the feed carried a date, first_seen_at otherwise — the COALESCE the
// store orders by.
func dryRunSortKey(item store.FeedItem) int64 {
	if item.PublishedAt != nil {
		return *item.PublishedAt
	}

	return item.FirstSeenAt
}

// unixMilliPtrRFC3339 renders a millisecond timestamp RFC 3339, nil for
// NULL.
func unixMilliPtrRFC3339(ms *int64) *string {
	if ms == nil {
		return nil
	}
	rendered := time.UnixMilli(*ms).UTC().Format(time.RFC3339)

	return &rendered
}

// stringOrEmpty dereferences an optional column to its wire form.
func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
