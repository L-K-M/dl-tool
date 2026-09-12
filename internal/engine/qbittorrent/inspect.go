// The magnet metadata resolution of T038: InspectMagnet resolves a
// magnet's manifest without creating a dl-tool task, through the daemon's
// own metadata machinery. The primary path drives POST
// torrents/fetchMetadata — whose hidden metadata download never enters
// torrents/info and which the daemon removes by itself the moment the
// metadata lands. The fallback, for a daemon whose fetchMetadata answers
// 404 or 405, adds one temporary stopped handle and removes it with
// deleteFiles=true on every exit path.

package qbittorrent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/uri"
)

const (
	// pathFetchMetadata and pathTorrentsInfo are the two reads this file
	// adds. fetchMetadata's single form parameter is `source` — verified
	// against release-5.2.3 src/webui/api/torrentscontroller.cpp,
	// fetchMetadataAction: requireParams({u"source"_s}) — not the `url`
	// the older wiki pages show.
	pathFetchMetadata = "torrents/fetchMetadata"
	pathTorrentsInfo  = "torrents/info"

	// formSource names fetchMetadata's parameter.
	formSource = "source"

	// formStopCondition and stopConditionMetadataReceived spell the add
	// option that stops the fallback's temporary handle the instant its
	// metadata arrives, so not one payload byte is ever fetched.
	formStopCondition             = "stopCondition"
	stopConditionMetadataReceived = "MetadataReceived"

	// metadataPollInterval is the poll cadence of both paths: 500 ms per
	// T038's steps.
	metadataPollInterval = 500 * time.Millisecond

	// metadataCleanupTimeout budgets the deferred removal of the fallback's
	// temporary handle. It runs under its own context — a caller that
	// cancelled must still remove the handle — so it cannot borrow the
	// caller's (already expired) deadline.
	metadataCleanupTimeout = 10 * time.Second
)

// ErrMetadataTimeout is returned when metadata did not arrive inside the
// caller's deadline. The API layer maps it to metadata_pending: true with
// files: null, never to an error response.
var ErrMetadataTimeout = errors.New("qbittorrent: magnet metadata not resolved")

// InspectMagnet resolves a magnet's manifest without creating a dl-tool
// task. It satisfies the magnetInspector interface internal/api declared
// in T031.
//
// Primary path: POST torrents/fetchMetadata with the magnet as `source`,
// polled until the daemon answers with the full info object. Fallback,
// used when that endpoint answers 404 or 405: add the magnet with
// stopped=true, paused=true and stopCondition=MetadataReceived, poll
// torrents/files until it answers, then remove the handle with
// torrents/delete and deleteFiles=true. Either way no torrent is left
// behind: the primary path's hidden metadata download never appears in
// torrents/info and the daemon removes it itself, and the fallback's
// temporary handle is removed by a defer on every exit path, including
// timeouts and a cancelled caller context. The handle is never written to
// tasks, so the T030 ownership filter keeps rejecting it and it can never
// become a task.
func (c *Client) InspectMagnet(ctx context.Context, magnet string) (uri.Manifest, error) {
	n, err := uri.ParseMagnet(magnet)
	if err != nil {
		return uri.Manifest{}, err
	}

	manifest, err := c.inspectViaFetchMetadata(ctx, n.URI)
	switch {
	case err == nil:
		return manifest, nil
	case isMetadataEndpointMissing(err):
		return c.inspectViaTemporaryHandle(ctx, n)
	default:
		return uri.Manifest{}, err
	}
}

// isMetadataEndpointMissing reports whether err is the 404 or 405 that
// means the daemon's generation has no torrents/fetchMetadata — the
// signal to fall back to the temporary-handle path.
func isMetadataEndpointMissing(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) &&
		(apiErr.status == http.StatusNotFound || apiErr.status == http.StatusMethodNotAllowed)
}

// fetchMetadataReply is the torrents/fetchMetadata response. A 202 carries
// only the identity trio; once the metadata is known the same endpoint
// answers 200 with the info object filled in (release-5.2.3
// torrentscontroller.cpp, fetchMetadataAction).
type fetchMetadataReply struct {
	InfohashV1 string              `json:"infohash_v1"`
	InfohashV2 string              `json:"infohash_v2"`
	Hash       string              `json:"hash"`
	Info       *fetchedTorrentInfo `json:"info"`
}

// fetchedTorrentInfo is the nested info object of the completed
// fetchMetadata answer, restricted to the keys the manifest maps.
// Length is the daemon's own total; the manifest sums the file sizes
// instead, so a reply whose two disagree is answered by the files.
type fetchedTorrentInfo struct {
	Name    string               `json:"name"`
	Length  int64                `json:"length"`
	Private *bool                `json:"private"`
	Files   []fetchedTorrentFile `json:"files"`
}

// fetchedTorrentFile is one entry of the info object's file list: a
// relative path and its length in bytes.
type fetchedTorrentFile struct {
	Path   string `json:"path"`
	Length int64  `json:"length"`
}

// inspectViaFetchMetadata drives the primary path: one POST per
// metadataPollInterval until the answer carries the full info object or
// the caller's deadline passes. A call that fails while the caller's
// context has ended is the deadline arriving mid-request, not a daemon
// fault: it reports ErrMetadataTimeout like the poll timeout does, so the
// API layer never mistakes an expired inspection for an engine outage.
func (c *Client) inspectViaFetchMetadata(ctx context.Context, magnet string) (uri.Manifest, error) {
	ticker := time.NewTicker(metadataPollInterval)
	defer ticker.Stop()

	for {
		reply, err := c.postFetchMetadata(ctx, magnet)
		if err != nil {
			if ctx.Err() != nil {
				return uri.Manifest{}, metadataTimeout(ctx.Err())
			}
			return uri.Manifest{}, err
		}
		if reply.Info != nil {
			return manifestFromFetchedInfo(reply)
		}

		select {
		case <-ctx.Done():
			return uri.Manifest{}, metadataTimeout(ctx.Err())
		case <-ticker.C:
		}
	}
}

// postFetchMetadata issues one POST torrents/fetchMetadata. The body's
// shape decides the outcome: the identity trio alone means pending, the
// info object means resolved.
func (c *Client) postFetchMetadata(ctx context.Context, magnet string) (fetchMetadataReply, error) {
	body, err := c.do(ctx, http.MethodPost, pathFetchMetadata, url.Values{formSource: {magnet}})
	if err != nil {
		return fetchMetadataReply{}, err
	}

	var reply fetchMetadataReply
	if err := json.Unmarshal(body, &reply); err != nil {
		return fetchMetadataReply{}, fmt.Errorf("qbittorrent: decode %s: %w", pathFetchMetadata, err)
	}
	return reply, nil
}

// inspectViaTemporaryHandle drives the fallback: one stopped add with
// stopCondition=MetadataReceived, torrents/files polled until the metadata
// arrives, then the manifest read from torrents/info and torrents/files of
// the same handle. The deferred removal is the cleanup contract: it runs
// on every exit path under its own budget, so a cancelled caller still
// removes the handle. The handle bypasses Client.Remove on purpose — that
// method now refuses ids the ownership-filtered cache does not hold, and
// the probe is deliberately never owned.
func (c *Client) inspectViaTemporaryHandle(ctx context.Context, n uri.Normalized) (uri.Manifest, error) {
	expected, err := c.uriTorrentID(ctx, n.URI)
	if err != nil {
		return uri.Manifest{}, err
	}
	if expected == "" {
		// ParseMagnet already guarantees one usable xt, so this is
		// unreachable in practice; it keeps decodeAddResult's pending-add
		// guarantee airtight rather than trusting the parser.
		return uri.Manifest{}, errors.New("qbittorrent: magnet carries no locally resolvable identity")
	}

	id, err := c.addMetadataProbe(ctx, n.URI, expected)
	if err != nil {
		return uri.Manifest{}, err
	}
	hash := ref(id)
	defer c.deleteMetadataProbe(hash)

	ticker := time.NewTicker(metadataPollInterval)
	defer ticker.Stop()

	for {
		files, err := c.Files(ctx, engine.NameQBittorrent+":"+hash)
		switch {
		case err == nil && len(files) > 0:
			return c.temporaryHandleManifest(ctx, hash, files)
		case err == nil:
			// Known torrent, metadata still arriving: the daemon answers
			// torrents/files with an empty list until it lands.
		default:
			if ctx.Err() != nil {
				return uri.Manifest{}, metadataTimeout(ctx.Err())
			}
			if !errors.Is(err, engine.ErrNotFound) {
				return uri.Manifest{}, err
			}
			// A 404 straight after the add: the daemon has not registered
			// the handle yet. Keep polling until the deadline.
		}

		select {
		case <-ctx.Done():
			return uri.Manifest{}, metadataTimeout(ctx.Err())
		case <-ticker.C:
		}
	}
}

// addMetadataProbe adds the magnet as a stopped, metadata-only handle and
// returns the namespaced engine id. The form is built here rather than
// through buildAddForm because the probe's parameters — no savepath, no
// category, nothing but the stop condition — are its own contract, and
// Add's typed fields own every other parameter the adapter sends.
func (c *Client) addMetadataProbe(ctx context.Context, magnet, expected string) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, field := range []addField{
		{"urls", magnet},
		{"stopped", "true"},
		{"paused", "true"},
		{formStopCondition, stopConditionMetadataReceived},
		{"autoTMM", "false"},
	} {
		if err := w.WriteField(field.name, field.value); err != nil {
			return "", fmt.Errorf("qbittorrent: build metadata probe form: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("qbittorrent: build metadata probe form: %w", err)
	}
	payload := buf.Bytes()

	status, body, err := c.authenticated(ctx, pathTorrentsAdd, func() (int, []byte, error) {
		return c.roundTrip(ctx, http.MethodPost, pathTorrentsAdd, nil, w.FormDataContentType(), payload)
	})
	if err != nil {
		return "", err
	}
	return decodeAddResult(status, body, engine.AddRequest{URIs: []string{magnet}}, expected)
}

// deleteMetadataProbe removes the fallback's temporary handle with
// deleteFiles=true under a fresh context, so a cancelled caller still
// triggers the removal. A failure is logged, never swallowed silently and
// never fatal: the manifest may already be resolved, and the daemon's own
// stop condition means nothing was written either way.
func (c *Client) deleteMetadataProbe(hash string) {
	ctx, cancel := context.WithTimeout(context.Background(), metadataCleanupTimeout)
	defer cancel()

	if err := c.removeTorrent(ctx, engine.NameQBittorrent+":"+hash, true); err != nil {
		slog.Warn("qbittorrent: magnet metadata probe handle not removed",
			"engine", engine.NameQBittorrent, "hash", hash, "error", err.Error())
	}
}

// temporaryHandleManifest reads the fallback handle's identity row and
// folds it together with the files the poll already resolved.
func (c *Client) temporaryHandleManifest(ctx context.Context, hash string, files []engine.FileEntry) (uri.Manifest, error) {
	row, err := c.torrentRow(ctx, hash)
	if err != nil {
		return uri.Manifest{}, err
	}

	manifest := uri.Manifest{
		Name:       row.Name,
		InfohashV1: strings.ToLower(row.InfohashV1),
		InfohashV2: strings.ToLower(row.InfohashV2),
		Private:    row.Private,
	}
	manifest.Files = make([]uri.ManifestFile, 0, len(files))
	for _, f := range files {
		size := f.Size
		if manifest.TotalSize > math.MaxInt64-size {
			return uri.Manifest{}, fmt.Errorf("qbittorrent: %s: file sizes overflow int64", pathTorrentsInfo)
		}
		manifest.TotalSize += size
		manifest.Files = append(manifest.Files, uri.ManifestFile{
			Index: f.Index,
			Path:  cleanTorrentPath(f.Path),
			Size:  size,
		})
	}
	return manifest, nil
}

// torrentRow reads one torrent's own row from torrents/info — the keys
// the manifest table names — bypassing the ownership-filtered cache the
// probe is deliberately outside of.
func (c *Client) torrentRow(ctx context.Context, hash string) (torrentJSON, error) {
	body, err := c.do(ctx, http.MethodGet, pathTorrentsInfo, url.Values{"hashes": {hash}})
	if err != nil {
		return torrentJSON{}, err
	}

	var rows []torrentJSON
	if err := json.Unmarshal(body, &rows); err != nil {
		return torrentJSON{}, fmt.Errorf("qbittorrent: decode %s: %w", pathTorrentsInfo, err)
	}
	for _, row := range rows {
		if row.Hash == hash {
			return row, nil
		}
	}
	return torrentJSON{}, fmt.Errorf("qbittorrent: %s: torrent %s not held by the daemon: %w",
		pathTorrentsInfo, hash, engine.ErrNotFound)
}

// manifestFromFetchedInfo maps a completed fetchMetadata answer onto
// uri.Manifest. The infohashes come from the reply's infohash_v1 and
// infohash_v2 keys, never from hash, which for a v2 or hybrid torrent is
// the truncated TorrentID, not the v1 identity (docs/06 section 3.5).
func manifestFromFetchedInfo(reply fetchMetadataReply) (uri.Manifest, error) {
	manifest := uri.Manifest{
		Name:       reply.Info.Name,
		InfohashV1: strings.ToLower(reply.InfohashV1),
		InfohashV2: strings.ToLower(reply.InfohashV2),
		Private:    reply.Info.Private,
	}
	manifest.Files = make([]uri.ManifestFile, 0, len(reply.Info.Files))
	for i, f := range reply.Info.Files {
		if manifest.TotalSize > math.MaxInt64-f.Length {
			return uri.Manifest{}, fmt.Errorf("qbittorrent: %s: file sizes overflow int64", pathFetchMetadata)
		}
		manifest.TotalSize += f.Length
		manifest.Files = append(manifest.Files, uri.ManifestFile{
			Index: i,
			Path:  cleanTorrentPath(f.Path),
			Size:  f.Length,
		})
	}
	return manifest, nil
}

// metadataTimeout renders ErrMetadataTimeout with the context's own cause
// wrapped for diagnosis.
func metadataTimeout(cause error) error {
	return fmt.Errorf("%w: %w", ErrMetadataTimeout, cause)
}

// cleanTorrentPath normalises one daemon-reported path onto the manifest's
// relative form: forward separators, no leading slash, no "." segments. A
// hostile ".." is not flattened here — the create path re-validates every
// resolved path against the data roots before anything touches disk.
func cleanTorrentPath(p string) string {
	if p == "" {
		return ""
	}
	return strings.TrimPrefix(path.Clean(strings.ReplaceAll(p, "\\", "/")), "/")
}
