# T069 — Evaluate rules with the fourteen-step algorithm and the reason-code enum

| Field | Value |
|---|---|
| **ID** | T069 |
| **Milestone** | M5 |
| **Status** | todo |
| **Depends on** | T015, T016, T067, T068 |
| **Blocks** | T070, T071, T073 |
| **Parallel-safe** | yes — creates `internal/rss/match.go` and `internal/rss/episode.go` only |
| **Implements** | [FR-073](../02-requirements.md#fr-073-evaluate-rules-with-the-documented-algorithm), [FR-078](../02-requirements.md#fr-078-route-a-rules-action-to-any-engine) |
| **Decisions** | [ADR-0009](../decisions/0009-native-cross-protocol-rss-rules.md), [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| **Est. size** | 3 new files, ~430 LOC |

## Goal
`Evaluate` runs steps 1 to 11 of the algorithm over one rule and a candidate item set and returns a decision
per item, each carrying one of the ten reason codes and a `reason_detail` naming the clause and its index.
`Resolve` performs step 13's per-`content_key` contest. Nothing is written and no task is created.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/08-rss-automation.md` §5 The matching algorithm](../08-rss-automation.md#5-the-matching-algorithm)
   — the fourteen steps in order and
   [§5.1 Rejection reason codes](../08-rss-automation.md#51-rejection-reason-codes) — the closed enum of ten
   codes and their `reason_detail` examples.
2. [`docs/08-rss-automation.md` §6 Episode filters](../08-rss-automation.md#6-episode-filters) — the token
   table, the two partial patterns, and §6.4's four smart-key formats and decision table.
3. [`docs/08-rss-automation.md` §7 Dedup](../08-rss-automation.md#7-dedup) — the three-key ladder and the
   v1/v2 info-hash comparison rules.
4. [`docs/tasks/T068-rule-document-and-validator.md`](T068-rule-document-and-validator.md) — `RuleDoc`,
   `EpisodeFilter` and `ParseEpisodeFilter`, which this task consumes unchanged.
5. [`docs/06-download-engines.md` §2 Routing table](../06-download-engines.md#2-routing-table) — the
   `engine.Route` rows a grabbed item follows, identical to a pasted URI's.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/rss/match.go` | create | Steps 1–11 and 13, the reason codes, scoring and routing. |
| `internal/rss/episode.go` | create | Token semantics, the two partial patterns and smart keys. |
| `internal/rss/match_test.go` | create | One case per step, per reason code and per worked example. |

No other file may be modified.

## Interface contract

```go
package rss

// Reason codes. The set is closed: 08-rss-automation.md section 5.1 owns it and the API repeats it.
const (
	ReasonCooldown           = "cooldown"
	ReasonExcluded           = "excluded"
	ReasonNoMatch            = "no_match"
	ReasonSize               = "size"
	ReasonEpisodeFilter      = "episode_filter"
	ReasonUnparseableEpisode = "unparseable_episode"
	ReasonDuplicateEpisode   = "duplicate_episode"
	ReasonDuplicateInfoHash  = "duplicate_infohash"
	ReasonAlreadyHave        = "already_have"
	ReasonBelowMinimumScore  = "below_minimum_score"
)

// Decision is one evaluated item. Matched decisions carry MatchedBy; rejected ones carry Reason
// and ReasonDetail. Steps 1 and 3 remove an item before evaluation and produce no Decision at all.
type Decision struct {
	ItemID       string
	Matched      bool
	Score        int
	MatchedBy    map[string]string // e.g. {"any_of": "ubuntu *desktop* amd64"}
	Reason       string
	ReasonDetail string // "none_of[0] = \"daily\"", "any_of[1] token \"amd64\" not found"
	Highlight    [2]int // byte offsets of the matched substring in the title, {0,0} when none
}

// Candidate is an accepted item, ready for step 13 and then for the grab in T071.
type Candidate struct {
	Item       store.FeedItem
	FeedURL    string
	Score      int
	ContentKey string // episode key when the rule has one, else the info hash, else identity
	EpisodeKey string // "" when episode.smart is off or no key was parsed
	Engine     string // engine.Route result: aria2 | qbittorrent | ytdlp
	Norm       uri.Normalized
	MatchedBy  map[string]string
}

// State answers the dedup ladder's questions. T070 passes a no-op implementation when
// ignore_state is true; T071 passes the database-backed one.
type State interface {
	// HasInfoHash reports whether a 40- or 64-hex hash was already grabbed. It never
	// truncates a v2 hash to compare it with a v1 column.
	HasInfoHash(ctx context.Context, hash string) (bool, error)
	// SeenEpisode reports whether this rule already stored that episode key.
	SeenEpisode(ctx context.Context, ruleID, key string) (bool, error)
	// BestScoreForContentKey returns the score of the stored grab for a content key,
	// and ok=false when there is none.
	BestScoreForContentKey(ctx context.Context, key string) (score int, ok bool, err error)
}

// Evaluate runs steps 1 to 11 for one rule. Items are the candidate set, already newest-first.
// It is side-effect free and safe to call concurrently.
func Evaluate(ctx context.Context, doc RuleDoc, rule store.Rule, items []store.FeedItem,
	feedURLByID map[string]string, st State, now int64) ([]Decision, []Candidate, error)

// Resolve is step 13: group by ContentKey, sort each group by
// (score DESC, rule priority ASC, published_at DESC) and return the winner of each group first.
// Losers keep their order and become rule_matches rows with status 'fallback' in T071.
func Resolve(cands []Candidate, rulePriority int) (winners, losers []Candidate)
```

```go
package rss

// Match applies the token semantics of 08-rss-automation.md section 6.2 to one title.
// A zero EpisodeFilter (empty filter string) matches everything.
func (f EpisodeFilter) Match(title string) bool

// The two title parsers of section 6.2, used by every range token.
//   partialPattern1 = \bs0?(\d{1,4})[ -_\.]?e(0?\d{1,4})(?:\D|\b)
//   partialPattern2 = \b(\d{1,4})x(0?\d{1,4})(?:\D|\b)
func ParseSeasonEpisode(title string) (season, episode int, ok bool)

// SmartKey builds the dedup key from the four formats of section 6.4, joining the non-empty
// captures with the literal character 'x' after an integer parse that strips leading zeros:
// "Show.S01E05.1080p" -> "1x5", "Show 1x05 1080p" -> "1x5", "Show.2017.01.01" -> "2017.01.01".
func SmartKey(title string) (string, bool)

// RepackVariants returns the key variants a REPACK or PROPER title stages. A title that is both
// stages key+"-REPACK" and key+"-PROPER" so neither can be grabbed later.
func RepackVariants(key, title string) []string
```

## Steps
1. Create `internal/rss/episode.go` with `Match`, `ParseSeasonEpisode`, `SmartKey` and `RepackVariants`,
   compiling the two partial patterns and the four smart formats once as package-level `regexp` values.
2. Implement the three token forms of doc 08 §6.2: a single number compiles
   `\b(?:s0?{S}[ -_\.]?e0?{E}|{S}x0?{E})(?:\D|\b)`; a range parses the title and compares within the
   season; the open form also matches every later season. Skip an inverted range.
3. Create `internal/rss/match.go` with the reason constants, `Decision`, `Candidate`, `State`, `Evaluate`
   and `Resolve`.
4. Implement steps 1 to 3 as pre-filters that return no `Decision`, then build the haystack from
   `match.fields` joined with `\n`, lowercasing both sides unless `case_sensitive`.
5. Evaluate `none_of` **before** `any_of` and report `excluded` with the entry index; then `any_of`,
   splitting an entry on `\s+` into AND-ed tokens in `wildcard` and `plain` mode only, and reporting
   `no_match` with the offending token.
6. Implement step 7 so an item with unknown `size_bytes` **passes**, and step 8 so a parsed filter that
   matches nothing yields `episode_filter`.
7. Implement step 9 with `SmartKey`: no key is `unparseable_episode`; a seen key follows §6.4's decision
   table exactly, including the REPACK/PROPER variants.
8. Implement step 10 as the ladder of doc 08 §7 through the `State` interface — info hash first, then the
   content key, yielding `duplicate_infohash` or `already_have`; a 40-hex value is only ever compared with
   a 40-hex one and a 64-hex value with a 64-hex one.
9. Implement step 11 by summing every matching `score.formats` weight and rejecting
   `below_minimum_score` below `score.minimum`; record the winning clause and the title byte offsets in
   `Highlight`.
10. Set `Candidate.Engine` from `uri.Normalize(item.download_url)` followed by `engine.Route(n, nil)`, so a
    rule lands HTTP items on aria2 and magnets on qBittorrent exactly as a pasted URI would.
11. Create `internal/rss/match_test.go` with a table producing every one of the ten reason codes; the nine
    worked examples of doc 08 §6.3; the three smart keys of §6.4; a mixed feed whose two accepted
    candidates carry `aria2` and `qbittorrent`; and a `Resolve` case where the higher-scoring release wins
    and the loser is returned second.
12. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestEveryReasonCodeIsProduced` asserts all ten codes appear, and no eleventh string.
- [ ] `TestNoneOfEvaluatedBeforeAnyOf` asserts an item hitting both reports `excluded`.
- [ ] `TestUnknownSizePasses` asserts an item with `size_bytes` NULL is not rejected.
- [ ] `TestEpisodeFilterWorkedExamples` covers all nine rows of doc 08 §6.3.
- [ ] `TestSmartKeyCollapsesNotations` asserts `S01E05` and `1x05` both produce `1x5`.
- [ ] `TestRuleRoutesHTTPAndMagnet` asserts one candidate is `aria2` and the other `qbittorrent`.
- [ ] `Evaluate` writes nothing: the test database row counts are unchanged afterwards.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/rss/... && echo MATCH_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/rss`, every test named above reported as `--- PASS`, and
the final line of stdout is exactly `MATCH_OK`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT write `rule_matches`, `rule_seen_episodes` or `rules.last_match_at`, and do NOT create a task.
  Steps 12 and 14 are T071.
- Do NOT expose an endpoint; T070 owns `POST /rules/test`.
- Do NOT re-validate the document. T068 guarantees every pattern compiles and every filter parses.
- Do NOT add a `guessit`-style parser; the four regexes of doc 08 §6.4 are the whole vocabulary, and a
  title matching none of them is `unparseable_episode` by design.
- Do NOT truncate a 64-hex v2 hash to 40 characters to make a comparison succeed.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
