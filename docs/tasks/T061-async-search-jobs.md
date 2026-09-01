# T061 — Run a search as an asynchronous job

| Field | Value |
|---|---|
| **ID** | T061 |
| **Milestone** | M4 |
| **Status** | todo |
| **Depends on** | T012, T055, T058 |
| **Blocks** | T062, T063, T064 |
| **Parallel-safe** | no — extends T055's `internal/api/search.go` and registers a job kind in `cmd/dl-tool/main.go` |
| **Implements** | [FR-054](../02-requirements.md#fr-054-run-a-search-as-an-asynchronous-job) |
| **Decisions** | [ADR-0015](../decisions/0015-db-backed-in-process-job-queue.md), [ADR-0008](../decisions/0008-torznab-first-declarative-yaml-second.md) |
| **Est. size** | 2 new files, ~430 LOC |

## Goal
`POST /api/v1/search` returns `202 {"id":"sch_…"}` immediately and enqueues one `jobs` row of kind `search`.
The worker fans out one goroutine per selected indexer and writes `search_results` as each answers, so
`GET /search/{id}` reports partial results with `finished:false` and later `finished:true`.
`DELETE /search/{id}` removes the job and cascades to its results.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §9.2 Search job lifecycle](../05-api-contract.md#92-search-job-lifecycle) — the
   sequence diagram, the three bodies, the status codes and the sort allowlist.
2. [`docs/04-data-model.md` §3.4 Search](../04-data-model.md#34-search) — the `search_jobs` and
   `search_results` DDL, including the `ON DELETE CASCADE`.
3. [`docs/04-data-model.md` §3.6 Jobs, schedule and preferences](../04-data-model.md#36-jobs-schedule-and-preferences)
   — the claim statement, at-least-once semantics and the backoff.
4. [`docs/07-search-and-indexers.md` §1 Two-tier design](../07-search-and-indexers.md#1-two-tier-design) — the
   fan-out diagram and the Download Station job shape the UI is built around.
5. [`docs/05-api-contract.md` §1.4 Cursor pagination](../05-api-contract.md#14-cursor-pagination) — `limit`,
   `cursor` and `total` apply to `results` only.
6. [`docs/tasks/T012-job-worker-pool.md`](T012-job-worker-pool.md) — `jobs.Handler`, `Register` and
   `EnqueueJob`.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/store/search.go` | create | `SearchJob`, `SearchResultRow`, their persistence, and the in-process `Tracker`. |
| `internal/jobs/handlers_search.go` | create | The `search` job handler and the per-indexer fan-out. |
| `internal/api/search.go` | modify | `POST /search`, `GET /search/{id}`, `DELETE /search/{id}`. |
| `internal/api/search_test.go` | modify | Lifecycle cases against two stub indexers. |
| `cmd/dl-tool/main.go` | edit | Register the `search` job kind on the worker. |

No other file may be modified.

## Interface contract

```go
package store

type SearchJob struct {
	ID             string  `db:"id"`
	OwnerID        string  `db:"owner_id"`
	Query          string  `db:"query"`
	IndexerIDsJSON string  `db:"indexer_ids_json"`
	CategoriesJSON *string `db:"categories_json"`
	Finished       bool    `db:"finished"`
	Total          int     `db:"total"`
	LastError      *string `db:"last_error"`
	StartedAt      int64   `db:"started_at"`
	FinishedAt     *int64  `db:"finished_at"`
}

func CreateSearchJob(ctx context.Context, db *sqlx.DB, j SearchJob) (SearchJob, error)
func GetSearchJob(ctx context.Context, db *sqlx.DB, id, ownerID string) (SearchJob, error) // ErrNotFound
func FinishSearchJob(ctx context.Context, db *sqlx.DB, id string, total int, lastErr *string, now int64) error
func DeleteSearchJob(ctx context.Context, db *sqlx.DB, id, ownerID string) error
func PurgeSearchJobs(ctx context.Context, db *sqlx.DB, olderThan int64) (int64, error) // 24 h retention

// SearchResultRow mirrors the search_results DDL. Package store never imports
// internal/search, so the job handler converts search.SearchResult into this type.
type SearchResultRow struct {
	ID          string  `db:"id"`
	SearchJobID string  `db:"search_job_id"`
	IndexerID   string  `db:"indexer_id"`
	Title       string  `db:"title"`
	DownloadURL *string `db:"download_url"`
	MagnetURI   *string `db:"magnet_uri"`
	InfoHash    *string `db:"info_hash"`
	SizeBytes   *int64  `db:"size_bytes"`
	Seeders     *int    `db:"seeders"`
	Leechers    *int    `db:"leechers"`
	Grabs       *int    `db:"grabs"`
	PublishedAt *int64  `db:"published_at"` // unix ms
	DetailsURL  *string `db:"details_url"`
	// plus category_ids_json, category_desc, download_volume_factor,
	// upload_volume_factor, minimum_ratio, minimum_seed_time_seconds, imdb_id, tmdb_id,
	// tvdb_id, year, genre, language, publisher, author, album, artist, exactly as
	// 04-data-model.md section 3.4 declares them.
}

// InsertResults writes one engine's page in a single transaction and returns the number
// of rows written. It is idempotent per (search_job_id, indexer_id): it deletes that
// indexer's rows first, because the job queue is at-least-once.
func InsertResults(ctx context.Context, db *sqlx.DB, jobID, indexerID string, rows []SearchResultRow) (int, error)

// ListResults pages one job's results. sort is one of seeders, title, size_bytes,
// leechers, published_at, indexer, each reversible with a leading '-'; the default is
// "-seeders". NULL seeders sort last in both directions.
func ListResults(ctx context.Context, db *sqlx.DB, jobID, sort string, limit int, cursor string) (rows []SearchResultRow, nextCursor string, total int, err error)

// EngineStatus is the volatile half of a search job: what each indexer is doing right
// now. status is queued | searching | done | error.
type EngineStatus struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Status string  `json:"status"`
	Count  int     `json:"count"`
	Error  *string `json:"error"`
}

// Tracker holds EngineStatus per running search job. One process, one tracker: the
// durable rows live in search_jobs and search_results, this holds only what is in
// flight. Searches is the process-wide instance both internal/api and internal/jobs use.
type Tracker struct{ /* mu sync.RWMutex; byJob map[string][]EngineStatus */ }

var Searches = NewTracker()

func NewTracker() *Tracker
func (t *Tracker) Start(jobID string, engines []EngineStatus)
func (t *Tracker) Set(jobID, indexerID, status string, count int, errText *string)
func (t *Tracker) Snapshot(jobID string) ([]EngineStatus, bool) // false once forgotten
func (t *Tracker) Forget(jobID string)
```

```go
package jobs

// SearchPayload is the jobs.payload_json of kind "search".
type SearchPayload struct {
	SearchJobID string   `json:"search_job_id"`
	Query       string   `json:"query"`
	IndexerIDs  []string `json:"indexer_ids"`
	Categories  []int    `json:"categories"`
}

// NewSearchHandler returns the handler for jobs.kind "search". It marks every selected
// indexer "searching", runs one goroutine per indexer with the per-engine 15 s deadline,
// writes each engine's rows as they arrive, and finishes the job when the last goroutine
// returns. max_attempts for this kind is 1: a search is re-run by the user, not retried.
func NewSearchHandler(db *sqlx.DB, log *slog.Logger, reg *search.Registry, run *search.Runner,
	idx *store.IndexerStore, hc *http.Client) Handler
```

```go
package api

type StartSearchInput struct {
	Body struct {
		Query      string   `json:"query"      minLength:"1"`
		IndexerIDs []string `json:"indexer_ids,omitempty"`
		Categories []int    `json:"categories,omitempty"`
	}
}

type SearchJobOutput struct {
	Body struct {
		ID         string               `json:"id"`
		Query      string               `json:"query"`
		Finished   bool                 `json:"finished"`
		Total      int                  `json:"total"`
		Engines    []store.EngineStatus `json:"engines"`
		Results    []SearchResultDTO    `json:"results"`
		NextCursor *string              `json:"next_cursor"`
	}
}

func (h *SearchHandlers) StartSearch(ctx context.Context, in *StartSearchInput) (*StartedOutput, error)
func (h *SearchHandlers) GetSearch(ctx context.Context, in *GetSearchInput) (*SearchJobOutput, error)
func (h *SearchHandlers) DeleteSearch(ctx context.Context, in *SearchIDInput) (*struct{}, error)
```

Operation ids added inside `RegisterSearchRoutes`: `start-search` (`202`), `get-search`, `delete-search`.

## Steps
1. Create `internal/store/search.go` with the two row structs, the six persistence functions and the
   `Tracker`, using explicit column lists and `?` placeholders throughout.
2. Encode the `ListResults` cursor as base64 JSON holding the last row's sort value, its `id` and a hash of
   the sort key, returning `ErrStaleCursor` on a mismatch, exactly like `ListTasks`.
3. Create `internal/jobs/handlers_search.go` with `SearchPayload` and `NewSearchHandler`. Resolve each
   indexer row, pick the Torznab client or the dlsearch `Runner` from `indexers.kind`, and run each in its own
   goroutine under an `errgroup`-free `sync.WaitGroup` with a per-engine `context.WithTimeout` of 15 s.
4. Convert each `search.SearchResult` into a `store.SearchResultRow`, timestamps to unix milliseconds, in
   the job handler. Mark each engine `searching` before its request, `done` with its row count on success, and `error` with
   the upstream message on failure; write results with `InsertResults` as each engine returns, never at the
   end.
5. Finish the job with `FinishSearchJob`, setting `total` to the sum of written rows, then leave the tracker
   snapshot in place until `DELETE /search/{id}` or the 24-hour purge calls `Forget`.
6. Add `StartSearch` to `internal/api/search.go`: validate a non-empty query, default `indexer_ids` to every
   enabled indexer, return `503` `/problems/engine-unavailable` when none is enabled and `422` for an unknown
   id, create the `search_jobs` row owned by the caller, seed the tracker with every engine `queued`, enqueue
   the job with `EnqueueJob(ctx, db, "search", nil, payload, now)` and answer `202`.
7. Add `GetSearch`: read the job by id **and** owner, take the tracker snapshot, page the results, and
   reconstruct the engine list from `search_results` counts when the tracker has forgotten the job.
8. Add `DeleteSearch`: delete the row, rely on the `ON DELETE CASCADE` for results, call `Forget`, and answer
   `204`; a job owned by another user answers `404`, never `403`.
9. Register the three operations inside `RegisterSearchRoutes`, and edit `cmd/dl-tool/main.go` to call
   `worker.Register("search", jobs.NewSearchHandler(...))` in `OnStart`.
10. Extend `internal/api/search_test.go` with `TestStartSearchReturns202AndID`,
    `TestPollShowsPartialThenFinished` against two stub indexers with 50 ms and 400 ms latencies,
    `TestDeleteRemovesJobAndResults`, `TestOtherUsersJobIs404`, `TestEmptyQueryIs422` and
    `TestNoEnabledIndexerIs503`.
11. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `POST /search` answers `202` with only `{"id":"sch_…"}` and does not block on any indexer.
- [ ] `TestPollShowsPartialThenFinished` asserts a first poll with `finished:false` and a non-empty `results`, and a later poll with `finished:true`.
- [ ] `DELETE /search/{id}` leaves zero rows in `search_results` for that job.
- [ ] A second execution of the same job row writes no duplicate results.
- [ ] `GET /search/{id}` for a job owned by another user returns `404` `/problems/not-found`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/api/... ./internal/store/... ./internal/jobs/..." && echo SEARCH_JOB_OK
```
Expected: three `ok  	github.com/L-K-M/dl-tool/internal/...` lines, every test named in step 10 listed as
passing, and the final line of stdout exactly `SEARCH_JOB_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT dedupe across engines and do NOT merge duplicate rows; T062 owns `Dedup`.
- Do NOT stream search progress over SSE; the UI polls, exactly as Download Station does.
- Do NOT retry a failed engine inside the job; `max_attempts` for kind `search` is 1.
- Do NOT persist the engine status in a new column; the tracker is in memory by design.
- Do NOT add saved searches; T064 owns them.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
