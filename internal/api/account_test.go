package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

// accountTestPassword is long enough to pass the doc 05 section 12 floor.
const accountTestPassword = "correct horse battery staple"

// accountBody mirrors the account object of doc 05 section 12 — the shape
// GET /account and PATCH /account return and GET /auth/me nests as user.
type accountBody struct {
	ID          string  `json:"id"`
	Username    string  `json:"username"`
	Enabled     bool    `json:"enabled"`
	Locale      string  `json:"locale"`
	LastLoginAt *string `json:"last_login_at"`
	CreatedAt   string  `json:"created_at"`
}

// seedUserWithPassword inserts the operator row with a real argon2id hash
// of password, so PATCH /account's current_password check has a
// verification target; seedUser's fixture hash matches nothing.
func seedUserWithPassword(t *testing.T, db *sqlx.DB, password string) store.User {
	t.Helper()

	user := seedUser(t, db)
	hash, err := secure.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := db.ExecContext(t.Context(),
		`UPDATE users SET password_hash = ? WHERE id = ?`, hash, user.ID,
	); err != nil {
		t.Fatalf("set password hash: %v", err)
	}
	user.PasswordHash = hash

	return user
}

// sessionCount reads the live session rows behind the revocation checks.
func sessionCount(t *testing.T, db *sqlx.DB, userID string) int {
	t.Helper()

	var count int
	if err := db.GetContext(t.Context(), &count,
		`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, userID,
	); err != nil {
		t.Fatalf("count sessions: %v", err)
	}

	return count
}

// TestGetAccountReturnsShape pins the account object of doc 05 section 12:
// every documented member present, RFC 3339 timestamps, and no password
// material — neither the hash nor a member that could carry one — under
// either credential.
func TestGetAccountReturnsShape(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUserWithPassword(t, db, accountTestPassword)
	cookieValue, _, _ := seedLiveSession(t, db, user.ID)
	token := seedLiveAPIToken(t, db, user.ID)

	for _, credential := range []any{
		cookieHeader(cookieValue),
		"Authorization: Bearer " + token,
	} {
		response := api.Get("/account", credential)
		if response.Code != http.StatusOK {
			t.Fatalf("GET /account = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
		}
		raw := response.Body.String()
		if strings.Contains(raw, "password") {
			t.Errorf("account response carries password material: %s", raw)
		}
		if strings.Contains(raw, user.PasswordHash) {
			t.Errorf("account response leaks the stored hash: %s", raw)
		}

		var body accountBody
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode account body %q: %v", raw, err)
		}
		if body.ID != user.ID || body.Username != user.Username ||
			!body.Enabled || body.Locale != user.Locale {
			t.Errorf("account = %+v, want the seeded row", body)
		}
		if body.LastLoginAt != nil {
			t.Errorf("last_login_at = %q, want null before any login", *body.LastLoginAt)
		}
		if _, err := time.Parse(time.RFC3339, body.CreatedAt); err != nil {
			t.Errorf("created_at %q is not RFC 3339: %v", body.CreatedAt, err)
		}
	}
}

// TestPatchAccountWrongCurrentIs403 pins the write ordering of doc 05
// section 12: current_password is verified before any write, so a
// mismatched one answers 403 /problems/forbidden and stores nothing — not
// even the unrelated username member of the same body.
func TestPatchAccountWrongCurrentIs403(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUserWithPassword(t, db, accountTestPassword)
	cookieValue, csrfToken, _ := seedLiveSession(t, db, user.ID)

	response := api.Patch("/account",
		map[string]any{
			"username":         "renamed",
			"password":         "a-brand-new-password",
			"current_password": "not the operator's password",
		},
		cookieHeader(cookieValue),
		csrfHeaderName+": "+csrfToken,
	)
	assertProblem(t, response, http.StatusForbidden, SlugForbidden)

	var stored store.User
	if err := db.GetContext(t.Context(), &stored,
		`SELECT id, username, password_hash, enabled, locale, last_login_at, created_at, updated_at
FROM users WHERE id = ?`, user.ID,
	); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if stored.Username != user.Username {
		t.Errorf("username = %q, want %q: the 403 wrote anyway", stored.Username, user.Username)
	}
	if stored.PasswordHash != user.PasswordHash {
		t.Error("password_hash changed on a rejected patch")
	}

	// A profile-only patch needs no current_password and applies.
	response = api.Patch("/account",
		map[string]any{"username": "  renamed  ", "locale": "de"},
		cookieHeader(cookieValue),
		csrfHeaderName+": "+csrfToken,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("PATCH /account profile = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	var patched accountBody
	if err := json.Unmarshal(response.Body.Bytes(), &patched); err != nil {
		t.Fatalf("decode patched account %q: %v", response.Body.String(), err)
	}
	if patched.Username != "renamed" || patched.Locale != "de" {
		t.Errorf("patched account = %+v, want username renamed, locale de", patched)
	}
}

// TestPatchAccountShortPasswordIs422 pins the validation failures of doc 05
// section 12: a password under twelve characters and a password change
// without current_password are both 422 /problems/validation-failed, as is
// a username that normalises to the empty string.
func TestPatchAccountShortPasswordIs422(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUserWithPassword(t, db, accountTestPassword)
	token := seedLiveAPIToken(t, db, user.ID)
	bearer := "Authorization: Bearer " + token

	for _, body := range []map[string]any{
		{"password": "short", "current_password": accountTestPassword},
		{"password": "a perfectly long new password"},
		{"username": "   "},
	} {
		response := api.Patch("/account", body, bearer)
		assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	}

	// None of the rejected bodies wrote anything.
	var stored store.User
	if err := db.GetContext(t.Context(), &stored,
		`SELECT id, username, password_hash, enabled, locale, last_login_at, created_at, updated_at
FROM users WHERE id = ?`, user.ID,
	); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if stored.PasswordHash != user.PasswordHash || stored.Username != user.Username {
		t.Errorf("a 422 patch wrote: %+v", stored)
	}
}

// TestPatchAccountRevokesOtherSessions pins the revocation rule of doc 05
// section 12: a password change revokes every session except the caller's —
// every session at all when the caller authenticated with an API token —
// and leaves API tokens valid.
func TestPatchAccountRevokesOtherSessions(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUserWithPassword(t, db, accountTestPassword)

	callerCookie, callerCSRF, _ := seedLiveSession(t, db, user.ID)
	otherCookie, _, _ := seedLiveSession(t, db, user.ID)
	token := seedLiveAPIToken(t, db, user.ID)
	bearer := "Authorization: Bearer " + token

	response := api.Patch("/account",
		map[string]any{
			"password":         "the first replacement password",
			"current_password": accountTestPassword,
		},
		cookieHeader(callerCookie),
		csrfHeaderName+": "+callerCSRF,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("PATCH /account password = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}

	// The caller's session survives; the stolen one is dead.
	if get := api.Get("/account", cookieHeader(callerCookie)); get.Code != http.StatusOK {
		t.Errorf("caller's session = %d after its own password change, want %d", get.Code, http.StatusOK)
	}
	assertProblem(t, api.Get("/account", cookieHeader(otherCookie)), http.StatusUnauthorized, SlugUnauthenticated)
	if count := sessionCount(t, db, user.ID); count != 1 {
		t.Errorf("sessions = %d, want only the caller's", count)
	}

	// API tokens are unaffected: the seeded token still authenticates,
	// including a second password change, which — the caller holding no
	// session — revokes every session, the earlier caller's included.
	secondCookie, _, _ := seedLiveSession(t, db, user.ID)
	response = api.Patch("/account",
		map[string]any{
			"password":         "the second replacement password",
			"current_password": "the first replacement password",
		},
		bearer,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("PATCH /account with token = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	assertProblem(t, api.Get("/account", cookieHeader(callerCookie)), http.StatusUnauthorized, SlugUnauthenticated)
	assertProblem(t, api.Get("/account", cookieHeader(secondCookie)), http.StatusUnauthorized, SlugUnauthenticated)
	if count := sessionCount(t, db, user.ID); count != 0 {
		t.Errorf("sessions = %d, want zero after a token-authenticated password change", count)
	}
	if get := api.Get("/account", bearer); get.Code != http.StatusOK {
		t.Errorf("bearer token = %d after the password change, want %d", get.Code, http.StatusOK)
	}
}
