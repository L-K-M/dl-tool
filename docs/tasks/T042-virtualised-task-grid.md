# T042 — Render the virtualised task grid and the mobile card list

| Field | Value |
|---|---|
| **ID** | T042 |
| **Milestone** | M3 |
| **Status** | todo |
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
6. Implement selection and the keyboard model of doc 09 §3.5 and §3.6, writing the selection into the T041
   store. One `keydown` listener, which returns immediately when the event target is `INPUT`, `TEXTAREA` or
   `isContentEditable`. Exactly one row carries `tabIndex={0}`.
7. Create `TaskCardList.tsx`: name clamped to two lines, the progress bar, then
   `Status · Size · ↓rate · ↑rate · ETA`, tap targets at least 44 px, rendered below 640 px.
   Use the fixed mobile height from doc 09 §3.9 and the content budget and overflow rules in §10.3.
8. Edit `web/src/App.tsx` so `TasksRoute` reads `:filter`, `:name` and renders `TaskGrid`.
9. Create `TaskGrid.test.tsx` with `msw` serving a `GET /tasks` page and the store seeded through
   `hydrate`: assert the fifteen headers, a formatted size cell, status sorting by `STATUS_ORDINAL`,
   `Shift+click` range selection, and `aria-rowcount` equal to the reported `total` while a 10 000-row
   store renders far fewer `role="row"` elements. Add `TestRowHeightTracksLayoutAndDensity` to check both
   densities on each side of the mobile breakpoint: rendered heights and virtualiser estimates follow
   doc 09 §3.9, including cached offsets and total virtual height after a breakpoint change, with row
   order and selection preserved. Cover all five metadata items at narrow mobile widths and assert the
   §10.3 budget, non-wrapping items, and touch-target styles.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestRendersDefaultColumns`, `TestStatusSortsByOrdinal`, `TestShiftClickSelectsRange` pass.
- [ ] `TestAriaRowcountIsTotalNotDomRows` passes with 10 000 tasks in the store.
- [ ] `TestRowHeightTracksLayoutAndDensity` passes; mobile cards retain the required content and tap
  targets without overlapping adjacent cards.
- [ ] Every cell whose source is null or zero renders `—`, and `eta_seconds: null` renders `∞`.
- [ ] The page body never scrolls horizontally; only the grid's own scroll container does.
- [ ] No column is added that doc 09 §3.1 defers to v2, and no cell renders a placeholder value.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo GRID_OK
```
Expected: Vitest reports `Test Files  6 passed (6)` including
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
Implementation Verification pending. Replace this section with fresh Verification and `make ci` output
when implementing the grid. The following is plan-repair evidence only, not proof of grid rendering.

Before repair, this contract check failed; after repair, the same check passed:

```bash
python3 - <<'PY'
from pathlib import Path
spec = Path('docs/09-web-ui-spec.md').read_text().split('### 3.9 Virtualisation\n', 1)[1].split('\n---', 1)[0]
assert 'mobile' in spec.lower(), 'No mobile fixed-height exception in canonical virtualisation contract'
PY
```

```text
AssertionError: No mobile fixed-height exception in canonical virtualisation contract
```

`npm ci --prefix web` installed the pinned dependencies; npm reported two high-severity audit findings.
No dependency changed. The first `make ci` attempt reached compose validation but failed because Docker
was absent from `PATH`. Reusing the existing Docker CLI with
`PATH="/tmp/t039-tools:$PATH" make ci` exited 0. Output excerpts:

```text
0 issues.
All matched files use Prettier code style!
 Test Files  6 passed (6)
      Tests  89 passed (89)
docker compose -f compose.yaml config -q
docker compose -f compose.yaml -f compose.dev.yaml config -q
./scripts/doclint.sh
🔍 2430 Total (in 212ms) 🔗 572 Unique ✅ 2404 OK 🚫 0 Errors 👻 26 Excluded
```

`git diff --check` exited 0. Task status and both T042 index rows were checked and remain `todo`.
No grid code was added; the new acceptance test remains implementation work.

Review reproduced stale offsets in the installed `@tanstack/virtual-core`: create three estimated rows,
read `getTotalSize()`, change `estimateSize` through `setOptions`, then call `measure()`. Output:

```text
initial total: 96
changed estimate, stale total: 96
cache reset total: 480
```

The canonical contract now requires that cache reset and defines the mobile content budget.
An isolated Chromium layout fixture exercised that budget with five metadata lines, adjacent cards,
and an oversized non-wrapping value using only the grid's horizontal scroller. Browser startup first
failed on missing shared libraries; using the existing browser library bundle resolved it. Output:

```text
MOBILE_BUDGET_OK width=320: five metadata lines, 44px target, long-value grid scroll
MOBILE_BUDGET_OK width=375: five metadata lines, 44px target, long-value grid scroll
MOBILE_BUDGET_OK width=639: five metadata lines, 44px target, long-value grid scroll
```

This proves the specified budget is feasible, not that the unimplemented TaskGrid uses it.
`npm audit --prefix web --omit=dev --json` reported zero vulnerabilities. The full audit identified
`js-yaml` and its dependent `@redocly/openapi-core` under development dependencies
([GHSA-2883-xcg3-v3hh](https://github.com/advisories/GHSA-2883-xcg3-v3hh)). Dependency remediation is
outside this plan repair; no pins changed.

## Blocked
Resolved by the plan repair: [doc 09 §3.9](../09-web-ui-spec.md#39-virtualisation) now separates table
row density from fixed mobile card height while preserving no auto-measurement and the existing mobile
content and tap-target requirements. This repairs the missing exception, not the implementation.
T042 and both index rows remain `todo`; implementation Verification is still pending.
