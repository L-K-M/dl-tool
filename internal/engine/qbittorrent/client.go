// Package qbittorrent implements the download-engine adapter for the
// qBittorrent WebAPI v2 of docs/06-download-engines.md section 5: the login
// and session cookie, the version probe, torrents/add and the lifecycle
// calls. State normalisation lives in map.go. The adapter is not a complete
// engine.Engine until T038 adds the last methods; it is not registered
// anywhere yet.
package qbittorrent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	infohash_v2 "github.com/anacrolix/torrent/types/infohash-v2"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/uri"
)

// Config is the adapter's construction input. BaseURL is
// DLTOOL_QBITTORRENT_URL, Username and Password the DLTOOL_QBITTORRENT_*
// credentials of docs/11-config-reference.md section 2.
type Config struct {
	BaseURL  string // e.g. http://qbittorrent:8080 — no trailing slash, no /api/v2 suffix
	Username string
	Password string
	Timeout  time.Duration // per-call deadline; 0 means defaultCallTimeout
}

const (
	// apiPrefix fronts every WebAPI method: /api/v2/APIName/methodName.
	apiPrefix = "/api/v2/"

	pathLogin       = "auth/login"
	pathVersion     = "app/version"
	pathWebapiVer   = "app/webapiVersion"
	pathTorrentsAdd = "torrents/add"
	pathTorrentsDel = "torrents/delete"

	// torrentMIME is the wire type of a raw .torrent upload.
	torrentMIME = "application/x-bittorrent"

	// uploadFilename names the single torrents file part. The name carries
	// no meaning for the daemon; it only makes the part a file upload.
	uploadFilename = "file.torrent"

	blobKindTorrent = "torrent"

	// formURLEncoded is the body type of every non-multipart POST.
	formURLEncoded = "application/x-www-form-urlencoded"

	// etaSentinel is qBittorrent's "unknown ETA" value: 100 days in
	// seconds.
	etaSentinel = int64(8640000)

	// defaultCallTimeout backs a zero Config.Timeout so a hung daemon
	// cannot stall a caller forever.
	defaultCallTimeout = 30 * time.Second

	// maxResponseBytes caps one response body. A misconfigured base URL
	// can point at an arbitrary HTTP server; the adapter must never buffer
	// an unbounded reply.
	maxResponseBytes = 64 << 20

	// v1TorrentHexChars and v2TorrentHexChars are the hex lengths of the
	// two infohash shapes: 20-byte SHA-1, 32-byte SHA-256.
	v1TorrentHexChars = 40
	v2TorrentHexChars = 64
)

// lifecyclePair is one daemon generation's stop/start endpoint spelling.
// 5.x renamed torrents/pause|resume to torrents/stop|start with no alias:
// the 4.x spelling answers 404 on 5.x and vice versa (docs/06 section 5.7).
type lifecyclePair struct {
	stop, start string
}

var (
	lifecycle5x = lifecyclePair{stop: "torrents/stop", start: "torrents/start"}
	lifecycle4x = lifecyclePair{stop: "torrents/pause", start: "torrents/resume"}
)

// renamedAddParams lists torrents/add parameters whose name changed between
// qBittorrent 4.x/5.2.x and master: skip_checking became seedMode there
// (both are read; 5.2.3 knows only skip_checking), and contentLayout
// replaced root_folder. Both sides ignore unknown parameters, so every
// pair is sent under both names with one value: stopped/paused comes from
// AddRequest.StartPaused, the two pairs below ride on AddRequest.Extra,
// verbatim and only under these names. Other Extra keys are not forwarded:
// the typed AddRequest fields own every other parameter the adapter sends.
var renamedAddParams = [][2]string{
	{"skip_checking", "seedMode"},
	{"contentLayout", "root_folder"},
}

// clientCapabilities is the declared set, exactly, sorted and stable
// (docs/06-download-engines.md section 1 table).
var clientCapabilities = []engine.Capability{
	engine.CapBitTorrent,
	engine.CapBTV2,
	engine.CapCategories,
	engine.CapMagnet,
	engine.CapPerFilePriority,
	engine.CapPerFileSelect,
	engine.CapRename,
	engine.CapSequential,
	engine.CapSetLocation,
	engine.CapShareLimits,
	engine.CapTags,
}

// Client talks to one qBittorrent daemon over its WebAPI v2. It is safe for
// concurrent use.
type Client struct {
	base    string // trimmed base URL, no trailing slash; every path hangs off it
	baseURL *url.URL
	referer string // scheme://host[:port] of the base URL, sent on every call
	auth    struct {
		username string
		password string
	}
	timeout time.Duration
	hc      *http.Client
	jar     http.CookieJar

	mu      sync.Mutex
	version string         // cached GET app/version body, e.g. "v5.2.3"
	webapi  string         // cached GET app/webapiVersion body, e.g. "2.15.1"
	life    *lifecyclePair // nil until the daemon's spelling is probed
}

// New returns a Client ready for Connect. It performs no I/O. The injected
// http.Client keeps its transport and timeout; a nil cookie jar is replaced
// with one, because the session cookie must live in a standards-compliant
// jar (docs/06 section 5.2).
func New(cfg Config, hc *http.Client) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("qbittorrent: parse base url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("qbittorrent: base url %q must be an absolute http(s) url", cfg.BaseURL)
	}
	if strings.HasSuffix(u.Path, "/api/v2") || strings.HasSuffix(u.Path, "/api/v2/") {
		return nil, fmt.Errorf("qbittorrent: base url %q must not carry the %s prefix's suffix", cfg.BaseURL, apiPrefix)
	}

	if hc == nil {
		hc = &http.Client{}
	}
	jar := hc.Jar
	if jar == nil {
		jar, err = cookiejar.New(nil)
		if err != nil {
			return nil, fmt.Errorf("qbittorrent: create cookie jar: %w", err)
		}
		// Copy the whole client rather than rebuild it field by field, so
		// a caller's CheckRedirect and any other setting survive; the jar is
		// the only field that changes.
		cpy := *hc
		cpy.Jar = jar
		hc = &cpy
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultCallTimeout
	}

	c := &Client{
		base:    strings.TrimRight(cfg.BaseURL, "/"),
		baseURL: u,
		referer: u.Scheme + "://" + u.Host,
		timeout: timeout,
		hc:      hc,
		jar:     jar,
	}
	c.auth.username = cfg.Username
	c.auth.password = cfg.Password
	return c, nil
}

// Name identifies the engine in tasks.engine and in engine task ids.
func (c *Client) Name() string { return engine.NameQBittorrent }

// Capabilities returns the declared set, sorted and stable.
func (c *Client) Capabilities() []engine.Capability {
	return slices.Clone(clientCapabilities)
}

// Accepts reports whether the URI is qBittorrent's lane: a magnet, a URL
// whose path ends .torrent, or a bare 40- or 64-hex infohash — row 2 of the
// routing table (docs/06-download-engines.md section 2).
func (c *Client) Accepts(uriStr string) bool {
	s := strings.TrimSpace(uriStr)

	if scheme, _, found := strings.Cut(s, ":"); found && strings.EqualFold(scheme, "magnet") {
		return true
	}
	if isBareInfohash(s) {
		return true
	}

	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.HasSuffix(strings.ToLower(u.Path), ".torrent")
}

// isBareInfohash reports whether s is exactly a 40- or 64-character hex
// string, upper or lower case.
func isBareInfohash(s string) bool {
	if len(s) != v1TorrentHexChars && len(s) != v2TorrentHexChars {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isHexDigit := ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
		if !isHexDigit {
			return false
		}
	}
	return true
}

// Connect logs in and probes both versions. Until it returns nil, no call is
// authenticated; after it, the version cache serves Health without I/O.
func (c *Client) Connect(ctx context.Context) error {
	if err := c.login(ctx); err != nil {
		return err
	}

	version, err := c.do(ctx, http.MethodGet, pathVersion, nil)
	if err != nil {
		return err
	}
	webapi, err := c.do(ctx, http.MethodGet, pathWebapiVer, nil)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.version = strings.TrimSpace(string(version))
	c.webapi = strings.TrimSpace(string(webapi))
	return nil
}

// Close releases the client. The sync/maindata poll arrives with T030; no
// long-lived component exists yet, so this is a no-op today.
func (c *Client) Close() error { return nil }

// Health returns the cached app/version, re-probing when the cache is empty.
// Any failure is engine.ErrUnavailable: a daemon that cannot serve
// app/version is not usable.
func (c *Client) Health(ctx context.Context) (string, error) {
	c.mu.Lock()
	version := c.version
	c.mu.Unlock()
	if version != "" {
		return version, nil
	}

	body, err := c.do(ctx, http.MethodGet, pathVersion, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %w", engine.ErrUnavailable, err)
	}
	version = strings.TrimSpace(string(body))

	c.mu.Lock()
	c.version = version
	c.mu.Unlock()
	return version, nil
}

// Add submits one magnet, .torrent URL or raw .torrent blob and returns the
// namespaced engine task id. Identity comes from added_torrent_ids, never
// from a torrents/info diff; a pending (202) add returns the identity
// resolved before the submission, which T030 reconciles once the daemon
// reports the torrent (docs/06 section 5.3).
func (c *Client) Add(ctx context.Context, req engine.AddRequest) (string, error) {
	if len(req.Blob) > 0 && req.BlobKind != blobKindTorrent {
		return "", engine.ErrNotSupported
	}
	if len(req.Blob) == 0 && len(req.URIs) == 0 {
		return "", errors.New("qbittorrent: add requires a uri or a torrent blob")
	}

	expected, err := expectedTorrentID(req)
	if err != nil {
		return "", err
	}

	payload, contentType, err := buildAddForm(req)
	if err != nil {
		return "", err
	}

	status, body, err := c.authenticated(ctx, pathTorrentsAdd, func() (int, []byte, error) {
		return c.roundTrip(ctx, http.MethodPost, pathTorrentsAdd, nil, contentType, payload)
	})
	if err != nil {
		switch status {
		case http.StatusConflict:
			return "", fmt.Errorf("qbittorrent: torrents/add: every submitted torrent failed: %w", err)
		case http.StatusUnsupportedMediaType:
			return "", fmt.Errorf("qbittorrent: torrents/add: uploaded file is not valid torrent metadata: %w", err)
		}
		return "", err
	}
	return decodeAddResult(status, body, req, expected)
}

// expectedTorrentID resolves the TorrentID the daemon will key on, before
// the submission; "" when it cannot be known locally (a .torrent URL the
// daemon has to fetch). Verified against the pinned sources: libtorrent's
// info_hash_t::get_best() (include/libtorrent/info_hash.hpp, RC_2_0) returns
// the 40-hex truncation of the v2 hash whenever one exists — hybrid
// included — and qBittorrent 5.2.3's InfoHash::toTorrentID()
// (src/base/bittorrent/infohash.cpp) is exactly that value. docs/06
// section 3.5's table says hybrid mirrors infohash_v1, which the daemon
// contradicts; the daemon wins here, and T100 owns the fixture-level
// confirmation.
func expectedTorrentID(req engine.AddRequest) (string, error) {
	if len(req.Blob) > 0 {
		return blobTorrentID(req.Blob)
	}
	if len(req.URIs) != 1 {
		// Zero URIs is already rejected by Add; more than one URI is
		// several torrents, so only the reply can name the returned one.
		return "", nil
	}
	return uriTorrentID(req.URIs[0])
}

// uriTorrentID resolves the TorrentID of one submitted URI.
func uriTorrentID(raw string) (string, error) {
	if isBareInfohash(raw) {
		return truncateV2(strings.ToLower(raw)), nil
	}

	if scheme, _, found := strings.Cut(raw, ":"); !found || !strings.EqualFold(scheme, "magnet") {
		return "", nil // a .torrent URL: only the daemon can resolve it
	}

	n, err := uri.ParseMagnet(raw)
	if err != nil {
		return "", err
	}
	switch {
	case n.InfohashV2 != "":
		return truncateV2(n.InfohashV2), nil
	case n.InfohashV1 != "":
		return n.InfohashV1, nil
	default:
		return "", nil
	}
}

// truncateV2 halves a 64-hex v2 infohash to the 40-hex TorrentID form and
// passes a 40-hex value through.
func truncateV2(v2Hex string) string {
	if len(v2Hex) > v1TorrentHexChars {
		return v2Hex[:v1TorrentHexChars]
	}
	return v2Hex
}

// blobTorrentID hashes the raw info dict exactly as it appears in the file —
// never re-encoded (docs/06 section 3.4) — through the pinned metainfo
// parser, which preserves the original bytes.
func blobTorrentID(blob []byte) (string, error) {
	mi, err := metainfo.Load(bytes.NewReader(blob))
	if err != nil {
		return "", fmt.Errorf("qbittorrent: parse torrent blob: %w", err)
	}

	info, err := mi.UnmarshalInfo()
	if err != nil {
		return "", fmt.Errorf("qbittorrent: decode torrent info: %w", err)
	}

	switch {
	case info.HasV2():
		v2 := infohash_v2.HashBytes(mi.InfoBytes)
		return v2.ToShort().HexString(), nil
	case info.HasV1():
		return mi.HashInfoBytes().HexString(), nil
	default:
		return "", errors.New("qbittorrent: torrent blob carries neither a v1 nor a v2 payload")
	}
}

// addField is one name/value pair of a multipart form, in send order.
type addField struct{ name, value string }

// buildAddForm serialises the torrents/add multipart body: urls for URIs
// newline-separated, one torrents part per blob, then the shared options.
// Both spellings of every renamed parameter are sent with the same value;
// unknown parameters are ignored by the daemon (docs/06 section 5.3).
func buildAddForm(req engine.AddRequest) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	if len(req.Blob) > 0 {
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition",
			fmt.Sprintf(`form-data; name="torrents"; filename="%s"`, uploadFilename))
		header.Set("Content-Type", torrentMIME)
		part, err := w.CreatePart(header)
		if err != nil {
			return nil, "", fmt.Errorf("qbittorrent: build add form: %w", err)
		}
		if _, err := part.Write(req.Blob); err != nil {
			return nil, "", fmt.Errorf("qbittorrent: build add form: %w", err)
		}
	}

	paused := strconv.FormatBool(req.StartPaused)
	fields := []addField{
		{"savepath", req.SaveDir},
		{"autoTMM", "false"},
		{"stopped", paused},
		{"paused", paused},
	}
	if len(req.URIs) > 0 {
		fields = append(fields, addField{"urls", strings.Join(req.URIs, "\n")})
	}
	if req.Category != "" {
		fields = append(fields, addField{"category", req.Category})
	}
	if len(req.Tags) > 0 {
		fields = append(fields, addField{"tags", strings.Join(req.Tags, ",")})
	}
	if req.Sequential {
		fields = append(fields, addField{"sequentialDownload", "true"})
	}
	if req.Filename != "" {
		fields = append(fields, addField{"rename", req.Filename})
	}
	for _, pair := range renamedAddParams {
		if value, ok := req.Extra[pair[0]]; ok {
			fields = append(fields, addField{pair[0], value}, addField{pair[1], value})
		}
	}

	for _, f := range fields {
		if err := w.WriteField(f.name, f.value); err != nil {
			return nil, "", fmt.Errorf("qbittorrent: build add form: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("qbittorrent: build add form: %w", err)
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// addResult is the JSON body WebAPI 2.14+ answers torrents/add with.
// success_count always equals len(added_torrent_ids): the daemon counts the
// ids it appends (release-5.2.3 torrentscontroller.cpp, addAction).
type addResult struct {
	SuccessCount    int      `json:"success_count"`
	PendingCount    int      `json:"pending_count"`
	FailureCount    int      `json:"failure_count"`
	AddedTorrentIDs []string `json:"added_torrent_ids"`
}

// decodeAddResult applies the documented outcome of docs/06 section 5.3.
// Success is never inferred from a 2xx status alone: malformed JSON,
// inconsistent counts, an unexpected id, or an immediate single add that
// returned no id are protocol errors.
func decodeAddResult(status int, body []byte, req engine.AddRequest, expected string) (string, error) {
	if status != http.StatusOK && status != http.StatusAccepted {
		// 409 and 415 are mapped by Add before this runs.
		return "", fmt.Errorf("qbittorrent: torrents/add: unexpected status %d", status)
	}

	var res addResult
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("qbittorrent: decode torrents/add result: %w", err)
	}

	submitted := len(req.URIs)
	if len(req.Blob) > 0 {
		submitted++
	}
	if res.SuccessCount+res.PendingCount+res.FailureCount != submitted {
		return "", fmt.Errorf(
			"qbittorrent: torrents/add counted %d+%d+%d outcomes for %d submitted torrents",
			res.SuccessCount, res.PendingCount, res.FailureCount, submitted)
	}
	if res.SuccessCount != len(res.AddedTorrentIDs) {
		return "", fmt.Errorf("qbittorrent: torrents/add success_count %d with %d added ids",
			res.SuccessCount, len(res.AddedTorrentIDs))
	}
	if submitted == 1 && (len(res.AddedTorrentIDs) > 1 ||
		(status == http.StatusOK && len(res.AddedTorrentIDs) != 1)) {
		// A single submission never names more than one id, and 200 means
		// at least one immediate success with nothing pending (06 section
		// 5.3), so it must name exactly one.
		return "", fmt.Errorf("qbittorrent: torrents/add named %d ids for a single submission",
			len(res.AddedTorrentIDs))
	}
	if len(res.AddedTorrentIDs) == 1 && expected != "" && res.AddedTorrentIDs[0] != expected {
		return "", fmt.Errorf("qbittorrent: torrents/add returned id %s, want %s",
			res.AddedTorrentIDs[0], expected)
	}

	// The engine reference is the id the daemon named, else the identity
	// resolved before the add (a pending 202 add).
	id := expected
	if len(res.AddedTorrentIDs) > 0 {
		id = res.AddedTorrentIDs[0]
	}
	if id == "" {
		return "", errors.New("qbittorrent: pending add of a uri whose identity is not locally resolvable")
	}
	return engine.NameQBittorrent + ":" + id, nil
}

// Pause stops a torrent, probing the 5.x spelling first and caching whichever
// pair the daemon answers (docs/06 section 5.7).
func (c *Client) Pause(ctx context.Context, id string) error {
	return c.lifecycle(ctx, id, stopOf)
}

// Resume starts a stopped torrent through the same probed pair as Pause.
func (c *Client) Resume(ctx context.Context, id string) error {
	return c.lifecycle(ctx, id, startOf)
}

func stopOf(p lifecyclePair) string  { return p.stop }
func startOf(p lifecyclePair) string { return p.start }

// lifecycle issues one stop/start call. The first call probes the 5.x
// spelling; a 404 falls back to the 4.x one once, and whichever answers is
// cached, so every later call goes straight to the daemon's own pair.
func (c *Client) lifecycle(ctx context.Context, id string, pick func(lifecyclePair) string) error {
	c.mu.Lock()
	known := c.life
	c.mu.Unlock()
	if known != nil {
		_, err := c.do(ctx, http.MethodPost, pick(*known), hashesForm(ref(id)))
		return err
	}

	_, err := c.do(ctx, http.MethodPost, pick(lifecycle5x), hashesForm(ref(id)))
	switch {
	case err == nil:
		c.rememberLifecycle(lifecycle5x)
		return nil
	case isEndpointMissing(err):
		// No alias exists in 5.x: the other spelling is the 4.x daemon.
	default:
		return err
	}

	if _, err := c.do(ctx, http.MethodPost, pick(lifecycle4x), hashesForm(ref(id))); err != nil {
		return err
	}
	c.rememberLifecycle(lifecycle4x)
	return nil
}

// rememberLifecycle caches the pair the daemon answers.
func (c *Client) rememberLifecycle(pair lifecyclePair) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.life = &pair
}

// Remove deletes a torrent, optionally with its data. engine.Engine's
// Remove(id) always retains data; the deleteData switch exists for the
// remove-with-data task action, the only caller allowed to pass true
// (docs/06 section 5.7).
func (c *Client) Remove(ctx context.Context, id string, deleteData bool) error {
	form := hashesForm(ref(id))
	form.Set("deleteFiles", strconv.FormatBool(deleteData))
	_, err := c.do(ctx, http.MethodPost, pathTorrentsDel, form)
	return err
}

// hashesForm is the hashes= carrier every torrents mutation takes.
func hashesForm(hash string) url.Values {
	return url.Values{"hashes": {hash}}
}

// ref strips the engine namespace from an engine task id, accepting both the
// namespaced "qbittorrent:<hash>" and a bare hash.
func ref(id string) string {
	return strings.TrimPrefix(id, engine.NameQBittorrent+":")
}

// login posts the credentials and proves the session works. Neither the
// login status nor its body is proof (docs/06 section 5.2): success requires
// the jar to hold a cookie — its name is not part of the contract, 5.2.3
// names it QBT_SID_<WebUI port> — and an authenticated GET app/version
// answering 200.
func (c *Client) login(ctx context.Context) error {
	form := url.Values{"username": {c.auth.username}, "password": {c.auth.password}}
	body := []byte(form.Encode())

	status, _, err := c.roundTrip(ctx, http.MethodPost, pathLogin, nil, formURLEncoded, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		// 401 bad credentials, 403 banned IP: both refuse the session.
		return fmt.Errorf("qbittorrent: login status %d: %w", status, engine.ErrUnavailable)
	}

	if len(c.jar.Cookies(c.baseURL)) == 0 {
		return fmt.Errorf("qbittorrent: login set no session cookie: %w", engine.ErrUnavailable)
	}

	status, _, err = c.roundTrip(ctx, http.MethodGet, pathVersion, nil, "", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("qbittorrent: session probe status %d: %w", status, engine.ErrUnavailable)
	}
	return nil
}

// do performs one authenticated call: POST when mutating, GET otherwise.
// A POST's form is the urlencoded body — never the query, so a password or
// session secret cannot leak into a URL — and a GET's form is the query.
// A 401 or 403 re-logs in and retries exactly once, never more.
func (c *Client) do(ctx context.Context, method, apiPath string, form url.Values) ([]byte, error) {
	var (
		query       url.Values
		body        []byte
		contentType string
	)
	if method != http.MethodGet && len(form) > 0 {
		body = []byte(form.Encode())
		contentType = formURLEncoded
	} else {
		query = form
	}

	_, responseBody, err := c.authenticated(ctx, apiPath, func() (int, []byte, error) {
		return c.roundTrip(ctx, method, apiPath, query, contentType, body)
	})
	return responseBody, err
}

// authenticated runs one API call and, when the session is refused, re-logs
// in and retries exactly once — a loop is impossible because login itself
// never comes through here. The 401 body is logged: a narrowed
// ServerDomains or a Host port mismatch is indistinguishable from an expired
// session otherwise (docs/06 section 5.2).
func (c *Client) authenticated(ctx context.Context, apiPath string, call func() (int, []byte, error)) (int, []byte, error) {
	status, body, err := call()
	if err != nil {
		return 0, nil, err
	}

	if isAuthRefusal(status) {
		if status == http.StatusUnauthorized {
			slog.Warn("qbittorrent: session rejected", "engine", engine.NameQBittorrent,
				"path", apiPath, "body", strings.TrimSpace(string(body)))
		}
		if err := c.login(ctx); err != nil {
			return 0, nil, err
		}

		status, body, err = call()
		if err != nil {
			return 0, nil, err
		}
		if isAuthRefusal(status) {
			return status, body, fmt.Errorf("qbittorrent: %s: session refused after re-login: %w",
				apiPath, engine.ErrUnavailable)
		}
	}

	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return status, body, &apiError{path: apiPath, status: status, body: strings.TrimSpace(string(body))}
	}
	return status, body, nil
}

// isAuthRefusal reports whether a status means the session was rejected.
func isAuthRefusal(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// apiError is one WebAPI failure reply: a status and its body.
type apiError struct {
	path   string
	status int
	body   string
}

func (e *apiError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("qbittorrent: %s: status %d", e.path, e.status)
	}
	return fmt.Sprintf("qbittorrent: %s: status %d: %s", e.path, e.status, e.body)
}

// isEndpointMissing reports whether err is a 404 from an endpoint the
// daemon's generation does not have — the signal to try the other
// stop/start spelling.
func isEndpointMissing(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound
}

// roundTrip performs one HTTP exchange: one API path, the Referer header,
// the per-call timeout. It never retries and never inspects the status;
// classification belongs to the callers. A transport failure wraps
// engine.ErrUnavailable.
func (c *Client) roundTrip(ctx context.Context, method, apiPath string, query url.Values, contentType string, body []byte) (int, []byte, error) {
	target := c.base + apiPrefix + apiPath
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("qbittorrent: build request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// A matching Referer is required by 4.x and correct on every version
	// (docs/06 section 5.2).
	req.Header.Set("Referer", c.referer)

	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("qbittorrent: %s transport: %w: %w", apiPath, engine.ErrUnavailable, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("qbittorrent: close response body", "engine", engine.NameQBittorrent, "error", err)
		}
	}()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return 0, nil, fmt.Errorf("qbittorrent: %s read response: %w: %w", apiPath, engine.ErrUnavailable, err)
	}
	if len(data) > maxResponseBytes {
		// A capped read is a truncated body; parse errors downstream would
		// only obscure the cause, so fail here instead.
		return 0, nil, fmt.Errorf("qbittorrent: %s response exceeds %d bytes: %w",
			apiPath, maxResponseBytes, engine.ErrUnavailable)
	}
	return resp.StatusCode, data, nil
}
