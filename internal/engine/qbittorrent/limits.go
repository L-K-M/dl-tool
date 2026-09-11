// The rate limits of docs/06-download-engines.md sections 10.1 and 5.7:
// per-task limits through torrents/setDownloadLimit and
// torrents/setUploadLimit, the global pair through transfer/setDownloadLimit
// and transfer/setUploadLimit, and the transfer/info read-back that proves
// a global fan-out landed. Bytes per second is the unit end to end — the
// PATCH body, these endpoints and every field read here all carry B/s, so
// no conversion exists and none is needed: no 1024 appears in this file or
// in limits_test.go.
//
// Neither transfer/toggleSpeedLimitsMode nor transfer/speedLimitsMode is
// ever called, here or anywhere else in the adapter: qBittorrent's own
// alternative-speed mode is a second source of truth for the same number,
// and dl-tool always pushes the one absolute value it computed itself
// (section 10.1). The fake of limits_test.go rejects those paths outright,
// so a regression fails the test that introduces it.

package qbittorrent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/L-K-M/dl-tool/internal/engine"
)

const (
	pathTorrentsSetDownloadLimit = "torrents/setDownloadLimit"
	pathTorrentsSetUploadLimit   = "torrents/setUploadLimit"
	pathTransferSetDownloadLimit = "transfer/setDownloadLimit"
	pathTransferSetUploadLimit   = "transfer/setUploadLimit"
	pathTransferInfo             = "transfer/info"

	// limitFormKey names the value field of every set endpoint.
	limitFormKey = "limit"

	// limitVerifyDeltas is how many sync/maindata deltas a per-task set
	// gets to confirm itself in the merged cache before the
	// unconfirmed mismatch is logged: one for the honest delta lag,
	// one for a lost reply, one spare — an ordinary poll cycle never
	// trips the warn.
	limitVerifyDeltas = 3
)

// ErrLimitNotApplied is returned when the read-back does not match what
// was sent.
var ErrLimitNotApplied = errors.New("qbittorrent: rate limit did not take effect")

// transferInfo is the GET /api/v2/transfer/info envelope, restricted to
// the keys dl-tool reads. Only DlRateLimit and UpRateLimit are consumed
// here; the rest documents the envelope the boot capability probe and the
// status panel of later tasks read.
type transferInfo struct {
	DlInfoSpeed       int64  `json:"dl_info_speed"`
	UpInfoSpeed       int64  `json:"up_info_speed"`
	DlRateLimit       int64  `json:"dl_rate_limit"`
	UpRateLimit       int64  `json:"up_rate_limit"`
	DHTNodes          int64  `json:"dht_nodes"`
	ConnectionStatus  string `json:"connection_status"` // connected | firewalled | disconnected
	UseAltSpeedLimits bool   `json:"use_alt_speed_limits"`
}

// SetRateLimits applies bytes per second. An empty id means the global
// limit. A nil direction is left unchanged. 0 means unlimited. It never
// restarts a transfer and never touches the alternative-speed mode:
// dl-tool always pushes one absolute value it computed itself, so neither
// transfer/toggleSpeedLimitsMode nor transfer/speedLimitsMode is called.
// The two fan-out shapes of docs/06 section 10.1:
//
//	id != ""  → POST torrents/setDownloadLimit and torrents/setUploadLimit, fields hashes and limit
//	id == ""  → POST transfer/setDownloadLimit and transfer/setUploadLimit, field limit
//
// One request goes out per non-nil direction, download first, each
// failure wrapped with the direction that failed, so a partial failure
// leaves a diagnosable state. A global set is then verified through
// GlobalLimits and a mismatch is returned wrapped around
// ErrLimitNotApplied; a per-task set is verified against the dl_limit and
// up_limit the maindata cache holds, with no extra request.
func (c *Client) SetRateLimits(ctx context.Context, id string, down, up *int64) error {
	if down == nil && up == nil {
		return nil
	}

	if id == "" {
		return c.setGlobalLimits(ctx, down, up)
	}

	hash := ref(id)
	if down != nil {
		if err := c.postLimit(ctx, pathTorrentsSetDownloadLimit, hashesForm(hash), *down); err != nil {
			return fmt.Errorf("qbittorrent: download limit: %w", err)
		}
	}
	if up != nil {
		if err := c.postLimit(ctx, pathTorrentsSetUploadLimit, hashesForm(hash), *up); err != nil {
			return fmt.Errorf("qbittorrent: upload limit: %w", err)
		}
	}

	c.verifyTaskLimitsLater(hash, down, up)
	return nil
}

// postLimit posts one set endpoint with its limit field. 0 is sent as the
// literal 0 — unlimited, never dropped as "unset" — and the value rides
// the form body, not the query, like every mutating call of this adapter.
func (c *Client) postLimit(ctx context.Context, apiPath string, form url.Values, limit int64) error {
	form.Set(limitFormKey, strconv.FormatInt(limit, 10))
	_, err := c.do(ctx, http.MethodPost, apiPath, form)
	return err
}

// setGlobalLimits posts the transfer/* pair, then proves it landed: the
// read-back runs after every global fan-out, and a mismatch is logged at
// warn with both values and returned wrapped around ErrLimitNotApplied,
// never swallowed (docs/06 section 10.1).
func (c *Client) setGlobalLimits(ctx context.Context, down, up *int64) error {
	if down != nil {
		if err := c.postLimit(ctx, pathTransferSetDownloadLimit, url.Values{}, *down); err != nil {
			return fmt.Errorf("qbittorrent: download limit: %w", err)
		}
	}
	if up != nil {
		if err := c.postLimit(ctx, pathTransferSetUploadLimit, url.Values{}, *up); err != nil {
			return fmt.Errorf("qbittorrent: upload limit: %w", err)
		}
	}

	gotDown, gotUp, err := c.GlobalLimits(ctx)
	if err != nil {
		return fmt.Errorf("qbittorrent: read back global limits: %w", err)
	}
	if down != nil && gotDown != *down {
		warnLimitMismatch("download", *down, gotDown)
		return fmt.Errorf("qbittorrent: download limit: %w: sent %d, read back %d",
			ErrLimitNotApplied, *down, gotDown)
	}
	if up != nil && gotUp != *up {
		warnLimitMismatch("upload", *up, gotUp)
		return fmt.Errorf("qbittorrent: upload limit: %w: sent %d, read back %d",
			ErrLimitNotApplied, *up, gotUp)
	}
	return nil
}

// warnLimitMismatch logs one read-back mismatch at warn, with both the
// value dl-tool pushed and the one the daemon reported — the fan-out
// owner's signal that the two disagree.
func warnLimitMismatch(direction string, sent, read int64) {
	slog.Warn("qbittorrent: global rate limit read back different from what was sent",
		"engine", engine.NameQBittorrent, "direction", direction,
		"sent_bps", sent, "read_bps", read)
}

// GlobalLimits reads the current global limits back from GET
// /api/v2/transfer/info. It is called after every global fan-out; a
// mismatch with what was sent is logged at warn and returned, never
// swallowed. Both values are bytes per second; 0 means the daemon imposes
// no limit.
func (c *Client) GlobalLimits(ctx context.Context) (down, up int64, err error) {
	body, err := c.do(ctx, http.MethodGet, pathTransferInfo, nil)
	if err != nil {
		return 0, 0, err
	}

	var info transferInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return 0, 0, fmt.Errorf("qbittorrent: decode %s: %w", pathTransferInfo, err)
	}
	return info.DlRateLimit, info.UpRateLimit, nil
}

// verifyTaskLimitsLater confirms a per-task set against the merged cache
// the sync/maindata poll maintains — never against a second request: the
// daemon reports dl_limit and up_limit in every torrent object a delta
// carries, so the delta that follows a set is the read-back. The cache may
// already hold the sent values (an idempotent re-set changed nothing, so
// no delta will ever name them) and then nothing is scheduled. Otherwise
// one watcher rides the client's stop signal for limitVerifyDeltas poll
// intervals, reading the cache alone each tick: a match retires it, and a
// mismatch that survives three deltas logs a single warn. A warn is all a
// per-task mismatch ever becomes — the caller's mutation already
// succeeded, so the error path of the global read-back has no equivalent
// here.
func (c *Client) verifyTaskLimitsLater(hash string, down, up *int64) {
	if c.cacheHoldsLimits(hash, down, up) {
		return
	}

	// The channel Close closes with the poll goroutine and every
	// subscriber: the watcher must never outlive the client whose cache
	// it reads.
	c.md.mu.Lock()
	stopped := c.md.stopSignalLocked()
	c.md.mu.Unlock()

	go func() {
		ticker := time.NewTicker(c.pollIntervalSetting())
		defer ticker.Stop()

		for range limitVerifyDeltas {
			select {
			case <-stopped:
				return
			case <-ticker.C:
			}
			if c.cacheHoldsLimits(hash, down, up) {
				return
			}
		}

		c.warnTaskLimitUnconfirmed(hash, down, up)
	}()
}

// warnTaskLimitUnconfirmed logs the per-task mismatch that survived
// limitVerifyDeltas deltas, carrying the sent values and the cached ones.
func (c *Client) warnTaskLimitUnconfirmed(hash string, down, up *int64) {
	cacheDown, cacheUp, held := c.cachedLimits(hash)

	attrs := []any{"engine", engine.NameQBittorrent, "hash", hash}
	if down != nil {
		attrs = append(attrs, "sent_down_bps", *down)
		if held {
			attrs = append(attrs, "cached_down_bps", cacheDown)
		}
	}
	if up != nil {
		attrs = append(attrs, "sent_up_bps", *up)
		if held {
			attrs = append(attrs, "cached_up_bps", cacheUp)
		}
	}
	if !held {
		attrs = append(attrs, "cached", "hash not held by the maindata cache")
	}
	slog.Warn("qbittorrent: per-task rate limit not confirmed by sync/maindata", attrs...)
}

// cacheHoldsLimits reports whether the merged cache already carries
// exactly the values that were sent, in the directions that were sent. A
// hash the cache does not hold cannot confirm anything.
func (c *Client) cacheHoldsLimits(hash string, down, up *int64) bool {
	c.md.mu.Lock()
	fields, held := c.md.cache.fields[hash]
	c.md.mu.Unlock()

	if !held {
		return false
	}
	return limitFieldMatches(fields, "dl_limit", down) && limitFieldMatches(fields, "up_limit", up)
}

// limitFieldMatches compares one cached limit field against the sent
// value. A nil sent value was not part of the set and cannot mismatch.
// The merged object decodes JSON numbers as float64.
func limitFieldMatches(fields map[string]any, name string, sent *int64) bool {
	if sent == nil {
		return true
	}
	value, ok := fields[name].(float64)
	return ok && int64(value) == *sent
}

// cachedLimits reads the dl_limit and up_limit pair the merged cache
// holds for one hash; held reports whether the cache knows the hash at
// all.
func (c *Client) cachedLimits(hash string) (down, up int64, held bool) {
	c.md.mu.Lock()
	fields, exists := c.md.cache.fields[hash]
	c.md.mu.Unlock()

	if !exists {
		return 0, 0, false
	}
	return cacheLimitField(fields, "dl_limit"), cacheLimitField(fields, "up_limit"), true
}

// cacheLimitField reads one numeric torrent field; a missing or
// non-numeric field reads as 0, the daemon's own no-limit value.
func cacheLimitField(fields map[string]any, name string) int64 {
	value, ok := fields[name].(float64)
	if !ok {
		return 0
	}
	return int64(value)
}
