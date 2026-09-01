# T062 — Report per-engine status and collapse duplicate results

| Field | Value |
|---|---|
| **ID** | T062 |
| **Milestone** | M4 |
| **Status** | todo |
| **Depends on** | T054, T058, T061 |
| **Blocks** | T063, T105 |
| **Parallel-safe** | no — extends T061's `internal/jobs/handlers_search.go` |
| **Implements** | [FR-055](../02-requirements.md#fr-055-surface-per-engine-status-and-errors) |
| **Decisions** | [ADR-0008](../decisions/0008-torznab-first-declarative-yaml-second.md) |
| **Est. size** | 1 new file, ~330 LOC |

## Goal
Every poll of `GET /search/{id}` names each selected indexer with `queued`, `searching`, `done` and a count,
or `error` with the upstream HTTP status and message. A search in which every indexer failed returns
`finished:true`, `total:0` and an `engines` array of errors, never a bare empty result set. Results that
several engines returned appear once.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §9.2 Search job lifecycle](../05-api-contract.md#92-search-job-lifecycle) — the
   `engines[]` vocabulary and the four rules under the example body.
2. [`docs/07-search-and-indexers.md` §2.5 Errors](../07-search-and-indexers.md#25-errors) — the Torznab error
   codes, what each does to the engine, and the four Prowlarr deviations including `Retry-After`.
3. [`docs/07-search-and-indexers.md` §5 The normalised `SearchResult`](../07-search-and-indexers.md#5-the-normalised-searchresult)
   — the deduplication rule and the five normalisation rules.
4. [`docs/09-web-ui-spec.md` §7 Search screen](../09-web-ui-spec.md#7-search-screen) — the per-indexer status
   strip and the three distinct zero states this data has to drive.
5. [`docs/tasks/T061-async-search-jobs.md`](T061-async-search-jobs.md) — `Tracker`, `EngineStatus`,
   `NewSearchHandler`.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/jobs/handlers_search.go` | modify | Error classification, `Retry-After`, the all-failed outcome. |
| `internal/jobs/handlers_search_test.go` | create | Fan-out, one-engine-fails and all-engines-fail cases. |
| `internal/search/normalize.go` | modify | Add `Dedup` and `NormaliseTitle`. |
| `internal/api/search.go` | modify | Collapse duplicates on read and always emit `engines[]`. |
| `internal/api/search_test.go` | modify | The FR-055 mixed-outcome case. |

No other file may be modified.

## Interface contract

```go
package search

// Dedup collapses results that several engines returned: by Infohash when present,
// otherwise by (NormaliseTitle(Title), SizeBytes). The surviving row is the one with the
// highest non-nil Seeders; ties keep the first, so the caller's ordering decides.
// 05-api-contract.md fixes the result object, so the collapsed engines are not named in
// the response; their rows stay in search_results.
func Dedup(in []SearchResult) []SearchResult

// NormaliseTitle lower-cases, collapses runs of whitespace, dots and underscores to a
// single space and trims. It is used for dedup only, never for display.
func NormaliseTitle(s string) string
```

```go
package jobs

// EngineFailure classifies what an engine did, so the tracker can carry a message a user
// can act on. Text is what reaches EngineStatus.Error verbatim.
type EngineFailure struct {
	Text       string        // e.g. "HTTP 503 from https://academictorrents.com/rss.xml"
	Disable    bool          // codes 100, 101, 102, 910: credentials or API disabled
	DisableMode string       // codes 202, 203: this t= mode only
	RetryAfter time.Duration // honoured from the Retry-After header on 429
}

// classify maps a transport error, a *search.TorznabError or an HTTP status onto an
// EngineFailure, following doc 07 section 2.5 row for row, including Prowlarr's
// non-spec 400, 410 Gone and 429 answers. Code 300 is not a failure: it is an empty
// result set.
func classify(err error, status int, url string) *EngineFailure
```

```go
package api

// SearchResultDTO is 05-api-contract.md section 9.2 verbatim. indexer_name is joined
// from the indexers row so the grid never needs a second request.
type SearchResultDTO struct {
	ID          string  `json:"id"`
	IndexerID   string  `json:"indexer_id"`
	IndexerName string  `json:"indexer_name"`
	Title       string  `json:"title"`
	// ... the remaining members exactly as doc 05 section 9.2 lists them
}
```

## Steps
1. Add `Dedup` and `NormaliseTitle` to `internal/search/normalize.go`; `Dedup` preserves input order for the
   surviving rows.
2. Add `classify` and `EngineFailure` to `internal/jobs/handlers_search.go`, mapping every row of doc 07
   §2.5: 100/101/102 and 910 mark the indexer `error` and set `last_error` to "check API key" plus the
   upstream text; 200/201/900 fail this engine only; 202/203 disable that mode for the engine; 300 yields an
   empty result set with status `done`; Prowlarr's `410 Gone` reads as "engine disabled upstream", not a
   transport failure.
3. Honour `Retry-After` on 429 and 503 by ending the engine with status `error` and the wait in the message;
   do not sleep past the per-engine 15 s deadline.
4. Record every failure with `store.Searches.Set(jobID, indexerID, "error", 0, &text)` and with
   `idx.RecordTest(ctx, indexerID, now, &text)` so the settings table shows the same message.
5. When every engine ended in `error`, finish the job with `total = 0` and `last_error` set to the count of
   failed engines; the job is `finished`, never left running.
6. In `internal/api/search.go`, run `search.Dedup` over the page read from `ListResults` before mapping to
   `SearchResultDTO`, and always emit `engines[]` — for a queued job, for a finished job, and for a job the
   tracker has forgotten, whose engines are reconstructed as `done` with their persisted counts.
7. Create `internal/jobs/handlers_search_test.go` with `TestFanOutWritesPerEngine`,
   `TestOneEngineFailsOthersSucceed` (a stub returning `HTTP 503`), `TestAllEnginesFailFinishesWithErrors`,
   `TestTorznabErrorCodeClassification` (a table over 100, 200, 202, 300, 410, 429, 910) and
   `TestRetryAfterIsReported`.
8. Extend `internal/api/search_test.go` with `TestEnginesArrayCarriesErrorAndResults` and
   `TestDuplicateAcrossEnginesAppearsOnce`.
9. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestOneEngineFailsOthersSucceed` asserts the failing indexer has `status:"error"` with the upstream status in `error`, while the other indexer's results are present.
- [ ] `TestAllEnginesFailFinishesWithErrors` asserts `finished:true`, `total:0` and a non-empty `engines[]`.
- [ ] `TestTorznabErrorCodeClassification` covers every row of doc 07 §2.5 and asserts code `300` is not a failure.
- [ ] `TestDuplicateAcrossEnginesAppearsOnce` asserts two engines returning one infohash produce one result row, keeping the higher seeder count.
- [ ] No response omits `engines[]`, including for a job whose tracker entry was forgotten.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/jobs/... ./internal/search/... ./internal/api/..." && echo SEARCH_STATUS_OK
```
Expected: three `ok  	github.com/L-K-M/dl-tool/internal/...` lines, every test named in steps 7 and 8 listed
as passing, and the final line of stdout exactly `SEARCH_STATUS_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add a response field naming the other engines that returned a duplicate; doc 05 §9.2 fixes the
  result object.
- Do NOT delete a duplicate row from `search_results`; collapsing happens on read.
- Do NOT retry a failed engine automatically, and do NOT disable an indexer row for a transient 503.
- Do NOT push search progress over SSE; the search screen polls.
- Do NOT change the sort allowlist or the cursor codec; T061 owns them.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
