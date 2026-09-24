//go:build integration

// The yt-dlp call site of the shared engine contract suite
// (docs/06-download-engines.md section 11). The engine has no daemon to
// containerise — it supervises a local subprocess — so the suite runs
// only when DLTOOL_YTDLP_PATH names an executable, and skips with a
// named reason otherwise.
package ytdlp

import (
	"context"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/engine/enginetest"
)

func TestYtdlpContract(t *testing.T) {
	binary := os.Getenv("DLTOOL_YTDLP_PATH")
	if binary == "" {
		t.Skip("DLTOOL_YTDLP_PATH is not set; no yt-dlp binary to exercise")
	}
	if _, err := exec.LookPath(binary); err != nil {
		// Set-but-broken is a CI misconfiguration, not an absent binary:
		// skipping here would turn a moved executable into a silent
		// no-coverage green.
		t.Fatalf("DLTOOL_YTDLP_PATH %q is set but not executable: %v", binary, err)
	}
	enginetest.RunContract(t, func(t *testing.T) engine.Engine {
		return newContractEngine(t, binary)
	})
}

// contractEngine lifts the adapter onto the two suite surfaces a
// subprocess engine needs: a fixture-URL rewrite in Add, because the
// suite's host.testcontainers.internal resolves inside daemon
// containers while this engine's process runs on the test host, and
// DownloadLimitReadback, which reports the limit the next spawn carries
// — the honest equivalent of a daemon's configured limit for an engine
// whose throttle lands in argv (T113 adds the confirmed flag).
type contractEngine struct {
	*Engine
	t *testing.T
}

var _ enginetest.DownloadLimitReadback = contractEngine{}

func newContractEngine(t *testing.T, binary string) contractEngine {
	t.Helper()
	jsRuntime := os.Getenv("DLTOOL_JS_RUNTIME_PATH")
	if jsRuntime == "" {
		jsRuntime = "node"
	}
	e := NewEngine(Config{
		BinaryPath:    binary,
		JSRuntimePath: jsRuntime,
		ArchiveDir:    filepath.Join(t.TempDir(), "archives"),
	}, testLogger())
	require.NoError(t, e.Connect(context.Background()))
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	return contractEngine{Engine: e, t: t}
}

// Add rewrites the fixture URL's container-facing host to loopback: the
// fixture listener binds 0.0.0.0, so the same port answers on
// 127.0.0.1 for a process running on the host. An empty SaveDir is
// redirected to a fresh tempdir — otherwise the subprocess would write
// the media file and the info document into the package directory, and
// a per-task dir also keeps the shared .dl-tool-info.json filename from
// colliding across suite tasks.
func (c contractEngine) Add(ctx context.Context, req engine.AddRequest) (string, error) {
	if req.SaveDir == "" {
		req.SaveDir = c.t.TempDir()
	}
	for i, raw := range req.URIs {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if u.Hostname() == testcontainers.HostInternal && u.Port() != "" {
			u.Host = net.JoinHostPort("127.0.0.1", u.Port())
			req.URIs[i] = u.String()
		}
	}
	return c.Engine.Add(ctx, req)
}

// DaemonDownloadLimit is the suite's readback: there is no daemon to
// query, so this reports the exact value stored for the next spawn —
// global for id "", the per-task effective minimum otherwise.
func (c contractEngine) DaemonDownloadLimit(_ context.Context, id string) (int64, error) {
	return c.Engine.pendingDownloadLimit(id)
}
