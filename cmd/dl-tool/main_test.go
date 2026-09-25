package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
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

	shutdownDrain(nil, server, db)

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

	shutdownDrain(nil, server, db)

	require.Equal(t, int32(1), broken.calls.Load())
	require.Equal(t, int32(1), healthy.calls.Load(), "one engine's failure must not skip the rest")
	require.Error(t, db.PingContext(context.Background()), "a failed engine close must not strand the store")
}

// The HTTP listener is released before the engines: an in-flight request
// never meets a closed engine mid-handler.
func TestShutdownDrainStopsHTTPBeforeEnginesClose(t *testing.T) {
	server := testServer(t)
	db := testDB(t)

	httpServer := &http.Server{Handler: server.Router}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = httpServer.Serve(listener) }()

	base := "http://" + listener.Addr().String()
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

	shutdownDrain(httpServer, server, db)

	require.Error(t, midCloseErr, "the HTTP listener must be dead before engines close")
}
