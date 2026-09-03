package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
// the Authenticate middleware is installed exactly as in production.
func newAuthTestServer(t *testing.T) (*Server, *sqlx.DB) {
	t.Helper()

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

	server, err := NewServer(
		&config.Config{},
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

// seedUser inserts the single operator row; T009 owns the real creation path.
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
	api, _ := newAuthTestAPI(t)

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
	api, _ := newAuthTestAPI(t)

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
		&config.Config{},
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
