package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/search"
	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

// JobKindSearch is the jobs.kind POST /search enqueues.
const JobKindSearch = "search"

// engineSearchDeadline is the per-engine bound of the fan-out: one indexer
// gets at most 15 s, and a hung engine can never stall the job's finish.
const engineSearchDeadline = 15 * time.Second

// SearchPayload is the jobs.payload_json of kind "search".
type SearchPayload struct {
	SearchJobID string   `json:"search_job_id"`
	Query       string   `json:"query"`
	IndexerIDs  []string `json:"indexer_ids"`
	Categories  []int    `json:"categories"`
}

// EngineFailure classifies what an engine did, so the tracker can carry a
// message a user can act on. Text is what reaches EngineStatus.Error
// verbatim.
type EngineFailure struct {
	Text        string        // e.g. "HTTP 503 from https://academictorrents.com/rss.xml"
	Disable     bool          // codes 100, 101, 102, 910: credentials or API disabled
	DisableMode string        // codes 202, 203: this t= mode only
	RetryAfter  time.Duration // honoured from the Retry-After header on 429
}

// classify maps a transport error, a *search.TorznabError or an HTTP status
// onto an EngineFailure, following docs/07-search-and-indexers.md section 2.5
// row for row, including Prowlarr's non-spec 400, 410 Gone and 429 answers.
// Code 300 is not a failure: it is an empty result set, and nil means the
// engine answered "done, zero rows". status is the HTTP status the caller
// observed out of band — 0 for the fan-out, whose errors carry their own —
// used when the error does not encode one.
func classify(err error, status int, rawURL string) *EngineFailure {
	if err == nil {
		return nil
	}
	var te *search.TorznabError
	if errors.As(err, &te) {
		return classifyTorznab(te, rawURL)
	}
	var ue *search.UpstreamError
	if errors.As(err, &ue) {
		return classifyUpstream(ue, rawURL)
	}
	if status >= http.StatusBadRequest {
		return &EngineFailure{Text: httpStatusText(status, rawURL)}
	}
	// A transport failure: the wire message is the redacted upstream one —
	// a fetch error can embed the request URL, api key included
	// (docs/05-api-contract.md section 9.2).
	return &EngineFailure{Text: secure.RedactError(err).Error()}
}

// classifyTorznab maps a Torznab error document's code onto the failure
// vocabulary of doc 07 section 2.5.
func classifyTorznab(te *search.TorznabError, rawURL string) *EngineFailure {
	switch te.Code {
	case 100, 101, 102, 910:
		// Incorrect credentials, account suspended, insufficient
		// privileges or a disabled API: the engine is unusable until the
		// operator fixes the key, and the message says so.
		return &EngineFailure{Text: "check API key" + descSuffix(te.Description), Disable: true}
	case 202, 203:
		// No such function / function not available: the engine does not
		// offer the requested t= mode — the fan-out only ever issues
		// t=search, so that is the mode to name.
		return &EngineFailure{
			Text:        "the engine does not support t=search" + descSuffix(te.Description),
			DisableMode: "search",
		}
	case 300:
		// "No such item" is an empty result set, not an error.
		return nil
	case 410:
		// Prowlarr's non-spec "Indexer is disabled": engine disabled
		// upstream, not a transport failure.
		return &EngineFailure{Text: "the engine is disabled upstream" + descSuffix(te.Description), Disable: true}
	case 429:
		// Prowlarr answers 429 with a non-spec error document and a
		// Retry-After header for indexer backoff and query limits.
		return &EngineFailure{
			Text:       httpStatusText(http.StatusTooManyRequests, rawURL) + retrySuffix(te.RetryAfter),
			RetryAfter: te.RetryAfter,
		}
	default:
		// 200, 201, 900 and everything else fail this engine only; the
		// error's own rendering names code, HTTP status and description.
		return &EngineFailure{Text: te.Error()}
	}
}

// classifyUpstream maps a dlsearch runner failure: a non-2xx answer becomes
// the documented "HTTP <status> from <url>" message and carries the
// Retry-After hold the runner recorded for 429 and 503; a status-0 error is
// a transport failure whose Detail is already redacted.
func classifyUpstream(ue *search.UpstreamError, rawURL string) *EngineFailure {
	if ue.Status > 0 {
		return &EngineFailure{
			Text:       httpStatusText(ue.Status, rawURL) + retrySuffix(ue.RetryAfter),
			RetryAfter: ue.RetryAfter,
		}
	}
	if ue.Detail == "" {
		return &EngineFailure{Text: "the engine did not answer"}
	}
	return &EngineFailure{Text: ue.Detail}
}

// httpStatusText renders the doc 05 section 9.2 engine-error shape:
// "HTTP 503 from https://academictorrents.com/rss.xml". The URL is redacted
// so a base carrying userinfo or a query cannot leak a key.
func httpStatusText(status int, rawURL string) string {
	if rawURL == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("HTTP %d from %s", status, secure.RedactURL(rawURL))
}

// descSuffix appends the upstream description to a classified message, or
// nothing when the server sent none.
func descSuffix(desc string) string {
	if desc == "" {
		return ""
	}
	return ": " + desc
}

// retrySuffix names the upstream wait in the engine message, so the poll
// shows why the engine stopped and when to try again. The engine still ends
// now — nothing sleeps past the per-engine deadline waiting it out.
func retrySuffix(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return fmt.Sprintf("; retry after %s", d.Round(time.Second))
}

// engineURL is the endpoint an engine failure message names: the torznab
// row's base URL, or the definition's request base for a dlsearch engine.
func engineURL(row store.Indexer, reg *search.Registry) string {
	if row.URL != nil && *row.URL != "" {
		return *row.URL
	}
	if reg != nil && row.DefinitionID != nil {
		if def, ok := reg.Get(*row.DefinitionID); ok && def.Request != nil {
			return def.Request.BaseURL
		}
	}
	return ""
}

// NewSearchHandler returns the handler for jobs.kind "search". It marks every
// selected indexer "searching", runs one goroutine per indexer with the
// per-engine 15 s deadline, writes each engine's rows as they arrive, and
// finishes the job when the last goroutine returns. max_attempts for this
// kind is 1: a search is re-run by the user, not retried. ua is the
// User-Agent the torznab path sends — the same dl-tool/<version> string the
// Runner carries for dlsearch indexers.
func NewSearchHandler(db *sqlx.DB, log *slog.Logger, reg *search.Registry, run *search.Runner, idx *store.IndexerStore, hc *http.Client, ua string) Handler {
	return func(ctx context.Context, j store.Job) error {
		var p SearchPayload
		if err := json.Unmarshal([]byte(j.PayloadJSON), &p); err != nil {
			return fmt.Errorf("jobs: decode %q payload: %w", JobKindSearch, err)
		}

		// Every return below this point — a store error, a cancel mid-run —
		// must still close the search_jobs row: max_attempts is 1, so the
		// queue never re-drives a failed handler, and an unfinished row
		// would poll as finished:false forever. Best-effort on a detached,
		// time-bounded context; a row deleted mid-run is already gone and
		// FinishSearchJob's ErrNotFound is the quiet answer.
		finished := false
		defer func() {
			if finished || p.SearchJobID == "" {
				return
			}
			writeCtx, cancel := detachedWrite(ctx)
			defer cancel()
			total, err := store.CountSearchResults(writeCtx, db, p.SearchJobID)
			if err != nil {
				log.WarnContext(ctx, "search result count failed during abnormal finish",
					"search_job_id", p.SearchJobID, "err", err)
			}
			msg := "the search ended without completing"
			if err := store.FinishSearchJob(writeCtx, db, p.SearchJobID, total, &msg, time.Now().UnixMilli()); err != nil &&
				!errors.Is(err, store.ErrNotFound) {
				log.WarnContext(ctx, "search job left unfinished", "search_job_id", p.SearchJobID, "err", err)
			}
		}()

		// A job whose search_jobs row is gone — the user deleted it between
		// enqueue and claim, or a re-delivery after the row went — has
		// nothing to do; the queue row completes quietly.
		if _, err := store.GetSearchJob(ctx, db, p.SearchJobID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Already deleted — there is no row for the deferred finish
				// to close, so skip the wasted writes too.
				finished = true
				return nil
			}
			return fmt.Errorf("jobs: load search job %s: %w", p.SearchJobID, err)
		}

		// Resolve every selected indexer and seed the tracker before the
		// first goroutine launches: an engine whose row vanished mid-flight
		// keeps its seat and reports the deletion like any other engine
		// error; the live ones enter as "searching" so no poll can observe
		// a not-yet-started engine as done.
		engines := make([]store.EngineStatus, 0, len(p.IndexerIDs))
		rows := make(map[string]store.Indexer, len(p.IndexerIDs))
		var failedEngines atomic.Int64
		for _, id := range p.IndexerIDs {
			row, err := idx.Get(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				msg := "the indexer no longer exists"
				engines = append(engines, store.EngineStatus{ID: id, Name: id, Status: store.EngineError, Error: &msg})
				failedEngines.Add(1)
				continue
			}
			if err != nil {
				return fmt.Errorf("jobs: resolve indexer %s: %w", id, err)
			}
			rows[id] = row
			engines = append(engines, store.EngineStatus{ID: id, Name: row.Name, Status: store.EngineSearching})
		}
		store.Searches.Start(p.SearchJobID, engines)

		var wg sync.WaitGroup
		var written atomic.Int64
		for _, id := range p.IndexerIDs {
			row, ok := rows[id]
			if !ok {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				engineCtx, cancel := context.WithTimeout(ctx, engineSearchDeadline)
				defer cancel()

				results, err := searchIndexer(engineCtx, row, p, reg, run, idx, hc, ua, log)
				if err != nil {
					f := classify(err, 0, engineURL(row, reg))
					if f == nil {
						// Torznab 300 "no such item": the engine answered
						// and the result set is empty — done, not error.
						store.Searches.Set(p.SearchJobID, id, store.EngineDone, 0, nil)
						return
					}
					store.Searches.Set(p.SearchJobID, id, store.EngineError, 0, &f.Text)
					failedEngines.Add(1)
					// The indexer's settings row shows the same message
					// the poll does. The write runs on the job context,
					// not the engine's: a slow engine's own deadline must
					// not lose it, and a deleted row is already gone.
					if rerr := idx.RecordTest(ctx, id, time.Now().UnixMilli(), &f.Text); rerr != nil &&
						!errors.Is(rerr, store.ErrNotFound) {
						log.WarnContext(ctx, "search engine failure record failed",
							"search_job_id", p.SearchJobID, "indexer_id", id, "err", rerr)
					}
					log.WarnContext(ctx, "search engine failed",
						"search_job_id", p.SearchJobID, "indexer_id", id, "err", f.Text)
					return
				}

				// The write runs on the job context, not the engine's: a
				// slow engine must not lose its own rows to the deadline.
				n, werr := store.InsertResults(ctx, db, p.SearchJobID, id, toResultRows(results))
				if werr != nil {
					msg := "the engine's results could not be stored"
					store.Searches.Set(p.SearchJobID, id, store.EngineError, 0, &msg)
					failedEngines.Add(1)
					log.ErrorContext(ctx, "search result write failed",
						"search_job_id", p.SearchJobID, "indexer_id", id, "err", werr)
					return
				}
				store.Searches.Set(p.SearchJobID, id, store.EngineDone, n, nil)
				written.Add(int64(n))
			}()
		}
		wg.Wait()

		var lastErr *string
		if f := failedEngines.Load(); f > 0 {
			s := fmt.Sprintf("%d of %d indexers failed", f, len(p.IndexerIDs))
			lastErr = &s
		}
		if err := store.FinishSearchJob(ctx, db, p.SearchJobID, int(written.Load()), lastErr, time.Now().UnixMilli()); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Deleted mid-run; the results cascaded with the row.
				finished = true
				return nil
			}
			return fmt.Errorf("jobs: finish search job %s: %w", p.SearchJobID, err)
		}
		finished = true
		return nil
	}
}

// searchIndexer runs one engine of the fan-out: the Torznab client for a
// torznab/newznab row, the dlsearch runner for a definition-backed one.
func searchIndexer(ctx context.Context, row store.Indexer, p SearchPayload, reg *search.Registry, run *search.Runner, idx *store.IndexerStore, hc *http.Client, ua string, log *slog.Logger) ([]search.SearchResult, error) {
	cfg := store.IndexerSettingsMap(log, row)

	switch row.Kind {
	case "torznab", "newznab":
		if row.URL == nil || *row.URL == "" {
			return nil, errors.New("the indexer has no url")
		}
		apiKey, err := idx.OpenAPIKey(row)
		if err != nil {
			return nil, fmt.Errorf("open indexer key: %w", err)
		}
		client, err := search.NewTorznabClient(
			searchHTTPClient(hc, cfg, *row.URL, log), *row.URL, apiKey, row.ID, ua,
		)
		if err != nil {
			return nil, err
		}
		return client.Search(ctx, search.Query{T: "search", Q: p.Query, Categories: p.Categories})
	case "dlsearch":
		if reg == nil || run == nil {
			return nil, errors.New("the search runner is not configured")
		}
		if row.DefinitionID == nil || *row.DefinitionID == "" {
			return nil, errors.New("the indexer has no definition_id")
		}
		def, ok := reg.Get(*row.DefinitionID)
		if !ok {
			return nil, fmt.Errorf("the indexer's definition %q is not loaded", *row.DefinitionID)
		}
		apiKey, err := idx.OpenAPIKey(row)
		if err != nil {
			return nil, fmt.Errorf("open indexer key: %w", err)
		}
		if apiKey.Reveal() != "" {
			cfg["api_key"] = apiKey.Reveal()
		}
		return run.Search(ctx, def, cfg, search.Query{Q: p.Query, Categories: p.Categories})
	default:
		return nil, fmt.Errorf("unknown indexer kind %q", row.Kind)
	}
}

// searchHTTPClient returns the outbound client for one engine call: the
// shared SSRF-guarded one, or a private-allowing guard scoped to the
// indexer's origin when the row's allow_private_network setting lifts the
// private-range denial — the same exemption the create/probe path applies.
func searchHTTPClient(hc *http.Client, cfg map[string]string, rawURL string, log *slog.Logger) *http.Client {
	if !settingTruthy(cfg["allow_private_network"]) {
		return hc
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return hc
	}
	return secure.NewClient(secure.NewGuard(log, true).ForOrigin(u))
}

// settingTruthy is the settings-document truth test: the reserved
// allow_private_network flag lands in the decoded map as "true"/"false".
func settingTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// toResultRows converts the engine's normalised results into store rows:
// unix-millisecond timestamps, nil for every value the source did not
// report. The acquisition handles (download_url, magnet_uri, details_url)
// persist — they are written for the later POST /tasks resolution and never
// leave through the API.
func toResultRows(results []search.SearchResult) []store.SearchResultRow {
	rows := make([]store.SearchResultRow, 0, len(results))
	for _, r := range results {
		row := store.SearchResultRow{
			Title:                  r.Title,
			DownloadVolumeFactor:   r.DownloadVolumeFactor,
			UploadVolumeFactor:     r.UploadVolumeFactor,
			Seeders:                r.Seeders,
			Leechers:               r.Leechers,
			Grabs:                  r.Grabs,
			MinimumRatio:           r.MinimumRatio,
			MinimumSeedTimeSeconds: r.MinimumSeedTimeSecs,
			Year:                   r.Year,
		}
		if r.DownloadURL != "" {
			row.DownloadURL = &r.DownloadURL
		}
		if r.MagnetURI != "" {
			row.MagnetURI = &r.MagnetURI
		}
		if r.Infohash != "" {
			row.InfoHash = &r.Infohash
		}
		if r.SizeBytes > 0 {
			row.SizeBytes = &r.SizeBytes
		}
		if r.PublishedAt != nil {
			if t, err := time.Parse(time.RFC3339, *r.PublishedAt); err == nil {
				ms := t.UnixMilli()
				row.PublishedAt = &ms
			}
		}
		if r.DetailsURL != "" {
			row.DetailsURL = &r.DetailsURL
		}
		if len(r.CategoryIDs) > 0 {
			if raw, err := json.Marshal(r.CategoryIDs); err == nil {
				s := string(raw)
				row.CategoryIDsJSON = &s
			}
		}
		if r.CategoryDesc != "" {
			row.CategoryDesc = &r.CategoryDesc
		}
		if r.IMDBID != "" {
			row.IMDBID = &r.IMDBID
		}
		if r.TMDBID != "" {
			row.TMDBID = &r.TMDBID
		}
		if r.TVDBID != "" {
			row.TVDBID = &r.TVDBID
		}
		if r.Genre != "" {
			row.Genre = &r.Genre
		}
		if r.Language != "" {
			row.Language = &r.Language
		}
		if r.Publisher != "" {
			row.Publisher = &r.Publisher
		}
		if r.Author != "" {
			row.Author = &r.Author
		}
		if r.Album != "" {
			row.Album = &r.Album
		}
		if r.Artist != "" {
			row.Artist = &r.Artist
		}
		rows = append(rows, row)
	}
	return rows
}
