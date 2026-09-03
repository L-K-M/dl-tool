package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/L-K-M/dl-tool/internal/config"
)

// registrySlugs mirrors the slug registry of docs/05-api-contract.md section
// 1.3; every error response "type" must be one of these.
var registrySlugs = []string{
	SlugSetupRequired,
	SlugUnauthenticated,
	SlugForbidden,
	SlugConfigLocked,
	SlugCSRFTokenMissing,
	SlugPathRejected,
	SlugSSRFBlocked,
	SlugNotFound,
	SlugConflict,
	SlugConcurrencyLimit,
	SlugSetupAlreadyComplete,
	SlugPayloadTooLarge,
	SlugUnsupportedMediaType,
	SlugValidationFailed,
	SlugUnsupportedScheme,
	SlugRateLimited,
	SlugInternal,
	SlugEngineUnavailable,
	SlugNotReady,
}

type problemDocument struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Errors []struct {
		Message  string `json:"message"`
		Location string `json:"location"`
		Value    any    `json:"value"`
	} `json:"errors"`
}

func newTestServer(t *testing.T, basePath string) *Server {
	t.Helper()

	server, err := NewServer(
		&config.Config{BasePath: basePath},
		nil,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	return server
}

func do(t *testing.T, handler http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(method, target, nil))

	return recorder
}

func decodeProblem(t *testing.T, recorder *httptest.ResponseRecorder) problemDocument {
	t.Helper()

	var problem problemDocument
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem body %q: %v", recorder.Body.String(), err)
	}

	return problem
}

// assertProblem checks the RFC 9457 shape every error response must carry:
// all four mandatory members, the media type, and a type from the registry.
func assertProblem(t *testing.T, recorder *httptest.ResponseRecorder, status int, slug string) problemDocument {
	t.Helper()

	if contentType := recorder.Header().Get("Content-Type"); contentType != problemMediaType {
		t.Errorf("Content-Type = %q, want %q", contentType, problemMediaType)
	}
	if recorder.Code != status {
		t.Errorf("status = %d, want %d", recorder.Code, status)
	}

	problem := decodeProblem(t, recorder)
	if problem.Type != slug {
		t.Errorf("type = %q, want %q", problem.Type, slug)
	}
	if problem.Title == "" {
		t.Error("title is empty")
	}
	if problem.Status != status {
		t.Errorf("problem status = %d, want %d", problem.Status, status)
	}
	if problem.Detail == "" {
		t.Error("detail is empty")
	}

	for _, registered := range registrySlugs {
		if problem.Type == registered {
			return problem
		}
	}
	t.Errorf("type %q is not in the slug registry", problem.Type)

	return problem
}

func TestOpenAPIMatchesCommittedDocument(t *testing.T) {
	server := newTestServer(t, "")

	committed, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.json"))
	if err != nil {
		t.Fatalf("read committed document: %v", err)
	}

	spec, err := server.Spec()
	if err != nil {
		t.Fatalf("Spec: %v", err)
	}
	if string(spec) != string(committed) {
		t.Error("Spec() differs from api/openapi.json; run make gen and commit the result")
	}

	served := do(t, server.Router, http.MethodGet, "/api/v1/openapi.json")
	if served.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/openapi.json = %d, want %d", served.Code, http.StatusOK)
	}
	if served.Body.String() != string(committed) {
		t.Error("served /openapi.json differs from api/openapi.json")
	}
}

func TestBasePathMountsEverything(t *testing.T) {
	for _, basePath := range []string{"", "/dl-tool"} {
		t.Run("base="+basePath, func(t *testing.T) {
			server := newTestServer(t, basePath)

			response := do(t, server.Router, http.MethodGet, basePath+"/api/v1/system/info")
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s/api/v1/system/info = %d, want %d", basePath, response.Code, http.StatusOK)
			}

			var body struct {
				Version string `json:"version"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode system info: %v", err)
			}
			if body.Version != Version {
				t.Errorf("version = %q, want %q", body.Version, Version)
			}
		})
	}
}

func TestOutsideBaseIs404(t *testing.T) {
	server := newTestServer(t, "/dl-tool")

	response := do(t, server.Router, http.MethodGet, "/api/v1/system/info")
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
}

func TestUnknownRouteIsProblemJSON(t *testing.T) {
	server := newTestServer(t, "/dl-tool")

	response := do(t, server.Router, http.MethodGet, "/dl-tool/api/v1/no-such-route")
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
}

// TestHumaErrorsCarryRegistrySlug drives Huma's own validation failure through
// the installed error factory: the response must be problem+json with the
// validation slug and populated errors[].
func TestHumaErrorsCarryRegistrySlug(t *testing.T) {
	server := newTestServer(t, "")

	huma.Register(server.API, huma.Operation{
		OperationID: "validation-probe",
		Method:      http.MethodGet,
		Path:        "/validation-probe",
	}, func(_ context.Context, _ *validationProbeInput) (*struct{}, error) {
		return &struct{}{}, nil
	})

	response := do(t, server.Router, http.MethodGet, "/api/v1/validation-probe")
	problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	if len(problem.Errors) == 0 {
		t.Fatal("errors[] is empty, want field-level validation failures")
	}
	if !strings.Contains(problem.Errors[0].Location, "name") {
		t.Errorf("errors[0].location = %q, want it to name the missing parameter", problem.Errors[0].Location)
	}
}

// TestRealIPHonoursTrustedProxies asserts the X-Forwarded-For rewrite applies
// only when the direct peer is a configured trusted proxy.
func TestRealIPHonoursTrustedProxies(t *testing.T) {
	for _, test := range []struct {
		name      string
		trusted   string
		forwarded string
		want      string
	}{
		{"trusted peer rewrites", "192.0.2.0/24", "203.0.113.7, 192.0.2.1", "203.0.113.7"},
		{"untrusted peer ignored", "10.0.0.0/8", "203.0.113.7", "192.0.2.1:43210"},
		{"no header keeps peer", "192.0.2.0/24", "", "192.0.2.1:43210"},
	} {
		t.Run(test.name, func(t *testing.T) {
			prefix, err := netip.ParsePrefix(test.trusted)
			if err != nil {
				t.Fatalf("parse prefix: %v", err)
			}

			server, err := NewServer(
				&config.Config{TrustedProxies: []netip.Prefix{prefix}},
				nil,
				slog.New(slog.NewJSONHandler(io.Discard, nil)),
			)
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}

			server.Base.Get("/remote-addr-probe", func(w http.ResponseWriter, r *http.Request) {
				if _, err := w.Write([]byte(r.RemoteAddr)); err != nil {
					t.Errorf("write probe: %v", err)
				}
			})

			request := httptest.NewRequest(http.MethodGet, "/remote-addr-probe", nil)
			request.RemoteAddr = "192.0.2.1:43210"
			if test.forwarded != "" {
				request.Header.Set("X-Forwarded-For", test.forwarded)
			}

			recorder := httptest.NewRecorder()
			server.Router.ServeHTTP(recorder, request)

			if recorder.Body.String() != test.want {
				t.Errorf("RemoteAddr = %q, want %q", recorder.Body.String(), test.want)
			}
		})
	}
}

func TestRedactedRequestURI(t *testing.T) {
	for _, test := range []struct {
		name   string
		target string
		want   string
	}{
		{"secret parameters redacted", "/api/v1/feeds?apikey=s3cret&token=t&passkey=p", "/api/v1/feeds?apikey=" + redactedValue + "&passkey=" + redactedValue + "&token=" + redactedValue},
		{"other parameters kept", "/api/v1/tasks?limit=10", "/api/v1/tasks?limit=10"},
		{"no query unchanged", "/api/v1/tasks", "/api/v1/tasks"},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := url.Parse(test.target)
			if err != nil {
				t.Fatalf("parse target: %v", err)
			}
			if got := redactedRequestURI(parsed); got != test.want {
				t.Errorf("redactedRequestURI = %q, want %q", got, test.want)
			}
		})
	}
}

type validationProbeInput struct {
	Name string `query:"name" required:"true"`
}
