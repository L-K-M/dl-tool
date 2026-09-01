# T073 — Build the rule editor with the live dry-run preview

| Field | Value |
|---|---|
| **ID** | T073 |
| **Milestone** | M5 |
| **Status** | todo |
| **Depends on** | T047, T050, T068, T070, T071, T072 |
| **Blocks** | — |
| **Parallel-safe** | no — adds a route to T040's `web/src/App.tsx` and extends T072's `rss.json` |
| **Implements** | — (renders [FR-073](../02-requirements.md#fr-073-evaluate-rules-with-the-documented-algorithm), [FR-075](../02-requirements.md#fr-075-dry-run-a-rule-and-explain-every-item) and [FR-077](../02-requirements.md#fr-077-run-a-rule-against-existing-items), covered by T069, T070 and T071) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md), [ADR-0009](../decisions/0009-native-cross-protocol-rss-rules.md) |
| **Est. size** | 2 new files, ~420 LOC |

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
5. [`docs/09-web-ui-spec.md` §4.1 Server-side folder browser](../09-web-ui-spec.md#41-server-side-folder-browser)
   — the `Destination` field reuses T047's browser unchanged.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/Rss/RuleEditor.tsx` | create | The rules list, the form and the preview column. |
| `web/src/components/Rss/RuleEditor.test.tsx` | create | Debounce, preview, inline regex error and save cases. |
| `web/src/locales/en/rss.json` | edit | Field labels, the ten reason sentences and the help popover. |
| `web/src/App.tsx` | edit | Route `/rss/rules` to the editor. |

No other file may be modified.

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
| Add stopped | `action.paused` |
| Ignore subsequent matches for … days | `throttle.cooldown_days` |

The headline reads exactly `matches N of the last 50 items`.

## Steps
1. Create `web/src/components/Rss/RuleEditor.tsx` with the three columns of doc 09 §8.2: the rules list
   with `[+] [⧉] [🗑]`, the form, and the preview.
2. Implement `useRulePreview` with a 250 ms debounce and an `AbortController` so an in-flight dry run is
   cancelled when the user types again; never queue two requests.
3. Post `{rule, feeds, limit: 50, ignore_state}` to `POST /rules/test` and render `results[]` in order:
   matches with `✓`, non-matches greyed with `✗` and the reason sentence from the `rss` namespace, keyed
   by `reason` and interpolating `reason_detail`.
4. Highlight the matched substring in a matched title using the offsets the response carries; never
   re-run the pattern in the browser.
5. Render a `422` inline under the responsible control by mapping `errors[].location` — for example
   `body.definition.episode.filter` — to the field, and keep the previous preview visible.
6. Bind the *Ignore already-downloaded* toggle to `ignore_state`, defaulting to on, and show `evaluated`,
   `matched` and `elapsed_ms` beneath the list.
7. Implement the docked test panel: a title input and `Test`, which posts a one-item dry run and renders
   `✓ MATCH` or the reason, naming the clause that decided it.
8. Add the `(?)` popover beside the mode toggle carrying the two qBittorrent help sentences of doc 09 §8.2
   verbatim, stored as two keys in `rss.json`.
9. Wire `Save` to `POST /rules` or `PATCH /rules/{id}`, and *Run rule against existing items* to
   `POST /rules/{id}/run`, confirming first and reporting `evaluated`, `matched` and the created count.
10. Use T047's folder browser for `Destination` and T050's category list for `Category`; add no new picker.
11. Edit `web/src/locales/en/rss.json` with the labels, the ten reason sentences and the help text, then
    edit `web/src/App.tsx` to route `/rss/rules`.
12. Create `web/src/components/Rss/RuleEditor.test.tsx` with `msw`: typing fires exactly one dry run after
    250 ms; the headline reads `matches 7 of the last 50 items`; a non-match renders its `✗` row and the
    reason sentence; a `422` on an unterminated group renders inline under **Must contain** and the
    previous preview survives; toggling *Ignore already-downloaded* re-posts with `ignore_state: false`;
    `Save` sends the mapped document; `Run` posts to `/rules/{id}/run`.
13. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestPreviewDebouncesToOneRequest` asserts one call for five keystrokes inside 250 ms.
- [ ] `TestPreviewListsNonMatchesWithReason` asserts the `✗` row and its sentence.
- [ ] `TestInvalidRegexShowsInlineErrorAndKeepsPreview` passes.
- [ ] `TestSaveSendsMappedDocument` asserts `match.any_of` is an array of lines, never a `|`-joined string.
- [ ] The thirteen labels appear character for character as doc 09 §8.2 gives them.
- [ ] The headline string is `matches N of the last 50 items`.

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
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

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
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
