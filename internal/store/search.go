package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// SearchJob is one row of the search_jobs table (docs/04-data-model.md
// section 3.4): the durable half of an asynchronous search.
type SearchJob struct {
	ID             string  `db:"id"`
	Query          string  `db:"query"`
	IndexerIDsJSON string  `db:"indexer_ids_json"`
	CategoriesJSON *string `db:"categories_json"`
	Finished       bool    `db:"finished"`
	Total          int     `db:"total"`
	LastError      *string `db:"last_error"`
	StartedAt      int64   `db:"started_at"`
	FinishedAt     *int64  `db:"finished_at"`
	CreatedAt      int64   `db:"created_at"`
	UpdatedAt      int64   `db:"updated_at"`
}

// SearchResultRow mirrors the search_results DDL. Package store never
// imports internal/search, so the job handler converts search.SearchResult
// into this type. DownloadURL, MagnetURI and DetailsURL are acquisition
// handles — they persist for the later POST /tasks resolution but are never
// serialized by an API mapper (07-search-and-indexers.md section 5 rule 6).
type SearchResultRow struct {
	ID                     string   `db:"id"`
	SearchJobID            string   `db:"search_job_id"`
	IndexerID              string   `db:"indexer_id"`
	Title                  string   `db:"title"`
	DownloadURL            *string  `db:"download_url"`
	MagnetURI              *string  `db:"magnet_uri"`
	InfoHash               *string  `db:"info_hash"`
	SizeBytes              *int64   `db:"size_bytes"`
	Seeders                *int     `db:"seeders"`
	Leechers               *int     `db:"leechers"`
	Grabs                  *int     `db:"grabs"`
	PublishedAt            *int64   `db:"published_at"` // unix ms
	DetailsURL             *string  `db:"details_url"`
	CategoryIDsJSON        *string  `db:"category_ids_json"`
	CategoryDesc           *string  `db:"category_desc"`
	DownloadVolumeFactor   float64  `db:"download_volume_factor"`
	UploadVolumeFactor     float64  `db:"upload_volume_factor"`
	MinimumRatio           *float64 `db:"minimum_ratio"`
	MinimumSeedTimeSeconds *int     `db:"minimum_seed_time_seconds"`
	IMDBID                 *string  `db:"imdb_id"`
	TMDBID                 *string  `db:"tmdb_id"`
	TVDBID                 *string  `db:"tvdb_id"`
	Year                   *int     `db:"year"`
	Genre                  *string  `db:"genre"`
	Language               *string  `db:"language"`
	Publisher              *string  `db:"publisher"`
	Author                 *string  `db:"author"`
	Album                  *string  `db:"album"`
	Artist                 *string  `db:"artist"`
	CreatedAt              int64    `db:"created_at"`
	UpdatedAt              int64    `db:"updated_at"`
}

const searchJobColumns = `id, query, indexer_ids_json, categories_json, finished,
total, last_error, started_at, finished_at, created_at, updated_at`

const searchResultColumns = `id, search_job_id, indexer_id, title, download_url,
magnet_uri, info_hash, size_bytes, seeders, leechers, grabs, published_at,
details_url, category_ids_json, category_desc, download_volume_factor,
upload_volume_factor, minimum_ratio, minimum_seed_time_seconds, imdb_id,
tmdb_id, tvdb_id, year, genre, language, publisher, author, album, artist,
created_at, updated_at`

const (
	queryCreateSearchJob = `INSERT INTO search_jobs
(id, query, indexer_ids_json, categories_json, finished, total, last_error,
 started_at, finished_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	queryGetSearchJob = `SELECT ` + searchJobColumns + `
FROM search_jobs WHERE id = ?`

	queryFinishSearchJob = `UPDATE search_jobs
SET finished = 1, total = ?, last_error = ?, finished_at = ?, updated_at = ?
WHERE id = ?`

	queryDeleteSearchJob = `DELETE FROM search_jobs WHERE id = ?`

	// The 24-hour retention of docs/04-data-model.md section 3.4; the
	// nightly cron of T091 is the caller. Results cascade with the job. The
	// id list lets the sweep also forget the tracker entries.
	queryPurgeSearchJobIDs = `SELECT id FROM search_jobs WHERE created_at < ?`
	queryPurgeSearchJobs   = `DELETE FROM search_jobs WHERE created_at < ?`

	queryDeleteIndexerResults = `DELETE FROM search_results
WHERE search_job_id = ? AND indexer_id = ?`

	queryInsertSearchResult = `INSERT INTO search_results
(` + searchResultColumns + `)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	// COUNT(*) has no column name; the alias keeps the StructScan explicit.
	queryCountResultsByIndexer = `SELECT indexer_id, COUNT(*) AS count FROM search_results
WHERE search_job_id = ? GROUP BY indexer_id`

	queryListSearchResults = `SELECT ` + searchResultColumns + `
FROM search_results
WHERE search_job_id = ?`

	queryCountSearchResults = `SELECT COUNT(*) FROM search_results WHERE search_job_id = ?`
)

// CreateSearchJob inserts one running job row; the id (sch_…), started_at and
// the timestamps are assigned here. IndexerIDsJSON and CategoriesJSON are the
// verbatim JSON documents the job fans out over.
func CreateSearchJob(ctx context.Context, db *sqlx.DB, j SearchJob) (SearchJob, error) {
	return insertSearchJobRow(ctx, db, j)
}

// insertSearchJobRow is the search_jobs insert every creation path shares, so
// the column list is maintained in exactly one place. ext is *sqlx.DB or a
// *sqlx.Tx, the same contract insertTaskRow has.
func insertSearchJobRow(ctx context.Context, ext sqlx.ExtContext, j SearchJob) (SearchJob, error) {
	j.ID = NewID(PrefixSearchJob)
	now := time.Now().UnixMilli()
	j.StartedAt = now
	j.CreatedAt, j.UpdatedAt = now, now
	if _, err := ext.ExecContext(
		ctx, queryCreateSearchJob,
		j.ID, j.Query, j.IndexerIDsJSON, j.CategoriesJSON, j.Finished,
		j.Total, j.LastError, j.StartedAt, j.FinishedAt, j.CreatedAt, j.UpdatedAt,
	); err != nil {
		return SearchJob{}, fmt.Errorf("store: create search job: %w", err)
	}

	return j, nil
}

// GetSearchJob resolves one row by id. ErrNotFound means the id addresses no
// row — deleted and unknown are the same answer.
func GetSearchJob(ctx context.Context, db *sqlx.DB, id string) (SearchJob, error) {
	var row SearchJob
	err := db.GetContext(ctx, &row, queryGetSearchJob, id)
	if errors.Is(err, sql.ErrNoRows) {
		return SearchJob{}, fmt.Errorf("store: search job %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return SearchJob{}, fmt.Errorf("store: search job %s: %w", id, err)
	}

	return row, nil
}

// FinishSearchJob stamps the terminal state: finished, the total rows written
// across every engine, an optional job-level error summary and finished_at.
// ErrNotFound means the job was deleted underneath the worker — the handler
// treats that as a completed job, not a failure.
func FinishSearchJob(ctx context.Context, db *sqlx.DB, id string, total int, lastErr *string, now int64) error {
	result, err := db.ExecContext(ctx, queryFinishSearchJob, total, lastErr, now, now, id)
	if err != nil {
		return fmt.Errorf("store: finish search job %s: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: finish search job %s: read rows affected: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: finish search job %s: %w", id, ErrNotFound)
	}

	return nil
}

// DeleteSearchJob removes the job row; its results cascade through the ON
// DELETE CASCADE of docs/04-data-model.md section 3.4. ErrNotFound means the
// id addresses no row.
func DeleteSearchJob(ctx context.Context, db *sqlx.DB, id string) error {
	return deleteSearchJobRow(ctx, db, id)
}

// deleteSearchJobRow is the search_jobs delete both delete paths share. ext
// is *sqlx.DB or a *sqlx.Tx.
func deleteSearchJobRow(ctx context.Context, ext sqlx.ExtContext, id string) error {
	result, err := ext.ExecContext(ctx, queryDeleteSearchJob, id)
	if err != nil {
		return fmt.Errorf("store: delete search job %s: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete search job %s: read rows affected: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: delete search job %s: %w", id, ErrNotFound)
	}

	return nil
}

// queryEnqueueSearchJob inserts the queue row for one search job in a
// single statement — including max_attempts 1, so a polling worker can
// never claim the row between an insert and a clamp update and read the
// DDL default. The jobs table belongs to T012; this insert is the search
// kind's own statement so the two statements it would otherwise need stay
// atomic without touching that file.
const queryEnqueueSearchJob = `INSERT INTO jobs
(id, kind, task_id, payload_json, run_after, max_attempts, created_at, updated_at)
VALUES (?, 'search', NULL, ?, ?, 1, ?, ?)`

// EnqueueSearchJob writes one pending queue row of kind "search" with
// max_attempts 1 — a search is re-run by the user, not retried. The payload
// is stored as JSON exactly as EnqueueJob would marshal it.
func EnqueueSearchJob(ctx context.Context, db *sqlx.DB, payload any, runAfter int64) (string, error) {
	return insertSearchQueueRow(ctx, db, payload, runAfter)
}

// insertSearchQueueRow is the jobs insert the standalone enqueue and the
// combined create share. ext is *sqlx.DB or a *sqlx.Tx.
func insertSearchQueueRow(ctx context.Context, ext sqlx.ExtContext, payload any, runAfter int64) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("store: marshal search job payload: %w", err)
	}

	id := NewID(PrefixJob)
	now := time.Now().UnixMilli()
	if _, err := ext.ExecContext(ctx, queryEnqueueSearchJob, id, string(data), runAfter, now, now); err != nil {
		return "", fmt.Errorf("store: enqueue search job: %w", err)
	}

	return id, nil
}

// CreateSearchJobAndEnqueue writes the search_jobs row and its queue row in
// one transaction: a search that exists always has its driver, and a failed
// enqueue cannot leave an orphaned job that polls queued forever. payloadOf
// builds the queue payload from the assigned job id — the id only exists
// inside the transaction, so the payload cannot be marshaled before it.
func CreateSearchJobAndEnqueue(ctx context.Context, db *sqlx.DB, j SearchJob, payloadOf func(searchJobID string) any, runAfter int64) (SearchJob, error) {
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return SearchJob{}, fmt.Errorf("store: create search job: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "search job create rollback failed", "err", err)
		}
	}()

	created, err := insertSearchJobRow(ctx, tx, j)
	if err != nil {
		return SearchJob{}, err
	}
	if _, err := insertSearchQueueRow(ctx, tx, payloadOf(created.ID), runAfter); err != nil {
		return SearchJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return SearchJob{}, fmt.Errorf("store: create search job: commit: %w", err)
	}

	return created, nil
}

// queryDeleteSearchQueueRow removes the queue row bound to one search job;
// DELETE /search/{id} calls it so a still-pending job is never claimed. The
// payload embeds the search job id under the search_job_id key — the
// json_extract match is exact and case-sensitive, unlike a LIKE predicate.
const queryDeleteSearchQueueRow = `DELETE FROM jobs
WHERE kind = 'search' AND json_extract(payload_json, '$.search_job_id') = ?`

// DeleteSearchQueueRow drops the queue row of one search job, whichever
// state it is in: a pending row is never claimed, and a running row's
// handler exits quietly once the search_jobs row is gone (its first read
// answers ErrNotFound).
func DeleteSearchQueueRow(ctx context.Context, db *sqlx.DB, searchJobID string) error {
	return deleteSearchQueueRow(ctx, db, searchJobID)
}

// deleteSearchQueueRow is the jobs delete both delete paths share. ext is
// *sqlx.DB or a *sqlx.Tx.
func deleteSearchQueueRow(ctx context.Context, ext sqlx.ExtContext, searchJobID string) error {
	if _, err := ext.ExecContext(ctx, queryDeleteSearchQueueRow, searchJobID); err != nil {
		return fmt.Errorf("store: delete queue row of search job %s: %w", searchJobID, err)
	}

	return nil
}

// DeleteSearchJobAndQueue removes the search_jobs row and its queue row in
// one transaction — DELETE /search/{id} either drops both or neither, so a
// failure cannot strand a claimable queue row or a driverless job. Both
// deletes become visible together at commit; their order inside the
// transaction is immaterial. ErrNotFound means the id addresses no job.
func DeleteSearchJobAndQueue(ctx context.Context, db *sqlx.DB, searchJobID string) error {
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: delete search job %s: %w", searchJobID, err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "search job delete rollback failed", "err", err)
		}
	}()

	if err := deleteSearchQueueRow(ctx, tx, searchJobID); err != nil {
		return err
	}
	if err := deleteSearchJobRow(ctx, tx, searchJobID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: delete search job %s: commit: %w", searchJobID, err)
	}

	return nil
}

// PurgeSearchJobs deletes every job created before olderThan (unix ms) and
// reports how many; results cascade, and each purged job's tracker entry is
// forgotten so the sweep cannot leak volatile state. The 24-hour retention
// sweep of T091 is the intended caller.
func PurgeSearchJobs(ctx context.Context, db *sqlx.DB, olderThan int64) (int64, error) {
	var ids []string
	if err := db.SelectContext(ctx, &ids, queryPurgeSearchJobIDs, olderThan); err != nil {
		return 0, fmt.Errorf("store: list purgeable search jobs: %w", err)
	}

	result, err := db.ExecContext(ctx, queryPurgeSearchJobs, olderThan)
	if err != nil {
		return 0, fmt.Errorf("store: purge search jobs: %w", err)
	}
	purged, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: purge search jobs: read rows affected: %w", err)
	}
	for _, id := range ids {
		Searches.Forget(id)
	}

	return purged, nil
}

// InsertResults writes one engine's page in a single transaction and returns
// the number of rows written. It is idempotent per (search_job_id,
// indexer_id): it deletes that indexer's rows first, because the job queue is
// at-least-once — a re-run replaces the page instead of doubling it.
func InsertResults(ctx context.Context, db *sqlx.DB, jobID, indexerID string, rows []SearchResultRow) (int, error) {
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin result insert: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "search result insert rollback failed", "err", err)
		}
	}()

	if _, err := tx.ExecContext(ctx, queryDeleteIndexerResults, jobID, indexerID); err != nil {
		return 0, fmt.Errorf("store: replace indexer %s results: %w", indexerID, err)
	}

	now := time.Now().UnixMilli()
	for _, r := range rows {
		r.ID = NewID(PrefixSearchResult)
		r.SearchJobID = jobID
		r.IndexerID = indexerID
		r.CreatedAt, r.UpdatedAt = now, now
		if _, err := tx.ExecContext(
			ctx, queryInsertSearchResult,
			r.ID, r.SearchJobID, r.IndexerID, r.Title, r.DownloadURL,
			r.MagnetURI, r.InfoHash, r.SizeBytes, r.Seeders, r.Leechers,
			r.Grabs, r.PublishedAt, r.DetailsURL, r.CategoryIDsJSON,
			r.CategoryDesc, r.DownloadVolumeFactor, r.UploadVolumeFactor,
			r.MinimumRatio, r.MinimumSeedTimeSeconds, r.IMDBID, r.TMDBID,
			r.TVDBID, r.Year, r.Genre, r.Language, r.Publisher, r.Author,
			r.Album, r.Artist, r.CreatedAt, r.UpdatedAt,
		); err != nil {
			return 0, fmt.Errorf("store: insert search result for indexer %s: %w", indexerID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit search results: %w", err)
	}

	return len(rows), nil
}

// CountSearchResults returns one job's total row count — the value
// FinishSearchJob stamps and the GET poll reports.
func CountSearchResults(ctx context.Context, db *sqlx.DB, jobID string) (int, error) {
	var n int
	if err := db.GetContext(ctx, &n, queryCountSearchResults, jobID); err != nil {
		return 0, fmt.Errorf("store: count results of %s: %w", jobID, err)
	}

	return n, nil
}

// CountResultsByIndexer returns the per-engine row counts of one job. The API
// rebuilds the engines array from it once the in-memory tracker has forgotten
// the job (a restart), where the volatile per-engine status no longer exists.
func CountResultsByIndexer(ctx context.Context, db *sqlx.DB, jobID string) (map[string]int, error) {
	var pairs []struct {
		IndexerID string `db:"indexer_id"`
		Count     int    `db:"count"`
	}
	if err := db.SelectContext(ctx, &pairs, queryCountResultsByIndexer, jobID); err != nil {
		return nil, fmt.Errorf("store: count results of %s: %w", jobID, err)
	}

	out := make(map[string]int, len(pairs))
	for _, p := range pairs {
		out[p.IndexerID] = p.Count
	}

	return out, nil
}

// searchSortSpec is one documented sort key of docs/05-api-contract.md
// section 9.2: its column and the cursor value extractor. Each key is
// defined exactly once — column and extractor cannot drift apart, which is
// what a separate map-plus-switch would allow.
type searchSortSpec struct {
	column string
	value  func(SearchResultRow) any
}

// searchSorts is the sort allowlist. Only values from this map reach ORDER
// BY or a cursor predicate; a user-supplied key is looked up, never
// concatenated.
var searchSorts = map[string]searchSortSpec{
	"seeders":      {"seeders", func(r SearchResultRow) any { return nilOrValue(r.Seeders) }},
	"title":        {"title", func(r SearchResultRow) any { return r.Title }},
	"size_bytes":   {"size_bytes", func(r SearchResultRow) any { return nilOrValue(r.SizeBytes) }},
	"leechers":     {"leechers", func(r SearchResultRow) any { return nilOrValue(r.Leechers) }},
	"published_at": {"published_at", func(r SearchResultRow) any { return nilOrValue(r.PublishedAt) }},
	"indexer":      {"indexer_id", func(r SearchResultRow) any { return r.IndexerID }},
}

// searchSortKeys renders the allowlist in sorted order for legible errors.
func searchSortKeys() []string {
	keys := make([]string, 0, len(searchSorts))
	for k := range searchSorts {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// defaultSearchSort is the documented default of GET /search/{id}: seeders
// descending.
const defaultSearchSort = "-seeders"

// ListResults pages one job's results. sort is one of seeders, title,
// size_bytes, leechers, published_at, indexer, each reversible with a leading
// '-'; the default is "-seeders". Every column sorts NULLS LAST in both
// directions — an unknown seeder count never outranks a real one. The cursor
// is a base64 JSON token holding the last row's sort value, its id and a hash
// of (job, sort), exactly like ListTasks; a mismatched or undecodable token
// is ErrStaleCursor. total counts the job's rows, ignoring the cursor
// (docs/05-api-contract.md section 1.4).
func ListResults(ctx context.Context, db *sqlx.DB, jobID, sort string, limit int, cursor string) (rows []SearchResultRow, nextCursor string, total int, err error) {
	if limit == 0 {
		limit = taskListDefaultLimit
	}
	if limit < 1 || limit > taskListMaxLimit {
		return nil, "", 0, fmt.Errorf("store: list results: limit %d outside 1..%d", limit, taskListMaxLimit)
	}

	sortKey := sort
	if sortKey == "" {
		sortKey = defaultSearchSort
	}
	key, descending := strings.CutPrefix(sortKey, "-")
	spec, ok := searchSorts[key]
	if !ok {
		return nil, "", 0, fmt.Errorf("%w: %q, want one of %q", ErrInvalidSort, sort, searchSortKeys())
	}
	column := spec.column

	pageQuery := queryListSearchResults
	pageArgs := []any{jobID}
	if cursor != "" {
		token, err := decodeTaskCursor(cursor)
		if err != nil {
			return nil, "", 0, err
		}
		if token.Hash != searchCursorHash(jobID, column, descending) {
			return nil, "", 0, fmt.Errorf("%w: issued for a different job or sort", ErrStaleCursor)
		}

		predicate, cursorArgs := searchCursorPredicate(column, descending, token.Value, token.LastID)
		pageQuery += " AND " + predicate
		pageArgs = append(pageArgs, cursorArgs...)
	}

	if err := db.GetContext(ctx, &total, queryCountSearchResults, jobID); err != nil {
		return nil, "", 0, fmt.Errorf("store: list results: count: %w", err)
	}

	direction := "ASC"
	if descending {
		direction = "DESC"
	}
	// One row past the limit decides whether another page exists, so
	// next_cursor is null exactly on the last page. NULLS LAST holds in
	// both directions: the cursor predicate treats the NULL cluster as the
	// tail of either ordering.
	pageQuery += fmt.Sprintf(
		" ORDER BY %s %s NULLS LAST, id %s LIMIT %d",
		column, direction, direction, limit+1,
	)

	var page []SearchResultRow
	if err := db.SelectContext(ctx, &page, pageQuery, pageArgs...); err != nil {
		return nil, "", 0, fmt.Errorf("store: list results: read page: %w", err)
	}
	if len(page) <= limit {
		return page, "", total, nil
	}
	page = page[:limit]

	last := page[len(page)-1]
	encoded, err := encodeTaskCursor(taskPageCursor{
		Hash:   searchCursorHash(jobID, column, descending),
		LastID: last.ID,
		Value:  spec.value(last),
	})
	if err != nil {
		return nil, "", 0, fmt.Errorf("store: list results: encode cursor: %w", err)
	}

	return page, encoded, total, nil
}

// searchCursorPredicate is the keyset predicate for the NULLS-LAST orderings
// ListResults produces. column comes from searchSortColumns only. A cursor
// inside the NULL cluster can only be followed by more NULLs; a non-NULL
// cursor is followed by the strictly-later non-NULL rows plus the whole NULL
// tail.
func searchCursorPredicate(column string, descending bool, value any, lastID string) (string, []any) {
	cmp := ">"
	if descending {
		cmp = "<"
	}
	if value == nil {
		return fmt.Sprintf("(%s IS NULL AND id %s ?)", column, cmp), []any{lastID}
	}

	return fmt.Sprintf(
		"(%s IS NULL OR %s %s ? OR (%s = ? AND id %s ?))",
		column, column, cmp, column, cmp,
	), []any{value, value, lastID}
}

// searchCursorHash binds a cursor to the job and the parsed sort that issued
// it.
func searchCursorHash(jobID, column string, descending bool) string {
	sum := sha256.Sum256([]byte(
		"job:" + jobID + "\x00" + fmt.Sprintf("sort:%s:%t", column, descending),
	))

	return base64.RawStdEncoding.EncodeToString(sum[:])
}

// EngineStatus is the volatile half of a search job: what each indexer is
// doing right now. status is queued | searching | done | error
// (docs/05-api-contract.md section 9.2).
type EngineStatus struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Status string  `json:"status" enum:"queued,searching,done,error"`
	Count  int     `json:"count"`
	Error  *string `json:"error"`
}

// Engine status vocabulary of the wire contract.
const (
	EngineQueued    = "queued"
	EngineSearching = "searching"
	EngineDone      = "done"
	EngineError     = "error"
)

// SearchTracker holds EngineStatus per running search job. One process, one
// tracker: the durable rows live in search_jobs and search_results, this
// holds only what is in flight. Searches is the process-wide instance both
// internal/api and internal/jobs use.
type SearchTracker struct {
	mu    sync.RWMutex
	byJob map[string][]EngineStatus
}

// Searches is the process-wide tracker instance.
var Searches = NewSearchTracker()

// NewSearchTracker returns an empty tracker.
func NewSearchTracker() *SearchTracker {
	return &SearchTracker{byJob: map[string][]EngineStatus{}}
}

// Start (re)seeds one job's engine list; the fan-out worker calls it with
// every engine "searching" before the goroutines launch, so a re-run of the
// same job row resets a stale snapshot instead of leaking it.
func (t *SearchTracker) Start(jobID string, engines []EngineStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byJob[jobID] = append([]EngineStatus(nil), engines...)
}

// Set updates one engine of one job. An unknown job — deleted, forgotten or
// never started — is a no-op: a row deleted under a running fan-out must not
// resurrect its tracker entry.
func (t *SearchTracker) Set(jobID, indexerID, status string, count int, errText *string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	engines, ok := t.byJob[jobID]
	if !ok {
		return
	}
	for i := range engines {
		if engines[i].ID == indexerID {
			engines[i].Status = status
			engines[i].Count = count
			engines[i].Error = errText
			return
		}
	}
}

// Snapshot returns a copy of one job's engine list; the bool is false once
// the job was forgotten (or never started), which is how the API knows to
// reconstruct the list from stored counts.
func (t *SearchTracker) Snapshot(jobID string) ([]EngineStatus, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	engines, ok := t.byJob[jobID]
	if !ok {
		return nil, false
	}

	return append([]EngineStatus(nil), engines...), true
}

// Forget drops one job's entry; DELETE /search/{id} and the retention purge
// call it.
func (t *SearchTracker) Forget(jobID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.byJob, jobID)
}

// IndexerSettingsMap decodes an indexer row's settings_json into the string
// map the dlsearch runner's Scope.Config reads — the reserved keys pass
// through so the runner sees allow_private_network. A stored document that is
// not valid JSON is treated as empty and logged: one corrupt row must not
// make the indexer unusable. The API's probe path and the job fan-out share
// this one decode so a setting can never drift between the two.
func IndexerSettingsMap(log *slog.Logger, row Indexer) map[string]string {
	out := map[string]string{}
	if row.SettingsJSON == nil || *row.SettingsJSON == "" {
		return out
	}
	var doc map[string]any
	if !json.Valid([]byte(*row.SettingsJSON)) {
		log.Warn("indexer settings_json is not valid JSON; using empty settings", "indexer_id", row.ID)
		return out
	}
	dec := json.NewDecoder(strings.NewReader(*row.SettingsJSON))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		log.Warn("indexer settings_json is not valid JSON; using empty settings", "indexer_id", row.ID)
		return out
	}
	for k, v := range doc {
		switch t := v.(type) {
		case string:
			out[k] = t
		case bool:
			out[k] = strconv.FormatBool(t)
		case json.Number:
			// verbatim — a float64 would render 1e+06 or lose precision
			out[k] = t.String()
		case nil:
			// JSON null carries no value; the key is dropped.
		default:
			// Nested objects and arrays are not runner config; fmt.Sprint
			// would smuggle Go-syntax garbage into Scope.Config, so the key
			// is dropped and logged like any other malformed value.
			log.Warn("indexer settings_json value is not a scalar; dropping key",
				"indexer_id", row.ID, "key", k)
		}
	}
	return out
}
