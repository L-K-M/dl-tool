package rss

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

// recordingCreator is the TaskCreator stand-in: it records every
// GrabRequest in order and creates a real tasks row per call — the
// rule_matches.task_id foreign key is enforced — or answers the canned
// error.
type recordingCreator struct {
	tasks *store.TaskStore

	mu    sync.Mutex
	grabs []GrabRequest
	err   error
}

func (c *recordingCreator) CreateForRule(ctx context.Context, g GrabRequest) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.grabs = append(c.grabs, g)
	if c.err != nil {
		return "", c.err
	}

	task, err := c.tasks.Create(ctx, store.Task{
		Engine:      "qbittorrent",
		SourceKind:  "torrent",
		Name:        "grabbed " + g.FeedItemID,
		Destination: "/data",
	})
	if err != nil {
		return "", err
	}

	return task.ID, nil
}

func (c *recordingCreator) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.grabs)
}

// seedRule stores one rule row; the document is marshalled exactly like
// the save path stores it — ApplyDefaults first, compact JSON.
func seedRule(t *testing.T, db *sqlx.DB, doc RuleDoc) store.Rule {
	t.Helper()

	doc.ApplyDefaults()
	raw, err := json.Marshal(doc)
	require.NoError(t, err)

	rule := store.Rule{
		ID:             store.NewID(store.PrefixRule),
		Name:           "rule-" + store.NewID(""),
		Enabled:        true,
		DefinitionJSON: string(raw),
	}
	require.NoError(t, store.CreateRule(t.Context(), db, rule))

	return rule
}

// grabItem is one feed_items row with a routable download URL — the same
// shape the parser stores.
func grabItem(feedID, identity, title string, published int64) store.FeedItem {
	return store.FeedItem{
		FeedID:      feedID,
		Identity:    identity,
		Title:       title,
		TitleNorm:   title,
		DownloadURL: strPtr("https://example.com/t/" + identity + ".torrent"),
		PublishedAt: ptrInt64(published),
	}
}

func seedItems(t *testing.T, db *sqlx.DB, items ...store.FeedItem) {
	t.Helper()

	_, err := store.UpsertFeedItems(t.Context(), db, items, testNow.UnixMilli())
	require.NoError(t, err)
}

// matchRows reads the rule_matches table in insert order.
func matchRows(t *testing.T, db *sqlx.DB) []struct {
	RuleID     string  `db:"rule_id"`
	FeedItemID *string `db:"feed_item_id"`
	TaskID     *string `db:"task_id"`
	InfoHash   *string `db:"info_hash"`
	ContentKey *string `db:"content_key"`
	Status     string  `db:"status"`
	Reason     *string `db:"reason"`
	Score      int     `db:"score"`
} {
	t.Helper()

	var rows []struct {
		RuleID     string  `db:"rule_id"`
		FeedItemID *string `db:"feed_item_id"`
		TaskID     *string `db:"task_id"`
		InfoHash   *string `db:"info_hash"`
		ContentKey *string `db:"content_key"`
		Status     string  `db:"status"`
		Reason     *string `db:"reason"`
		Score      int     `db:"score"`
	}
	require.NoError(t, db.SelectContext(t.Context(), &rows,
		`SELECT rule_id, feed_item_id, task_id, info_hash, content_key, status, reason, score
		 FROM rule_matches ORDER BY created_at, id`))

	return rows
}

func seenKeys(t *testing.T, db *sqlx.DB, ruleID string) []string {
	t.Helper()

	var keys []string
	require.NoError(t, db.SelectContext(t.Context(), &keys,
		`SELECT episode_key FROM rule_seen_episodes WHERE rule_id = ? ORDER BY episode_key`, ruleID))

	return keys
}

func ruleByID(t *testing.T, db *sqlx.DB, id string) store.Rule {
	t.Helper()

	rule, err := store.RuleByID(t.Context(), db, id)
	require.NoError(t, err)

	return rule
}

// TestRunRuleReportsEvaluatedAndGrabbed: twenty stored items and a rule
// matching three give evaluated=20, matched=3 and three created tasks —
// the row of docs/05 section 10.2 the endpoint reports verbatim.
func TestRunRuleReportsEvaluatedAndGrabbed(t *testing.T) {
	db := newTestDB(t)
	feed := newFeed(t, db, testFeedURL)
	now := testNow.UnixMilli()

	items := make([]store.FeedItem, 20)
	for i := range items {
		title := fmt.Sprintf("unrelated item %d", i)
		if i < 3 {
			title = fmt.Sprintf("ubuntu desktop %d", i)
		}
		items[i] = grabItem(feed.ID, fmt.Sprintf("id-%d", i), title, now+int64(i))
	}
	seedItems(t, db, items...)

	rule := seedRule(t, db, RuleDoc{Match: MatchSpec{AnyOf: []string{"*desktop*"}}})
	creator := &recordingCreator{tasks: store.NewTaskStore(db)}

	// The run clock is an hour past the newest published_at, so the
	// watermark assertion below can only pass if last_match_at comes from
	// the last winner's published_at rather than the run's now.
	report, err := RunRule(t.Context(), db, rule.ID, 0, creator, now+int64(time.Hour/time.Millisecond))
	require.NoError(t, err)
	require.Equal(t, 20, report.Evaluated)
	require.Equal(t, 3, report.Matched)
	require.Equal(t, 0, report.Fallbacks)
	require.Len(t, report.CreatedTaskIDs, 3)
	require.Equal(t, 3, creator.count())

	// Every grab carried the rule's identity and the item's download URL.
	for _, grab := range creator.grabs {
		require.Equal(t, rule.ID, grab.RuleID)
		require.Contains(t, grab.URI, "example.com/t/")
	}

	rows := matchRows(t, db)
	require.Len(t, rows, 3)
	for _, row := range rows {
		require.Equal(t, rule.ID, row.RuleID)
		require.Equal(t, matchSent, row.Status)
		require.NotNil(t, row.TaskID)
		require.NotNil(t, row.FeedItemID)
	}

	// The watermark landed on the last winner's published_at: the winners
	// sort published-DESC, so the last committed is the oldest matched.
	require.NotNil(t, ruleByID(t, db, rule.ID).LastMatchAt)
	require.Equal(t, now, *ruleByID(t, db, rule.ID).LastMatchAt)
}

// TestGrabCarriesRuleTags pins the action.tags hand-off of doc 04 section
// 5: a committed grab forwards the rule's tag list to the task creator so
// the created task carries them verbatim.
func TestGrabCarriesRuleTags(t *testing.T) {
	db := newTestDB(t)
	feed := newFeed(t, db, testFeedURL)
	now := testNow.UnixMilli()

	seedItems(t, db, grabItem(feed.ID, "id-t", "ubuntu desktop tagged", now))
	rule := seedRule(t, db, RuleDoc{
		Action: ActionSpec{Tags: []string{"iso", "linux"}},
	})
	creator := &recordingCreator{tasks: store.NewTaskStore(db)}

	report, err := RunRule(t.Context(), db, rule.ID, 0, creator, now)
	require.NoError(t, err)
	require.Len(t, report.CreatedTaskIDs, 1)
	require.Len(t, creator.grabs, 1)
	require.Equal(t, []string{"iso", "linux"}, creator.grabs[0].Tags)
}

// TestContentKeyContestCreatesOneTaskAndOneFallback: two releases sharing a
// content_key — here the same info hash — commit one task and one
// 'fallback' row, the runner-up a later run can retry with.
func TestContentKeyContestCreatesOneTaskAndOneFallback(t *testing.T) {
	db := newTestDB(t)
	feed := newFeed(t, db, testFeedURL)
	now := testNow.UnixMilli()

	hash := "dcb9178653b651c7ca4526e11fa8e22f74e2fd7a"
	better := withHash(grabItem(feed.ID, "id-a", "release A", now+100), hash)
	worse := withHash(grabItem(feed.ID, "id-b", "release B", now), hash)
	seedItems(t, db, better, worse)

	rule := seedRule(t, db, RuleDoc{})
	creator := &recordingCreator{tasks: store.NewTaskStore(db)}

	report, err := RunRule(t.Context(), db, rule.ID, 0, creator, now)
	require.NoError(t, err)
	require.Equal(t, 2, report.Matched)
	require.Equal(t, 1, report.Fallbacks)
	require.Len(t, report.CreatedTaskIDs, 1)
	require.Equal(t, 1, creator.count())

	rows := matchRows(t, db)
	require.Len(t, rows, 2)
	statuses := map[string]string{} // status -> feed_item_id
	for _, row := range rows {
		statuses[row.Status] = *row.FeedItemID
		require.Equal(t, hash, *row.ContentKey)
	}
	require.Len(t, statuses, 2, "one sent row and one fallback row")
	// Only the grab carries the hash; the fallback must not hold it — the
	// unique partial index on info_hash forbids a second non-NULL row.
	var hashes int
	for _, row := range rows {
		if row.InfoHash != nil {
			hashes++
			require.Equal(t, matchSent, row.Status)
		}
	}
	require.Equal(t, 1, hashes)
}

// TestMaxPerRunCaps: throttle.max_per_run=1 commits exactly one task of
// the three matched items; the uncapped winners leave no row, so the next
// run grabs the next release.
func TestMaxPerRunCaps(t *testing.T) {
	db := newTestDB(t)
	feed := newFeed(t, db, testFeedURL)
	now := testNow.UnixMilli()

	seedItems(t, db,
		grabItem(feed.ID, "id-1", "release one", now+300),
		grabItem(feed.ID, "id-2", "release two", now+200),
		grabItem(feed.ID, "id-3", "release three", now+100),
	)
	rule := seedRule(t, db, RuleDoc{Throttle: ThrottleSpec{MaxPerRun: 1}})
	creator := &recordingCreator{tasks: store.NewTaskStore(db)}

	report, err := RunRule(t.Context(), db, rule.ID, 0, creator, now)
	require.NoError(t, err)
	require.Equal(t, 3, report.Matched)
	require.Len(t, report.CreatedTaskIDs, 1)
	require.Equal(t, 1, creator.count())
	require.Len(t, matchRows(t, db), 1)

	// The second run spends the next slot on the next-best winner — the
	// first grab's already_have dedup keeps it from re-grabbing.
	report, err = RunRule(t.Context(), db, rule.ID, 0, creator, now)
	require.NoError(t, err)
	require.Len(t, report.CreatedTaskIDs, 1)
	require.Equal(t, 2, creator.count())
	require.Len(t, matchRows(t, db), 2)
}

// TestDuplicateInfoHashAcrossFeeds: the same info hash arriving through
// two feeds creates one task — the contest picks the winner — and a
// second run creates nothing, the hash now committed.
func TestDuplicateInfoHashAcrossFeeds(t *testing.T) {
	db := newTestDB(t)
	feedA := newFeed(t, db, "https://example.com/a.xml")
	feedB := newFeed(t, db, "https://example.com/b.xml")
	now := testNow.UnixMilli()

	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	seedItems(t, db,
		withHash(grabItem(feedA.ID, "id-a", "release on feed A", now), hash),
		withHash(grabItem(feedB.ID, "id-b", "release on feed B", now), hash),
	)
	rule := seedRule(t, db, RuleDoc{})
	creator := &recordingCreator{tasks: store.NewTaskStore(db)}

	report, err := RunRule(t.Context(), db, rule.ID, 0, creator, now)
	require.NoError(t, err)
	require.Equal(t, 2, report.Matched)
	require.Len(t, report.CreatedTaskIDs, 1)

	report, err = RunRule(t.Context(), db, rule.ID, 0, creator, now)
	require.NoError(t, err)
	require.Equal(t, 0, report.Matched)
	require.Empty(t, report.CreatedTaskIDs)
	require.Equal(t, 1, creator.count())
}

// TestFailedHandoffDoesNotStageEpisodeKey: a CreateForRule error leaves
// status='failed' with the error text, stages no episode key and advances
// no watermark — the item re-enters and is retried on the next run.
func TestFailedHandoffDoesNotStageEpisodeKey(t *testing.T) {
	db := newTestDB(t)
	feed := newFeed(t, db, testFeedURL)
	now := testNow.UnixMilli()

	seedItems(t, db, grabItem(feed.ID, "id-e", "Show.S01E05.1080p", now))
	rule := seedRule(t, db, RuleDoc{Episode: &EpisodeSpec{Smart: true}})
	creator := &recordingCreator{tasks: store.NewTaskStore(db), err: errors.New("engine refused the grab")}

	report, err := RunRule(t.Context(), db, rule.ID, 0, creator, now)
	require.NoError(t, err)
	require.Equal(t, 1, report.Matched)
	require.Empty(t, report.CreatedTaskIDs)

	rows := matchRows(t, db)
	require.Len(t, rows, 1)
	require.Equal(t, matchFailed, rows[0].Status)
	require.NotNil(t, rows[0].Reason)
	require.Contains(t, *rows[0].Reason, "engine refused")
	require.Empty(t, seenKeys(t, db, rule.ID))
	require.Nil(t, ruleByID(t, db, rule.ID).LastMatchAt)

	// The retry — the error cleared — grabs the same row in place and
	// stages the episode key.
	creator.err = nil
	report, err = RunRule(t.Context(), db, rule.ID, 0, creator, now)
	require.NoError(t, err)
	require.Len(t, report.CreatedTaskIDs, 1)
	require.Len(t, matchRows(t, db), 1)
	require.Equal(t, []string{"1x5"}, seenKeys(t, db, rule.ID))
	require.NotNil(t, ruleByID(t, db, rule.ID).LastMatchAt)
}

// TestSecondRunIsIdempotent: a second RunRule over unchanged items creates
// no task and no second rule_matches row — the committed rows themselves
// are the dedup state.
func TestSecondRunIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	feed := newFeed(t, db, testFeedURL)
	now := testNow.UnixMilli()

	seedItems(t, db,
		grabItem(feed.ID, "id-1", "release one", now+100),
		grabItem(feed.ID, "id-2", "release two", now),
	)
	rule := seedRule(t, db, RuleDoc{})
	creator := &recordingCreator{tasks: store.NewTaskStore(db)}

	report, err := RunRule(t.Context(), db, rule.ID, 0, creator, now)
	require.NoError(t, err)
	require.Len(t, report.CreatedTaskIDs, 2)
	require.Len(t, matchRows(t, db), 2)

	report, err = RunRule(t.Context(), db, rule.ID, 0, creator, now)
	require.NoError(t, err)
	require.Equal(t, 0, report.Matched)
	require.Empty(t, report.CreatedTaskIDs)
	require.Equal(t, 2, creator.count())
	require.Len(t, matchRows(t, db), 2)
}

// cancelFirstCreator cancels the poll context during the first grab, then
// delegates: the stand-in for a refresh client that walks away mid-pass.
type cancelFirstCreator struct {
	recordingCreator
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelFirstCreator) CreateForRule(ctx context.Context, g GrabRequest) (string, error) {
	c.once.Do(c.cancel)

	return c.recordingCreator.CreateForRule(ctx, g)
}

// TestMalformedItemDoesNotWedgeRulePass pins the regression: a stored
// download_url that fails normalisation is remote feed content, and one
// bad enclosure beside valid items must neither abort the pass nor stop
// the valid grabs from committing.
func TestMalformedItemDoesNotWedgeRulePass(t *testing.T) {
	db := newTestDB(t)
	feed := newFeed(t, db, testFeedURL)
	now := testNow.UnixMilli()

	bad := grabItem(feed.ID, "id-bad", "release bad", now+100)
	bad.DownloadURL = strPtr("ed2k://|file|release bad|1|0123456789abcdef0123456789abcdef|/")
	seedItems(t, db,
		grabItem(feed.ID, "id-1", "release one", now+200),
		bad,
		grabItem(feed.ID, "id-2", "release two", now),
	)
	seedRule(t, db, RuleDoc{})
	creator := &recordingCreator{tasks: store.NewTaskStore(db)}

	require.NoError(t, RunAllRules(t.Context(), db, feed.ID, creator, now))
	require.Equal(t, 2, creator.count(), "the valid items still commit their grabs")
	require.Len(t, matchRows(t, db), 2)
}

// TestPollRulePassSurvivesPollContextCancel: the rule pass commits grabs
// the fetch already paid for, and recordSuccess has stored the validators
// that make the next poll answer 304 — so a poll context cancelled mid-pass
// (a refresh client gone, a shutdown boundary) must not abort the hand-offs,
// or the stored items sit unevaluated until the feed changes again.
func TestPollRulePassSurvivesPollContextCancel(t *testing.T) {
	db := newTestDB(t)

	const etag = `"v1"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	feed := newFeed(t, db, srv.URL+"/feed")
	seedRule(t, db, RuleDoc{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	creator := &cancelFirstCreator{
		recordingCreator: recordingCreator{tasks: store.NewTaskStore(db)},
		cancel:           cancel,
	}
	parser := stubParser{items: []store.FeedItem{
		grabItem(feed.ID, "id-1", "release one", testNow.UnixMilli()),
		grabItem(feed.ID, "id-2", "release two", testNow.UnixMilli()),
	}}

	origin, err := url.Parse(srv.URL)
	require.NoError(t, err)
	client := secure.NewClient(secure.NewGuard(discardLog(), true).ForOrigin(origin))
	client.Timeout = 10 * time.Second
	poller := NewPoller(db, client, parser, creator, discardLog(), func() time.Time { return testNow })
	poller.startedAt = testNow.Add(-time.Hour)

	res, err := poller.Poll(ctx, feed, false)
	require.NoError(t, err)
	require.Equal(t, 2, res.ItemsAdded)
	require.Equal(t, 2, creator.count(), "the pass must finish its grabs after the poll context ends")

	rows := matchRows(t, db)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.Equal(t, matchSent, row.Status,
			"a cancelled poll context must not strand a grab at %q", row.Status)
	}
}

// TestPoll304RunsNoRule: a 304 poll runs no rule — the creator sees no
// new grab — while the 200 that added the item did run the pass.
func TestPoll304RunsNoRule(t *testing.T) {
	db := newTestDB(t)

	const etag = `"v1"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	feed := newFeed(t, db, srv.URL+"/feed")
	rule := seedRule(t, db, RuleDoc{})
	creator := &recordingCreator{tasks: store.NewTaskStore(db)}
	parser := stubParser{items: []store.FeedItem{
		grabItem(feed.ID, "id-1", "release one", testNow.UnixMilli()),
	}}

	origin, err := url.Parse(srv.URL)
	require.NoError(t, err)
	client := secure.NewClient(secure.NewGuard(discardLog(), true).ForOrigin(origin))
	client.Timeout = 10 * time.Second
	poller := NewPoller(db, client, parser, creator, discardLog(), func() time.Time { return testNow })
	poller.startedAt = testNow.Add(-time.Hour)

	res, err := poller.Poll(t.Context(), feed, false)
	require.NoError(t, err)
	require.True(t, res.Fetched)
	require.Equal(t, 1, res.ItemsAdded)
	require.Equal(t, 1, creator.count(), "the 200 that added an item runs the rules pass")
	require.Len(t, matchRows(t, db), 1)

	res, err = poller.Poll(t.Context(), feedByID(t, db, feed.ID), false)
	require.NoError(t, err)
	require.True(t, res.NotModified)
	require.Equal(t, 1, creator.count(), "a 304 runs no rule")

	// And the committed match names the rule for matched_rules.
	rows := matchRows(t, db)
	require.Equal(t, rule.ID, rows[0].RuleID)
	require.Equal(t, matchSent, rows[0].Status)
}
