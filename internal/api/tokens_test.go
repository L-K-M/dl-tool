package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

// tokenValuePattern is the "dlt_" + 32 hex characters of doc 05 section 12.
var tokenValuePattern = regexp.MustCompile(`^dlt_[0-9a-f]{32}$`)

// tokenListItem is the list member of doc 05 section 12; there is no token
// member to decode because the list must never carry one.
type tokenListItem struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Prefix     string  `json:"prefix"`
	LastUsedAt *string `json:"last_used_at"`
	ExpiresAt  *string `json:"expires_at"`
	CreatedAt  string  `json:"created_at"`
}

type createTokenBody struct {
	tokenListItem
	Token string `json:"token"`
}

type listTokensBody struct {
	Items      []tokenListItem `json:"items"`
	NextCursor *string         `json:"next_cursor"`
	Total      int             `json:"total"`
}

// newTokenTestAPI returns the humatest wrapper plus a management bearer
// token that drives the token endpoints themselves; the token under test is
// always one the test creates through POST.
func newTokenTestAPI(t *testing.T) (humatest.TestAPI, *sqlx.DB, string) {
	t.Helper()

	api, db := newAuthTestAPI(t)
	user := seedUser(t, db)
	management := seedLiveAPIToken(t, db, user.ID)

	return api, db, management
}

// createToken drives POST /api-tokens and decodes the 201 body.
func createToken(t *testing.T, api humatest.TestAPI, bearer, name string, expiresAt *time.Time) createTokenBody {
	t.Helper()

	payload := map[string]any{"name": name, "expires_at": nil}
	if expiresAt != nil {
		payload["expires_at"] = expiresAt.UTC().Format(time.RFC3339)
	}
	response := api.Post("/api-tokens", "Authorization: Bearer "+bearer, payload)
	if response.Code != http.StatusCreated {
		t.Fatalf("POST /api-tokens = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}

	var body createTokenBody
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode creation body %q: %v", response.Body.String(), err)
	}

	return body
}

func listTokens(t *testing.T, api humatest.TestAPI, bearer, query string) listTokensBody {
	t.Helper()

	response := api.Get("/api-tokens"+query, "Authorization: Bearer "+bearer)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api-tokens%s = %d, want %d: %s", query, response.Code, http.StatusOK, response.Body.String())
	}

	var body listTokensBody
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list body %q: %v", response.Body.String(), err)
	}

	return body
}

// TestTokenRevealedOnce pins the one reveal of doc 05 section 12: the
// creation response is the only payload ever carrying the token value, and
// the value is a live bearer credential.
func TestTokenRevealedOnce(t *testing.T) {
	api, _, management := newTokenTestAPI(t)

	created := createToken(t, api, management, "bookmarklet", nil)
	if !tokenValuePattern.MatchString(created.Token) {
		t.Fatalf("token = %q, want dlt_ plus 32 lowercase hex characters", created.Token)
	}
	if !strings.HasPrefix(created.ID, store.PrefixAPIToken) {
		t.Errorf("id = %q, want a %s ULID", created.ID, store.PrefixAPIToken)
	}
	if created.Prefix != created.Token[:8] {
		t.Errorf("prefix = %q, want the first 8 characters of the token", created.Prefix)
	}
	if created.Name != "bookmarklet" {
		t.Errorf("name = %q, want %q", created.Name, "bookmarklet")
	}
	if created.LastUsedAt != nil {
		t.Errorf("last_used_at = %q, want null before any use", *created.LastUsedAt)
	}
	if created.ExpiresAt != nil {
		t.Errorf("expires_at = %q, want null for a token without expiry", *created.ExpiresAt)
	}
	if _, err := time.Parse(time.RFC3339, created.CreatedAt); err != nil {
		t.Errorf("created_at %q is not RFC 3339: %v", created.CreatedAt, err)
	}

	// expires_at may also be omitted outright; both spellings mean no expiry.
	omitted := api.Post("/api-tokens", "Authorization: Bearer "+management, map[string]any{"name": "no key"})
	if omitted.Code != http.StatusCreated {
		t.Fatalf("POST without expires_at = %d, want %d: %s", omitted.Code, http.StatusCreated, omitted.Body.String())
	}
	var omittedBody createTokenBody
	if err := json.Unmarshal(omitted.Body.Bytes(), &omittedBody); err != nil {
		t.Fatalf("decode omitted-expiry body: %v", err)
	}
	if omittedBody.ExpiresAt != nil {
		t.Errorf("expires_at = %q, want null when the key is omitted", *omittedBody.ExpiresAt)
	}

	// A label is bounded: a 257-character name is a 422, not a stored row,
	// while exactly 256 characters is still accepted.
	tooLong := api.Post("/api-tokens", "Authorization: Bearer "+management,
		map[string]any{"name": strings.Repeat("n", 257)})
	assertProblem(t, tooLong, http.StatusUnprocessableEntity, SlugValidationFailed)
	atLimit := api.Post("/api-tokens", "Authorization: Bearer "+management,
		map[string]any{"name": strings.Repeat("n", 256)})
	if atLimit.Code != http.StatusCreated {
		t.Errorf("256-char name = %d, want %d: %s", atLimit.Code, http.StatusCreated, atLimit.Body.String())
	}

	// The minted value authenticates — it is a live bearer credential.
	response := api.Get("/api-tokens", "Authorization: Bearer "+created.Token)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api-tokens with the new token = %d, want %d", response.Code, http.StatusOK)
	}
	if strings.Contains(response.Body.String(), created.Token) {
		t.Error("the list response repeats the token value; only creation may reveal it")
	}
	if strings.Contains(response.Body.String(), `"token"`) {
		t.Error("the list response carries a token member")
	}

	list := listTokens(t, api, management, "")
	if list.Total != 4 || len(list.Items) != 4 {
		t.Fatalf("list = %d items / total %d, want 4/4", len(list.Items), list.Total)
	}
	var item *tokenListItem
	for i := range list.Items {
		if list.Items[i].ID == created.ID {
			item = &list.Items[i]
		}
	}
	if item == nil {
		t.Fatal("the created token is missing from the list")
	}
	if item.Prefix != created.Prefix || item.Name != created.Name {
		t.Errorf("listed = %+v, want prefix %q and name %q", item, created.Prefix, created.Name)
	}
	if item.LastUsedAt == nil {
		t.Error("last_used_at is null after the token authenticated a request")
	}
}

// TestListHasNoSecret pins NFR-016 on the read side: no response but the
// creation carries the value, and none ever carries the stored hash.
func TestListHasNoSecret(t *testing.T) {
	api, _, management := newTokenTestAPI(t)
	created := createToken(t, api, management, "deploy hook", nil)

	response := api.Get("/api-tokens", "Authorization: Bearer "+management)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api-tokens = %d, want %d", response.Code, http.StatusOK)
	}

	raw := response.Body.String()
	for _, secret := range []string{
		created.Token,
		management,
		secure.HashToken(created.Token),
		secure.HashToken(management),
	} {
		if strings.Contains(raw, secret) {
			t.Errorf("list response %q leaks %q", raw, secret)
		}
	}
	if strings.Contains(raw, `"token"`) || strings.Contains(raw, `"token_hash"`) {
		t.Errorf("list response carries a secret member: %s", raw)
	}
}

// TestDeleteRevokesImmediately pins the revoke contract of doc 05 section
// 12: the row stays, but its next bearer use answers 401.
func TestDeleteRevokesImmediately(t *testing.T) {
	api, _, management := newTokenTestAPI(t)
	created := createToken(t, api, management, "temporary", nil)

	response := api.Get("/api-tokens", "Authorization: Bearer "+created.Token)
	if response.Code != http.StatusOK {
		t.Fatalf("GET with the new token = %d, want %d", response.Code, http.StatusOK)
	}

	response = api.Delete("/api-tokens/"+created.ID, "Authorization: Bearer "+management)
	if response.Code != http.StatusNoContent {
		t.Fatalf("DELETE /api-tokens/%s = %d, want %d", created.ID, response.Code, http.StatusNoContent)
	}

	response = api.Get("/api-tokens", "Authorization: Bearer "+created.Token)
	assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)

	// The row survives as the audit trail but no longer lists.
	list := listTokens(t, api, management, "")
	for _, item := range list.Items {
		if item.ID == created.ID {
			t.Error("the revoked token still lists")
		}
	}

	// A second revoke and an unknown id both answer 404.
	response = api.Delete("/api-tokens/"+created.ID, "Authorization: Bearer "+management)
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
	response = api.Delete("/api-tokens/"+store.NewID(store.PrefixAPIToken), "Authorization: Bearer "+management)
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
}

// TestExpiredCreatedTokenIs401 pins the expiry half of the bearer rule on
// the endpoint path: a token created with a past expires_at is born dead.
func TestExpiredCreatedTokenIs401(t *testing.T) {
	api, _, management := newTokenTestAPI(t)
	past := time.Now().Add(-time.Hour)
	created := createToken(t, api, management, "already dead", &past)

	response := api.Get("/api-tokens", "Authorization: Bearer "+created.Token)
	assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)
}

// TestTokenListPaginates pins the section 1.4 envelope on the token list:
// limit pages the rows, next_cursor continues the walk and a token that is
// not a page cursor answers 422.
func TestTokenListPaginates(t *testing.T) {
	api, _, management := newTokenTestAPI(t)
	createToken(t, api, management, "one", nil)
	createToken(t, api, management, "two", nil)

	// The management token is the third live row.
	seen := map[string]int{}
	cursor := ""
	for range 3 {
		query := "?limit=1"
		if cursor != "" {
			query += "&cursor=" + cursor
		}
		page := listTokens(t, api, management, query)
		if page.Total != 3 {
			t.Errorf("total = %d, want 3", page.Total)
		}
		if len(page.Items) != 1 {
			t.Fatalf("page has %d items, want 1", len(page.Items))
		}
		seen[page.Items[0].ID]++
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if len(seen) != 3 {
		t.Errorf("walked %d distinct tokens, want all 3", len(seen))
	}

	response := api.Get("/api-tokens?cursor=not-a-cursor", "Authorization: Bearer "+management)
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
}

// TestTokenNeverLogged pins NFR-016 on the write side: across issue, use and
// revoke, no log record carries the token value or its hash.
func TestTokenNeverLogged(t *testing.T) {
	var mu sync.Mutex
	var records []slog.Record

	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	db, err := store.Open(
		t.Context(),
		filepath.Join(configDir, "dl-tool.db"),
		filepath.Join(root, "backups"),
	)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	server, err := NewServer(
		&config.Config{ConfigDir: configDir, SessionTTL: sessionTestExpiry},
		db,
		slog.New(captureHandler{mu: &mu, records: &records}),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(server.Shutdown)
	api := humatest.Wrap(t, server.API)

	user := seedUser(t, db)
	management := seedLiveAPIToken(t, db, user.ID)

	created := createToken(t, api, management, "audit me", nil)
	if response := api.Get("/api-tokens", "Authorization: Bearer "+created.Token); response.Code != http.StatusOK {
		t.Fatalf("GET with the new token = %d, want %d", response.Code, http.StatusOK)
	}
	if response := api.Delete("/api-tokens/"+created.ID, "Authorization: Bearer "+management); response.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want %d", response.Code, http.StatusNoContent)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(records) == 0 {
		t.Fatal("no log records captured; the leak check would pass vacuously")
	}
	for _, record := range records {
		var line strings.Builder
		line.WriteString(record.Message)
		record.Attrs(func(attr slog.Attr) bool {
			line.WriteString(attr.Value.String())
			return true
		})
		for _, secret := range []string{created.Token, secure.HashToken(created.Token)} {
			if strings.Contains(line.String(), secret) {
				t.Errorf("log record carries the token: %s", line.String())
			}
		}
	}
}
