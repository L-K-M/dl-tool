package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

const (
	// sessionCookieName is the unprefixed name of docs/05-api-contract.md
	// section 1.2. The prefixed variants of docs/12-security-and-threat-model.md
	// section 6.1 follow the listener's TLS state, which the config does not
	// model yet.
	sessionCookieName = "dltool_session"
	csrfHeaderName    = "X-DLTOOL-CSRF"

	// The auth-scheme is matched case-insensitively (RFC 9110 section 11.1);
	// the token itself is case-sensitive.
	authSchemeBearer = "Bearer"
	apiTokenPrefix   = "dlt_"

	// Identity.Method values.
	authMethodSession = "session"
	authMethodToken   = "token"

	// sessionTouchInterval bounds the last_seen_at write rate: at most one
	// write per minute and session, no matter how chatty the client is.
	sessionTouchInterval = time.Minute

	// The one-time setup token of doc 12 section 6.4: written to
	// <config>/setup-token mode 0600, regenerated on every boot while the
	// users table is empty, deleted once setup succeeds.
	setupTokenFileName = "setup-token"
	setupTokenFileMode = 0o600

	// The operation keys of the two endpoints that authenticate by body
	// instead of credential, relative to /api/v1. The middleware exempts
	// them from credential resolution; their handlers carry their own
	// controls.
	opAuthSetup = http.MethodPost + " /auth/setup"
	opAuthLogin = http.MethodPost + " /auth/login"

	// The one 401 detail every login failure shares — wrong password,
	// disabled account, unknown user — so login cannot enumerate accounts
	// (doc 12 section 6.3).
	loginRejectionDetail = "the username or password is not accepted"

	setupRequiredDetail = "the operator account has not been created yet; complete first-run setup first"
	setupCompleteDetail = "the operator account already exists; first-run setup cannot run again"
	setupTokenDetail    = "the setup token is not valid"
	rateLimitedDetail   = "too many attempts; retry after the delay in Retry-After"
	noCredentialDetail  = "no credential was presented"

	// Stable event codes for log-based alerting and the shipped fail2ban
	// filter (doc 12 sections 6.3 and 6.4).
	eventLoginFailed = "auth.login_failed"
	eventSetupFailed = "auth.setup_failed"

	// Brute-force controls of doc 12 section 6.3. Waiting permits another
	// attempt; only success or inactivity resets consecutive failures.
	throttleSourceCapacity = 10
	throttleSourceWindow   = 5 * time.Minute
	throttleBackoffStart   = 1 * time.Second
	throttleBackoffCap     = 15 * time.Minute
	// 1s<<10 already exceeds the 15-minute cap; larger shift counts are
	// clamped here, so an ever-growing failure count cannot overflow.
	throttleBackoffMaxShift = 10

	// Sweep floor: once a throttle map grows past it, recording sweeps the
	// expired entries, so address or username spray cannot grow memory
	// without bound.
	throttleSweepFloor = 1024

	// cacheControlNoStore keeps responses carrying the CSRF token out of
	// every cache on the way back.
	cacheControlNoStore = "no-store"

	// OpenAPI security scheme names of docs/05-api-contract.md section 1.2.
	schemeSession = "sessionCookie"
	schemeBearer  = "bearerToken"
)

// credentialRequired names both credentials of doc 05 section 1.2, for the
// operations the middleware protects; the two body-authenticated
// operations declare an explicit empty list instead, so the document shows
// them as anonymous.
var credentialRequired = []map[string][]string{{schemeSession: {}}, {schemeBearer: {}}}

// Identity is what the middleware puts on the request context.
type Identity struct {
	User   store.User
	Method string // "session" | "token"
	CSRF   string // the session's csrf_token; empty for token authentication

	// SessionID names the sessions row behind a cookie-authenticated
	// identity, so /auth/logout can delete it; empty for token authentication.
	SessionID string
}

// Transport reports whether a request arrived over TLS; it drives the session
// cookie's Secure flag (docs/12-security-and-threat-model.md section 6.1).
type Transport int

const (
	TransportPlain Transport = iota
	TransportTLS
)

// identityContextKey carries the authenticated Identity on the request context.
type identityContextKey struct{}

// IdentityFrom returns the caller's identity, or ok == false when unauthenticated.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityContextKey{}).(Identity)

	return identity, ok
}

// NewSessionCookie builds the session cookie exactly as docs/05-api-contract.md
// section 1.2 and docs/12-security-and-threat-model.md section 6.1 specify:
// HttpOnly, SameSite=Lax, Path equal to the base path plus "/", and Secure when
// the request arrived over TLS. Login and setup set it; logout expires it.
func NewSessionCookie(cfg *config.Config, value string, transport Transport) *http.Cookie {
	cookie := &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     cfg.BasePath + "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(math.Ceil(cfg.SessionTTL.Seconds())),
	}
	if transport == TransportTLS {
		cookie.Secure = true
	}

	return cookie
}

// authService owns the first-run gate, the four /auth operations and the
// brute-force controls of doc 12 section 6.3. It is constructed once per
// boot; a nil db — the openapi subcommand — yields an inert service that
// only registers its operations for the generated document.
type authService struct {
	cfg *config.Config
	db  *sqlx.DB

	// setupMu serializes setup: the gate check, the account insert and the
	// token teardown are one critical section, so two concurrent calls can
	// never both create the operator account.
	setupMu sync.Mutex

	// setupDone caches "the users table is not empty". Seeded at boot,
	// flipped by a successful setup, and re-checked from the row count
	// while false so out-of-band inserts are picked up.
	setupDone atomic.Bool

	// setupToken is the in-memory copy of the boot-minted token; it is the
	// comparison target while setup is callable.
	setupToken string

	throttle loginThrottle
}

// newAuthService counts the users table at boot and, while it is empty,
// mints the one-time setup token: written to <config>/setup-token mode
// 0600 and logged on its own line so docker compose logs shows it.
func newAuthService(cfg *config.Config, db *sqlx.DB, log *slog.Logger) (*authService, error) {
	// A non-positive TTL would mint sessions that are born expired and fail
	// every request after login; refuse it loudly at construction instead.
	// (A nil db — the openapi subcommand — skips serving entirely, so the
	// check only matters there for schema shape, not values.)
	if db != nil && cfg.SessionTTL <= 0 {
		return nil, fmt.Errorf("session ttl must be positive, got %s", cfg.SessionTTL)
	}

	service := &authService{cfg: cfg, db: db, throttle: newLoginThrottle()}
	if db == nil {
		return service, nil
	}

	count, err := store.CountUsers(context.Background(), db)
	if err != nil {
		return nil, fmt.Errorf("count users at boot: %w", err)
	}
	if count > 0 {
		service.setupDone.Store(true)

		return service, nil
	}

	token, err := secure.NewToken()
	if err != nil {
		return nil, fmt.Errorf("mint setup token: %w", err)
	}
	path := filepath.Join(cfg.ConfigDir, setupTokenFileName)
	if err := writeSetupToken(path, token); err != nil {
		return nil, fmt.Errorf("write setup token: %w", err)
	}
	service.setupToken = token
	log.Info("first-run setup token", "token", token, "path", path)

	return service, nil
}

// writeSetupToken replaces the token file so it always ends up fresh with
// mode 0600, even when an older file carried looser permissions.
func writeSetupToken(path, token string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale setup token: %w", err)
	}
	if err := os.WriteFile(path, []byte(token), setupTokenFileMode); err != nil {
		return fmt.Errorf("write setup token file: %w", err)
	}

	return nil
}

// setupComplete reports whether the operator account exists, refreshing
// the boot-time cache from the row count. While false, every endpoint
// except POST /auth/setup answers 401 /problems/setup-required (doc 05
// section 1.2).
func (a *authService) setupComplete(ctx context.Context) (bool, error) {
	if a.setupDone.Load() {
		return true, nil
	}

	count, err := store.CountUsers(ctx, a.db)
	if err != nil {
		return false, err
	}
	if count > 0 {
		a.setupDone.Store(true)

		return true, nil
	}

	return false, nil
}

// middleware is the gate and the credential check of every /api/v1 route:
// the first-run setup gate first, then the cookie or bearer resolution and
// the CSRF layers of doc 12 section 6.2. POST /auth/setup and
// POST /auth/login authenticate by body; their handlers enforce the gate
// and the throttles themselves.
func (a *authService) middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), requestInfoContextKey{}, newRequestInfo(a.cfg, r))

			operation := operationKey(a.cfg, r)
			if operation != opAuthSetup {
				complete, err := a.setupComplete(ctx)
				if err != nil {
					writeProblem(w, internalFailure(ctx, "count users", err))

					return
				}
				if !complete {
					writeProblem(w, Problem(SlugSetupRequired, http.StatusUnauthorized, setupRequiredDetail))

					return
				}
			}
			if operation == opAuthSetup || operation == opAuthLogin {
				next.ServeHTTP(w, r.WithContext(ctx))

				return
			}

			identity, problem := resolveIdentity(r, a.db)
			if problem != nil {
				writeProblem(w, problem)

				return
			}
			if problem := csrfProblem(r, identity); problem != nil {
				writeProblem(w, problem)

				return
			}

			// Put the identity and a user-attributed logger on the context
			// (docs/14-conventions.md section 3.1).
			ctx = context.WithValue(ctx, identityContextKey{}, identity)
			ctx = context.WithValue(ctx, loggerContextKey{}, logFromContext(ctx).With(
				slog.String("user_id", identity.User.ID),
			))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// operationKey identifies the endpoint the middleware is looking at: the
// method plus the path relative to the API base, so a configured base path
// does not change the key. Paths that never mount under /api/v1 (humatest
// drives the sub-router directly) carry no prefix to strip.
func operationKey(cfg *config.Config, r *http.Request) string {
	return r.Method + " " + strings.TrimPrefix(r.URL.Path, cfg.BasePath+apiV1Path)
}

// requestInfo carries what the /auth handlers need from the transport: the
// TLS state for the cookie's Secure flag and the peer address for the
// brute-force buckets.
type requestInfo struct {
	Transport Transport
	PeerIP    string
}

// requestInfoContextKey carries the requestInfo the middleware observed.
type requestInfoContextKey struct{}

func newRequestInfo(cfg *config.Config, r *http.Request) requestInfo {
	peer, ok := r.Context().Value(peerAddressContextKey{}).(string)
	if !ok {
		peer = r.RemoteAddr
	}

	transport := TransportPlain
	if r.TLS != nil || (strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") &&
		isTrustedProxy(peer, cfg.TrustedProxies)) {
		transport = TransportTLS
	}

	// The root router's realIP middleware has already rewritten RemoteAddr
	// to the original client for trusted proxies — walking X-Forwarded-For
	// right to left past every trusted hop, so spoofed leftmost entries
	// cannot choose the address. The host split below therefore already sees
	// the client, not the proxy; bare addresses carry no port, which the
	// fallback handles.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	return requestInfo{Transport: transport, PeerIP: host}
}

// requestInfoFrom returns the middleware's observation; a zero value means
// the handler was reached without the middleware, which serving prevents.
func requestInfoFrom(ctx context.Context) requestInfo {
	info, _ := ctx.Value(requestInfoContextKey{}).(requestInfo)

	return info
}

// resolveIdentity authenticates the request: the bearer header first, then
// the session cookie. The returned problem never carries a credential.
func resolveIdentity(r *http.Request, db *sqlx.DB) (Identity, error) {
	if header := r.Header.Get("Authorization"); header != "" {
		return resolveBearer(r, db, header)
	}

	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return Identity{}, unauthenticated("no credential was presented")
	}

	return resolveSession(r, db, cookie.Value)
}

// resolveBearer authenticates an Authorization header. The Bearer scheme
// is case-insensitive per RFC 9110 section 11.1; a value without it or
// without the dlt_ prefix is not a dl-tool token.
func resolveBearer(r *http.Request, db *sqlx.DB, header string) (Identity, error) {
	scheme, token, _ := strings.Cut(header, " ")
	if !strings.EqualFold(scheme, authSchemeBearer) || !strings.HasPrefix(token, apiTokenPrefix) {
		return Identity{}, unauthenticated("the Authorization header is not a dl-tool bearer token")
	}

	user, err := store.UserByAPITokenHash(r.Context(), db, secure.HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return Identity{}, unauthenticated("the bearer token is unknown, expired or revoked")
	}
	if err != nil {
		return Identity{}, internalFailure(r.Context(), "resolve api token", err)
	}

	return Identity{User: user, Method: authMethodToken}, nil
}

// resolveSession authenticates the session cookie and throttles the
// last_seen_at bookkeeping write to once a minute per session.
func resolveSession(r *http.Request, db *sqlx.DB, cookieValue string) (Identity, error) {
	session, user, err := store.SessionByTokenHash(r.Context(), db, secure.HashToken(cookieValue))
	if errors.Is(err, store.ErrNotFound) {
		return Identity{}, unauthenticated("the session is unknown or has expired")
	}
	if err != nil {
		return Identity{}, internalFailure(r.Context(), "resolve session", err)
	}

	if time.Since(time.UnixMilli(session.LastSeenAt)) >= sessionTouchInterval {
		if err := store.TouchSession(r.Context(), db, session.ID, time.Now().UnixMilli()); err != nil {
			// Bookkeeping must not fail the request.
			logFromContext(r.Context()).Warn("session touch failed", slog.Any("err", err))
		}
	}

	return Identity{
		User:      user,
		Method:    authMethodSession,
		CSRF:      session.CSRFToken,
		SessionID: session.ID,
	}, nil
}

// csrfProblem applies the two credential-independent CSRF layers of
// docs/12-security-and-threat-model.md section 6.2, in order: the
// Origin/Referer host check on every authenticated request, then the
// X-DLTOOL-CSRF synchroniser token on cookie-authenticated mutations. Bearer
// requests are exempt from the token layer.
func csrfProblem(r *http.Request, identity Identity) error {
	if originHostMismatch(r) {
		return Problem(
			SlugCSRFTokenMissing,
			http.StatusForbidden,
			"the Origin or Referer host does not match the request host",
		)
	}

	if identity.Method != authMethodSession || !isMutatingMethod(r.Method) {
		return nil
	}
	if identity.CSRF == "" || !secure.EqualToken(r.Header.Get(csrfHeaderName), identity.CSRF) {
		// An empty stored csrf_token would make EqualToken("", "") pass and
		// leave the synchroniser layer vacuous; fail closed instead.
		return Problem(
			SlugCSRFTokenMissing,
			http.StatusForbidden,
			"a cookie-authenticated mutation needs a matching X-DLTOOL-CSRF header",
		)
	}

	return nil
}

// originHostMismatch is the second CSRF layer: when Origin is present — or,
// absent Origin, Referer — its host must equal the request host. Absent both
// headers means a non-browser client and passes. A Referer alone never grants
// anything: it is not a credential, only a same-origin signal.
func originHostMismatch(r *http.Request) bool {
	source := r.Header.Get("Origin")
	if source == "" {
		source = r.Header.Get("Referer")
	}
	if source == "" {
		return false
	}

	parsed, err := url.Parse(source)
	if err != nil || parsed.Host == "" {
		// Unparseable or hostless (Origin: null) fails closed.
		return true
	}

	return !strings.EqualFold(parsed.Host, r.Host)
}

// isMutatingMethod reports whether the method carries the CSRF token
// requirement (docs/05-api-contract.md section 1.2).
func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// unauthenticated is the 401 problem for an absent, expired or revoked credential.
func unauthenticated(detail string) error {
	return Problem(SlugUnauthenticated, http.StatusUnauthorized, detail)
}

// internalFailure logs the cause and answers the generic internal problem;
// the wire detail never carries the underlying error.
func internalFailure(ctx context.Context, operation string, err error) error {
	logFromContext(ctx).Error("internal failure",
		slog.String("operation", operation),
		slog.Any("err", err),
	)

	return Problem(SlugInternal, http.StatusInternalServerError, "an internal error occurred")
}

// loginThrottle combines an account backoff with atomic source admission.
type loginThrottle struct {
	mu       sync.Mutex
	accounts map[string]time.Time // username → earliest next attempt
	failures map[string]int
	sources  map[string]sourceBucket
}

// sourceBucket measures tokens in refill time, avoiding fractional rounding.
type sourceBucket struct {
	credit time.Duration
	at     time.Time
}

func newLoginThrottle() loginThrottle {
	return loginThrottle{
		accounts: map[string]time.Time{},
		failures: map[string]int{},
		sources:  map[string]sourceBucket{},
	}
}

// accountKey matches SQLite's ASCII-only NOCASE username comparison.
func accountKey(username string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, username)
}

// checkAccount allows an elapsed wait without erasing consecutive failures.
func (t *loginThrottle) checkAccount(username string, now time.Time) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	username = accountKey(username)
	eligible, pending := t.accounts[username]
	if pending && now.Sub(eligible) >= throttleBackoffCap {
		delete(t.accounts, username)
		delete(t.failures, username)
	}
	if !pending || !now.Before(eligible) {
		return 0, false
	}

	return eligible.Sub(now), true
}

// checkSource reserves an attempt before password work, even under concurrency.
func (t *loginThrottle) checkSource(ip string, now time.Time) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	bucket, exists := t.sources[ip]
	if !exists {
		bucket = sourceBucket{credit: throttleSourceWindow, at: now}
	}
	if now.After(bucket.at) {
		bucket.credit += min(now.Sub(bucket.at), throttleSourceWindow-bucket.credit)
		bucket.at = now
	}

	const attemptCost = throttleSourceWindow / throttleSourceCapacity
	if bucket.credit < attemptCost {
		t.sources[ip] = bucket
		return attemptCost - bucket.credit, true
	}

	bucket.credit -= attemptCost
	t.sources[ip] = bucket
	if len(t.sources) > throttleSweepFloor {
		t.sweepLocked(now)
	}
	return 0, false
}

// recordFailure escalates only the account; source admission already spent a token.
func (t *loginThrottle) recordFailure(username string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	username = accountKey(username)
	failures := min(t.failures[username]+1, throttleBackoffMaxShift+1)
	t.failures[username] = failures
	delay := min(throttleBackoffStart<<(failures-1), throttleBackoffCap)
	t.accounts[username] = now.Add(delay)

	if len(t.accounts) > throttleSweepFloor {
		t.sweepLocked(now)
	}
}

// recordSuccess clears the account ladder without refunding its source token.
func (t *loginThrottle) recordSuccess(username string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	username = accountKey(username)
	delete(t.accounts, username)
	delete(t.failures, username)
}

// sweepLocked retains recent failures, but drops inactive account and source keys.
func (t *loginThrottle) sweepLocked(now time.Time) {
	for username, eligible := range t.accounts {
		if now.Sub(eligible) >= throttleBackoffCap {
			delete(t.accounts, username)
			delete(t.failures, username)
		}
	}
	for ip, bucket := range t.sources {
		if now.Sub(bucket.at) >= throttleSourceWindow {
			delete(t.sources, ip)
		}
	}
}

// rateLimited answers 429 /problems/rate-limited with the Retry-After
// header doc 12 section 6.3 requires, rounded up to whole seconds.
func rateLimited(wait time.Duration) error {
	seconds := int(math.Ceil(wait.Seconds()))
	if seconds < 1 {
		seconds = 1
	}

	return huma.ErrorWithHeaders(
		Problem(SlugRateLimited, http.StatusTooManyRequests, rateLimitedDetail),
		http.Header{"Retry-After": {strconv.Itoa(seconds)}},
	)
}

// dummyPasswordHash exists so an unknown username costs the same argon2id
// work as a known one (doc 12 section 6.3, "identical timing"). Its value
// is irrelevant; it is minted with the current parameters and never matches.
var dummyPasswordHash = sync.OnceValue(func() string {
	hash, err := secure.HashPassword("dl-tool login timing equalisation placeholder")
	if err != nil {
		panic("api: mint dummy password hash: " + err.Error())
	}

	return hash
})

// userBody is the API shape of the operator account (doc 05 section 4.1):
// RFC 3339 timestamps, no secrets.
type userBody struct {
	ID          string  `json:"id" doc:"User id"`
	Username    string  `json:"username" doc:"Account username"`
	Enabled     bool    `json:"enabled" doc:"Whether the account may log in"`
	Locale      string  `json:"locale" doc:"Preferred UI locale"`
	LastLoginAt *string `json:"last_login_at" format:"date-time" doc:"RFC 3339 timestamp of the last successful login"`
	CreatedAt   string  `json:"created_at" format:"date-time" doc:"RFC 3339 timestamp of account creation"`
}

func newUserBody(u store.User) userBody {
	body := userBody{
		ID:        u.ID,
		Username:  u.Username,
		Enabled:   u.Enabled,
		Locale:    u.Locale,
		CreatedAt: time.UnixMilli(u.CreatedAt).UTC().Format(time.RFC3339),
	}
	if u.LastLoginAt != nil {
		stamp := time.UnixMilli(*u.LastLoginAt).UTC().Format(time.RFC3339)
		body.LastLoginAt = &stamp
	}

	return body
}

// authEnvelope is the {"user":…,"csrf_token":…} body shared by setup,
// login and me (doc 05 sections 4.1 and 4.2).
type authEnvelope struct {
	User      userBody `json:"user" doc:"The operator account"`
	CSRFToken string   `json:"csrf_token" doc:"Per-session token; send as X-DLTOOL-CSRF on cookie-authenticated mutations"`
}

// registerOperations registers the four /auth operations of doc 05 section 4
// on the Huma API; Server.registerOperations is the call site.
func (a *authService) registerOperations(hapi huma.API) {
	huma.Register(hapi, huma.Operation{
		OperationID:   "post-auth-setup",
		Method:        http.MethodPost,
		Path:          "/auth/setup",
		DefaultStatus: http.StatusCreated,
		Summary:       "Create the operator account with the one-time setup token",
		Description:   "Callable only while the operator account does not exist. Every other endpoint answers 401 /problems/setup-required until it succeeds.",
		Tags:          []string{"auth"},
		Security:      []map[string][]string{},
	}, a.handleSetup)

	huma.Register(hapi, huma.Operation{
		OperationID: "post-auth-login",
		Method:      http.MethodPost,
		Path:        "/auth/login",
		Summary:     "Start a session with username and password",
		Description: "Returns the session cookie and the per-session CSRF token.",
		Tags:        []string{"auth"},
		Security:    []map[string][]string{},
	}, a.handleLogin)

	huma.Register(hapi, huma.Operation{
		OperationID: "post-auth-logout",
		Method:      http.MethodPost,
		Path:        "/auth/logout",
		Summary:     "End the session",
		Description: "Deletes the server-side session row and expires the cookie; a bearer call has no session to delete and does not revoke the token. Cookie-authenticated calls must send X-DLTOOL-CSRF.",
		Tags:        []string{"auth"},
		Security:    credentialRequired,
	}, a.handleLogout)

	huma.Register(hapi, huma.Operation{
		OperationID: "get-auth-me",
		Method:      http.MethodGet,
		Path:        "/auth/me",
		Summary:     "Read the caller's account and CSRF token",
		Description: "What the SPA calls on boot to choose between the setup wizard, the login screen and the app shell. With bearer authentication the csrf_token is empty.",
		Tags:        []string{"auth"},
		Security:    credentialRequired,
	}, a.handleMe)
}

type setupInput struct {
	Body struct {
		SetupToken string `json:"setup_token" required:"true" minLength:"1" doc:"The one-time token printed at first boot"`
		Username   string `json:"username" required:"true" minLength:"1" doc:"Operator username"`
		Password   string `json:"password" required:"true" minLength:"12" doc:"Account password, at least 12 characters"`
		Locale     string `json:"locale,omitempty" default:"en" doc:"Preferred UI locale"`
	}
}

type setupOutput struct {
	Status       int    `enum:"201" doc:"Created"`
	SetCookie    string `header:"Set-Cookie" doc:"The new session cookie"`
	CacheControl string `header:"Cache-Control" doc:"no-store; the body carries the CSRF token"`
	Body         authEnvelope
}

// handleSetup creates the single operator account. The endpoint sits behind
// the same per-source-IP budget as login while it is callable, because a
// guessed setup token mints the operator account outright (doc 12 section
// 6.4).
func (a *authService) handleSetup(ctx context.Context, input *setupInput) (*setupOutput, error) {
	// Serialize the whole critical section — gate check, insert, token
	// teardown — so two concurrent calls can never both create the account
	// or race the file removal.
	a.setupMu.Lock()
	defer a.setupMu.Unlock()

	complete, err := a.setupComplete(ctx)
	if err != nil {
		return nil, internalFailure(ctx, "count users", err)
	}
	if complete {
		return nil, Problem(SlugSetupAlreadyComplete, http.StatusConflict, setupCompleteDetail)
	}

	info := requestInfoFrom(ctx)
	now := time.Now()
	if wait, limited := a.throttle.checkSource(info.PeerIP, now); limited {
		return nil, rateLimited(wait)
	}
	if !secure.EqualToken(input.Body.SetupToken, a.setupToken) {
		// The source budget covers token guessing; the per-account ladder is
		// for login only — an attacker must not pre-load it for a username
		// they pick freely during the setup window.
		logFromContext(ctx).Warn(eventSetupFailed, slog.String("source_ip", info.PeerIP))

		return nil, unauthenticated(setupTokenDetail)
	}

	hash, err := secure.HashPassword(input.Body.Password)
	if err != nil {
		return nil, internalFailure(ctx, "hash password", err)
	}
	user := store.User{
		ID:           store.NewID(store.PrefixUser),
		Username:     input.Body.Username,
		PasswordHash: hash,
		Enabled:      true,
		Locale:       input.Body.Locale,
	}
	if err := store.CreateUser(ctx, a.db, user); err != nil {
		return nil, internalFailure(ctx, "create user", err)
	}
	// CreateUser stamps the row's timestamps; mirror the created_at stamp
	// so the response envelope carries it.
	user.CreatedAt = now.UnixMilli()

	// The token is spent: drop the file and the in-memory copy, and close
	// the gate for the rest of this process.
	if err := os.Remove(filepath.Join(a.cfg.ConfigDir, setupTokenFileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, internalFailure(ctx, "remove setup token", err)
	}
	a.setupToken = ""
	a.setupDone.Store(true)

	envelope, cookieValue, err := a.issueSession(ctx, user, now)
	if err != nil {
		return nil, err
	}

	return &setupOutput{
		Status:       http.StatusCreated,
		SetCookie:    NewSessionCookie(a.cfg, cookieValue, info.Transport).String(),
		CacheControl: cacheControlNoStore,
		Body:         *envelope,
	}, nil
}

type loginInput struct {
	Body struct {
		Username string `json:"username" required:"true" minLength:"1" doc:"Account username"`
		Password string `json:"password" required:"true" doc:"Account password"`
	}
}

type loginOutput struct {
	SetCookie    string `header:"Set-Cookie" doc:"The new session cookie"`
	CacheControl string `header:"Cache-Control" doc:"no-store; the body carries the CSRF token"`
	Body         authEnvelope
}

// handleLogin verifies the body credentials. Every failure — wrong
// password, disabled account, unknown user — answers the same detail after
// the same argon2id work, so the endpoint cannot enumerate accounts (doc 12
// section 6.3).
func (a *authService) handleLogin(ctx context.Context, input *loginInput) (*loginOutput, error) {
	info := requestInfoFrom(ctx)
	now := time.Now()
	if wait, limited := a.throttle.checkSource(info.PeerIP, now); limited {
		return nil, rateLimited(wait)
	}
	if wait, limited := a.throttle.checkAccount(input.Body.Username, now); limited {
		return nil, rateLimited(wait)
	}

	user, err := store.UserByUsername(ctx, a.db, input.Body.Username)
	if errors.Is(err, store.ErrNotFound) {
		// Burn the same argon2id work a real comparison would; the dummy
		// hash is minted with the current parameters, so this call cannot
		// fail and only its cost matters.
		_, _, _ = secure.VerifyPassword(dummyPasswordHash(), input.Body.Password)

		return nil, a.loginRejected(ctx, input.Body.Username, info.PeerIP, now)
	}
	if err != nil {
		return nil, internalFailure(ctx, "resolve user", err)
	}

	ok, needsRehash, err := secure.VerifyPassword(user.PasswordHash, input.Body.Password)
	if err != nil {
		return nil, internalFailure(ctx, "verify password", err)
	}
	if !ok || !user.Enabled {
		// A disabled account answers exactly like a wrong password.
		return nil, a.loginRejected(ctx, input.Body.Username, info.PeerIP, now)
	}
	if needsRehash {
		logFromContext(ctx).Debug("password hash is below the current parameters; upgrade it on the next password change")
	}

	a.throttle.recordSuccess(input.Body.Username)
	if err := store.TouchLastLogin(ctx, a.db, user.ID, now.UnixMilli()); err != nil {
		return nil, internalFailure(ctx, "touch last login", err)
	}
	lastLogin := now.UnixMilli()
	user.LastLoginAt = &lastLogin

	envelope, cookieValue, err := a.issueSession(ctx, user, now)
	if err != nil {
		return nil, err
	}

	return &loginOutput{
		SetCookie:    NewSessionCookie(a.cfg, cookieValue, info.Transport).String(),
		CacheControl: cacheControlNoStore,
		Body:         *envelope,
	}, nil
}

// loginRejected records the failure and answers the one 401 every login
// failure shares.
func (a *authService) loginRejected(ctx context.Context, username, ip string, now time.Time) error {
	a.throttle.recordFailure(username, now)
	logFromContext(ctx).Warn(eventLoginFailed, slog.String("source_ip", ip))

	return unauthenticated(loginRejectionDetail)
}

// issueSession mints the session cookie value and its CSRF token, persists
// the row and returns the response envelope plus the raw cookie value.
func (a *authService) issueSession(ctx context.Context, user store.User, now time.Time) (*authEnvelope, string, error) {
	cookieValue, err := secure.NewToken()
	if err != nil {
		return nil, "", internalFailure(ctx, "mint session token", err)
	}
	csrfToken, err := secure.NewToken()
	if err != nil {
		return nil, "", internalFailure(ctx, "mint csrf token", err)
	}

	session := store.Session{
		ID:     store.NewID(store.PrefixSession),
		UserID: user.ID,
		// Deliberately not hashed, unlike TokenHash: /auth/me must re-serve
		// the CSRF token to the SPA after a reload, and the token is useless
		// without the cookie it accompanies.
		TokenHash:  secure.HashToken(cookieValue),
		CSRFToken:  csrfToken,
		ExpiresAt:  now.Add(a.cfg.SessionTTL).UnixMilli(),
		LastSeenAt: now.UnixMilli(),
	}
	if err := store.CreateSession(ctx, a.db, session); err != nil {
		return nil, "", internalFailure(ctx, "create session", err)
	}

	return &authEnvelope{User: newUserBody(user), CSRFToken: csrfToken}, cookieValue, nil
}

type logoutInput struct{}

type logoutOutput struct {
	Status    int    `enum:"204" doc:"No Content"`
	SetCookie string `header:"Set-Cookie" doc:"The expired session cookie; absent for bearer authentication"`
}

// handleLogout deletes the server-side session row and expires the cookie.
// A bearer call has no session to delete and answers 204 all the same.
func (a *authService) handleLogout(ctx context.Context, _ *logoutInput) (*logoutOutput, error) {
	identity, ok := IdentityFrom(ctx)
	if !ok {
		// The middleware guarantees a credential; this guards wiring.
		return nil, unauthenticated(noCredentialDetail)
	}

	cookie := ""
	if identity.Method == authMethodSession {
		if err := store.DeleteSession(ctx, a.db, identity.SessionID); err != nil {
			return nil, internalFailure(ctx, "delete session", err)
		}
		expired := NewSessionCookie(a.cfg, "", requestInfoFrom(ctx).Transport)
		expired.MaxAge = -1
		cookie = expired.String()
	}

	return &logoutOutput{Status: http.StatusNoContent, SetCookie: cookie}, nil
}

type meInput struct{}

type meOutput struct {
	CacheControl string `header:"Cache-Control" doc:"no-store; the body carries the CSRF token"`

	Body authEnvelope
}

// handleMe returns the caller's account and CSRF token; for bearer
// authentication the csrf_token is empty, because no session exists.
func (a *authService) handleMe(ctx context.Context, _ *meInput) (*meOutput, error) {
	identity, ok := IdentityFrom(ctx)
	if !ok {
		// The middleware guarantees a credential; this guards wiring.
		return nil, unauthenticated(noCredentialDetail)
	}

	return &meOutput{
		CacheControl: cacheControlNoStore,
		Body:         authEnvelope{User: newUserBody(identity.User), CSRFToken: identity.CSRF},
	}, nil
}
