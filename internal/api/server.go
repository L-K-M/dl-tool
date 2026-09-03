// Package api owns the HTTP surface: one chi sub-router mounted at the
// configured base path, carrying the Huma-built /api/v1 API, and nothing else.
package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/config"
)

const (
	apiTitle         = "dl-tool"
	apiV1Path        = "/api/v1"
	problemMediaType = "application/problem+json"
	redactedValue    = "__redacted__"
)

// Version is the build version the spec and the system-info placeholder
// report. main overrides it with the linker-stamped value before NewServer.
var Version = "dev"

// Server owns the chi router, the Huma API and the HTTP listener.
type Server struct {
	Router chi.Router
	// Base is the sub-router mounted at cfg.BasePath. /healthz, /readyz and the SPA go here.
	Base chi.Router
	// V1 is the /api/v1 sub-router the Huma API is built on. Every Huma operation path is
	// relative to it, so an operation registered as "/tasks" is served at <base>/api/v1/tasks
	// and appears in the document as "/tasks" beneath servers[0].url.
	V1  chi.Router
	API huma.API

	db *sqlx.DB
}

// NewServer builds the router and the Huma API. Every route lives under
// cfg.BasePath; a request outside it gets 404. db may be nil in a unit test.
func NewServer(cfg *config.Config, db *sqlx.DB, log *slog.Logger) (*Server, error) {
	installErrorFactory()

	root := chi.NewMux()
	// Set the problem handlers before Mount so chi copies them into every child.
	root.NotFound(notFound)
	root.MethodNotAllowed(notFound)
	// requestLogger wraps recoverer so a panicked request is still logged,
	// with the status the recoverer wrote.
	root.Use(middleware.RequestID, realIP(cfg.TrustedProxies), requestLogger(log), recoverer)

	// One sub-router carries everything; nothing is registered outside it
	// (docs/10-deployment-and-compose.md section 7.3 rule 2).
	base := chi.NewMux()
	root.Mount(cmp.Or(cfg.BasePath, "/"), base)

	v1 := chi.NewMux()
	base.Mount(apiV1Path, v1)

	humaConfig := huma.DefaultConfig(apiTitle, Version)
	humaConfig.Servers = []*huma.Server{{URL: cfg.BasePath + apiV1Path}}
	// The default create hook installs the schema-link transformer, which would
	// inject a $schema member into every response; docs/05-api-contract.md
	// section 1 defines no such member.
	humaConfig.CreateHooks = nil

	server := &Server{
		Router: root,
		Base:   base,
		V1:     v1,
		API:    humachi.New(v1, humaConfig),
		db:     db,
	}
	server.registerOperations()

	return server, nil
}

// Spec renders the OpenAPI 3.1 document the `openapi` subcommand prints. The
// bytes are identical to what GET <base>/api/v1/openapi.json serves.
func (s *Server) Spec() ([]byte, error) {
	spec, err := json.Marshal(s.API.OpenAPI())
	if err != nil {
		return nil, fmt.Errorf("render openapi document: %w", err)
	}

	return spec, nil
}

// registerOperations mounts the placeholder operation that keeps the document
// non-empty until the real resources land; their tasks own them.
func (s *Server) registerOperations() {
	huma.Register(s.API, huma.Operation{
		OperationID: "get-system-info",
		Method:      http.MethodGet,
		Path:        "/system/info",
		Summary:     "Read system information",
	}, func(_ context.Context, _ *systemInfoInput) (*systemInfoOutput, error) {
		output := &systemInfoOutput{}
		output.Body.Version = Version

		return output, nil
	})
}

type systemInfoInput struct{}

type systemInfoOutput struct {
	Body struct {
		Version string `json:"version" doc:"Build version of the dl-tool process"`
	}
}

// loggerContextKey carries the request-scoped logger on the request context.
type loggerContextKey struct{}

// requestLogger logs one line per request and puts a *slog.Logger carrying
// request_id on the context; handlers take it from there (doc 14 section 3.1).
func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			requestLog := log.With(slog.String("request_id", middleware.GetReqID(r.Context())))

			wrapped := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(wrapped, r.WithContext(context.WithValue(r.Context(), loggerContextKey{}, requestLog)))

			requestLog.Info("request",
				slog.String("method", r.Method),
				slog.String("path", redactedRequestURI(r.URL)),
				slog.Int("status", wrapped.Status()),
				slog.Int("bytes", wrapped.BytesWritten()),
				slog.Int64("duration_ms", time.Since(started).Milliseconds()),
			)
		})
	}
}

// recoverer turns a handler panic into the registry's internal problem and
// logs the stack; chi's Recoverer would write a bare non-conforming 500.
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				if err, ok := recovered.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					// A control-flow signal, not a fault: re-panic so net/http
					// aborts the connection quietly, as chi's Recoverer does.
					panic(err)
				}
				logFromContext(r.Context()).Error("panic recovered",
					slog.Any("err", recovered),
					slog.String("stack", string(debug.Stack())),
				)
				writeProblem(w, Problem(SlugInternal, http.StatusInternalServerError, "an internal error occurred"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// realIP rewrites RemoteAddr to the original client address from
// X-Forwarded-For, but only when the direct peer is a configured trusted
// proxy (docs/10-deployment-and-compose.md section 7.3). chi's
// middleware.RealIP is deprecated for trusting the header from any peer.
func realIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			forwardedFor := r.Header.Get("X-Forwarded-For")
			if forwardedFor == "" || !isTrustedProxy(r.RemoteAddr, trusted) {
				next.ServeHTTP(w, r)

				return
			}

			r.RemoteAddr = clientFromForwardedFor(forwardedFor, trusted)
			next.ServeHTTP(w, r)
		})
	}
}

// clientFromForwardedFor walks the chain right to left past every trusted
// hop and returns the first untrusted entry. The leftmost entries are
// client-supplied and spoofable; the rightmost is what the nearest trusted
// proxy observed.
func clientFromForwardedFor(forwardedFor string, trusted []netip.Prefix) string {
	entries := strings.Split(forwardedFor, ",")
	for i := len(entries) - 1; i > 0; i-- {
		entry := strings.TrimSpace(entries[i])
		if !isTrustedIP(entry, trusted) {
			return entry
		}
	}

	return strings.TrimSpace(entries[0])
}

// isTrustedProxy reports whether the direct peer address falls inside a
// trusted-proxy prefix. A rewritten RemoteAddr may carry no port, so the
// port split is best-effort.
func isTrustedProxy(remoteAddr string, trusted []netip.Prefix) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}

	return isTrustedIP(host, trusted)
}

// isTrustedIP reports whether the bare address falls inside a trusted-proxy
// prefix.
func isTrustedIP(host string, trusted []netip.Prefix) bool {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}

	for _, prefix := range trusted {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// logFromContext returns the request-scoped logger, or the process default
// when called outside a request.
func logFromContext(ctx context.Context) *slog.Logger {
	if log, ok := ctx.Value(loggerContextKey{}).(*slog.Logger); ok && log != nil {
		return log
	}

	return slog.Default()
}

// secretQueryParameters names the query keys whose values never reach logs.
var secretQueryParameters = []string{"apikey", "token", "passkey"}

// redactedRequestURI renders the request URI with the known secret query
// parameters redacted (doc 14 section 3.3). Headers are never logged at all,
// so Authorization, Cookie and X-Api-Key cannot leak through this path either.
func redactedRequestURI(u *url.URL) string {
	uri := u.RequestURI()
	if u.RawQuery == "" {
		return uri
	}

	query, parseErr := url.ParseQuery(u.RawQuery)
	redacted := false
	for key := range query {
		if isSecretQueryParameter(key) {
			query.Set(key, redactedValue)
			redacted = true
		}
	}

	if redacted {
		return u.EscapedPath() + "?" + query.Encode()
	}
	if parseErr != nil {
		// A malformed pair may hide a secret the parser dropped; fail closed.
		return u.EscapedPath() + "?" + redactedValue
	}

	return uri
}

// isSecretQueryParameter matches keys case-insensitively: Token= must redact
// as surely as token=.
func isSecretQueryParameter(key string) bool {
	for _, name := range secretQueryParameters {
		if strings.EqualFold(key, name) {
			return true
		}
	}

	return false
}

// notFound answers every unmatched route — and every unmatched method on a
// registered one — with the registry's not-found problem, so chi's plain-text
// 404 and 405 defaults never escape the router.
func notFound(w http.ResponseWriter, _ *http.Request) {
	writeProblem(w, Problem(SlugNotFound, http.StatusNotFound, "the requested route does not exist"))
}

// writeProblem renders a problem outside Huma's operation pipeline, for
// router-level misses.
func writeProblem(w http.ResponseWriter, problem error) {
	var model *huma.ErrorModel
	if !errors.As(problem, &model) {
		model = internalProblem()
	}

	body, err := json.Marshal(model)
	if err != nil {
		// ErrorModel holds only marshalable values; this guard is a last resort.
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", problemMediaType)
	w.WriteHeader(model.Status)
	if _, err := w.Write(body); err != nil {
		// The status line is already sent; a write failure means the client went away.
		return
	}
}

// internalProblem is the fallback shape for a problem that is not an ErrorModel.
func internalProblem() *huma.ErrorModel {
	return &huma.ErrorModel{
		Type:   SlugInternal,
		Title:  http.StatusText(http.StatusInternalServerError),
		Status: http.StatusInternalServerError,
		Detail: "an internal error occurred",
	}
}
