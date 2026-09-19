# T065 — Store RSS feeds and items and serve feed CRUD

| Field | Value |
|---|---|
| **ID** | T065 |
| **Milestone** | M5 |
| **Status** | todo |
| **Depends on** | T006, T007, T008 |
| **Blocks** | T066, T068, T072, T117 |
| **Parallel-safe** | no — it also edits the shared file `internal/api/server.go` |
| **Implements** | [FR-070](../02-requirements.md#fr-070-manage-feeds-and-refresh-on-demand) |
| **Decisions** | [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md), [ADR-0009](../decisions/0009-native-cross-protocol-rss-rules.md) |
| **Est. size** | 3 new files, ~560 LOC — the two mark-read operations, `unread_count` and the redacted DTO mapping widen the pre-review estimate |

## Goal
`/feeds` creates, lists, updates and deletes feed rows, `GET /feeds/{id}/items` pages a feed's stored
items newest first, and `PATCH /feeds/{id}/items` with `POST /feeds/{id}/items/read-all` mark stored items
read. The store layer also carries the upsert, trim and scheduling queries the poller needs, so T066 adds
no SQL of its own beyond one fetch-state write.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §10.1 Feeds](../05-api-contract.md#101-feeds) — the feed object, the accepted
   members, the item page and every status code.
2. [`docs/04-data-model.md` §3.5 RSS](../04-data-model.md#35-rss) — the `feeds` and `feed_items` DDL and
   their indices. The columns are fixed; this task adds none.
3. [`docs/05-api-contract.md` §1.4 Cursor pagination](../05-api-contract.md#14-cursor-pagination) — the
   cursor shape `GET /feeds/{id}/items` reuses.
4. [`docs/05-api-contract.md` §1.6 Units, timestamps and nulls](../05-api-contract.md#16-units-timestamps-and-nulls)
   — RFC 3339 on the wire, Unix milliseconds in the store, and the `__redacted__` write-back rule.
5. [`docs/04-data-model.md` §7 Retention](../04-data-model.md#7-retention) — `feed_items` keeps the newest
   `feeds.item_cap` rows per feed.
6. [`docs/14-conventions.md` §2.4 SQL and sqlx](../14-conventions.md#24-sql-and-sqlx) — explicit column
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

// Feed is one row of feeds plus the computed unread_count. Secrets never appear here; the URL is
// stored verbatim and redacted only on the wire (see the FeedDTO rules below).
type Feed struct {
	ID               string  `db:"id"                 json:"id"`
	URL              string  `db:"url"                json:"url"`
	Title            *string `db:"title"              json:"title"`
	Enabled          bool    `db:"enabled"            json:"enabled"`
	RefreshIntervalS int     `db:"refresh_interval_s" json:"refresh_interval_s"`
	ItemCap          int     `db:"item_cap"           json:"item_cap"`
	Priority         int     `db:"priority"           json:"priority"`
	// UnreadCount is not a feeds column: ListFeeds and FeedByID populate it with a correlated
	// `read = 0` subquery over feed_items, and no write ever touches it.
	UnreadCount      int     `db:"unread_count"       json:"-"`
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
	Read        bool    `db:"read"`
	FirstSeenAt int64   `db:"first_seen_at"`
	RawJSON     *string `db:"raw_json"`
	CreatedAt   int64   `db:"created_at"`
	UpdatedAt   int64   `db:"updated_at"`
}

// ListFeeds returns every feed with UnreadCount populated, ordered by (title, id) so the feed
// list is stable; FeedByID resolves one row the same way, ErrNotFound when the id addresses none.
func ListFeeds(ctx context.Context, db *sqlx.DB) ([]Feed, error)
func FeedByID(ctx context.Context, db *sqlx.DB, id string) (Feed, error)
func CreateFeed(ctx context.Context, db *sqlx.DB, f Feed) error

// UpdateFeed writes the operator-owned columns — url, title, enabled, refresh_interval_s,
// item_cap, priority and updated_at — and leaves every fetch-state column untouched.
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

// FeedItemFilter scopes ListFeedItems: FeedIDs empty means every feed, UnreadOnly selects the
// read = 0 rows, and the cursor is bound to both — a cursor minted under another filter answers
// ErrStaleCursor, which the handler maps to 422 exactly like the task list's cursor.
type FeedItemFilter struct {
	FeedIDs    []string
	UnreadOnly bool
	Limit      int
	Cursor     string
}

// ListFeedItems pages newest first — COALESCE(published_at, first_seen_at) DESC, id DESC as the
// tie-break — and returns the page, the next cursor and the total matching the filter, ignoring
// the cursor (doc 05 §1.4).
func ListFeedItems(ctx context.Context, db *sqlx.DB, f FeedItemFilter) (items []FeedItem, nextCursor string, total int, err error)

// SetFeedItemsRead marks the listed items of one feed read or unread and returns how many rows
// changed state. An id that names no row of this feed is skipped, which makes the call idempotent.
func SetFeedItemsRead(ctx context.Context, db *sqlx.DB, feedID string, ids []string, read bool, now int64) (int64, error)

// MarkAllFeedItemsRead marks every unread item of one feed read and returns the count.
func MarkAllFeedItemsRead(ctx context.Context, db *sqlx.DB, feedID string, now int64) (int64, error)
```

`feed_items.read` carries the read state — doc 04 §3.5 ships the column and `idx_feed_items_read`, and
§10.1 defines `unread_count` and the `unread` filter on it: `unread_count` on the feed object counts the
feed's `read = 0` rows and the `unread` query selects exactly those rows. The `UpsertFeedItems` conflict
update touches `title` and `updated_at` only — `read` and `first_seen_at` are insert-time facts a re-poll
must not reset.

The wire objects are DTOs, not the store rows. `FeedDTO` renders the §10.1 feed object: `last_fetch_at`,
`last_success_at`, `next_fetch_at` and `disabled_till` become RFC 3339 strings or `null` (§1.6), and
`priority` and `unread_count` ride along. A credential-bearing `url` is redacted member-wise — userinfo
and the values of the `apikey`, `token` and `passkey` query parameters become `__redacted__` while the
rest of the URL stays readable, reusing `secretQueryParameters`, `isSecretQueryParameter` and
`redactedValue` from `server.go`; this is §10.1's feed rule, not `secure.RedactURL`, which strips the
whole query for logs. A PATCH `url` that still contains `__redacted__` is a no-op on the stored column,
the same semantics `extract_passwords` and the indexer `api_key` already carry. `FeedItemDTO` renders the
§10.1 item object — `read`, `published_at` as RFC 3339 or `null` — with `matched_rules` rendered as `[]`
until T071 populates it from `rule_matches` (deferral register).

`auto_download` is not modelled on either write body: the member is absent from both schemas, so a sender
gets `422` under the request body's `additionalProperties: false` — explicit, never a silent no-op. T068
adds the member and the `auto:<feed_id>` rule lifecycle it drives (deferral register), and the interim is
pinned by `TestAutoDownloadIsRejectedUntilT068`, which T068 then replaces. The read side is pinned by
`TestFeedObjectShape`: §10.1's feed object carries no `auto_download` member, so `FeedDTO` never renders
one — the interim wire shape cannot vary by implementer.

Statuses, exactly doc 05 §10.1: `200` · `201` · `204` · `404` · `409 /problems/conflict` on a duplicate
`url` · `422 /problems/validation-failed` for a non-`http(s)` URL, `refresh_interval_s` below `300` and not
`0`, or `item_cap` below `0`. `PATCH /feeds/{id}/items` and `POST /feeds/{id}/items/read-all` return `200`
`{"updated":n}` where `n` counts only the rows whose state changed, and `404` when the feed id addresses
no row. Only the PATCH body can `422` — an empty `ids`, more than 500 ids, or a missing `read`; `read-all`
takes no body.

## Steps
1. Create `internal/store/feeds.go` with the two structs and the thirteen functions above, every statement
   carrying an explicit column list and a context. `ListFeeds` and `FeedByID` select `unread_count` as a
   correlated `(SELECT COUNT(*) FROM feed_items i WHERE i.feed_id = feeds.id AND i.read = 0)` expression in
   the column list.
2. Generate ids with `store.NewID(store.PrefixFeed)` for feeds and `store.PrefixFeedItem` for items, per
   [`docs/04-data-model.md` §1.5](../04-data-model.md#15-id-prefix-allocation).
3. Implement `UpsertFeedItems` in one `sqlx.Tx`, counting new rows from the `RowsAffected` of inserts that
   did not conflict; a repeated identity inside one batch must not create a second row. The conflict update
   writes `title` and `updated_at` only, never `read` or `first_seen_at`.
4. Implement `TrimFeedItems` with a single `DELETE ... WHERE id NOT IN (SELECT id ... LIMIT :cap)`.
5. Create `internal/api/feeds.go` with `FeedHandlers`, `NewFeedHandlers`, `Register` and the seven
   operations `List`, `Create`, `Patch`, `Delete`, `Items`, `MarkItems`, `MarkAllRead`, mirroring T050's
   handler shape, plus the `FeedDTO`/`FeedItemDTO` mapping and the member-wise URL redaction described
   above.
6. Map `store.ErrNotFound` to `404` and a `UNIQUE` violation on `feeds.url` to `409 /problems/conflict`;
   never return the raw SQLite error text.
7. Set `next_fetch_at = now` on create so a new feed is picked up by the next poll pass, and leave
   `escalation_level` at `0`.
8. Serve `GET /feeds/{id}/items` with `limit` (default 50, max 200), `cursor` and `unread`, returning
   `next_cursor` and `total` exactly as doc 05 §10.1 shows; the feed id is resolved first so an unknown one
   is `404`, and a cursor minted under another filter is `422`.
9. Serve `PATCH /feeds/{id}/items` and `POST /feeds/{id}/items/read-all` per §10.1 — the PATCH body's
   `ids` is required, 1–500 entries, and `read` is required; `read-all` takes no body. Both answer
   `{"updated":n}` counting only rows whose state changed, skip ids that belong to another feed or no
   row, and are idempotent.
10. Edit `internal/api/server.go` to construct the handlers and call `Register(api)`.
11. Create `internal/api/feeds_test.go` covering: create then list; a duplicate URL is `409`; `ftp://` is
    `422`; `refresh_interval_s: 60` is `422`; patch toggles `enabled`; delete returns `204` and cascades the
    feed's items; the item page is newest first and its cursor returns the next page without overlap; the
    feed object carries `priority`, `unread_count` and RFC 3339 fetch timestamps; a credential-bearing URL
    is redacted on read and a `__redacted__` PATCH leaves the stored URL untouched; `auto_download` is
    `422`; `unread` filters to `read = 0` rows; `PATCH .../items` and `read-all` mark and count correctly.
12. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestFeedCrud`, `TestDuplicateFeedURLConflicts` and `TestFeedValidation` pass.
- [ ] `TestFeedItemsPagingNewestFirst` asserts no row appears on two pages.
- [ ] `TestDeleteFeedCascadesItems` asserts zero `feed_items` rows survive.
- [ ] `TestUpsertFeedItemsIsIdempotent` asserts a second upsert of the same batch adds `0` rows and keeps
  `read` untouched.
- [ ] `TestFeedObjectShape` asserts the feed object carries `priority`, `unread_count` and RFC 3339
  `next_fetch_at`, and no `auto_download` member.
- [ ] `TestCredentialFeedURLIsRedacted` asserts a `user:pass@` URL returns `__redacted__` userinfo on
  read, that every name `isSecretQueryParameter` recognises is masked (one case per name), and that a
  PATCH echoing the redacted URL leaves the stored URL unchanged.
- [ ] `TestUnreadFilterMatchesReadColumn` asserts `unread=true` returns only `read = 0` rows and
  `unread_count` tracks the same predicate.
- [ ] `TestMarkFeedItemsRead` asserts `updated` counts only rows that changed state and that ids of a
  different feed are skipped.
- [ ] `TestMarkAllFeedItemsReadIsIdempotent` asserts the second `read-all` call returns `{"updated":0}`.
- [ ] `TestAutoDownloadIsRejectedUntilT068` asserts `POST /feeds` with `auto_download: true` is `422`.
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
- Do NOT add a column, a `read_items` table or a folder table; doc 04 §3.5 is the whole schema, and the
  feed tree's folders are a client-side grouping in T072.
- Do NOT model `auto_download` or write `rules` rows — the member and the `auto:<feed_id>` lifecycle are
  T068's (deferral register); until then the absent member is a `422`, pinned by
  `TestAutoDownloadIsRejectedUntilT068`.
- Do NOT populate `matched_rules` — T071 joins `rule_matches`; render `[]` until then.
- Do NOT write `rule_matches` or `rule_seen_episodes`; T071 owns them.
- Do NOT add an `unread` counter to the SSE payload; the feeds screen refetches.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked

This task cannot run as written: the file predates the 2026-09-01 consistency review and its
interface contract diverges from the docs and the task suite in five places, so the
documented `§10.1` surface cannot be served inside the `## Files` table without deviating
from one authority or the other. Recorded rather than silently widened, matching the
record-then-repair workflow of T046 (#160/#161), T049 (#167/#168), T050 (#169/#170, #171/#172),
T052 (#184/#185), T057 (#201/#202), T058 (#204), T063 (#212/#214) and T064 (#217).

1. The contract structs cannot render the documented objects. `feeds.priority` exists in the
   schema (doc 04 §3.5, migration `00001_init.sql`) and is a member of the §10.1 feed object
   and of both the POST and PATCH member sets — it is the rule engine's per-run tie-break
   (doc 08 §5 step 13) — but the `Feed` struct above carries no field for it, so the feed
   object can neither return `priority` nor accept it on write. `feed_items.read` likewise
   exists (`idx_feed_items_read` was added with it in #12) and is a member of the §10.1 item
   object (`"read":false`), but `FeedItem` has no field for it. `ListFeedItems` returns
   `(items, next_cursor, error)` — no `total` — while step 8 and §1.4 require `total` in the
   envelope. And `unread_count`, a required member of every §10.1 feed object, is produced by
   none of the eleven functions. Remedy: amend the interface contract — add `Priority` to
   `Feed`, `Read` to `FeedItem`, a `total` return on `ListFeedItems` (or a sibling count
   function), and an `unread_count` source on the feed read path (a field populated by a
   correlated `read = 0` subquery in `ListFeeds`/`FeedByID`, or a batch count function). All
   of it lands in `internal/store/feeds.go`, which the Files table already admits; only the
   contract text needs the amendment.

2. The "read state is derived" paragraph is stale. It asserts doc 04 §3.5 has no read column;
   the section has carried `read INTEGER NOT NULL DEFAULT 0` and `idx_feed_items_read` since
   #12, and §10.1 defines `unread_count` and the `unread` filter on `read = 0`. Implementing
   the derived predicate `first_seen_at >= feeds.last_success_at` would resurrect a marked
   item as unread until the next successful poll — breaking the mark-read semantics #12 added
   the column for — and would leave the covering index dead schema. Remedy: rewrite the
   paragraph to the `read = 0` semantics of §10.1.

3. `auto_download` cannot be served inside this scope. §10.1 has `POST /feeds` and
   `PATCH /feeds/{id}` accept it — `true` creates the `auto:<feed_id>` rule, `false` deletes
   it, and `DELETE /feeds/{id}` removes it — but this task's own out-of-scope list forbids
   `rules` writes (T068 and T071 own them), and constructing a valid `auto:` `definition_json`
   needs T068's rule-document schema (doc 08). Silently dropping the member is an undetected
   no-op for the add-dialog's checkbox; inventing an undocumented `422` breaks the contract
   the other way. Remedy: name the carrier — either widen this task to a minimal auto-rule
   write (which imports T068's rule-document schema into this task), or defer the member to
   T068 or a new carrier via a deferral-register row, with the interim wire behaviour spelled
   out.

4. The mark-read endpoints are orphaned. `PATCH /feeds/{id}/items` and
   `POST /feeds/{id}/items/read-all` are defined in §10.1 (`{"updated":n}`, statuses
   `200`/`404`/`422`), doc 09 §8.4 builds **Mark read** / **Mark all read** on them, and
   T072 step 8 treats read state as server-derived — but no task's operation set or Files
   table includes them (checked T066, T067, T068, T072, T117). Remedy: name a carrier — the
   natural one is this task, since both endpoints live in `internal/api/feeds.go` over one
   or two more functions in `internal/store/feeds.go`, both already in the Files table — or
   a new task between T065 and T072.

5. `matched_rules` on the §10.1 item object reads `rule_matches`, which T071 owns; until then
   the member can only render `[]` or be omitted. Remedy: the repair names one of the two as
   the interim wire behaviour.

**Repair applied.** The contract and steps above now carry the remedies: `Feed` gained `Priority` and the
computed `UnreadCount`, `FeedItem` gained `Read`, `ListFeedItems` returns `total` through a
`FeedItemFilter`, and `SetFeedItemsRead`/`MarkAllFeedItemsRead` serve the two mark-read endpoints inside
the existing Files table (finding 4's named carrier: this task). The stale derived-read paragraph is
rewritten to `read = 0` semantics (finding 2), and the DTO rules spell out `priority`, `unread_count`,
RFC 3339 timestamps and the §10.1 credential-URL redaction the pre-review contract omitted (finding 1).
`auto_download` is deferred to T068 — which owns the `RuleDoc` schema a valid `definition_json` needs —
with the interim wire behaviour pinned: the member is absent from the write schemas, so a sender gets
`422` under `additionalProperties: false` instead of a silent no-op (finding 3). `matched_rules` renders
`[]` until T071 populates it from `rule_matches` (finding 5). Both deferrals are named in the register of
`00-task-index.md`, T068 and T071 carry the extra Files rows, and T072 gained the T068 edge so its
add-dialog checkbox can never post a member no merged task serves.
