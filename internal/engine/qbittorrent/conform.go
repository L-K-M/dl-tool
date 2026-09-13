package qbittorrent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"

	"github.com/L-K-M/dl-tool/internal/engine"
)

const (
	pathPreferences    = "app/preferences"
	pathSetPreferences = "app/setPreferences"
	pathSearchPlugins  = "search/plugins"
	prefQueueing       = "queueing_enabled"
	conformanceOK      = "ok"
	conformanceForced  = "forced"
	conformanceWarn    = "warn"
)

// Conform disables competing automation and lifts native queue ceilings without touching transfers.
func (c *Client) Conform(ctx context.Context, maxActiveTotal int) ([]engine.ConformanceCheck, error) {
	checks := []engine.ConformanceCheck{
		{Key: "rss_processing_enabled", Want: "false", Severity: conformanceOK},
		{Key: "scheduler_enabled", Want: "false", Severity: conformanceOK},
		{Key: "auto_tmm_enabled", Want: "false", Severity: conformanceOK},
	}
	// These names were observed on the pinned daemon before any writes (T101 Evidence).
	queueKeys := []string{"max_active_downloads", "max_active_uploads", "max_active_torrents", "max_active_checking_torrents"}
	changed := map[string]any{}
	body, readErr := c.conformanceRequest(ctx, http.MethodGet, pathPreferences, nil)
	var prefs map[string]any
	if readErr == nil {
		readErr = json.Unmarshal(body, &prefs)
	}
	for i := range checks {
		value, ok := prefs[checks[i].Key].(bool)
		checks[i].Got = preferenceText(prefs, checks[i].Key)
		if readErr != nil || !ok {
			warnConformance(&checks[i])
			continue
		}
		if value {
			changed[checks[i].Key] = false
		}
	}
	queueing, known := prefs[prefQueueing].(bool)
	if !known || readErr != nil || maxActiveTotal < 0 {
		checks = append(checks, engine.ConformanceCheck{Key: prefQueueing, Want: "false or limits raised", Got: preferenceText(prefs, prefQueueing), Warn: true, Severity: conformanceWarn})
	} else if queueing && maxActiveTotal == 0 {
		// Zero is dl-tool's unlimited setting; no finite native queue can represent it.
		checks = append(checks, engine.ConformanceCheck{Key: prefQueueing, Want: "false", Got: "true", Severity: conformanceOK})
		changed[prefQueueing] = false
	} else if !queueing {
		checks = append(checks, engine.ConformanceCheck{Key: prefQueueing, Want: "false", Got: "false", Severity: conformanceOK})
	} else {
		for _, key := range queueKeys {
			check := engine.ConformanceCheck{Key: key, Want: strconv.Itoa(maxActiveTotal), Got: preferenceText(prefs, key), Severity: conformanceOK}
			value, ok := prefs[key].(float64)
			if !ok || math.Trunc(value) != value {
				warnConformance(&check)
			} else if value < float64(maxActiveTotal) {
				changed[key] = maxActiveTotal
			}
			checks = append(checks, check)
		}
	}
	if errors.Is(readErr, engine.ErrUnavailable) {
		return checks, readErr
	}

	// One sparse form avoids rewriting preferences dl-tool does not own.
	if len(changed) > 0 {
		encoded, err := json.Marshal(changed)
		if err == nil {
			_, err = c.conformanceRequest(ctx, http.MethodPost, pathSetPreferences, url.Values{"json": {string(encoded)}})
		}
		for i := range checks {
			if _, change := changed[checks[i].Key]; !change {
				continue
			}
			if err != nil {
				warnConformance(&checks[i])
				continue
			}
			checks[i].Forced = true
			checks[i].Severity = conformanceForced
		}
	}
	pluginCheck := engine.ConformanceCheck{Key: pathSearchPlugins, Want: "0", Severity: conformanceOK}
	body, err := c.conformanceRequest(ctx, http.MethodGet, pathSearchPlugins, nil)
	var plugins []json.RawMessage
	if err == nil {
		err = json.Unmarshal(body, &plugins)
	}
	if err != nil || plugins == nil {
		pluginCheck.Got = "unreadable"
		warnConformance(&pluginCheck)
	} else {
		pluginCheck.Got = strconv.Itoa(len(plugins))
		if len(plugins) > 0 {
			warnConformance(&pluginCheck)
		}
	}
	return append(checks, pluginCheck), nil
}

func (c *Client) conformanceRequest(ctx context.Context, method, path string, form url.Values) ([]byte, error) {
	var body []byte
	var contentType string
	if form != nil {
		body = []byte(form.Encode())
		contentType = formURLEncoded
	}
	_, result, err := c.authenticated(ctx, path, func() (int, []byte, error) { return c.roundTrip(ctx, method, path, nil, contentType, body) })
	return result, err
}

func warnConformance(check *engine.ConformanceCheck) {
	check.Warn = true
	check.Severity = conformanceWarn
}

func preferenceText(prefs map[string]any, key string) string {
	value, exists := prefs[key]
	if !exists {
		return "missing"
	}
	// Only expected scalar types may reach logs; arbitrary daemon content stays out.
	switch value := value.(type) {
	case bool:
		return strconv.FormatBool(value)
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	default:
		return "invalid"
	}
}
