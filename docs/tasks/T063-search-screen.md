# T063 — Build the search screen

| Field | Value |
|---|---|
| **ID** | T063 |
| **Milestone** | M4 |
| **Status** | done |
| **Depends on** | T014, T041, T042, T044, T061, T062 |
| **Blocks** | T064 |
| **Parallel-safe** | no — replaces the `/search` placeholder in T040's `App.tsx` |
| **Implements** | [FR-058](../02-requirements.md#fr-058-create-a-task-from-a-search-result-in-one-click) (client and server halves; the "server half is T020" attribution predates `res_` ids — T020's merged contract has no `search_result_ids` member — so this task's Files table covers both) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md) |
| **Est. size** | 4 new files, 12 modified, ~1,050 LOC |

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
| `internal/store/models.go` | modify | `SourceDisplayURI *string` (`db:"source_display_uri"`) on `Task` — the struct is defined here, not in `tasks.go`. |
| `internal/store/search.go` | modify | `GetSearchResult` — resolve a `res_` id to its row, acquisition fields included, while its job is live. |
| `internal/store/search_test.go` | create | `GetSearchResult` cases: live job resolves; unknown id, a deleted job and a row with neither `magnet_uri` nor `download_url` are all `ErrNotFound`. |
| `internal/store/tasks.go` | modify | The create insert's `source_display_uri` column and `queryGetTask` selecting it — the get-side read that feeds the task DTO. |
| `internal/store/tasks_list.go` | modify | `queryListTasksPage` selects `source_display_uri` — it feeds `GET /tasks` and, through `SSEHandlers.Snapshot` → `sync.Project`, the SSE snapshot. |
| `internal/api/tasks.go` | modify | `search_result_ids` on `CreateTasksBody`, the one-source-family rule, resolution and the `search-result:<res_id>` display source. |
| `internal/api/tasks_test.go` | modify | `humatest` cases for the search-result family and its rejections, plus the REST-side pin that the task DTO follows the shared `DisplaySourceURI` rule. |
| `internal/api/search_test.go` | modify | Asserts a seeded result's serialized `GET /search/{id}` row carries no acquisition key. |
| `internal/sync/delta.go` | modify | `DisplaySourceURI` — the shared helper `Project` and the REST DTO both call: `SourceDisplayURI` wins; the nil-or-empty fallback strips userinfo and, for non-`magnet:` schemes, `RawQuery`, while a `magnet:` keeps only a `urn:` `xt` and drops out without one — fail-closed for rows that predate the column. |
| `internal/sync/delta_test.go` | modify | `DisplaySourceURI` pins: a set `source_display_uri` renders verbatim; NULL and empty fall back to `source_uri` with userinfo and query string stripped, a `magnet:` source keeps its `urn` `xt` values only, and one without a usable `xt` drops out entirely. |
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
  acquisition source. That rendering must hold on every path that emits a task —
  `GET /tasks`, `GET /tasks/{id}`, the PATCH response and the SSE snapshot/deltas —
  so `queryListTasksPage` selects the column too and `sync.Project` prefers the field.
- Both renderers route through one shared helper — `DisplaySourceURI`, exported from
  `internal/sync/delta.go` and called by `Project` and by the REST DTO in
  `internal/api/tasks.go` (the api package already imports `internal/sync` for
  `Project`) — never two copies of a security-sensitive rule that can drift. The rule:
  `source_display_uri` set wins; a NULL or empty column falls back to `source_uri`
  sanitized fail-closed. The sanitizer strips userinfo always, drops a source that
  cannot be parsed or whose credentials ride in opaque form (`user:pass@host` —
  today's sync renderer already nils `u.Opaque != ""`, the REST one does not and the
  shared helper keeps the stricter behaviour), and strips `RawQuery` for every scheme
  except `magnet:` — a magnet's query is untrusted (`tr`, `xs` and `ws` values
  commonly embed tracker passkeys and indexer API keys), yet stripping it whole
  would render a bare, useless `magnet:`, so there it keeps every `xt`
  parameter whose value parses with URI scheme `urn` — compared
  case-insensitively, so `URN:btih:` survives — and carrying no `?` or `#` of
  its own: an RFC 8141 q-/f-component is not part of the content identity, so
  such an `xt` is dropped whole. Every `xt` in the magnet is evaluated, not
  just the first; a non-`urn` `xt` is discarded with `dn`, `tr`, `xs` and the
  rest, and a magnet left with no surviving `xt` is dropped like an unparsable
  source. Rows written before this column existed have it NULL and a stored
  `source_uri` may carry a secret in its query string (a Torznab `passkey` rides
  there, not in userinfo), so the fallback is deliberately fail-closed — every
  pre-existing row's emitted non-magnet URI loses its query string, benign or not.
  No backfill: the column is written at insert only.
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
2. Add `SourceDisplayURI *string` (`db:"source_display_uri"`) to `store.Task` in `internal/store/models.go`,
   include the column in the create insert and in both selects that feed the task DTO —
   `queryGetTask` (`internal/store/tasks.go`) and `queryListTasksPage`
   (`internal/store/tasks_list.go`, which also feeds the SSE snapshot via
   `SSEHandlers.Snapshot` → `sync.Project`) — and let both `displaySourceURI` renderers
   prefer it when set. The preference and the nil-or-empty fallback live in one shared
   `DisplaySourceURI` helper in `internal/sync/delta.go` that `Project` and the REST
   renderer in `internal/api/tasks.go` both call — the rule is the contract's,
   including the `magnet:` `xt` carve-out. `internal/sync/delta_test.go` pins all
   six cases — set, NULL, empty, a magnet keeping every `urn` `xt` it carries
   (scheme matched case-insensitively, so an `URN:btih:` `xt` survives, and a
   second `urn` `xt` survives alongside), a magnet with both a `urn` and a
   non-`urn` `xt` keeping just the `urn` one, and a magnet with no usable `xt`
   (absent, non-`urn` or q-component-carrying) dropping out — beside the
   existing `TestProjectDropsUnsanitizableSources`.
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
    chunk submitted failed (the ids are known client-side) and fires one error toast per
    chunk; a failed row stays selectable so it can be resubmitted — a transport error's
    outcome is unknown and a `5xx` or `429` may succeed on retry. A resubmit of a result the
    server already committed is detected by the pipeline and rejected with
    `/problems/conflict` rather than silently creating a second task; a `rejected[]` entry of
    that type is terminal success, not failure — the row gets the `✓`, is excluded from the
    rejection summary toast, and is no longer selectable — so no row is left without a
    terminal state. `TestBulkChunkPartialRejection` covers one all-fail `404` chunk alongside
    a succeeding chunk and a `201` with a non-empty `rejected[]`, asserting the error toast
    for the all-fail chunk, the summary toast for the `rejected[]` chunk (conflict entries
    excluded; no rejection toast at all when every entry is a conflict), that only resolved
    ids get the `✓` link, and that an id that answered `/problems/conflict` ends resolved
    and unselectable.
13. Edit `App.tsx` to route `/search` to `<SearchScreen />`, and add every string to
    `web/src/locales/en/common.json` under a `search` key; no literal user-facing text in the components.
14. Create `SearchScreen.test.tsx` with `msw` handlers: `TestPollStopsWhenFinished`,
    `TestPartialResultsRenderBeforeFinish`, `TestEngineErrorShownInStrip`, `TestThreeZeroStates`,
    `TestNullCountsRenderEmDash`, `TestRowDownloadPostsSearchResultID`,
    `TestBulkChunkPartialRejection` and `TestStopDeletesTheJob`; add the
    search-result draft case to `AddTaskDialog.test.tsx`.
15. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [x] `TestPollStopsWhenFinished` asserts no further `GET /search/{id}` request is made after `finished:true`.
- [x] `TestPartialResultsRenderBeforeFinish` asserts rows render while one indexer is still `searching`.
- [x] `TestThreeZeroStates` asserts three distinct rendered states, each with its documented call to action.
- [x] `TestRowDownloadPostsSearchResultID` asserts the body carries `search_result_ids` with the row's
      `res_` id plus the CSRF header, and no provider URL or magnet leaves the client.
- [x] `TestCreateTasksSearchResultIDs` seeds a result whose `download_url` embeds `passkey=secret`, posts
      its `res_` id and asserts a task was created whose `source_uri` renders `search-result:<res_id>` —
      no response field or `rejected[]` detail carries the passkey or the URL.
- [x] A `humatest` case in `internal/api/search_test.go` asserts the seeded result's serialized
      `GET /search/{id}` row carries no `download_url`, `magnet_uri` or `details_url` key, so the
      acquisition fields cannot re-enter the wire shape unnoticed.
- [x] An unknown or expired `res_` id returns a `rejected[]` entry carrying `search_result_id` and no
      `uri`; a submission whose ids all fail returns `404 /problems/not-found` with a problem detail
      body and no `rejected[]` member.
- [x] A body mixing `search_result_ids` with `uris` or `blob` returns `422 /problems/validation-failed`;
      an empty `search_result_ids` array is the existing empty-submission `422`.
- [x] The existing all-`uris`-fail `422` is asserted alongside the new all-fail `404` in
      `internal/api/tasks_test.go`, so the shared no-`rejected[]` problem shape is pinned, not assumed.
- [x] `aria-rowcount` equals the job's `total`, not the number of rows in the DOM.
- [x] A case in `internal/sync/delta_test.go` asserts `Project` renders `source_display_uri`
      verbatim when set, a NULL or empty column falls back to `source_uri` with userinfo
      and query string stripped, a `magnet:` source keeps all of its `urn` `xt`
      parameters (a sibling non-`urn` `xt` is dropped; the `urn` prefix within
      each `xt` is matched case-insensitively), and a magnet with no usable `xt`
      drops out — so neither the SSE snapshot nor a delta can emit a
      query-string secret for any row (the contract treats every `urn` `xt`
      value as non-secret), including ones written before the column existed.
- [x] No task-emitting path serialises `source_uri` directly — the PATCH response and
      every SSE delta are built through `queryGetTask`/`queryListTasksPage` and rendered
      by the shared `DisplaySourceURI` helper; verified by grep for other task-row
      selectors and DTO constructors before implementation begins.
- [x] A `humatest` case in `internal/api/tasks_test.go` pins the REST renderer to the
      shared rule: a task row with NULL `source_display_uri` and a `magnet:`
      `source_uri` serialises as `magnet:?xt=urn:…` (the `urn:` `xt` only) in the
      `GET /tasks` body, and a row whose `source_uri` parses with `u.Opaque != ""`
      (e.g. `user:pass@host`) omits the display source — so a re-duplicated renderer
      in `internal/api/tasks.go` fails the suite, not a grep.

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

Run on the task-branch head (re-run after the review-fix commit — the
counts grew by the new cases it adds):

```
$ make lint && make vet && make typecheck && make test PKG="./internal/api/... ./internal/store/..." && make test-web && echo SEARCH_UI_OK
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint
> lint
> eslint .
cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
go vet ./...
cd web && npx tsc --noEmit -p tsconfig.json
go test -race -count=1 ./internal/api/... ./internal/store/...
ok  	github.com/L-K-M/dl-tool/internal/api	127.664s
ok  	github.com/L-K-M/dl-tool/internal/store	78.015s
cd web && npx vitest run
 ✓ src/components/AddTask/AddTaskDialog.test.tsx (14 tests)
 ✓ src/components/Search/SearchScreen.test.tsx (9 tests)
 Test Files  18 passed (18)
      Tests  234 passed (234)
SEARCH_UI_OK
```

The step-14 Vitest cases and the step-4 Go cases, run with the verbose reporters:

```
 ✓ src/components/Search/SearchScreen.test.tsx > TestPollStopsWhenFinished
 ✓ src/components/Search/SearchScreen.test.tsx > TestPartialResultsRenderBeforeFinish
 ✓ src/components/Search/SearchScreen.test.tsx > TestEngineErrorShownInStrip
 ✓ src/components/Search/SearchScreen.test.tsx > TestThreeZeroStates
 ✓ src/components/Search/SearchScreen.test.tsx > TestNullCountsRenderEmDash
 ✓ src/components/Search/SearchScreen.test.tsx > TestRowDownloadPostsSearchResultID
 ✓ src/components/Search/SearchScreen.test.tsx > TestBulkChunkPartialRejection
 ✓ src/components/Search/SearchScreen.test.tsx > TestStopDeletesTheJob
 ✓ src/components/AddTask/AddTaskDialog.test.tsx > TestSearchResultDraftPostsIDs
 ✓ src/components/AddTask/AddTaskDialog.test.tsx > TestSearchResultDraftChunksAt50
--- PASS: TestPollResultOmitsAcquisitionKeys (0.08s)
--- PASS: TestCreateTasksSearchResultIDs (0.04s)
--- PASS: TestTaskCreateRejectsGoneResult (0.03s)
--- PASS: TestTaskCreateMixedFamilies422 (0.07s)
--- PASS: TestTaskCreateResultConflicts (0.03s)
--- PASS: TestTaskCreateResultUnroutableSource (0.03s)
--- PASS: TestTaskDisplaySourceRendersSharedRule (0.39s)
ok  	github.com/L-K-M/dl-tool/internal/api	4.055s
--- PASS: TestGetSearchResultResolvesLiveRow (0.42s)
--- PASS: TestGetSearchResultGoneJobIsNotFound (0.40s)
--- PASS: TestGetSearchResultRequiresAcquisitionSource (0.40s)
ok  	github.com/L-K-M/dl-tool/internal/store	2.275s
--- PASS: TestProjectDropsUnsanitizableSources (0.00s)
--- PASS: TestProjectDisplaySourceURIWins (0.00s)
ok  	github.com/L-K-M/dl-tool/internal/sync	1.054s
```

The review-fix commits also add `TestEnterKeyDoesNotStartSecondJob` (the
Enter-during-search guard, including the in-flight-POST window) and
`TestTaskCreateTooManySearchResults` (over-cap multipart payload), and
strengthen `TestStopDeletesTheJob` (poll count stable after DELETE),
`TestEngineErrorShownInStrip` (focus and hover each open the tooltip),
`TestTaskCreateResultConflicts` (an isolated within-submission
duplicate), `TestTaskDisplaySourceRendersSharedRule` (authority-form
credentials), `TestProjectDropsUnsanitizableSources` (non-magnet fragment
strip) and `TestTaskCreateMixedFamilies422` (multipart file and junk
parts).

The existing all-`uris`-fail `422` shares the no-`rejected[]` problem shape with the new
all-fail `404`: `TestCreateTasksDuplicateTorrent` (tasks_test.go) pins the `uris` side and
`TestTaskCreateRejectsGoneResult` the `search_result_ids` side.

Scope (`git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort`):

```
internal/api/search_test.go
internal/api/tasks_test.go
web/src/App.tsx
web/src/components/AddTask/AddTaskDialog.test.tsx
web/src/components/AddTask/AddTaskDialog.tsx
web/src/components/Search/ResultsGrid.tsx
web/src/components/Search/SearchScreen.test.tsx
web/src/components/Search/SearchScreen.tsx
web/src/locales/en/common.json
```

plus, already committed on this branch, the Files-table backend set
(`internal/store/models.go`, `internal/store/search.go`, `internal/store/search_test.go`,
`internal/store/tasks.go`, `internal/store/tasks_list.go`, `internal/api/tasks.go`,
`internal/sync/delta.go`, `internal/sync/delta_test.go`) and the §7.1 regenerated pair
`api/openapi.json` and `web/src/api/schema.d.ts`. Their generated diffs carry only the
`search_result_ids` member on the create-tasks body and `search_result_id` plus the
`omitempty` `uri` on a rejected entry.

## Blocked
Resolved by the follow-up plan repair to #214: the `## Files` table now lists the four rows the
blocker named — `internal/store/models.go` for the `SourceDisplayURI` field on `Task`,
`internal/store/tasks_list.go` for `queryListTasksPage` selecting the column,
`internal/sync/delta.go` for `Project` preferring it, and `internal/sync/delta_test.go` for the
set/NULL/empty pins — and the `internal/store/tasks.go` row's purpose no longer claims the field
lives there. The contract prescribes the sanitizing fallback the blocker required: on nil-or-empty
`source_display_uri`, both `displaySourceURI` renderers strip `RawQuery` as well as userinfo, so
pre-existing rows fail closed and no migration or backfill is needed. T063 remains unimplemented
and still marked `todo`.

Stopped before implementing (2026-09-18, second blocker): the repaired `## Files` table still
does not admit the files the prescribed `source_display_uri` change lives in. Step 2 and the
store row's purpose presume `internal/store/tasks.go` holds both the `Task` struct and every
read that feeds the task DTO; it holds neither in full.

Three required files are missing from the table, and a fourth is the natural pin:

1. `internal/store/models.go` — `Task` is defined there (models.go:47), not in `tasks.go`.
   `SourceDisplayURI *string` (`db:"source_display_uri"`) cannot be added to `store.Task`
   from a permitted file; Go requires the field inside the struct's definition block. The
   plan already treats the file as distinct: T017's Files table lists
   `internal/store/models.go` for "Add the `Task` and `TaskEvent` row structs", and the T022
   and T050 task files both call out "models.go is outside this task's Files table".
2. `internal/store/tasks_list.go` — `queryListTasksPage` (tasks_list.go:115) is a read that
   feeds the task DTO twice over: `GET /tasks` directly, and `GET /events` through
   `SSEHandlers.Snapshot` (`internal/api/sse.go`:151-160, `ListTasks` → `sync.Project`).
   Without the column the list path falls back to userinfo-stripping the stored
   `source_uri` — which for a search-result task is the provider `download_url`, passkey
   and all — contradicting the contract line this file quotes: "no response ever carries
   the acquisition source". `queryGetTask` (tasks.go:123) is the only DTO-feeding read the
   table admits.
3. `internal/sync/delta.go` — `Project`'s `displaySourceURI` (delta.go:83-95) renders
   `t.SourceURI` with only userinfo stripped. A passkey rides in the query string, not
   userinfo, so unless the projection prefers `SourceDisplayURI` every `sync` delta leaks
   the acquisition URL to every connected client.
4. `internal/sync/delta_test.go` — the pin for (3): a row carrying `source_display_uri`
   projects it instead of the stripped `source_uri`. `TestProjectDropsUnsanitizableSources`
   is the sibling precedent.

Not required: `internal/store/tasks_infohash.go` — `queryFindTaskByInfohash` scans into
`store.Task` but never feeds a DTO or snapshot, and sqlx tolerates a struct field with no
matching column, so the select needs no change.

Evidence (run from the repo root at `a852ca1`, verbatim):

```
$ grep -n "^type Task struct" internal/store/*.go
internal/store/models.go:47:type Task struct {
$ grep -rn "source_display_uri" internal/
internal/store/migrations/00001_init.sql:79:  source_display_uri TEXT,               -- API-safe; search-result:<res_id> for a grabbed result
$ grep -rn "SELECT id, engine, engine_ref, source_kind, source_uri" internal/store/*.go
internal/store/tasks.go:123:	queryGetTask = `SELECT id, engine, engine_ref, source_kind, source_uri, name, infohash_v1, infohash_v2,
internal/store/tasks_infohash.go:80:	queryFindTaskByInfohash = `SELECT id, engine, engine_ref, source_kind, source_uri, name, infohash_v1, infohash_v2,
internal/store/tasks_list.go:115:const queryListTasksPage = `SELECT id, engine, engine_ref, source_kind, source_uri, name, infohash_v1, infohash_v2,
$ grep -n "ListTasks\|sync.Project" internal/api/sse.go
39:	// snapshotPageSize is the largest page TaskStore.ListTasks accepts; the
154:		rows, cursor, _, err := h.tasks.ListTasks(ctx, filter)
159:			snap[row.ID] = sync.Project(row)
$ grep -n "t.SourceURI\|func displaySourceURI" internal/sync/delta.go
83:func displaySourceURI(t store.Task) any {
84:	if t.SourceURI == nil {
88:	u, err := url.Parse(*t.SourceURI)
```

Reading the hits: the `source_display_uri` match is the DDL alone — no Go code references the
column. Of the three full-row selects, `queryGetTask` is in the Files table,
`queryFindTaskByInfohash` never feeds a DTO, and `queryListTasksPage` feeds `GET /tasks`
and the SSE snapshot.

Note that the delta.go leak the third item describes is not contingent on this task:
`displaySourceURI` clears `u.User` and returns `u.String()` with `RawQuery` intact
(delta.go:92-94), so any stored `source_uri` carrying a query-string secret is already
broadcast over `GET /events` today. And since nothing writes the column yet, every
existing row has `source_display_uri` NULL: the repair's `delta.go` row closes that
existing hole only if its prescription reads "prefer `SourceDisplayURI`, and when it is
NULL strip `RawQuery` as well as userinfo from `source_uri`" — a sanitizing fallback, not
a backfill, so no migration row is needed. The accepted cost: every pre-existing row's
emitted URI loses its query string (benign or not) — deliberate fail-closed behavior.
The column is written at insert only, so pre-existing rows regain benign parameters
solely through a later backfill; nothing else populates their column.

Which file should answer it: this file's `## Files` table — the repair needs four rows
(`internal/store/models.go` for the `SourceDisplayURI` field on `Task`,
`internal/store/tasks_list.go` for `queryListTasksPage` selecting the column — scanned
into the pinned `*string` field, so NULL arrives as nil and needs no `COALESCE`; nil or
empty selects the sanitized `source_uri` fallback,
`internal/sync/delta.go` for `Project` preferring it and clearing `RawQuery` on the
nil-or-empty fallback, and `internal/sync/delta_test.go` for pins covering NULL, empty
and set values), and the
`internal/store/tasks.go` row's purpose should drop the claim that the field lives there.
No compliant implementation exists without them: the scope check of the Verification
block would list all four outside the table.

---

Resolved by the earlier plan repair (#212/#213): the `## Files` table now admits the server half the record prescribed —
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
