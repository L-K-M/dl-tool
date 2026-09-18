# T063 — Build the search screen

| Field | Value |
|---|---|
| **ID** | T063 |
| **Milestone** | M4 |
| **Status** | todo |
| **Depends on** | T014, T041, T042, T044, T061, T062 |
| **Blocks** | T064 |
| **Parallel-safe** | no — replaces the `/search` placeholder in T040's `App.tsx` |
| **Implements** | [FR-058](../02-requirements.md#fr-058-create-a-task-from-a-search-result-in-one-click) (client and server halves; the "server half is T020" attribution predates `res_` ids — T020's merged contract has no `search_result_ids` member — so this task's Files table covers both) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md) |
| **Est. size** | 4 new files, 8 modified, ~1,000 LOC |

## Goal
`/search` renders Download Station's search screen: query box, indexer multi-select, category filter,
`Search` / `Stop`, a live per-indexer status strip, a virtualised results grid sorted by seeders descending,
a per-row `⬇`, and `Download selected ▾` with exactly two items. Results appear as each indexer answers.
Both download paths submit `search_result_ids`, so this task also adds that fourth source family to
`POST /tasks` and resolves each `res_` id to its server-only acquisition source.

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
7. [`docs/05-api-contract.md` §5.2 `POST /tasks`](../05-api-contract.md#52-post-tasks) — the
   `search_result_ids` source family: one family per request, per-id authorisation through its live search
   job, the `search-result:<res_id>` display source and the provider-free rejection shape.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/store/search.go` | modify | `GetSearchResult` — resolve a `res_` id to its row, acquisition fields included, while its job is live. |
| `internal/store/search_test.go` | create | `GetSearchResult` cases: live job resolves; unknown id, a deleted job and a row with neither `magnet_uri` nor `download_url` are all `ErrNotFound`. |
| `internal/store/tasks.go` | modify | `SourceDisplayURI` on `Task`, the insert column and the reads that feed the task DTO. |
| `internal/api/tasks.go` | modify | `search_result_ids` on `CreateTasksBody`, the one-source-family rule, resolution and the `search-result:<res_id>` display source. |
| `internal/api/tasks_test.go` | modify | `humatest` cases for the search-result family and its rejections. |
| `internal/api/search_test.go` | modify | Asserts a seeded result's serialized `GET /search/{id}` row carries no acquisition key. |
| `web/src/components/Search/SearchScreen.tsx` | create | The screen, the poll loop, the indexer and category pickers, the status strip. |
| `web/src/components/Search/ResultsGrid.tsx` | create | The virtualised result table and its row actions. |
| `web/src/components/Search/SearchScreen.test.tsx` | create | Poll lifecycle, status strip, zero states, add-to-queue bodies. |
| `web/src/components/AddTask/AddTaskDialog.tsx` | modify | `initialSearchResultIds` pre-fill so *Download to…* submits `res_` ids. |
| `web/src/components/AddTask/AddTaskDialog.test.tsx` | modify | The search-result draft: no URL entry, Create posts `search_result_ids`. |
| `web/src/App.tsx` | edit | Replace `<Placeholder screen="search" />` with `<SearchScreen />`. |
| `web/src/locales/en/common.json` | edit | Search-screen strings and the add dialog's result-draft line. |

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

/** Mirrors the generated SearchResultDTO: metadata and the opaque res_ id only.
 *  download_url, magnet_uri and details_url are server-only acquisition data and
 *  have no field here (doc 05 §9.2, doc 07 §5 rule 6). */
export interface SearchResultView {
  id: string;
  indexer_id: string;
  indexer_name: string;
  title: string;
  info_hash: string | null;
  size_bytes: number | null;
  seeders: number | null;
  leechers: number | null;
  published_at: string | null;
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

Adding a result posts its opaque `res_` id — the server resolves the stored acquisition
source, so the browser never receives, reconstructs or reposts a provider URL or magnet
(doc 09 §7, doc 05 §5.2):

```ts
await api.POST('/tasks', {
  body: { search_result_ids: [r.id], destination: defaultDestination },
  headers: { 'X-DLTOOL-CSRF': csrfToken()! },
});
```

The server half, all in the Files table:

```go
// internal/store/search.go

// GetSearchResult resolves an opaque res_ id to its stored row — including the
// server-only acquisition fields — while its search job is still live. An id whose
// job was deleted or purged, or whose row carries neither magnet_uri nor
// download_url, is indistinguishable from an unknown id: ErrNotFound.
// dl-tool is single-user (ADR-0019), so §5.2's "authorise each result through its
// own search job" means the id resolves only while its search_jobs row exists;
// there is no per-user owner column to check.
func GetSearchResult(ctx context.Context, db *sqlx.DB, id string) (SearchResultRow, error)
```

```go
// internal/api/tasks.go — CreateTasksBody gains the fourth source family of doc 05
// §5.2, beside uris, blob and the multipart file parts:

SearchResultIDs []string `json:"search_result_ids,omitempty" maxItems:"50"`
```

- Exactly one source family per request: a body mixing `search_result_ids` with `uris`,
  `blob` or file parts is `422 /problems/validation-failed`. An empty `search_result_ids`
  array counts as no family at all, so the existing empty-submission `422` applies; a
  repeated id is a duplicate `rejected[]` entry of type `/problems/conflict`, the type the
  `uris` family already uses for a duplicate in one submission. Duplicate detection runs on
  the raw ids before any resolution: a repeated occurrence is `/problems/conflict` even when
  the id's first occurrence is `/problems/not-found`.
- Per id, `GetSearchResult` resolves the row; `ErrNotFound` produces a `rejected[]` entry
  of type `/problems/not-found`. A rejected search source carries only
  `search_result_id`: `RejectedURI` gains ``SearchResultID string `json:"search_result_id,omitempty"``
  and `URI` becomes `omitempty`; the detail never contains provider data.
- A resolved `magnet_uri` (preferred) or `download_url` feeds the existing
  normalise → route → insert pipeline unchanged. The task row stores the resolved URI in
  the server-only `source_uri` and `search-result:<res_id>` in `source_display_uri`; the
  task DTO renders `source_display_uri` when it is set, so no response ever carries the
  acquisition source.
- `201` with per-id `rejected[]` entries while at least one id resolves — duplicates of a
  resolved id included; the `404` applies only when no submitted id resolves. The
  all-unavailable `404 /problems/not-found` is a problem detail body with no `rejected[]`
  member, the same shape the all-`uris`-fail `422` already returns.

```tsx
// web/src/components/AddTask/AddTaskDialog.tsx — extended signature:
export function AddTaskDialog(props: { open: boolean; onOpenChange: (o: boolean) => void;
  initialUris?: string[]; initialSearchResultIds?: string[] }): JSX.Element;
```

When `initialSearchResultIds` is non-empty the URL textarea and dropzone are replaced by one
read-only `N search results` line, the FTP-credentials block and the file-selection checkbox
stay disabled (`POST /tasks/inspect` takes no `res_` ids — see Out of scope), and `Create`
posts `search_result_ids` with the chosen destination and options.

The three zero states, each its own render: `No results` (every indexer answered with zero),
`All indexers failed` (list every `engines[].error`, offer `Retry`), `No indexers enabled` (link to
`/settings/indexers`).

## Steps
1. Add `GetSearchResult` to `internal/store/search.go`: one query joining `search_results` to its
   `search_jobs` row, returning `ErrNotFound` for an unknown id, a job that no longer exists, or a row
   with neither `magnet_uri` nor `download_url`. Create `internal/store/search_test.go` with the
   live-job, unknown-id, deleted-job and no-acquisition-URI cases.
2. Add `SourceDisplayURI *string` (`db:"source_display_uri"`) to `store.Task` in `internal/store/tasks.go`,
   include the column in the create insert and in the selects that feed the task DTO, and let
   `displaySourceURI` prefer it when set.
3. In `internal/api/tasks.go`, add `SearchResultIDs` to `CreateTasksBody`; enforce the one-family and
   empty/duplicate rules of the contract above; resolve each id, feed every resolved acquisition URI
   through the existing normalise → route → insert pipeline, store `search-result:<res_id>` as the
   display source, and return `404 /problems/not-found` when every id failed.
4. Extend `internal/api/tasks_test.go` with `humatest` cases: `TestCreateTasksSearchResultIDs` (a seeded
   result whose stored `download_url` embeds `passkey=secret` creates a task whose response `source_uri`
   is `search-result:<res_id>`, and no response field carries the passkey or URL), the per-id
   `search_result_id` rejection shape, the all-fail `404`, and the mixed-family `422`. Extend
   `internal/api/search_test.go` to assert the seeded result's serialized `GET /search/{id}` row carries
   no `download_url`, `magnet_uri` or `details_url` key.
5. Create `SearchScreen.tsx` with `useSearchJob`, built on `@tanstack/react-query` with
   `refetchInterval: (data) => (data?.finished ? false : 1000)`.
6. Render the query input, the indexer popover (checkbox list with *All* / *None*, a health dot per row and
   the last error in its tooltip, disabled indexers greyed with a link to `/settings/indexers`), and the
   category select populated from `GET /indexers/categories`.
7. Keep the indexer and category selection in `sessionStorage` under `dl.search.v1` so it survives navigation
   within the session; T064 moves it into the preference document.
8. Render the per-indexer status strip with `aria-live="polite"` and the four states `queued ○`,
   `searching ◐`, `done ● N`, `error ✕`, the error visible on hover **and** on click.
9. Create `ResultsGrid.tsx` reusing T042's TanStack Table v8 plus `@tanstack/react-virtual` setup, with
   `RESULT_COLUMN_ORDER`, `aria-rowcount` set to `total`, and `formatBytes` and `formatWhen` from T041.
10. Render every null numeric cell as `—`, and grey the seeders and leechers cells for a row whose indexer
    reported `seeders_unknown`.
11. Wire the per-row `⬇` to `onDownload(r, 'immediate')`: post `/tasks` with `search_result_ids: [r.id]`,
    flash the row, replace the button with a `✓` linking to the created task, and show one toast per
    failure naming the result title.
12. Wire the footer: the selected count and summed size (a null `size_bytes` contributes 0), and
    `Download selected ▾` with exactly two items — *Download immediately* and *Download to…*, the latter
    opening T049's add dialog through its new `initialSearchResultIds` prop; extend `AddTaskDialog` per
    the contract above. Both bulk paths submit in chunks of at most 50 ids — the body's
    `maxItems:"50"` cap. A `201` whose `rejected[]` is non-empty marks each rejected
    `search_result_id`'s row failed and fires one summary toast per chunk reporting the
    rejected count and naming the first rejected title; only resolved ids get the `✓` link.
    A chunk-level failure — transport error or any non-`201` status — marks every id that
    chunk submitted failed (the ids are known client-side), so no row is left without a
    terminal state; `TestBulkChunkPartialRejection` covers one all-fail `404` chunk alongside
    a succeeding chunk.
13. Edit `App.tsx` to route `/search` to `<SearchScreen />`, and add every string to
    `web/src/locales/en/common.json` under a `search` key; no literal user-facing text in the components.
14. Create `SearchScreen.test.tsx` with `msw` handlers: `TestPollStopsWhenFinished`,
    `TestPartialResultsRenderBeforeFinish`, `TestEngineErrorShownInStrip`, `TestThreeZeroStates`,
    `TestNullCountsRenderEmDash`, `TestRowDownloadPostsSearchResultID`,
    `TestBulkChunkPartialRejection` and `TestStopDeletesTheJob`; add the
    search-result draft case to `AddTaskDialog.test.tsx`.
15. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestPollStopsWhenFinished` asserts no further `GET /search/{id}` request is made after `finished:true`.
- [ ] `TestPartialResultsRenderBeforeFinish` asserts rows render while one indexer is still `searching`.
- [ ] `TestThreeZeroStates` asserts three distinct rendered states, each with its documented call to action.
- [ ] `TestRowDownloadPostsSearchResultID` asserts the body carries `search_result_ids` with the row's
      `res_` id plus the CSRF header, and no provider URL or magnet leaves the client.
- [ ] `TestCreateTasksSearchResultIDs` seeds a result whose `download_url` embeds `passkey=secret`, posts
      its `res_` id and asserts a task was created whose `source_uri` renders `search-result:<res_id>` —
      no response field or `rejected[]` detail carries the passkey or the URL.
- [ ] A `humatest` case in `internal/api/search_test.go` asserts the seeded result's serialized
      `GET /search/{id}` row carries no `download_url`, `magnet_uri` or `details_url` key, so the
      acquisition fields cannot re-enter the wire shape unnoticed.
- [ ] An unknown or expired `res_` id returns a `rejected[]` entry carrying `search_result_id` and no
      `uri`; a submission whose ids all fail returns `404 /problems/not-found` with a problem detail
      body and no `rejected[]` member.
- [ ] A body mixing `search_result_ids` with `uris` or `blob` returns `422 /problems/validation-failed`;
      an empty `search_result_ids` array is the existing empty-submission `422`.
- [ ] The existing all-`uris`-fail `422` is asserted alongside the new all-fail `404` in
      `internal/api/tasks_test.go`, so the shared no-`rejected[]` problem shape is pinned, not assumed.
- [ ] `aria-rowcount` equals the job's `total`, not the number of rows in the DOM.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make vet && make typecheck && make test PKG="./internal/api/... ./internal/store/..." && make test-web && echo SEARCH_UI_OK
```
Expected: `ok` for `internal/api` and `internal/store` with the step-4 cases running, Vitest reports
every test named in step 14 as passing, including the file
`web/src/components/Search/SearchScreen.test.tsx`, and the final line of stdout is exactly
`SEARCH_UI_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, plus `api/openapi.json` and
`web/src/api/schema.d.ts` — the §7.1 standing-exception paths this task regenerates with `make gen` —
and nothing else. Inspect both generated diffs: the only changes are the `search_result_ids` member on
the create-tasks body and the optional `search_result_id` on a rejected entry — `SearchResultView` is a
client-side type, not a schema member, so dropping its acquisition fields produces no generated diff
(the response schema already omits them). Use `git status`, not `git diff`: a file this task creates is
untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add saved searches, the `Save…` button or a Playwright spec; T064 owns all three.
- Do NOT add an indexer-management screen; `/settings/indexers` is M6 work and this screen only links to it.
- Do NOT hard-code a category tree in the client; it comes from `GET /indexers/categories`.
- Do NOT open a modal from the clipboard, and do NOT add optimistic task rows for a magnet result.
- Do NOT poll faster than once per second, and do NOT subscribe to SSE for search progress.
- Do NOT add `search_result_ids` to `POST /tasks/inspect`: doc 05 §5.3 lists the field but no task owns it
  — that residual gap is recorded under `## Blocked`. The file-selection path stays disabled for `res_`
  drafts.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
Resolved by the plan repair: the `## Files` table now admits the server half the record prescribed —
`internal/api/tasks.go`, `internal/store/search.go` and `internal/store/tasks.go` for the
`search_result_ids` family, its resolution and the `search-result:<res_id>` display source — plus
`web/src/components/AddTask/` so *Download to…* can pre-fill the dialog with `res_` ids. The contract
drops the acquisition fields from `SearchResultView`, posts `search_result_ids`, and
`TestRowDownloadPostsSearchResultID` asserts on them. One residual gap stays out of scope: doc 05 §5.3
also lists `search_result_ids` on `POST /tasks/inspect`, but no task owns it, so the file-selection path
is disabled for `res_` drafts. T063 remains unimplemented and still marked `todo`.

Original blocker: this task could not run as written — its interface contract contradicted the
implemented API on a fact the whole task turns on — how a search result becomes a task.

- This file's contract gives `SearchResultView` the fields `download_url`, `magnet_uri` and
  `details_url`, and its example posts `api.POST('/tasks', { body: { uris: [r.magnet_uri ??
  r.download_url!] ... } })`. Acceptance criterion `TestRowDownloadPostsMagnetOrURL` asserts the
  request body carries `magnet_uri`/`download_url`.
- The merged server never returns those fields. `SearchResultDTO` in
  `internal/api/search.go` deliberately omits all three ("server-only acquisition data"),
  matching `docs/05-api-contract.md` §9.2 and `docs/07-search-and-indexers.md` §5 rule 6
  (added in that file's 2026-09-01 change-log entry: Torznab download URLs embed the
  operator's per-user tracker passkey, so acquisition URLs are server-only). `GET /search/{id}`
  returns opaque `res_` ids and metadata only.
- The documented replacement — `POST /tasks` accepting `search_result_ids` (doc 05 §5.2 and
  §9.2; the `search-result:<res_id>` rendering already anticipated by
  `tasks.source_display_uri` and `InspectTasksOutputBody.source_uri`) — is specified but not
  implemented: `CreateTasksBody` in `internal/api/tasks.go` has no `search_result_ids` field,
  `api/openapi.json` and `web/src/api/schema.d.ts` have none, and no `res_` id resolves through
  `uris` (`normaliseSubmission` rejects it). No task file owns adding it.

Evidence for the absence claims above (run from the repo root at `aed413f`):

```
$ grep -rn "search_result_ids" internal/api/tasks.go api/openapi.json web/src/api/schema.d.ts
(exit 1 — no matches)
$ grep -rn "res_" internal/api/tasks.go
(exit 1 — no matches; every uris entry goes through normaliseSubmission → uri.Normalize,
internal/api/tasks.go:983-994, so a res_ id is an unsupported-scheme rejection)
$ grep -n "download_url\|magnet_uri\|details_url" internal/api/search.go
272:// metadata and the opaque res_ id only: download_url, magnet_uri and
273:// details_url are server-only acquisition data and have no field here
```

Either direction fails a hard rule. Implementing this file's contract ships a dead `⬇`:
the generated client type has no `magnet_uri`/`download_url`, so every click posts
`uris:[null]` and the server 422s; the named test would only pass against a mock response the
real API can never produce. Implementing the docs' contract requires server work — a
`search_result_ids` member on `CreateTasksBody`, resolution through `search_results`, and
regenerated `api/openapi.json` + `web/src/api/schema.d.ts` — all outside this task's Files
table.

Which file should answer it: `docs/05-api-contract.md` §5.2/§9.2 already specify the
`search_result_ids` flow, so the plan needs a task (or an amended T063) covering the server
half in `internal/api/tasks.go` plus `make gen`, sequenced before or merged into this task;
then this file's contract should be corrected to drop the acquisition fields from
`SearchResultView` and to post `search_result_ids`, and `TestRowDownloadPostsMagnetOrURL`
rewritten to assert on `res_` ids and the CSRF header.
