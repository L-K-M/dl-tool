package rss

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"

	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/store"
)

// TaskCreator hands a grabbed item to the ordinary task-creation path.
// internal/api implements it by calling TaskHandlers.CreateTasks, so a rule
// grab passes the same normalisation, routing, destination containment and
// concurrency checks a pasted URI passes.
type TaskCreator interface {
	CreateForRule(ctx context.Context, g GrabRequest) (taskID string, err error)
}

// GrabRequest is one accepted candidate, expressed in the vocabulary of
// POST /tasks.
type GrabRequest struct {
	URI           string
	Destination   string // rule action.destination, empty means the server default
	Category      string
	Tags          []string // rule action.tags; the create path makes them on demand
	Paused        bool
	ContentLayout string // original | subfolder | no_subfolder
	Engine        string // rule action.engine, empty means let the router decide
	RuleID        string
	FeedItemID    string
}

// CommitReport is what one rule produced in one cycle.
type CommitReport struct {
	Evaluated      int
	Matched        int
	CreatedTaskIDs []string
	Fallbacks      int
}

// The rule_matches.status values Commit writes (docs/04-data-model.md
// section 3.5). The schema's fifth value, 'rejected', is a dry-run
// outcome this table never stores.
const (
	matchQueued   = "queued"
	matchSent     = "sent"
	matchFailed   = "failed"
	matchFallback = "fallback"
)

// The hex widths of the two stored infohash shapes: a 40-hex value
// compares against tasks.infohash_v1, a 64-hex value against
// tasks.infohash_v2 — never truncated (docs/04-data-model.md section 3.5).
const (
	grabInfohashV1Len = 40
	grabInfohashV2Len = 64
)

// dbState is the State of T069 backed by rule_matches, rule_seen_episodes
// and tasks.
type dbState struct{ db *sqlx.DB }

// HasInfoHash reports whether the hash was already grabbed. A rule_matches
// row holds the hash only while it owns the content — the column is
// written on the 'sent' transition or back-filled by the metainfo parse —
// so a 'queued' crash remnant, a 'failed' hand-off or a 'fallback'
// runner-up can never wedge a retry. The tasks lookup mirrors the create
// path's duplicate rule: a 'removed' tombstone holds no identity.
func (s dbState) HasInfoHash(ctx context.Context, hash string) (bool, error) {
	var n int
	if err := s.db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM rule_matches WHERE info_hash = ?`, hash); err != nil {
		return false, fmt.Errorf("rss: lookup match info hash: %w", err)
	}
	if n > 0 {
		return true, nil
	}

	var query string
	switch len(hash) {
	case grabInfohashV1Len:
		query = `SELECT COUNT(*) FROM tasks WHERE infohash_v1 = ? AND state <> 'removed'`
	case grabInfohashV2Len:
		query = `SELECT COUNT(*) FROM tasks WHERE infohash_v2 = ? AND state <> 'removed'`
	default:
		// A value of neither stored width is no hash a task could carry;
		// the rule_matches answer stands alone.
		return false, nil
	}
	if err := s.db.GetContext(ctx, &n, query, hash); err != nil {
		return false, fmt.Errorf("rss: lookup task info hash: %w", err)
	}

	return n > 0, nil
}

// SeenEpisode reports whether the rule already stored the episode key.
func (s dbState) SeenEpisode(ctx context.Context, ruleID, key string) (bool, error) {
	var n int
	if err := s.db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM rule_seen_episodes WHERE rule_id = ? AND episode_key = ?`,
		ruleID, key); err != nil {
		return false, fmt.Errorf("rss: lookup episode key: %w", err)
	}

	return n > 0, nil
}

// BestScoreForContentKey returns the best score among the committed grabs
// of one content key — 'sent' rows only, because a grab that never became
// content cannot wedge the key into already_have: 'queued' is a crash
// remnant its own item must be allowed to retry, 'failed' never landed and
// 'fallback' was the runner-up.
func (s dbState) BestScoreForContentKey(ctx context.Context, key string) (int, bool, error) {
	var best sql.NullInt64
	if err := s.db.GetContext(ctx, &best,
		`SELECT MAX(score) FROM rule_matches WHERE content_key = ? AND status = ?`,
		key, matchSent); err != nil {
		return 0, false, fmt.Errorf("rss: lookup content key: %w", err)
	}
	if !best.Valid {
		return 0, false, nil
	}

	return int(best.Int64), true, nil
}

// Commit performs steps 13 and 14 of docs/08-rss-automation.md section 5
// for one rule: Resolve, then for each winner insert a rule_matches row at
// status 'queued' in its own transaction — a crash mid-grab leaves the row
// as evidence, never a silent loss — call the TaskCreator, and move the
// row to 'sent' with its task_id or to 'failed' with the error text.
// Losers are inserted as 'fallback', the winners' staged episode keys land
// in rule_seen_episodes, rules.last_match_at advances to the last grabbed
// winner's published_at, and throttle.max_per_run caps the successful
// grabs of one run when it is non-zero.
func Commit(ctx context.Context, db *sqlx.DB, doc RuleDoc, rule store.Rule,
	cands []Candidate, tc TaskCreator, now int64) (CommitReport, error) {

	winners, losers := Resolve(cands)
	report := CommitReport{Matched: len(cands), Fallbacks: len(losers)}

	// Every contested group's runner-up is the retry stock a later run
	// draws on when the winner's hand-off fails: the rows land before the
	// grabs so a crash mid-run loses nothing the evaluation committed.
	for _, loser := range losers {
		if _, err := upsertMatch(ctx, db, rule.ID, matchFallback, loser, now); err != nil {
			return report, fmt.Errorf("rss: commit rule %s: %w", rule.ID, err)
		}
	}

	sent := 0
	grabbed := false
	var lastPublished int64
	for _, winner := range winners {
		// max_per_run counts grabs only: a failed hand-off consumes no
		// slot, so a run whose engine dropped a submission still spends
		// the full allowance (docs/08-rss-automation.md section 5 step 14).
		if doc.Throttle.MaxPerRun > 0 && sent >= doc.Throttle.MaxPerRun {
			break
		}

		matchID, err := queueMatch(ctx, db, rule.ID, winner, now)
		if err != nil {
			return report, fmt.Errorf("rss: commit rule %s: %w", rule.ID, err)
		}
		taskID, err := tc.CreateForRule(ctx, GrabRequest{
			URI:           stringOrEmpty(winner.Item.DownloadURL),
			Destination:   doc.Action.Destination,
			Category:      doc.Action.Category,
			Tags:          doc.Action.Tags,
			Paused:        doc.Action.Paused,
			ContentLayout: doc.Action.ContentLayout,
			Engine:        doc.Action.Engine,
			RuleID:        rule.ID,
			FeedItemID:    winner.Item.ID,
		})
		if err != nil {
			// The grab stays retryable: 'failed' keeps no info_hash and
			// stages no episode key, so the next evaluation re-enters
			// the item instead of losing it to a transient refusal.
			if ferr := failMatch(ctx, db, matchID, err.Error(), now); ferr != nil {
				return report, fmt.Errorf("rss: commit rule %s: %w", rule.ID, errors.Join(ferr, err))
			}
			continue
		}
		if err := sendMatch(ctx, db, matchID, rule.ID, winner, taskID, now); err != nil {
			return report, fmt.Errorf("rss: commit rule %s: %w", rule.ID, err)
		}
		report.CreatedTaskIDs = append(report.CreatedTaskIDs, taskID)
		sent++
		grabbed = true
		lastPublished = publishedAt(winner)
	}

	// The dedup watermark advances on a committed grab only — a run whose
	// every hand-off failed re-evaluates the same window — and
	// SetRuleLastMatchAt's MAX keeps it monotonic across runs.
	if grabbed {
		if err := store.SetRuleLastMatchAt(ctx, db, rule.ID, lastPublished); err != nil {
			return report, fmt.Errorf("rss: commit rule %s: %w", rule.ID, err)
		}
	}

	return report, nil
}

// matchWriter is the SQL surface upsertMatch needs: *sqlx.DB and *sqlx.Tx
// both satisfy it, so the fallback write can run bare while the 'queued'
// write rides the per-candidate transaction.
type matchWriter interface {
	sqlx.ExtContext
	GetContext(ctx context.Context, dest any, query string, args ...any) error
}

// upsertMatch writes one candidate's rule_matches row at the given status.
// One (rule_id, feed_item_id) pair owns one row — the schema carries no
// uniqueness for the pair — so the write is an update first and an insert
// only when no row exists: a re-committed item — a failed grab
// re-entering, or a crash-orphaned 'queued' row — moves in place instead
// of duplicating, and the stale task identity clears with the status.
func upsertMatch(ctx context.Context, ex matchWriter, ruleID, status string,
	cand Candidate, now int64) (string, error) {

	result, err := ex.ExecContext(ctx, `UPDATE rule_matches
		SET status = ?, score = ?, content_key = ?, title = ?, reason = NULL,
			task_id = NULL, info_hash = NULL, matched_at = ?, updated_at = ?
		WHERE rule_id = ? AND feed_item_id = ?`,
		status, cand.Score, cand.ContentKey, cand.Item.Title, now, now, ruleID, cand.Item.ID)
	if err != nil {
		return "", fmt.Errorf("queue match of item %s: %w", cand.Item.ID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("queue match of item %s: %w", cand.Item.ID, err)
	}

	if rows > 0 {
		var id string
		if err := ex.GetContext(ctx, &id,
			`SELECT id FROM rule_matches WHERE rule_id = ? AND feed_item_id = ? LIMIT 1`,
			ruleID, cand.Item.ID); err != nil {
			return "", fmt.Errorf("queue match of item %s: %w", cand.Item.ID, err)
		}

		return id, nil
	}

	id := store.NewID(store.PrefixRuleMatch)
	if _, err := ex.ExecContext(ctx, `INSERT INTO rule_matches
		(id, rule_id, feed_item_id, content_key, title, status, score, matched_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, ruleID, cand.Item.ID, cand.ContentKey, cand.Item.Title, status, cand.Score, now, now, now); err != nil {
		return "", fmt.Errorf("queue match of item %s: %w", cand.Item.ID, err)
	}

	return id, nil
}

// queueMatch lands the winner's row at 'queued' in its own transaction,
// committed before the task hand-off so a crash mid-grab leaves the row
// as evidence.
func queueMatch(ctx context.Context, db *sqlx.DB, ruleID string, cand Candidate, now int64) (string, error) {
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin match tx: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "rss: rollback of match queue failed", "error", err)
		}
	}()

	id, err := upsertMatch(ctx, tx, ruleID, matchQueued, cand, now)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("queue match of item %s: commit: %w", cand.Item.ID, err)
	}

	return id, nil
}

// sendMatch lands the successful grab: 'sent' with its task id and the
// item's info hash — the write that hands the hash to HasInfoHash — and
// the staged episode keys, in one transaction so an accepted task never
// leaves a half-committed match.
func sendMatch(ctx context.Context, db *sqlx.DB, matchID, ruleID string,
	cand Candidate, taskID string, now int64) error {

	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("send match %s: %w", matchID, err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "rss: rollback of match send failed", "error", err)
		}
	}()

	if _, err := tx.ExecContext(ctx, `UPDATE rule_matches
		SET status = ?, task_id = ?, info_hash = ?, updated_at = ? WHERE id = ?`,
		matchSent, taskID, cand.Item.InfoHash, now, matchID); err != nil {
		return fmt.Errorf("send match %s: %w", matchID, err)
	}
	for _, key := range cand.stagedKeys {
		// OR IGNORE guards a key another winner staged between the
		// evaluation and this commit: a stored key is a committed fact
		// this write must not error on.
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO rule_seen_episodes (rule_id, episode_key, seen_at) VALUES (?, ?, ?)`,
			ruleID, key, now); err != nil {
			return fmt.Errorf("send match %s: %w", matchID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("send match %s: commit: %w", matchID, err)
	}

	return nil
}

// failMatch lands the refused hand-off: 'failed' with the error text in
// reason. info_hash stays NULL — the grab never owned the content — so the
// item re-enters evaluation on the next run.
func failMatch(ctx context.Context, db *sqlx.DB, matchID, reason string, now int64) error {
	if _, err := db.ExecContext(ctx, `UPDATE rule_matches
		SET status = ?, reason = ?, updated_at = ? WHERE id = ?`,
		matchFailed, reason, now, matchID); err != nil {
		return fmt.Errorf("fail match %s: %w", matchID, err)
	}

	return nil
}

// storedRuleDoc decodes the validated document of one rules row. The
// stored JSON was marshalled from a defaulted document at save time;
// ApplyDefaults is idempotent, so a hand-written row still evaluates under
// the documented defaults.
func storedRuleDoc(rule store.Rule) (RuleDoc, error) {
	var doc RuleDoc
	if err := json.Unmarshal([]byte(rule.DefinitionJSON), &doc); err != nil {
		return RuleDoc{}, fmt.Errorf("rss: rule %s: decode document: %w", rule.ID, err)
	}
	doc.ApplyDefaults()

	return doc, nil
}

// RunRule evaluates one saved rule against the items already stored for
// its feeds and commits the result — the body of POST /rules/{id}/run and
// the engine of the post-poll pass. limit caps the items read per feed,
// newest first; a non-positive limit reads every stored item.
func RunRule(ctx context.Context, db *sqlx.DB, ruleID string, limit int,
	tc TaskCreator, now int64) (CommitReport, error) {

	rule, err := store.RuleByID(ctx, db, ruleID)
	if err != nil {
		return CommitReport{}, fmt.Errorf("rss: run rule: %w", err)
	}
	doc, err := storedRuleDoc(rule)
	if err != nil {
		return CommitReport{}, err
	}

	// The feed scope resolves exactly like the dry run's: rule.feeds
	// carries URLs, not ids, and an empty list means every enabled feed.
	feeds, err := dryRunFeeds(ctx, db, DryRunRequest{Rule: doc})
	if err != nil {
		return CommitReport{}, fmt.Errorf("rss: run rule %s: %w", rule.ID, err)
	}
	if limit <= 0 {
		limit = math.MaxInt
	}
	feedByID := make(map[string]FeedRef, len(feeds))
	var items []store.FeedItem
	for _, feed := range feeds {
		feedByID[feed.ID] = FeedRef{URL: feed.URL, Priority: feed.Priority}
		batch, err := newestFeedItems(ctx, db, feed.ID, limit)
		if err != nil {
			return CommitReport{}, fmt.Errorf("rss: run rule %s: %w", rule.ID, err)
		}
		items = append(items, batch...)
	}
	// The newest-first merge ListFeedItems itself pages in, so a capped
	// rule spends its grabs on the freshest items.
	slices.SortStableFunc(items, func(a, b store.FeedItem) int {
		if c := cmp.Compare(dryRunSortKey(b), dryRunSortKey(a)); c != 0 {
			return c
		}

		return cmp.Compare(b.ID, a.ID)
	})

	decisions, cands, err := Evaluate(ctx, doc, rule, items, feedByID, dbState{db: db}, now)
	if err != nil {
		return CommitReport{}, fmt.Errorf("rss: run rule %s: %w", rule.ID, err)
	}
	report, err := Commit(ctx, db, doc, rule, cands, tc, now)
	// Steps 1 and 3 remove items before evaluation, so evaluated counts
	// decisions only, not the stored item set — the same accounting the
	// dry run reports.
	report.Evaluated = len(decisions)

	return report, err
}

// RunAllRules evaluates every enabled rule — ListRules' (priority ASC,
// name ASC) order is the evaluation order — over the items of one feed and
// commits each rule's candidates. poll.go calls it after a successful
// fetch that added at least one item. A rule that fails joins the
// returned error without stopping the rest: the poller logs it and the
// next poll retries.
func RunAllRules(ctx context.Context, db *sqlx.DB, feedID string, tc TaskCreator, now int64) error {
	feed, err := store.FeedByID(ctx, db, feedID)
	if err != nil {
		return fmt.Errorf("rss: run rules: %w", err)
	}
	rules, err := store.ListRules(ctx, db, true)
	if err != nil {
		return fmt.Errorf("rss: run rules: list: %w", err)
	}
	items, err := newestFeedItems(ctx, db, feedID, math.MaxInt)
	if err != nil {
		return fmt.Errorf("rss: run rules: list items of feed %s: %w", feedID, err)
	}

	feedByID := map[string]FeedRef{feedID: {URL: feed.URL, Priority: feed.Priority}}
	state := dbState{db: db}
	var errs []error
	for _, rule := range rules {
		doc, err := storedRuleDoc(rule)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		_, cands, err := Evaluate(ctx, doc, rule, items, feedByID, state, now)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := Commit(ctx, db, doc, rule, cands, tc, now); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}
