package obs_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"

	"github.com/L-K-M/dl-tool/internal/api"
	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/obs"
)

const (
	healthzPath = "/healthz"
	readyzPath  = "/readyz"
	apiV1Prefix = "/api/v1"
	metricsPath = "/metrics"

	// The response bodies and slugs fixed by docs/05-api-contract.md
	// section 13.1; asserted as literals so a constant drifting from the
	// contract fails here.
	jsonContentType    = "application/json"
	problemContentType = "application/problem+json"
	liveBody           = `{"status":"ok"}`
	readyBody          = `{"status":"ready"}`
	notReadyType       = "/problems/not-ready"

	// The metrics address semantics of docs/11-config-reference.md
	// section 2: the exact value "off" disables metrics, an empty address
	// falls back to the default.
	metricsOffValue = "off"
	defaultAddr     = "127.0.0.1:9090"

	// listenerDeadline bounds how long a test waits for the metrics
	// listener to come up or shut down.
	listenerDeadline = 2 * time.Second
	pollInterval     = 10 * time.Millisecond
)

// metricNames is the fixed metric set of the T010 interface contract; every
// name must be reachable through the metrics listener.
var metricNames = []string{
	"dltool_tasks_total",
	"dltool_task_transitions_total",
	"dltool_bytes_transferred_total",
	"dltool_engine_poll_duration_seconds",
	"dltool_engine_errors_total",
	"dltool_jobs_total",
	"dltool_sse_clients",
	"process_start_time_seconds",
}

func TestHealthzDoesNotTouchTheDatabase(t *testing.T) {
	// A nil handle proves Live performs no dependency lookup at all.
	health := obs.NewHealth(nil)

	recorder := httptest.NewRecorder()
	health.Live(recorder, httptest.NewRequest(http.MethodGet, healthzPath, nil))

	response := recorder.Result()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if got := response.Header.Get("Content-Type"); got != jsonContentType {
		t.Errorf("content type = %q, want %q", got, jsonContentType)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != liveBody {
		t.Errorf("body = %q, want %q", body, liveBody)
	}
}

func TestReadyzIs503BeforeMigration(t *testing.T) {
	health := obs.NewHealth(nil)

	recorder := httptest.NewRecorder()
	health.Ready(recorder, httptest.NewRequest(http.MethodGet, readyzPath, nil))

	response := recorder.Result()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	if got := response.Header.Get("Content-Type"); got != problemContentType {
		t.Errorf("content type = %q, want %q", got, problemContentType)
	}

	var problem problemDocument
	if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Type != notReadyType {
		t.Errorf("type = %q, want %q", problem.Type, notReadyType)
	}
	if problem.Status != http.StatusServiceUnavailable {
		t.Errorf("problem status = %d, want %d", problem.Status, http.StatusServiceUnavailable)
	}
}

func TestReadyzIs200AfterMigration(t *testing.T) {
	health := obs.NewHealth(newMemoryDB(t))
	health.MarkReady()

	recorder := httptest.NewRecorder()
	health.Ready(recorder, httptest.NewRequest(http.MethodGet, readyzPath, nil))

	response := recorder.Result()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != readyBody {
		t.Errorf("body = %q, want %q", body, readyBody)
	}
}

func TestReadyzIs503WhenDatabaseFails(t *testing.T) {
	db := newMemoryDB(t)
	health := obs.NewHealth(db)
	health.MarkReady()

	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	recorder := httptest.NewRecorder()
	health.Ready(recorder, httptest.NewRequest(http.MethodGet, readyzPath, nil))

	response := recorder.Result()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}

	var problem problemDocument
	if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Type != notReadyType {
		t.Errorf("type = %q, want %q", problem.Type, notReadyType)
	}
}

// TestHealthEndpointsRequireNoCredential exercises the mounted routes: the
// two process endpoints sit outside /api/v1, answer without any credential,
// and a store that never opened never reports ready.
func TestHealthEndpointsRequireNoCredential(t *testing.T) {
	server := newMainRouter(t)

	healthz := doMainRouterRequest(t, server, healthzPath)
	if healthz.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want %d", healthz.StatusCode, http.StatusOK)
	}

	readyz := doMainRouterRequest(t, server, readyzPath)
	if readyz.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz status = %d, want %d", readyz.StatusCode, http.StatusServiceUnavailable)
	}

	// The process endpoints live on the base router only.
	inside := doMainRouterRequest(t, server, apiV1Prefix+healthzPath)
	if inside.StatusCode != http.StatusNotFound {
		t.Errorf("GET /api/v1/healthz status = %d, want %d", inside.StatusCode, http.StatusNotFound)
	}
}

func TestMetricsNotOnMainListener(t *testing.T) {
	server := newMainRouter(t)

	response := doMainRouterRequest(t, server, metricsPath)
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("GET /metrics status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}
}

func TestMetricsListenerExposesMetricSet(t *testing.T) {
	metrics := obs.NewMetrics()
	// Prometheus omits a registered collector whose label space is empty, so
	// every series gets one observation here; only registration can put the
	// names on the endpoint.
	metrics.TasksTotal.WithLabelValues("queued", "aria2").Set(1)
	metrics.TaskTransitionsTotal.WithLabelValues("queued", "downloading", "aria2").Inc()
	metrics.BytesTransferredTotal.WithLabelValues("down").Add(1)
	metrics.EnginePollDuration.WithLabelValues("aria2").Observe(0.1)
	metrics.EngineErrorsTotal.WithLabelValues("aria2", "rpc").Inc()
	metrics.JobsTotal.WithLabelValues("search", "done").Inc()
	metrics.SSEClients.Set(1)

	body := serveMetricsUntilCancelled(t, metrics, freeLoopbackAddr(t))

	for _, name := range metricNames {
		if !strings.Contains(body, name) {
			t.Errorf("metrics body is missing %q", name)
		}
	}
}

// TestEmptyMetricsAddrDisablesMetrics pins the disable value of doc 11
// section 2: the exact string "off" — never an empty address, which means
// unset and falls back to the default (TestEmptyMetricsAddrFallsBackToDefault)
// — starts no listener and returns at once.
func TestEmptyMetricsAddrDisablesMetrics(t *testing.T) {
	metrics := obs.NewMetrics()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- metrics.ListenAndServe(ctx, metricsOffValue)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe(off) = %v, want nil", err)
		}
	case <-time.After(listenerDeadline):
		t.Fatal("ListenAndServe(off) did not return; it started serving")
	}
}

func TestEmptyMetricsAddrFallsBackToDefault(t *testing.T) {
	metrics := obs.NewMetrics()
	// Marks the answer as this registry's, not any other process that
	// happens to hold the default port.
	metrics.SSEClients.Set(1)

	body := serveMetricsUntilCancelled(t, metrics, "")

	if !strings.Contains(body, "dltool_sse_clients") {
		t.Errorf("default-address body = %q, want a dltool series", body)
	}
}

// problemDocument is the decoded application/problem+json of GET /readyz.
type problemDocument struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// serveMetricsUntilCancelled starts ListenAndServe on addr, waits for it to
// answer GET /metrics, cancels it and asserts a clean return. An empty addr
// exercises the default-address fallback inside ListenAndServe.
func serveMetricsUntilCancelled(t *testing.T, metrics *obs.Metrics, addr string) string {
	t.Helper()

	listenAddr := addr
	if listenAddr == "" {
		listenAddr = defaultAddr
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- metrics.ListenAndServe(ctx, addr)
	}()

	body := waitForMetricsBody(t, "http://"+listenAddr+metricsPath)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe returned %v, want nil", err)
		}
	case <-time.After(listenerDeadline):
		t.Fatal("ListenAndServe did not return after cancellation")
	}

	return body
}

// waitForMetricsBody polls until the listener answers, so the test does not
// race the goroutine that is still binding.
func waitForMetricsBody(t *testing.T, url string) string {
	t.Helper()

	client := &http.Client{Timeout: listenerDeadline}
	deadline := time.Now().Add(listenerDeadline)
	for {
		response, err := client.Get(url)
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("GET %s status = %d, want %d", url, response.StatusCode, http.StatusOK)
			}
			if readErr != nil {
				t.Fatalf("read metrics body: %v", readErr)
			}
			if closeErr != nil {
				t.Fatalf("close metrics body: %v", closeErr)
			}

			return string(body)
		}

		if time.Now().After(deadline) {
			t.Fatalf("metrics listener at %s never answered: %v", url, err)
		}
		time.Sleep(pollInterval)
	}
}

// freeLoopbackAddr reserves an ephemeral loopback port and releases it for
// ListenAndServe to bind.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback listener: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release reserved listener: %v", err)
	}

	return address
}

// newMainRouter builds the real chi router with no database and no
// credentials available to the request.
func newMainRouter(t *testing.T) http.Handler {
	t.Helper()

	server, err := api.NewServer(&config.Config{}, nil, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}

	return server.Router
}

func doMainRouterRequest(t *testing.T, handler http.Handler, target string) *http.Response {
	t.Helper()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))

	response := recorder.Result()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("drain response body: %v", err)
	}

	return response
}

// newMemoryDB opens a private in-memory SQLite database for the readiness
// probe; it needs no migrations to answer SELECT 1.
func newMemoryDB(t *testing.T) *sqlx.DB {
	t.Helper()

	db, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open memory database: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() {
		// database/sql Close is idempotent, so a test that closed the handle
		// itself (TestReadyzIs503WhenDatabaseFails) closes it again for nil.
		if err := db.Close(); err != nil {
			t.Fatalf("close memory database: %v", err)
		}
	})

	return db
}
