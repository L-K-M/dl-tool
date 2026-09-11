// The peer listing of docs/06-download-engines.md section 5.7: GET
// sync/torrentPeers, a rid-delta endpoint shaped exactly like
// sync/maindata but scoped to one torrent's peers. Every wire fact below
// was read from one full response and one delta response of a live
// release-5.2.3 daemon (docs/tasks/T035-task-peers.md Evidence), never
// from the wiki:
//
//   - the envelope carries rid, full_update, show_flags and a peers
//     object whose keys are the peer's "ip:port"; a delta carries only
//     changed peers — as per-peer partial objects — plus a
//     peers_removed array of the "ip:port" keys that disappeared.
//   - a peer object carries client, peer_id_client, progress (0.0-1.0),
//     dl_speed and up_speed (bytes per second, no unit conversion),
//     downloaded, uploaded, connection, flags, flags_desc, relevance,
//     files, host_name, ip, port, country and country_code.
//   - flags is a letter string ("D X L P"), not a number; client is
//     empty until the handshake completes; country is "N/A" and
//     country_code empty for an address the GeoIP database cannot name.
package qbittorrent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/L-K-M/dl-tool/internal/engine"
)

const pathSyncTorrentPeers = "sync/torrentPeers"

// peerUnknownCountry is the GeoIP manager's "no country for this
// address" answer. Like the -1 counts of a never-contacted tracker row
// it is the daemon's unknown marker, not a value, so Country reports nil
// for it — doc 05 section 1.6's "never a sentinel standing in for
// unknown" rule.
const peerUnknownCountry = "N/A"

// PeerEntry is one connected peer, normalised onto the wire shape of
// 05 section 5.9. Client, Flags and Country are nil when the engine does
// not report them. Rates are bytes per second, never KB/s.
type PeerEntry struct {
	Address      string // "host:port", the daemon's own peer identity
	Client       *string
	Progress     float64 // 0.0 to 1.0
	DownloadRate int64
	UploadRate   int64
	Flags        *string
	Country      *string
}

// peersResponse is the GET /api/v2/sync/torrentPeers envelope. Peers
// values are partial objects in a delta, so they are held as raw JSON
// and merged key by key, never decoded into peerJSON before merging.
// show_flags states whether the WebUI shows country flags; nothing here
// consumes it.
type peersResponse struct {
	Rid          int                        `json:"rid"`
	FullUpdate   bool                       `json:"full_update"`
	Peers        map[string]json.RawMessage `json:"peers"`
	PeersRemoved []string                   `json:"peers_removed"`
}

// peerJSON is one value of the response's peers object, with the key
// names of the captured 5.2.3 response. The keys this adapter does not
// consume (peer_id_client, downloaded, uploaded, connection, flags_desc,
// relevance, files, host_name, ip, port, country_code) are left to the
// decoder to ignore.
type peerJSON struct {
	Client   string  `json:"client"`
	Progress float64 `json:"progress"`
	DlSpeed  int64   `json:"dl_speed"`
	UpSpeed  int64   `json:"up_speed"`
	Flags    string  `json:"flags"`
	Country  string  `json:"country"`
}

// peerSession is one torrent's slice of the delta protocol: the last
// accepted rid and the merged peer objects keyed by "ip:port". mu is
// held across the whole Peers exchange for the torrent, so two
// concurrent calls cannot merge out-of-order responses — the second
// waits, re-reads the rid and is answered against the state the first
// already merged.
type peerSession struct {
	mu    sync.Mutex
	rid   int
	peers map[string]map[string]any
}

// peerSessions is the per-client home of the torrentPeers state. The
// store is package-level and keyed by client because a Client cannot
// grow fields without editing client.go, which this task's Files table
// does not list; its mutex plays the role of the client mutex of the
// task's step 2. Entries are created on first use and dropped when a
// torrent leaves the maindata cache or the daemon reports it gone, so
// growth is bounded by the torrents dl-tool actively lists peers for.
var peerSessions = struct {
	mu       sync.Mutex
	byClient map[*Client]map[string]*peerSession
}{byClient: make(map[*Client]map[string]*peerSession)}

// peerSession returns the torrent's session, creating it on first use.
func (c *Client) peerSession(hash string) *peerSession {
	peerSessions.mu.Lock()
	defer peerSessions.mu.Unlock()

	sessions := peerSessions.byClient[c]
	if sessions == nil {
		sessions = make(map[string]*peerSession)
		peerSessions.byClient[c] = sessions
	}
	if sessions[hash] == nil {
		sessions[hash] = &peerSession{}
	}

	return sessions[hash]
}

// dropPeerSession removes the torrent's session — its rid and its merged
// peer set — so a re-added torrent starts at rid=0 with a full response
// instead of merging into a stale peer set.
func (c *Client) dropPeerSession(hash string) {
	peerSessions.mu.Lock()
	defer peerSessions.mu.Unlock()

	if sessions := peerSessions.byClient[c]; sessions != nil {
		delete(sessions, hash)
	}
}

// Peers lists the peers currently connected to one torrent through
// GET /api/v2/sync/torrentPeers, holding one rid per torrent and merging
// exactly as the maindata cache of T030 does: a full_update replaces the
// set, a delta deep-merges each per-peer partial object and then applies
// peers_removed. A torrent the maindata cache no longer holds has left
// the daemon (or was never owned), so its session is dropped before the
// request. A daemon that answers 404 for the hash wraps
// engine.ErrNotFound; a transport failure arrives wrapped in
// engine.ErrUnavailable from the round trip.
func (c *Client) Peers(ctx context.Context, id string) ([]PeerEntry, error) {
	hash := ref(id)

	// The 1 Hz maindata poll notices a removal within its interval; a
	// hash it no longer holds must not keep a rid a re-add could ride.
	c.md.mu.Lock()
	_, held := c.md.cache.fields[hash]
	c.md.mu.Unlock()
	if !held {
		c.dropPeerSession(hash)
	}

	session := c.peerSession(hash)
	session.mu.Lock()
	defer session.mu.Unlock()

	form := url.Values{
		"hash": {hash},
		"rid":  {strconv.Itoa(session.rid)},
	}
	body, err := c.do(ctx, http.MethodGet, pathSyncTorrentPeers, form)
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound {
			c.dropPeerSession(hash)
			return nil, fmt.Errorf("qbittorrent: %s: torrent not held by the daemon: %w: %w",
				pathSyncTorrentPeers, engine.ErrNotFound, err)
		}

		return nil, err
	}

	// A decode failure leaves the session untouched: the daemon answers
	// a rid it does not recognise with a full response, so a stale rid
	// repairs itself on the next call.
	var resp peersResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("qbittorrent: decode %s: %w", pathSyncTorrentPeers, err)
	}

	session.merge(resp)

	return session.entries(), nil
}

// merge applies one response to the session. A full update rebuilds the
// peer set solely from the response — a peer the response omits is
// gone, exactly as mergeFull prunes a torrent the response omits; an
// undecodable peer value keeps the last complete object, because invalid
// data is not a removal. A delta deep-merges each reported peer's
// partial object into the stored one and then applies peers_removed,
// reusing the maindata merge helpers (decodeTorrentFields,
// torrentFieldsEqual) rather than copying them.
func (s *peerSession) merge(resp peersResponse) {
	if resp.FullUpdate {
		fresh := make(map[string]map[string]any, len(resp.Peers))
		for key, raw := range resp.Peers {
			if fields, ok := decodeTorrentFields(raw); ok {
				fresh[key] = fields
			} else if old, held := s.peers[key]; held {
				fresh[key] = old
			} else {
				// The shared decode helper already logged its own warn —
				// under the maindata name — so this line is the one a
				// torrentPeers operator can grep for.
				slog.Warn("qbittorrent: torrentPeers full update carries an undecodable peer",
					"engine", engine.NameQBittorrent, "peer", key)
			}
		}
		s.peers = fresh
		s.rid = resp.Rid

		return
	}

	if s.peers == nil {
		s.peers = make(map[string]map[string]any, len(resp.Peers))
	}
	for key, raw := range resp.Peers {
		partial, ok := decodeTorrentFields(raw)
		if !ok {
			// Same double-naming trade-off as the full-update branch: the
			// helper's warn misattributes the endpoint, this one does not.
			slog.Warn("qbittorrent: torrentPeers delta carries an undecodable peer",
				"engine", engine.NameQBittorrent, "peer", key)

			continue
		}
		stored := s.peers[key]
		if stored == nil {
			stored = make(map[string]any, len(partial))
			s.peers[key] = stored
		} else if torrentFieldsEqual(stored, partial) {
			continue
		}
		for name, value := range partial {
			stored[name] = value
		}
	}
	for _, key := range resp.PeersRemoved {
		delete(s.peers, key)
	}
	s.rid = resp.Rid
}

// entries projects the merged peer objects onto PeerEntry, sorted by
// address so the listing is stable between polls. Each stored object is
// round-tripped through peerJSON — the same projection taskInfoFromCache
// applies to a cached torrent object — and an object that cannot be
// projected is logged and skipped, never fatal to the listing.
func (s *peerSession) entries() []PeerEntry {
	entries := make([]PeerEntry, 0, len(s.peers))
	for address, fields := range s.peers {
		raw, err := json.Marshal(fields)
		if err != nil {
			slog.Warn("qbittorrent: cached peer object is not marshalable",
				"engine", engine.NameQBittorrent, "peer", address, "error", err)
			continue
		}

		var peer peerJSON
		if err := json.Unmarshal(raw, &peer); err != nil {
			slog.Warn("qbittorrent: cached peer object does not match the torrentPeers shape",
				"engine", engine.NameQBittorrent, "peer", address, "error", err)
			continue
		}
		entries = append(entries, toPeerEntry(address, peer))
	}

	slices.SortFunc(entries, func(a, b PeerEntry) int {
		return strings.Compare(a.Address, b.Address)
	})
	return entries
}

// toPeerEntry maps one merged peer object onto PeerEntry. Client, Flags
// and Country stay nil for an absent or empty key — and for the daemon's
// "N/A" unknown-country marker — so the wire answer carries null, never
// "" or a sentinel. Rates pass through as the bytes per second the
// daemon already reports; progress is clamped onto the 0.0-1.0 range of
// doc 05 section 1.6.
func toPeerEntry(address string, peer peerJSON) PeerEntry {
	return PeerEntry{
		Address:      address,
		Client:       optionalPeerString(peer.Client),
		Progress:     min(max(peer.Progress, 0.0), 1.0),
		DownloadRate: peer.DlSpeed,
		UploadRate:   peer.UpSpeed,
		Flags:        optionalPeerString(peer.Flags),
		Country:      optionalPeerCountry(peer.Country),
	}
}

// optionalPeerString reports nil for the empty string, the engine's
// "nothing to report" spelling of an optional peer field.
func optionalPeerString(value string) *string {
	if value == "" {
		return nil
	}

	return &value
}

// optionalPeerCountry additionally reports nil for the daemon's "N/A"
// unknown-country marker, which is a sentinel for "no value" rather than
// a value.
func optionalPeerCountry(value string) *string {
	if value == peerUnknownCountry {
		return nil
	}

	return optionalPeerString(value)
}
