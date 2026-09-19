// Package rss owns feed polling: the conditional GET, the Sonarr backoff
// ladder and the scheduling writes of docs/08-rss-automation.md section 2.
// Item parsing (T067) and rule evaluation (T071) plug in behind it.
package rss

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

// Version stamps the poll's User-Agent; cmd/dl-tool overrides it with the
// linker-stamped build version before NewPoller, exactly like api.Version.
var Version = "dev"

// The request constants of docs/08-rss-automation.md section 2.1 and 2.3,
// verbatim. maxBody is the feed-poll body cap — section 2.4 of
// docs/12-security-and-threat-model.md hands the value to doc 08.
const (
	userAgent    = "dl-tool/%s (+https://github.com/L-K-M/dl-tool)"
	acceptHeader = "application/rss+xml, application/atom+xml, application/xml;q=0.9, text/xml;q=0.9, */*;q=0.5"
	acceptEnc    = "gzip, deflate"
	maxBody      = 16 << 20 // 16 MiB
	totalTimeout = 60 * time.Second
	startupGrace = 15 * time.Minute
	pollParallel = 4
)

const (
	// settingRSSIntervalS is the settings key of the global poll interval
	// (docs/11-config-reference.md); its documented default applies when no
	// row exists — the migration seeds none.
	settingRSSIntervalS    = "rss_interval_s"
	defaultGlobalIntervalS = 1800

	// dueFeedLimit bounds one PollDue pass. The cron entry fires every
	// minute and leftovers stay due, so the cap only paces a feed fleet
	// far larger than any real deployment; it is never a correctness gate.
	dueFeedLimit = 256

	// maxSkipScan bounds the skipHours/skipDays search: a feed that marks
	// every hour and every day skipped cannot loop the reschedule forever —
	// after a week of hours the unadjusted candidate is used.
	maxSkipScan = 24 * 7
)

// BackoffPeriods is Sonarr's escalation ladder in seconds, verbatim from
// docs/08-rss-automation.md section 2.1.
var BackoffPeriods = [10]int64{0, 60, 300, 900, 1800, 3600, 10800, 21600, 43200, 86400}

// ItemParser turns one fetched body into rows ready for upsert. T067
// implements it in parse.go; this package compiles and tests against a stub.
type ItemParser interface {
	ParseFeed(feedID, baseURL string, body []byte) (FeedMeta, []store.FeedItem, error)
}

// FeedMeta carries the channel-level publisher hints of
// docs/08-rss-automation.md section 2.2.
type FeedMeta struct {
	Title            string
	TTLMinutes       int // <ttl>, already clamped to [5,1440]; 0 when absent
	ImpliedIntervalS int // sy:updatePeriod / sy:updateFrequency; 0 when absent
	SkipHours        []int
	SkipDays         []time.Weekday
}

// Result is one poll outcome and the body of POST /feeds/{id}/refresh
// (docs/05-api-contract.md section 10.1).
type Result struct {
	Fetched     bool   `json:"fetched"`
	NotModified bool   `json:"not_modified"`
	ItemsAdded  int    `json:"items_added"`
	ElapsedMS   int64  `json:"elapsed_ms"`
	Error       string `json:"error,omitempty"`
}

// Poller fetches feeds through the shared SSRF-guarded client. It owns no
// http.Client of its own and carries no state that survives a restart — the
// feeds table holds every scheduler fact.
type Poller struct {
	db        *sqlx.DB
	hc        *http.Client
	parser    ItemParser
	log       *slog.Logger
	now       func() time.Time
	startedAt time.Time
}

// NewPoller takes the shared SSRF-guarded client built in internal/secure;
// the poller never constructs an http.Client of its own.
func NewPoller(db *sqlx.DB, hc *http.Client, p ItemParser, log *slog.Logger, now func() time.Time) *Poller {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}

	return &Poller{db: db, hc: hc, parser: p, log: log, now: now, startedAt: now()}
}

// Jitter is deterministic per feed:
// ((int64(fnv1a(feedID)) % 2001) - 1000) / 10000.0, in [-0.10,+0.10]. The
// modulo runs on the unsigned hash so the result stays inside the documented
// band for every id.
func Jitter(feedID string) float64 {
	h := fnv.New64a()
	if _, err := h.Write([]byte(feedID)); err != nil {
		return 0 // hash.Hash.Write cannot fail
	}

	return float64(int64(h.Sum64()%2001)-1000) / 10000.0
}

// EffectiveInterval is max(configured, ttlMinutes*60, impliedIntervalS, 300)
// seconds, where configured is feeds.refresh_interval_s or, when that is 0,
// the rss_interval_s setting (docs/08-rss-automation.md section 2.1).
func EffectiveInterval(configuredS, globalS int, m FeedMeta) time.Duration {
	seconds := configuredS
	if seconds == 0 {
		seconds = globalS
	}
	seconds = max(seconds, m.TTLMinutes*60, m.ImpliedIntervalS, 300)

	return time.Duration(seconds) * time.Second
}

// NextFetchAt is now + EffectiveInterval * (1 + Jitter(feedID)), in unix
// milliseconds.
func NextFetchAt(now int64, d time.Duration, feedID string) int64 {
	return now + int64(float64(d.Milliseconds())*(1+Jitter(feedID)))
}

// Poll fetches one feed. force skips disabled_till and is set by the refresh
// endpoint; a scheduled poll of a still-disabled feed does nothing. A fetch,
// parse or status failure is recorded on the ladder and reported through
// Result.Error — the error return is reserved for failures to record the
// outcome itself, so the refresh endpoint can answer 200 with error set.
func (p *Poller) Poll(ctx context.Context, f store.Feed, force bool) (res Result, err error) {
	now := p.now().UnixMilli()
	defer func() {
		res.ElapsedMS = p.now().UnixMilli() - now
	}()

	if !force && f.DisabledTill != nil && *f.DisabledTill > now {
		// DueFeeds already excludes this feed; the check here keeps a
		// direct Poll call honest too.
		return res, nil
	}

	globalS, err := p.globalInterval(ctx)
	if err != nil {
		return res, err
	}

	fetchCtx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()
	resp, err := p.request(fetchCtx, f)
	if err != nil {
		res.Error = secure.RedactError(err).Error()
		return res, p.recordFailure(ctx, f, now, globalS, res.Error)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			p.log.WarnContext(ctx, "feed response body close failed", "feed_id", f.ID, "err", cerr)
		}
	}()

	res.Fetched = true
	switch resp.StatusCode {
	case http.StatusNotModified:
		res.NotModified = true
		return res, p.recordSuccess(ctx, f, now, globalS, storedMeta(f))

	case http.StatusOK:
		return p.completeFetch(ctx, f, resp, &res, now, globalS)

	default:
		res.Error = fmt.Sprintf("HTTP %d from %s", resp.StatusCode, secure.RedactURL(f.URL))
		return res, p.recordFailure(ctx, f, now, globalS, res.Error)
	}
}

// completeFetch handles the 200 path: capped body read, parse, upsert, trim,
// then the success bookkeeping with the fresh validators and publisher hints.
func (p *Poller) completeFetch(ctx context.Context, f store.Feed, resp *http.Response, res *Result, now int64, globalS int) (Result, error) {
	body, err := readCapped(resp)
	if err != nil {
		res.Error = err.Error()
		return *res, p.recordFailure(ctx, f, now, globalS, res.Error)
	}

	if p.parser == nil {
		// Interim until T067 wires parse.go: the fetch succeeded but no
		// items can be extracted. Failing the poll keeps the feed visibly
		// broken instead of silently reporting zero items forever.
		res.Error = "feed parser is not wired yet (T067)"
		return *res, p.recordFailure(ctx, f, now, globalS, res.Error)
	}

	meta, items, err := p.parser.ParseFeed(f.ID, f.URL, body)
	if err != nil {
		res.Error = fmt.Sprintf("feed parse failed: %s", err.Error())
		return *res, p.recordFailure(ctx, f, now, globalS, res.Error)
	}

	added, err := store.UpsertFeedItems(ctx, p.db, items, now)
	if err != nil {
		return *res, fmt.Errorf("rss: upsert items of feed %s: %w", f.ID, err)
	}
	if _, err := store.TrimFeedItems(ctx, p.db, f.ID, f.ItemCap); err != nil {
		return *res, fmt.Errorf("rss: trim items of feed %s: %w", f.ID, err)
	}
	res.ItemsAdded = added

	// The validators are stored verbatim, weak W/"…" prefix included, so
	// the next poll replays exactly what the publisher sent.
	f.ETag = headerValue(resp, "ETag")
	f.LastModified = headerValue(resp, "Last-Modified")
	if meta.TTLMinutes > 0 {
		f.TTLMinutes = &meta.TTLMinutes
	} else {
		f.TTLMinutes = nil
	}

	return *res, p.recordSuccess(ctx, f, now, globalS, meta)
}

// request builds the conditional GET of doc 08 section 2.3: the three
// constant headers plus whichever stored validators exist, replayed verbatim.
func (p *Poller) request(ctx context.Context, f store.Feed) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("rss: build request for %s: %w", secure.RedactURL(f.URL), err)
	}
	req.Header.Set("User-Agent", fmt.Sprintf(userAgent, Version))
	req.Header.Set("Accept", acceptHeader)
	req.Header.Set("Accept-Encoding", acceptEnc)
	if f.ETag != nil {
		req.Header.Set("If-None-Match", *f.ETag)
	}
	if f.LastModified != nil {
		req.Header.Set("If-Modified-Since", *f.LastModified)
	}

	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// recordSuccess lands the post-poll state of a 200 or 304: the ladder steps
// down by one (never reset), last_error clears, and the next poll is the
// jittered effective interval out, pushed past any publisher skip window.
func (p *Poller) recordSuccess(ctx context.Context, f store.Feed, now int64, globalS int, m FeedMeta) error {
	f.EscalationLevel = max(f.EscalationLevel-1, 0)
	f.DisabledTill = nil
	f.LastError = nil
	f.LastFetchAt = &now
	f.LastSuccessAt = &now
	f.NextFetchAt = nextAllowed(NextFetchAt(now, EffectiveInterval(f.RefreshIntervalS, globalS, m), f.ID), m)

	if err := store.UpdateFeedFetchState(ctx, p.db, f); err != nil {
		return fmt.Errorf("rss: record success of feed %s: %w", f.ID, err)
	}

	return nil
}

// recordFailure lands the ladder step of doc 08 section 2.4: level +1
// clamped to 9, last_error stored, and — only outside the startup grace —
// disabled_till = now + BackoffPeriods[level]. Inside the grace the feed is
// never auto-disabled, so it retries at its normal jittered interval.
func (p *Poller) recordFailure(ctx context.Context, f store.Feed, now int64, globalS int, errText string) error {
	f.EscalationLevel = min(f.EscalationLevel+1, len(BackoffPeriods)-1)
	f.LastError = &errText
	f.LastFetchAt = &now

	if p.now().Sub(p.startedAt) >= startupGrace {
		disabledTill := now + BackoffPeriods[f.EscalationLevel]*1000
		f.DisabledTill = &disabledTill
		f.NextFetchAt = disabledTill
	} else {
		f.DisabledTill = nil
		f.NextFetchAt = NextFetchAt(now, EffectiveInterval(f.RefreshIntervalS, globalS, storedMeta(f)), f.ID)
	}

	if err := store.UpdateFeedFetchState(ctx, p.db, f); err != nil {
		return fmt.Errorf("rss: record failure of feed %s: %w", f.ID, err)
	}

	return nil
}

// storedMeta rebuilds the interval hints a 304 can reuse: the stored
// ttl_minutes survives, while skip windows and the syndication interval are
// parse-time facts that persist nowhere, so they are absent here.
func storedMeta(f store.Feed) FeedMeta {
	m := FeedMeta{}
	if f.TTLMinutes != nil {
		m.TTLMinutes = *f.TTLMinutes
	}

	return m
}

// nextAllowed pushes a candidate next_fetch_at (unix ms) out of the
// publisher's skipHours/skipDays windows, evaluated in GMT per doc 08
// section 2.2. Feeds whose hints mark every hour or day skipped stop after a
// week of hours rather than reschedule forever.
func nextAllowed(candidate int64, m FeedMeta) int64 {
	if len(m.SkipHours) == 0 && len(m.SkipDays) == 0 {
		return candidate
	}

	t := time.UnixMilli(candidate).UTC()
	for range maxSkipScan {
		if !slices.Contains(m.SkipDays, t.Weekday()) && !slices.Contains(m.SkipHours, t.Hour()) {
			return t.UnixMilli()
		}
		t = t.Add(time.Hour)
	}

	return candidate
}

// globalInterval reads the rss_interval_s settings row; an absent or
// undecodable row falls back to the documented 1800-second default
// (docs/11-config-reference.md).
func (p *Poller) globalInterval(ctx context.Context) (int, error) {
	var valueJSON string
	err := p.db.GetContext(ctx, &valueJSON, `SELECT value_json FROM settings WHERE key = ?`, settingRSSIntervalS)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultGlobalIntervalS, nil
	}
	if err != nil {
		return 0, fmt.Errorf("rss: read settings key %s: %w", settingRSSIntervalS, err)
	}

	var seconds int
	if err := json.Unmarshal([]byte(valueJSON), &seconds); err != nil || seconds <= 0 {
		return defaultGlobalIntervalS, nil
	}

	return seconds, nil
}

// stackedBody reads through the decompressor while Close releases both it
// and the wire body underneath — a decompressor's own Close never reaches
// the original resp.Body, so without the stack the connection would leak.
type stackedBody struct {
	io.Reader
	decoded io.Closer
	wire    io.Closer
}

func (s stackedBody) Close() error {
	return errors.Join(s.decoded.Close(), s.wire.Close())
}

// readCapped hands the body to secure.ReadCapped, decoding gzip or deflate
// first so the streaming cap measures the real feed bytes — a compressed
// bomb cannot bypass it. The declared Content-Length is rejected before a
// byte is consumed, decompressor setup included (doc 12 section 2.2 rule 7);
// an over-cap read then reports the doc-08 section 2.1 text verbatim.
func readCapped(resp *http.Response) ([]byte, error) {
	if resp.ContentLength > maxBody {
		return nil, errors.New("feed body exceeds 16 MiB")
	}

	decoded, err := decodeBody(resp)
	if err != nil {
		return nil, fmt.Errorf("feed body decode failed: %w", err)
	}
	if decoded != resp.Body {
		resp.Body = stackedBody{Reader: decoded, decoded: decoded, wire: resp.Body}
	}

	body, err := secure.ReadCapped(resp, maxBody)
	if err != nil {
		if errors.Is(err, secure.ErrBodyTooLarge) {
			return nil, errors.New("feed body exceeds 16 MiB")
		}

		return nil, fmt.Errorf("feed body read failed: %w", err)
	}

	return body, nil
}

// decodeBody wraps resp.Body in the decompressor Content-Encoding names.
// The Accept header offers gzip and deflate, so both must be handled here —
// the transport's automatic gzip does not run once Accept-Encoding is set
// explicitly. deflate answers are sniffed for the zlib wrapper: a stream
// starting with the zlib CMF byte 0x78 goes through zlib, anything else is a
// raw flate stream.
func decodeBody(resp *http.Response) (io.ReadCloser, error) {
	switch strings.ToLower(resp.Header.Get("Content-Encoding")) {
	case "gzip":
		return gzip.NewReader(resp.Body)
	case "deflate":
		br := bufio.NewReader(resp.Body)
		head, err := br.Peek(2)
		if err == nil && len(head) == 2 && head[0] == 0x78 {
			return zlib.NewReader(br)
		}

		return flate.NewReader(br), nil
	default:
		return resp.Body, nil
	}
}

// headerValue returns a response header as a storable pointer, nil when the
// publisher sent none.
func headerValue(resp *http.Response, name string) *string {
	v := resp.Header.Get(name)
	if v == "" {
		return nil
	}

	return &v
}

// PollDue is the jobs.Handler body for kind "rss_poll": it polls every due
// feed, pollParallel at a time and never more than one request per host at
// once — each host's queue is claimed whole by a single worker. Per-feed
// failures are recorded on the feed's ladder and logged; they do not fail
// the job, because the ladder — not the job retry — owns feed retries.
func (p *Poller) PollDue(ctx context.Context, _ store.Job) error {
	feeds, err := store.DueFeeds(ctx, p.db, p.now().UnixMilli(), dueFeedLimit)
	if err != nil {
		return fmt.Errorf("rss: list due feeds: %w", err)
	}
	if len(feeds) == 0 {
		return nil
	}

	byHost := make(map[string][]store.Feed, len(feeds))
	hosts := make([]string, 0, len(feeds))
	for _, f := range feeds {
		host := feedHost(f.URL)
		if _, seen := byHost[host]; !seen {
			hosts = append(hosts, host)
		}
		byHost[host] = append(byHost[host], f)
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	for range min(pollParallel, len(hosts)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if len(hosts) == 0 {
					mu.Unlock()
					return
				}
				host := hosts[0]
				hosts = hosts[1:]
				queue := byHost[host]
				mu.Unlock()

				for _, f := range queue {
					if ctx.Err() != nil {
						return
					}
					res, err := p.Poll(ctx, f, false)
					switch {
					case err != nil:
						p.log.ErrorContext(ctx, "feed poll bookkeeping failed",
							"feed_id", f.ID, "err", err)
					case res.Error != "":
						p.log.WarnContext(ctx, "feed poll failed",
							"feed_id", f.ID, "err", res.Error)
					}
				}
			}
		}()
	}
	wg.Wait()

	return nil
}

// feedHost derives the one-request-per-host key in memory from the feed URL;
// an unparseable URL groups under itself so two malformed feeds never share
// a bucket.
func feedHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}

	return u.Host
}
