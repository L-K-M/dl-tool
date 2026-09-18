# T064 — Save and re-run a search

| Field | Value |
|---|---|
| **ID** | T064 |
| **Milestone** | M4 |
| **Status** | todo |
| **Depends on** | T043, T045, T061, T063, T129 |
| **Blocks** | — |
| **Parallel-safe** | no — extends T063's `SearchScreen.tsx` and [T129](T129-ui-prefs-document.md)'s `useUiPrefs.ts` |
| **Implements** | [FR-057](../02-requirements.md#fr-057-save-and-re-run-a-search), [FR-058](../02-requirements.md#fr-058-create-a-task-from-a-search-result-in-one-click) (its end-to-end proof) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md) |
| **Est. size** | 3 new files, ~360 LOC |

## Goal
`Save…` captures the current name, query, indexer selection and category selection into the server-side
preference document; a `Saved ▾` popover re-runs any of them with exactly the same selection. One Playwright
spec proves the whole M4 loop in a browser: search, then one click on a result creates a task.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/09-web-ui-spec.md` §7 Search screen](../09-web-ui-spec.md#7-search-screen) — the `Save…` control and
   what a saved search holds.
2. [`docs/05-api-contract.md` §11.4 `GET /prefs` and `PUT /prefs`](../05-api-contract.md#114-get-prefs-and-put-prefs)
   — the document travels whole and unknown members are stored verbatim.
3. [`docs/09-web-ui-spec.md` §3.3 Persistence](../09-web-ui-spec.md#33-persistence) — the debounce rule the
   preference writer already follows.
4. [`docs/13-testing-and-verification.md` §6.1 Playwright scenarios that must exist](../13-testing-and-verification.md#61-playwright-scenarios-that-must-exist)
   — the spec style and the acceptance sentence pattern.
5. [`docs/tasks/T063-search-screen.md`](T063-search-screen.md) — `useSearchJob`, `SearchResultView` and the
   `sessionStorage` selection this task replaces.
6. [`docs/tasks/T043-playwright-harness-and-grid-performance.md`](T043-playwright-harness-and-grid-performance.md)
   — `web/e2e/fixtures.ts`, `loginAsAdmin` and the throwaway state directory.
7. [`docs/tasks/T129-ui-prefs-document.md`](T129-ui-prefs-document.md) — the server transport `useUiPrefs`
   now implements: `hydrate`, the whole-document PUT and the verbatim unknown-member rule the `search`
   member rides.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/Search/SavedSearches.tsx` | create | The `Save…` dialog and the `Saved ▾` popover. |
| `web/src/components/Search/SavedSearches.test.tsx` | create | Save, re-run, rename, delete and the 50-entry cap. |
| `web/e2e/search.spec.ts` | create | The browser proof of search → one-click add, and of re-running a saved search. |
| `web/src/components/Search/SearchScreen.tsx` | edit | Mount the two controls; read the selection from prefs instead of `sessionStorage`. |
| `web/src/components/Search/SearchScreen.test.tsx` | edit | Re-pin the cases the prefs-document selection source touches. |
| `web/src/store/useUiPrefs.ts` | edit | Add the `search` member to the preference document. |

No other file may be modified.

## Interface contract

```ts
// web/src/store/useUiPrefs.ts — added to UiPrefs, which PUT /prefs stores verbatim
export interface SavedSearch {
  id: string;          // crypto.randomUUID()
  name: string;        // 1..64 characters, unique within the document
  query: string;
  indexerIds: string[];
  categories: number[];
  createdAt: string;   // RFC 3339
  lastTotal: number;   // total of the last run, for the "new since last view" badge
}

export interface SearchPrefs {
  indexerIds: string[];
  categories: number[];
  saved: SavedSearch[];   // at most 50; saving a 51st is refused with a toast
}
```

```tsx
// web/src/components/Search/SavedSearches.tsx
export function SaveSearchButton(props: {
  query: string;
  indexerIds: string[];
  categories: number[];
}): JSX.Element;

export function SavedSearchesMenu(props: {
  onRun: (s: SavedSearch) => void;
}): JSX.Element;

/** Refuses an empty name, a duplicate name and an empty query, each with the reason
 *  rendered inline under the field. */
export function validateSavedName(name: string, existing: SavedSearch[]): string | null;
```

```ts
// web/e2e/search.spec.ts — both routes are stubbed so the spec needs no engine daemon
await page.route('**/api/v1/search**', route => route.fulfill({ json: searchJobFixture }));
await page.route('**/api/v1/tasks', route => route.fulfill({ status: 201, json: taskFixture }));
```

## Steps
1. Edit `web/src/store/useUiPrefs.ts` to add `search: SearchPrefs` with the defaults
   `{ indexerIds: [], categories: [], saved: [] }`, merged like every other member so an older stored
   document still loads.
2. Create `SavedSearches.tsx` with `SaveSearchButton`, `SavedSearchesMenu` and `validateSavedName`; both
   controls read and write only through the `useUiPrefs` store, never through `fetch`.
3. Cap `saved` at 50 entries and refuse the 51st with a toast naming the cap; deleting is immediate and
   undoable only by saving again.
4. Edit `SearchScreen.tsx`: mount `SaveSearchButton` and `SavedSearchesMenu` beside `Search` and `Stop`, and
   take the indexer and category selection from `useUiPrefs().search` instead of `sessionStorage`, deleting
   the `dl.search.v1` key on first load.
5. Running a saved search sets the query and both selections, then calls `useSearchJob().start` with exactly
   the stored `indexerIds` and `categories` — never with the defaults.
6. After a run finishes, write `lastTotal` back to the saved entry and badge the entry when a later run
   returns a higher total.
7. Create `SavedSearches.test.tsx` with `TestSaveCapturesQueryAndSelection`,
   `TestRunSavedSearchUsesStoredIndexers`, `TestDuplicateNameRefused`, `TestFiftyEntryCap` and
   `TestPrefsDocumentRoundTrips`.
8. Create `web/e2e/search.spec.ts` using `loginAsAdmin` from T043's fixtures: stub the two routes above, type
   a query, press `Search`, assert the per-indexer strip reaches `done`, click the first row's `⬇`, and
   assert the button becomes `✓` and the success toast names the created task, all within five seconds.
9. Add a second case to the spec: save the search, reload the page, re-run it from `Saved ▾`, and assert the
   intercepted `POST /api/v1/search` body carries the same `indexer_ids`.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestRunSavedSearchUsesStoredIndexers` asserts the `POST /search` body equals the saved selection.
- [ ] `TestFiftyEntryCap` asserts the 51st save is refused and the document still holds 50 entries.
- [ ] The Playwright spec asserts the row's `⬇` becomes `✓` within five seconds of the click.
- [ ] The Playwright spec's second case asserts the re-run body carries the stored `indexer_ids`.
- [ ] Reloading the page restores the query selection from `GET /prefs`, with no `sessionStorage` fallback left.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && make e2e && echo SAVED_SEARCH_OK
```
Expected: Vitest reports every test named in step 7 as passing; the Playwright list reporter reports
`web/e2e/search.spec.ts` with `2 passed`; the final line of stdout is exactly `SAVED_SEARCH_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT edit `Sidebar.tsx`; T044 owns the sidebar tree and its saved-search branch is not part of M4.
- Do NOT add a server-side saved-search table or endpoint; the preference document already stores unknown
  members verbatim.
- Do NOT start aria2 or qBittorrent for the Playwright spec; both stubbed routes exist so it needs neither.
- Do NOT re-run a saved search automatically on a schedule; that is RSS, and M5 owns it.
- Do NOT store a saved search in `localStorage`; the document is server-side so every browser agrees.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
Stopped mid-implementation (2026-09-18): the `## Files` table has no locale
catalogue row, but the two controls need new user-visible strings and every
shipped string must resolve through `t()` against a catalogue key.

The store member, both controls and the `SearchScreen` wiring are implemented
and typecheck-clean on branch `task/T064-saved-searches` (commits `4d42caf`,
`6cce22e`), along with `SavedSearches.test.tsx`, the `SearchScreen.test.tsx`
re-pin and `web/e2e/search.spec.ts`; only the string catalogue blocks the
Verification run. [T063](T063-search-screen.md)'s own `## Files` table carries
`web/src/locales/en/common.json` for the same reason — this task's table was
written without one, although the `Save…` dialog, the `Saved ▾` popover, its
empty state, four inline refusal reasons, the 50-entry cap toast, the
rename/delete aria labels and the "new since last view" badge all introduce
text no existing key covers.

The catalogue requirement is not stylistic. `web/eslint.config.js` rejects
literal JSX text ("add the key to a locale catalogue"), `initI18n` installs a
`parseMissingKeyHandler` that returns the key verbatim — so a missing key
renders `search.savedMenu`, not a label — and `i18n.test.ts`'s
`TestMissingKeyThrowsInTests` keeps a strict mode that treats a missing key
as a thrown error. Verified on this tree:

```text
$ node -e 'i18next.init({resources:{en:{common:{}}}, parseMissingKeyHandler:(k)=>k, interpolation:{escapeValue:false}}).then(()=>{console.log(i18next.t("search.savedMenu",{defaultValue:"Saved"}));console.log(i18next.t("Added {{title}}",{title:"x"}))})'
search.savedMenu        # defaultValue is overridden by the missing-key handler
Added {{title}}         # handler output is not interpolated, so natural
                        # language keys cannot carry placeholders either
```

The same fallback is what the new tests trip over — all five
`SavedSearches.test.tsx` cases fail at `getByRole("button", { name: /^Saved/ })`
because the rendered label is the literal text `search.savedMenu`. The unit
tests cannot pass until the keys exist in a catalogue, which makes the
missing table row load-bearing rather than cosmetic.

Every permitted path is therefore forbidden:

- Literal JSX text — banned by the `no-restricted-syntax` rule
  (`JSXText[value=/[A-Za-z]{2,}/]`).
- `t()` with a catalogue-missing key, `defaultValue` or a natural-language
  key — renders the key string in production and throws under strict mode;
  `TestNoEmptyCatalogueValues` and `TestM3CataloguesExist` pin en as the
  complete shipped catalogue ([`09-web-ui-spec.md` §10.2](../09-web-ui-spec.md#102-i18n)).
- Reusing existing keys — the closest candidates
  (`shell.sidebar.savedSearches`, `settings.dirty.save`,
  `errors.problem.conflict`) cover at most three of the strings; "Save…",
  the empty-name/too-long/empty-query refusals, the cap toast, the added
  toast and the badge have no plausible key, and cross-namespace reuse
  (`settings:`, `errors:`) for a search-screen control is itself a drift
  hazard the catalogue organisation exists to prevent.

Which file should answer it: `web/src/locales/en/common.json`, the file
T063's `## Files` table listed for the same component family. The repair is
one row in this task's `## Files` table (plus the `search.*` keys for the
strings above); the staged branch then resumes with a mechanical
`defaultValue`-to-catalogue swap. The alternative — a documented exemption
letting this component embed English text — weakens the i18n convention for
every later task and should be an ADR, not an inline choice.

Resolved by the plan repair that created [T129](T129-ui-prefs-document.md): the `## Blocked` record
below named two repairs, and the repair took the first — the prefs slice of T107 moved forward into a
new earlier task rather than into this one's `## Files` table. T129 owns `GET`/`PUT /prefs`, the
`Prefs`/`PutPrefs` store accessors, the `useUiPrefs` transport switch off `localStorage`, and the
client call sites and re-pinned tests the switch drags with it (`theme.ts`, `GeneralSection.tsx`,
hydration in `App.tsx`). This task's table gained only a `SearchScreen.test.tsx` row for the
selection-source change; its goal, contract and criteria now read as written because the document
exists when it runs. T107 narrowed to the tag and watch-folder operations and dropped FR-144. T064
remains unimplemented and still marked `todo`, now depending on T129.

Stopped before implementing (2026-09-18): the task assumes the server-side preference document
exists; it does not, and building it needs files outside the `## Files` table.

The goal, the interface contract, the acceptance criteria and the out-of-scope rules all turn on
`GET`/`PUT /prefs`: "the server-side preference document", "`UiPrefs`, which `PUT /prefs` stores
verbatim", "restores the query selection from `GET /prefs`", "the preference document already
stores unknown members verbatim" and "Do NOT store a saved search in `localStorage`". Reality:
no `/prefs` route is registered, the `ui_prefs` table has no reader or writer, and `useUiPrefs` —
the only persistence file this task may edit — reads and writes `localStorage` exclusively.
[T045](T045-column-management-and-ui-prefs.md) built it that way deliberately ("the endpoints
arrive with M6's preferences task (T107) and this task persists to `localStorage` only"), and
[T107](T107-tag-and-watch-folder-endpoints.md) (M6, `todo`) owns `internal/api/prefs.go`,
`Prefs`/`PutPrefs` in `internal/store/settings.go`, the `internal/api/server.go` registration and
`internal/api/prefs_test.go`.

So every permitted implementation path is forbidden:

- `localStorage` — the only storage `useUiPrefs` has — is barred by "Do NOT store a saved search
  in `localStorage`; the document is server-side so every browser agrees."
- Server-side — required by the goal and the `GET /prefs` acceptance criterion — needs the four
  T107 files plus regenerated `api/openapi.json` and `web/src/api/schema.d.ts`, all outside this
  Files table.
- The criterion "restores the query selection from `GET /prefs`, with no `sessionStorage`
  fallback left" names a test no code can pass: there is no `GET /prefs` to restore from, and the
  e2e spec stubs only `/api/v1/search**` and `/api/v1/tasks` against the real binary.

Evidence (run from the repo root at `1c418df`, verbatim):

```text
$ grep -rn "prefs\|Prefs" internal/api/ --include='*.go'
(exit 1 — no matches; not even a test file mentions prefs)
$ grep -in 'prefs' api/openapi.json
(exit 1 — no matches; no route, schema member or tag mentions prefs)
$ python3 -c "import json; print(len(json.load(open('api/openapi.json'))['paths']))"
31
$ grep -rn "ui_prefs" internal/ | grep -v migrations
internal/store/db_test.go:37: "tasks", "ui_prefs", "users", "watch_folders",
internal/store/db_test.go:49: "idx_tasks_state", "idx_tasks_updated", "idx_ui_prefs_key",
$ grep -n "ui_prefs" internal/store/migrations/00001_init.sql
288:CREATE TABLE ui_prefs (
293:CREATE UNIQUE INDEX idx_ui_prefs_key ON ui_prefs(user_id, key);
316:DROP TABLE ui_prefs;
(the table exists only in the initial migration; the sole non-migration references are the
expected table/index lists in internal/store/db_test.go, so no application code reads or writes
its rows)
$ grep -n "localStorage\|api\.\|fetch" web/src/store/useUiPrefs.ts
86:      localStorage.getItem(PREFS_KEY) ?? "null",
182:        localStorage.setItem(PREFS_KEY, JSON.stringify(merged));
(the store never touches the network; its entire persistence is localStorage)
```

Which file should answer it: this task's `## Files` table together with
[T107](T107-tag-and-watch-folder-endpoints.md)'s. The repair choices are (a) pull the prefs
slice of T107 forward into this task or a new earlier task — `internal/api/prefs.go` (the prefs
pair only), `Prefs`/`PutPrefs` in `internal/store/settings.go`, two registrations in
`internal/api/server.go`, `internal/api/prefs_test.go` (the prefs pins), the §7.1 generated pair,
and the `useUiPrefs` transport switch — with T107's table narrowed to tags and watch folders; or
(b) amend this task's text so `search` persists in the document as it exists today — `localStorage`
until T107 wires the transport — rewriting the goal, the `PUT /prefs` contract note, the
`GET /prefs` acceptance criterion and the localStorage prohibition, which weakens FR-057's "every
browser agrees" until M6. Re-sequencing T064 behind T107 would invert the milestone order: T107
sits in M6 behind T083/T084, so M4 could not close.

Separately — a pre-existing main defect this task's Verification exposed: `make e2e` failed in
`web/e2e/pwa.spec.ts` "api requests bypass the cache" because the spec still polled the literal
cache name `"dl-tool-assets-v1"` while `web/public/sw.js` since `e07b159` (#195) writes
`dl-tool@<scope>:assets-v1` and deletes the legacy name on activation. The failure was
deterministic (`Received array: []` after the 5 s poll) on the `Task verification` runs of this
docs-only branch — the first `make e2e`-running task branch since #195 merged — and was the
pre-existing defect, not a T064 regression. The repair landed on main as #218 (the spec now
discovers the `:assets-v1` cache by suffix) and this branch carries it via the merge of
`65fb87f`; the pwa spec passes locally on this tree.
