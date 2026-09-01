# T065 — Store RSS feeds and items and serve feed CRUD

| Field | Value |
|---|---|
| **ID** | T065 |
| **Milestone** | M5 |
| **Status** | todo |
| **Depends on** | T006, T007, T008 |
| **Blocks** | T066, T068, T072 |
| **Parallel-safe** | no — it also edits the shared file `internal/api/server.go` |
| **Implements** | [FR-070](../02-requirements.md#fr-070-manage-feeds-and-refresh-on-demand) |
| **Decisions** | [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md), [ADR-0009](../decisions/0009-native-cross-protocol-rss-rules.md) |
| **Est. size** | 3 new files, ~380 LOC |

## Goal
`/feeds` creates, lists, updates and deletes feed rows, and `GET /feeds/{id}/items` pages a feed's stored
items newest first. The store layer also carries the upsert, trim and scheduling queries the poller needs, so
T066 adds no SQL of its own beyond one fetch-state write.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §10.1 Feeds](../05-api-contract.md#101-feeds) — the feed object, the accepted
   members, the item page and every status code.
2. [`docs/04-data-model.md` §3.5 RSS](../04-data-model.md#35-rss) — the `feeds` and `feed_items` DDL and
   their indices. The columns are fixed; this task adds none.
3. [`docs/05-api-contract.md` §1.4 Cursor pagination](../05-api-contract.md#14-cursor-pagination) — the
   cursor shape `GET /feeds/{id}/items` reuses.
4. [`docs/04-data-model.md` §7 Retention](../04-data-model.md#7-retention) — `feed_items` keeps the newest
   `feeds.item_cap` rows per feed.
5. [`docs/14-conventions.md` §2.4 SQL and sqlx](../14-conventions.md#24-sql-and-sqlx) — explicit column
   lists, `?` placeholders, one transaction per multi-statement write.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/store/feeds.go` | create | `Feed`, `FeedItem` and every query over `feeds` and `feed_items`. |
| `internal/api/feeds.go` | create | Feed CRUD, `GET /feeds/{id}/items` and the group's `Register`. |
| `internal/api/feeds_test.go` | create | `humatest` cases for CRUD, paging, conflict and validation. |
| `internal/api/server.go` | edit | Call `NewFeedHandlers(...).Register(api)` once. |

No other file may be modified.

## Interface contract

```go
package store

// Feed is one row of feeds. Secrets never appear here; the URL is stored verbatim.
type Feed struct {
	ID               string  `db:"id"                 json:"id"`
	URL              string  `db:"url"                json:"url"`
	Title            *string `db:"title"              json:"title"`
	Enabled          bool    `db:"enabled"            json:"enabled"`
	RefreshIntervalS int     `db:"refresh_interval_s" json:"refresh_interval_s"`
	ItemCap          int     `db:"item_cap"           json:"item_cap"`
	ETag             *string `db:"etag"               json:"-"`
	LastModified     *string `db:"last_modified"      json:"-"`
	TTLMinutes       *int    `db:"ttl_minutes"        json:"-"`
	LastFetchAt      *int64  `db:"last_fetch_at"      json:"-"`
	LastSuccessAt    *int64  `db:"last_success_at"    json:"-"`
	NextFetchAt      int64   `db:"next_fetch_at"      json:"-"`
	EscalationLevel  int     `db:"escalation_level"   json:"-"`
	DisabledTill     *int64  `db:"disabled_till"      json:"-"`
	LastError        *string `db:"last_error"         json:"-"`
	CreatedAt        int64   `db:"created_at"         json:"-"`
	UpdatedAt        int64   `db:"updated_at"         json:"-"`
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
	FirstSeenAt int64   `db:"first_seen_at"`
	RawJSON     *string `db:"raw_json"`
	CreatedAt   int64   `db:"created_at"`
	UpdatedAt   int64   `db:"updated_at"`
}

func ListFeeds(ctx context.Context, db *sqlx.DB) ([]Feed, error)
func FeedByID(ctx context.Context, db *sqlx.DB, id string) (Feed, error)
func CreateFeed(ctx context.Context, db *sqlx.DB, f Feed) error
func UpdateFeed(ctx context.Context, db *sqlx.DB, f Feed) error
func DeleteFeed(ctx context.Context, db *sqlx.DB, id string) error

// DueFeeds returns enabled feeds with next_fetch_at <= now and no future disabled_till,
// oldest next_fetch_at first. It is the poller's work list.
func DueFeeds(ctx context.Context, db *sqlx.DB, now int64, limit int) ([]Feed, error)

// UpdateFeedFetchState writes only etag, last_modified, ttl_minutes, last_fetch_at,
// last_success_at, next_fetch_at, escalation_level, disabled_till, last_error and updated_at.
// The ladder that computes them lives in internal/rss/poll.go (T066).
func UpdateFeedFetchState(ctx context.Context, db *sqlx.DB, f Feed) error

// UpsertFeedItems inserts items in one transaction with
// INSERT ... ON CONFLICT (feed_id, identity) DO UPDATE SET title=..., updated_at=...
// and returns how many rows were new.
func UpsertFeedItems(ctx context.Context, db *sqlx.DB, items []FeedItem, now int64) (int, error)

// TrimFeedItems deletes all but the newest cap rows of one feed, ordered by
// COALESCE(published_at, first_seen_at) DESC. cap <= 0 keeps everything.
func TrimFeedItems(ctx context.Context, db *sqlx.DB, feedID string, cap int) (int64, error)

// ListFeedItems pages one feed newest first. feedIDs empty means every feed.
func ListFeedItems(ctx context.Context, db *sqlx.DB, feedIDs []string, limit int, cursor string) ([]FeedItem, string, error)
```

Read state has no column in doc 04 §3.5, so it is derived, not stored: an item is **unread** when
`first_seen_at >= feeds.last_success_at`, that is, it arrived in the most recent successful poll.
`unread_count` counts those rows and the `unread` query filters on the same predicate.

Statuses, exactly doc 05 §10.1: `200` · `201` · `204` · `404` · `409 /problems/conflict` on a duplicate
`url` · `422 /problems/validation-failed` for a non-`http(s)` URL, `refresh_interval_s` below `300` and not
`0`, or `item_cap` below `0`.

## Steps
1. Create `internal/store/feeds.go` with the two structs and the eleven functions above, every statement
   carrying an explicit column list and a context.
2. Generate ids with `store.NewID(store.PrefixFeed)` for feeds and `store.PrefixFeedItem` for items, per
   [`docs/04-data-model.md` §1.5](../04-data-model.md#15-id-prefix-allocation).
3. Implement `UpsertFeedItems` in one `sqlx.Tx`, counting new rows from the `RowsAffected` of inserts that
   did not conflict; a repeated identity inside one batch must not create a second row.
4. Implement `TrimFeedItems` with a single `DELETE ... WHERE id NOT IN (SELECT id ... LIMIT :cap)`.
5. Create `internal/api/feeds.go` with `FeedHandlers`, `NewFeedHandlers`, `Register` and the five
   operations `List`, `Create`, `Patch`, `Delete`, `Items`, mirroring T050's handler shape.
6. Map `store.ErrNotFound` to `404` and a `UNIQUE` violation on `feeds.url` to `409 /problems/conflict`;
   never return the raw SQLite error text.
7. Set `next_fetch_at = now` on create so a new feed is picked up by the next poll pass, and leave
   `escalation_level` at `0`.
8. Serve `GET /feeds/{id}/items` with `limit` (default 50, max 200), `cursor` and `unread`, returning
   `next_cursor` and `total` exactly as doc 05 §10.1 shows.
9. Edit `internal/api/server.go` to construct the handlers and call `Register(api)`.
10. Create `internal/api/feeds_test.go` covering: create then list; a duplicate URL is `409`; `ftp://` is
    `422`; `refresh_interval_s: 60` is `422`; patch toggles `enabled`; delete returns `204` and cascades the
    feed's items; the item page is newest first and its cursor returns the next page without overlap.
11. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestFeedCrud`, `TestDuplicateFeedURLConflicts` and `TestFeedValidation` pass.
- [ ] `TestFeedItemsPagingNewestFirst` asserts no row appears on two pages.
- [ ] `TestDeleteFeedCascadesItems` asserts zero `feed_items` rows survive.
- [ ] `TestUpsertFeedItemsIsIdempotent` asserts a second upsert of the same batch adds `0` rows.
- [ ] No new column, table or index exists beyond doc 04 §3.5.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/api/... ./internal/store/..." && echo FEEDS_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/api` and `ok  github.com/L-K-M/dl-tool/internal/store`,
every test named above reported as `--- PASS`, and the final line of stdout is exactly `FEEDS_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT fetch anything. `POST /feeds/{id}/refresh` and every HTTP call belong to T066.
- Do NOT parse XML or fill `download_url`, `info_hash` or `title_norm` from a feed body; T067 owns parsing.
- Do NOT add a `read` column, a `read_items` table or a folder table; doc 04 §3.5 has none, and the feed
  tree's folders are a client-side grouping in T072.
- Do NOT write `rules`, `rule_matches` or `rule_seen_episodes`; T068 and T071 own them.
- Do NOT add an `unread` counter to the SSE payload; the feeds screen refetches.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
