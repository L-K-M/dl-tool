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
	root.Use(middleware.RequestID, realIP(cfg.TrustedProxies), middleware.Recoverer, requestLogger(log))

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

// realIP rewrites RemoteAddr from X-Forwarded-For, but only when the direct
// peer is a configured trusted proxy (docs/10-deployment-and-compose.md
// section 7.3). chi's middleware.RealIP is deprecated for trusting the header
// from any peer.
func realIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			forwardedFor := r.Header.Get("X-Forwarded-For")
			if forwardedFor == "" || !isTrustedProxy(r.RemoteAddr, trusted) {
				next.ServeHTTP(w, r)

				return
			}

			// The leftmost entry is the original client.
			client, _, _ := strings.Cut(forwardedFor, ",")
			r.RemoteAddr = strings.TrimSpace(client)
			next.ServeHTTP(w, r)
		})
	}
}

// isTrustedProxy reports whether the direct peer address falls inside a
// trusted-proxy prefix.
func isTrustedProxy(remoteAddr string, trusted []netip.Prefix) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}

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

	query := u.Query()
	redacted := false
	for _, name := range secretQueryParameters {
		if _, ok := query[name]; ok {
			query.Set(name, redactedValue)
			redacted = true
		}
	}
	if !redacted {
		return uri
	}

	return u.EscapedPath() + "?" + query.Encode()
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
