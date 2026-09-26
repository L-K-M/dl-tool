package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/api"
	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/store"
)

// closeProbeEngine is an Engine whose only live members are Name and Close:
// the embedded nil interface panics on anything else, so a drain that
// touches a non-lifecycle method fails loudly.
type closeProbeEngine struct {
	engine.Engine
	name    string
	onClose func() error
	calls   atomic.Int32
}

func (e *closeProbeEngine) Name() string { return e.name }

func (e *closeProbeEngine) Close() error {
	e.calls.Add(1)
	if e.onClose != nil {
		return e.onClose()
	}

	return nil
}

// testServer builds the document-only server: routes register, but with a
// nil store no background loops start, so Shutdown drains nothing.
func testServer(t *testing.T) *api.Server {
	t.Helper()

	server, err := api.NewServer(&config.Config{}, nil, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	require.NoError(t, err)

	return server
}

func testDB(t *testing.T) *sqlx.DB {
	t.Helper()

	dir := t.TempDir()
	db, err := store.Open(
		t.Context(),
		filepath.Join(dir, "dl-tool.db"),
		filepath.Join(dir, "backups"),
	)
	require.NoError(t, err)
	// Belt and braces for aborts before the drain: DB.Close is idempotent,
	// so a second close after the drain's own is a nil no-op.
	t.Cleanup(func() {
		if cerr := db.Close(); cerr != nil {
			t.Logf("test db close: %v", cerr)
		}
	})

	return db
}

// Every engine registered on the composition root's registry must receive
// Close during the drain, while the store is still open — doc 17 §2 orders
// engine teardown between the loop drain and the database close.
func TestShutdownDrainClosesEnginesBeforeDatabaseClose(t *testing.T) {
	db := testDB(t)
	server := testServer(t)

	var pingErr error
	live := &closeProbeEngine{
		name: "live",
		onClose: func() error {
			// The store must still answer inside Close: adapters release
			// resources and close subscriber channels here, and a write-back
			// path like qBittorrent's InfohashWriter must never meet a
			// closed pool.
			pingErr = db.PingContext(context.Background())

			return nil
		},
	}
	second := &closeProbeEngine{name: "second"}
	server.Engines.Register(live)
	server.Engines.Register(second)

	shutdownDrain(nil, server, db, nil, func() {})

	require.Equal(t, int32(1), live.calls.Load(), "Close must run exactly once in the drain")
	require.Equal(t, int32(1), second.calls.Load(), "every registered engine must close")
	require.NoError(t, pingErr, "the store must still be open while engines close")
	require.Error(t, db.PingContext(context.Background()), "the store must be closed when the drain returns")
}

// An engine that fails Close must not abort the drain: the rest of the
// teardown — the database close — still runs.
func TestShutdownDrainClosesDatabaseWhenEngineCloseFails(t *testing.T) {
	db := testDB(t)
	server := testServer(t)

	broken := &closeProbeEngine{name: "broken", onClose: func() error { return engine.ErrUnavailable }}
	healthy := &closeProbeEngine{name: "healthy"}
	server.Engines.Register(broken)
	server.Engines.Register(healthy)

	shutdownDrain(nil, server, db, nil, func() {})

	require.Equal(t, int32(1), broken.calls.Load())
	require.Equal(t, int32(1), healthy.calls.Load(), "one engine's failure must not skip the rest")
	require.Error(t, db.PingContext(context.Background()), "a failed engine close must not strand the store")
}

// Doc 17 §2 orders ingress ahead of the runtime drain: by the time the
// scheduler and workers begin draining, /readyz must already report the
// draining 503 — not merely a failed probe — and the listener must already
// refuse new connections. The injected step probes both mid-drain.
func TestShutdownDrainStopsIngressBeforeRuntimeDrain(t *testing.T) {
	db := testDB(t)
	server := testServer(t)

	httpServer, addr := testHTTPServer(t, server.Router, nil)
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err, "the listener must be live before the drain")
	require.NoError(t, conn.Close())

	drainRuntime := func() {
		// The draining 503 carries its own detail — a nil-db or pre-migration
		// 503 does not count as a withdrawn readiness.
		recorder := httptest.NewRecorder()
		server.Health.Ready(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
		require.Contains(t, recorder.Body.String(), "shutting down",
			"readiness must be withdrawn before the runtime drain runs")

		// New connections must already be refused: Shutdown closes the
		// listener first, so refusal is near-instant once it starts.
		deadline := time.Now().Add(5 * time.Second)
		for {
			c, dialErr := net.DialTimeout("tcp", addr, time.Second)
			if dialErr != nil {
				return
			}
			_ = c.Close()
			if time.Now().After(deadline) {
				t.Error("the listener still accepts connections during the runtime drain")

				return
			}
			time.Sleep(time.Millisecond)
		}
	}

	shutdownDrain(httpServer, server, db, nil, drainRuntime)
	require.Error(t, db.PingContext(context.Background()), "the store must be closed when the drain returns")
}

// A handler still in flight when Shutdown's budget expires must be dead
// before engines close — the SSE pattern, a handler parked on its request
// context, is what holds Shutdown to the deadline. The drain escalates to
// httpServer.Close, which cancels the request context, and then joins the
// conn goroutines through liveConns, so engine teardown observes the
// handler already gone.
func TestShutdownDrainForceClosesOverrunningHandler(t *testing.T) {
	db := testDB(t)
	server := testServer(t)

	entered := make(chan struct{})
	exited := make(chan struct{})
	server.Router.Get("/stuck", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(exited)
	})

	var liveConns sync.WaitGroup
	httpServer, addr := testHTTPServer(t, server.Router, &liveConns)

	go func() {
		resp, err := http.Get("http://" + addr + "/stuck")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the parked handler never ran")
	}

	// Shrink the budget so Shutdown overruns on the live handler.
	savedTimeout := shutdownTimeout
	shutdownTimeout = 50 * time.Millisecond
	defer func() { shutdownTimeout = savedTimeout }()

	engine := &closeProbeEngine{
		name: "probe",
		onClose: func() error {
			// The drain's conn join is what orders this: exited must
			// already be closed, not merely on its way — without the
			// Close escalation the handler is still parked here.
			select {
			case <-exited:
			default:
				t.Error("an overrunning handler was still live when engines closed")
			}

			return nil
		},
	}
	server.Engines.Register(engine)

	shutdownDrain(httpServer, server, db, &liveConns, func() {})

	require.Equal(t, int32(1), engine.calls.Load())
	require.Error(t, db.PingContext(context.Background()), "the store must be closed when the drain returns")
}

// A handler that ignores its request context survives both the graceful
// budget and the force-close; the drain joins it for one more
// shutdownTimeout and then proceeds rather than hanging the process.
func TestShutdownDrainBoundedJoinOfWedgedHandler(t *testing.T) {
	db := testDB(t)
	server := testServer(t)

	entered := make(chan struct{})
	unblock := make(chan struct{})
	server.Router.Get("/wedged", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-unblock
	})
	// The wedged handler outlives the test if it is never released.
	t.Cleanup(func() { close(unblock) })

	var liveConns sync.WaitGroup
	httpServer, addr := testHTTPServer(t, server.Router, &liveConns)

	go func() {
		resp, err := http.Get("http://" + addr + "/wedged")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the wedged handler never ran")
	}

	savedTimeout := shutdownTimeout
	shutdownTimeout = 50 * time.Millisecond
	defer func() { shutdownTimeout = savedTimeout }()

	// Two budgets — Shutdown's own plus the conn join — must still finish
	// well inside the failure threshold.
	start := time.Now()
	shutdownDrain(httpServer, server, db, &liveConns, func() {})
	require.Less(t, time.Since(start), 5*time.Second, "a wedged handler must not hang the drain")
	require.Error(t, db.PingContext(context.Background()), "the store must be closed when the drain returns")
}

// testHTTPServer serves h on a loopback listener and closes both in cleanup.
// The drain owns the ordinary shutdown; cleanup only sweeps the listener and
// conn goroutines if a test aborts before the drain ran — Server.Close is
// idempotent, so the double-close after a real drain is a nil no-op.
func testHTTPServer(t *testing.T, h http.Handler, liveConns *sync.WaitGroup) (*http.Server, string) {
	t.Helper()

	httpServer := &http.Server{Handler: h}
	if liveConns != nil {
		httpServer.ConnState = func(_ net.Conn, s http.ConnState) {
			switch s {
			case http.StateNew:
				liveConns.Add(1)
			case http.StateClosed, http.StateHijacked:
				liveConns.Done()
			}
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() {
		if cerr := httpServer.Close(); cerr != nil {
			t.Logf("test http server close: %v", cerr)
		}
	})
	go func() { _ = httpServer.Serve(listener) }()

	return httpServer, listener.Addr().String()
}

// The HTTP listener is released before the engines: an in-flight request
// never meets a closed engine mid-handler.
func TestShutdownDrainStopsHTTPBeforeEnginesClose(t *testing.T) {
	server := testServer(t)
	db := testDB(t)

	httpServer, addr := testHTTPServer(t, server.Router, nil)
	base := "http://" + addr
	probe := &http.Client{Timeout: 2 * time.Second}
	resp, err := probe.Get(base + "/healthz")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode, "the listener must be live before the drain")

	var midCloseErr error
	server.Engines.Register(&closeProbeEngine{
		name: "probe",
		onClose: func() error {
			// When engines close, the listener must already be gone.
			midResp, midErr := probe.Get(base + "/healthz")
			if midErr == nil {
				_ = midResp.Body.Close()
			}
			midCloseErr = midErr

			return nil
		},
	})

	shutdownDrain(httpServer, server, db, nil, func() {})

	require.Error(t, midCloseErr, "the HTTP listener must be dead before engines close")
}
