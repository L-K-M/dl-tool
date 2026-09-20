# T073 — Build the rule editor with the live dry-run preview

| Field | Value |
|---|---|
| **ID** | T073 |
| **Milestone** | M5 |
| **Status** | done |
| **Depends on** | T047, T050, T068, T070, T071, T072 |
| **Blocks** | — |
| **Parallel-safe** | no — adds a route to T040's `web/src/App.tsx`, extends T072's `rss.json` and edits `internal/rss` and `internal/api` files of T068–T071 |
| **Implements** | — (renders [FR-073](../02-requirements.md#fr-073-evaluate-rules-with-the-documented-algorithm), [FR-075](../02-requirements.md#fr-075-dry-run-a-rule-and-explain-every-item) and [FR-077](../02-requirements.md#fr-077-run-a-rule-against-existing-items), covered by T069, T070 and T071) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md), [ADR-0009](../decisions/0009-native-cross-protocol-rss-rules.md) |
| **Est. size** | 2 new files, ~420 LOC, plus the `highlight`/`titles`/`tags` members the F115/F116/F379 repair assigned here |

## Goal
`/rss/rules` renders the three-column rule editor. Every keystroke re-posts the in-progress document to
`POST /rules/test` on a 250 ms debounce and the right column lists **matches and non-matches**, each
non-match with the clause that rejected it — the improvement over both Download Station and qBittorrent.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/09-web-ui-spec.md` §8.2 Rule editor](../09-web-ui-spec.md#82-rule-editor) — the wireframe, the
   thirteen field labels in order, the live-preview rules, the verbatim `(?)` help text and the
   *Run rule against existing items* action.
2. [`docs/05-api-contract.md` §10.3 `POST /rules/test` — the dry run](../05-api-contract.md#103-post-rulestest--the-dry-run)
   — the request body, the `results[]` shape and the `422` `errors[].location` used for inline errors.
3. [`docs/05-api-contract.md` §10.2 Rule CRUD](../05-api-contract.md#102-rule-crud) — save, patch, delete
   and `POST /rules/{id}/run`.
4. [`docs/08-rss-automation.md` §5.1 Rejection reason codes](../08-rss-automation.md#51-rejection-reason-codes)
   — the ten codes each need one sentence in the `rss` namespace.
5. [`docs/08-rss-automation.md` §4 The rule document](../08-rss-automation.md#4-the-rule-document) — the
   schema `internal/rss/ruledoc.go` encodes, including the `action.tags` member this task adds.
6. [`docs/09-web-ui-spec.md` §4.1 Server-side folder browser](../09-web-ui-spec.md#41-server-side-folder-browser)
   — the `Destination` field reuses T047's browser unchanged.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/Rss/RuleEditor.tsx` | create | The rules list, the form and the preview column. |
| `web/src/components/Rss/RuleEditor.test.tsx` | create | Debounce, preview, inline regex error and save cases. |
| `web/src/locales/en/rss.json` | edit | Field labels, the ten reason sentences and the help popover. |
| `web/src/App.tsx` | edit | Route `/rss/rules` to the editor. |
| `internal/rss/dryrun.go` | edit | Carry `highlight` on `DryRunItem`, widen `FeedID`/`Feed`/`DownloadURL` to `*string`, add `Tags` to `WouldDo`, and evaluate `titles` in place of stored items. |
| `internal/rss/dryrun_test.go` | edit | Pin the offsets, the UTF-8 basis and the `titles` path. |
| `internal/rss/ruledoc.go` | edit | Add `Tags` to `ActionSpec` (doc 08 §4.2's `action.tags`). |
| `internal/rss/grab.go` | edit | Add `Tags` to `GrabRequest` and populate it in `Commit`. |
| `internal/rss/grab_test.go` | edit | Pin `GrabRequest.Tags` reaching the creator. |
| `internal/api/rules.go` | edit | Accept `titles` on `POST /rules/test`; pass `g.Tags` in `ruleTaskCreator.CreateForRule`. |
| `internal/api/rules_test.go` | edit | Pin the `titles` dry run and `tags` in the created `POST /tasks` body. |

No other file may be modified. `api/openapi.json` and `web/src/api/schema.d.ts` regenerate with
`make gen` per doc 13 §7.1 — `titles`, `highlight` and `tags` all change the schema.

## Interface contract

```tsx
// web/src/components/Rss/RuleEditor.tsx
import type { components } from '../../api/schema';

// The rule document is the generated schema type; the editor never defines its own copy.
export type RuleDoc = components['schemas']['RuleDoc'];
export type DryRunItem = components['schemas']['DryRunItem'];

export interface EditorState {
  doc: RuleDoc;
  dirty: boolean;
  preview: { evaluated: number; matched: number; elapsed_ms: number; results: DryRunItem[] } | null;
  fieldErrors: Record<string, string>;   // location -> message, from the 422 errors[]
}

export function RuleEditor(): JSX.Element;

// Debounced dry run. It keeps the last successful preview when a request fails validation,
// so the panel freezes on the last valid result instead of clearing.
export function useRulePreview(doc: RuleDoc, feedIds: string[]): {
  preview: EditorState['preview'];
  fieldErrors: EditorState['fieldErrors'];
  pending: boolean;
};

export const PREVIEW_DEBOUNCE_MS = 250;
export const PREVIEW_LIMIT = 50;   // "matches N of the last 50 items"
```

```go
// internal/rss/dryrun.go — the members F115 and F116 add to the dry run.
type DryRunRequest struct {
	// ...existing members...
	Titles []string // evaluated in place of stored items when non-empty
}

type DryRunItem struct {
	FeedID      *string `json:"feed_id"`      // nil on titles rows; no omitempty, nil marshals null
	Feed        *string `json:"feed"`         // same
	DownloadURL *string `json:"download_url"` // same
	// ...existing members... (PublishedAt is already *string)
	Highlight *[2]int `json:"highlight,omitempty"` // [start,end) UTF-8 byte offsets into Title; nil unless Matched and the span is non-empty
}

type WouldDo struct {
	// ...existing members...
	Tags []string `json:"tags,omitempty"` // echoes action.tags so the preview shows them
}

// internal/rss/ruledoc.go — F379's member, doc 08 §4.2's action.tags.
type ActionSpec struct {
	// ...existing members...
	Tags []string `json:"tags,omitempty"`
}

// internal/rss/grab.go — the tag list rides the grab so the created task carries it.
type GrabRequest struct {
	// ...existing members...
	Tags []string
}

// internal/api/rules.go — TestRuleInput.Body gains the member doc 05 §10.3 lists.
	Titles []string `json:"titles,omitempty" doc:"Arbitrary titles evaluated statelessly in place of stored items; feeds, limit and ignore_state do not apply"`
```

Wire rules the editor depends on:

- `errors[].location` from the dry run is prefixed `body.rule.`; from `POST`/`PATCH /rules` it is
  `body.definition.` (F255's recorded conflict, pinned in doc 05 §10.3). Strip either prefix before
  mapping the remainder — `episode.filter` → **Episode filter**, `match.any_of` → **Must contain**,
  `match.none_of` → **Must not contain** — onto the same control.
- `highlight` arrives as `[start,end)` — half-open, `end` exclusive — UTF-8 byte offsets into `title`.
  Convert to UTF-16 code-unit indices before slicing: walk the title accumulating
  `encoder.encode(title.slice(0, i)).length` (or equivalent), never `title.slice(start, end)` on the
  raw values.
- A `titles` request is bounded — at most 50 titles of 500 UTF-8 bytes each, an empty array is a `422` —
  and ignores `feeds`, `limit` and `ignore_state` (synthesized items evaluate statelessly); the one
  result per title carries `feed_id`, `feed`, `download_url` and `published_at` as JSON `null`, which
  `DryRunItem`'s `FeedID`, `Feed` and `DownloadURL` becoming `*string` expresses; `PublishedAt` is
  already `*string`, stays nil for synthesized items, and none of the four members carries `omitempty`,
  so a nil marshals as `null`, never an absent key. A synthesized item has no `size_bytes`, so
  `match.min_size`/`max_size` pass per doc 08 §5 step 7 — the panel's verdict tests the title clauses.
- `limit` is per feed (doc 05 §10.3), so a rule scoped to several feeds can return more than
  `PREVIEW_LIMIT` rows. The editor renders only the newest `PREVIEW_LIMIT` — the merged results arrive
  newest-first — and the headline's `N` counts matches among the displayed rows, so
  `matches N of the last 50 items` is literal (F316). `evaluated`/`matched`/`elapsed_ms` beneath the
  list still show the server counters.

Form controls, in this order and with these labels, verbatim from doc 09 §8.2: **Enabled** ·
**Use regular expressions** · **Must contain** · **Must not contain** · **Episode filter** ·
**Use smart episode filter** · **Apply to feeds** · **Destination** · **Category** · **Tags** ·
**Add stopped** · **Ignore subsequent matches for … days** · **Last match**.

Field-to-document mapping — the editor writes the document of doc 08 §4, not qBittorrent's keys:

| Control | Document member |
|---|---|
| Use regular expressions | `match.mode` = `regex` when ticked, else `wildcard` |
| Must contain | `match.any_of[]`, one entry per line, empty lines dropped |
| Must not contain | `match.none_of[]`, same rule |
| Episode filter | `episode.filter` |
| Use smart episode filter | `episode.smart` |
| Apply to feeds | `feeds[]` as feed URLs |
| Destination / Category | `action.destination` / `action.category` |
| Tags | `action.tags[]` — created on demand, like `POST /tasks`' `tags` |
| Add stopped | `action.paused` — a checkbox; no global add-stopped setting exists (doc 05 §11.1), so the wireframe's `Use global ▾` is an artifact (F315) |
| Ignore subsequent matches for … days | `throttle.cooldown_days` |

The headline reads exactly `matches N of the last 50 items`.

## Steps
1. Land the wire members the editor builds on. In `internal/rss/ruledoc.go` add `Tags` to `ActionSpec`;
   in `internal/rss/grab.go` add `Tags` to `GrabRequest`, populate it from `doc.Action.Tags` in `Commit`,
   and pass `g.Tags` through `ruleTaskCreator.CreateForRule` into the `POST /tasks` body in
   `internal/api/rules.go`. In `internal/rss/dryrun.go` add `Highlight` to `DryRunItem`, filled from
   `Decision.Highlight` only when `Matched` and the span is non-empty; widen `FeedID`, `Feed` and
   `DownloadURL` to `*string` with no `omitempty` so a nil marshals as `null` on titles rows; and add
   `Tags` to `WouldDo`, echoed from `req.Rule.Action.Tags`. Then add `Titles` to
   `DryRunRequest`: when non-empty, `DryRun` synthesizes one `store.FeedItem` per title — `Title` set,
   `ID` and `Identity` carrying a distinct synthetic value so the decision join and dedup keys stay
   correct, every other field at its zero value (`PublishedAt` nil maps to `null` through the existing
   `unixMilliPtrRFC3339`) — evaluates them with `StatelessState`, and skips the
   stored-item selection entirely (`feeds`, `limit` and `ignore_state` do not apply; more than 50
   titles, a title over 500 bytes or an empty array is a `422`). Add `Titles` to `TestRuleInput.Body`
   and forward it in `internal/api/rules.go`.
   Then `make gen` so `api/openapi.json` and `web/src/api/schema.d.ts` carry the new members.
2. Create `web/src/components/Rss/RuleEditor.tsx` with the three columns of doc 09 §8.2: the rules list
   with `[+] [⧉] [🗑]` and the *Import / export rules* buttons, the form, and the preview.
3. Implement `useRulePreview` with a 250 ms debounce and an `AbortController` so an in-flight dry run is
   cancelled when the user types again; never queue two requests.
4. Post `{rule, feeds, limit: 50, ignore_state}` to `POST /rules/test` and render the newest
   `PREVIEW_LIMIT` rows of `results[]` in order: matches with `✓`, non-matches greyed with `✗` and the
   reason sentence from the `rss` namespace, keyed by `reason` and interpolating `reason_detail`.
5. Highlight the matched substring in a matched title using the `highlight` offsets the response
   carries — UTF-8 byte offsets converted to UTF-16 code-unit indices before slicing; never re-run the
   pattern in the browser.
6. Render a `422` inline under the responsible control by mapping `errors[].location` — for example
   `body.rule.episode.filter` from the dry run or `body.definition.episode.filter` from save — to the
   field, and keep the previous preview visible.
7. Bind the *Ignore already-downloaded* toggle to `ignore_state`, defaulting to on, and show `evaluated`,
   `matched` and `elapsed_ms` beneath the list.
8. Implement the docked test panel: a title input and `Test`, which posts a one-item dry run —
   `{rule, titles: [title]}` — and renders `✓ MATCH` or the reason, naming the clause
   that decided it.
9. Add the `(?)` popover beside the mode toggle carrying the two qBittorrent help sentences of doc 09 §8.2
   verbatim, stored as two keys in `rss.json`.
10. Wire `Save` to `POST /rules` or `PATCH /rules/{id}`, and *Run rule against existing items* to
    `POST /rules/{id}/run`, confirming first and reporting `evaluated`, `matched` and the created count.
11. Wire the toolbar's *Import / export rules* (F317's owner): export downloads a JSON array of the
    `{"name","definition"}` pairs of `GET /rules`; import reads such a file — a single object or an
    array — and posts each entry to `POST /rules`, skipping a name that already exists (`POST /rules`
    answers a duplicate with `409`) and reporting skipped names and per-name failures. No new endpoint.
12. Use T047's folder browser for `Destination` and T050's category list for `Category`; add no new picker.
13. Edit `web/src/locales/en/rss.json` with the labels, the ten reason sentences and the help text, then
    edit `web/src/App.tsx` to route `/rss/rules`.
14. Create `web/src/components/Rss/RuleEditor.test.tsx` with `msw`: typing fires exactly one dry run after
    250 ms; the headline reads `matches 7 of the last 50 items`; a non-match renders its `✗` row and the
    reason sentence; a `422` on an unterminated group renders inline under **Must contain** and the
    previous preview survives; toggling *Ignore already-downloaded* re-posts with `ignore_state: false`;
    `Save` sends the mapped document; `Run` posts to `/rules/{id}/run`. Add a case per new member: a
    matched row with a non-ASCII title and a `highlight` pair marks the right substring; the docked
    panel posts `titles`; `Save` sends `action.tags`.
15. Extend `internal/rss/dryrun_test.go`, `internal/rss/grab_test.go` and `internal/api/rules_test.go` to
    pin the members of step 1: `DryRunItem.Highlight` carries `Decision.Highlight`, a `titles` request
    never touches `feed_items`, a titles row marshals all four absent members as `null`, and a grab
    lands `action.tags` in the created task's `tags`.
16. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [x] `TestPreviewDebouncesToOneRequest` asserts one call for five keystrokes inside 250 ms.
- [x] `TestPreviewListsNonMatchesWithReason` asserts the `✗` row and its sentence.
- [x] `TestInvalidRegexShowsInlineErrorAndKeepsPreview` passes.
- [x] `TestSaveSendsMappedDocument` asserts `match.any_of` is an array of lines, never a `|`-joined string.
- [x] `TestHighlightConvertsUtf8Offsets` marks the matched substring of a non-ASCII title correctly —
      slicing the raw byte offsets would select the wrong span.
- [x] `TestTitlePanelPostsTitles` asserts the docked panel's request body carries `titles` and no `feeds`.
- [x] `TestSaveSendsMappedDocument` also asserts `action.tags` is the entered list.
- [x] The thirteen labels appear character for character as doc 09 §8.2 gives them.
- [x] The headline string is `matches N of the last 50 items`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo RULE_EDITOR_OK
```
Expected: Vitest reports every test file passing, including
`src/components/Rss/RuleEditor.test.tsx` with the tests named above, and the final line of stdout is
exactly `RULE_EDITOR_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table plus `api/openapi.json` and `web/src/api/schema.d.ts`
from `make gen`, and nothing else — the command sorts, so the output order will not match the table's
row order. Use `git status`, not `git diff`: a file this task creates is untracked, and
`git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement the matching algorithm in TypeScript. Every verdict, score and highlight offset comes
  from `POST /rules/test`.
- Do NOT split **Must contain** on `|`; the document uses arrays, and doc 08 §4.3 explains the trap.
- Do NOT add a qBittorrent `rules.json` import button; the importer is cut from the product. The rules-list
  toolbar's import and export move JSON documents dl-tool itself produced.
- Do NOT add a YAML editor in v1; the document travels as JSON (doc 05 §10.2).
- Do NOT create a category or a feed from this screen beyond selecting an existing one.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

Run on the task-branch head. The vitest output interleaves `msw` unhandled-request stacks and
`flushSync` lifecycle warnings from other screens (pre-existing noise — the same lines appear on
a clean tree); those are elided here, every test-file and summary line is verbatim.

```
$ make lint && make typecheck && make test-web && echo RULE_EDITOR_OK
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
cd web && npx tsc --noEmit -p tsconfig.json
cd web && npx vitest run

 RUN  v4.1.11 /home/paseo/.paseo/worktrees/0a6udotz/loop-t073-1-1789896356/web

 ✓ src/main.test.ts (1 test) 1585ms
 ✓ src/store/useUiPrefs.test.ts (17 tests) 1028ms
 ✓ src/components/FolderBrowser/FolderBrowserDialog.test.tsx (9 tests) 1255ms
 ✓ src/components/DetailPane/DetailPane.test.tsx (13 tests) 1327ms
 ✓ src/components/Settings/IndexersSection.test.tsx (6 tests) 1564ms
 ✓ src/lib/format.test.ts (6 tests) 71ms
 ✓ src/i18n.test.ts (4 tests) 45ms
 ✓ src/store/useTasks.test.ts (17 tests) 142ms
 ✓ src/components/AddTask/AddTaskDialog.test.tsx (14 tests) 2530ms
 ✓ src/api/events.test.ts (16 tests) 163ms
 ✓ src/components/Rss/FeedsScreen.test.tsx (10 tests) 2699ms
 ✓ src/eslint.test.ts (1 test) 1221ms
 ✓ src/sw.test.ts (2 tests) 9ms
 ✓ src/api/client.test.ts (12 tests) 14ms
 ✓ src/components/Search/SavedSearches.test.tsx (9 tests) 3008ms
 ✓ src/components/Settings/SettingsScreen.test.tsx (12 tests) 3100ms
 ✓ src/lib/theme.test.ts (9 tests) 3117ms
 ✓ src/components/Rss/RuleEditor.test.tsx (9 tests) 5167ms
   ✓ TestPreviewDebouncesToOneRequest  1392ms
   ✓ TestPreviewListsNonMatchesWithReason  535ms
   ✓ TestInvalidRegexShowsInlineErrorAndKeepsPreview  848ms
   ✓ TestSaveSendsMappedDocument  698ms
   ✓ TestHighlightConvertsUtf8Offsets  381ms
   ✓ TestTitlePanelPostsTitles  435ms
   ✓ TestIgnoreStateToggleReposts  653ms
 ✓ src/components/Search/SearchScreen.test.tsx (10 tests) 6200ms
 ✓ src/App.test.tsx (55 tests) 6155ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx (34 tests) 10499ms

 Test Files  22 passed (22)
      Tests  279 passed (279)
   Duration  12.64s

RULE_EDITOR_OK
```

`make test` (Go suites) on the same tree:

```
$ go test ./...
ok  	github.com/L-K-M/dl-tool/internal/api	35.127s
ok  	github.com/L-K-M/dl-tool/internal/config	0.086s
ok  	github.com/L-K-M/dl-tool/internal/engine	4.699s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	1.846s
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	7.465s
ok  	github.com/L-K-M/dl-tool/internal/fsx	0.022s
ok  	github.com/L-K-M/dl-tool/internal/jobs	1.351s
ok  	github.com/L-K-M/dl-tool/internal/obs	0.078s
ok  	github.com/L-K-M/dl-tool/internal/rss	(cached)
ok  	github.com/L-K-M/dl-tool/internal/search	1.408s
ok  	github.com/L-K-M/dl-tool/internal/secure	1.447s
ok  	github.com/L-K-M/dl-tool/internal/store	8.328s
ok  	github.com/L-K-M/dl-tool/internal/sync	3.306s
ok  	github.com/L-K-M/dl-tool/internal/uri	0.016s
```

Scope — this branch commits incrementally, so `git status` is clean at the head and the branch
scope is `git diff origin/main...HEAD --name-only` plus the two untracked-at-the-time files. It is
exactly the Files table plus the two generated artifacts, and nothing else:

```
api/openapi.json
internal/api/rules.go
internal/api/rules_test.go
internal/rss/dryrun.go
internal/rss/dryrun_test.go
internal/rss/grab.go
internal/rss/grab_test.go
internal/rss/ruledoc.go
web/src/App.tsx
web/src/api/schema.d.ts
web/src/components/Rss/RuleEditor.test.tsx
web/src/components/Rss/RuleEditor.tsx
web/src/locales/en/rss.json
```

## Blocked

This task cannot run as written: three of its requirements have no backend or document member to
bind to, every fix lives outside the `## Files` table, and all three are already registered as
confirmed in `PLAN-REVIEW-FINDINGS.md` (F115 and F116 at HIGH, F379 at M). Each was re-verified
against the code on this branch.

1. **Step 4's substring highlight has no offsets on the wire (F116).** Step 4 says "using the
   offsets the response carries" and Out-of-scope says every highlight offset comes from
   `POST /rules/test`, but `rss.DryRunItem` (`internal/rss/dryrun.go:39-51`) has no offset
   member. The engine computes one — `Decision.Highlight [2]int`, byte offsets into the title
   (`internal/rss/match.go:46`), populated at `internal/rss/match.go:481` — and `DryRun` drops it.
   The generated `web/src/api/schema.d.ts` `DryRunItem` (lines 1097-1112) confirms the wire shape
   carries only `matched_by` (clause → pattern). The two renderings the table permits are both
   forbidden: re-running the pattern in the browser (step 4's "never", and wildcard entries such
   as `ubuntu *desktop* amd64` are not substrings of the title anyway) or silently dropping a
   documented requirement (doc 08 §8 requirement 3, listed "in priority order").

2. **Step 7's docked "Test a title" panel has no request to send (F115).** `POST /rules/test`
   accepts exactly `rule`, `feeds`, `limit`, `ignore_state` (`internal/api/rules.go:93-99`) and
   `rss.DryRun` evaluates stored `feed_items` only. No member carries an arbitrary title, so the
   "one-item dry run" cannot be issued; inventing one is an undocumented API edge, and evaluating
   the title client-side is forbidden ("Do NOT implement the matching algorithm in TypeScript").

3. **The mandatory `Tags` label has no document member (F379).** The acceptance criterion demands
   all thirteen labels verbatim, `Tags` included, but `RuleDoc`/`ActionSpec`
   (`internal/rss/ruledoc.go:40-50, 88-94`) define no `tags` member and `rss.GrabRequest`
   (`internal/rss/grab.go:29-38`) carries none — a rendered control would save nothing and tag
   nothing at grab time.

Adjacent fact the repair should pin while it is here: step 5's example location is
`body.definition.episode.filter`, but the dry-run 422 is emitted under `body.rule.`
(`internal/api/rules.go:458` calls `ruleValidationProblemAt("body.rule.", …)` while CRUD uses
`body.definition.` at `rules.go:528`) — F255's recorded conflict. The editor must map both
prefixes, and the contract should say so. Doc 09 §8.2's toolbar *Import / export rules* (F317)
also has no owning step here; the Out-of-scope note assumes the toolbar exists.

Evidence to rerun before ruling, observed on this branch:

- `sed -n '39,51p' internal/rss/dryrun.go` — the `DryRunItem` struct verbatim: no offset member
  under any name.
- `grep -n "Highlight" internal/rss/match.go internal/rss/dryrun.go` — `match.go:46` declares the
  `[2]int` offsets, `match.go:481` fills them, and `dryrun.go` never mentions the field.
- `sed -n '1097,1112p' web/src/api/schema.d.ts` — the generated `DryRunItem` members: `download_url`,
  `feed`, `feed_id`, `matched`, `matched_by?`, `published_at`, `reason?`, `reason_detail?`,
  `score?`, `title`, `would_do?`; no offsets.
- `sed -n '93,99p' internal/api/rules.go` — `TestRuleInput.Body` is `Rule`, `Feeds`, `Limit`,
  `IgnoreState`; no `titles`/`items` member.
- `grep -n "tags" internal/rss/ruledoc.go internal/rss/grab.go` — no match.

Remedies, mirroring the findings' suggested fixes:

1. For F116: add `highlight [start,end]` (byte offsets into the UTF-8 `title`, omitted when
   `{0,0}`, present only when `matched`) to `rss.DryRunItem` in `internal/rss/dryrun.go`,
   populated from `Decision.Highlight`; doc 05 §10.3 must pin that the offsets are UTF-8 bytes
   and that the editor converts them to UTF-16 code-unit indices before slicing — Go bytes and
   JavaScript string indices only coincide for ASCII titles; update its example with a
   non-ASCII title so the two index systems diverge; widen T073's
   Files table with `internal/rss/dryrun.go` and `internal/rss/dryrun_test.go`
   (`api/openapi.json` and `web/src/api/schema.d.ts` are already implicitly in scope via
   doc 13 §7.1). Alternative: drop requirement 3 of doc 08 §8 and step 4 of this task.
2. For F115: add an optional `titles: string[]` member to `POST /rules/test` — doc 05 §10.3's body
   table, `TestRuleInput.Body` in `internal/api/rules.go`, and a title-evaluation path in
   `internal/rss/dryrun.go` returning the same `DryRunReport` shape — and widen T073's Files table
   with `internal/api/rules.go`, `internal/api/rules_test.go`, `internal/rss/dryrun.go` and
   `internal/rss/dryrun_test.go`, citing the member from step 7. Alternative: delete the docked
   test panel from doc 09 §8.2 and step 7.
3. For F379: add a `tags` member to the rule document (doc 08 §4.1/§4.2,
   `internal/rss/ruledoc.go`) and to `rss.GrabRequest` (`internal/rss/grab.go:29-38`), thread
   `GrabRequest.Tags` into `ruleTaskCreator.CreateForRule` (`internal/api/rules.go:141-165`
   already builds the `POST /tasks` body, whose `tags` member exists), add the mapping row to
   this task's field table, and widen the Files table with `internal/rss/ruledoc.go`,
   `internal/rss/grab.go`, and their tests. Alternative: drop `Tags` from
   doc 09 §8.2's thirteen labels and from this task.
4. In all cases pin the `errors[].location` prefixes the editor maps: `body.rule.` from the dry
   run, `body.definition.` from save. F317's toolbar import/export still has no owning task —
   assign one or record it Out-of-scope before T073 can close.

**Repair applied 2026-09-20.** The ruling took the additive half of remedies 1–3 — the smallest
interpretation consistent with the accepted ADRs, the merged code and doc 09 §1's comparison table,
which advertises the docked test field, the live preview and first-class tags — plus remedy 4 in full:

- F116: `DryRunItem` gains `Highlight *[2]int` (`json:"highlight,omitempty"`), filled from
  `Decision.Highlight` only when `Matched` and the span is non-empty. Doc 05 §10.3 now pins the
  `[start,end)` half-open UTF-8 byte offsets and that the editor converts them to UTF-16 code-unit
  indices before slicing, and its example carries a non-ASCII title (`täysi ubuntu-…`, `[7,13]` bytes
  vs `[6,12]` code units) so the two index systems diverge.
- F115: `POST /rules/test` gains `titles: string[]` — doc 05 §10.3's body table,
  `TestRuleInput.Body.Titles` and `DryRunRequest.Titles` — evaluated statelessly in place of stored
  items, one synthesized `store.FeedItem` per title (`feed_id`, `feed`, `download_url` and
  `published_at` all JSON `null`, request order preserved), `feeds`, `limit` and `ignore_state`
  inapplicable, bounded at 50 titles of 500 UTF-8 bytes each with an empty array a `422`. Step 8 cites
  the member.
- F379: `ActionSpec` gains `Tags` (doc 08 §4.1/§4.2's `action.tags`, created on demand like
  `POST /tasks`' `tags`), `GrabRequest` gains `Tags`, `Commit` populates it and
  `ruleTaskCreator.CreateForRule` forwards it into the `POST /tasks` body — which already accepts
  `tags`. `WouldDo` gains `Tags` so the dry-run preview echoes them. The field-mapping table gains the
  `Tags` row.
- Remedy 4: doc 05 §10.3 now pins both `errors[].location` prefixes (`body.rule.` dry run,
  `body.definition.` save) and the editor maps both onto the same control — F255 resolved. F317's
  import/export is assigned here: step 11 wires it through `GET /rules` + `POST /rules`, skipping
  names that already exist (a duplicate name is `409`) and reporting the skips — no new endpoint.
  Two adjacent rows closed in the same pass: F315's second half (**Add stopped** is a
  checkbox bound to `action.paused` — no global add-stopped setting exists, so `Use global ▾` was a
  wireframe artifact) and F316 (the editor renders only the newest `PREVIEW_LIMIT` merged rows and
  counts the headline's `N` among them, so "the last 50 items" is literal).
- The `## Files` table carries `internal/rss/dryrun.go`, `internal/rss/dryrun_test.go`,
  `internal/rss/ruledoc.go`, `internal/rss/grab.go`, `internal/rss/grab_test.go`,
  `internal/api/rules.go` and `internal/api/rules_test.go`; `api/openapi.json` and
  `web/src/api/schema.d.ts` stay implicitly in scope via doc 13 §7.1.

F115, F116, F255, F315, F316, F317 and F379 are marked resolved in `PLAN-REVIEW-FINDINGS.md`. The index
row stays `todo`; the next loop iteration implements the task.
