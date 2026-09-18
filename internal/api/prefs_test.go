package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/store"
)

// getPrefs calls GET /prefs with a bearer credential.
func getPrefs(t *testing.T, env *tasksTestEnv, bearer string) map[string]any {
	t.Helper()

	response := env.api.Get("/prefs", "Authorization: Bearer "+bearer)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /prefs status = %d, want 200: %s", response.Code, response.Body.String())
	}

	var doc map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode GET /prefs body %q: %v", response.Body.String(), err)
	}

	return doc
}

// seedPeerUser inserts a second account so the per-user isolation of the
// preference document is provable.
func seedPeerUser(t *testing.T, db *sqlx.DB) store.User {
	t.Helper()

	now := time.Now().UnixMilli()
	user := store.User{
		ID:           store.NewID(store.PrefixUser),
		Username:     "peer",
		PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaA",
		Enabled:      true,
		Locale:       "en",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	_, err := db.ExecContext(t.Context(), `INSERT INTO users
(id, username, password_hash, enabled, locale, last_login_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, NULL, ?, ?)`,
		user.ID, user.Username, user.PasswordHash, user.Enabled, user.Locale, user.CreatedAt, user.UpdatedAt,
	)
	if err != nil {
		t.Fatalf("insert peer user: %v", err)
	}

	return user
}

// TestPrefsRoundTripPerUser pins the document contract of doc 05 section
// 11.4: the stored document comes back verbatim per account, an account with
// no rows gets an empty object, and one account's writes never reach the
// other's read.
func TestPrefsRoundTripPerUser(t *testing.T) {
	env := newTasksTestEnv(t)
	peer := seedPeerUser(t, env.db)
	peerBearer := seedLiveAPIToken(t, env.db, peer.ID)

	// No rows yet: the SPA defaults patch over an empty object.
	if doc := getPrefs(t, env, env.bearer); len(doc) != 0 {
		t.Fatalf("fresh GET /prefs = %v, want empty object", doc)
	}

	doc := map[string]any{
		"version":          float64(3),
		"theme":            "dark",
		"sidebarCollapsed": true,
		"grid":             map[string]any{"density": "compact"},
	}
	response := env.api.Put("/prefs", doc, "Authorization: Bearer "+env.bearer)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT /prefs status = %d, want 200: %s", response.Code, response.Body.String())
	}

	got := getPrefs(t, env, env.bearer)
	if diff := cmp.Diff(doc, got); diff != "" {
		t.Errorf("GET /prefs mismatch (-want +got):\n%s", diff)
	}

	// The peer account neither sees nor inherits the first account's rows.
	if peerDoc := getPrefs(t, env, peerBearer); len(peerDoc) != 0 {
		t.Fatalf("peer GET /prefs = %v, want empty object", peerDoc)
	}

	// Wholesale replace: members absent from the second PUT do not survive.
	rewrite := map[string]any{"version": float64(3), "theme": "light"}
	response = env.api.Put("/prefs", rewrite, "Authorization: Bearer "+env.bearer)
	if response.Code != http.StatusOK {
		t.Fatalf("second PUT /prefs status = %d, want 200: %s", response.Code, response.Body.String())
	}
	got = getPrefs(t, env, env.bearer)
	if diff := cmp.Diff(rewrite, got); diff != "" {
		t.Errorf("replaced GET /prefs mismatch (-want +got):\n%s", diff)
	}

	// Missing credentials are unauthenticated.
	response = env.api.Get("/prefs")
	if response.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /prefs status = %d, want 401", response.Code)
	}
}

// TestPrefsUnknownMemberPreserved pins doc 05 section 11.4's verbatim rule: a
// member the server does not model round-trips unchanged, nested shape
// included.
func TestPrefsUnknownMemberPreserved(t *testing.T) {
	env := newTasksTestEnv(t)

	doc := map[string]any{
		"version": float64(3),
		"search": map[string]any{
			"saved": []any{map[string]any{"name": "linux", "query": "arch"}},
		},
		"futureFlag": map[string]any{"nested": []any{float64(1), "two", true, nil}},
	}
	response := env.api.Put("/prefs", doc, "Authorization: Bearer "+env.bearer)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT /prefs status = %d, want 200: %s", response.Code, response.Body.String())
	}

	got := getPrefs(t, env, env.bearer)
	if diff := cmp.Diff(doc, got); diff != "" {
		t.Errorf("unknown members mismatch (-want +got):\n%s", diff)
	}
}

// TestPrefsTooLarge pins the 64 KiB cap of doc 05 section 11.4: a document one
// byte over the limit is 413, one at the limit still stores.
func TestPrefsTooLarge(t *testing.T) {
	env := newTasksTestEnv(t)

	// {"pad":"<N x's>"} — the JSON framing is ten bytes, so N is the
	// document size minus ten.
	oversized := `{"pad":"` + strings.Repeat("x", int(maxPrefsBodyBytes)-9) + `"}`
	response := env.api.Do(
		http.MethodPut,
		"/prefs",
		"Content-Type: application/json",
		strings.NewReader(oversized),
		"Authorization: Bearer "+env.bearer,
	)
	assertProblem(t, response, http.StatusRequestEntityTooLarge, SlugPayloadTooLarge)

	// Exactly 64 KiB is the largest legal document.
	atLimit := `{"pad":"` + strings.Repeat("x", int(maxPrefsBodyBytes)-10) + `"}`
	response = env.api.Do(
		http.MethodPut,
		"/prefs",
		"Content-Type: application/json",
		strings.NewReader(atLimit),
		"Authorization: Bearer "+env.bearer,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT /prefs at 64 KiB status = %d, want 200: %s", response.Code, response.Body.String())
	}
}

// TestPrefsRejectsNonObject pins the JSON-object requirement of doc 05
// section 11.4: arrays, scalars and null are 422, not silently stored.
func TestPrefsRejectsNonObject(t *testing.T) {
	env := newTasksTestEnv(t)

	for _, body := range []string{
		`[1,2]`,
		`"text"`,
		`42`,
		`true`,
		`null`,
		// Malformed bodies starting with '{' must not slip past the pinned
		// 422 into the decoder's generic error path.
		`{"a":`,
		`{"a":1}junk`,
		// A number that overflows float64 fails the decode probe; a switch
		// to json.Valid would silently move this off the 422 contract.
		`{"v":1e999}`,
	} {
		response := env.api.Do(
			http.MethodPut,
			"/prefs",
			"Content-Type: application/json",
			strings.NewReader(body),
			"Authorization: Bearer "+env.bearer,
		)
		assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	}
}
