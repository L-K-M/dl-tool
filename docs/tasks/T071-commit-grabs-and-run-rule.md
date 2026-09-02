# T071 — Commit rule matches as tasks and run a rule against existing items

| Field | Value |
|---|---|
| **ID** | T071 |
| **Milestone** | M5 |
| **Status** | todo |
| **Depends on** | T020, T024, T066, T069, T070 |
| **Blocks** | T073 |
| **Parallel-safe** | no — extends `internal/rss/poll.go` and `internal/api/rules.go` |
| **Implements** | [FR-077](../02-requirements.md#fr-077-run-a-rule-against-existing-items) |
| **Decisions** | [ADR-0009](../decisions/0009-native-cross-protocol-rss-rules.md), [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| **Est. size** | 2 new files, ~370 LOC |

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
11. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestRunRuleReportsEvaluatedAndGrabbed` asserts `evaluated=20` and three created tasks.
- [ ] `TestContentKeyContestCreatesOneTaskAndOneFallback` passes.
- [ ] `TestMaxPerRunCaps` and `TestFailedHandoffDoesNotStageEpisodeKey` pass.
- [ ] `TestSecondRunIsIdempotent` asserts no second task and no second `rule_matches` row.
- [ ] A `304` poll runs no rule.
- [ ] Every task is created through `CreateForRule`; `internal/rss` contains no `INSERT INTO tasks`.

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
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
