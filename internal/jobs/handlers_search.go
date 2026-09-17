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
					// The wire message is the redacted upstream one: a
					// torznab error can embed the request URL, api key
					// included (docs/05-api-contract.md section 9.2).
					msg := secure.RedactError(err).Error()
					store.Searches.Set(p.SearchJobID, id, store.EngineError, 0, &msg)
					failedEngines.Add(1)
					log.WarnContext(ctx, "search engine failed",
						"search_job_id", p.SearchJobID, "indexer_id", id, "err", msg)
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
