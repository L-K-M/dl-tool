package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/rss"
	"github.com/L-K-M/dl-tool/internal/store"
)

const (
	operationListFeeds            = "list-feeds"
	operationCreateFeed           = "create-feed"
	operationPatchFeed            = "patch-feed"
	operationDeleteFeed           = "delete-feed"
	operationListFeedItems        = "list-feed-items"
	operationMarkFeedItems        = "mark-feed-items"
	operationMarkAllFeedItemsRead = "mark-all-feed-items-read"
	operationRefreshFeed          = "refresh-feed"

	feedURLDetail              = "the url must be an absolute http or https URL"
	feedRefreshIntervalDetail  = "refresh_interval_s is 0 for the global RSS interval or at least 300 seconds"
	feedConflictDetail         = "a feed with that url already exists"
	feedItemsPatchDetail       = "ids carries 1..500 item ids and read is required"
	feedRefreshIntervalMinimum = 300
)

// FeedDTO renders the feed object of docs/05-api-contract.md section 10.1:
// timestamps are RFC 3339 or null (next_fetch_at is a NOT NULL column, so it
// is always present), and a credential-bearing url is redacted member-wise
// (feedDTO). There is no auto_download member — the member is write-only on
// POST and PATCH, and the feed object never carries it on read.
type FeedDTO struct {
	ID               string  `json:"id"`
	URL              string  `json:"url"              doc:"The feed URL; userinfo and apikey/token/passkey query values render as __redacted__"`
	Title            *string `json:"title"`
	Enabled          bool    `json:"enabled"`
	RefreshIntervalS int     `json:"refresh_interval_s" doc:"Seconds between polls; 0 uses the global RSS interval"`
	ItemCap          int     `json:"item_cap"           doc:"Retained feed_items rows for this feed"`
	Priority         int     `json:"priority"           doc:"Per-run tie-break of the rule engine; lower wins"`
	UnreadCount      int     `json:"unread_count"       doc:"Items of this feed with read = false"`
	LastFetchAt      *string `json:"last_fetch_at"      format:"date-time"`
	LastSuccessAt    *string `json:"last_success_at"    format:"date-time"`
	NextFetchAt      string  `json:"next_fetch_at"      format:"date-time"`
	EscalationLevel  int     `json:"escalation_level"`
	DisabledTill     *string `json:"disabled_till"      format:"date-time"`
	LastError        *string `json:"last_error"`
}

// FeedItemMatchedRuleDTO is one entry of the item object's matched_rules
// member. T071 populates it from rule_matches; until then the member renders
// [] (deferral register).
type FeedItemMatchedRuleDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// FeedItemDTO renders the item object of docs/05-api-contract.md section
// 10.1: published_at is RFC 3339 or null and matched_rules is [] until T071
// joins rule_matches.
type FeedItemDTO struct {
	ID           string                   `json:"id"`
	FeedID       string                   `json:"feed_id"`
	Title        string                   `json:"title"`
	Link         *string                  `json:"link"`
	DownloadURL  *string                  `json:"download_url"`
	InfoHash     *string                  `json:"info_hash"`
	SizeBytes    *int64                   `json:"size_bytes"`
	PublishedAt  *string                  `json:"published_at" format:"date-time"`
	Read         bool                     `json:"read"`
	MatchedRules []FeedItemMatchedRuleDTO `json:"matched_rules"`
}

// CreateFeedInput is the JSON body of POST /feeds. auto_download is
// write-only (doc 05 section 10.1): true creates the enabled auto:<feed_id>
// rule scoped to this feed's url; the feed object never carries the member
// on read.
type CreateFeedInput struct {
	Body struct {
		URL              string  `json:"url"                required:"true" minLength:"1" doc:"http or https feed URL; userinfo and passkey-style query secrets are stored but never returned; a url containing __redacted__ is rejected — it is a rendered form, not a fetchable address"`
		Title            *string `json:"title,omitempty"`
		Enabled          *bool   `json:"enabled,omitempty"           doc:"Default true"`
		RefreshIntervalS *int    `json:"refresh_interval_s,omitempty" minimum:"0" doc:"Seconds between polls; 0 uses the global RSS interval, otherwise at least 300"`
		ItemCap          *int    `json:"item_cap,omitempty"           minimum:"0" doc:"Retained items per feed; default 50"`
		Priority         *int    `json:"priority,omitempty"           doc:"Rule-engine per-run tie-break; lower wins; default 0"`
		AutoDownload     bool    `json:"auto_download,omitempty"      doc:"true creates the auto:<feed_id> rule that grabs every item of this feed"`
	}
}

// PatchFeedInput addresses one feed by id. The body fields are pointers: an
// omitted field is nil and stays untouched. A url that still carries a
// __redacted__ member is a no-op on the stored column — the same write-back
// rule the indexer's api_key and extract_passwords carry.
type PatchFeedInput struct {
	ID   string `path:"id" doc:"The fed_ id of the feed"`
	Body struct {
		URL              *string `json:"url,omitempty"              minLength:"1" doc:"http or https feed URL; resubmitting a __redacted__ rendering leaves the stored url unchanged"`
		Title            *string `json:"title,omitempty"`
		Enabled          *bool   `json:"enabled,omitempty"`
		RefreshIntervalS *int    `json:"refresh_interval_s,omitempty" minimum:"0"`
		ItemCap          *int    `json:"item_cap,omitempty"           minimum:"0"`
		Priority         *int    `json:"priority,omitempty"`
		// AutoDownload is a pointer so an omitted or null member leaves the
		// auto:<feed_id> rule untouched: only an explicit false deletes it.
		AutoDownload *bool `json:"auto_download,omitempty" doc:"true creates the auto:<feed_id> rule, false deletes it"`
	}
}

// DeleteFeedInput addresses one feed by id.
type DeleteFeedInput struct {
	ID string `path:"id" doc:"The fed_ id of the feed"`
}

// ListFeedItemsInput is the query of GET /feeds/{id}/items
// (docs/05-api-contract.md section 10.1): the section's own bounds —
// default 50, range 1..200 — rather than section 1.4's.
type ListFeedItemsInput struct {
	ID     string `path:"id"    doc:"The fed_ id of the feed"`
	Limit  int    `query:"limit"  minimum:"1" maximum:"200" default:"50" doc:"Page size"`
	Cursor string `query:"cursor" doc:"Opaque page token from a previous response"`
	Unread bool   `query:"unread" doc:"Only items with read = false"`
}

// MarkFeedItemsInput is PATCH /feeds/{id}/items: the listed ids of this one
// feed go read or unread. ids of another feed, or of no row, are skipped —
// the call is idempotent.
type MarkFeedItemsInput struct {
	ID   string `path:"id" doc:"The fed_ id of the feed"`
	Body struct {
		IDs  []string `json:"ids"  required:"true" minItems:"1" maxItems:"500" doc:"itm_ ids of this feed's items"`
		Read *bool    `json:"read" required:"true"                            doc:"true marks read, false marks unread"`
	}
}

// MarkAllFeedItemsReadInput addresses one feed by id; the operation takes no
// body.
type MarkAllFeedItemsReadInput struct {
	ID string `path:"id" doc:"The fed_ id of the feed"`
}

// RefreshFeedInput addresses one feed by id; the operation takes no body.
type RefreshFeedInput struct {
	ID string `path:"id" doc:"The fed_ id of the feed"`
}

// ListFeedsOutput is the GET /feeds body.
type ListFeedsOutput struct {
	Body struct {
		Feeds []FeedDTO `json:"feeds"`
	}
}

// FeedOutput carries 201 from Create and 200 from Patch.
type FeedOutput struct {
	Status int `json:"-"`
	Body   FeedDTO
}

// ListFeedItemsOutput is the cursor pagination envelope of doc 05 section
// 1.4 carrying the section 10.1 item objects.
type ListFeedItemsOutput struct {
	Body struct {
		Items      []FeedItemDTO `json:"items"`
		NextCursor *string       `json:"next_cursor" doc:"Token for the next page; null on the last page"`
		Total      int           `json:"total"       doc:"Rows matching the filter, ignoring the cursor"`
	}
}

// FeedItemsUpdatedOutput is the {"updated":n} answer of both mark-read
// operations: n counts only the rows whose state changed.
type FeedItemsUpdatedOutput struct {
	Body struct {
		Updated int64 `json:"updated" doc:"Rows whose read state changed"`
	}
}

// RefreshFeedOutput is the poll Result of POST /feeds/{id}/refresh —
// fetched, not_modified, items_added, elapsed_ms and an optional error
// (docs/05-api-contract.md section 10.1).
type RefreshFeedOutput struct {
	Body rss.Result
}

// FeedHandlers owns the feed operations of docs/05-api-contract.md section
// 10.1 over the feeds and feed_items tables.
type FeedHandlers struct {
	db     *sqlx.DB
	poller *rss.Poller
}

// NewFeedHandlers builds the feed handlers over db and wires the poller the
// refresh endpoint and the rss_poll job share: the guarded client hc and the
// item parser arrive from the composition root, so the endpoint and the job
// poll through the same SSRF-guarded client (docs/14-conventions.md section
// 8.3). The parser is nil until T067 lands parse.go; the poller reports that
// as a fetch failure rather than panic. A nil db or hc — the document-only
// builds — leaves poller nil and refresh answers 503.
func NewFeedHandlers(db *sqlx.DB, hc *http.Client, parser rss.ItemParser, log *slog.Logger) *FeedHandlers {
	h := &FeedHandlers{db: db}
	if db != nil && hc != nil {
		h.poller = rss.NewPoller(db, hc, parser, log, time.Now)
	}

	return h
}

// Register mounts the eight operations on the Huma API;
// Server.registerOperations is the call site.
func (h *FeedHandlers) Register(hapi huma.API) {
	huma.Register(hapi, huma.Operation{
		OperationID: operationListFeeds,
		Method:      http.MethodGet,
		Path:        "/feeds",
		Summary:     "List the feeds",
		Description: "Every feed ordered by title, each carrying its unread item count. A credential-bearing url is returned member-redacted: userinfo and the apikey, token and passkey query values render as __redacted__.",
		Tags:        []string{"feeds"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.List)

	huma.Register(hapi, huma.Operation{
		OperationID:   operationCreateFeed,
		Method:        http.MethodPost,
		Path:          "/feeds",
		DefaultStatus: http.StatusCreated,
		Summary:       "Create a feed",
		Description:   "Creates one RSS feed; next_fetch_at is now, so the next poll pass picks it up. A duplicate url is 409 /problems/conflict. The url must be http or https; refresh_interval_s is 0 for the global interval or at least 300; item_cap is non-negative.",
		Tags:          []string{"feeds"},
		Security:      credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Create)

	huma.Register(hapi, huma.Operation{
		OperationID: operationPatchFeed,
		Method:      http.MethodPatch,
		Path:        "/feeds/{id}",
		Summary:     "Update a feed",
		Description: "Partial update of url, title, enabled, refresh_interval_s, item_cap and priority; omitted fields are untouched. A url that still contains __redacted__ leaves the stored url unchanged. Renaming onto an existing url is 409 /problems/conflict. The fetch-state columns are the poller's and survive the write.",
		Tags:        []string{"feeds"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Patch)

	huma.Register(hapi, huma.Operation{
		OperationID:   operationDeleteFeed,
		Method:        http.MethodDelete,
		Path:          "/feeds/{id}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Delete a feed",
		Description:   "Removes the feed row; its stored items go with it through ON DELETE CASCADE.",
		Tags:          []string{"feeds"},
		Security:      credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Delete)

	huma.Register(hapi, huma.Operation{
		OperationID: operationListFeedItems,
		Method:      http.MethodGet,
		Path:        "/feeds/{id}/items",
		Summary:     "List a feed's items",
		Description: "Cursor-paginated, newest first, with an optional unread filter. A cursor is bound to the feed and filter that issued it — reusing it under another filter is 422.",
		Tags:        []string{"feeds"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Items)

	huma.Register(hapi, huma.Operation{
		OperationID: operationMarkFeedItems,
		Method:      http.MethodPatch,
		Path:        "/feeds/{id}/items",
		Summary:     "Mark feed items read or unread",
		Description: "Marks the listed items of this one feed read or unread and reports how many rows changed state. Ids of another feed, or of no row, are skipped, so the call is idempotent.",
		Tags:        []string{"feeds"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.MarkItems)

	huma.Register(hapi, huma.Operation{
		OperationID: operationMarkAllFeedItemsRead,
		Method:      http.MethodPost,
		Path:        "/feeds/{id}/items/read-all",
		Summary:     "Mark every item of the feed read",
		Description: "Marks every unread item of the feed read and reports how many rows changed state; a second call answers {\"updated\":0}. Takes no body.",
		Tags:        []string{"feeds"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.MarkAllRead)

	huma.Register(hapi, huma.Operation{
		OperationID: operationRefreshFeed,
		Method:      http.MethodPost,
		Path:        "/feeds/{id}/refresh",
		Summary:     "Poll the feed now",
		Description: "Forces one conditional GET now, bypassing disabled_till and the backoff ladder, and reports the poll outcome. A fetch failure is still 200 with error set; 404 means the feed id addresses no row.",
		Tags:        []string{"feeds"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Refresh)
}

// List serves GET /feeds: every feed, unread_count included, the url
// member-redacted on the way out.
func (h *FeedHandlers) List(ctx context.Context, _ *struct{}) (*ListFeedsOutput, error) {
	rows, err := store.ListFeeds(ctx, h.db)
	if err != nil {
		return nil, internalFailure(ctx, "list feeds", err)
	}

	output := &ListFeedsOutput{}
	output.Body.Feeds = make([]FeedDTO, 0, len(rows))
	for _, row := range rows {
		output.Body.Feeds = append(output.Body.Feeds, feedDTO(row))
	}

	return output, nil
}

// Create serves POST /feeds. The body is validated before the write: the
// url must be an absolute http or https URL, refresh_interval_s is 0 or at
// least 300. next_fetch_at is now, so the poller's next pass picks the new
// feed up (task T065 step 7).
func (h *FeedHandlers) Create(ctx context.Context, in *CreateFeedInput) (*FeedOutput, error) {
	if strings.Contains(in.Body.URL, redactedValue) {
		// A url copied from a redacted GET /feeds response is not a
		// fetchable address; storing it would break the feed silently.
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, feedURLDetail)
	}
	if err := checkFeedURL(in.Body.URL); err != nil {
		return nil, err
	}
	if err := checkFeedRefreshInterval(in.Body.RefreshIntervalS); err != nil {
		return nil, err
	}

	feed := store.Feed{
		ID:               store.NewID(store.PrefixFeed),
		URL:              in.Body.URL,
		Title:            in.Body.Title,
		Enabled:          in.Body.Enabled == nil || *in.Body.Enabled,
		RefreshIntervalS: valueOr(in.Body.RefreshIntervalS, 0),
		ItemCap:          valueOr(in.Body.ItemCap, 50),
		Priority:         valueOr(in.Body.Priority, 0),
		// Now, so the next poll pass picks the feed up; escalation_level
		// stays at its zero value.
		NextFetchAt: time.Now().UnixMilli(),
	}
	if err := store.CreateFeed(ctx, h.db, feed); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, Problem(SlugConflict, http.StatusConflict, feedConflictDetail)
		}

		return nil, internalFailure(ctx, "create feed", err)
	}
	// The two writes share no transaction (doc 05 section 10.1): a failed
	// rule create leaves a valid committed feed, and the client repairs by
	// re-sending auto_download: true on PATCH.
	if in.Body.AutoDownload {
		if err := ensureAutoRule(ctx, h.db, feed); err != nil {
			// The committed feed is repairable through PATCH
			// auto_download: true, but the problem body carries no feed
			// id — log it so the orphan can be found.
			logFromContext(ctx).Error("create auto rule failed; feed committed",
				slog.String("feed_id", feed.ID), slog.Any("err", err))

			return nil, internalFailure(ctx, "create auto rule", err)
		}
	}

	return &FeedOutput{Status: http.StatusCreated, Body: feedDTO(feed)}, nil
}

// Patch serves PATCH /feeds/{id}: the operator-owned columns merge onto the
// stored row and the fetch-state columns survive untouched. A provided url
// goes through the same scheme check as create — unless it still carries a
// __redacted__ member, which is a no-op on the stored column (doc 05
// section 1.6). The response is the row read back, so unread_count is
// populated like the list's.
func (h *FeedHandlers) Patch(ctx context.Context, in *PatchFeedInput) (*FeedOutput, error) {
	feed, err := store.FeedByID(ctx, h.db, in.ID)
	if err != nil {
		return nil, FromStore(err)
	}

	urlMoved := in.Body.URL != nil && !strings.Contains(*in.Body.URL, redactedValue) && *in.Body.URL != feed.URL
	if in.Body.URL != nil && !strings.Contains(*in.Body.URL, redactedValue) {
		if err := checkFeedURL(*in.Body.URL); err != nil {
			return nil, err
		}
		feed.URL = *in.Body.URL
	}
	if in.Body.Title != nil {
		feed.Title = in.Body.Title
	}
	if in.Body.Enabled != nil {
		feed.Enabled = *in.Body.Enabled
	}
	if err := checkFeedRefreshInterval(in.Body.RefreshIntervalS); err != nil {
		return nil, err
	}
	if in.Body.RefreshIntervalS != nil {
		feed.RefreshIntervalS = *in.Body.RefreshIntervalS
	}
	if in.Body.ItemCap != nil {
		feed.ItemCap = *in.Body.ItemCap
	}
	if in.Body.Priority != nil {
		feed.Priority = *in.Body.Priority
	}

	if err := store.UpdateFeed(ctx, h.db, feed); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, Problem(SlugConflict, http.StatusConflict, feedConflictDetail)
		}

		return nil, FromStore(err)
	}

	// Read back so the answer carries the same unread_count the list
	// reports; a read-back ErrNotFound is the row vanishing after a
	// committed update, an internal failure rather than a second 404.
	updated, err := store.FeedByID(ctx, h.db, in.ID)
	if err != nil {
		return nil, internalFailure(ctx, "read back feed", err)
	}

	// The lifecycle resolves by name after the feed write commits, so a
	// rule created or re-scoped here sees the post-patch url (doc 05
	// section 10.1). An omitted member — nil — leaves the rule untouched
	// unless the url moved out from under an existing auto: rule: the rule
	// tracks the feed, so its scope follows. Like the create above the
	// writes share no transaction: a failed rule write leaves the
	// committed feed patch in place, retryable by re-sending the member.
	var lifecycleErr error
	switch {
	case in.Body.AutoDownload == nil:
		if urlMoved {
			lifecycleErr = keepAutoRuleScoped(ctx, h.db, updated)
		}
	case *in.Body.AutoDownload:
		lifecycleErr = ensureAutoRule(ctx, h.db, updated)
	default:
		lifecycleErr = dropAutoRule(ctx, h.db, updated.ID)
	}
	if lifecycleErr != nil {
		return nil, internalFailure(ctx, "auto rule lifecycle", lifecycleErr)
	}

	return &FeedOutput{Status: http.StatusOK, Body: feedDTO(updated)}, nil
}

// Delete serves DELETE /feeds/{id}: the auto:<feed_id> rule goes first —
// a failed rule delete answers 500 with the feed intact and the call
// retryable, so no orphaned auto: rule can outlive its feed — then the row
// goes and ON DELETE CASCADE takes its feed_items rows with it.
func (h *FeedHandlers) Delete(ctx context.Context, in *DeleteFeedInput) (*struct{}, error) {
	if err := dropAutoRule(ctx, h.db, in.ID); err != nil {
		return nil, internalFailure(ctx, "delete auto rule", err)
	}
	if err := store.DeleteFeed(ctx, h.db, in.ID); err != nil {
		return nil, FromStore(err)
	}

	return nil, nil
}

// Items serves GET /feeds/{id}/items: the feed id resolves first, so an
// unknown one is 404 before the filter runs; a cursor minted under another
// filter or feed is 422 (doc 05 section 1.4).
func (h *FeedHandlers) Items(ctx context.Context, in *ListFeedItemsInput) (*ListFeedItemsOutput, error) {
	if _, err := store.FeedByID(ctx, h.db, in.ID); err != nil {
		return nil, FromStore(err)
	}

	rows, nextCursor, total, err := store.ListFeedItems(ctx, h.db, store.FeedItemFilter{
		FeedIDs:    []string{in.ID},
		UnreadOnly: in.Unread,
		Limit:      in.Limit,
		Cursor:     in.Cursor,
	})
	if err != nil {
		if errors.Is(err, store.ErrStaleCursor) {
			return nil, feedItemsProblem(err)
		}

		return nil, internalFailure(ctx, "list feed items", err)
	}

	output := &ListFeedItemsOutput{}
	output.Body.Items = make([]FeedItemDTO, 0, len(rows))
	for _, row := range rows {
		output.Body.Items = append(output.Body.Items, feedItemDTO(row))
	}
	output.Body.Total = total
	if nextCursor != "" {
		output.Body.NextCursor = &nextCursor
	}

	return output, nil
}

// MarkItems serves PATCH /feeds/{id}/items: the listed ids of this feed go
// read or unread; the answer counts only rows whose state changed.
func (h *FeedHandlers) MarkItems(ctx context.Context, in *MarkFeedItemsInput) (*FeedItemsUpdatedOutput, error) {
	if len(in.Body.IDs) == 0 || in.Body.Read == nil {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, feedItemsPatchDetail)
	}
	if _, err := store.FeedByID(ctx, h.db, in.ID); err != nil {
		return nil, FromStore(err)
	}

	updated, err := store.SetFeedItemsRead(ctx, h.db, in.ID, in.Body.IDs, *in.Body.Read, time.Now().UnixMilli())
	if err != nil {
		return nil, internalFailure(ctx, "mark feed items", err)
	}

	output := &FeedItemsUpdatedOutput{}
	output.Body.Updated = updated

	return output, nil
}

// MarkAllRead serves POST /feeds/{id}/items/read-all: every unread item of
// the feed goes read; the answer counts only rows whose state changed.
func (h *FeedHandlers) MarkAllRead(ctx context.Context, in *MarkAllFeedItemsReadInput) (*FeedItemsUpdatedOutput, error) {
	if _, err := store.FeedByID(ctx, h.db, in.ID); err != nil {
		return nil, FromStore(err)
	}

	updated, err := store.MarkAllFeedItemsRead(ctx, h.db, in.ID, time.Now().UnixMilli())
	if err != nil {
		return nil, internalFailure(ctx, "mark all feed items read", err)
	}

	output := &FeedItemsUpdatedOutput{}
	output.Body.Updated = updated

	return output, nil
}

// Refresh serves POST /feeds/{id}/refresh: one forced conditional GET
// through the shared poller, bypassing the ladder (docs/05-api-contract.md
// section 10.1). The outcome — including a fetch failure — is the 200 body;
// only a missing feed or a bookkeeping failure is an error response.
func (h *FeedHandlers) Refresh(ctx context.Context, in *RefreshFeedInput) (*RefreshFeedOutput, error) {
	// The guard precedes the store call: a nil db — the document-only
	// builds that leave poller nil — must answer 503, not panic inside
	// sqlx before the configured check ever runs.
	if h.poller == nil {
		return nil, Problem(SlugEngineUnavailable, http.StatusServiceUnavailable, "the feed poller is not configured")
	}

	feed, err := store.FeedByID(ctx, h.db, in.ID)
	if err != nil {
		return nil, FromStore(err)
	}

	result, err := h.poller.Poll(ctx, feed, true)
	if err != nil {
		return nil, internalFailure(ctx, "refresh feed", err)
	}

	return &RefreshFeedOutput{Body: result}, nil
}

// feedItemsProblem maps the store's list error onto the registered
// validation slug with the offending field located in errors[].
func feedItemsProblem(err error) error {
	detail := "the cursor was issued for a different filter or feed"

	return &huma.ErrorModel{
		Type:   SlugValidationFailed,
		Title:  http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity,
		Detail: detail,
		Errors: []*huma.ErrorDetail{{Message: detail, Location: "query.cursor"}},
	}
}

// checkFeedURL enforces the doc 05 section 10.1 write rule: the url must be
// an absolute http or https URL — a feed is fetched over HTTP and nothing
// else.
func checkFeedURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return Problem(SlugValidationFailed, http.StatusUnprocessableEntity, feedURLDetail)
	}

	return nil
}

// checkFeedRefreshInterval enforces the section 10.1 cadence rule: 0 means
// "use the global RSS interval", anything else is at least 300 seconds. The
// schema's minimum already rejects negatives; this check owns the gap.
func checkFeedRefreshInterval(value *int) error {
	if value != nil && *value != 0 && *value < feedRefreshIntervalMinimum {
		return Problem(SlugValidationFailed, http.StatusUnprocessableEntity, feedRefreshIntervalDetail)
	}

	return nil
}

// valueOr returns the pointed-at value or the fallback when unset.
func valueOr[T any](v *T, fallback T) T {
	if v == nil {
		return fallback
	}

	return *v
}

// feedDTO renders one store row into the section 10.1 feed object:
// millisecond columns become RFC 3339 or null and the url is redacted
// member-wise — the stored row itself keeps the secret, because the poller
// needs it verbatim.
func feedDTO(f store.Feed) FeedDTO {
	return FeedDTO{
		ID:               f.ID,
		URL:              redactFeedURL(f.URL),
		Title:            f.Title,
		Enabled:          f.Enabled,
		RefreshIntervalS: f.RefreshIntervalS,
		ItemCap:          f.ItemCap,
		Priority:         f.Priority,
		UnreadCount:      f.UnreadCount,
		LastFetchAt:      unixMilliToRFC3339(f.LastFetchAt),
		LastSuccessAt:    unixMilliToRFC3339(f.LastSuccessAt),
		NextFetchAt:      time.UnixMilli(f.NextFetchAt).UTC().Format(time.RFC3339),
		EscalationLevel:  f.EscalationLevel,
		DisabledTill:     unixMilliToRFC3339(f.DisabledTill),
		LastError:        scrubFeedLastError(f.LastError, f.URL),
	}
}

// scrubFeedLastError removes the feed's own url from a stored fetch error:
// a *url.Error string embeds the request URL verbatim, userinfo and query
// secrets included, so the member-wise redaction of the url field would be
// undone by last_error. The poller should write the column through
// secure.RedactError; this is the read-side backstop for any error that
// still carries the raw address.
func scrubFeedLastError(lastError *string, rawURL string) *string {
	if lastError == nil || rawURL == "" || !strings.Contains(*lastError, rawURL) {
		return lastError
	}

	scrubbed := strings.ReplaceAll(*lastError, rawURL, redactFeedURL(rawURL))

	return &scrubbed
}

// autoRuleName is the reserved name the auto_download member creates and
// removes under: auto:<feed_id>. The rule endpoints reject user-authored
// names carrying the prefix, so the lifecycle can never adopt a foreign
// rule — and it resolves by name, never by a stored back-reference (doc 05
// section 10.1).
func autoRuleName(feedID string) string {
	return autoRulePrefix + feedID
}

// ensureAutoRule creates the rule auto_download: true asks for when none
// carries the name: enabled, scoped to the feed's url, an empty match block
// — which passes every item — and an omitted action.destination, which
// resolves to the global default at grab time (doc 05 sections 10.1 and
// 10.2). The document is built, defaulted and validated like a POST /rules
// submission, so the lifecycle can never store a rule the write path would
// reject. A name that already resolves is the desired state — idempotent,
// and so is a UNIQUE race against another writer — except when the feed's
// url moved under it: the rule tracks the feed, so its scope is re-written
// to the current url in place, keeping the row's id, its dedup watermark
// and every member the operator edited.
func ensureAutoRule(ctx context.Context, db *sqlx.DB, feed store.Feed) error {
	name := autoRuleName(feed.ID)
	switch existing, err := store.RuleByName(ctx, db, name); {
	case err == nil:
		var stored rss.RuleDoc
		if err := json.Unmarshal([]byte(existing.DefinitionJSON), &stored); err != nil {
			return fmt.Errorf("auto rule %s: decode: %w", name, err)
		}
		if len(stored.Feeds) == 1 && stored.Feeds[0] == feed.URL {
			return nil
		}

		stored.Feeds = []string{feed.URL}
		stored.ApplyDefaults()
		if errs := stored.Validate(); len(errs) > 0 {
			return fmt.Errorf("auto rule %s: %s", name, errs[0].Message)
		}
		raw, err := json.Marshal(stored)
		if err != nil {
			return fmt.Errorf("auto rule %s: encode: %w", name, err)
		}
		existing.Enabled = *stored.Enabled
		existing.Priority = stored.Priority
		existing.DefinitionJSON = string(raw)

		return store.UpdateRule(ctx, db, existing)
	case !errors.Is(err, store.ErrNotFound):
		return err
	}

	doc := rss.RuleDoc{Name: name, Feeds: []string{feed.URL}}
	doc.ApplyDefaults()
	if errs := doc.Validate(); len(errs) > 0 {
		return fmt.Errorf("auto rule %s: %s", name, errs[0].Message)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("auto rule %s: encode: %w", name, err)
	}

	rule := store.Rule{
		ID:             store.NewID(store.PrefixRule),
		Name:           name,
		Enabled:        *doc.Enabled,
		Priority:       doc.Priority,
		DefinitionJSON: string(raw),
	}
	if err := store.CreateRule(ctx, db, rule); err != nil && !errors.Is(err, store.ErrConflict) {
		return err
	}

	return nil
}

// dropAutoRule removes the auto:<feed_id> rule when one carries the name
// and is a no-op otherwise — an absent rule is the state auto_download:
// false asks for. A delete lost to a concurrent lifecycle call is the same
// desired state, so ErrNotFound from the delete is a no-op too.
func dropAutoRule(ctx context.Context, db *sqlx.DB, feedID string) error {
	rule, err := store.RuleByName(ctx, db, autoRuleName(feedID))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := store.DeleteRule(ctx, db, rule.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	return nil
}

// keepAutoRuleScoped re-scopes an existing auto:<feed_id> rule after the
// feed's url moved, and creates nothing when the feed has none: an omitted
// auto_download leaves the rule's existence to the operator — only the
// scope of a live rule follows the url.
func keepAutoRuleScoped(ctx context.Context, db *sqlx.DB, feed store.Feed) error {
	if _, err := store.RuleByName(ctx, db, autoRuleName(feed.ID)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}

		return err
	}

	return ensureAutoRule(ctx, db, feed)
}

// feedItemDTO renders one feed_items row into the section 10.1 item object;
// matched_rules is [] until T071 populates it from rule_matches.
func feedItemDTO(item store.FeedItem) FeedItemDTO {
	return FeedItemDTO{
		ID:           item.ID,
		FeedID:       item.FeedID,
		Title:        item.Title,
		Link:         item.Link,
		DownloadURL:  item.DownloadURL,
		InfoHash:     item.InfoHash,
		SizeBytes:    item.SizeBytes,
		PublishedAt:  unixMilliToRFC3339(item.PublishedAt),
		Read:         item.Read,
		MatchedRules: []FeedItemMatchedRuleDTO{},
	}
}

// redactFeedURL is the member-wise redaction of doc 05 section 10.1 — the
// feed rule, not secure.RedactURL, which strips the whole query for logs.
// Userinfo and the values of the query parameters isSecretQueryParameter
// recognises become __redacted__; the rest of the URL stays readable, so
// the operator can still tell which feed it is. A query string that fails
// to parse is redacted whole — a malformed pair may hide a secret the
// parser dropped, the same fail-closed redactedRequestURI applies.
func redactFeedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		// Writes validate the url, so an unparseable stored value is
		// corrupt; fail closed rather than render a would-be secret.
		return redactedValue
	}

	changed := false
	if u.User != nil {
		u.User = url.User(redactedValue)
		changed = true
	}
	if u.RawQuery != "" {
		query, parseErr := url.ParseQuery(u.RawQuery)
		redacted := false
		for key := range query {
			if isSecretQueryParameter(key) {
				query.Set(key, redactedValue)
				redacted = true
			}
		}
		switch {
		case redacted:
			u.RawQuery = query.Encode()
			changed = true
		case parseErr != nil:
			u.RawQuery = redactedValue
			changed = true
		}
	}
	if !changed {
		return raw
	}

	return u.String()
}
