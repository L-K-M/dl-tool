package aria2

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/stretchr/testify/require"
)

func runConform(t *testing.T, c *Client, total int) ([]engine.ConformanceCheck, error) {
	t.Helper()
	probe, ok := any(c).(interface {
		Conform(context.Context, int) ([]engine.ConformanceCheck, error)
	})
	require.True(t, ok, "adapter must implement Conform")
	return probe.Conform(t.Context(), total)
}

func TestConformRaisesAria2Concurrency(t *testing.T) {
	for _, current := range []string{"2", "5", "9"} {
		t.Run(current, func(t *testing.T) {
			root := t.TempDir()
			_, fake := newTestClient(t, okResponder(map[string]any{"aria2.getGlobalOption": map[string]string{
				"max-concurrent-downloads": current, "dir": root, "save-session": filepath.Join(root, "session"),
			}}))
			roots := []string{root}
			c, err := New(Config{URL: fake.srv.URL, Secret: testSecret, DataRoots: roots}, fake.srv.Client())
			require.NoError(t, err)
			roots[0] = filepath.Join(root, "changed")
			checks, err := runConform(t, c, 5)
			require.NoError(t, err)
			require.Len(t, checks, 3)
			require.Equal(t, "ok", checks[1].Severity, "New must snapshot configured roots")
			require.Equal(t, "ok", checks[2].Severity)
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if current != "2" {
				require.Len(t, fake.calls, 1)
				require.Equal(t, "ok", checks[0].Severity)
				return
			}
			require.Len(t, fake.calls, 2)
			require.Equal(t, "aria2.changeGlobalOption", fake.calls[1].Method)
			require.Equal(t, map[string]any{"max-concurrent-downloads": "5"}, fake.calls[1].Params[1])
			require.Equal(t, engine.ConformanceCheck{Key: "max-concurrent-downloads", Want: "5", Got: "2", Forced: true, Severity: "forced"}, checks[0])
		})
	}
}

func TestConformConfiguredRoots(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		dir  string
		warn bool
	}{
		{root, false}, {filepath.Join(root, "child"), false}, {root + "-sibling", true}, {"relative", true},
	} {
		t.Run(tc.dir, func(t *testing.T) {
			_, fake := newTestClient(t, okResponder(map[string]any{"aria2.getGlobalOption": map[string]string{
				"max-concurrent-downloads": "5", "dir": tc.dir, "save-session": "session",
			}}))
			c, err := New(Config{URL: fake.srv.URL, Secret: testSecret, DataRoots: []string{root}}, fake.srv.Client())
			require.NoError(t, err)
			checks, err := runConform(t, c, 5)
			require.NoError(t, err)
			require.Equal(t, tc.warn, checks[1].Warn)
			fake.mu.Lock()
			defer fake.mu.Unlock()
			require.Len(t, fake.calls, 1)
		})
	}
}

func TestConformAria2Warnings(t *testing.T) {
	for _, tc := range []string{"session", "read", "write", "malformed", "unreachable"} {
		t.Run(tc, func(t *testing.T) {
			c, fake := newTestClient(t, func(method string) (any, *rpcFault) {
				if tc == "read" || (tc == "write" && method == "aria2.changeGlobalOption") {
					return nil, &rpcFault{Code: 1, Message: "denied"}
				}
				if method == "aria2.changeGlobalOption" {
					return "OK", nil
				}
				if tc == "malformed" {
					return "not options", nil
				}
				return map[string]string{"max-concurrent-downloads": "1", "dir": t.TempDir()}, nil
			})
			if tc == "unreachable" {
				fake.srv.Close()
			}
			checks, err := runConform(t, c, 5)
			if tc == "unreachable" {
				require.ErrorIs(t, err, engine.ErrUnavailable)
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, checks)
			if tc == "session" {
				require.Equal(t, "warn", checks[2].Severity)
				return
			}
			require.Equal(t, "warn", checks[0].Severity)
			require.False(t, checks[0].Forced)
		})
	}
}
