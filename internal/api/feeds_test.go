package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/L-K-M/dl-tool/internal/store"
)

// createFeed posts one feed with the test bearer credential.
func (e *tasksTestEnv) createFeed(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Post("/feeds", body, "Authorization: Bearer "+e.bearer)
}

// getFeeds calls GET /feeds with the test bearer credential.
func (e *tasksTestEnv) getFeeds(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Get("/feeds", "Authorization: Bearer "+e.bearer)
}

// patchFeed patches one feed by id with the test bearer credential.
func (e *tasksTestEnv) patchFeed(t *testing.T, id string, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Patch("/feeds/"+id, body, "Authorization: Bearer "+e.bearer)
}

// deleteFeed deletes one feed by id with the test bearer credential.
func (e *tasksTestEnv) deleteFeed(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Delete("/feeds/"+id, "Authorization: Bearer "+e.bearer)
}

// getFeedItems calls GET /feeds/{id}/items with the test bearer
// credential; query carries the raw query string without the leading ?.
func (e *tasksTestEnv) getFeedItems(t *testing.T, id, query string) *httptest.ResponseRecorder {
	t.Helper()

	path := "/feeds/" + id + "/items"
	if query != "" {
		path += "?" + query
	}

	return e.api.Get(path, "Authorization: Bearer "+e.bearer)
}

// markFeedItems calls PATCH /feeds/{id}/items with the test bearer
// credential.
func (e *tasksTestEnv) markFeedItems(t *testing.T, id string, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Patch("/feeds/"+id+"/items", body, "Authorization: Bearer "+e.bearer)
}

// markAllFeedItemsRead calls POST /feeds/{id}/items/read-all with the test
// bearer credential.
func (e *tasksTestEnv) markAllFeedItemsRead(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Post("/feeds/"+id+"/items/read-all", "Authorization: Bearer "+e.bearer)
}

// decodeFeedList decodes the GET /feeds envelope.
func decodeFeedList(t *testing.T, recorder *httptest.ResponseRecorder) []FeedDTO {
	t.Helper()

	var body struct {
		Feeds []FeedDTO `json:"feeds"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body.Feeds
}

// decodeFeedBody decodes the flat feed object POST and PATCH return.
func decodeFeedBody(t *testing.T, recorder *httptest.ResponseRecorder) FeedDTO {
	t.Helper()

	var body FeedDTO
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body
}

// feedItemsPage is the decoded GET /feeds/{id}/items envelope.
type feedItemsPage struct {
	Items      []FeedItemDTO `json:"items"`
	NextCursor *string       `json:"next_cursor"`
	Total      int           `json:"total"`
}

// decodeFeedItemsPage decodes the cursor pagination envelope.
func decodeFeedItemsPage(t *testing.T, recorder *httptest.ResponseRecorder) feedItemsPage {
	t.Helper()

	var body feedItemsPage
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body
}

// decodeUpdated decodes the {"updated":n} body of both mark-read
// operations.
func decodeUpdated(t *testing.T, recorder *httptest.ResponseRecorder) int64 {
	t.Helper()

	var body struct {
		Updated int64 `json:"updated"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body.Updated
}

// seedFeed writes one feed row through the API and returns its id, so item
// fixtures land on a real row.
func (e *tasksTestEnv) seedFeed(t *testing.T, url string) string {
	t.Helper()

	response := e.createFeed(t, map[string]any{"url": url})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}

	return decodeFeedBody(t, response).ID
}

// seedFeedItems upserts one batch straight through the store, so a test
// pins items without the fetch the poller owns. publishedAt bases the
// newest-first ordering: item i carries base+i.
func (e *tasksTestEnv) seedFeedItems(t *testing.T, feedID string, count int, base int64) []store.FeedItem {
	t.Helper()

	items := make([]store.FeedItem, count)
	for i := range items {
		published := base + int64(i)
		items[i] = store.FeedItem{
			FeedID:      feedID,
			Identity:    fmt.Sprintf("identity-%d", i),
			Title:       fmt.Sprintf("item-%d", i),
			TitleNorm:   fmt.Sprintf("item %d", i),
			PublishedAt: &published,
		}
	}
	if _, err := store.UpsertFeedItems(t.Context(), e.db, items, time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed feed items: %v", err)
	}

	return items
}

// feedItemRows reads the feed_items rows of one feed straight from the
// store, ordered like the API page.
func (e *tasksTestEnv) feedItemRows(t *testing.T, feedID string) []store.FeedItem {
	t.Helper()

	items, _, _, err := store.ListFeedItems(t.Context(), e.db, store.FeedItemFilter{FeedIDs: []string{feedID}})
	if err != nil {
		t.Fatalf("list feed items: %v", err)
	}

	return items
}

// TestFeedCrud pins doc 05 section 10.1: create returns 201 with the feed
// object, the list is title-ordered, patch merges the provided fields,
// delete is 204 — and an empty list encodes [], never null.
func TestFeedCrud(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.getFeeds(t)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"feeds":[]`) {
		t.Errorf("empty feed list = %s, want \"feeds\":[]", response.Body.String())
	}

	response = env.createFeed(t, map[string]any{
		"url": "https://archlinux.org/feeds/releases/", "title": "Arch Linux releases",
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	created := decodeFeedBody(t, response)
	if !strings.HasPrefix(created.ID, store.PrefixFeed) ||
		created.URL != "https://archlinux.org/feeds/releases/" ||
		created.Title == nil || *created.Title != "Arch Linux releases" ||
		!created.Enabled {
		t.Errorf("created = %+v, want the submitted fields and enabled by default", created)
	}

	response = env.createFeed(t, map[string]any{"url": "https://debian.org/feeds/", "title": "Debian"})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}

	feeds := decodeFeedList(t, env.getFeeds(t))
	if len(feeds) != 2 || *feeds[0].Title != "Arch Linux releases" || *feeds[1].Title != "Debian" {
		t.Fatalf("feeds = %+v, want [Arch Linux releases, Debian] sorted by title", feeds)
	}

	// The patch merges: url survives an enabled-only write.
	response = env.patchFeed(t, created.ID, map[string]any{"enabled": false})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	patched := decodeFeedBody(t, response)
	if patched.Enabled || patched.URL != created.URL || patched.Title == nil || *patched.Title != *created.Title {
		t.Errorf("patched = %+v, want enabled false with url and title untouched", patched)
	}

	response = env.deleteFeed(t, created.ID)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusNoContent, response.Body.String())
	}
	response = env.deleteFeed(t, created.ID)
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)

	response = env.patchFeed(t, created.ID, map[string]any{"enabled": true})
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
}

// TestDuplicateFeedURLConflicts pins the 409 of doc 05 section 10.1: a url
// already taken conflicts on create and on patch — never a silent merge.
func TestDuplicateFeedURLConflicts(t *testing.T) {
	env := newTasksTestEnv(t)

	first := map[string]any{"url": "https://archlinux.org/feeds/releases/"}
	if response := env.createFeed(t, first); response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	second := env.seedFeed(t, "https://debian.org/feeds/")

	response := env.createFeed(t, first)
	assertProblem(t, response, http.StatusConflict, SlugConflict)

	response = env.patchFeed(t, second, map[string]any{"url": "https://archlinux.org/feeds/releases/"})
	assertProblem(t, response, http.StatusConflict, SlugConflict)

	feeds := decodeFeedList(t, env.getFeeds(t))
	if len(feeds) != 2 {
		t.Errorf("feeds = %+v, want both rows intact", feeds)
	}
}

// TestFeedValidation pins the 422s of doc 05 section 10.1: a non-http(s)
// url, a refresh_interval_s below 300 and not 0, and an item_cap below 0 —
// on create and on patch alike.
func TestFeedValidation(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.createFeed(t, map[string]any{"url": "ftp://mirror.example.org/feed"})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	response = env.createFeed(t, map[string]any{"url": "not a url"})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	response = env.createFeed(t, map[string]any{
		"url": "https://archlinux.org/feeds/releases/", "refresh_interval_s": 60,
	})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	response = env.createFeed(t, map[string]any{
		"url": "https://archlinux.org/feeds/releases/", "item_cap": -1,
	})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	feedID := env.seedFeed(t, "https://archlinux.org/feeds/releases/")

	response = env.patchFeed(t, feedID, map[string]any{"url": "ftp://mirror.example.org/feed"})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	response = env.patchFeed(t, feedID, map[string]any{"refresh_interval_s": 60})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	response = env.patchFeed(t, feedID, map[string]any{"item_cap": -1})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	// The documented boundary values pass.
	response = env.patchFeed(t, feedID, map[string]any{"refresh_interval_s": 300, "item_cap": 0})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	patched := decodeFeedBody(t, response)
	if patched.RefreshIntervalS != 300 || patched.ItemCap != 0 {
		t.Errorf("patched = %+v, want refresh_interval_s 300 and item_cap 0", patched)
	}
}

// TestFeedItemsPagingNewestFirst pins the doc 05 section 10.1 item page:
// COALESCE(published_at, first_seen_at) DESC with the id tie-break, the
// cursor returns the next page without overlap, next_cursor is null on the
// last page, total counts the filter ignoring cursor and limit, and a
// cursor minted under another feed or filter is 422.
func TestFeedItemsPagingNewestFirst(t *testing.T) {
	env := newTasksTestEnv(t)

	feedID := env.seedFeed(t, "https://archlinux.org/feeds/releases/")
	otherID := env.seedFeed(t, "https://debian.org/feeds/")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	seeded := env.seedFeedItems(t, feedID, 5, base)
	env.seedFeedItems(t, otherID, 2, base)

	// Newest first: seeded[i] is oldest when i is smallest.
	response := env.getFeedItems(t, feedID, "limit=2")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	page := decodeFeedItemsPage(t, response)
	if page.Total != 5 {
		t.Errorf("total = %d, want 5 ignoring the limit", page.Total)
	}
	if page.NextCursor == nil {
		t.Fatalf("next_cursor = null on a non-last page; body %s", response.Body.String())
	}

	seen := map[string]bool{}
	var lastKey int64
	for pageNumber := 0; ; pageNumber++ {
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatalf("item %s appeared on two pages", item.ID)
			}
			seen[item.ID] = true
			key := feedItemSortKeyOf(t, item)
			if pageNumber > 0 || len(seen) > 1 {
				if key > lastKey {
					t.Errorf("item %s sort key %d follows %d — not newest first", item.ID, key, lastKey)
				}
			}
			lastKey = key
		}
		if page.NextCursor == nil {
			break
		}
		response = env.getFeedItems(t, feedID, "limit=2&cursor="+*page.NextCursor)
		if response.Code != http.StatusOK {
			t.Fatalf("page %d status = %d, want %d; body %s", pageNumber+1, response.Code, http.StatusOK, response.Body.String())
		}
		page = decodeFeedItemsPage(t, response)
		if page.Total != 5 {
			t.Errorf("page %d total = %d, want 5 ignoring the cursor", pageNumber+1, page.Total)
		}
		if pageNumber > len(seeded) {
			t.Fatal("paging did not terminate")
		}
	}
	if len(seen) != len(seeded) {
		t.Errorf("paged through %d items, want %d", len(seen), len(seeded))
	}
	for _, item := range seeded {
		if !seen[env.feedItemID(t, item)] {
			t.Errorf("seeded item %q never listed", item.Identity)
		}
	}

	// A cursor minted for one feed is stale under another.
	first := env.getFeedItems(t, feedID, "limit=2")
	cursor := *decodeFeedItemsPage(t, first).NextCursor
	response = env.getFeedItems(t, otherID, "limit=2&cursor="+cursor)
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	// An unknown feed id is 404 before the filter runs, and a token that
	// is not a cursor at all is 422.
	response = env.getFeedItems(t, "fed_01JKQ8Z9YV6M3P0R2S4T6V8W0X", "")
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
	response = env.getFeedItems(t, feedID, "cursor=not-a-cursor")
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
}

// feedItemSortKeyOf decodes the page's ordering claim for one listed item:
// published_at when present. The seeds always carry published_at, so a null
// here would be a rendering bug worth failing on.
func feedItemSortKeyOf(t *testing.T, item FeedItemDTO) int64 {
	t.Helper()

	if item.PublishedAt == nil {
		t.Fatalf("item %s published_at = null; the seed wrote one", item.ID)
	}
	parsed, err := time.Parse(time.RFC3339, *item.PublishedAt)
	if err != nil {
		t.Fatalf("item %s published_at %q is not RFC 3339: %v", item.ID, *item.PublishedAt, err)
	}

	return parsed.UnixMilli()
}

// feedItemID resolves a seeded item's store id through the listing.
func (e *tasksTestEnv) feedItemID(t *testing.T, item store.FeedItem) string {
	t.Helper()

	for _, row := range e.feedItemRows(t, item.FeedID) {
		if row.Identity == item.Identity {
			return row.ID
		}
	}
	t.Fatalf("seeded item %q of feed %s not found", item.Identity, item.FeedID)

	return ""
}

// TestDeleteFeedCascadesItems pins the doc 05 section 10.1 delete: 204, and
// ON DELETE CASCADE leaves zero feed_items rows.
func TestDeleteFeedCascadesItems(t *testing.T) {
	env := newTasksTestEnv(t)

	feedID := env.seedFeed(t, "https://archlinux.org/feeds/releases/")
	env.seedFeedItems(t, feedID, 3, time.Now().UnixMilli())

	response := env.deleteFeed(t, feedID)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusNoContent, response.Body.String())
	}

	var count int
	if err := env.db.GetContext(
		t.Context(), &count, `SELECT COUNT(*) FROM feed_items WHERE feed_id = ?`, feedID,
	); err != nil {
		t.Fatalf("count feed items: %v", err)
	}
	if count != 0 {
		t.Errorf("%d feed_items rows survived the cascade, want 0", count)
	}
}

// TestUpsertFeedItemsIsIdempotent pins the contract's upsert: a second
// upsert of the same batch adds 0 rows and keeps read and first_seen_at
// untouched, and a repeated identity inside one batch creates one row.
func TestUpsertFeedItemsIsIdempotent(t *testing.T) {
	env := newTasksTestEnv(t)
	ctx := t.Context()

	feedID := env.seedFeed(t, "https://archlinux.org/feeds/releases/")
	base := time.Now().UnixMilli()
	items := []store.FeedItem{
		{FeedID: feedID, Identity: "a", Title: "first", TitleNorm: "first"},
		{FeedID: feedID, Identity: "b", Title: "second", TitleNorm: "second"},
		// A repeated identity inside one batch updates, never duplicates.
		{FeedID: feedID, Identity: "b", Title: "second renamed", TitleNorm: "second renamed"},
	}

	added, err := store.UpsertFeedItems(ctx, env.db, items, base)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if added != 2 {
		t.Fatalf("first upsert added %d, want 2 unique identities", added)
	}
	if rows := env.feedItemRows(t, feedID); len(rows) != 2 {
		t.Fatalf("%d feed_items rows, want 2", len(rows))
	}

	// Mark one row read, then re-upsert with a changed title: the count
	// is 0, the title refreshes, and read and first_seen_at survive.
	if _, err := store.MarkAllFeedItemsRead(ctx, env.db, feedID, base); err != nil {
		t.Fatalf("mark all read: %v", err)
	}
	added, err = store.UpsertFeedItems(ctx, env.db, []store.FeedItem{
		{FeedID: feedID, Identity: "a", Title: "first renamed", TitleNorm: "first renamed"},
		{FeedID: feedID, Identity: "b", Title: "second again", TitleNorm: "second again"},
	}, base+1000)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if added != 0 {
		t.Errorf("second upsert added %d, want 0", added)
	}

	rows := env.feedItemRows(t, feedID)
	if len(rows) != 2 {
		t.Fatalf("%d feed_items rows after re-upsert, want 2", len(rows))
	}
	for _, row := range rows {
		if !row.Read {
			t.Errorf("item %q read reset to false by the re-upsert", row.Identity)
		}
		if row.FirstSeenAt != base {
			t.Errorf("item %q first_seen_at = %d, want the insert-time %d", row.Identity, row.FirstSeenAt, base)
		}
	}
}

// TestFeedObjectShape pins the section 10.1 feed object: priority and
// unread_count ride along, the fetch timestamps render RFC 3339, and no
// auto_download member exists on the wire until T068 adds it.
func TestFeedObjectShape(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.createFeed(t, map[string]any{
		"url": "https://archlinux.org/feeds/releases/", "priority": 3,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "auto_download") {
		t.Errorf("feed object carries auto_download before T068: %s", response.Body.String())
	}

	created := decodeFeedBody(t, response)
	if created.Priority != 3 {
		t.Errorf("priority = %d, want 3", created.Priority)
	}
	if created.UnreadCount != 0 {
		t.Errorf("unread_count = %d, want 0", created.UnreadCount)
	}
	nextFetch, err := time.Parse(time.RFC3339, created.NextFetchAt)
	if err != nil || !strings.HasSuffix(created.NextFetchAt, "Z") {
		t.Fatalf("next_fetch_at = %q, want RFC 3339 UTC ending in Z (%v)", created.NextFetchAt, err)
	}
	if nextFetch.After(time.Now().Add(time.Minute)) || nextFetch.Before(time.Now().Add(-time.Minute)) {
		t.Errorf("next_fetch_at = %q, want now — the next poll pass picks the feed up", created.NextFetchAt)
	}
	if created.LastFetchAt != nil || created.LastSuccessAt != nil || created.DisabledTill != nil {
		t.Errorf("created = %+v, want the fetch timestamps null before the first poll", created)
	}

	// unread_count tracks the read = 0 predicate.
	env.seedFeedItems(t, created.ID, 2, time.Now().UnixMilli())
	feeds := decodeFeedList(t, env.getFeeds(t))
	if len(feeds) != 1 || feeds[0].UnreadCount != 2 {
		t.Fatalf("feeds = %+v, want one feed with unread_count 2", feeds)
	}
}

// TestCredentialFeedURLIsRedacted pins the section 10.1 feed rule: a
// credential-bearing url is redacted member-wise on read — userinfo and
// every name isSecretQueryParameter recognises — while the stored row
// keeps the secret, and a PATCH echoing the redacted url is a no-op on the
// stored column.
func TestCredentialFeedURLIsRedacted(t *testing.T) {
	env := newTasksTestEnv(t)

	userinfo := env.seedFeed(t, "https://user:pass@tracker.example.com/feed.xml")
	for _, name := range secretQueryParameters {
		env.seedFeed(t, "https://tracker.example.com/feed.xml?"+name+"=sekret&genre=iso")
	}
	feeds := decodeFeedList(t, env.getFeeds(t))
	if len(feeds) != 1+len(secretQueryParameters) {
		t.Fatalf("feeds = %+v, want %d rows", feeds, 1+len(secretQueryParameters))
	}
	for _, feed := range feeds {
		if strings.Contains(feed.URL, "pass@") || strings.Contains(feed.URL, "sekret") {
			t.Errorf("feed %s url leaks a credential: %q", feed.ID, feed.URL)
		}
		if !strings.Contains(feed.URL, redactedValue) {
			t.Errorf("feed %s url = %q, want __redacted__ members", feed.ID, feed.URL)
		}
		// The non-secret member stays readable.
		if feed.ID != userinfo && !strings.Contains(feed.URL, "genre=iso") {
			t.Errorf("feed %s url = %q, want the genre member kept", feed.ID, feed.URL)
		}
	}
	for _, feed := range feeds {
		if feed.ID == userinfo && feed.URL != "https://"+redactedValue+"@tracker.example.com/feed.xml" {
			t.Errorf("userinfo url = %q, want https://__redacted__@tracker.example.com/feed.xml", feed.URL)
		}
	}

	// The stored row keeps the secret: the poller fetches it verbatim.
	var stored string
	if err := env.db.GetContext(
		t.Context(), &stored, `SELECT url FROM feeds WHERE id = ?`, userinfo,
	); err != nil {
		t.Fatalf("read stored url: %v", err)
	}
	if stored != "https://user:pass@tracker.example.com/feed.xml" {
		t.Fatalf("stored url = %q, want the verbatim credential URL", stored)
	}

	// A PATCH echoing the redacted url is a no-op on the column.
	response := env.patchFeed(t, userinfo, map[string]any{
		"url": "https://" + redactedValue + "@tracker.example.com/feed.xml",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	if err := env.db.GetContext(
		t.Context(), &stored, `SELECT url FROM feeds WHERE id = ?`, userinfo,
	); err != nil {
		t.Fatalf("re-read stored url: %v", err)
	}
	if stored != "https://user:pass@tracker.example.com/feed.xml" {
		t.Errorf("stored url = %q after a redacted PATCH, want it untouched", stored)
	}
}

// TestUnreadFilterMatchesReadColumn pins the read = 0 predicate of doc 05
// section 10.1: unread=true returns only those rows, unread_count tracks
// the same predicate, and total counts the filter ignoring cursor and
// limit — it drops under unread=true.
func TestUnreadFilterMatchesReadColumn(t *testing.T) {
	env := newTasksTestEnv(t)

	feedID := env.seedFeed(t, "https://archlinux.org/feeds/releases/")
	seeded := env.seedFeedItems(t, feedID, 4, time.Now().UnixMilli())

	// Two of the four go read.
	readIDs := []string{
		env.feedItemID(t, seeded[0]),
		env.feedItemID(t, seeded[1]),
	}
	response := env.markFeedItems(t, feedID, map[string]any{"ids": readIDs, "read": true})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}

	response = env.getFeedItems(t, feedID, "unread=true&limit=1")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	page := decodeFeedItemsPage(t, response)
	if page.Total != 2 {
		t.Errorf("unread total = %d, want 2 — the filter, ignoring the limit", page.Total)
	}
	for _, item := range page.Items {
		if item.Read {
			t.Errorf("unread page carried read item %s", item.ID)
		}
	}
	if page.NextCursor == nil {
		t.Fatal("unread page 1 next_cursor = null, want a second page")
	}
	page = decodeFeedItemsPage(t, env.getFeedItems(t, feedID, "unread=true&limit=1&cursor="+*page.NextCursor))
	if page.Total != 2 {
		t.Errorf("unread page 2 total = %d, want 2 ignoring the cursor", page.Total)
	}
	for _, item := range page.Items {
		if item.Read {
			t.Errorf("unread page 2 carried read item %s", item.ID)
		}
	}
	if page.NextCursor != nil {
		t.Errorf("unread last page next_cursor = %q, want null", *page.NextCursor)
	}

	feeds := decodeFeedList(t, env.getFeeds(t))
	if len(feeds) != 1 || feeds[0].UnreadCount != 2 {
		t.Errorf("feeds = %+v, want unread_count tracking read = 0 at 2", feeds)
	}

	// A cursor minted under unread=false is stale under unread=true.
	all := decodeFeedItemsPage(t, env.getFeedItems(t, feedID, "limit=1"))
	response = env.getFeedItems(t, feedID, "unread=true&limit=1&cursor="+*all.NextCursor)
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
}

// TestMarkFeedItemsRead pins the PATCH /feeds/{id}/items contract: updated
// counts only rows whose state changed, ids of a different feed are
// skipped, and a repeat is a reported no-op.
func TestMarkFeedItemsRead(t *testing.T) {
	env := newTasksTestEnv(t)

	feedID := env.seedFeed(t, "https://archlinux.org/feeds/releases/")
	otherID := env.seedFeed(t, "https://debian.org/feeds/")
	seeded := env.seedFeedItems(t, feedID, 3, time.Now().UnixMilli())
	foreign := env.seedFeedItems(t, otherID, 1, time.Now().UnixMilli())

	ids := []string{
		env.feedItemID(t, seeded[0]),
		env.feedItemID(t, seeded[1]),
		env.feedItemID(t, foreign[0]),    // another feed's id is skipped
		"itm_01JKQ8Z9YV6M3P0R2S4T6V8W0X", // no row is skipped
	}
	response := env.markFeedItems(t, feedID, map[string]any{"ids": ids, "read": true})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	if updated := decodeUpdated(t, response); updated != 2 {
		t.Errorf("updated = %d, want 2 — only the rows whose state changed", updated)
	}

	// Idempotent: the same write changes nothing.
	response = env.markFeedItems(t, feedID, map[string]any{"ids": ids, "read": true})
	if updated := decodeUpdated(t, response); updated != 0 {
		t.Errorf("repeat updated = %d, want 0", updated)
	}

	// The foreign item is still unread — the skip is not a leak.
	foreignRows := env.feedItemRows(t, otherID)
	if len(foreignRows) != 1 || foreignRows[0].Read {
		t.Errorf("foreign feed items = %+v, want the one row still unread", foreignRows)
	}

	// Marking one back unread counts one change.
	response = env.markFeedItems(t, feedID, map[string]any{"ids": []string{ids[0]}, "read": false})
	if updated := decodeUpdated(t, response); updated != 1 {
		t.Errorf("unread updated = %d, want 1", updated)
	}

	// The body's own 422s: an empty ids, more than 500, a missing read.
	response = env.markFeedItems(t, feedID, map[string]any{"ids": []string{}, "read": true})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	tooMany := make([]string, 501)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("itm_%026d", i)
	}
	response = env.markFeedItems(t, feedID, map[string]any{"ids": tooMany, "read": true})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	response = env.markFeedItems(t, feedID, map[string]any{"ids": ids[:1]})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	// An unknown feed is 404.
	response = env.markFeedItems(t, "fed_01JKQ8Z9YV6M3P0R2S4T6V8W0X", map[string]any{"ids": ids[:1], "read": true})
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
}

// TestMarkAllFeedItemsReadIsIdempotent pins the read-all contract of doc 05
// section 10.1: the first call counts the rows it changed, the second is a
// reported {"updated":0}, and an unknown feed is 404.
func TestMarkAllFeedItemsReadIsIdempotent(t *testing.T) {
	env := newTasksTestEnv(t)

	feedID := env.seedFeed(t, "https://archlinux.org/feeds/releases/")
	env.seedFeedItems(t, feedID, 3, time.Now().UnixMilli())

	response := env.markAllFeedItemsRead(t, feedID)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	if updated := decodeUpdated(t, response); updated != 3 {
		t.Errorf("updated = %d, want 3", updated)
	}

	response = env.markAllFeedItemsRead(t, feedID)
	if updated := decodeUpdated(t, response); updated != 0 {
		t.Errorf("second read-all updated = %d, want 0", updated)
	}

	response = env.markAllFeedItemsRead(t, "fed_01JKQ8Z9YV6M3P0R2S4T6V8W0X")
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
}

// TestAutoDownloadIsRejectedUntilT068 pins the interim wire behaviour of
// the deferral register: auto_download is absent from both write schemas,
// so a sender gets 422 under additionalProperties: false on POST and on
// PATCH — never a silent no-op.
func TestAutoDownloadIsRejectedUntilT068(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.createFeed(t, map[string]any{
		"url": "https://archlinux.org/feeds/releases/", "auto_download": true,
	})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	feedID := env.seedFeed(t, "https://archlinux.org/feeds/releases/")
	response = env.patchFeed(t, feedID, map[string]any{"auto_download": false})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
}
