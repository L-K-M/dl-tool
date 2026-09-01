# T068 — Validate rule documents and serve rule CRUD

| Field | Value |
|---|---|
| **ID** | T068 |
| **Milestone** | M5 |
| **Status** | todo |
| **Depends on** | T007, T008, T065 |
| **Blocks** | T069, T070, T073 |
| **Parallel-safe** | no — registers a second operation group on T007's Huma API |
| **Implements** | [FR-074](../02-requirements.md#fr-074-reject-a-malformed-episode-filter-at-save-time) |
| **Decisions** | [ADR-0009](../decisions/0009-native-cross-protocol-rss-rules.md), [ADR-0010](../decisions/0010-never-execute-third-party-definitions.md) |
| **Est. size** | 4 new files, ~430 LOC. The validator and the endpoints ship together because the whole requirement is "rejected at save time". |

## Goal
`RuleDoc` is the Go shape of the rule document, `Validate` rejects every malformed member with the field
path that caused it, and `/rules` stores validated documents. A malformed `episode.filter` such as `1x01`
is `422` at save time, never a rule that silently matches nothing.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/08-rss-automation.md` §4 The rule document](../08-rss-automation.md#4-the-rule-document) — the
   schema, the field reference with every default, and the arrays-not-pipe-splitting rule.
2. [`docs/08-rss-automation.md` §6.1 Syntax](../08-rss-automation.md#61-syntax) and
   [§6.3 Worked examples](../08-rss-automation.md#63-worked-examples) — the filter grammar, the mandatory
   trailing `;`, season normalisation, and the row that is `422` at save time.
3. [`docs/05-api-contract.md` §10.2 Rule CRUD](../05-api-contract.md#102-rule-crud) — the rule object, the
   accepted members and the `errors[]` location for a bad filter.
4. [`docs/04-data-model.md` §3.5 RSS](../04-data-model.md#35-rss) and
   [§4.6 `rules.definition_json` → `match.mode`](../04-data-model.md#46-rulesdefinition_json--matchmode) —
   the `rules` DDL and the three match modes.
5. [`docs/08-rss-automation.md` §5.2 Regex safety](../08-rss-automation.md#52-regex-safety) — RE2, the
   1024-byte pattern cap and the 32-entry `score.formats` cap.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/rss/ruledoc.go` | create | `RuleDoc`, its defaults, `Validate` and `ParseEpisodeFilter`. |
| `internal/store/rules.go` | create | `Rule` and the CRUD queries over `rules`. |
| `internal/api/rules.go` | create | Rule CRUD handlers and the group's `Register`. |
| `internal/api/rules_test.go` | create | Validation, CRUD, conflict and status-code cases. |
| `internal/api/server.go` | edit | Call `NewRuleHandlers(...).Register(api)` once. |

No other file may be modified.

## Interface contract

```go
package rss

// RuleDoc is the rule document of 08-rss-automation.md section 4. It is JSON on the wire and in
// rules.definition_json, and YAML only in the editor. Zero values are filled by ApplyDefaults.
type RuleDoc struct {
	Name     string       `json:"name"`
	Enabled  *bool        `json:"enabled,omitempty"`
	Priority int          `json:"priority,omitempty"`
	Feeds    []string     `json:"feeds,omitempty"`
	Match    MatchSpec    `json:"match"`
	Episode  *EpisodeSpec `json:"episode,omitempty"`
	Score    *ScoreSpec   `json:"score,omitempty"`
	Action   ActionSpec   `json:"action"`
	Throttle ThrottleSpec `json:"throttle,omitempty"`
}

type MatchSpec struct {
	Mode           string   `json:"mode,omitempty"`            // wildcard | regex | plain, default wildcard
	CaseSensitive  bool     `json:"case_sensitive,omitempty"`  // default false
	Fields         []string `json:"fields,omitempty"`          // title | description | category, default [title]
	AnyOf          []string `json:"any_of,omitempty"`
	NoneOf         []string `json:"none_of,omitempty"`
	MinSize        string   `json:"min_size,omitempty"`        // IEC, e.g. "1GiB"
	MaxSize        string   `json:"max_size,omitempty"`
	PublishedAfter string   `json:"published_after,omitempty"` // RFC 3339
}

type EpisodeSpec struct {
	Smart            bool   `json:"smart,omitempty"`
	Filter           string `json:"filter,omitempty"`
	AllowRepackProper *bool `json:"allow_repack_proper,omitempty"` // default true
}

type ScoreSpec struct {
	Minimum int           `json:"minimum,omitempty"`
	Formats []ScoreFormat `json:"formats,omitempty"`
}
type ScoreFormat struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
	Weight  int    `json:"weight"`
}

type ActionSpec struct {
	Destination   string `json:"destination,omitempty"`
	Category      string `json:"category,omitempty"`
	Paused        bool   `json:"paused,omitempty"`
	ContentLayout string `json:"content_layout,omitempty"` // original | subfolder | no_subfolder
	Engine        string `json:"engine,omitempty"`
}

type ThrottleSpec struct {
	CooldownDays int `json:"cooldown_days,omitempty"`
	MaxPerRun    int `json:"max_per_run,omitempty"`
}

// FieldError names one rejected member with a JSON-pointer-style location such as
// "body.definition.episode.filter", which the API copies into errors[].location.
type FieldError struct {
	Location string
	Message  string
}

// ApplyDefaults fills mode=wildcard, fields=[title], enabled=true and allow_repack_proper=true.
func (d *RuleDoc) ApplyDefaults()

// Validate returns every problem at once, never only the first. It compiles each pattern with
// regexp (RE2), rejects an empty string inside any_of or none_of, a pattern longer than 1024
// bytes, more than 32 score.formats entries, an unparseable size or published_after, and a
// content_layout or mode outside its enum.
func (d RuleDoc) Validate() []FieldError

// EpisodeFilter is a parsed episode.filter. Season leading zeros are stripped at parse time,
// which qBittorrent does not do; see 08-rss-automation.md section 6.1.
type EpisodeFilter struct {
	Season int
	Tokens []EpisodeToken
}

// EpisodeToken is a single number, an inclusive range, or an open-ended range.
type EpisodeToken struct {
	From, To int  // To is 0 for a single number, -1 for the open form "01-"
	Open     bool
}

// ParseEpisodeFilter enforces ^(\d{1,4})x(.*;)$ — the trailing ';' is mandatory. An empty filter
// parses to a zero EpisodeFilter that matches everything. An inverted range such as "14-12" is
// skipped, not an error.
func ParseEpisodeFilter(s string) (EpisodeFilter, error)
```

```go
package store

type Rule struct {
	ID             string `db:"id"              json:"id"`
	Name           string `db:"name"            json:"name"`
	Enabled        bool   `db:"enabled"         json:"enabled"`
	Priority       int    `db:"priority"        json:"priority"`
	DefinitionJSON string `db:"definition_json" json:"-"`
	LastMatchAt    *int64 `db:"last_match_at"   json:"-"`
	CreatedAt      int64  `db:"created_at"      json:"-"`
	UpdatedAt      int64  `db:"updated_at"      json:"-"`
}

// ListRules returns every rule ordered by (priority ASC, name ASC) — the evaluation order.
func ListRules(ctx context.Context, db *sqlx.DB, enabledOnly bool) ([]Rule, error)
func RuleByID(ctx context.Context, db *sqlx.DB, id string) (Rule, error)
func CreateRule(ctx context.Context, db *sqlx.DB, r Rule) error
func UpdateRule(ctx context.Context, db *sqlx.DB, r Rule) error
func DeleteRule(ctx context.Context, db *sqlx.DB, id string) error
func SetRuleLastMatchAt(ctx context.Context, db *sqlx.DB, id string, at int64) error
```

Statuses, exactly doc 05 §10.2: `200` · `201` · `204` · `404` · `409 /problems/conflict` on a duplicate
`name` · `422 /problems/validation-failed` carrying one `errors[]` entry per `FieldError`.

## Steps
1. Create `internal/rss/ruledoc.go` with the structs, `ApplyDefaults`, `Validate` and
   `ParseEpisodeFilter`; parse IEC sizes (`1GiB`, `700MiB`) with a small helper in the same file.
2. Implement `ParseEpisodeFilter` exactly as doc 08 §6.1: match `^(\d{1,4})x(.*;)$`, split group 2 on
   `;`, skip empty tokens, strip leading zeros from every token **and from the season**.
3. Make `Validate` return a `FieldError` with location `episode.filter` for `1x01` (no trailing `;`), and
   the API prefix `body.definition.` when it renders them.
4. Compile every `match` entry and every `score.formats[].pattern` during validation and report an
   uncompilable pattern as a `FieldError`, so no rule with a bad regex can ever reach the matcher.
5. Create `internal/store/rules.go` with `Rule` and the six functions above, explicit column lists only.
6. Create `internal/api/rules.go` with `RuleHandlers`, `NewRuleHandlers`, `Register` and `List`, `Create`,
   `Patch`, `Delete`; store `definition_json` as compact JSON produced by `json.Marshal` after
   `ApplyDefaults`, and mirror `name`, `enabled` and `priority` into their own columns.
7. Reject a `definition.name` that disagrees with the request's `name` with `422`, and map a `UNIQUE`
   violation on `rules.name` to `409 /problems/conflict`.
8. Edit `internal/api/server.go` to construct the handlers and call `Register(api)`.
9. Create `internal/api/rules_test.go`: `POST /rules` with `episode.filter: "1x01"` is `422` and
   `errors[0].location` is `body.definition.episode.filter`; `"1x01-;"` is accepted; an empty string in
   `none_of` is `422`; `match.mode: "glob"` is `422`; an uncompilable regex is `422`; a duplicate name is
   `409`; `GET /rules` returns `(priority ASC, name ASC)`; `PATCH` re-validates; `DELETE` is `204`.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestEpisodeFilterMissingSemicolonIsRejected` asserts the `422` and the exact location string.
- [ ] `TestEmptyPatternInNoneOfIsRejected` passes — `none_of: ["web-dl", ""]` never reaches the store.
- [ ] `TestParseEpisodeFilterNormalisesSeason` asserts `01x05;` parses to season `1`.
- [ ] `TestValidateReportsEveryProblem` asserts a document with three faults yields three `errors[]`.
- [ ] `TestRuleListOrderedByPriorityThenName` passes.
- [ ] Stored `definition_json` round-trips through `RuleDoc` unchanged.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/api/... ./internal/rss/... ./internal/store/..." && echo RULEDOC_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/api`, `ok  github.com/L-K-M/dl-tool/internal/rss` and
`ok  github.com/L-K-M/dl-tool/internal/store`, every test named above reported as `--- PASS`, and the final
line of stdout is exactly `RULEDOC_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT match anything. Token semantics, the smart filter and the fourteen steps are T069.
- Do NOT add `POST /rules/test` or `POST /rules/{id}/run`; T070 and T071 own them.
- Do NOT accept a YAML string in `definition`; the wire form is a JSON object (doc 05 §10.2).
- Do NOT split `any_of` or `none_of` on `|`. They are real arrays; doc 08 §4.3 explains the trap that
  splitting reproduces.
- Do NOT import a qBittorrent `rules.json`, and do NOT add an import endpoint or converter — the importer
  is cut from the product.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
