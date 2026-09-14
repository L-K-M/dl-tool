# T042 — Render the virtualised task grid and the mobile card list

| Field | Value |
|---|---|
| **ID** | T042 |
| **Milestone** | M3 |
| **Status** | done |
| **Depends on** | T021, T040, T041 |
| **Blocks** | T043, T044, T045, T048, T063, T104 |
| **Parallel-safe** | no — it also edits the shared file `web/src/App.tsx` |
| **Implements** | — (renders [FR-012](../02-requirements.md#fr-012-list-and-filter-tasks), covered by T021; the performance and accessibility gates are T043 and T104) |
| **Decisions** | [ADR-0006](../decisions/0006-sse-with-rid-deltas.md), [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md) |
| **Est. size** | 4 new files, ~420 LOC. The card list ships here because it renders the same rows through the same virtualiser with a different row renderer. |

## Goal
The task route renders every row the server's filter returns, virtualised at a fixed row height, with the
fifteen default columns, the status renderer and the progress bar of doc 09 §3. Below 640 px the same rows
render as cards. The grid reports `aria-rowcount` as the **total** row count.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/09-web-ui-spec.md` §3.1 Default columns](../09-web-ui-spec.md#31-default-columns) — id, header,
   width, alignment, sort type, renderer and source field for every column.
2. [`docs/09-web-ui-spec.md` §3.2 Status renderer](../09-web-ui-spec.md#32-status-renderer) — the ordinal,
   label, token and icon per state.
3. [`docs/09-web-ui-spec.md` §3.5 Selection](../09-web-ui-spec.md#35-selection),
   [§3.6 Keyboard](../09-web-ui-spec.md#36-keyboard), [§3.8 Progress bar](../09-web-ui-spec.md#38-progress-bar),
   [§3.9 Virtualisation](../09-web-ui-spec.md#39-virtualisation).
4. [`docs/09-web-ui-spec.md` §10.3 Responsive and mobile](../09-web-ui-spec.md#103-responsive-and-mobile)
   and [§10.4 Accessibility](../09-web-ui-spec.md#104-accessibility).
5. [`docs/05-api-contract.md` §5.1 `GET /tasks`](../05-api-contract.md#51-get-tasks) — query parameters,
   the page envelope and `total`.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/TaskGrid/TaskGrid.tsx` | create | Column definitions, the virtualised grid and its keyboard model. |
| `web/src/components/TaskGrid/TaskCardList.tsx` | create | The mobile row renderer over the same virtualiser. |
| `web/src/components/TaskGrid/TaskGrid.test.tsx` | create | Rendering, sorting, selection and `aria-rowcount`. |
| `web/src/locales/en/grid.json` | create | Column headers, status labels and grid empty states. |
| `web/src/App.tsx` | edit | Render `TasksRoute` through `TaskGrid` instead of the placeholder. |
| `web/src/main.test.ts` | edit | Replace placeholder-only proof with entrypoint-to-grid proof under the base path. |
| `web/src/App.test.tsx` | edit | Mock task pages and verify route-to-grid requests while retaining auth and layout coverage. |

No other file may be modified.

## Interface contract

```tsx
// web/src/components/TaskGrid/TaskGrid.tsx
import type { ColumnDef } from '@tanstack/react-table';
import type { SidebarFilter, Task } from '../../store/useTasks';

export interface TaskGridProps {
  filter: SidebarFilter;
  category?: string;
  tag?: string;
}

/** Ordered ids for the current filter, resolved server-side by GET /tasks?state=.
 *  Follows next_cursor with limit=500 until it is null and returns { ids, total }. */
export function useTaskIds(p: TaskGridProps): { ids: string[]; total: number; isLoading: boolean };

/** T051 calls this after any delta that changes state, inserts or removes a task. */
export function invalidateTaskList(qc: import('@tanstack/react-query').QueryClient): Promise<void>;

/** Column ids, in default order. Sources are doc 09 §3.1. */
export const DEFAULT_COLUMN_ORDER = [
  'select','queuePos','name','size','progress','status','dlSpeed','ulSpeed','eta',
  'peers','ratio','uploaded','destination','addedOn','completedOn',
] as const;

/** Sort ordinal of doc 09 §3.2; `status` sorts by this, never alphabetically. */
export const STATUS_ORDINAL: Record<Task['state'], number> = {
  downloading: 0, seeding: 1, checking: 2, extracting: 3, moving: 4,
  queued: 5, paused: 6, completed: 7, error: 8, removed: 9,
};

export const columns: ColumnDef<Task>[];
export function TaskGrid(props: TaskGridProps): JSX.Element;
```

```tsx
// web/src/components/TaskGrid/TaskCardList.tsx
export function TaskCardList(props: { ids: string[]; total: number }): JSX.Element;
```

Virtualiser and roles, verbatim:

```ts
useVirtualizer({ count: rows.length, getScrollElement, estimateSize: () => rowHeight, overscan: 10 })
```

```html
<div role="grid" aria-rowcount={total} aria-colcount={visibleColumnCount} aria-multiselectable="true">
  <div role="row" aria-rowindex="1"> <span role="columnheader" aria-colindex="1" aria-sort="none"> … 
  <div role="row" aria-rowindex={index + 2} aria-selected={selected} tabIndex={focused ? 0 : -1}>
    <span role="gridcell" aria-colindex={c}> …
```

Row heights follow [doc 09 §3.9](../09-web-ui-spec.md#39-virtualisation): table density applies only
at tablet/desktop breakpoints; mobile cards use their own fixed height in either density. Never measure
rows. Keep the rendered height and virtualiser estimate equal when the breakpoint or density changes,
including the cache reset required by doc 09 §3.9.
Progress bar attributes: `role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow={pct}
aria-valuetext="78% — 4.1 GB of 5.2 GB"`.

## Steps
1. Create `web/src/locales/en/grid.json` with the fifteen headers, the ten status labels and the two grid
   empty-state sentences from doc 09 §10.5.
2. Create `TaskGrid.tsx`. Define `columns` for the fifteen ids in `DEFAULT_COLUMN_ORDER` with the widths and
   alignments of doc 09 §3.1, every cell rendered through the T041 formatters, and `—` wherever the source
   is null or zero.
3. Implement `useTaskIds` with `@tanstack/react-query`, keyed by `['tasks', filter, category, tag]`, paging
   `GET /tasks` through `next_cursor`; keep `total` from the first page.
4. Read each row's live fields from `useTasks` by id, so a delta re-renders one row and not the array.
5. Wire `@tanstack/react-table` with `getCoreRowModel` and `getSortedRowModel`, `columnResizeMode:
   'onChange'`, and one `@tanstack/react-virtual` virtualiser over the sorted rows. Exactly one element has
   `overflow: auto`; the header row is sticky and translated by `-scrollLeft`.
6. Implement selection from doc 09 §3.5 and this task's keyboard subset under
   [doc 09 §3.6 Implementation ownership](../09-web-ui-spec.md#36-keyboard), writing selection into the
   T041 store. One `keydown` listener returns immediately when the event target is `INPUT`, `TEXTAREA`
   or `isContentEditable`. Exactly one row carries `tabIndex={0}`. Leave cross-component shortcuts to
   their named owners; do not install dead handlers or suppress keys for unavailable targets.
7. Create `TaskCardList.tsx`: name clamped to two lines, the progress bar, then
   `Status · Size · ↓rate · ↑rate · ETA`, tap targets at least 44 px, rendered below 640 px.
   Use the fixed mobile height from doc 09 §3.9 and the content budget and overflow rules in §10.3.
8. Edit `web/src/App.tsx` so `TasksRoute` reads `:filter`, `:name` and renders `TaskGrid`.
   Update `main.test.ts` and `App.test.tsx` with deterministic MSW task-page handlers; retain strict
   unhandled-request errors. Replace the entrypoint's app-wide placeholder-text assertion with
   `TestEntrypointRendersTaskGridUnderBase`: boot the real entrypoint under `/dl-tool/`, await a task
   row from the mocked API, and preserve root mounting, containment and single auth-boot assertions.
   Add `TestTaskRoutesRequestServerFilters` for the default, state, category and tag routes: assert
   base-prefixed task requests and the query parameters from doc 05 §5.1, then await the returned row.
   Preserve existing authentication, CSRF, layout, redirect and route assertions; do not stub TaskGrid
   or hide task requests to keep placeholder tests passing.
9. Create `TaskGrid.test.tsx` with `msw` serving a `GET /tasks` page and the store seeded through
   `hydrate`: assert the fifteen headers, a formatted size cell, status sorting by `STATUS_ORDINAL`,
   `Shift+click` range selection, and `aria-rowcount` equal to the reported `total` while a 10 000-row
   store renders far fewer `role="row"` elements. Add `TestRowHeightTracksLayoutAndDensity` to check both
   densities on each side of the mobile breakpoint: rendered heights and virtualiser estimates follow
   doc 09 §3.9, including cached offsets and total virtual height after a breakpoint change, with row
   order and selection preserved. Cover all five metadata items at narrow mobile widths and assert the
   §10.3 budget, non-wrapping items, and touch-target styles. Add
   `TestGridKeyboardNavigationAndSelection` covering this task's entire §3.6 subset, roving focus across
   virtualised rows, the editable-target guard and no interception of later-owned shortcuts.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [x] `TestEntrypointRendersTaskGridUnderBase` and `TestTaskRoutesRequestServerFilters` pass in the
  existing entrypoint and App suites, preserving their root, auth, CSRF, layout and routing guarantees.
- [x] `TestRendersDefaultColumns`, `TestStatusSortsByOrdinal`, `TestShiftClickSelectsRange` pass.
- [x] `TestAriaRowcountIsTotalNotDomRows` passes with 10 000 tasks in the store.
- [x] `TestGridKeyboardNavigationAndSelection` proves this task's
  [keyboard subset](../09-web-ui-spec.md#36-keyboard), including the editable-target guard and leaving
  later-owned shortcuts unintercepted.
- [x] `TestRowHeightTracksLayoutAndDensity` passes; mobile cards retain the required content and tap
  targets without overlapping adjacent cards.
- [x] Every cell whose source is null or zero renders `—`, and `eta_seconds: null` renders `∞`.
- [x] The page body never scrolls horizontally; only the grid's own scroll container does.
- [x] No column is added that doc 09 §3.1 defers to v2, and no cell renders a placeholder value.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo GRID_OK
```
Expected: Vitest reports `Test Files  7 passed (7)` including
`src/components/TaskGrid/TaskGrid.test.tsx`, every test named above appears as passing, and the final line
of stdout is exactly `GRID_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT build the `Columns ▾` popover, column resizing, reordering or persistence; T045 owns them.
- Do NOT build the toolbar, the sidebar or the status bar; T044 owns them.
- Do NOT build the context menu or the detail pane; T045 and T048 own them.
- Do NOT add column virtualisation; doc 09 §3.9 forbids it at this column count.
- Do NOT recompute the filter sets client-side; the row set comes from `GET /tasks?state=`.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

`npm ci --prefix web` installed the pinned dependencies. npm reported two existing high-severity
findings; no pins changed. Density is a controlled grid input; preference persistence remains outside
this task. The larger diff covers the required renderers, keyboard model and integration tests only.

### Acceptance proof

The named tests below cover the acceptance criteria. `TestMissingCellsAndErrorDetails` covers missing
values and infinity; `TestOnlyGridScrollsHorizontally` covers scroll ownership and pinned cells.
`TestRendersDefaultColumns` asserts the exact column set and real formatted values.
The existing App assertions retain authentication, CSRF, layout and routing coverage.

Supplemental command: `cd web && npx vitest run --reporter=verbose` (exit 0), excerpts:

```text
✓ src/main.test.ts > TestEntrypointRendersTaskGridUnderBase 575ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestRendersDefaultColumns 226ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestStatusSortsByOrdinal 326ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestShiftClickSelectsRange 151ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestRowCheckboxTogglesWithoutClearingOthers mobile=false 128ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestRowCheckboxTogglesWithoutClearingOthers mobile=true 63ms
 ✓ src/App.test.tsx > TestTaskRoutesRequestServerFilters / 82ms
 ✓ src/App.test.tsx > TestTaskRoutesRequestServerFilters /tasks/downloading 56ms
 ✓ src/App.test.tsx > TestTaskRoutesRequestServerFilters /tasks/category/Linux 68ms
 ✓ src/App.test.tsx > TestTaskRoutesRequestServerFilters /tasks/tag/archive 66ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestShrinkingPageKeepsVirtualIndicesInBounds 722ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestAriaRowcountIsTotalNotDomRows 564ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestGridKeyboardNavigationAndSelection 1521ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestDensityIsControlledNotCached 58ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestRowHeightTracksLayoutAndDensity 787ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestMobileContentBudgetAndTargets 320 20ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestMobileContentBudgetAndTargets 375 20ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestMobileContentBudgetAndTargets 639 15ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestMissingCellsAndErrorDetails 38ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestLiveCellsAndSortInvalidation 119ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestSecondarySortTracksLiveChanges 81ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestSortReadsUnsortedLiveChanges 71ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestTimestampSortUsesInstants 51ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestGridOwnsHeaderAndRows 58ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestPageFailureKeepsGridAndRetries 62ms
 ✓ src/components/TaskGrid/TaskGrid.test.tsx > TestOnlyGridScrollsHorizontally 63ms
```

### Verification

`make lint && make typecheck && make test-web && echo GRID_OK` exited 0:

```text
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


 Test Files  7 passed (7)
      Tests  114 passed (114)
   Start at  05:11:31
   Duration  6.00s (transform 713ms, setup 0ms, import 3.40s, tests 8.38s, environment 2.05s)

GRID_OK
```

Expected auth-error responses and the deliberately failed task-page network request are omitted above.
The retry regression first failed with an unhandled query error, then passed with a retained grid and
working Retry button. Timestamp sorting, header ownership, density ownership and mobile line-height
regressions also failed before their fixes and passed afterward.
`TestSortReadsUnsortedLiveChanges` and `TestSecondarySortTracksLiveChanges` then reproduced stale
sort snapshots; refreshing on sort changes and watching every active key fixed both.

### Full gate

`PATH="/tmp/t039-tools:$PATH" make ci` exited 0, reusing the existing Docker CLI for compose validation.
Output excerpts:

```text
0 issues.
All matched files use Prettier code style!
go vet ./...
go test -race -count=1 ./...
?   	github.com/L-K-M/dl-tool/cmd/dl-tool	[no test files]
ok  	github.com/L-K-M/dl-tool/internal/api	91.921s
ok  	github.com/L-K-M/dl-tool/internal/config	1.149s
ok  	github.com/L-K-M/dl-tool/internal/engine	22.013s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.243s
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	8.915s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.017s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.561s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.182s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.185s
ok  	github.com/L-K-M/dl-tool/internal/store	72.643s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.382s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.071s
?   	github.com/L-K-M/dl-tool/web/node_modules/flatted/golang/pkg/flatted	[no test files]
 Test Files  7 passed (7)
      Tests  114 passed (114)
docker compose -f compose.yaml config -q
docker compose -f compose.yaml -f compose.dev.yaml config -q
./scripts/doclint.sh
🔍 2441 Total (in 254ms) 🔗 572 Unique ✅ 2415 OK 🚫 0 Errors 👻 26 Excluded
```

### Browser layout

Chromium exercised the real App with intercepted auth/task responses. The first launch lacked
`libdbus`; the existing browser library bundle supplied it without changing the repository.
At 320, 375, 639, 640 and 1024 px: the body stayed viewport-wide, adjacent rows did not overlap,
and breakpoint changes reset virtual offsets. Mobile metadata retained all five non-wrapping values
and 44 px controls. The first run exposed an 18 px status line; its flex-item fix restored 16 px.
The browser probe was temporary; persistent E2E ownership remains T043.

```text
BROWSER_LAYOUT_OK 320 {"bodyWidth":320,"viewport":320,"rowHeight":160,"nextTop":208,"rowBottom":208,"gridWidth":100,"scrollWidth":173,"card":{"bottom":200,"items":[{"height":16,"whiteSpace":"nowrap"},{"height":16,"whiteSpace":"nowrap"},{"height":16,"whiteSpace":"nowrap"},{"height":16,"whiteSpace":"nowrap"},{"height":16,"whiteSpace":"nowrap"}],"input":{"width":44,"height":44}}}
BROWSER_LAYOUT_OK 375 {"bodyWidth":375,"viewport":375,"rowHeight":160,"nextTop":208,"rowBottom":208,"gridWidth":155,"scrollWidth":173,"card":{"bottom":200,"items":[{"height":16,"whiteSpace":"nowrap"},{"height":16,"whiteSpace":"nowrap"},{"height":16,"whiteSpace":"nowrap"},{"height":16,"whiteSpace":"nowrap"},{"height":16,"whiteSpace":"nowrap"}],"input":{"width":44,"height":44}}}
BROWSER_LAYOUT_OK 639 {"bodyWidth":639,"viewport":639,"rowHeight":160,"nextTop":208,"rowBottom":208,"gridWidth":419,"scrollWidth":419,"card":{"bottom":200,"items":[{"height":16,"whiteSpace":"nowrap"},{"height":16,"whiteSpace":"nowrap"},{"height":16,"whiteSpace":"nowrap"},{"height":16,"whiteSpace":"nowrap"},{"height":16,"whiteSpace":"nowrap"}],"input":{"width":44,"height":44}}}
BROWSER_LAYOUT_OK 640 {"bodyWidth":640,"viewport":640,"rowHeight":32,"nextTop":112,"rowBottom":112,"gridWidth":420,"scrollWidth":1730,"card":null}
BROWSER_LAYOUT_OK 1024 {"bodyWidth":1024,"viewport":1024,"rowHeight":32,"nextTop":112,"rowBottom":112,"gridWidth":804,"scrollWidth":1730,"card":null}
BROWSER_CHECKBOX_OK 320
BROWSER_CHECKBOX_OK 1024
```

### Scope

The exact `git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort` command
printed the three remaining modified grid files before the third incremental commit:

```text
web/src/components/TaskGrid/TaskCardList.tsx
web/src/components/TaskGrid/TaskGrid.test.tsx
web/src/components/TaskGrid/TaskGrid.tsx
```

Earlier steps were already committed and pushed, as requested. The complete PR scope, confirmed with
`git diff --name-only origin/main...HEAD -- . ':(exclude)docs'`, is:

```text
web/src/App.test.tsx
web/src/App.tsx
web/src/components/TaskGrid/TaskCardList.tsx
web/src/components/TaskGrid/TaskGrid.test.tsx
web/src/components/TaskGrid/TaskGrid.tsx
web/src/locales/en/grid.json
web/src/main.test.ts
```

`git diff --check` passed. Only the task document and both T042 index rows accompany these code files.
No generated API or dependency files changed. Squash merge will place code, Evidence and both rows in
one main commit. Review and remote gates remain required before merge.

### Review round 1

Read the complete GLM summary, inline comments and review for `a58ce7b`.
`TestRowCheckboxTogglesWithoutClearingOthers` reproduced lost selection on desktop and mobile before
the fix; both cases now pass, as does the real-browser checkbox probe. Checkbox clicks now toggle
membership through the existing selection owner without collapsing other rows. Mobile controls no
longer use a label that could synthesize a second click.

Rejected the reported stale-index crash: the installed virtualizer includes `count` in its measurement
and index dependencies. `TestShrinkingPageKeepsVirtualIndicesInBounds` passes with its real virtualizer
from 100 rows at the last row, to one replacement, to zero. No invalid-index behavior was reproduced.

The missing-test, missing-Verification, missing-CI, excerpt, fence and status findings are chunk-boundary
false positives: this document contains each named passing test, both command outputs, balanced fences,
and completed status matching both index rows. The module-cycle finding predicts hypothetical future
edits, not a current failure; the entrypoint, unit suites and browser build execute it successfully.
Extracting a new primitives file is outside this task's Files table and unnecessary for correctness.

Deferred optional suggestions: handler memoization, grid label, peer pluralization, filter-guard
consolidation, invalid-filter coverage, test-fixture cleanup and non-task overflow ownership when those
screens gain content. `total`, row indexing, empty-state copy and resize configuration follow the
explicit task contract; lateral cell navigation is not in its keyboard subset. No Makefile or store
changes were made for out-of-scope suggestions. Required review of the updated head remains pending.

## Blocked

None. Earlier plan repairs resolved the task's scope, suite-count, keyboard and mobile-height blockers.
