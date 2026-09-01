# T070 — Dry-run a rule and report a reason code for every evaluated item

| Field | Value |
|---|---|
| **ID** | T070 |
| **Milestone** | M5 |
| **Status** | todo |
| **Depends on** | T068, T069 |
| **Blocks** | T073 |
| **Parallel-safe** | no — extends T068's `internal/api/rules.go` |
| **Implements** | [FR-075](../02-requirements.md#fr-075-dry-run-a-rule-and-explain-every-item), [FR-076](../02-requirements.md#fr-076-dry-run-reproducibly-by-ignoring-stored-state) |
| **Decisions** | [ADR-0009](../decisions/0009-native-cross-protocol-rss-rules.md) |
| **Est. size** | 2 new files, ~330 LOC |

## Goal
`POST /rules/test` evaluates an unsaved rule document against the stored items of the named feeds and
returns **every** item — matched and unmatched — with its score, the clause that matched, or a reason code
and the clause index responsible. It creates nothing, stores nothing, and repeats byte for byte.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §10.3 `POST /rules/test` — the dry run](../05-api-contract.md#103-post-rulestest--the-dry-run)
   — the body table, the worked request, the worked response and the six contract details.
2. [`docs/08-rss-automation.md` §8 Dry run](../08-rss-automation.md#8-dry-run) — the behavioural contract
   and the six UI requirements the payload has to support.
3. [`docs/tasks/T069-rule-matching-algorithm.md`](T069-rule-matching-algorithm.md) — `Evaluate`,
   `Decision`, `State` and the reason constants, consumed unchanged.
4. [`docs/tasks/T068-rule-document-and-validator.md`](T068-rule-document-and-validator.md) — `RuleDoc`,
   `ApplyDefaults` and `Validate`, whose `422` shape this endpoint reuses.
5. [`docs/05-api-contract.md` §1.3 Errors](../05-api-contract.md#13-errors--rfc-9457-applicationproblemjson)
   — the problem shape and the `errors[]` member.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/rss/dryrun.go` | create | Item selection, the stateless `State`, timing and the report. |
| `internal/rss/dryrun_test.go` | create | Reproducibility, closed-enum and limit cases. |
| `internal/api/rules.go` | edit | Add the `POST /rules/test` operation. |
| `internal/api/rules_test.go` | edit | Endpoint cases: `200`, `404`, `422`, and every item returned. |

No other file may be modified.

## Interface contract

```go
package rss

// DryRunRequest mirrors the body of POST /rules/test. FeedIDs empty means the rule's own feeds,
// and an empty rule.feeds means every enabled feed. Limit is items per feed, newest first.
type DryRunRequest struct {
	Rule        RuleDoc
	FeedIDs     []string
	Limit       int  // default 200, range 1..500
	IgnoreState bool // default true
}

// DryRunItem is one row of results[]. MatchedBy is present only when Matched is true;
// Reason and ReasonDetail only when it is false.
type DryRunItem struct {
	FeedID      string            `json:"feed_id"`
	Feed        string            `json:"feed"`
	Title       string            `json:"title"`
	PublishedAt *string           `json:"published_at"`
	DownloadURL string            `json:"download_url"`
	Matched     bool              `json:"matched"`
	Score       int               `json:"score,omitempty"`
	MatchedBy   map[string]string `json:"matched_by,omitempty"`
	WouldDo     *WouldDo          `json:"would_do,omitempty"`
	Reason       string           `json:"reason,omitempty"`
	ReasonDetail string           `json:"reason_detail,omitempty"`
}

// WouldDo restates the rule's action for a matched item; it is the preview, never a promise.
type WouldDo struct {
	Destination string `json:"destination"`
	Category    string `json:"category"`
	Paused      bool   `json:"paused"`
}

type DryRunReport struct {
	Evaluated int          `json:"evaluated"`
	Matched   int          `json:"matched"`
	ElapsedMS int64        `json:"elapsed_ms"`
	Results   []DryRunItem `json:"results"`
}

// DryRun selects the items, runs Evaluate and builds the report. It opens no transaction and
// writes nothing. ErrNotFound is returned when a named feed id does not exist.
func DryRun(ctx context.Context, db *sqlx.DB, req DryRunRequest) (DryRunReport, error)

// StatelessState is the State implementation used when IgnoreState is true: every lookup
// answers "not seen", so repeated calls over an unchanged item set are identical.
type StatelessState struct{}
```

```go
package api

type TestRuleInput struct {
	Body struct {
		Rule        json.RawMessage `json:"rule"          required:"true"`
		Feeds       []string        `json:"feeds,omitempty"`
		Limit       int             `json:"limit,omitempty"        minimum:"1" maximum:"500" default:"200"`
		IgnoreState *bool           `json:"ignore_state,omitempty"`
	}
}

type TestRuleOutput struct{ Body rss.DryRunReport }

func (h *RuleHandlers) TestRule(ctx context.Context, in *TestRuleInput) (*TestRuleOutput, error)
```

`ignore_state` defaults to **true** when the member is absent. Statuses, exactly doc 05 §10.3: `200`
whatever the per-item outcomes · `404` when a named feed id does not exist · `422` when the document fails
`Validate`, with `errors[].location` pointing at the offending member.

## Steps
1. Create `internal/rss/dryrun.go` with `DryRunRequest`, `DryRunItem`, `WouldDo`, `DryRunReport`,
   `StatelessState` and `DryRun`.
2. Resolve the feed set: `req.FeedIDs`, else `rule.feeds` matched by URL, else every enabled feed; return
   `store.ErrNotFound` for an unknown id.
3. Select at most `Limit` items per feed with `store.ListFeedItems`, newest first, and build
   `feedURLByID` and a feed-title map in the same pass.
4. Call `Evaluate` with `StatelessState` when `IgnoreState`, otherwise with the database-backed state;
   measure `ElapsedMS` around the call with `time.Since`.
5. Emit one `DryRunItem` per evaluated item — matched and unmatched — and set `Evaluated` to
   `len(Results)`; steps 1 and 3 of the algorithm remove items before evaluation and those items are
   absent from both counts.
6. Fill `WouldDo` from the rule's `action` for matched items only, and leave `Reason` empty there.
7. Edit `internal/api/rules.go` to register `POST /rules/test`, unmarshalling `rule` into `RuleDoc`,
   calling `ApplyDefaults` then `Validate`, and returning `422` with one `errors[]` entry per
   `FieldError` before any database access.
8. Guarantee the handler is side-effect free: it takes no transaction, and a panic in evaluation is
   impossible because every pattern was compiled during validation.
9. Create `internal/rss/dryrun_test.go`: 200 stored items yield `evaluated == len(results) == 200`; every
   unmatched entry carries a code from the closed set of ten; two identical calls with
   `ignore_state: true` around a real `rule_matches` insert are equal under `go-cmp`; `limit: 5` returns
   five items per feed; `matched_by` and `reason` are never both present.
10. Edit `internal/api/rules_test.go`: a malformed `episode.filter` is `422`; an unknown feed id is `404`;
    a valid document is `200` with `evaluated`, `matched` and `elapsed_ms` present.
11. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestDryRunReturnsEveryEvaluatedItem` asserts `evaluated == matched + rejected == len(results)`.
- [ ] `TestDryRunReasonEnumIsClosed` fails on any code outside the ten of doc 08 §5.1.
- [ ] `TestDryRunIsReproducibleWithIgnoreState` compares two reports with `go-cmp` and finds no diff.
- [ ] `TestDryRunWritesNothing` asserts `rule_matches` and `rule_seen_episodes` row counts are unchanged.
- [ ] `reason_detail` names the clause and its index, e.g. `none_of[0] = "daily"`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/rss/... ./internal/api/..." && echo DRYRUN_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/rss` and `ok  github.com/L-K-M/dl-tool/internal/api`,
every test named above reported as `--- PASS`, and the final line of stdout is exactly `DRYRUN_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT create a task, write `rule_matches`, or set `rules.last_match_at`; T071 owns every write.
- Do NOT fetch a feed to freshen it first; the dry run reads stored items only.
- Do NOT return only the matches. Returning `matchingArticles` alone is the defect this endpoint exists
  to fix.
- Do NOT cache the report; the editor calls this endpoint on a 250 ms debounce and expects live results.
- Do NOT add a rate limit or a per-user quota to this endpoint in v1.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
