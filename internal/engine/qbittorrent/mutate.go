// The mutation calls of docs/06-download-engines.md section 5.7 that
// change a torrent's sharing and placement: share limits, location,
// category, name, tags and sequential download. Every call posts a
// urlencoded form through Client.do; every method strips the
// "qbittorrent:" namespace off the engine task id itself, like the rest
// of the adapter.
//
// Units: tasks.seeding_time_limit and the PATCH body carry seconds;
// the daemon's seedingTimeLimit takes minutes. SetShareLimits is the
// single place that converts, rounding up, so a 90-second limit is sent
// as 2 minutes and an explicit 0 stays 0.

package qbittorrent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/L-K-M/dl-tool/internal/engine"
)

const (
	pathSetShareLimits = "torrents/setShareLimits"
	pathSetLocation    = "torrents/setLocation"
	pathSetCategory    = "torrents/setCategory"
	pathRename         = "torrents/rename"
	pathAddTags        = "torrents/addTags"
	pathRemoveTags     = "torrents/removeTags"
	pathToggleSeq      = "torrents/toggleSequentialDownload"

	// shareLimitUseGlobal is the daemon's "use the global default"
	// sentinel of torrents/setShareLimits, read verbatim from the pinned
	// sources: release-5.2.3 src/base/bittorrent/sharelimits.h declares
	// `inline const qreal DEFAULT_RATIO_LIMIT = -2;` and
	// `inline const int DEFAULT_SEEDING_TIME_LIMIT = -2;`. The live
	// capture of a torrent that never had a share limit set agrees
	// (qb_maindata_full_5.2.3.json: ratio_limit, seeding_time_limit and
	// inactive_seeding_time_limit all -2; the max_* twins report the
	// resolved global values instead, so they are not the sentinel).
	shareLimitUseGlobal = "-2"

	// shareLimitActionDefault is the shareLimitAction spelling dl-tool
	// always sends: release-5.2.3 requires the parameter, dl-tool never
	// changes the action, and "Default" means "whatever the daemon's
	// share-limit settings say" — the enum name the daemon itself
	// serialises back in torrents/info's share_limit_action.
	shareLimitActionDefault = "Default"

	// tagListSeparator joins the tags field of addTags and removeTags:
	// the daemon splits the field on commas.
	tagListSeparator = ","
)

// SetShareLimits posts torrents/setShareLimits with both limits on every
// call, so qBittorrent stops seeding on whichever fires first —
// deliberately OR, unlike Download Station's AND (FR-019). A nil ratio or
// nil seedSeconds means "use the global default" and is sent as the
// sentinel; an explicit 0 stops as soon as the daemon's check runs.
// seedSeconds carries dl-tool's stored seconds — the value of
// tasks.seeding_time_limit — and is converted here, rounded up; dl-tool
// does not expose inactiveSeedingTimeLimit, so it always rides as the
// sentinel.
func (c *Client) SetShareLimits(ctx context.Context, id string, ratio *float64, seedSeconds *int64) error {
	form := url.Values{
		"hashes":                   {ref(id)},
		"ratioLimit":               {ratioOrGlobal(ratio)},
		"seedingTimeLimit":         {secondsOrGlobal(seedSeconds)},
		"inactiveSeedingTimeLimit": {shareLimitUseGlobal},
		"shareLimitAction":         {shareLimitActionDefault},
	}
	_, err := c.do(ctx, http.MethodPost, pathSetShareLimits, form)

	return notFoundOr(err, pathSetShareLimits)
}

// ratioOrGlobal renders ratioLimit: nil becomes the use-the-global
// sentinel, 0 stays 0 (stop as soon as the check runs).
func ratioOrGlobal(ratio *float64) string {
	if ratio == nil {
		return shareLimitUseGlobal
	}

	return strconv.FormatFloat(*ratio, 'f', -1, 64)
}

// secondsOrGlobal renders seedingTimeLimit: nil becomes the
// use-the-global sentinel, otherwise the seconds are rounded up to whole
// minutes — 0 stays 0, 90 becomes 2.
func secondsOrGlobal(seconds *int64) string {
	if seconds == nil {
		return shareLimitUseGlobal
	}

	return strconv.FormatInt(secondsToSeedMinutes(*seconds), 10)
}

// secondsToSeedMinutes rounds seed seconds up to whole minutes: 90 s is
// 2 min, because a limit crossed at 1:30 must not wait until 2:00 to fire.
func secondsToSeedMinutes(seconds int64) int64 {
	return (seconds + 59) / 60
}

// SetLocation posts torrents/setLocation with hashes and location. The
// path is already resolved and jailed by internal/fsx; this method never
// joins or cleans a path itself. The endpoint creates the directory when
// it does not exist and disables Automatic Torrent Management on its own
// (release-5.2.3 torrentscontroller.cpp), so dl-tool needs neither call.
func (c *Client) SetLocation(ctx context.Context, id, path string) error {
	form := url.Values{
		"hashes":   {ref(id)},
		"location": {path},
	}
	_, err := c.do(ctx, http.MethodPost, pathSetLocation, form)

	return notFoundOr(err, pathSetLocation)
}

// SetCategory posts torrents/setCategory with hashes and category. An
// empty category clears it: the daemon's setCategory("") unassigns.
func (c *Client) SetCategory(ctx context.Context, id, category string) error {
	form := url.Values{
		"hashes":   {ref(id)},
		"category": {category},
	}
	_, err := c.do(ctx, http.MethodPost, pathSetCategory, form)

	return notFoundOr(err, pathSetCategory)
}

// Rename posts torrents/rename with hash and name. It renames the
// torrent, never a file on disk; the daemon answers 404 for a hash it
// does not hold, which maps to engine.ErrNotFound.
func (c *Client) Rename(ctx context.Context, id, name string) error {
	form := url.Values{
		"hash": {ref(id)},
		"name": {name},
	}
	_, err := c.do(ctx, http.MethodPost, pathRename, form)

	return notFoundOr(err, pathRename)
}

// SetTags replaces the task's tags: one torrents/removeTags for the tags
// to drop and one torrents/addTags for the tags to add, both with
// comma-separated values. The diff runs against the tags the maindata
// cache holds, so an unchanged tag set issues no request at all. A side
// with nothing in it is never sent: the daemon's removeTags with an
// empty tags value removes every tag, not none.
func (c *Client) SetTags(ctx context.Context, id string, tags []string) error {
	current, err := c.cachedTags(id)
	if err != nil {
		return err
	}

	drop, add := tagDiff(current, tags)
	if len(drop) > 0 {
		form := url.Values{
			"hashes": {ref(id)},
			"tags":   {strings.Join(drop, tagListSeparator)},
		}
		if _, err := c.do(ctx, http.MethodPost, pathRemoveTags, form); err != nil {
			return notFoundOr(err, pathRemoveTags)
		}
	}
	if len(add) > 0 {
		form := url.Values{
			"hashes": {ref(id)},
			"tags":   {strings.Join(add, tagListSeparator)},
		}
		if _, err := c.do(ctx, http.MethodPost, pathAddTags, form); err != nil {
			return notFoundOr(err, pathAddTags)
		}
	}

	return nil
}

// cachedTags reads the comma-concatenated tags of one cached torrent. A
// hash the cache does not hold is one the daemon does not know either —
// dl-tool owns its transfers exclusively (ADR-0017) and the cache mirrors
// the daemon's set — so the honest answer is engine.ErrNotFound, not a
// diff against a guessed empty set.
func (c *Client) cachedTags(id string) ([]string, error) {
	hash := ref(id)

	c.md.mu.Lock()
	fields, held := c.md.cache.fields[hash]
	c.md.mu.Unlock()

	if !held {
		return nil, fmt.Errorf("qbittorrent: %s: torrent not held by the daemon: %w",
			id, engine.ErrNotFound)
	}

	raw, _ := fields["tags"].(string)

	return splitTags(raw), nil
}

// tagDiff splits wanted against held: drop is what held carries that
// wanted does not, add the reverse. Both keep first-seen order.
func tagDiff(held, wanted []string) (drop, add []string) {
	heldSet := make(map[string]struct{}, len(held))
	for _, tag := range held {
		heldSet[tag] = struct{}{}
	}
	wantedSet := make(map[string]struct{}, len(wanted))
	for _, tag := range wanted {
		wantedSet[tag] = struct{}{}
	}

	for _, tag := range held {
		if _, keep := wantedSet[tag]; !keep {
			drop = append(drop, tag)
		}
	}
	for _, tag := range wanted {
		if _, exists := heldSet[tag]; !exists {
			add = append(add, tag)
		}
	}

	return drop, add
}

// SetSequential posts torrents/toggleSequentialDownload when the
// requested value differs from the seq_dl field the cache holds. The
// endpoint is a toggle, not a setter: calling it unconditionally would
// flip a torrent that already matches, so the cached value is the guard
// and a cache miss is engine.ErrNotFound — without a known current value
// there is nothing safe to post.
func (c *Client) SetSequential(ctx context.Context, id string, sequential bool) error {
	current, err := c.cachedSequential(id)
	if err != nil {
		return err
	}
	if current == sequential {
		return nil
	}

	_, err = c.do(ctx, http.MethodPost, pathToggleSeq, hashesForm(ref(id)))

	return notFoundOr(err, pathToggleSeq)
}

// cachedSequential reads the seq_dl flag of one cached torrent, false
// when the object carries no such key.
func (c *Client) cachedSequential(id string) (bool, error) {
	hash := ref(id)

	c.md.mu.Lock()
	fields, held := c.md.cache.fields[hash]
	c.md.mu.Unlock()

	if !held {
		return false, fmt.Errorf("qbittorrent: %s: torrent not held by the daemon: %w",
			id, engine.ErrNotFound)
	}

	current, _ := fields["seq_dl"].(bool)

	return current, nil
}

// notFoundOr maps a 404 reply onto engine.ErrNotFound for the mutations
// the daemon answers with one; transport failures already wrap
// engine.ErrUnavailable inside roundTrip, so they pass through.
func notFoundOr(err error, apiPath string) error {
	if err == nil {
		return nil
	}

	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound {
		return fmt.Errorf("qbittorrent: %s: torrent not held by the daemon: %w: %w",
			apiPath, engine.ErrNotFound, err)
	}

	return err
}
