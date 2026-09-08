//go:build integration

// The aria2 call site of the shared engine contract suite
// (docs/06-download-engines.md §11): one throwaway aria2 daemon per subtest,
// built from deploy/aria2/Dockerfile.
package aria2

import (
	"context"
	"encoding/json"
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

	// methodGetOption and methodGetGlobalOption are the read-only option
	// surfaces DownloadLimitReadback queries; the write-side change*
	// methods live in client.go.
	methodGetOption       = "aria2.getOption"
	methodGetGlobalOption = "aria2.getGlobalOption"

	// containerTimeout bounds container start (build, pull, readiness wait),
	// connect and terminate, so a hung daemon fails fast instead of running
	// out the whole go test deadline.
	containerTimeout = 60 * time.Second
)

// TestAria2Contract runs the shared engine conformance suite against a real
// aria2 daemon built from deploy/aria2/Dockerfile.
func TestAria2Contract(t *testing.T) {
	enginetest.RunContract(t, func(t *testing.T) engine.Engine { return newAria2(t) })
}

// requestedLimit is the cap the readback test round-trips: the suite's
// own rateLimitBytesPerSecond, which the exact-setting obligation hangs on.
const requestedLimit int64 = 1 << 20

// TestAria2DaemonLimitReadback pins the readback the contract suite relies
// on to daemon truth. A limit set through the adapter must read back
// exactly; overwriting the daemon's option directly — bypassing the
// adapter entirely — must read back as the injected value, so an adapter
// that echoes its last request instead of querying the daemon cannot
// satisfy the suite's equality check by accident.
func TestAria2DaemonLimitReadback(t *testing.T) {
	e := newAria2(t)
	ctx, cancel := context.WithTimeout(context.Background(), containerTimeout)
	defer cancel()

	fixtureURL, _ := enginetest.Fixture(t)
	id, err := e.Add(ctx, engine.AddRequest{URIs: []string{fixtureURL}, StartPaused: true})
	require.NoError(t, err)

	// Per-task: adapter round trip, then a wrong value injected straight
	// into the daemon at three quarters of the request.
	requested := requestedLimit
	require.NoError(t, e.SetRateLimits(ctx, id, &requested, nil))
	report(t, ctx, e, id, requestedLimit)
	inject(t, ctx, e, methodChangeOption, []any{ref(id), map[string]string{
		optMaxDownloadLimit: strconv.FormatInt(wrongThreeQuarters, 10),
	}})
	report(t, ctx, e, id, wrongThreeQuarters)

	// Global: the same two steps through the daemon's global options.
	require.NoError(t, e.SetRateLimits(ctx, "", &requested, nil))
	report(t, ctx, e, "", requestedLimit)
	inject(t, ctx, e, methodChangeGlobalOption, []any{map[string]string{
		optMaxOverallDownloadRate: strconv.FormatInt(wrongThreeQuarters, 10),
	}})
	report(t, ctx, e, "", wrongThreeQuarters)
}

// wrongThreeQuarters is the wrong daemon value this test injects: three
// quarters of the request, small enough to stay inside the suite's timing
// window — the exact setting only the readback can expose.
const wrongThreeQuarters = 3 * (1 << 20) / 4

// report asserts the daemon reports exactly want for id's limit.
func report(t *testing.T, ctx context.Context, e readbackClient, id string, want int64) {
	t.Helper()

	reported, err := e.DaemonDownloadLimit(ctx, id)
	require.NoError(t, err, "read the daemon's limit for %q back", id)
	require.Equal(t, want, reported,
		"the daemon must report %d B/s for %q, not %d — the readback must query the daemon, not echo a request",
		want, id, reported)
}

// inject overwrites a daemon option directly, bypassing the adapter.
func inject(t *testing.T, ctx context.Context, e readbackClient, method string, params []any) {
	t.Helper()
	_, err := e.call(ctx, method, params...)
	require.NoError(t, err, "inject %s straight into the daemon", method)
}

// readbackClient adds the suite's DownloadLimitReadback to the aria2
// client. It rides the client's transport but issues its own
// getOption/getGlobalOption calls and decodes the answer itself, so the
// suite learns what the daemon is configured with — never what the
// adapter was merely asked to set.
type readbackClient struct {
	*Client
}

// DaemonDownloadLimit reads the daemon's configured download limit: one
// task's max-download-limit through aria2.getOption, or the global
// max-overall-download-limit through aria2.getGlobalOption. id follows
// SetRateLimits: "" is the global limit, otherwise the engine-namespaced
// task id. aria2 answers both keys as plain digits in bytes/second.
func (c readbackClient) DaemonDownloadLimit(ctx context.Context, id string) (int64, error) {
	method, key := methodGetGlobalOption, optMaxOverallDownloadRate
	var params []any
	if id != "" {
		method, key = methodGetOption, optMaxDownloadLimit
		params = []any{ref(id)}
	}

	raw, err := c.call(ctx, method, params...)
	if err != nil {
		return 0, err
	}

	options := map[string]string{}
	if err := json.Unmarshal(raw, &options); err != nil {
		return 0, fmt.Errorf("aria2: decode %s reply: %w", method, err)
	}
	value, err := strconv.ParseInt(options[key], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("aria2: %s replied %q, not a plain B/s count: %w", key, options[key], err)
	}
	return value, nil
}

// newAria2 starts one throwaway aria2 container and returns a connected
// client wrapped in the suite's DownloadLimitReadback. It is the suite's
// newEngine: every subtest gets its own daemon.
func newAria2(t *testing.T) readbackClient {
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

	return readbackClient{Client: client}
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
