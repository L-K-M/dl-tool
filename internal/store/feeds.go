package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

const (
	// feedItemsDefaultLimit and feedItemsMaxLimit are the page bounds of
	// GET /feeds/{id}/items (docs/05-api-contract.md section 10.1):
	// optional, default 50, range 1..200. Huma enforces them for the
	// endpoint; ListFeedItems re-checks them for every other caller.
	feedItemsDefaultLimit = 50
	feedItemsMaxLimit     = 200
)

// Feed is one row of feeds plus the computed unread_count. Secrets never appear here; the URL is
// stored verbatim and redacted only on the wire (see the FeedDTO rules below).
type Feed struct {
	ID               string  `db:"id"                 json:"id"`
	URL              string  `db:"url"                json:"-"`
	Title            *string `db:"title"              json:"title"`
	Enabled          bool    `db:"enabled"            json:"enabled"`
	RefreshIntervalS int     `db:"refresh_interval_s" json:"refresh_interval_s"`
	ItemCap          int     `db:"item_cap"           json:"item_cap"`
	Priority         int     `db:"priority"           json:"priority"`
	// UnreadCount is not a feeds column: ListFeeds and FeedByID populate it with a correlated
	// `read = 0` subquery over feed_items, and no write ever touches it.
	UnreadCount     int     `db:"unread_count"       json:"-"`
	ETag            *string `db:"etag"               json:"-"`
	LastModified    *string `db:"last_modified"      json:"-"`
	TTLMinutes      *int    `db:"ttl_minutes"        json:"-"`
	LastFetchAt     *int64  `db:"last_fetch_at"      json:"-"`
	LastSuccessAt   *int64  `db:"last_success_at"    json:"-"`
	NextFetchAt     int64   `db:"next_fetch_at"      json:"-"`
	EscalationLevel int     `db:"escalation_level"   json:"-"`
	DisabledTill    *int64  `db:"disabled_till"      json:"-"`
	LastError       *string `db:"last_error"         json:"-"`
	CreatedAt       int64   `db:"created_at"         json:"-"`
	UpdatedAt       int64   `db:"updated_at"         json:"-"`
}

// FeedItem is one row of feed_items. Identity is resolved by the parser (T067), never here.
type FeedItem struct {
	ID          string  `db:"id"`
	FeedID      string  `db:"feed_id"`
	GUID        *string `db:"guid"`
	Identity    string  `db:"identity"`
	Title       string  `db:"title"`
	TitleNorm   string  `db:"title_norm"`
	Link        *string `db:"link"`
	DownloadURL *string `db:"download_url"`
	InfoHash    *string `db:"info_hash"` // 40 or 64 lowercase hex
	SizeBytes   *int64  `db:"size_bytes"`
	PublishedAt *int64  `db:"published_at"`
	Read        bool    `db:"read"`
	FirstSeenAt int64   `db:"first_seen_at"`
	RawJSON     *string `db:"raw_json"`
	CreatedAt   int64   `db:"created_at"`
	UpdatedAt   int64   `db:"updated_at"`
}

// FeedItemFilter scopes ListFeedItems: FeedIDs empty means every feed, UnreadOnly selects the
// read = 0 rows, and the cursor is bound to both — a cursor minted under another filter answers
// ErrStaleCursor, which the handler maps to 422 exactly like the task list's cursor.
type FeedItemFilter struct {
	FeedIDs    []string
	UnreadOnly bool
	Limit      int
	Cursor     string
}

// feedColumns is the explicit column list every feeds SELECT shares, so a
// later migration cannot silently widen a StructScan target
// (docs/14-conventions.md section 2.4). unread_count is not a feeds column:
// the read queries append it as a correlated subquery.
const feedColumns = `id, url, title, enabled, refresh_interval_s, item_cap,
priority, etag, last_modified, ttl_minutes, last_fetch_at, last_success_at,
next_fetch_at, escalation_level, disabled_till, last_error, created_at,
updated_at`

// querySelectFeeds carries the unread_count of docs/05-api-contract.md
// section 10.1 — the feed's feed_items rows with read = 0 — as a correlated
// expression in the column list, so the list and the detail read agree.
const querySelectFeeds = `SELECT ` + feedColumns + `,
(SELECT COUNT(*) FROM feed_items i WHERE i.feed_id = feeds.id AND i.read = 0) AS unread_count
FROM feeds`

// feedItemColumns is the explicit column list every feed_items SELECT shares.
const feedItemColumns = `id, feed_id, guid, identity, title, title_norm, link,
download_url, info_hash, size_bytes, published_at, read, first_seen_at,
raw_json, created_at, updated_at`

// ListFeeds returns every feed with UnreadCount populated, ordered by (title, id) so the feed
// list is stable; FeedByID resolves one row the same way, ErrNotFound when the id addresses none.
func ListFeeds(ctx context.Context, db *sqlx.DB) ([]Feed, error) {
	var feeds []Feed
	if err := db.SelectContext(ctx, &feeds, querySelectFeeds+` ORDER BY title, id`); err != nil {
		return nil, fmt.Errorf("store: list feeds: %w", err)
	}

	return feeds, nil
}

// FeedByID resolves one row by id with UnreadCount populated like ListFeeds.
// ErrNotFound means the id addresses no row.
func FeedByID(ctx context.Context, db *sqlx.DB, id string) (Feed, error) {
	var feed Feed
	err := db.GetContext(ctx, &feed, querySelectFeeds+` WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Feed{}, fmt.Errorf("store: feed %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Feed{}, fmt.Errorf("store: feed %s: %w", id, err)
	}

	return feed, nil
}

// queryCreateFeed writes every column: the caller owns the identity and
// scheduling fields, and the fetch-state columns take their zero values so a
// create can never smuggle a ladder position in.
const queryCreateFeed = `INSERT INTO feeds (` + feedColumns + `)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// CreateFeed inserts one row. The caller owns f.ID (a fed_ ULID) and
// f.NextFetchAt; created_at and updated_at stamp here. A duplicate url is
// ErrConflict — the API maps it to 409 /problems/conflict.
func CreateFeed(ctx context.Context, db *sqlx.DB, f Feed) error {
	now := time.Now().UnixMilli()
	if _, err := db.ExecContext(ctx, queryCreateFeed,
		f.ID, f.URL, f.Title, f.Enabled, f.RefreshIntervalS, f.ItemCap,
		f.Priority, f.ETag, f.LastModified, f.TTLMinutes, f.LastFetchAt,
		f.LastSuccessAt, f.NextFetchAt, f.EscalationLevel, f.DisabledTill,
		f.LastError, now, now,
	); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("store: create feed %s: %w", f.ID, ErrConflict)
		}

		return fmt.Errorf("store: create feed %s: %w", f.ID, err)
	}

	return nil
}

// queryUpdateFeed touches only the operator-owned columns; every fetch-state
// column — etag through last_error — is the poller's and survives a PATCH.
const queryUpdateFeed = `UPDATE feeds
SET url = ?, title = ?, enabled = ?, refresh_interval_s = ?, item_cap = ?,
priority = ?, updated_at = ?
WHERE id = ?`

// UpdateFeed writes the operator-owned columns — url, title, enabled, refresh_interval_s,
// item_cap, priority and updated_at — and leaves every fetch-state column untouched.
// ErrNotFound means id addresses no row; ErrConflict means url belongs to another row.
func UpdateFeed(ctx context.Context, db *sqlx.DB, f Feed) error {
	result, err := db.ExecContext(ctx, queryUpdateFeed,
		f.URL, f.Title, f.Enabled, f.RefreshIntervalS, f.ItemCap, f.Priority,
		time.Now().UnixMilli(), f.ID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("store: update feed %s: %w", f.ID, ErrConflict)
		}

		return fmt.Errorf("store: update feed %s: %w", f.ID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update feed %s: read rows affected: %w", f.ID, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: update feed %s: %w", f.ID, ErrNotFound)
	}

	return nil
}

// DeleteFeed removes the row; its feed_items rows go with it through
// ON DELETE CASCADE (docs/04-data-model.md section 3.5). ErrNotFound means
// id addresses no row.
func DeleteFeed(ctx context.Context, db *sqlx.DB, id string) error {
	result, err := db.ExecContext(ctx, `DELETE FROM feeds WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete feed %s: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete feed %s: read rows affected: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: delete feed %s: %w", id, ErrNotFound)
	}

	return nil
}

// queryDueFeeds is the poller's work list: enabled feeds whose next poll is
// due and that are not inside a disabled_till window, oldest first. The
// partial index idx_feeds_next_fetch covers the enabled and next_fetch_at
// half of the predicate.
const queryDueFeeds = `SELECT ` + feedColumns + ` FROM feeds
WHERE enabled = 1 AND next_fetch_at <= ? AND (disabled_till IS NULL OR disabled_till <= ?)
ORDER BY next_fetch_at, id LIMIT ?`

// DueFeeds returns enabled feeds with next_fetch_at <= now and no future disabled_till,
// oldest next_fetch_at first. It is the poller's work list.
func DueFeeds(ctx context.Context, db *sqlx.DB, now int64, limit int) ([]Feed, error) {
	if limit < 1 {
		return nil, nil
	}

	var feeds []Feed
	if err := db.SelectContext(ctx, &feeds, queryDueFeeds, now, now, limit); err != nil {
		return nil, fmt.Errorf("store: list due feeds: %w", err)
	}

	return feeds, nil
}

// queryUpdateFeedFetchState is the mirror of queryUpdateFeed: only the
// fetch-state columns move; the operator-owned ones survive a poll.
const queryUpdateFeedFetchState = `UPDATE feeds
SET etag = ?, last_modified = ?, ttl_minutes = ?, last_fetch_at = ?,
last_success_at = ?, next_fetch_at = ?, escalation_level = ?,
disabled_till = ?, last_error = ?, updated_at = ?
WHERE id = ?`

// UpdateFeedFetchState writes only etag, last_modified, ttl_minutes, last_fetch_at,
// last_success_at, next_fetch_at, escalation_level, disabled_till, last_error and updated_at.
// The ladder that computes them lives in internal/rss/poll.go (T066).
// ErrNotFound means id addresses no row.
func UpdateFeedFetchState(ctx context.Context, db *sqlx.DB, f Feed) error {
	result, err := db.ExecContext(ctx, queryUpdateFeedFetchState,
		f.ETag, f.LastModified, f.TTLMinutes, f.LastFetchAt, f.LastSuccessAt,
		f.NextFetchAt, f.EscalationLevel, f.DisabledTill, f.LastError,
		time.Now().UnixMilli(), f.ID,
	)
	if err != nil {
		return fmt.Errorf("store: update fetch state of feed %s: %w", f.ID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update fetch state of feed %s: read rows affected: %w", f.ID, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: update fetch state of feed %s: %w", f.ID, ErrNotFound)
	}

	return nil
}

// queryFeedItemIdentities finds the (feed_id, identity) pairs a batch already
// holds, so the upsert can count genuinely new rows — an ON CONFLICT DO UPDATE
// touches one row either way, so RowsAffected cannot tell them apart.
const queryFeedItemIdentities = `SELECT feed_id, identity FROM feed_items
WHERE feed_id = ? AND identity IN (?)`

// queryUpsertFeedItem refreshes only title and updated_at on a repeated
// identity: read and first_seen_at are insert-time facts a re-poll must not
// reset (docs/05-api-contract.md section 10.1).
const queryUpsertFeedItem = `INSERT INTO feed_items
(id, feed_id, guid, identity, title, title_norm, link, download_url,
 info_hash, size_bytes, published_at, read, first_seen_at, raw_json,
 created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (feed_id, identity) DO UPDATE SET
  title = excluded.title,
  updated_at = excluded.updated_at`

// UpsertFeedItems inserts items in one transaction with
// INSERT ... ON CONFLICT (feed_id, identity) DO UPDATE SET title=..., updated_at=...
// and returns how many rows were new. An empty id gets a fresh itm_ ULID and a
// zero first_seen_at takes now; a repeated identity inside one batch updates
// the row the earlier entry wrote instead of creating a second one.
func UpsertFeedItems(ctx context.Context, db *sqlx.DB, items []FeedItem, now int64) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}

	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: upsert feed items: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "store: rollback of feed items upsert failed", "error", err)
		}
	}()

	existing, err := feedItemIdentities(ctx, tx, items)
	if err != nil {
		return 0, err
	}

	inserted := 0
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		key := item.FeedID + "\x00" + item.Identity
		id := item.ID
		if id == "" {
			id = NewID(PrefixFeedItem)
		}
		firstSeen := item.FirstSeenAt
		if firstSeen == 0 {
			firstSeen = now
		}
		if _, err := tx.ExecContext(ctx, queryUpsertFeedItem,
			id, item.FeedID, item.GUID, item.Identity, item.Title,
			item.TitleNorm, item.Link, item.DownloadURL, item.InfoHash,
			item.SizeBytes, item.PublishedAt, item.Read, firstSeen,
			item.RawJSON, now, now,
		); err != nil {
			return 0, fmt.Errorf("store: upsert feed item %s: %w", item.Identity, err)
		}
		if _, dup := seen[key]; !dup {
			if _, known := existing[key]; !known {
				inserted++
			}
			seen[key] = struct{}{}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: upsert feed items: commit: %w", err)
	}

	return inserted, nil
}

// feedItemIdentities returns the set of (feed_id, identity) pairs the batch
// addresses that already exist, keyed "feedID\x00identity".
func feedItemIdentities(ctx context.Context, tx *sqlx.Tx, items []FeedItem) (map[string]struct{}, error) {
	byFeed := make(map[string][]string)
	for _, item := range items {
		byFeed[item.FeedID] = append(byFeed[item.FeedID], item.Identity)
	}

	existing := make(map[string]struct{}, len(items))
	for feedID, identities := range byFeed {
		query, args, err := sqlx.In(queryFeedItemIdentities, feedID, identities)
		if err != nil {
			return nil, fmt.Errorf("store: upsert feed items: build identity lookup: %w", err)
		}
		var rows []struct {
			FeedID   string `db:"feed_id"`
			Identity string `db:"identity"`
		}
		if err := tx.SelectContext(ctx, &rows, query, args...); err != nil {
			return nil, fmt.Errorf("store: upsert feed items: read identities: %w", err)
		}
		for _, row := range rows {
			existing[row.FeedID+"\x00"+row.Identity] = struct{}{}
		}
	}

	return existing, nil
}

// queryTrimFeedItems keeps the newest cap rows under the same ordering the
// item list pages in, so retention never disagrees with what the screen shows.
const queryTrimFeedItems = `DELETE FROM feed_items
WHERE feed_id = ? AND id NOT IN (
	SELECT id FROM feed_items
	WHERE feed_id = ?
	ORDER BY COALESCE(published_at, first_seen_at) DESC, id DESC
	LIMIT ?)`

// TrimFeedItems deletes all but the newest cap rows of one feed, ordered by
// COALESCE(published_at, first_seen_at) DESC. cap <= 0 keeps everything.
// It returns the number of deleted rows.
func TrimFeedItems(ctx context.Context, db *sqlx.DB, feedID string, cap int) (int64, error) {
	if cap <= 0 {
		return 0, nil
	}

	result, err := db.ExecContext(ctx, queryTrimFeedItems, feedID, feedID, cap)
	if err != nil {
		return 0, fmt.Errorf("store: trim items of feed %s: %w", feedID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: trim items of feed %s: read rows affected: %w", feedID, err)
	}

	return rows, nil
}

// feedItemCursor is the decoded page token: the sort value and id of the last
// row of the page that issued it, plus a hash binding it to the filter that
// produced it (docs/05-api-contract.md section 1.4). The sort value is
// COALESCE(published_at, first_seen_at), which is never NULL because
// first_seen_at is NOT NULL.
type feedItemCursor struct {
	Hash   string `json:"h"`
	LastID string `json:"i"`
	Value  int64  `json:"v"`
}

// ListFeedItems pages newest first — COALESCE(published_at, first_seen_at) DESC, id DESC as the
// tie-break — and returns the page, the next cursor and the total matching the filter, ignoring
// the cursor and the limit (doc 05 §1.4).
func ListFeedItems(ctx context.Context, db *sqlx.DB, f FeedItemFilter) (items []FeedItem, nextCursor string, total int, err error) {
	limit := f.Limit
	if limit == 0 {
		limit = feedItemsDefaultLimit
	}
	if limit < 1 || limit > feedItemsMaxLimit {
		return nil, "", 0, fmt.Errorf("store: list feed items: limit %d outside 1..%d", limit, feedItemsMaxLimit)
	}

	where := []string{}
	args := []any{}
	if len(f.FeedIDs) > 0 {
		where = append(where, "feed_id IN (?)")
		args = append(args, f.FeedIDs)
	}
	if f.UnreadOnly {
		where = append(where, "read = 0")
	}
	filter := "1 = 1"
	if len(where) > 0 {
		filter = strings.Join(where, " AND ")
	}

	hash := feedItemFilterHash(f)
	pageWhere := filter
	pageArgs := args
	if f.Cursor != "" {
		cursor, err := decodeFeedItemCursor(f.Cursor)
		if err != nil {
			return nil, "", 0, err
		}
		if cursor.Hash != hash {
			return nil, "", 0, fmt.Errorf("%w: issued for a different filter", ErrStaleCursor)
		}

		pageWhere += ` AND (COALESCE(published_at, first_seen_at) < ?
OR (COALESCE(published_at, first_seen_at) = ? AND id < ?))`
		pageArgs = append(pageArgs, cursor.Value, cursor.Value, cursor.LastID)
	}

	// total counts the filter, ignoring the cursor (docs/05-api-contract.md
	// section 1.4); it runs after the cursor check so an invalid token
	// fails fast instead of paying for the count scan.
	countQuery, countArgs, err := sqlx.In("SELECT COUNT(*) FROM feed_items WHERE "+filter, args...)
	if err != nil {
		return nil, "", 0, fmt.Errorf("store: list feed items: build count: %w", err)
	}
	if err := db.GetContext(ctx, &total, countQuery, countArgs...); err != nil {
		return nil, "", 0, fmt.Errorf("store: list feed items: count: %w", err)
	}

	// One row past the limit decides whether another page exists, so
	// next_cursor is null exactly on the last page.
	query := `SELECT ` + feedItemColumns + ` FROM feed_items WHERE ` + pageWhere +
		` ORDER BY COALESCE(published_at, first_seen_at) DESC, id DESC` +
		fmt.Sprintf(" LIMIT %d", limit+1)
	query, queryArgs, err := sqlx.In(query, pageArgs...)
	if err != nil {
		return nil, "", 0, fmt.Errorf("store: list feed items: build page query: %w", err)
	}

	if err := db.SelectContext(ctx, &items, query, queryArgs...); err != nil {
		return nil, "", 0, fmt.Errorf("store: list feed items: read page: %w", err)
	}
	if len(items) <= limit {
		return items, "", total, nil
	}
	items = items[:limit]

	last := items[len(items)-1]
	nextCursor, err = encodeFeedItemCursor(feedItemCursor{
		Hash:   hash,
		LastID: last.ID,
		Value:  feedItemSortKey(last),
	})
	if err != nil {
		return nil, "", 0, fmt.Errorf("store: list feed items: encode cursor: %w", err)
	}

	return items, nextCursor, total, nil
}

// feedItemSortKey is the order value of one row: published_at when the feed
// carried a date, first_seen_at otherwise — the COALESCE the page orders by.
func feedItemSortKey(item FeedItem) int64 {
	if item.PublishedAt != nil {
		return *item.PublishedAt
	}

	return item.FirstSeenAt
}

// feedItemFilterHash binds a cursor to the filter that produced it: the feed
// id set (order-insensitive) and the unread flag.
func feedItemFilterHash(f FeedItemFilter) string {
	feedIDs := slices.Clone(f.FeedIDs)
	slices.Sort(feedIDs)
	canonical := strings.Join([]string{
		"feeds:" + strings.Join(feedIDs, ","),
		fmt.Sprintf("unread:%t", f.UnreadOnly),
	}, "\x00")
	sum := sha256.Sum256([]byte(canonical))

	return base64.RawStdEncoding.EncodeToString(sum[:])
}

// encodeFeedItemCursor renders a page token as base64 JSON.
func encodeFeedItemCursor(c feedItemCursor) (string, error) {
	encoded, err := json.Marshal(c)
	if err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

// decodeFeedItemCursor parses a page token. A token that is not base64 JSON
// of the cursor shape is reported as ErrStaleCursor: it does not belong to
// this (or any) filter, and the wire outcome is the same 422.
func decodeFeedItemCursor(token string) (feedItemCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return feedItemCursor{}, fmt.Errorf("%w: token is not valid base64", ErrStaleCursor)
	}

	var cursor feedItemCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		return feedItemCursor{}, fmt.Errorf("%w: token is not a page cursor", ErrStaleCursor)
	}
	if cursor.LastID == "" {
		return feedItemCursor{}, fmt.Errorf("%w: token carries no row", ErrStaleCursor)
	}

	return cursor, nil
}

// querySetFeedItemsRead touches only rows whose state would actually change
// (read <> ?), so the returned count is the {"updated":n} of doc 05 section
// 10.1 and a repeat call is a reported no-op, never a silent one.
const querySetFeedItemsRead = `UPDATE feed_items
SET read = ?, updated_at = ?
WHERE feed_id = ? AND read <> ? AND id IN (?)`

// SetFeedItemsRead marks the listed items of one feed read or unread and returns how many rows
// changed state. An id that names no row of this feed is skipped, which makes the call idempotent.
func SetFeedItemsRead(ctx context.Context, db *sqlx.DB, feedID string, ids []string, read bool, now int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	query, args, err := sqlx.In(querySetFeedItemsRead, read, now, feedID, read, ids)
	if err != nil {
		return 0, fmt.Errorf("store: mark items of feed %s read: %w", feedID, err)
	}
	result, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("store: mark items of feed %s read: %w", feedID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: mark items of feed %s read: read rows affected: %w", feedID, err)
	}

	return rows, nil
}

// MarkAllFeedItemsRead marks every unread item of one feed read and returns the count.
func MarkAllFeedItemsRead(ctx context.Context, db *sqlx.DB, feedID string, now int64) (int64, error) {
	result, err := db.ExecContext(
		ctx,
		`UPDATE feed_items SET read = 1, updated_at = ? WHERE feed_id = ? AND read = 0`,
		now, feedID,
	)
	if err != nil {
		return 0, fmt.Errorf("store: mark all items of feed %s read: %w", feedID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: mark all items of feed %s read: read rows affected: %w", feedID, err)
	}

	return rows, nil
}
