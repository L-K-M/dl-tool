# T063 — Build the search screen

| Field | Value |
|---|---|
| **ID** | T063 |
| **Milestone** | M4 |
| **Status** | todo |
| **Depends on** | T014, T041, T042, T044, T061, T062 |
| **Blocks** | T064 |
| **Parallel-safe** | no — replaces the `/search` placeholder in T040's `App.tsx` |
| **Implements** | [FR-058](../02-requirements.md#fr-058-create-a-task-from-a-search-result-in-one-click) (client half; the server half is T020) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md) |
| **Est. size** | 3 new files, ~430 LOC |

## Goal
`/search` renders Download Station's search screen: query box, indexer multi-select, category filter,
`Search` / `Stop`, a live per-indexer status strip, a virtualised results grid sorted by seeders descending,
a per-row `⬇`, and `Download selected ▾` with exactly two items. Results appear as each indexer answers.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/09-web-ui-spec.md` §7 Search screen](../09-web-ui-spec.md#7-search-screen) — the wireframe, the
   eight behaviour bullets and the three distinct zero states.
2. [`docs/05-api-contract.md` §9.2 Search job lifecycle](../05-api-contract.md#92-search-job-lifecycle) — the
   poll body, `engines[].status` and the sort values.
3. [`docs/05-api-contract.md` §9.1 Indexer CRUD](../05-api-contract.md#91-indexer-crud) — the indexer list and
   `GET /indexers/categories`.
4. [`docs/09-web-ui-spec.md` §10.5 Empty states](../09-web-ui-spec.md#105-empty-states) and
   [§10.6 Toasts and optimistic updates](../09-web-ui-spec.md#106-toasts-and-optimistic-updates).
5. [`docs/09-web-ui-spec.md` §3.9 Virtualisation](../09-web-ui-spec.md#39-virtualisation) and
   [§10.4 Accessibility](../09-web-ui-spec.md#104-accessibility) — `aria-rowcount` is the total, and the
   status strip is `aria-live="polite"`.
6. [`docs/tasks/T042-virtualised-task-grid.md`](T042-virtualised-task-grid.md) — the TanStack Table v8 and
   virtualiser patterns to reuse, and [`T041`](T041-task-store-and-formatters.md) for `formatBytes` and
   `formatWhen`.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/Search/SearchScreen.tsx` | create | The screen, the poll loop, the indexer and category pickers, the status strip. |
| `web/src/components/Search/ResultsGrid.tsx` | create | The virtualised result table and its row actions. |
| `web/src/components/Search/SearchScreen.test.tsx` | create | Poll lifecycle, status strip, zero states, add-to-queue bodies. |
| `web/src/App.tsx` | edit | Replace `<Placeholder screen="search" />` with `<SearchScreen />`. |
| `web/src/locales/en/common.json` | edit | Search-screen strings. |

No other file may be modified.

## Interface contract

```tsx
// web/src/components/Search/SearchScreen.tsx
export interface EngineStatusView {
  id: string;
  name: string;
  status: 'queued' | 'searching' | 'done' | 'error';
  count: number;
  error: string | null;
}

export interface SearchResultView {
  id: string;
  indexer_id: string;
  indexer_name: string;
  title: string;
  download_url: string | null;
  magnet_uri: string | null;
  info_hash: string | null;
  size_bytes: number;
  seeders: number | null;
  leechers: number | null;
  published_at: string | null;
  details_url: string | null;
  category_ids: number[];
}

/** Starts a job with POST /search, then polls GET /search/{id} every 1000 ms until
 *  finished, then stops. Stop() abandons the poll and calls DELETE /search/{id}. */
export function useSearchJob(): {
  start: (q: { query: string; indexer_ids: string[]; categories: number[] }) => Promise<void>;
  stop: () => Promise<void>;
  jobId: string | null;
  finished: boolean;
  total: number;
  engines: EngineStatusView[];
  results: SearchResultView[];
  error: string | null;
};

export function SearchScreen(): JSX.Element;
```

```tsx
// web/src/components/Search/ResultsGrid.tsx
/** Columns in this order, matching doc 09 §7: checkbox, Name, Size, Seeders, Leechers,
 *  Age, Indexer, and the per-row download action. Default sort is seeders descending.
 *  A null seeder or leecher count renders the em dash "—", never 0 and never -1. */
export const RESULT_COLUMN_ORDER = [
  'select', 'title', 'size', 'seeders', 'leechers', 'age', 'indexer', 'actions',
] as const;

export function ResultsGrid(props: {
  results: SearchResultView[];
  total: number;
  selected: Set<string>;
  onSelectedChange: (next: Set<string>) => void;
  onDownload: (r: SearchResultView, mode: 'immediate' | 'choose') => void;
}): JSX.Element;
```

Adding a result posts the URI already returned by the search — no second lookup:

```ts
await api.POST('/tasks', {
  body: { uris: [r.magnet_uri ?? r.download_url!], destination: defaultDestination },
  headers: { 'X-DLTOOL-CSRF': csrfToken()! },
});
```

The three zero states, each its own render: `No results` (every indexer answered with zero),
`All indexers failed` (list every `engines[].error`, offer `Retry`), `No indexers enabled` (link to
`/settings/indexers`).

## Steps
1. Create `SearchScreen.tsx` with `useSearchJob`, built on `@tanstack/react-query` with
   `refetchInterval: (data) => (data?.finished ? false : 1000)`.
2. Render the query input, the indexer popover (checkbox list with *All* / *None*, a health dot per row and
   the last error in its tooltip, disabled indexers greyed with a link to `/settings/indexers`), and the
   category select populated from `GET /indexers/categories`.
3. Keep the indexer and category selection in `sessionStorage` under `dl.search.v1` so it survives navigation
   within the session; T064 moves it into the preference document.
4. Render the per-indexer status strip with `aria-live="polite"` and the four states `queued ○`,
   `searching ◐`, `done ● N`, `error ✕`, the error visible on hover **and** on click.
5. Create `ResultsGrid.tsx` reusing T042's TanStack Table v8 plus `@tanstack/react-virtual` setup, with
   `RESULT_COLUMN_ORDER`, `aria-rowcount` set to `total`, and `formatBytes` and `formatWhen` from T041.
6. Render every null numeric cell as `—`, and grey the seeders and leechers cells for a row whose indexer
   reported `seeders_unknown`.
7. Wire the per-row `⬇` to `onDownload(r, 'immediate')`: post `/tasks`, flash the row, replace the button with
   a `✓` linking to the created task, and show one toast per failure naming the result title.
8. Wire the footer: the selected count and summed size, and `Download selected ▾` with exactly two items —
   *Download immediately* and *Download to…*, the latter opening T049's add dialog pre-filled with the
   selected URIs.
9. Edit `App.tsx` to route `/search` to `<SearchScreen />`, and add every string to
   `web/src/locales/en/common.json` under a `search` key; no literal user-facing text in the components.
10. Create `SearchScreen.test.tsx` with `msw` handlers: `TestPollStopsWhenFinished`,
    `TestPartialResultsRenderBeforeFinish`, `TestEngineErrorShownInStrip`, `TestThreeZeroStates`,
    `TestNullCountsRenderEmDash`, `TestRowDownloadPostsMagnetOrURL` and `TestStopDeletesTheJob`.
11. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestPollStopsWhenFinished` asserts no further `GET /search/{id}` request is made after `finished:true`.
- [ ] `TestPartialResultsRenderBeforeFinish` asserts rows render while one indexer is still `searching`.
- [ ] `TestThreeZeroStates` asserts three distinct rendered states, each with its documented call to action.
- [ ] `TestRowDownloadPostsMagnetOrURL` asserts the body carries `magnet_uri` when present and `download_url` otherwise, plus the CSRF header.
- [ ] `aria-rowcount` equals the job's `total`, not the number of rows in the DOM.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo SEARCH_UI_OK
```
Expected: Vitest reports every test named in step 10 as passing, including the file
`src/components/Search/SearchScreen.test.tsx`, and the final line of stdout is exactly `SEARCH_UI_OK`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT add saved searches, the `Save…` button or a Playwright spec; T064 owns all three.
- Do NOT add an indexer-management screen; `/settings/indexers` is M6 work and this screen only links to it.
- Do NOT hard-code a category tree in the client; it comes from `GET /indexers/categories`.
- Do NOT open a modal from the clipboard, and do NOT add optimistic task rows for a magnet result.
- Do NOT poll faster than once per second, and do NOT subscribe to SSE for search progress.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
