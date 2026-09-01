# T066 — Poll feeds with conditional GET, jitter and the Sonarr backoff ladder

| Field | Value |
|---|---|
| **ID** | T066 |
| **Milestone** | M5 |
| **Status** | todo |
| **Depends on** | T012, T054, T065 |
| **Blocks** | T067, T071, T072, T081, T083, T091, T117 |
| **Parallel-safe** | no — adds `POST /feeds/{id}/refresh` to T065's `internal/api/feeds.go` |
| **Implements** | [FR-071](../02-requirements.md#fr-071-poll-feeds-politely-with-conditional-get-and-backoff) |
| **Decisions** | [ADR-0015](../decisions/0015-db-backed-in-process-job-queue.md), [ADR-0009](../decisions/0009-native-cross-protocol-rss-rules.md) |
| **Est. size** | 3 new files, 4 touched, ~400 LOC |

## Goal
An `rss_poll` job fetches every due feed through the shared SSRF-guarded client, replays `ETag` and
`Last-Modified`, treats `304` as "nothing new", and reschedules with deterministic jitter. Consecutive
failures walk Sonarr's ten-step ladder; `POST /feeds/{id}/refresh` bypasses it and reports the outcome.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/08-rss-automation.md` §2 Feed model and polling](../08-rss-automation.md#2-feed-model-and-polling)
   — the defaults table, the jitter formula, the publisher hints, the conditional-GET headers and the
   failure model. Reproduce every value; invent none.
2. [`docs/05-api-contract.md` §10.1 Feeds](../05-api-contract.md#101-feeds) — the refresh response body.
3. [`docs/tasks/T065-feed-store-and-crud.md`](T065-feed-store-and-crud.md) — `DueFeeds`,
   `UpdateFeedFetchState`, `UpsertFeedItems` and `TrimFeedItems`, which this task calls and does not change.
4. [`docs/12-security-and-threat-model.md` §2.2 How to implement it](../12-security-and-threat-model.md#22-how-to-implement-it)
   — every outbound fetch uses the one guarded client; the body is capped while streaming.
5. [`docs/tasks/T012-job-worker-pool.md`](T012-job-worker-pool.md) — `jobs.Handler`, `Register` and
   `store.EnqueueJob`.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/rss/poll.go` | create | Scheduling, conditional GET, the size cap, the ladder, feed writes. |
| `internal/rss/poll_test.go` | create | `httptest` cases for 200/304/500, jitter and the ladder. |
| `internal/jobs/cron.go` | create | The `robfig/cron/v3` entry that enqueues the `rss_poll` job. |
| `internal/api/feeds.go` | edit | Add `POST /feeds/{id}/refresh`. |

No other file may be modified.

## Interface contract

```go
package rss

// ItemParser turns one fetched body into rows ready for upsert. T067 implements it in parse.go;
// this package must compile and be testable with a stub.
type ItemParser interface {
	ParseFeed(feedID, baseURL string, body []byte) (FeedMeta, []store.FeedItem, error)
}

// FeedMeta carries the channel-level publisher hints of 08-rss-automation.md section 2.2.
type FeedMeta struct {
	Title            string
	TTLMinutes       int            // <ttl>, already clamped to [5,1440]; 0 when absent
	ImpliedIntervalS int            // sy:updatePeriod / sy:updateFrequency; 0 when absent
	SkipHours        []int          // 0..23, GMT
	SkipDays         []time.Weekday
}

// Result is one poll outcome and is the body of POST /feeds/{id}/refresh.
type Result struct {
	Fetched     bool   `json:"fetched"`
	NotModified bool   `json:"not_modified"`
	ItemsAdded  int    `json:"items_added"`
	ElapsedMS   int64  `json:"elapsed_ms"`
	Error       string `json:"error,omitempty"`
}

type Poller struct{ /* db, hc, parser, log, now, globalIntervalS, startedAt */ }

// NewPoller takes the shared SSRF-guarded client built in internal/secure; the poller never
// constructs an http.Client of its own.
func NewPoller(db *sqlx.DB, hc *http.Client, p ItemParser, log *slog.Logger, now func() time.Time) *Poller

// Poll fetches one feed. force skips disabled_till and the ladder and is set by the refresh endpoint.
func (p *Poller) Poll(ctx context.Context, f store.Feed, force bool) (Result, error)

// PollDue is the jobs.Handler body for kind "rss_poll": 4 feeds in parallel, at most 1 per host.
func (p *Poller) PollDue(ctx context.Context, j store.Job) error

// BackoffPeriods is Sonarr's ladder, verbatim, in seconds.
var BackoffPeriods = [10]int64{0, 60, 300, 900, 1800, 3600, 10800, 21600, 43200, 86400}

// Jitter is deterministic per feed: ((int64(fnv1a(feedID)) % 2001) - 1000) / 10000.0, in [-0.10,+0.10].
func Jitter(feedID string) float64

// EffectiveInterval is max(configured, ttlMinutes*60, impliedIntervalS, 300) seconds, where
// configured is feeds.refresh_interval_s or, when that is 0, the rss_interval_s setting.
func EffectiveInterval(configuredS, globalS int, m FeedMeta) time.Duration

// NextFetchAt is now + EffectiveInterval * (1 + Jitter(feedID)), in unix milliseconds.
func NextFetchAt(now int64, d time.Duration, feedID string) int64
```

Request constants, exactly doc 08 §2.1 and §2.3:

```go
const (
	userAgent    = "dl-tool/%s (+https://github.com/L-K-M/dl-tool)"
	acceptHeader = "application/rss+xml, application/atom+xml, application/xml;q=0.9, text/xml;q=0.9, */*;q=0.5"
	acceptEnc    = "gzip, deflate"
	maxBody      = 16 << 20        // 16 MiB
	connTimeout  = 10 * time.Second
	totalTimeout = 60 * time.Second
	startupGrace = 15 * time.Minute
	pollParallel = 4
)
```

`etag` and `last_modified` are replayed **verbatim**, weak `W/"…"` prefix included, as `If-None-Match`
and `If-Modified-Since`.

## Steps
1. Create `internal/rss/poll.go` with the constants, `Jitter`, `EffectiveInterval`, `NextFetchAt`,
   `BackoffPeriods`, `NewPoller`, `Poll` and `PollDue`.
2. Build each request with the three headers plus the two conditional headers when the stored validators
   exist, read the body through `io.LimitReader(resp.Body, maxBody+1)`, and fail the poll with
   `feed body exceeds 16 MiB` when the limit is passed.
3. On `304`: set `last_fetch_at` and `last_success_at`, decrement `escalation_level` by one with a floor of
   `0`, clear `last_error`, reschedule, and parse nothing.
4. On `200`: call `p.parser.ParseFeed`, `store.UpsertFeedItems`, then `store.TrimFeedItems` with the feed's
   `item_cap`, store the new `etag`, `last_modified` and `ttl_minutes`, and reset `last_error` to `NULL`.
5. On any failure: increment `escalation_level` by one, clamp to `9`, set
   `disabled_till = now + BackoffPeriods[level]`, store the error text in `last_error`, and never
   auto-disable within `startupGrace` of process start. The level is **decremented** on success, never reset.
6. Honour `skipHours`/`skipDays` by rescheduling to the next allowed hour without fetching, and derive the
   one-request-per-host rule in memory from `url.Parse(f.URL).Host`; `internal/store/feeds.go` gains
   nothing.
7. Create `internal/jobs/cron.go` with `NewScheduler(db, log)`, `Start(ctx)` and one `@every 1m` entry that
   enqueues a single `rss_poll` job when no pending row of that kind exists; stop the cron when `ctx` ends.
8. Edit `internal/api/feeds.go` to add `POST /feeds/{id}/refresh`, calling `Poll(ctx, f, true)` and
   returning the `Result` body of doc 05 §10.1; a fetch failure is still `200` with `error` set.
9. Create `internal/rss/poll_test.go` against `httptest.Server`: a `200` stores items and both validators;
   the second poll sends `If-None-Match` and `If-Modified-Since` and a `304` adds no item; three
   consecutive `500`s give levels 1, 2, 3 and `disabled_till` deltas of 60, 300 and 900 seconds; a success
   after them decrements to 2; a 17 MiB body fails without an item; `Jitter` is stable per id and within
   `[-0.10, +0.10]`; `EffectiveInterval` never returns under 300 s.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestConditionalGetSendsBothValidators` and `TestNotModifiedAddsNoItems` pass.
- [ ] `TestBackoffLadderEscalatesAndDecrements` asserts the exact seconds 60, 300, 900.
- [ ] `TestBodyCapRejectsOversizeFeed` passes and no partial item is stored.
- [ ] `TestJitterIsDeterministicAndBounded` passes.
- [ ] `POST /feeds/{id}/refresh` polls a feed whose `disabled_till` is in the future.
- [ ] No `http.Client` is constructed in `internal/rss`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/rss/... ./internal/jobs/... ./internal/api/..." && echo POLL_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/rss`, `ok  github.com/L-K-M/dl-tool/internal/jobs` and
`ok  github.com/L-K-M/dl-tool/internal/api`, every test named above reported as `--- PASS`, and the final
line of stdout is exactly `POLL_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement `ParseFeed`; T067 owns `internal/rss/parse.go`. Test against a stub parser.
- Do NOT evaluate rules or create tasks after a poll; T071 wires the matcher into `Poll`.
- Do NOT retry inside one poll. Retries are the ladder's job (doc 08 §2.1, retries `0`).
- Do NOT reset `escalation_level` to zero on success, and do NOT delete a feed that keeps failing.
- Do NOT read `<ttl>` as seconds; BEP 36 says seconds, dl-tool reads minutes (doc 08 §2.2).

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
