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
	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/engine/aria2"
	"github.com/L-K-M/dl-tool/internal/obs"
	"github.com/L-K-M/dl-tool/internal/store"
	"github.com/L-K-M/dl-tool/internal/sync"
)

const (
	apiTitle         = "dl-tool"
	apiV1Path        = "/api/v1"
	problemMediaType = "application/problem+json"
	redactedValue    = "__redacted__"

	// reconcilerPollInterval is the reconciliation sweep cadence of
	// docs/17-operations-and-runbook.md section 1.6: once at boot, then 1 Hz.
	reconcilerPollInterval = time.Second

	// bootSweepBudget bounds the boot sweep inside NewServer: an engine
	// that black-holes packets would otherwise hold startup — and so the
	// listener and the UI — for its full per-call RPC timeout, serially per
	// engine. Whatever the sweep misses, the 1 Hz poll picks up seconds
	// later (docs/17 section 1.6: a boot must not be locked out of its UI
	// by an engine that is down).
	bootSweepBudget = 10 * time.Second
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

	// Health owns the /healthz and /readyz handlers on the base router;
	// cmd/dl-tool calls its MarkReady once store.Open has returned.
	Health *obs.Health

	// auth owns the first-run gate, the /auth operations and the login
	// throttles.
	auth *authService

	// Engines is the routing-time availability table: the create endpoint
	// resolves every URI to an engine through it and answers 503 when that
	// engine is not registered (doc 05 section 5.2). It is exported so tests
	// can register stand-ins into this shared registry; the task handlers
	// capture the same pointer in NewServer, so reassigning the field after
	// construction has no effect.
	Engines *engine.Registry

	// tasks owns the /tasks collection operations.
	tasks *TaskHandlers

	// SSE owns the live-update endpoints: GET /events and GET /sync, and
	// the hub they read from. It is exported for the composition root in
	// cmd/dl-tool, which owns the *obs.Metrics instance whose
	// dltool_sse_clients gauge the hub's client count connects to.
	SSE *SSEHandlers
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

	// The process endpoints of docs/05-api-contract.md section 13.1 sit on
	// the base router — under DLTOOL_BASE_PATH, outside /api/v1 and therefore
	// outside the Authenticate middleware. With a nil db they stay up but
	// never report ready. /metrics is deliberately absent: it belongs to the
	// metrics listener alone.
	health := obs.NewHealth(db)
	base.Get("/healthz", health.Live)
	base.Get("/readyz", health.Ready)

	// The auth service owns the first-run gate, the /auth operations and
	// the brute-force controls. With a nil db it registers its operations
	// for the generated document but stays inert — and silent, for the same
	// stdout reason as the middleware below.
	auth, err := newAuthService(cfg, db, log)
	if err != nil {
		return nil, fmt.Errorf("build auth service: %w", err)
	}

	// Every route under /api/v1 requires a session cookie or a bearer token
	// (docs/05-api-contract.md section 1.2), except the two /auth operations
	// that authenticate by body; /healthz, /readyz and the SPA live on the
	// base router and stay anonymous. Use must run before humachi.New
	// registers the first route on v1. A nil db — the openapi subcommand and
	// router-only unit tests — skips the middleware: there is no store to
	// resolve credentials against. Deliberately silent: the openapi
	// subcommand's stdout is the committed api/openapi.json, so a
	// construction-time log line would corrupt the gen drift gate.
	if db != nil {
		v1.Use(auth.middleware())
	}

	humaConfig := huma.DefaultConfig(apiTitle, Version)
	// Huma's UI fetches CDN assets; serve the local specification without it.
	humaConfig.DocsPath = ""
	humaConfig.Servers = []*huma.Server{{URL: cfg.BasePath + apiV1Path}}
	// The default create hook installs the schema-link transformer, which would
	// inject a $schema member into every response; docs/05-api-contract.md
	// section 1 defines no such member.
	humaConfig.CreateHooks = nil

	// The engine registry owns engine availability at routing time. The
	// aria2 adapter joins when its RPC endpoint is configured; adapters that
	// arrive with their own tasks register the same way, and a routed engine
	// that is absent leaves POST /tasks answering 503 for its URIs. A
	// malformed RPC URL is a configuration error and fails server
	// construction loudly — degradation to 503 is for engines that are
	// absent or down, not for operator mistakes.
	engines := engine.NewRegistry()
	if cfg.Aria2URL != "" {
		aria2Engine, err := aria2.New(aria2.Config{URL: cfg.Aria2URL, Secret: cfg.Aria2Secret.Reveal()}, nil)
		if err != nil {
			return nil, fmt.Errorf("build aria2 engine: %w", err)
		}
		engines.Register(aria2Engine)
	}

	// The sync hub fans task deltas to SSE subscribers at 1 Hz and answers
	// GET /sync from its ring. The loop is process-lifetime: it starts with
	// the server, idles through snapshot errors (a closed store) and dies
	// with the process — no request context could own it. With a nil db the
	// operations still register for the generated document, but nothing
	// feeds the hub.
	hub := sync.NewHub()
	sseHandlers := NewSSEHandlers(hub, db)
	if db != nil {
		go func() {
			// Loop returns nil once its context is cancelled; anything else
			// is a defect worth a log line even though the loop runs detached.
			if err := hub.Loop(context.Background(), time.Second, sseHandlers.Snapshot); err != nil {
				log.Error("sync loop stopped", slog.String("err", err.Error()))
			}
		}()
	}

	server := &Server{
		Router:  root,
		Base:    base,
		V1:      v1,
		API:     humachi.New(v1, humaConfig),
		db:      db,
		Health:  health,
		auth:    auth,
		Engines: engines,
		tasks:   NewTaskHandlers(db, engines, cfg.DataRoots),
		SSE:     sseHandlers,
	}
	// The two credentials of docs/05-api-contract.md section 1.2, so the
	// generated document tells clients how the API is protected. Individual
	// operations declare their own requirements (or an explicit empty one
	// for the two that authenticate by body).
	server.API.OpenAPI().Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		schemeSession: {
			Type:        "apiKey",
			Description: "The dltool_session cookie issued by POST /auth/setup and POST /auth/login; mutations also need the X-DLTOOL-CSRF header.",
			Name:        sessionCookieName,
			In:          "cookie",
		},
		schemeBearer: {
			Type:         "http",
			Scheme:       "bearer",
			BearerFormat: apiTokenPrefix,
			Description:  "An API token issued by the account settings; bearer requests are exempt from CSRF.",
		},
	}
	server.registerOperations()

	// The namespaces the server owns are reserved before the SPA catch-all,
	// so a typo under them fails loudly as problem+json instead of serving
	// the SPA shell: an unknown API route, a health subpath, and /metrics,
	// which belongs to the metrics listener alone (doc 05 section 1.1).
	for _, reserved := range []string{"/api", "/api/*", "/metrics", "/metrics/*", "/healthz/*", "/readyz/*"} {
		base.Handle(reserved, http.HandlerFunc(notFound))
	}

	// The reconciler keeps the tasks table in step with the engines: one
	// full sweep here — stage S10 of docs/17-operations-and-runbook.md
	// section 1, before main opens the listener, so a server built by
	// NewServer has reconciled once before it accepts its first request —
	// then the 1 Hz poll loop beside the hub's. A failed sweep is a warning,
	// never a construction error: a boot must not be locked out of its UI
	// by an engine that is down (doc 17 section 1.6). The loop's context is
	// the process lifetime, like the hub loop above — no request context
	// could own it; Run stops with that context and the process exits with
	// the server. A nil db (the openapi subcommand, router-only tests) has
	// no tasks to reconcile and skips both.
	if db != nil {
		reconciler := engine.NewReconciler(engines, store.NewTaskStore(db), reconcilerPollInterval)
		bootCtx, cancelBoot := context.WithTimeout(context.Background(), bootSweepBudget)
		if err := reconciler.Boot(bootCtx); err != nil {
			log.Warn("boot reconciliation failed; retrying on the poll loop",
				slog.String("err", err.Error()))
		}
		cancelBoot()
		go func() {
			if err := reconciler.Run(context.Background()); err != nil {
				log.Error("reconciler stopped", slog.String("err", err.Error()))
			}
		}()
	}

	// The SPA mounts last, on the base catch-all, so /api/v1, /healthz and
	// /readyz keep every route they claim (doc 10 section 7.3 rules 2 and 8).
	// Nothing is registered outside the base: with BasePath=/dl-tool a request
	// to /anything stays a 404.
	spa, err := SPAHandler(cfg.BasePath)
	if err != nil {
		return nil, fmt.Errorf("build SPA handler: %w", err)
	}
	base.Handle("/*", spa)

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
	s.auth.registerOperations(s.API)
	s.tasks.registerOperations(s.API)
	s.SSE.RegisterOperations(s.API)

	// The bulk-action and patch operations of docs/05-api-contract.md
	// sections 5.7 and 5.5; their handlers live in tasks_actions.go.
	huma.Register(s.API, huma.Operation{
		OperationID: operationTaskActions,
		Method:      http.MethodPost,
		Path:        "/tasks/actions",
		Summary:     "Apply bulk lifecycle actions",
		Description: "Applies one of the nine actions to up to 500 tasks and reports a per-id outcome, so one bad id never fails the batch. The queue actions rewrite dl-tool's own queue and contact no engine. delete_data is accepted but currently a no-op: local data removal arrives with a later task.",
		Tags:        []string{"tasks"},
		Security:    credentialRequired,
	}, s.tasks.Actions)

	huma.Register(s.API, huma.Operation{
		OperationID: operationPatchTask,
		Method:      http.MethodPatch,
		Path:        "/tasks/{id}",
		Summary:     "Update a task",
		Description: "Partial update of the display name, category, tags, per-task rate limits, share limits and the sequential flag. Omitted fields are untouched; a non-nil tags array replaces the whole set. The rate limits reach a running task without restarting it.",
		Tags:        []string{"tasks"},
		Security:    credentialRequired,
	}, s.tasks.PatchTask)

	huma.Register(s.API, huma.Operation{
		OperationID: operationDeleteTask,
		Method:      http.MethodDelete,
		Path:        "/tasks/{id}",
		Summary:     "Remove a task with or without its data",
		Description: "Marks the task removed and, with delete_data=true, unlinks exactly the files recorded in task_files after re-checking every resolved path against the data roots. delete_data=false keeps every byte. force_complete=true completes the task instead of removing it. The response reports what happened, so a client never has to guess.",
		Tags:        []string{"tasks"},
		Security:    credentialRequired,
		// Same strictness as the read path: a mistyped query key is 422.
		RejectUnknownQueryParameters: true,
	}, s.tasks.DeleteTask)

	huma.Register(s.API, huma.Operation{
		OperationID: operationListTaskEvents,
		Method:      http.MethodGet,
		Path:        "/tasks/{id}/events",
		Summary:     "Read the task's event log",
		Description: "Cursor-paginated, newest first; one row per state transition and engine outcome. code is a stable i18n key; the UI translates it and falls back to message.",
		Tags:        []string{"tasks"},
		Security:    credentialRequired,
		// Same strictness as every other query-carrying operation: a
		// mistyped query key is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, s.tasks.ListTaskEvents)

	huma.Register(s.API, huma.Operation{
		OperationID: "get-system-info",
		Method:      http.MethodGet,
		Path:        "/system/info",
		Summary:     "Read system information",
		// Behind the same middleware as every /api/v1 route.
		Security: credentialRequired,
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

// peerAddressContextKey preserves the socket peer before proxy rewriting.
type peerAddressContextKey struct{}

// realIP rewrites RemoteAddr to the original client address from
// X-Forwarded-For, but only when the direct peer is a configured trusted
// proxy (docs/10-deployment-and-compose.md section 7.3). chi's
// middleware.RealIP is deprecated for trusting the header from any peer.
func realIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// TLS forwarding is trusted by socket peer, not by the client IP.
			r = r.WithContext(context.WithValue(r.Context(), peerAddressContextKey{}, r.RemoteAddr))
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
