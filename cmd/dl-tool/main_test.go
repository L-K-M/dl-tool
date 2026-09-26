package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

// blockingListener parks its Close — the call http.Server.Shutdown makes
// inside closeListenersLocked — until released, so a test can hold the
// ingress-stop boundary open and observe what the drain does meanwhile.
type blockingListener struct {
	net.Listener
	entered   chan struct{}
	release   chan struct{}
	completed chan struct{}
}

func (l *blockingListener) Close() error {
	close(l.entered)
	<-l.release
	err := l.Listener.Close()
	close(l.completed)
	return err
}

// Doc 17 §2 orders ingress ahead of the runtime drain: the runtime step
// must not start until the listener close has completed inside Shutdown —
// after it, /readyz already reports the draining 503 and one Dial is
// refused immediately, with no retry window.
func TestShutdownDrainStopsIngressBeforeRuntimeDrain(t *testing.T) {
	db := testDB(t)
	server := testServer(t)

	base, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	bl := &blockingListener{
		Listener:  base,
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		completed: make(chan struct{}),
	}
	// Any abort must release the held Close before the server cleanup below
	// can call it — defer, not t.Cleanup: defers run before cleanups, and
	// Server.Close would otherwise block on the held listener forever.
	releaseOnce := sync.OnceFunc(func() { close(bl.release) })
	defer releaseOnce()

	httpServer := &http.Server{Handler: server.Router}
	t.Cleanup(func() {
		if cerr := httpServer.Close(); cerr != nil {
			t.Logf("test http server close: %v", cerr)
		}
	})
	go func() {
		if serr := httpServer.Serve(bl); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			t.Errorf("http serve: %v", serr)
		}
	}()
	addr := base.Addr().String()

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	require.NoError(t, err, "the listener must be live before the drain")
	require.NoError(t, conn.Close())

	// Baseline: readiness must not already report draining before the drain
	// runs — otherwise the mid-drain 503 would prove nothing.
	baseline := httptest.NewRecorder()
	server.Health.Ready(baseline, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	require.NotContains(t, baseline.Body.String(), "shutting down",
		"readiness must not already report a drain before one begins")

	runtimeRan := make(chan struct{})
	drainRuntime := func() {
		defer close(runtimeRan)

		// The listener close must already have completed — not merely be
		// in progress somewhere on the Shutdown goroutine. t.Errorf, not
		// require: this runs on the drain goroutine, where FailNow would
		// silently abort shutdownDrain instead of reporting.
		select {
		case <-bl.completed:
		default:
			t.Error("the runtime drain started before the listener close completed")
		}

		// The draining 503 carries its own detail — a nil-db or
		// pre-migration 503 does not count as a withdrawn readiness.
		recorder := httptest.NewRecorder()
		server.Health.Ready(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if recorder.Code != http.StatusServiceUnavailable {
			t.Errorf("mid-drain readyz = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
		}
		if !strings.Contains(recorder.Body.String(), "shutting down") {
			t.Errorf("mid-drain readyz detail = %q, want the draining detail", recorder.Body.String())
		}

		// With the listener close complete, one Dial is refused outright —
		// no retry window.
		c, dialErr := net.DialTimeout("tcp", addr, time.Second)
		if dialErr == nil {
			_ = c.Close()
			t.Error("the listener still accepts connections when the runtime drain starts")
		}
	}

	drainDone := make(chan struct{})
	go func() {
		shutdownDrain(httpServer, server, db, nil, drainRuntime)
		close(drainDone)
	}()

	select {
	case <-bl.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain never reached the listener close")
	}

	// While the Close is held, the runtime drain must not have run.
	select {
	case <-runtimeRan:
		t.Error("the runtime drain ran while the listener close was still held")
	case <-time.After(200 * time.Millisecond):
	}

	releaseOnce()

	select {
	case <-drainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain never finished after the listener close released")
	}
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

// A handler that ignores its request context survives the grace budget and
// the force-close. Closing engines or the store under a live request is
// the race the conn join exists to prevent, so on an exhausted join budget
// the drain returns — bounded, before server.Shutdown, engines and db —
// and leaves them to process exit. Nothing claims a completed safe close.
func TestShutdownDrainWedgedHandlerLeavesResourcesForProcessExit(t *testing.T) {
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

	engine := &closeProbeEngine{name: "probe"}
	server.Engines.Register(engine)

	start := time.Now()
	shutdownDrain(httpServer, server, db, &liveConns, func() {})
	require.Less(t, time.Since(start), 5*time.Second, "a wedged handler must not hang the drain")

	// The drain declined unsafe teardown while the handler is still held:
	// no engine Close ran and the store still answers.
	require.Equal(t, int32(0), engine.calls.Load(),
		"engines must not close while a live handler could still reach them")
	require.NoError(t, db.PingContext(context.Background()),
		"the store must remain open when the drain bails on a live handler")
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
