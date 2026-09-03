package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

// authTestHost is the Host header the humatest requests carry; the
// Origin/Referer layer compares against it.
const authTestHost = "dl.example.test"

// sessionTestExpiry is the lifetime of a live session fixture; an expired
// fixture sits the same distance in the past.
const sessionTestExpiry = time.Hour

// newAuthTestServer builds the full server against a real migrated store, so
// the auth middleware is installed exactly as in production. The config
// directory sits beside the database, where the boot mints the setup token.
func newAuthTestServer(t *testing.T) (*Server, *sqlx.DB) {
	t.Helper()

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
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	return server, db
}

// newAuthTestAPI wraps the server's Huma API for humatest and registers the
// probe operation that echoes the request identity back.
func newAuthTestAPI(t *testing.T) (humatest.TestAPI, *sqlx.DB) {
	t.Helper()

	server, db := newAuthTestServer(t)
	api := humatest.Wrap(t, server.API)

	// The probe must answer every method the CSRF rules treat as mutating,
	// or a method chi would 404 never exercises the middleware path.
	operation := huma.Operation{Path: "/auth-probe"}
	for _, method := range []string{
		http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
	} {
		operation.Method = method
		huma.Register(api, operationWithID(operation, "auth-probe-"+strings.ToLower(method)), authProbeHandler)
	}

	return api, db
}

func operationWithID(operation huma.Operation, id string) huma.Operation {
	operation.OperationID = id

	return operation
}

type authProbeBody struct {
	UserID string `json:"user_id"`
	Method string `json:"method"`
}

type authProbeOutput struct {
	Body authProbeBody
}

// authProbeHandler proves the middleware ran: it echoes the identity it finds
// on the context and fails loudly when there is none.
func authProbeHandler(ctx context.Context, _ *struct{}) (*authProbeOutput, error) {
	identity, ok := IdentityFrom(ctx)
	if !ok {
		return nil, Problem(SlugInternal, http.StatusInternalServerError, "no identity on the request context")
	}

	output := &authProbeOutput{}
	output.Body.UserID = identity.User.ID
	output.Body.Method = identity.Method

	return output, nil
}

// seedUser inserts the single operator row directly, for the middleware
// tests that bypass the real creation path.
func seedUser(t *testing.T, db *sqlx.DB) store.User {
	t.Helper()

	now := time.Now().UnixMilli()
	user := store.User{
		ID:           store.NewID(store.PrefixUser),
		Username:     "operator",
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
		t.Fatalf("insert user: %v", err)
	}

	return user
}

// seedSession inserts a session and returns the cookie value, the CSRF token
// and the session id. expiresAt and lastSeenAt are unix ms, so the tests
// control expiry and touch throttling without touching the clock.
func seedSession(t *testing.T, db *sqlx.DB, userID string, expiresAt, lastSeenAt int64) (string, string, string) {
	t.Helper()

	cookieValue, err := secure.NewToken()
	if err != nil {
		t.Fatalf("mint cookie value: %v", err)
	}
	csrfToken, err := secure.NewToken()
	if err != nil {
		t.Fatalf("mint csrf token: %v", err)
	}

	session := store.Session{
		ID:         store.NewID(store.PrefixSession),
		UserID:     userID,
		TokenHash:  secure.HashToken(cookieValue),
		CSRFToken:  csrfToken,
		ExpiresAt:  expiresAt,
		LastSeenAt: lastSeenAt,
	}
	if err := store.CreateSession(t.Context(), db, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	return cookieValue, csrfToken, session.ID
}

// seedLiveSession inserts a session that expires in an hour and was just seen.
func seedLiveSession(t *testing.T, db *sqlx.DB, userID string) (string, string, string) {
	t.Helper()

	return seedSession(
		t, db, userID,
		time.Now().Add(sessionTestExpiry).UnixMilli(),
		time.Now().UnixMilli(),
	)
}

// seedAPIToken inserts an api_tokens row and returns the bearer value, dlt_
// prefix included. revokedAt and expiresAt steer the row's validity.
func seedAPIToken(t *testing.T, db *sqlx.DB, userID string, revokedAt, expiresAt *int64) string {
	t.Helper()

	secret, err := secure.NewToken()
	if err != nil {
		t.Fatalf("mint api token: %v", err)
	}
	token := apiTokenPrefix + secret

	now := time.Now().UnixMilli()
	_, err = db.ExecContext(t.Context(), `INSERT INTO api_tokens
(id, user_id, name, token_hash, prefix, last_used_at, expires_at, revoked_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, NULL, ?, ?, ?, ?)`,
		store.NewID(store.PrefixAPIToken), userID, "test token", secure.HashToken(token),
		token[:8], expiresAt, revokedAt, now, now,
	)
	if err != nil {
		t.Fatalf("insert api token: %v", err)
	}

	return token
}

// seedLiveAPIToken inserts a token that is neither revoked nor expired.
func seedLiveAPIToken(t *testing.T, db *sqlx.DB, userID string) string {
	t.Helper()

	return seedAPIToken(t, db, userID, nil, nil)
}

// cookieHeader builds the Cookie header humatest sends.
func cookieHeader(cookieValue string) string {
	return "Cookie: " + sessionCookieName + "=" + cookieValue
}

// headerArgs spreads a header list into humatest's variadic any.
func headerArgs(headers []string) []any {
	args := make([]any, len(headers))
	for i, header := range headers {
		args[i] = header
	}

	return args
}

func decodeProbeBody(t *testing.T, recorder *httptest.ResponseRecorder) authProbeBody {
	t.Helper()

	var body authProbeBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode probe body %q: %v", recorder.Body.String(), err)
	}

	return body
}

func TestNoCredentialIs401(t *testing.T) {
	api, db := newAuthTestAPI(t)
	seedUser(t, db)

	response := api.Get("/auth-probe")
	assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)
}

func TestCookieReadWithoutCSRFIs2xx(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUser(t, db)
	cookieValue, _, _ := seedLiveSession(t, db, user.ID)

	response := api.Get("/auth-probe", cookieHeader(cookieValue))
	if response.Code != http.StatusOK {
		t.Fatalf("GET with cookie = %d, want %d", response.Code, http.StatusOK)
	}

	body := decodeProbeBody(t, response)
	if body.UserID != user.ID {
		t.Errorf("user_id = %q, want %q", body.UserID, user.ID)
	}
	if body.Method != authMethodSession {
		t.Errorf("method = %q, want %q", body.Method, authMethodSession)
	}
}

func TestCookieMutationWithoutCSRFIs403(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUser(t, db)
	cookieValue, _, _ := seedLiveSession(t, db, user.ID)

	response := api.Post("/auth-probe", cookieHeader(cookieValue))
	problem := assertProblem(t, response, http.StatusForbidden, SlugCSRFTokenMissing)
	if problem.Type != "/problems/csrf-token-missing" {
		t.Errorf("type = %q, want %q", problem.Type, "/problems/csrf-token-missing")
	}
}

// TestEmptyCSRFTokenSessionIs403 pins the fail-closed guard: a session row
// with a blank csrf_token must not make the synchroniser check vacuous.
func TestEmptyCSRFTokenSessionIs403(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUser(t, db)
	cookieValue, _, sessionID := seedLiveSession(t, db, user.ID)

	if _, err := db.ExecContext(t.Context(),
		`UPDATE sessions SET csrf_token = '' WHERE id = ?`, sessionID,
	); err != nil {
		t.Fatalf("blank csrf token: %v", err)
	}

	response := api.Post("/auth-probe", cookieHeader(cookieValue))
	assertProblem(t, response, http.StatusForbidden, SlugCSRFTokenMissing)
}

func TestCookieMutationWithWrongCSRFIs403(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUser(t, db)
	cookieValue, _, _ := seedLiveSession(t, db, user.ID)

	forged, err := secure.NewToken()
	if err != nil {
		t.Fatalf("mint forged csrf token: %v", err)
	}

	response := api.Post("/auth-probe", cookieHeader(cookieValue), csrfHeaderName+": "+forged)
	assertProblem(t, response, http.StatusForbidden, SlugCSRFTokenMissing)
}

func TestCookieMutationWithCSRFIs2xx(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUser(t, db)
	cookieValue, csrfToken, _ := seedLiveSession(t, db, user.ID)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		response := api.Do(method, "/auth-probe", cookieHeader(cookieValue), csrfHeaderName+": "+csrfToken)
		if response.Code < 200 || response.Code > 299 {
			t.Errorf("%s with cookie and CSRF = %d, want 2xx", method, response.Code)
		}
	}

	response := api.Post("/auth-probe", cookieHeader(cookieValue), csrfHeaderName+": "+csrfToken)
	if response.Code != http.StatusOK {
		t.Fatalf("POST with cookie and CSRF = %d, want %d", response.Code, http.StatusOK)
	}
	if body := decodeProbeBody(t, response); body.Method != authMethodSession {
		t.Errorf("method = %q, want %q", body.Method, authMethodSession)
	}
}

func TestBearerMutationWithoutCSRFIs2xx(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUser(t, db)
	token := seedLiveAPIToken(t, db, user.ID)

	response := api.Post("/auth-probe", "Authorization: Bearer "+token)
	if response.Code != http.StatusOK {
		t.Fatalf("POST with bearer token = %d, want %d", response.Code, http.StatusOK)
	}
	if body := decodeProbeBody(t, response); body.Method != authMethodToken {
		t.Errorf("method = %q, want %q", body.Method, authMethodToken)
	}

	// The lookup stamps last_used_at (task step 3).
	var lastUsedAt *int64
	if err := db.GetContext(t.Context(), &lastUsedAt,
		`SELECT last_used_at FROM api_tokens WHERE user_id = ?`, user.ID,
	); err != nil {
		t.Fatalf("read last_used_at: %v", err)
	}
	if lastUsedAt == nil {
		t.Error("last_used_at is NULL, want the request to have stamped it")
	}
}

// TestBearerSchemeIsCaseInsensitive pins RFC 9110 section 11.1: the
// auth-scheme is case-insensitive, the token itself is not.
func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUser(t, db)
	token := seedLiveAPIToken(t, db, user.ID)

	for _, scheme := range []string{"bearer", "BEARER", "BeArEr"} {
		response := api.Get("/auth-probe", "Authorization: "+scheme+" "+token)
		if response.Code != http.StatusOK {
			t.Errorf("GET with %q scheme = %d, want %d", scheme, response.Code, http.StatusOK)
		}
	}
}

func TestMalformedAuthorizationIs401(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUser(t, db)
	token := seedLiveAPIToken(t, db, user.ID)

	for _, header := range []string{
		"Authorization: Basic dXNlcjpwYXNz",
		"Authorization: Bearer " + token[len(apiTokenPrefix):], // no dlt_ prefix
		"Authorization: Bearer dlt_",                           // empty secret
		"Authorization: Bearer dlt_unknownsecret",
	} {
		response := api.Get("/auth-probe", header)
		assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)
	}
}

// TestDisabledUserSessionIs401 pins the account kill switch: flipping
// users.enabled to 0 invalidates an otherwise live session.
func TestDisabledUserSessionIs401(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUser(t, db)
	cookieValue, _, _ := seedLiveSession(t, db, user.ID)

	if _, err := db.ExecContext(t.Context(),
		`UPDATE users SET enabled = 0 WHERE id = ?`, user.ID,
	); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	response := api.Get("/auth-probe", cookieHeader(cookieValue))
	assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)
}

func TestExpiredSessionIs401(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUser(t, db)
	cookieValue, _, _ := seedSession(
		t, db, user.ID,
		time.Now().Add(-sessionTestExpiry).UnixMilli(),
		time.Now().Add(-sessionTestExpiry).UnixMilli(),
	)

	response := api.Get("/auth-probe", cookieHeader(cookieValue))
	assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)
}

func TestRevokedTokenIs401(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUser(t, db)
	now := time.Now().UnixMilli()
	token := seedAPIToken(t, db, user.ID, &now, nil)

	response := api.Get("/auth-probe", "Authorization: Bearer "+token)
	assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)
}

func TestExpiredTokenIs401(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUser(t, db)
	past := time.Now().Add(-sessionTestExpiry).UnixMilli()
	token := seedAPIToken(t, db, user.ID, nil, &past)

	response := api.Get("/auth-probe", "Authorization: Bearer "+token)
	assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)
}

// TestForgedRefererAloneIsRejected pins the design of
// docs/12-security-and-threat-model.md section 6.2: Referer is a same-origin
// signal, never a credential.
func TestForgedRefererAloneIsRejected(t *testing.T) {
	api, db := newAuthTestAPI(t)
	seedUser(t, db)

	response := api.Get("/auth-probe",
		"Host: "+authTestHost,
		"Referer: http://"+authTestHost+"/",
	)
	assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)
}

// TestCrossHostOriginAndRefererAre403 is the second CSRF layer: a present
// Origin — or, absent Origin, a present Referer — must match the request host.
func TestCrossHostOriginAndRefererAre403(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUser(t, db)
	cookieValue, csrfToken, _ := seedLiveSession(t, db, user.ID)
	token := seedLiveAPIToken(t, db, user.ID)

	for _, test := range []struct {
		name    string
		headers []string
	}{
		{"cross-host Origin on cookie mutation", []string{
			"Host: " + authTestHost,
			cookieHeader(cookieValue),
			csrfHeaderName + ": " + csrfToken,
			"Origin: https://evil.example",
		}},
		{"cross-host Referer on cookie mutation", []string{
			"Host: " + authTestHost,
			cookieHeader(cookieValue),
			csrfHeaderName + ": " + csrfToken,
			"Referer: https://evil.example/attack",
		}},
		{"hostless Origin fails closed", []string{
			"Host: " + authTestHost,
			cookieHeader(cookieValue),
			csrfHeaderName + ": " + csrfToken,
			"Origin: null",
		}},
		{"cross-host Origin on bearer mutation", []string{
			"Host: " + authTestHost,
			"Authorization: Bearer " + token,
			"Origin: https://evil.example",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := api.Post("/auth-probe", headerArgs(test.headers)...)
			assertProblem(t, response, http.StatusForbidden, SlugCSRFTokenMissing)
		})
	}
}

// TestSameHostOriginAndRefererPass completes the layer: matching hosts pass,
// for both credentials.
func TestSameHostOriginAndRefererPass(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUser(t, db)
	cookieValue, csrfToken, _ := seedLiveSession(t, db, user.ID)
	token := seedLiveAPIToken(t, db, user.ID)

	for _, test := range []struct {
		name    string
		headers []string
	}{
		{"Origin on cookie mutation", []string{
			"Host: " + authTestHost,
			cookieHeader(cookieValue),
			csrfHeaderName + ": " + csrfToken,
			"Origin: https://" + authTestHost,
		}},
		{"Referer on cookie mutation", []string{
			"Host: " + authTestHost,
			cookieHeader(cookieValue),
			csrfHeaderName + ": " + csrfToken,
			"Referer: https://" + authTestHost + "/tasks",
		}},
		{"Origin on bearer mutation", []string{
			"Host: " + authTestHost,
			"Authorization: Bearer " + token,
			"Origin: https://" + authTestHost,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := api.Post("/auth-probe", headerArgs(test.headers)...)
			if response.Code != http.StatusOK {
				t.Errorf("POST = %d, want %d", response.Code, http.StatusOK)
			}
		})
	}
}

// TestSessionTouchIsThrottled pins the one-write-per-minute bound on
// last_seen_at (task step 8).
func TestSessionTouchIsThrottled(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUser(t, db)

	readLastSeen := func(sessionID string) int64 {
		t.Helper()

		var lastSeenAt int64
		if err := db.GetContext(t.Context(), &lastSeenAt,
			`SELECT last_seen_at FROM sessions WHERE id = ?`, sessionID,
		); err != nil {
			t.Fatalf("read last_seen_at: %v", err)
		}

		return lastSeenAt
	}

	// A session seen two minutes ago is touched by the next request.
	stale := time.Now().Add(-2 * time.Minute).UnixMilli()
	staleCookie, _, staleSession := seedSession(
		t, db, user.ID, time.Now().Add(sessionTestExpiry).UnixMilli(), stale,
	)
	if response := api.Get("/auth-probe", cookieHeader(staleCookie)); response.Code != http.StatusOK {
		t.Fatalf("GET with stale session = %d, want %d", response.Code, http.StatusOK)
	}
	if got := readLastSeen(staleSession); got <= stale {
		t.Errorf("last_seen_at = %d, want it bumped past %d", got, stale)
	}

	// A session seen seconds ago is not written again.
	fresh := time.Now().UnixMilli()
	freshCookie, _, freshSession := seedSession(
		t, db, user.ID, time.Now().Add(sessionTestExpiry).UnixMilli(), fresh,
	)
	if response := api.Get("/auth-probe", cookieHeader(freshCookie)); response.Code != http.StatusOK {
		t.Fatalf("GET with fresh session = %d, want %d", response.Code, http.StatusOK)
	}
	if got := readLastSeen(freshSession); got != fresh {
		t.Errorf("last_seen_at = %d, want it untouched at %d", got, fresh)
	}
}

// TestAPITokenStampIsThrottled pins the one-write-per-interval bound on
// api_tokens.last_used_at: a token seen moments ago is not re-stamped.
func TestAPITokenStampIsThrottled(t *testing.T) {
	api, db := newAuthTestAPI(t)
	user := seedUser(t, db)
	token := seedLiveAPIToken(t, db, user.ID)

	readLastUsed := func() int64 {
		t.Helper()

		var lastUsedAt *int64
		if err := db.GetContext(t.Context(), &lastUsedAt,
			`SELECT last_used_at FROM api_tokens WHERE user_id = ?`, user.ID,
		); err != nil {
			t.Fatalf("read last_used_at: %v", err)
		}
		if lastUsedAt == nil {
			t.Fatal("last_used_at is NULL, want a stamped value")
		}

		return *lastUsedAt
	}

	if response := api.Get("/auth-probe", "Authorization: Bearer "+token); response.Code != http.StatusOK {
		t.Fatalf("first GET = %d, want %d", response.Code, http.StatusOK)
	}
	first := readLastUsed()

	if response := api.Get("/auth-probe", "Authorization: Bearer "+token); response.Code != http.StatusOK {
		t.Fatalf("second GET = %d, want %d", response.Code, http.StatusOK)
	}
	if got := readLastUsed(); got != first {
		t.Errorf("last_used_at = %d, want it untouched at %d", got, first)
	}
}

// TestSessionJSONHidesSecrets pins the json tags on store.Session: the token
// hash and the CSRF token must never serialize.
func TestSessionJSONHidesSecrets(t *testing.T) {
	encoded, err := json.Marshal(store.Session{
		ID: "session", UserID: "user", TokenHash: "tokenhash", CSRFToken: "csrftoken",
		ExpiresAt: 1, LastSeenAt: 2,
	})
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}

	for _, secret := range []string{"tokenhash", "csrftoken", "token_hash", "csrf_token"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("session JSON %q leaks %q", encoded, secret)
		}
	}
}

// TestStoreFailureIs500 keeps the wire detail generic when the credential
// lookup itself fails; the cause belongs in the logs only.
func TestStoreFailureIs500(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(
		t.Context(),
		filepath.Join(root, "config", "dl-tool.db"),
		filepath.Join(root, "backups"),
	)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	server, err := NewServer(
		&config.Config{ConfigDir: t.TempDir(), SessionTTL: sessionTestExpiry},
		db,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	api := humatest.Wrap(t, server.API)
	huma.Register(api, huma.Operation{
		OperationID: "auth-probe-read",
		Method:      http.MethodGet,
		Path:        "/auth-probe",
	}, authProbeHandler)

	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	response := api.Get("/auth-probe", cookieHeader("irrelevant"))
	problem := assertProblem(t, response, http.StatusInternalServerError, SlugInternal)
	if problem.Detail != "an internal error occurred" {
		t.Errorf("detail = %q, want the generic internal detail", problem.Detail)
	}
}

// TestSessionCookieAttributes pins docs/05-api-contract.md section 1.2:
// HttpOnly, SameSite=Lax, Path equal to the base path plus "/", Secure on TLS.
func TestSessionCookieAttributes(t *testing.T) {
	for _, test := range []struct {
		name       string
		basePath   string
		transport  Transport
		wantPath   string
		wantSecure bool
	}{
		{"root base path", "", TransportPlain, "/", false},
		{"nested base path", "/dl-tool", TransportPlain, "/dl-tool/", false},
		{"TLS sets Secure", "", TransportTLS, "/", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cookie := NewSessionCookie(&config.Config{BasePath: test.basePath}, "opaque", test.transport)
			if cookie.Name != sessionCookieName {
				t.Errorf("Name = %q, want %q", cookie.Name, sessionCookieName)
			}
			if cookie.Path != test.wantPath {
				t.Errorf("Path = %q, want %q", cookie.Path, test.wantPath)
			}
			if !cookie.HttpOnly {
				t.Error("HttpOnly is false, want true")
			}
			if cookie.SameSite != http.SameSiteLaxMode {
				t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
			}
			if cookie.Secure != test.wantSecure {
				t.Errorf("Secure = %v, want %v", cookie.Secure, test.wantSecure)
			}
		})
	}
}

// TestBaseRoutesStayAnonymous pins the middleware scope: only /api/v1 is
// behind Authenticate; routes on the base router — /healthz, /readyz and the
// SPA — answer without a credential.
func TestBaseRoutesStayAnonymous(t *testing.T) {
	server, _ := newAuthTestServer(t)

	server.Base.Get("/base-probe", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	response := do(t, server.Router, http.MethodGet, "/base-probe")
	if response.Code != http.StatusNoContent {
		t.Errorf("GET /base-probe = %d, want %d", response.Code, http.StatusNoContent)
	}
}

// authOpsFixture drives the real /auth operations against a fresh store.
type authOpsFixture struct {
	API       humatest.TestAPI
	DB        *sqlx.DB
	ConfigDir string
	Token     string // the setup token the boot minted
}

func newAuthOpsFixture(t *testing.T) authOpsFixture {
	t.Helper()

	server, db := newAuthTestServer(t)

	token, err := os.ReadFile(filepath.Join(server.auth.cfg.ConfigDir, setupTokenFileName))
	if err != nil {
		t.Fatalf("read the boot-minted setup token: %v", err)
	}

	return authOpsFixture{
		API:       humatest.Wrap(t, server.API),
		DB:        db,
		ConfigDir: server.auth.cfg.ConfigDir,
		Token:     string(token),
	}
}

// completeSetup drives the setup happy path and returns the response
// envelope plus the session cookie it issued.
func completeSetup(t *testing.T, fixture authOpsFixture) (authEnvelopeBody, string) {
	t.Helper()

	response := fixture.API.Post("/auth/setup", map[string]any{
		"setup_token": fixture.Token,
		"username":    "alice",
		"password":    "correct horse battery",
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("POST /auth/setup = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}

	return decodeAuthEnvelope(t, response), sessionCookieValue(t, response)
}

// authEnvelopeBody decodes the {"user":…,"csrf_token":…} body of doc 05
// section 4.
type authEnvelopeBody struct {
	User struct {
		ID          string  `json:"id"`
		Username    string  `json:"username"`
		Enabled     bool    `json:"enabled"`
		Locale      string  `json:"locale"`
		LastLoginAt *string `json:"last_login_at"`
		CreatedAt   string  `json:"created_at"`
	} `json:"user"`
	CSRFToken string `json:"csrf_token"`
}

func decodeAuthEnvelope(t *testing.T, recorder *httptest.ResponseRecorder) authEnvelopeBody {
	t.Helper()

	var envelope authEnvelopeBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode auth envelope %q: %v", recorder.Body.String(), err)
	}

	return envelope
}

// sessionCookieValue extracts the session cookie value from Set-Cookie.
func sessionCookieValue(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()

	for _, line := range recorder.Header().Values("Set-Cookie") {
		name, value, _ := strings.Cut(strings.SplitN(line, ";", 2)[0], "=")
		if name == sessionCookieName {
			return value
		}
	}
	t.Fatalf("no %s cookie in Set-Cookie %v", sessionCookieName, recorder.Header().Values("Set-Cookie"))

	return ""
}

func retryAfterSeconds(t *testing.T, recorder *httptest.ResponseRecorder) int {
	t.Helper()

	seconds, err := strconv.Atoi(recorder.Header().Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After %q is not a number: %v", recorder.Header().Get("Retry-After"), err)
	}

	return seconds
}

// TestEveryEndpointBeforeSetupIs401 pins doc 05 section 1.2: while no user
// row exists, every endpoint except POST /auth/setup answers
// 401 /problems/setup-required — including login.
func TestEveryEndpointBeforeSetupIs401(t *testing.T) {
	api, _ := newAuthTestAPI(t)

	for _, target := range []struct{ method, path string }{
		{http.MethodGet, "/auth-probe"},
		{http.MethodPost, "/auth-probe"},
		{http.MethodPost, "/auth/login"},
		{http.MethodGet, "/auth/me"},
		{http.MethodPost, "/auth/logout"},
	} {
		t.Run(target.method+" "+target.path, func(t *testing.T) {
			response := api.Do(target.method, target.path)
			assertProblem(t, response, http.StatusUnauthorized, "/problems/setup-required")
		})
	}
}

func TestSetupWrongTokenIs401(t *testing.T) {
	fixture := newAuthOpsFixture(t)

	response := fixture.API.Post("/auth/setup", map[string]any{
		"setup_token": "bm90LXRoZS10b2tlbg",
		"username":    "alice",
		"password":    "correct horse battery",
	})
	assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)
}

func TestPasswordUnder12CharsRejected(t *testing.T) {
	fixture := newAuthOpsFixture(t)

	response := fixture.API.Post("/auth/setup", map[string]any{
		"setup_token": fixture.Token,
		"username":    "alice",
		"password":    "short",
	})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
}

// TestSetupCreatesAccountAndIssuesSession pins the 201 envelope of doc 05
// section 4.1 and the cookie attributes of doc 12 section 6.1.
func TestSetupCreatesAccountAndIssuesSession(t *testing.T) {
	fixture := newAuthOpsFixture(t)

	response := fixture.API.Post("/auth/setup", map[string]any{
		"setup_token": fixture.Token,
		"username":    "alice",
		"password":    "correct horse battery",
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("POST /auth/setup = %d, want %d", response.Code, http.StatusCreated)
	}

	envelope := decodeAuthEnvelope(t, response)
	if envelope.User.ID == "" || !strings.HasPrefix(envelope.User.ID, store.PrefixUser) {
		t.Errorf("user.id = %q, want a %s ULID", envelope.User.ID, store.PrefixUser)
	}
	if envelope.User.Username != "alice" {
		t.Errorf("user.username = %q, want %q", envelope.User.Username, "alice")
	}
	if !envelope.User.Enabled {
		t.Error("user.enabled = false, want true")
	}
	if envelope.User.Locale != "en" {
		t.Errorf("user.locale = %q, want the default %q", envelope.User.Locale, "en")
	}
	if envelope.User.LastLoginAt != nil {
		t.Errorf("user.last_login_at = %q, want null before any login", *envelope.User.LastLoginAt)
	}
	if _, err := time.Parse(time.RFC3339, envelope.User.CreatedAt); err != nil {
		t.Errorf("user.created_at %q is not RFC 3339: %v", envelope.User.CreatedAt, err)
	}
	if envelope.CSRFToken == "" {
		t.Error("csrf_token is empty")
	}

	cookieLine := ""
	for _, line := range response.Header().Values("Set-Cookie") {
		if strings.HasPrefix(line, sessionCookieName+"=") {
			cookieLine = line
		}
	}
	if cookieLine == "" {
		t.Fatalf("no %s cookie in Set-Cookie", sessionCookieName)
	}
	for _, attribute := range []string{"HttpOnly", "SameSite=Lax", "Path=/"} {
		if !strings.Contains(cookieLine, attribute) {
			t.Errorf("Set-Cookie %q lacks %s", cookieLine, attribute)
		}
	}
}

func TestSetupTokenFileDeletedOnSuccess(t *testing.T) {
	fixture := newAuthOpsFixture(t)
	completeSetup(t, fixture)

	if _, err := os.Stat(filepath.Join(fixture.ConfigDir, setupTokenFileName)); !os.IsNotExist(err) {
		t.Errorf("setup token file still exists after setup (stat err %v), want it deleted", err)
	}
}

// TestArgon2idPHCString pins the stored hash format of doc 12 section 6.3.
func TestArgon2idPHCString(t *testing.T) {
	fixture := newAuthOpsFixture(t)
	completeSetup(t, fixture)

	var hash string
	if err := fixture.DB.GetContext(t.Context(),
		&hash, `SELECT password_hash FROM users`,
	); err != nil {
		t.Fatalf("read password hash: %v", err)
	}

	if prefix := "$argon2id$v=19$m=19456,t=2,p=1$"; !strings.HasPrefix(hash, prefix) {
		t.Errorf("stored hash %q lacks the prefix %q", hash, prefix)
	}
}

func TestSecondSetupIs409(t *testing.T) {
	fixture := newAuthOpsFixture(t)
	completeSetup(t, fixture)

	response := fixture.API.Post("/auth/setup", map[string]any{
		"setup_token": fixture.Token,
		"username":    "mallory",
		"password":    "another correct horse",
	})
	assertProblem(t, response, http.StatusConflict, SlugSetupAlreadyComplete)
}

// TestLoginAfterSetupIssuesUsableSession walks the SPA boot sequence:
// login, then GET /auth/me with the issued cookie.
func TestLoginAfterSetupIssuesUsableSession(t *testing.T) {
	fixture := newAuthOpsFixture(t)
	completeSetup(t, fixture)

	response := fixture.API.Post("/auth/login", map[string]any{
		"username": "alice",
		"password": "correct horse battery",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("POST /auth/login = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}

	envelope := decodeAuthEnvelope(t, response)
	if envelope.User.LastLoginAt == nil {
		t.Error("user.last_login_at is null after a successful login, want the login stamp")
	}
	if envelope.CSRFToken == "" {
		t.Error("csrf_token is empty")
	}

	me := fixture.API.Get("/auth/me", cookieHeader(sessionCookieValue(t, response)))
	if me.Code != http.StatusOK {
		t.Fatalf("GET /auth/me with the login cookie = %d, want %d", me.Code, http.StatusOK)
	}
	if cacheControl := me.Header().Get("Cache-Control"); cacheControl != cacheControlNoStore {
		t.Errorf("/auth/me Cache-Control = %q, want %q", cacheControl, cacheControlNoStore)
	}
	meEnvelope := decodeAuthEnvelope(t, me)
	if meEnvelope.User.Username != "alice" {
		t.Errorf("me user.username = %q, want %q", meEnvelope.User.Username, "alice")
	}
	if meEnvelope.CSRFToken != envelope.CSRFToken {
		t.Error("me csrf_token differs from the login csrf_token")
	}
}

// elapsedBackoff steps past the account ladder's first wait without
// hard-coding it: the constant lives beside the throttle implementation.
func elapsedBackoff() time.Duration {
	return throttleBackoffStart + 100*time.Millisecond
}

// TestLoginFailureIsIndistinguishable pins doc 12 section 6.3: a wrong
// password, a disabled account and an unknown user share one detail.
func TestLoginFailureIsIndistinguishable(t *testing.T) {
	fixture := newAuthOpsFixture(t)
	completeSetup(t, fixture)

	login := func(username, password string) problemDocument {
		t.Helper()

		response := fixture.API.Post("/auth/login", map[string]any{
			"username": username,
			"password": password,
		})

		return assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)
	}

	wrongPassword := login("alice", "wrong horse battery")

	// The disabled attempt below must fail for the same account, and the
	// account ladder throttles a second failure within its wait — so let the
	// wait elapse between the two.
	time.Sleep(elapsedBackoff())
	if _, err := fixture.DB.ExecContext(t.Context(), `UPDATE users SET enabled = 0`); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	unknownUser := login("nobody", "whatever password")
	disabled := login("alice", "correct horse battery")

	if unknownUser.Detail != wrongPassword.Detail || disabled.Detail != wrongPassword.Detail {
		t.Errorf("login failure details differ: wrong password %q, unknown user %q, disabled %q",
			wrongPassword.Detail, unknownUser.Detail, disabled.Detail)
	}
}

// TestAccountBackoffAfterFailures pins the per-account ladder of doc 12
// section 6.3: the second rapid failure for one account answers 429 with
// Retry-After.
func TestAccountBackoffAfterFailures(t *testing.T) {
	fixture := newAuthOpsFixture(t)
	completeSetup(t, fixture)

	credentials := map[string]any{"username": "alice", "password": "wrong horse battery"}

	response := fixture.API.Post("/auth/login", credentials)
	assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)

	response = fixture.API.Post("/auth/login", credentials)
	assertProblem(t, response, http.StatusTooManyRequests, SlugRateLimited)
	if seconds := retryAfterSeconds(t, response); seconds < 1 {
		t.Errorf("Retry-After = %d, want at least 1", seconds)
	}
}

// TestLoginBackoffIsNotPermanent pins the other half of the rule: the
// ladder resets once its wait has elapsed, so nothing locks out forever.
func TestLoginBackoffIsNotPermanent(t *testing.T) {
	fixture := newAuthOpsFixture(t)
	completeSetup(t, fixture)

	response := fixture.API.Post("/auth/login", map[string]any{
		"username": "alice", "password": "wrong horse battery",
	})
	assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)

	time.Sleep(elapsedBackoff())

	response = fixture.API.Post("/auth/login", map[string]any{
		"username": "alice", "password": "correct horse battery",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("login after the backoff elapsed = %d, want %d", response.Code, http.StatusOK)
	}
}

// TestElevenFailedLoginsFromOneIPYield429 pins the per-source-IP bucket of
// doc 12 section 6.3: ten attempts per five minutes, the next answers 429.
// The usernames differ so the account ladder stays out of the way; the
// successful setup consumed one attempt of the shared bucket.
func TestElevenFailedLoginsFromOneIPYield429(t *testing.T) {
	fixture := newAuthOpsFixture(t)
	completeSetup(t, fixture)

	for attempt := 1; attempt <= 11; attempt++ {
		response := fixture.API.Post("/auth/login", map[string]any{
			"username": fmt.Sprintf("nobody%02d", attempt),
			"password": "wrong horse battery",
		})

		if attempt <= 9 {
			assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)

			continue
		}

		assertProblem(t, response, http.StatusTooManyRequests, SlugRateLimited)
		if seconds := retryAfterSeconds(t, response); seconds < 1 {
			t.Errorf("attempt %d Retry-After = %d, want at least 1", attempt, seconds)
		}
	}
}

// TestLogoutExpiresSession pins the logout contract of doc 05 section 4.2:
// 204, the sessions row is gone, the cookie is expired and stops working.
func TestLogoutExpiresSession(t *testing.T) {
	fixture := newAuthOpsFixture(t)
	envelope, cookie := completeSetup(t, fixture)

	response := fixture.API.Post("/auth/logout", cookieHeader(cookie), csrfHeaderName+": "+envelope.CSRFToken)
	if response.Code != http.StatusNoContent {
		t.Fatalf("POST /auth/logout = %d, want %d", response.Code, http.StatusNoContent)
	}

	var sessions int
	if err := fixture.DB.GetContext(t.Context(), &sessions, `SELECT COUNT(*) FROM sessions`); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 0 {
		t.Errorf("%d session rows remain, want 0", sessions)
	}

	if !strings.Contains(response.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Errorf("Set-Cookie %q does not expire the cookie", response.Header().Get("Set-Cookie"))
	}

	me := fixture.API.Get("/auth/me", cookieHeader(cookie))
	assertProblem(t, me, http.StatusUnauthorized, SlugUnauthenticated)
}

// TestBearerLogoutIs204 covers the bearer half of logout: no session row to
// delete, no cookie to expire.
func TestBearerLogoutIs204(t *testing.T) {
	fixture := newAuthOpsFixture(t)
	completeSetup(t, fixture)

	var userID string
	if err := fixture.DB.GetContext(t.Context(), &userID, `SELECT id FROM users`); err != nil {
		t.Fatalf("read user id: %v", err)
	}
	token := seedLiveAPIToken(t, fixture.DB, userID)

	response := fixture.API.Post("/auth/logout", "Authorization: Bearer "+token)
	if response.Code != http.StatusNoContent {
		t.Fatalf("POST /auth/logout with bearer = %d, want %d", response.Code, http.StatusNoContent)
	}
	if values := response.Header().Values("Set-Cookie"); len(values) != 0 {
		t.Errorf("bearer logout set %v, want no Set-Cookie", values)
	}
}

// TestConcurrentSetupYieldsOne201AndOne409 pins the serialization of the
// setup critical section: two racing valid calls create exactly one account.
func TestConcurrentSetupYieldsOne201AndOne409(t *testing.T) {
	fixture := newAuthOpsFixture(t)

	body := map[string]any{
		"setup_token": fixture.Token,
		"username":    "alice",
		"password":    "correct horse battery",
	}

	codes := make(chan int, 2)
	for range 2 {
		go func() {
			response := fixture.API.Post("/auth/setup", body)
			codes <- response.Code
		}()
	}

	first, second := <-codes, <-codes
	for _, code := range []int{first, second} {
		if code != http.StatusCreated && code != http.StatusConflict {
			t.Fatalf("concurrent setup answered %d, want %d or %d", code, http.StatusCreated, http.StatusConflict)
		}
	}
	if first == http.StatusCreated && second == http.StatusCreated {
		t.Error("both concurrent setups returned 201, want exactly one")
	}

	var users int
	if err := fixture.DB.GetContext(t.Context(), &users, `SELECT COUNT(*) FROM users`); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 {
		t.Errorf("%d user rows after concurrent setup, want 1", users)
	}
}

// TestSetupFailureDoesNotPoisonTheLoginLadder pins the review fix: a failed
// setup attempt consumes only the source budget, so the username it carried
// starts its login life without a pre-loaded backoff.
func TestSetupFailureDoesNotPoisonTheLoginLadder(t *testing.T) {
	fixture := newAuthOpsFixture(t)

	for range 2 {
		response := fixture.API.Post("/auth/setup", map[string]any{
			"setup_token": "bm90LXRoZS10b2tlbg",
			"username":    "alice",
			"password":    "correct horse battery",
		})
		assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)
	}

	completeSetup(t, fixture)

	response := fixture.API.Post("/auth/login", map[string]any{
		"username": "alice",
		"password": "correct horse battery",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("first login for alice = %d, want %d", response.Code, http.StatusOK)
	}
}

// TestZeroSessionTTLFailsConstruction pins the loud failure: a non-positive
// TTL would mint sessions that are born expired, so NewServer refuses it.
func TestZeroSessionTTLFailsConstruction(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(
		t.Context(),
		filepath.Join(root, "config", "dl-tool.db"),
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

	if _, err := NewServer(
		&config.Config{ConfigDir: filepath.Join(root, "config")},
		db,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	); err == nil || !strings.Contains(err.Error(), "session ttl") {
		t.Fatalf("NewServer error = %v, want a session ttl rejection", err)
	}
}
