package aria2

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/L-K-M/dl-tool/internal/engine"
)

const (
	methodGetGlobalOption     = "aria2.getGlobalOption"
	optMaxConcurrentDownloads = "max-concurrent-downloads"
	optSaveSession            = "save-session"
	conformanceOK             = "ok"
	conformanceForced         = "forced"
	conformanceWarn           = "warn"
)

// Conform raises the daemon ceiling and reports daemon-flag mismatches without changing them.
func (c *Client) Conform(ctx context.Context, maxActiveTotal int) ([]engine.ConformanceCheck, error) {
	checks := []engine.ConformanceCheck{
		{Key: optMaxConcurrentDownloads, Want: strconv.Itoa(maxActiveTotal), Severity: conformanceOK},
		{Key: optDir, Want: strings.Join(c.dataRoots, ", "), Severity: conformanceOK},
		{Key: optSaveSession, Want: "non-empty", Severity: conformanceOK},
	}
	raw, err := c.call(ctx, methodGetGlobalOption)
	if errors.Is(err, engine.ErrUnavailable) {
		for i := range checks {
			checks[i].Got = "unavailable"
			warnConformance(&checks[i])
		}
		return checks, err
	}
	var options map[string]string
	if err == nil {
		err = json.Unmarshal(raw, &options)
	}
	if err != nil || options == nil {
		for i := range checks {
			checks[i].Got = "unreadable"
			warnConformance(&checks[i])
		}
		return checks, nil
	}
	for i := range checks {
		checks[i].Got = options[checks[i].Key]
	}
	current, err := strconv.Atoi(options[optMaxConcurrentDownloads])
	switch {
	case err != nil || current < 0 || maxActiveTotal < 0:
		warnConformance(&checks[0])
	case current < maxActiveTotal:
		result, err := c.call(ctx, methodChangeGlobalOption, map[string]string{optMaxConcurrentDownloads: checks[0].Want})
		var outcome string
		if err != nil || json.Unmarshal(result, &outcome) != nil || outcome != "OK" {
			warnConformance(&checks[0])
			break
		}
		checks[0].Forced = true
		checks[0].Severity = conformanceForced
	}
	if !c.containsDirectory(options[optDir]) {
		warnConformance(&checks[1])
	}
	if strings.TrimSpace(options[optSaveSession]) == "" {
		warnConformance(&checks[2])
	}
	return checks, nil
}

func warnConformance(check *engine.ConformanceCheck) {
	check.Warn = true
	check.Severity = conformanceWarn
}

func (c *Client) containsDirectory(dir string) bool {
	if !filepath.IsAbs(dir) {
		return false
	}
	for _, root := range c.dataRoots {
		if !filepath.IsAbs(root) {
			continue
		}
		relative, err := filepath.Rel(root, dir)
		if err == nil && (relative == "." || filepath.IsLocal(relative)) {
			return true
		}
	}
	return false
}
