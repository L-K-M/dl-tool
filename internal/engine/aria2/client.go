package aria2

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/L-K-M/dl-tool/internal/engine"
)

// Config is the adapter's construction input. URL is DLTOOL_ARIA2_URL and
// Secret is DLTOOL_ARIA2_SECRET (docs/11-config-reference.md §2).
type Config struct {
	URL     string        // e.g. http://aria2:6800/jsonrpc
	Secret  string        // sent as the first positional parameter, "token:" + Secret
	Timeout time.Duration // per-call deadline; 0 means defaultCallTimeout
}

const (
	// eventsBuffer lets one tick's events drain without stalling the poll
	// loop on a slow consumer.
	eventsBuffer = 64

	// eventsPollInterval is the tellActive cadence that keeps rates moving
	// between WebSocket notifications (docs/06-download-engines.md §4.5).
	eventsPollInterval = time.Second

	// WebSocket reconnect backoff (§4.5): half a second to start, doubling
	// per consecutive failure, capped at half a minute. A successful
	// connection resets the ladder, so a daemon that flaps recovers at full
	// speed.
	wsBackoffInitial = 500 * time.Millisecond
	wsBackoffMax     = 30 * time.Second

	// wsHandshakeTimeout bounds a WebSocket dial against a silent host.
	wsHandshakeTimeout = 10 * time.Second

	// wsPingWriteTimeout bounds one ping frame write against a stuck socket.
	wsPingWriteTimeout = 5 * time.Second

	// listCount requests all retained records in the required single batch.
	// aria2 parses the count as a signed 64-bit integer.
	listCount int64 = math.MaxInt64

	// defaultCallTimeout backs a zero Config.Timeout so a hung daemon cannot
	// stall a caller forever.
	defaultCallTimeout = 30 * time.Second
)

// wsReadIdle is the default read-idle window of a notification
// connection: a half-open connection — a partition, a NAT timeout, a
// middlebox that drops silently — never delivers a close frame, so without
// a deadline the blocked read would hang the transport until TCP gives up.
// A ping at a third of the window keeps a healthy connection alive and
// turns a dead peer into a read error the reconnect ladder answers (§4.5).
// Per client, snapshotted once per connection: package-level mutable state
// here would be a latent data race between a test's override and the
// production goroutines of every other client in the package.
const wsReadIdle = 90 * time.Second

// JSON-RPC method names dl-tool calls (docs/06-download-engines.md §4.3).
const (
	methodGetVersion           = "aria2.getVersion"
	methodAddURI               = "aria2.addUri"
	methodAddMetalink          = "aria2.addMetalink"
	methodTellActive           = "aria2.tellActive"
	methodTellWaiting          = "aria2.tellWaiting"
	methodTellStopped          = "aria2.tellStopped"
	methodTellStatus           = "aria2.tellStatus"
	methodGetFiles             = "aria2.getFiles"
	methodPause                = "aria2.pause"
	methodUnpause              = "aria2.unpause"
	methodRemove               = "aria2.remove"
	methodRemoveDownloadResult = "aria2.removeDownloadResult"
	methodChangeOption         = "aria2.changeOption"
	methodChangeGlobalOption   = "aria2.changeGlobalOption"
)

// WebSocket notification methods and the TaskEvent kind each maps to
// (docs/06-download-engines.md §4.5).
const (
	notifyDownloadStart      = "aria2.onDownloadStart"
	notifyDownloadPause      = "aria2.onDownloadPause"
	notifyDownloadStop       = "aria2.onDownloadStop"
	notifyDownloadComplete   = "aria2.onDownloadComplete"
	notifyDownloadError      = "aria2.onDownloadError"
	notifyBtDownloadComplete = "aria2.onBtDownloadComplete"
)

// notificationKinds is the six-row table of docs/06-download-engines.md
// §4.5. A message whose method is absent from this map is not one of
// aria2's notifications and is dropped.
var notificationKinds = map[string]engine.EventKind{
	notifyDownloadStart:      engine.EventStarted,
	notifyDownloadPause:      engine.EventPaused,
	notifyDownloadStop:       engine.EventRemoved,
	notifyDownloadComplete:   engine.EventCompleted,
	notifyDownloadError:      engine.EventError,
	notifyBtDownloadComplete: engine.EventProgress,
}

// Option keys: the long option name without the leading --, every value a
// string (docs/06-download-engines.md §4.2).
const (
	optDir                    = "dir"
	optOut                    = "out"
	optPause                  = "pause"
	optSelectFile             = "select-file"
	optMaxDownloadLimit       = "max-download-limit"
	optMaxUploadLimit         = "max-upload-limit"
	optMaxOverallDownloadRate = "max-overall-download-limit"
	optMaxOverallUploadRate   = "max-overall-upload-limit"
)

// blobKindMetalink is engine.AddRequest.BlobKind's metalink value. Torrent
// and nzb blobs belong to other engines and are rejected by Add.
const blobKindMetalink = "metalink"

// clientCapabilities is the declared set, exactly, sorted and stable. CapRename
// is deliberately absent: aria2 names an output only at add time through the
// out option, so Rename on a running transfer is ErrNotSupported and declaring
// the capability would be a lie (T028's contract suite checks this).
var clientCapabilities = []engine.Capability{
	engine.CapFTP,
	engine.CapHTTP,
	engine.CapMetalink,
	engine.CapPerFileSelect,
	engine.CapPushEvents,
	engine.CapSetLocation,
	engine.CapSFTP,
}

// statusKeys is the tellStatus key list dl-tool reads
// (docs/06-download-engines.md §4.4) — exactly the keys statusResult decodes.
// verifiedLength and verifyIntegrityPending must be requested: the checking
// state is detected by their presence, not by status.
var statusKeys = []string{
	"gid", "status", "totalLength", "completedLength", "uploadLength",
	"downloadSpeed", "uploadSpeed", "dir", "files", "errorCode", "errorMessage",
	"infoHash", "numSeeders", "seeder", "connections", "followedBy",
	"verifiedLength", "verifyIntegrityPending",
}

// Client implements engine.Engine over aria2's JSON-RPC 2.0 endpoint for HTTP,
// FTP, SFTP and Metalink transfers.
type Client struct {
	url     string
	secret  string
	timeout time.Duration
	hc      *http.Client

	// readIdle is the notification connection's read-idle window, snapshotted
	// per connection from wsReadIdle; the reconnect test shortens it on the
	// instance it owns, never through package state.
	readIdle time.Duration

	nextID    atomic.Uint64
	done      chan struct{}
	closeOnce sync.Once
}

var _ engine.Engine = (*Client)(nil)

// New returns a Client ready for Connect. It performs no I/O.
func New(cfg Config, hc *http.Client) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("aria2: parse rpc url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("aria2: rpc url %q must be an absolute http(s) url", cfg.URL)
	}

	if hc == nil {
		hc = &http.Client{}
	}
	// The client must never follow redirects: the request body carries
	// the RPC secret, and a 3xx would replay it to whatever host the
	// response names. http.Client has only these four fields, so a
	// shallow copy is complete — injected clients get the rule too.
	hc = &http.Client{
		Transport: hc.Transport,
		Jar:       hc.Jar,
		Timeout:   hc.Timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultCallTimeout
	}

	return &Client{
		url:      cfg.URL,
		secret:   cfg.Secret,
		timeout:  timeout,
		hc:       hc,
		readIdle: wsReadIdle,
		done:     make(chan struct{}),
	}, nil
}

// Name identifies the engine in tasks.engine and in engine task ids.
func (c *Client) Name() string { return engine.NameAria2 }

// Capabilities returns the declared set, sorted and stable.
func (c *Client) Capabilities() []engine.Capability {
	return slices.Clone(clientCapabilities)
}

// Accepts reports whether the URI's scheme is one of aria2's lanes: http(s),
// ftp and sftp (docs/06-download-engines.md §2, rows 4-6). aria2 has no
// ftps scheme, so ftps is not accepted. Metalink files arrive over http(s),
// so no extra row is needed.
func (c *Client) Accepts(uriStr string) bool {
	u, err := url.Parse(uriStr)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "http", "https", "ftp", "sftp":
		return true
	default:
		return false
	}
}

// Connect verifies the daemon answers getVersion. Foreign-transfer detection
// at connect time belongs to the reconciler (T026), which holds the
// engine_ref → task-id map.
func (c *Client) Connect(ctx context.Context) error {
	_, err := c.Health(ctx)
	return err
}

// Close stops the Events polling loop. In-flight calls are unaffected.
func (c *Client) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	return nil
}

// Health returns the daemon version, or ErrUnavailable when unreachable.
func (c *Client) Health(ctx context.Context) (string, error) {
	raw, err := c.call(ctx, methodGetVersion)
	if err != nil {
		return "", err
	}

	var version struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &version); err != nil {
		return "", fmt.Errorf("aria2: decode getVersion: %w: %w", engine.ErrUnavailable, err)
	}
	return version.Version, nil
}

// Add submits one download and returns the namespaced engine task id.
// URIs go to addUri, a metalink blob to addMetalink (first GID recorded);
// any other blob is ErrNotSupported because torrents belong to qBittorrent.
func (c *Client) Add(ctx context.Context, req engine.AddRequest) (string, error) {
	if len(req.Blob) > 0 {
		if req.BlobKind == blobKindMetalink {
			return c.addMetalink(ctx, req)
		}
		return "", engine.ErrNotSupported
	}
	return c.addURI(ctx, req)
}

func (c *Client) addURI(ctx context.Context, req engine.AddRequest) (string, error) {
	if len(req.URIs) == 0 {
		return "", errors.New("aria2: add requires at least one uri")
	}

	raw, err := c.call(ctx, methodAddURI, any(req.URIs), addOptions(req))
	if err != nil {
		return "", err
	}

	var gid string
	if err := json.Unmarshal(raw, &gid); err != nil {
		return "", fmt.Errorf("aria2: decode addUri gid: %w", err)
	}
	return engine.NameAria2 + ":" + gid, nil
}

func (c *Client) addMetalink(ctx context.Context, req engine.AddRequest) (string, error) {
	// addMetalink takes base64 and returns an array of GIDs; the first is
	// the recorded handle and followedBy carries the rest (§4.3).
	blob := base64.StdEncoding.EncodeToString(req.Blob)
	raw, err := c.call(ctx, methodAddMetalink, blob, addOptions(req))
	if err != nil {
		return "", err
	}

	var gids []string
	if err := json.Unmarshal(raw, &gids); err != nil {
		return "", fmt.Errorf("aria2: decode addMetalink gids: %w", err)
	}
	if len(gids) == 0 {
		return "", errors.New("aria2: addMetalink returned no gids")
	}
	return engine.NameAria2 + ":" + gids[0], nil
}

// addOptions encodes the add-time options from an AddRequest: long names,
// string values, absent when unset (§4.2). Extra keys ride along verbatim —
// the engine-specific escape hatch AddRequest documents — with the typed
// fields winning on a collision.
func addOptions(req engine.AddRequest) map[string]string {
	opts := make(map[string]string, len(req.Extra)+4)
	for key, value := range req.Extra {
		opts[key] = value
	}
	if req.SaveDir != "" {
		opts[optDir] = req.SaveDir
	}
	if req.Filename != "" {
		opts[optOut] = req.Filename
	}
	if req.StartPaused {
		opts[optPause] = "true"
	}
	if value := selectFileValue(req.SelectFiles); value != "" {
		opts[optSelectFile] = value
	}
	return opts
}

// selectFileValue converts the engine's 0-based file indices into aria2's
// 1-based comma-separated select-file value, sorted and duplicate-free.
func selectFileValue(selected []int) string {
	if len(selected) == 0 {
		return ""
	}

	indices := append([]int(nil), selected...)
	sort.Ints(indices)
	indices = slices.Compact(indices)

	parts := make([]string, 0, len(indices))
	for _, i := range indices {
		parts = append(parts, strconv.Itoa(i+1))
	}
	return strings.Join(parts, ",")
}

// List returns every GID the daemon reports, as one JSON-RPC batch of
// tellActive, tellWaiting and tellStopped. Foreign transfers are not filtered
// here: this package has no access to the tasks table, and the ownership rule
// of §8 is applied by the reconciler (T026).
func (c *Client) List(ctx context.Context) ([]engine.TaskInfo, error) {
	keys := any(statusKeys)
	batch := []rpcRequest{
		c.newRequest(methodTellActive, []any{keys}),
		c.newRequest(methodTellWaiting, []any{0, listCount, keys}),
		c.newRequest(methodTellStopped, []any{0, listCount, keys}),
	}

	responses, err := c.post(ctx, batch)
	if err != nil {
		return nil, err
	}
	if len(responses) != len(batch) {
		return nil, fmt.Errorf("aria2: rpc batch returned %d replies for %d requests", len(responses), len(batch))
	}

	// JSON-RPC permits arbitrary reply order; correlate by ID, never position.
	byID := make(map[string]rpcReply, len(responses))
	for _, response := range responses {
		if _, duplicate := byID[response.ID]; duplicate {
			return nil, errors.New("aria2: duplicate rpc reply id")
		}
		byID[response.ID] = response
	}

	var infos []engine.TaskInfo
	for _, request := range batch {
		response, found := byID[request.ID]
		if !found {
			return nil, fmt.Errorf("aria2: missing rpc reply for %q", request.ID)
		}
		results, err := decodeStatuses(response)
		if err != nil {
			return nil, err
		}
		for _, r := range results {
			infos = append(infos, toTaskInfo(r))
		}
	}
	return infos, nil
}

// Get returns one task's normalised status.
func (c *Client) Get(ctx context.Context, id string) (engine.TaskInfo, error) {
	result, err := c.getStatus(ctx, id, statusKeys)
	if err != nil {
		return engine.TaskInfo{}, err
	}
	return toTaskInfo(result), nil
}

// getStatus shares decoding while allowing metadata-only queries.
func (c *Client) getStatus(ctx context.Context, id string, keys []string) (statusResult, error) {
	raw, err := c.call(ctx, methodTellStatus, ref(id), keys)
	if err != nil {
		return statusResult{}, err
	}

	var result statusResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return statusResult{}, fmt.Errorf("aria2: decode tellStatus: %w", err)
	}
	return result, nil
}

// Files returns SaveDir-relative paths, 0-based indices and nil priorities.
func (c *Client) Files(ctx context.Context, id string) ([]engine.FileEntry, error) {
	raw, err := c.call(ctx, methodGetFiles, ref(id))
	if err != nil {
		return nil, err
	}

	var entries []fileEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("aria2: decode getFiles: %w", err)
	}
	if len(entries) == 0 {
		return toFileEntries(entries), nil
	}

	// getFiles supplies absolute paths, but Engine.FileEntry requires relative ones.
	status, err := c.getStatus(ctx, id, []string{optDir})
	if err != nil {
		return nil, err
	}
	for index, entry := range entries {
		if entry.Path == "" {
			continue // Metadata may not have resolved a filename yet.
		}
		relative, err := filepath.Rel(status.Dir, entry.Path)
		if err != nil {
			return nil, fmt.Errorf("aria2: make file path relative: %w", err)
		}
		if !filepath.IsLocal(relative) {
			return nil, errors.New("aria2: file path escapes download directory")
		}
		entries[index].Path = filepath.ToSlash(relative)
	}
	return toFileEntries(entries), nil
}

// Pause moves an active download to the front of the waiting queue.
func (c *Client) Pause(ctx context.Context, id string) error {
	return c.simple(ctx, methodPause, ref(id))
}

// Resume moves a paused download back to waiting.
func (c *Client) Resume(ctx context.Context, id string) error {
	return c.simple(ctx, methodUnpause, ref(id))
}

// Remove drops the transfer from aria2, always retaining the payload data.
// aria2.remove faults with "Active Download not found for GID#…" on a download
// that already stopped — the normal case for a completed or failed task — so
// that fault is success here and removeDownloadResult decides whether the GID
// exists at all (§4.3).
func (c *Client) Remove(ctx context.Context, id string) error {
	gid := ref(id)

	if _, err := c.call(ctx, methodRemove, gid); err != nil {
		if !errors.Is(err, engine.ErrNotFound) {
			return err
		}
		// Already stopped, or a GID the daemon has never seen.
	}

	if _, err := c.call(ctx, methodRemoveDownloadResult, gid); err != nil {
		return err
	}
	return nil
}

// SetFiles applies a selection only. aria2 has skip-versus-selected, no
// numeric per-file priority, so a non-nil priorities map is ErrNotSupported
// and no request is sent (§1.1). An empty selection is ErrNotSupported too:
// aria2 cannot express "deselect everything".
func (c *Client) SetFiles(ctx context.Context, id string, selected []int, priorities map[int]int) error {
	if priorities != nil {
		return engine.ErrNotSupported
	}

	value := selectFileValue(selected)
	if value == "" {
		// aria2 requires at least one selected file; "select nothing" is
		// inexpressible, so it is an unsupported capability rather than a
		// silent no-op the caller could mistake for success.
		return engine.ErrNotSupported
	}
	return c.simple(ctx, methodChangeOption, ref(id), map[string]string{optSelectFile: value})
}

// SetLocation changes dir. ⚠ aria2.changeOption restarts an active transfer
// for every option outside its small safe list, and dir is not on it (§4.6);
// the reconciler surfaces that to the UI.
func (c *Client) SetLocation(ctx context.Context, id, path string) error {
	return c.simple(ctx, methodChangeOption, ref(id), map[string]string{optDir: path})
}

// SetRateLimits applies bytes/second limits; 0 means unlimited, a nil
// direction is left unchanged. Both max-*-limit options are on changeOption's
// safe list, so a running transfer is not restarted (§4.6). id == "" sets the
// global limits through changeGlobalOption.
func (c *Client) SetRateLimits(ctx context.Context, id string, down, up *int64) error {
	if id == "" {
		opts := make(map[string]string)
		if down != nil {
			opts[optMaxOverallDownloadRate] = strconv.FormatInt(*down, 10)
		}
		if up != nil {
			opts[optMaxOverallUploadRate] = strconv.FormatInt(*up, 10)
		}
		if len(opts) == 0 {
			return nil
		}
		return c.simple(ctx, methodChangeGlobalOption, any(opts))
	}

	opts := make(map[string]string)
	if down != nil {
		opts[optMaxDownloadLimit] = strconv.FormatInt(*down, 10)
	}
	if up != nil {
		opts[optMaxUploadLimit] = strconv.FormatInt(*up, 10)
	}
	if len(opts) == 0 {
		return nil
	}
	return c.simple(ctx, methodChangeOption, ref(id), any(opts))
}

// Rename is unsupported: aria2 names an output only at add time through the
// out option, and CapRename is not declared.
func (c *Client) Rename(ctx context.Context, id, name string) error {
	return engine.ErrNotSupported
}

// SetCategory is unsupported: aria2 has no category concept.
func (c *Client) SetCategory(ctx context.Context, id, category string) error {
	return engine.ErrNotSupported
}

// SetShareLimits is unsupported: aria2 is never used for BitTorrent seeding.
func (c *Client) SetShareLimits(ctx context.Context, id string, ratio *float64, seedMinutes *int64) error {
	return engine.ErrNotSupported
}

// Events opens the WebSocket notification transport and keeps the 1 Hz
// tellActive progress batch beside it. Notifications carry the state
// changes (§4.5); the poll keeps rates moving between them. The channel
// closes when ctx is cancelled or the client is closed, after both loops
// have stopped — one closer, owned by the supervisor below.
func (c *Client) Events(ctx context.Context) (<-chan engine.TaskEvent, error) {
	events := make(chan engine.TaskEvent, eventsBuffer)

	var loops sync.WaitGroup
	loops.Add(2)
	go func() { defer loops.Done(); c.pollEvents(ctx, events) }()
	go func() { defer loops.Done(); c.notifyEvents(ctx, events) }()
	go func() { loops.Wait(); close(events) }()

	return events, nil
}

// notifyEvents maintains the WebSocket connection on the same host and path
// as the RPC endpoint, reconnecting with exponential backoff on every drop.
// It returns only when ctx is cancelled or the client is closed; a daemon
// that stays down costs one dial per backoff step, nothing more.
func (c *Client) notifyEvents(ctx context.Context, events chan<- engine.TaskEvent) {
	dialer := &websocket.Dialer{HandshakeTimeout: wsHandshakeTimeout}
	backoff := wsBackoffInitial

	for {
		conn, _, err := dialer.DialContext(ctx, c.wsURL(), nil)
		if err == nil {
			backoff = wsBackoffInitial
			stop := c.closeOnDone(ctx, conn)
			readErr := c.readNotifications(ctx, conn, events)
			stop()
			if ctx.Err() != nil {
				return
			}
			// Debug, not warn: a flapping daemon drops the socket often, and
			// the reconnect below is the designed answer (§4.5).
			slog.Debug("aria2: websocket dropped, reconnecting",
				"engine", engine.NameAria2, "error", readErr)
		} else if ctx.Err() != nil {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, wsBackoffMax)
	}
}

// closeOnDone hands one connection's lifetime to a watchdog: a blocked
// ReadMessage observes neither a cancelled context nor a closed client, so
// closing the socket is the only way to unblock the read loop. The
// watchdog owns conn.Close() on all three exits — cancelled, closed, or
// read loop returned on its own — and the returned stop ends it and waits,
// so no goroutine outlives notifyEvents.
func (c *Client) closeOnDone(ctx context.Context, conn *websocket.Conn) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
		case <-c.done:
		case <-done:
		}
		if err := conn.Close(); err != nil {
			slog.Debug("aria2: close websocket", "engine", engine.NameAria2, "error", err)
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// readNotifications consumes one WebSocket connection until it drops: each
// of the six notifications maps to its TaskEvent kind and is emitted with
// the namespaced id. A notification is unidirectional — it carries no id,
// and the client must not respond to it (§4.5) — so this loop never writes
// a data frame; its only writes are ping control frames, which gorilla
// permits concurrently with reads.
func (c *Client) readNotifications(ctx context.Context, conn *websocket.Conn, events chan<- engine.TaskEvent) error {
	readIdle := c.readIdle
	resetDeadline := func() {
		if err := conn.SetReadDeadline(time.Now().Add(readIdle)); err != nil {
			slog.Debug("aria2: set websocket read deadline", "engine", engine.NameAria2, "error", err)
		}
	}
	resetDeadline()
	// A healthy peer answers every ping with a pong, and any pong — this
	// client sends no identifiable pings of its own — proves liveness.
	conn.SetPongHandler(func(string) error { resetDeadline(); return nil })

	ping := time.NewTicker(readIdle / 3)
	stopPing := make(chan struct{})
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		for {
			select {
			case <-ping.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsPingWriteTimeout)); err != nil {
					return
				}
			case <-stopPing:
				return
			}
		}
	}()
	defer func() {
		close(stopPing)
		ping.Stop()
		<-pingDone
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		resetDeadline()

		kind, gid, ok := decodeNotification(data)
		if !ok {
			continue
		}
		event := engine.TaskEvent{TaskID: engine.NameAria2 + ":" + gid, Kind: kind}
		select {
		case events <- event:
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return nil
		}
	}
}

// rpcNotification is one aria2 WebSocket notification: a method name and a
// single-element params array whose struct carries the gid (§4.5). The id
// key is deliberately absent — notifications never carry one.
type rpcNotification struct {
	Method string `json:"method"`
	Params []struct {
		GID string `json:"gid"`
	} `json:"params"`
}

// decodeNotification maps one message onto its event kind and gid. ok is
// false for anything that is not one of the six notifications with a gid:
// an unknown method, a malformed frame or a stray reply.
func decodeNotification(data []byte) (kind engine.EventKind, gid string, ok bool) {
	var n rpcNotification
	if err := json.Unmarshal(data, &n); err != nil {
		return "", "", false
	}
	if len(n.Params) == 0 || n.Params[0].GID == "" {
		return "", "", false
	}

	kind, known := notificationKinds[n.Method]
	if !known {
		return "", "", false
	}

	return kind, n.Params[0].GID, true
}

// wsURL derives the WebSocket endpoint from the configured RPC URL: the
// same host and path with the scheme swapped, http→ws and https→wss, the
// way aria2 serves the notification transport on its RPC path.
func (c *Client) wsURL() string {
	switch {
	case strings.HasPrefix(c.url, "https://"):
		return "wss://" + strings.TrimPrefix(c.url, "https://")
	case strings.HasPrefix(c.url, "http://"):
		return "ws://" + strings.TrimPrefix(c.url, "http://")
	default:
		return c.url
	}
}

func (c *Client) pollEvents(ctx context.Context, events chan<- engine.TaskEvent) {
	ticker := time.NewTicker(eventsPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-ticker.C:
		}

		results, err := c.tellActive(ctx)
		if err != nil {
			// Debug, not warn: a down daemon faults once per tick and the
			// reconciler (T026) owns reporting engine health.
			slog.Debug("aria2: events poll failed", "engine", engine.NameAria2, "error", err)
			continue
		}

		for i := range results {
			info := toTaskInfo(results[i])
			event := engine.TaskEvent{TaskID: info.ID, Kind: engine.EventProgress, Info: &info}
			select {
			case events <- event:
			case <-ctx.Done():
				return
			case <-c.done:
				return
			}
		}
	}
}

func (c *Client) tellActive(ctx context.Context) ([]statusResult, error) {
	raw, err := c.call(ctx, methodTellActive, any(statusKeys))
	if err != nil {
		return nil, err
	}

	var results []statusResult
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, fmt.Errorf("aria2: decode tellActive: %w", err)
	}
	return results, nil
}

// rpcRequest is one JSON-RPC 2.0 call object.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

// rpcReply is one JSON-RPC 2.0 reply, single or batch element.
type rpcReply struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcFault       `json:"error"`
}

// rpcFault is aria2's error object. Every aria2 fault carries code 1; only
// the message distinguishes a missing GID from a malformed parameter (§4.7).
type rpcFault struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// newRequest builds a call object, prepending the secret as "token:<secret>",
// the first positional parameter of every aria2.* method (§4.2).
// system.multicall is never used: batches are posted as JSON arrays instead.
func (c *Client) newRequest(method string, params []any) rpcRequest {
	return rpcRequest{
		JSONRPC: "2.0",
		ID:      "dl-tool-" + strconv.FormatUint(c.nextID.Add(1), 10),
		Method:  method,
		Params:  append([]any{"token:" + c.secret}, params...),
	}
}

// call performs one JSON-RPC request and returns the unwrapped result,
// checking that the reply carries the request's id.
func (c *Client) call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	req := c.newRequest(method, params)
	responses, err := c.post(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(responses) == 0 {
		return nil, fmt.Errorf("aria2: rpc reply missing for request %q", req.ID)
	}
	if responses[0].ID != req.ID {
		return nil, fmt.Errorf("aria2: rpc reply id %q does not match request %q", responses[0].ID, req.ID)
	}
	return responses[0].result()
}

// simple issues one call whose result payload carries no information.
func (c *Client) simple(ctx context.Context, method string, params ...any) error {
	_, err := c.call(ctx, method, params...)
	return err
}

// post sends one request object or one batch array and decodes the matching
// reply shape. A transport failure or a non-200 status is ErrUnavailable.
func (c *Client) post(ctx context.Context, body any) ([]rpcReply, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("aria2: marshal rpc: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("aria2: build rpc request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("aria2: rpc transport: %w: %w", engine.ErrUnavailable, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("aria2: close rpc response body", "engine", engine.NameAria2, "error", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("aria2: rpc status %d: %w", resp.StatusCode, engine.ErrUnavailable)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("aria2: read rpc response: %w: %w", engine.ErrUnavailable, err)
	}

	switch body.(type) {
	case rpcRequest:
		var single rpcReply
		if err := json.Unmarshal(data, &single); err != nil {
			return nil, fmt.Errorf("aria2: decode rpc response: %w: %w", engine.ErrUnavailable, err)
		}
		return []rpcReply{single}, nil
	default:
		var batch []rpcReply
		if err := json.Unmarshal(data, &batch); err != nil {
			return nil, fmt.Errorf("aria2: decode rpc batch response: %w: %w", engine.ErrUnavailable, err)
		}
		return batch, nil
	}
}

// result unwraps one reply, mapping a not-found fault to ErrNotFound and every
// other fault to a plain error.
func (r rpcReply) result() (json.RawMessage, error) {
	if r.Error == nil {
		return r.Result, nil
	}
	if isNotFoundMessage(r.Error.Message) {
		return nil, fmt.Errorf("aria2: rpc fault %d %q: %w", r.Error.Code, r.Error.Message, engine.ErrNotFound)
	}
	return nil, fmt.Errorf("aria2: rpc fault %d: %s", r.Error.Code, r.Error.Message)
}

// isNotFoundMessage matches aria2's gid-missing fault messages, verified
// against release-1.37.0 src/RpcMethodImpl.cc: str2Gid's "GID %s is not
// found" (a gid this session never issued), tellStatus's "No such download
// for GID#%s" and getFiles's "No file data is available for GID#%s"
// (issued, then purged), remove's "Active Download not found for GID#%s"
// (no active or waiting group) and removeDownloadResult's "Could not remove
// download result of GID#%s". Every aria2 fault carries code 1, so the
// message is the only discriminator. Ambiguous faults — "GID#%s cannot be
// removed/paused/unpaused now", "Cannot change option for GID#%s" — fire
// for downloads that exist but are in the wrong state (a completed task, a
// dependency-unresolved group), so they stay generic errors rather than
// being read as not-found (§4.7). Every message names its GID, so a
// "not found" without one stays generic too.
func isNotFoundMessage(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "gid") &&
		(strings.Contains(m, "not found") ||
			strings.Contains(m, "no such download") ||
			strings.Contains(m, "no file data is available") ||
			strings.Contains(m, "could not remove download result"))
}

// decodeStatuses decodes an array of tellStatus results from one reply.
func decodeStatuses(resp rpcReply) ([]statusResult, error) {
	raw, err := resp.result()
	if err != nil {
		return nil, err
	}

	var results []statusResult
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, fmt.Errorf("aria2: decode tellStatus results: %w", err)
	}
	return results, nil
}

// ref strips the engine namespace from an engine task id, accepting both the
// namespaced "aria2:<gid>" and a bare gid.
func ref(id string) string {
	return strings.TrimPrefix(id, engine.NameAria2+":")
}
