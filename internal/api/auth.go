package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

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

	authSchemeBearer = "Bearer "
	apiTokenPrefix   = "dlt_"

	// Identity.Method values.
	authMethodSession = "session"
	authMethodToken   = "token"

	// sessionTouchInterval bounds the last_seen_at write rate: at most one
	// write per minute and session, no matter how chatty the client is.
	sessionTouchInterval = time.Minute
)

// Identity is what the middleware puts on the request context.
type Identity struct {
	User   store.User
	Method string // "session" | "token"
	CSRF   string // the session's csrf_token; empty for token authentication
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
// the request arrived over TLS. T009's login and setup set it.
func NewSessionCookie(cfg *config.Config, value string, transport Transport) *http.Cookie {
	cookie := &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     cfg.BasePath + "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if transport == TransportTLS {
		cookie.Secure = true
	}

	return cookie
}

// Authenticate resolves the cookie or the bearer token, enforces CSRF on
// POST, PUT, PATCH and DELETE made with cookie authentication, and answers
// 401 /problems/unauthenticated or 403 /problems/csrf-token-missing.
//
// cfg is part of the contract for the cookie attributes above and for the
// session lifetime T009 will need; the checks themselves are
// config-independent today.
func Authenticate(db *sqlx.DB, cfg *config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity, problem := resolveIdentity(r, db)
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
			ctx := context.WithValue(r.Context(), identityContextKey{}, identity)
			ctx = context.WithValue(ctx, loggerContextKey{}, logFromContext(ctx).With(
				slog.String("user_id", identity.User.ID),
			))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
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

// resolveBearer authenticates an Authorization header. A value without the
// Bearer scheme or without the dlt_ prefix is not a dl-tool token.
func resolveBearer(r *http.Request, db *sqlx.DB, header string) (Identity, error) {
	token, ok := strings.CutPrefix(header, authSchemeBearer)
	if !ok || !strings.HasPrefix(token, apiTokenPrefix) {
		return Identity{}, unauthenticated("the Authorization header is not a dl-tool bearer token")
	}

	user, err := store.UserByAPITokenHash(r.Context(), db, secure.HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return Identity{}, unauthenticated("the bearer token is unknown, expired or revoked")
	}
	if err != nil {
		return Identity{}, storeFailure(r, "resolve api token", err)
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
		return Identity{}, storeFailure(r, "resolve session", err)
	}

	if time.Since(time.UnixMilli(session.LastSeenAt)) >= sessionTouchInterval {
		if err := store.TouchSession(r.Context(), db, session.ID, time.Now().UnixMilli()); err != nil {
			// Bookkeeping must not fail the request.
			logFromContext(r.Context()).Warn("session touch failed", slog.Any("err", err))
		}
	}

	return Identity{User: user, Method: authMethodSession, CSRF: session.CSRFToken}, nil
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
	if !secure.EqualToken(r.Header.Get(csrfHeaderName), identity.CSRF) {
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

// storeFailure logs the cause and answers the generic internal problem; the
// wire detail never carries the store error.
func storeFailure(r *http.Request, operation string, err error) error {
	logFromContext(r.Context()).Error("authentication store failure",
		slog.String("operation", operation),
		slog.Any("err", err),
	)

	return Problem(SlugInternal, http.StatusInternalServerError, "an internal error occurred")
}
