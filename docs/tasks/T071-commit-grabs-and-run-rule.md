# T071 — Commit rule matches as tasks and run a rule against existing items

| Field | Value |
|---|---|
| **ID** | T071 |
| **Milestone** | M5 |
| **Status** | done |
| **Depends on** | T020, T024, T065, T066, T067, T069, T070 |
| **Blocks** | T073 |
| **Parallel-safe** | no — extends `internal/rss/poll.go`, `internal/api/rules.go`, `internal/api/server.go` and `cmd/dl-tool/main.go` |
| **Implements** | [FR-077](../02-requirements.md#fr-077-run-a-rule-against-existing-items) |
| **Decisions** | [ADR-0009](../decisions/0009-native-cross-protocol-rss-rules.md), [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| **Est. size** | 2 new files, ~420 LOC plus the `matched_rules` join the T065 repair assigned here and the composition-root wiring the F251 repair assigned here |

## Goal
Steps 12 to 14 of the algorithm become real: a successful poll evaluates every enabled rule, the winner of
each `content_key` group becomes a task through the same path `POST /tasks` uses, losers are recorded as
`fallback`, and `POST /rules/{id}/run` applies a saved rule to items already stored.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/08-rss-automation.md` §5 The matching algorithm](../08-rss-automation.md#5-the-matching-algorithm)
   — steps 12, 13 and 14, and the `queued` / `sent` / `fallback` status rule under the step list.
2. [`docs/05-api-contract.md` §10.2 Rule CRUD](../05-api-contract.md#102-rule-crud) — the
   `POST /rules/{id}/run` response and why `created_task_ids` can be shorter than `matched`.
3. [`docs/04-data-model.md` §3.5 RSS](../04-data-model.md#35-rss) — `rule_matches` and
   `rule_seen_episodes`, including the unique partial index on `info_hash`.
4. [`docs/tasks/T020-create-tasks-endpoint.md`](T020-create-tasks-endpoint.md) — `CreateTasksInput` and
   `CreateTasks`, the one creation path a grab is allowed to use.
5. [`docs/tasks/T069-rule-matching-algorithm.md`](T069-rule-matching-algorithm.md) — `Evaluate`,
   `Resolve`, `Candidate` and `State`.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/rss/grab.go` | create | `dbState`, `Commit`, `RunRule` and the `TaskCreator` seam. |
| `internal/rss/grab_test.go` | create | Commit, fallback, cap, cooldown and run-against-existing cases. |
| `internal/rss/poll.go` | edit | Evaluate every enabled rule after a successful poll. |
| `internal/api/rules.go` | edit | Add `POST /rules/{id}/run` and the `TaskCreator` adapter. |
| `internal/api/rules_test.go` | edit | The endpoint's report and its status codes. |
| `internal/store/feeds.go` | edit | Add `ListItemMatchedRules`, the `rule_matches` join that fills `matched_rules`. |
| `internal/api/feeds.go` | edit | Render `matched_rules` in place of T065's interim `[]`. |
| `internal/api/feeds_test.go` | edit | Pin the populated member. |
| `internal/rss/poll_test.go` | edit | Pass the added `TaskCreator` argument at the two `NewPoller` call sites. |
| `internal/api/server.go` | edit | Build the one `ruleTaskCreator`, hand it to `NewRuleHandlers` and `NewFeedHandlers`, expose it as `Server.RuleCreator`, and pass `rss.NewParser` as the feed parser. |
| `cmd/dl-tool/main.go` | edit | Give the `rss_poll` poller `rss.NewParser` and `server.RuleCreator`. |

No other file may be modified.

## Interface contract

```go
package rss

// TaskCreator hands a grabbed item to the ordinary task-creation path. internal/api implements it
// by calling TaskHandlers.CreateTasks, so a rule grab passes the same normalisation, routing,
// destination containment and concurrency checks a pasted URI passes.
type TaskCreator interface {
	CreateForRule(ctx context.Context, g GrabRequest) (taskID string, err error)
}

// GrabRequest is one accepted candidate, expressed in the vocabulary of POST /tasks.
type GrabRequest struct {
	URI           string
	Destination   string // rule action.destination, empty means the server default
	Category      string
	Paused        bool
	ContentLayout string // original | subfolder | no_subfolder
	Engine        string // rule action.engine, empty means let the router decide
	RuleID        string
	FeedItemID    string
}

// CommitReport is what one rule produced in one cycle.
type CommitReport struct {
	Evaluated      int
	Matched        int
	CreatedTaskIDs []string
	Fallbacks      int
}

// Commit performs steps 13 and 14 for one rule: Resolve, then for each winner insert a
// rule_matches row with status 'queued', call the TaskCreator, move the row to 'sent' with its
// task_id, or to 'failed' with the error text. Losers are inserted as 'fallback'. It stages the
// episode keys into rule_seen_episodes, sets rules.last_match_at to the winner's published_at,
// and stops after throttle.max_per_run grabs when that value is non-zero.
func Commit(ctx context.Context, db *sqlx.DB, doc RuleDoc, rule store.Rule,
	cands []Candidate, tc TaskCreator, now int64) (CommitReport, error)

// RunRule evaluates one saved rule against the items already stored for its feeds and commits the
// result. It is the body of POST /rules/{id}/run and of the post-poll pass.
func RunRule(ctx context.Context, db *sqlx.DB, ruleID string, limit int,
	tc TaskCreator, now int64) (CommitReport, error)

// RunAllRules evaluates every enabled rule in (priority ASC, name ASC) order over the items of one
// feed. poll.go calls it after a successful fetch that added at least one item.
func RunAllRules(ctx context.Context, db *sqlx.DB, feedID string, tc TaskCreator, now int64) error

// NewPoller gains the TaskCreator the post-poll pass runs RunAllRules with; the Poller stores
// it beside the parser. A nil creator disables the pass, which keeps the T066 poll tests
// valid with a nil argument.
func NewPoller(db *sqlx.DB, hc *http.Client, p ItemParser, tc TaskCreator, log *slog.Logger, now func() time.Time) *Poller

// dbState is the State of T069 backed by rule_matches, rule_seen_episodes and tasks.
type dbState struct{ /* db */ }
```

```go
package api

// ruleTaskCreator adapts the existing task handlers to rss.TaskCreator. It builds the same body
// POST /tasks accepts and reuses its validation; it never writes the tasks table directly.
type ruleTaskCreator struct{ tasks *TaskHandlers }

func (c ruleTaskCreator) CreateForRule(ctx context.Context, g rss.GrabRequest) (string, error)

type RunRuleInput struct{ ID string `path:"id"` }
type RunRuleOutput struct {
	Body struct {
		Evaluated      int      `json:"evaluated"`
		Matched        int      `json:"matched"`
		CreatedTaskIDs []string `json:"created_task_ids"`
		ElapsedMS      int64    `json:"elapsed_ms"`
	}
}

func (h *RuleHandlers) RunRule(ctx context.Context, in *RunRuleInput) (*RunRuleOutput, error)

// NewServer hoists NewTaskHandlers into a local, builds one ruleTaskCreator from it, and hands
// it to NewRuleHandlers and NewFeedHandlers; the exported RuleCreator field carries the same
// instance to cmd/dl-tool, which gives it to the rss_poll poller.
RuleCreator rss.TaskCreator

func NewRuleHandlers(db *sqlx.DB, tc rss.TaskCreator) *RuleHandlers
func NewFeedHandlers(db *sqlx.DB, hc *http.Client, p rss.ItemParser, tc rss.TaskCreator, log *slog.Logger) *FeedHandlers
```

A rule-created task goes through the ordinary creation path, so a grab counts against the concurrency
limits exactly like a manual add. Statuses: `200` · `404` for an unknown rule id ·
`503 /problems/engine-unavailable` when the engine refuses every grab.

## Steps
1. Create `internal/rss/grab.go` with `dbState`, implementing `HasInfoHash` against `rule_matches` and
   `tasks` — a 40-hex value against `infohash_v1`, a 64-hex value against `infohash_v2`, never truncated —
   `SeenEpisode` against `rule_seen_episodes`, and `BestScoreForContentKey` against `rule_matches`.
2. Implement `Commit`: `Resolve` first, then one `sqlx.Tx` per candidate that inserts the `rule_matches`
   row before the hand-off so a crash mid-grab leaves a `queued` row, never a silent loss.
3. Set `status = 'sent'` and `task_id` after a successful `CreateForRule`; on error set
   `status = 'failed'`, store the message in `reason`, and continue with the next candidate.
4. Insert losers with `status = 'fallback'` and the same `content_key`, so a failed hand-off can be
   retried with the runner-up.
5. Write `rule_seen_episodes` rows for the winners' staged keys only, honour
   `throttle.max_per_run`, and set `rules.last_match_at` with `store.SetRuleLastMatchAt` to the last
   winner's `published_at`.
6. Implement `RunRule` and `RunAllRules` over `store.ListRules(ctx, db, true)` in `(priority ASC,
   name ASC)` order, reusing `Evaluate` and never re-implementing a step.
7. Edit `internal/rss/poll.go` so a `200` that added items calls `RunAllRules`; a `304` must not, and a
   rule error must be logged without failing the poll or advancing the backoff ladder.
8. Edit `internal/api/rules.go` with `ruleTaskCreator` and the `POST /rules/{id}/run` operation, returning
   the doc 05 §10.2 body and measuring `elapsed_ms`.
9. Create `internal/rss/grab_test.go`: twenty stored items and a rule matching three give
   `evaluated=20, matched=3` and three tasks; two releases sharing a `content_key` create one task and one
   `fallback` row; `max_per_run: 1` stops after one grab; a duplicate info hash across two feeds creates
   one task; a `CreateForRule` error leaves `status='failed'` and does not stage the episode key; a second
   `RunRule` over unchanged items creates nothing.
10. Edit `internal/api/rules_test.go` for the endpoint: `200` with `created_task_ids`, `404` for an unknown
    id, and `created_task_ids` shorter than `matched` when dedup suppressed a grab.
11. Add `ListItemMatchedRules(ctx, db, ids []string)` to `internal/store/feeds.go` — one join over
    `rule_matches` to `rules` returning each item's matching rules as `{id, name}` pairs, so
    `GET /feeds/{id}/items` renders the `matched_rules` member of doc 05 §10.1 instead of the interim `[]`
    T065 ships: wire the map into `internal/api/feeds.go`, defaulting items with no matches to `[]`, and
    pin it with `TestFeedItemsMatchedRules` in `internal/api/feeds_test.go`. Every stored `rule_matches`
    row represents a rule that matched the item, so no status filter applies. An empty `ids` slice
    returns an empty map without querying — an empty items page is a normal request, and skipping the
    query keeps that case independent of how the engine parses an empty `IN` list.
12. Edit `internal/api/server.go`: hoist the `NewTaskHandlers` call into a `tasks` local above the
    `Server` literal, build `ruleTaskCreator{tasks: tasks}` once, and set `tasks: tasks`,
    `feeds: NewFeedHandlers(db, searchDeps.HTTP, rss.NewParser(time.Now), creator, log)`,
    `rules: NewRuleHandlers(db, creator)` and `RuleCreator: creator` on the literal, importing
    `internal/rss`. Edit `cmd/dl-tool/main.go` so the `rss_poll` job's poller is built with
    `rss.NewParser(time.Now)` and `server.RuleCreator`. Update the two `NewPoller` calls in
    `internal/rss/poll_test.go` for the added parameter. One creator instance then serves the
    run endpoint, the refresh poller and the cron poller, and both parser injection points
    close T067's nil-parser deferral in the same pass.
13. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [x] `TestRunRuleReportsEvaluatedAndGrabbed` asserts `evaluated=20` and three created tasks.
- [x] `TestContentKeyContestCreatesOneTaskAndOneFallback` passes.
- [x] `TestMaxPerRunCaps` and `TestFailedHandoffDoesNotStageEpisodeKey` pass.
- [x] `TestSecondRunIsIdempotent` asserts no second task and no second `rule_matches` row.
- [x] A `304` poll runs no rule.
- [x] Every task is created through `CreateForRule`; `internal/rss` contains no `INSERT INTO tasks`.
- [x] `TestFeedItemsMatchedRules` asserts an item with a committed `rule_matches` row lists that rule's
  `id` and `name`, and an unmatched item still renders `[]`.
- [x] `NewServer` hands one `ruleTaskCreator` to `NewRuleHandlers`, `NewFeedHandlers` and
  `Server.RuleCreator`, and both production parser injection points — `NewFeedHandlers` in
  `internal/api/server.go` and `NewPoller` in `cmd/dl-tool/main.go` — pass `rss.NewParser`, so a `200` that
  adds items runs the rules pass in production (docs/14-conventions.md §8.3).

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/rss/... ./internal/api/..." && echo GRAB_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/rss` and `ok  github.com/L-K-M/dl-tool/internal/api`,
every test named above reported as `--- PASS`, and the final line of stdout is exactly `GRAB_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT insert into `tasks` from `internal/rss`; the only creation path is `CreateForRule`.
- Do NOT prune `rule_matches` or `rule_seen_episodes`; doc 04 §7 keeps them forever and exposes a per-row
  forget action instead.
- Do NOT back-fill `rule_matches.info_hash` from a fetched `.torrent` here; the metainfo path owns that.
- Do NOT retry a failed grab automatically in v1; the `fallback` rows exist so a later task can.
- Do NOT let a rule bypass the destination containment check or the concurrency limiter.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

Run on the task-branch head, verbatim:

```
$ make lint && make test PKG="./internal/rss/... ./internal/api/..." && echo GRAB_OK
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
go test -race -count=1 ./internal/rss/... ./internal/api/...
ok  	github.com/L-K-M/dl-tool/internal/rss	15.795s
ok  	github.com/L-K-M/dl-tool/internal/api	160.192s
GRAB_OK
```

The acceptance-criteria tests, from the same head under `go test -race -count=1 -v`:

```
--- PASS: TestRunRuleReportsEvaluatedAndGrabbed (0.52s)
--- PASS: TestContentKeyContestCreatesOneTaskAndOneFallback (0.44s)
--- PASS: TestMaxPerRunCaps (0.45s)
--- PASS: TestDuplicateInfoHashAcrossFeeds (0.45s)
--- PASS: TestFailedHandoffDoesNotStageEpisodeKey (0.41s)
--- PASS: TestSecondRunIsIdempotent (0.42s)
--- PASS: TestPoll304RunsNoRule (0.44s)
--- PASS: TestFeedItemsMatchedRules (0.43s)
--- PASS: TestRunRuleCommitsGrabsAsTasks (0.47s)
--- PASS: TestRunRuleUnknownIsNotFound (0.40s)
--- PASS: TestRunRuleCreatedIDsShorterThanMatched (0.48s)
```

Scope check:

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
cmd/dl-tool/main.go
internal/api/feeds.go
internal/api/feeds_test.go
internal/api/rules.go
internal/api/rules_test.go
internal/api/server.go
internal/rss/grab.go
internal/rss/grab_test.go
internal/rss/poll.go
internal/rss/poll_test.go
internal/store/feeds.go
```

plus the regenerated `api/openapi.json` and `web/src/api/schema.d.ts` `make gen` emits for the new
`POST /rules/{id}/run` operation — the same generated-artifact exemption T070's merge recorded.

The `internal/rss` tree contains no `INSERT INTO tasks`: every grab lands through
`TaskCreator.CreateForRule`, implemented in `internal/api` as `ruleTaskCreator` calling
`TaskHandlers.CreateTasks`.

## Blocked

This task cannot run as written: the contract hands a `TaskCreator` to both consumers — `Poller`
for the post-poll `RunAllRules` pass and `RuleHandlers` for `POST /rules/{id}/run` — but no file
in the `## Files` table can deliver one. The contract pins the implementation to `internal/api`
(`ruleTaskCreator{tasks *TaskHandlers}` calling `TaskHandlers.CreateTasks`), and a `*TaskHandlers`
exists only inside `internal/api/server.go`, while the `rss_poll` job's `Poller` is built only in
`cmd/dl-tool/main.go`. Both files are outside the table, so every seam the table permits — a
`Poller.SetTaskCreator` setter, a `NewPoller`/`NewRuleHandlers`/`NewFeedHandlers` parameter, a
`Deps` member — lands with no caller. docs/14-conventions.md §8.3 counts a constructor or
injection point with no call site as not done, and a registered `POST /rules/{id}/run` whose
creator is nil in the only construction path has no documented behaviour: the contract's status
list (200 · 404 · 503 "when the engine refuses every grab") covers a refusal, not a missing wire,
so answering for it means inventing an API edge — the improvise-instead-of-stop case.

Evidence to rerun before ruling, observed on this branch:

- `grep -n "NewTaskHandlers\|NewRuleHandlers\|NewFeedHandlers" internal/api/server.go` —
  `tasks:` and `rules:` are set inside one `Server` literal (`NewTaskHandlers(db, engines,
  cfg.DataRoots, taskGuard, net.DefaultResolver)` then `NewRuleHandlers(db)`), so `rules` can
  never receive `tasks` without restructuring the literal or a post-construction call — both
  server.go edits.
- `grep -rn "NewPoller(" cmd internal` — two call sites: `cmd/dl-tool/main.go` builds the
  `rss_poll` job's poller; `internal/api/feeds.go` builds the refresh endpoint's. The feeds.go
  site is editable but `NewFeedHandlers(db, hc, parser, log)` receives no `*TaskHandlers` and its
  own call site is server.go, so even it cannot construct `ruleTaskCreator`.
- `grep -n "server.go\|main.go" docs/tasks/T*.md` — within M5, T072 and T073 are web-only and
  own neither file; the first `main.go` owners are T074/T076/T077/T079 and the first `server.go`
  owners T080+, all in later milestones. Read ownership from `## Files` tables only — prose
  mentions (this file's `## Blocked` section included) also match the grep. So inside M5 the
  wiring has no carrier, and the M5 exit
  checkpoint itself — docs/00-INDEX.md, "a rule auto-downloads the Arch Linux release feed" —
  cannot pass until it lands. Same defect class as the recorded findings F342 (T083's
  `TaskCreator`), F087, F162 and F263 — unwired constructors the plan review already counts as
  defects, not deferrals.
- Compounding upstream: both `NewPoller` call sites still pass a nil `ItemParser` — T067's
  `## Evidence` records "both are outside the Files table … until a later task wires
  `rss.NewParser`", and no task carries that either — so even a wired creator never sees a `200`
  that added items in production: `Poll` leaves through the parser-not-wired failure before
  `completeFetch` can upsert.

Remedy — the owner picks one:

1. Widen this task's `## Files` table with `internal/api/server.go` and `cmd/dl-tool/main.go`:
   hoist `NewTaskHandlers` into a local before the `Server` literal so `NewRuleHandlers` (for
   `RunRule`) and `NewFeedHandlers` (for the refresh poller's creator) can receive it or a
   `ruleTaskCreator` built from it, and pass `rss.NewParser(time.Now)` at both parser injection
   points — server.go's `NewFeedHandlers` argument and main.go's `rss_poll` job — plus the
   creator, e.g. an exported accessor on `*api.Server`, to the main.go poller. One pass then
   closes both halves of T067's parser deferral and this task's wiring together.
2. Keep the table and name a new carrier task owning server.go and main.go that lands before the
   M5 exit checkpoint; T071 would then ship the seams nil-gated and documented as unwired like
   T067's parser — but the nil-creator behaviour of the registered `POST /rules/{id}/run` still
   needs a documented status, which is itself a contract edit.

Which file should answer: this task's `## Files` table (remedy 1) or `docs/tasks/00-task-index.md`'s
roster plus the new task file (remedy 2). The gap belongs in `PLAN-REVIEW-FINDINGS.md` during the
repair; F342 already records the T083 twin.

**Repair applied 2026-09-19.** The ruling picked remedy 1, the smallest interpretation consistent with
the accepted ADRs and the merged code: this task's `## Files` table now carries
`internal/api/server.go`, `cmd/dl-tool/main.go` and `internal/rss/poll_test.go`, and the contract pins
the seams — `NewPoller`, `NewRuleHandlers` and `NewFeedHandlers` each take the `TaskCreator`,
`Server.RuleCreator` exposes the one `ruleTaskCreator` so `cmd/dl-tool` can hand it to the `rss_poll`
poller, and both parser injection points receive `rss.NewParser(time.Now)`, closing T067's nil-parser
deferral in the same pass. A nil creator disables the post-poll pass so T066's poll tests stay valid.
The injection-point half was already recorded plan finding F251; the nil-parser half is registered as
F670, and both rows are marked resolved by this repair. The index row stays `todo`; the next loop
iteration implements the task.
