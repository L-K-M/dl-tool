// The tracker calls of docs/06-download-engines.md section 5.7: GET
// torrents/trackers plus the torrents/addTrackers and removeTrackers
// mutations. Every wire fact below was read from one real response of a
// live release-5.2.3 daemon and cross-checked against its sources
// (docs/tasks/T034-task-trackers.md Evidence), never from the wiki:
//
//   - a row carries url, tier, status, msg, num_peers, num_seeds,
//     num_leeches and num_downloaded, and the real tracker rows add
//     updating, endpoints, next_announce and min_announce; the synthetic
//     DHT, PeX and LSD rows carry none of the latter four.
//   - status is a JSON number — the daemon's TrackerEndpointState
//     (1 not contacted, 2 working, 4 not working, 5 tracker error,
//     6 unreachable; the synthetic rows also use 0 disabled) — rendered
//     here as its decimal string and stored verbatim downstream.
//   - the -1 counts of a never-contacted tracker are the "unknown"
//     defaults of TrackerEntryStatus, not values.
//   - no field of the response is an update timer; next_announce and
//     min_announce are epoch seconds, so UpdateTimerSeconds has no wire
//     source and stays nil.

package qbittorrent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/L-K-M/dl-tool/internal/engine"
)

const (
	pathTorrentsTrackers = "torrents/trackers"
	pathAddTrackers      = "torrents/addTrackers"
	pathRemoveTrackers   = "torrents/removeTrackers"

	// trackerAddSeparator joins the urls field of torrents/addTrackers:
	// parseTrackerEntries splits it on newlines and counts an empty line
	// as a tier bump (release-5.2.3 trackerentry.cpp), so one url per line
	// keeps every added tracker in tier 0.
	trackerAddSeparator = "\n"

	// trackerRemoveSeparator joins the urls field of
	// torrents/removeTrackers: the daemon splits on pipes and then
	// percent-decodes each element (release-5.2.3 torrentscontroller.cpp),
	// so each url must ride one percent-encoded element to survive.
	trackerRemoveSeparator = "|"

	// trackerUnknownCount is the daemon's "no value" marker for the
	// num_seeds and num_peers counts of a real tracker row (the
	// TrackerEntryStatus defaults of trackerentrystatus.h).
	trackerUnknownCount = -1
)

// The synthetic DHT, PeX and LSD rows of getStickyTrackers spell their
// urls "** [DHT] **", "** [PeX] **" and "** [LSD] **". Detection is the
// bracketed form, not a fixed name list, so a renamed synthetic row keeps
// its meaning.
const (
	pseudoTrackerPrefix = "** ["
	pseudoTrackerSuffix = "] **"
)

// isPseudoTrackerURL reports whether url is one of the daemon's synthetic
// swarm-source rows, which can be listed but never removed.
func isPseudoTrackerURL(url string) bool {
	return strings.HasPrefix(url, pseudoTrackerPrefix) && strings.HasSuffix(url, pseudoTrackerSuffix)
}

// TrackerEntry is one row of GET /api/v2/torrents/trackers, normalised
// onto the wire shape of 05 section 5.9. Status is the engine's own value
// rendered as a string and stored verbatim; Seeds and Peers are nil when
// the engine does not report them for that row — the -1 of a real row it
// never heard from, and every count of the synthetic DHT, PeX and LSD
// rows, whose numbers count peers this swarm discovered, not tracker
// counts. UpdateTimerSeconds is nil: the daemon reports no such value.
type TrackerEntry struct {
	URL                string
	Status             string
	Seeds              *int
	Peers              *int
	Message            string
	UpdateTimerSeconds *int
}

// trackerJSON is one element of GET /api/v2/torrents/trackers, with the
// key names of the captured 5.2.3 response. The keys this adapter does
// not consume (tier, num_leeches, num_downloaded and the real-row extras)
// are left to the decoder to ignore.
type trackerJSON struct {
	URL     string `json:"url"`
	Status  int    `json:"status"`
	Message string `json:"msg"`
	Seeds   int    `json:"num_seeds"`
	Peers   int    `json:"num_peers"`
}

// Trackers lists the trackers of one torrent: the synthetic DHT, PeX and
// LSD rows first, then the real rows in tier order, exactly as the daemon
// serialises them. id is the engine-namespaced task id. A daemon that
// answers 404 for the hash wraps engine.ErrNotFound: no engine holds the
// torrent, the caller's foreign-task answer.
func (c *Client) Trackers(ctx context.Context, id string) ([]TrackerEntry, error) {
	body, err := c.do(ctx, http.MethodGet, pathTorrentsTrackers, url.Values{"hash": {ref(id)}})
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound {
			return nil, fmt.Errorf("qbittorrent: %s: torrent not held by the daemon: %w: %w",
				pathTorrentsTrackers, engine.ErrNotFound, err)
		}

		return nil, err
	}

	var rows []trackerJSON
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("qbittorrent: decode %s: %w", pathTorrentsTrackers, err)
	}

	return toTrackerEntries(rows), nil
}

// toTrackerEntries maps the captured row shape onto TrackerEntry. The -1
// counts of a real row mean "unknown" and become nil; the synthetic rows
// report peer-discovery counts, not tracker counts, and their Seeds and
// Peers are nil on the wire shape of 05 section 5.9.
func toTrackerEntries(rows []trackerJSON) []TrackerEntry {
	entries := make([]TrackerEntry, 0, len(rows))
	for _, row := range rows {
		entry := TrackerEntry{
			URL:     row.URL,
			Status:  strconv.Itoa(row.Status),
			Message: row.Message,
		}

		if !isPseudoTrackerURL(row.URL) {
			if row.Seeds != trackerUnknownCount {
				seeds := row.Seeds
				entry.Seeds = &seeds
			}
			if row.Peers != trackerUnknownCount {
				peers := row.Peers
				entry.Peers = &peers
			}
		}

		entries = append(entries, entry)
	}

	return entries
}

// AddTrackers posts torrents/addTrackers with hash and a newline-separated
// urls field. An empty list is a caller bug — the API layer requires at
// least one url — and so is an empty url or one carrying a line break:
// the daemon splits the field on newlines and an embedded one would add
// announce urls the caller never named.
func (c *Client) AddTrackers(ctx context.Context, id string, urls []string) error {
	if len(urls) == 0 {
		return fmt.Errorf("qbittorrent: %s of %q: empty url list", pathAddTrackers, id)
	}
	for _, raw := range urls {
		if raw == "" || strings.ContainsAny(raw, "\r\n") {
			return fmt.Errorf("qbittorrent: %s of %q: url %q is empty or carries a line break",
				pathAddTrackers, id, raw)
		}
	}

	form := url.Values{
		"hash": {ref(id)},
		"urls": {strings.Join(urls, trackerAddSeparator)},
	}
	_, err := c.do(ctx, http.MethodPost, pathAddTrackers, form)

	return err
}

// RemoveTrackers posts torrents/removeTrackers with hash and a
// pipe-separated urls field, each url percent-encoded so a pipe inside
// one survives the daemon's split-then-decode. A pseudo-tracker url is
// refused locally with engine.ErrNotSupported, without issuing the
// request: the daemon would answer 204 and silently keep the synthetic
// row, so the refusal is dl-tool's, not the daemon's.
func (c *Client) RemoveTrackers(ctx context.Context, id string, urls []string) error {
	if len(urls) == 0 {
		return fmt.Errorf("qbittorrent: %s of %q: empty url list", pathRemoveTrackers, id)
	}

	encoded := make([]string, 0, len(urls))
	for _, raw := range urls {
		if isPseudoTrackerURL(raw) {
			return fmt.Errorf("qbittorrent: %s: %q is a pseudo-tracker and cannot be removed: %w",
				pathRemoveTrackers, raw, engine.ErrNotSupported)
		}
		encoded = append(encoded, escapeTrackerURL(raw))
	}

	form := url.Values{
		"hash": {ref(id)},
		"urls": {strings.Join(encoded, trackerRemoveSeparator)},
	}
	_, err := c.do(ctx, http.MethodPost, pathRemoveTrackers, form)

	return err
}

// escapeTrackerURL percent-encodes every byte outside RFC 3986's
// unreserved set. The daemon's removeTrackers splits the urls field on
// pipes before percent-decoding each element, so a pipe or a percent sign
// inside a url must arrive already escaped to survive both layers.
func escapeTrackerURL(raw string) string {
	var b strings.Builder
	b.Grow(len(raw) + 8)
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			b.WriteString("%")
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0xF])
		}
	}

	return b.String()
}

// upperHex is the hex alphabet of one percent-encoded byte.
const upperHex = "0123456789ABCDEF"
