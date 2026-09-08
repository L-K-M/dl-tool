//go:build integration

// The aria2 call site of the shared engine contract suite
// (docs/06-download-engines.md §11): one throwaway aria2 daemon per subtest,
// built from deploy/aria2/Dockerfile.
package aria2

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/engine/enginetest"
)

const (
	// rpcPort is aria2's JSON-RPC and WebSocket port, exposed for the mapped
	// endpoint the client connects to.
	rpcPort = "6800/tcp"

	// rpcSecret is the container's --rpc-secret and the client's Config.Secret.
	// A test-local value only; it never leaves the throwaway container.
	rpcSecret = "dl-tool-enginetest"

	// downloadsDir is the daemon's --dir inside the container.
	downloadsDir = "/downloads"

	// dockerfileContext is the deploy/aria2 build context T115 later
	// publishes; the contract suite builds its daemon from it.
	dockerfileContext = "../../../deploy/aria2"

	// containerTimeout bounds container start (build, pull, readiness wait),
	// connect and terminate, so a hung daemon fails fast instead of running
	// out the whole go test deadline.
	containerTimeout = 60 * time.Second
)

// TestAria2Contract runs the shared engine conformance suite against a real
// aria2 daemon built from deploy/aria2/Dockerfile.
func TestAria2Contract(t *testing.T) {
	enginetest.RunContract(t, newAria2)
}

// newAria2 starts one throwaway aria2 container and returns a connected
// client. It is the suite's newEngine: every subtest gets its own daemon.
func newAria2(t *testing.T) engine.Engine {
	t.Helper()

	// The fixture server must exist before the container, so its port can be
	// tunneled in; enginetest.Fixture is per-t, so the suite's own later
	// call serves from this same server.
	fixtureURL, _ := enginetest.Fixture(t)
	fixturePort, err := fixturePort(fixtureURL)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), containerTimeout)
	defer cancel()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context: dockerfileContext,
			},
			// The flags the daemon needs to serve RPC: --rpc-listen-all
			// binds 0.0.0.0 for the mapped port, --rpc-secret gates every
			// call, --dir lands the fixture body in the container, and
			// --allow-overwrite with --auto-file-renaming=false makes a
			// second download of the same URL a fresh full transfer instead
			// of an instant "file exists" completion.
			Cmd: []string{
				"aria2c",
				"--enable-rpc",
				"--rpc-listen-all",
				"--rpc-secret=" + rpcSecret,
				"--dir=" + downloadsDir,
				"--allow-overwrite=true",
				"--auto-file-renaming=false",
			},
			ExposedPorts: []string{rpcPort},
			// Listening on the RPC port is readiness; the client's Connect
			// then proves the daemon answers getVersion.
			WaitingFor: wait.ForListeningPort(rpcPort),
			// Tunnels the fixture port to the container so
			// host.testcontainers.internal resolves and reaches it (task
			// step 9).
			HostAccessPorts: []int{fixturePort},
		},
		Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		terminateCtx, cancel := context.WithTimeout(context.Background(), containerTimeout)
		defer cancel()
		require.NoError(t, container.Terminate(terminateCtx))
	})

	host, err := container.Host(ctx)
	require.NoError(t, err)
	mapped, err := container.MappedPort(ctx, rpcPort)
	require.NoError(t, err)

	client, err := New(Config{
		URL:    "http://" + net.JoinHostPort(host, mapped.Port()) + "/jsonrpc",
		Secret: rpcSecret,
	}, nil)
	require.NoError(t, err)
	require.NoError(t, client.Connect(ctx), "connect to the throwaway aria2 daemon")
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close aria2 client: %v", err)
		}
	})

	return client
}

// fixturePort extracts the numeric port from a fixture URL so it can be
// tunneled into the container.
func fixturePort(fixtureURL string) (int, error) {
	served, err := url.Parse(fixtureURL)
	if err != nil {
		return 0, fmt.Errorf("parse fixture url %q: %w", fixtureURL, err)
	}
	_, port, err := net.SplitHostPort(served.Host)
	if err != nil {
		return 0, fmt.Errorf("split fixture host %q: %w", served.Host, err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		return 0, fmt.Errorf("fixture port %q: %w", port, err)
	}
	return number, nil
}
