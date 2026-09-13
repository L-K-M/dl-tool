package qbittorrent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type conformanceFixture struct {
	mu                      sync.Mutex
	prefs                   map[string]any
	plugins                 []any
	writes                  []map[string]any
	paths                   []string
	readStatus, writeStatus int
}

func cleanPreferences() map[string]any {
	return map[string]any{"rss_processing_enabled": false, "scheduler_enabled": false, "auto_tmm_enabled": false,
		"queueing_enabled": true, "max_active_downloads": 5, "max_active_uploads": 5, "max_active_torrents": 5, "max_active_checking_torrents": 5,
		"unrelated": "preserve me"}
}

func conformClient(t *testing.T, fixture *conformanceFixture) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		fixture.paths = append(fixture.paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/v2/app/preferences":
			assert.Equal(t, http.MethodGet, r.Method)
			if fixture.readStatus != 0 {
				w.WriteHeader(fixture.readStatus)
				return
			}
			assert.NoError(t, json.NewEncoder(w).Encode(fixture.prefs))
		case "/api/v2/app/setPreferences":
			assert.Equal(t, http.MethodPost, r.Method)
			if !assert.NoError(t, r.ParseForm()) {
				return
			}
			assert.Len(t, r.PostForm, 1)
			assert.Len(t, r.PostForm["json"], 1)
			var changed map[string]any
			if !assert.NoError(t, json.Unmarshal([]byte(r.PostForm.Get("json")), &changed)) {
				return
			}
			fixture.writes = append(fixture.writes, changed)
			if fixture.writeStatus != 0 {
				w.WriteHeader(fixture.writeStatus)
				return
			}
			for key, value := range changed {
				fixture.prefs[key] = value
			}
		case "/api/v2/search/plugins":
			assert.Equal(t, http.MethodGet, r.Method)
			assert.NoError(t, json.NewEncoder(w).Encode(fixture.plugins))
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{BaseURL: server.URL}, server.Client())
	require.NoError(t, err)
	return client, server
}

func conform(t *testing.T, client *Client, total int) ([]engine.ConformanceCheck, error) {
	t.Helper()
	probe, ok := any(client).(interface {
		Conform(context.Context, int) ([]engine.ConformanceCheck, error)
	})
	require.True(t, ok, "adapter must implement Conform")
	return probe.Conform(t.Context(), total)
}

func TestConformNoWriteWhenClean(t *testing.T) {
	for _, queueing := range []bool{true, false} {
		fixture := &conformanceFixture{prefs: cleanPreferences(), plugins: []any{}}
		fixture.prefs["queueing_enabled"] = queueing
		client, _ := conformClient(t, fixture)
		checks, err := conform(t, client, 5)
		require.NoError(t, err)
		require.NotEmpty(t, checks)
		for _, check := range checks {
			require.Equal(t, "ok", check.Severity)
			require.False(t, check.Forced)
			require.False(t, check.Warn)
		}
		require.Empty(t, fixture.writes)
		require.Equal(t, []string{"/api/v2/app/preferences", "/api/v2/search/plugins"}, fixture.paths)
	}
}

func TestConformForcesAutoTMMOff(t *testing.T) {
	for _, key := range []string{"rss_processing_enabled", "scheduler_enabled", "auto_tmm_enabled", "max_active_downloads", "max_active_uploads", "max_active_torrents", "max_active_checking_torrents"} {
		t.Run(key, func(t *testing.T) {
			fixture := &conformanceFixture{prefs: cleanPreferences(), plugins: []any{}}
			var want any = false
			if key[:3] == "max" {
				fixture.prefs[key] = 1
				want = float64(5)
			} else {
				fixture.prefs[key] = true
			}
			client, _ := conformClient(t, fixture)
			checks, err := conform(t, client, 5)
			require.NoError(t, err)
			require.Equal(t, []map[string]any{{key: want}}, fixture.writes)
			forced := 0
			for _, check := range checks {
				if check.Key != key {
					require.Equal(t, "ok", check.Severity)
					continue
				}
				require.Equal(t, "forced", check.Severity)
				require.True(t, check.Forced)
				require.False(t, check.Warn)
				forced++
			}
			require.Equal(t, 1, forced)
		})
	}
}

func TestConformBatchesChanges(t *testing.T) {
	fixture := &conformanceFixture{prefs: cleanPreferences(), plugins: []any{}}
	expected := map[string]any{}
	for _, key := range []string{"rss_processing_enabled", "scheduler_enabled", "auto_tmm_enabled"} {
		fixture.prefs[key] = true
		expected[key] = false
	}
	for _, key := range []string{"max_active_downloads", "max_active_uploads", "max_active_torrents", "max_active_checking_torrents"} {
		fixture.prefs[key] = 1
		expected[key] = float64(5)
	}
	client, _ := conformClient(t, fixture)
	_, err := conform(t, client, 5)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{expected}, fixture.writes)
	for key, value := range expected {
		require.Equal(t, value, fixture.prefs[key])
	}
}

func TestConformWarnsOnSearchPlugin(t *testing.T) {
	fixture := &conformanceFixture{prefs: cleanPreferences(), plugins: []any{map[string]string{"name": "third-party"}}}
	client, _ := conformClient(t, fixture)
	checks, err := conform(t, client, 5)
	require.NoError(t, err)
	warnings := 0
	for _, check := range checks {
		if check.Severity != "warn" {
			continue
		}
		warnings++
		require.Equal(t, "search/plugins", check.Key)
		require.Equal(t, "1", check.Got)
		require.True(t, check.Warn)
	}
	require.Equal(t, 1, warnings)
	require.Empty(t, fixture.writes)
	require.Equal(t, []string{"/api/v2/app/preferences", "/api/v2/search/plugins"}, fixture.paths)
}

func TestConformNeverFailsBoot(t *testing.T) {
	for _, scenario := range []string{"read500", "write500", "missing", "wrong-type", "null", "unreachable"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := &conformanceFixture{prefs: cleanPreferences(), plugins: []any{}}
			switch scenario {
			case "read500":
				fixture.readStatus = http.StatusInternalServerError
			case "write500":
				fixture.prefs["auto_tmm_enabled"] = true
				fixture.writeStatus = http.StatusInternalServerError
			case "missing":
				delete(fixture.prefs, "auto_tmm_enabled")
			case "wrong-type":
				fixture.prefs["auto_tmm_enabled"] = "false"
			case "null":
				fixture.prefs = nil
			}
			client, server := conformClient(t, fixture)
			if scenario == "unreachable" {
				server.Close()
			}
			checks, err := conform(t, client, 5)
			if scenario == "unreachable" {
				require.ErrorIs(t, err, engine.ErrUnavailable)
				return
			}
			require.NoError(t, err)
			warnings := 0
			for _, check := range checks {
				if check.Warn {
					warnings++
					require.Equal(t, "warn", check.Severity)
					require.False(t, check.Forced)
				}
			}
			require.Positive(t, warnings)
			if scenario != "write500" {
				require.Empty(t, fixture.writes)
			}
		})
	}
}

func TestConformPreservesNativeUnlimited(t *testing.T) {
	const nativeUnlimited = -1

	fixture := &conformanceFixture{prefs: cleanPreferences(), plugins: []any{}}
	for _, key := range []string{"max_active_downloads", "max_active_uploads", "max_active_torrents", "max_active_checking_torrents"} {
		fixture.prefs[key] = nativeUnlimited
	}
	client, _ := conformClient(t, fixture)
	checks, err := conform(t, client, 5)
	require.NoError(t, err)
	require.Empty(t, fixture.writes, "unlimited native ceilings must not become finite")
	for _, check := range checks {
		require.Equal(t, "ok", check.Severity, check.Key)
		require.False(t, check.Forced, check.Key)
	}
}

func TestConformUnlimitedDisablesQueueing(t *testing.T) {
	fixture := &conformanceFixture{prefs: cleanPreferences(), plugins: []any{}}
	client, _ := conformClient(t, fixture)
	checks, err := conform(t, client, 0)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"queueing_enabled": false}}, fixture.writes)
	require.Contains(t, checks, engine.ConformanceCheck{Key: "queueing_enabled", Want: "false", Got: "true", Forced: true, Severity: "forced"})
}
