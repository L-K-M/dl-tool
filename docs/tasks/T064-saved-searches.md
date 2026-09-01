# T064 — Save and re-run a search

| Field | Value |
|---|---|
| **ID** | T064 |
| **Milestone** | M4 |
| **Status** | todo |
| **Depends on** | T043, T045, T061, T063 |
| **Blocks** | — |
| **Parallel-safe** | no — extends T063's `SearchScreen.tsx` and T045's `useUiPrefs.ts` |
| **Implements** | [FR-057](../02-requirements.md#fr-057-save-and-re-run-a-search), [FR-058](../02-requirements.md#fr-058-create-a-task-from-a-search-result-in-one-click) (its end-to-end proof) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md) |
| **Est. size** | 3 new files, ~330 LOC |

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

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/Search/SavedSearches.tsx` | create | The `Save…` dialog and the `Saved ▾` popover. |
| `web/src/components/Search/SavedSearches.test.tsx` | create | Save, re-run, rename, delete and the 50-entry cap. |
| `web/e2e/search.spec.ts` | create | The browser proof of search → one-click add, and of re-running a saved search. |
| `web/src/components/Search/SearchScreen.tsx` | edit | Mount the two controls; read the selection from prefs instead of `sessionStorage`. |
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
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
