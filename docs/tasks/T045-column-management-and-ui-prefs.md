# T045 — Add column management and the UI preference document

| Field | Value |
|---|---|
| **ID** | T045 |
| **Milestone** | M3 |
| **Status** | todo |
| **Depends on** | T042, T044 |
| **Blocks** | T053, T064, T104 |
| **Parallel-safe** | no — extends T042's `TaskGrid.tsx`/`TaskGrid.test.tsx` and T044's `Toolbar.tsx` |
| **Implements** | — (client half of [FR-144](../02-requirements.md#fr-144-persist-server-side-ui-preferences), covered by T107) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md) |
| **Est. size** | 3 new files, ~330 LOC |

## Goal
Columns can be shown, hidden, reordered, resized and sorted from the header and from a `Columns ▾` popover
that also works by keyboard, and the whole preference document survives a reload through
`localStorage['dl.ui.prefs.v1']`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/09-web-ui-spec.md` §3.3 Persistence](../09-web-ui-spec.md#33-persistence) — the exact document,
   the debounce rule and the TanStack Table v8 shapes reproduced verbatim.
2. [`docs/09-web-ui-spec.md` §3.4 Sorting, resizing, reordering, visibility](../09-web-ui-spec.md#34-sorting-resizing-reordering-visibility)
   — the gestures, the `aria-sort` values and the keyboard alternative to dragging.
3. [`docs/05-api-contract.md` §11.4 `GET /prefs` and `PUT /prefs`](../05-api-contract.md#114-get-prefs-and-put-prefs)
   — the document travels whole and unknown members are stored verbatim.
4. [`docs/tasks/T042-virtualised-task-grid.md`](T042-virtualised-task-grid.md) — `DEFAULT_COLUMN_ORDER`.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/store/useUiPrefs.ts` | create | The preference document, its defaults and its persistence. |
| `web/src/store/useUiPrefs.test.ts` | create | Defaults, merge, debounce and corrupt-value handling. |
| `web/src/components/TaskGrid/ColumnsMenu.tsx` | create | The `Columns ▾` popover with search, checkboxes and move buttons. |
| `web/src/components/TaskGrid/TaskGrid.tsx` | edit | Drive table state from the prefs store; add resize, reorder and `aria-sort`. |
| `web/src/components/TaskGrid/TaskGrid.test.tsx` | edit | Align sorting expectations with the persisted default; cover remount persistence, `aria-sort`, the multi-sort badge and pinned columns. |
| `web/src/components/Shell/Toolbar.tsx` | edit | Replace the disabled `Columns ▾` placeholder with the popover. |
| `web/src/locales/en/common.json` | edit | `Columns ▾` popover strings (filter, move up/down, reset); doc 09 §10.2 forbids hardcoded text. |

No other file may be modified.

## Interface contract

```ts
// web/src/store/useUiPrefs.ts
export const PREFS_KEY = 'dl.ui.prefs.v1';

export interface UiPrefs {
  version: 1;
  grid: {
    order: string[];
    visibility: Record<string, boolean>;
    sizing: Record<string, number>;
    sorting: { id: string; desc: boolean }[];
    density: 'comfortable' | 'compact';
  };
  sidebarWidth: number;
  sidebarCollapsed: boolean;
  detailHeight: number;
  detailTab: string;
  theme: 'system' | 'light' | 'dark';
  lastDestination: string | null;
}

export const defaultPrefs: UiPrefs;

export interface UiPrefsState extends UiPrefs {
  /** Deep-merges a patch, then schedules one write 500 ms later. */
  patch: (p: Partial<UiPrefs>) => void;
  /** Called by the grid on gesture end; a write is never scheduled during a drag. */
  setDragging: (dragging: boolean) => void;
  resetGrid: () => void;
}

export const useUiPrefs: import('zustand').UseBoundStore<import('zustand').StoreApi<UiPrefsState>>;
```

The `grid` members map 1:1 onto TanStack Table v8 state and are wired straight through, using the API doc
09 §3.3 reproduces verbatim:

```ts
onColumnVisibilityChange, setColumnVisibility, resetColumnVisibility,
column.getIsVisible, column.toggleVisibility
```

```tsx
// web/src/components/TaskGrid/ColumnsMenu.tsx
export function ColumnsMenu(props: {
  table: import('@tanstack/react-table').Table<import('../../store/useTasks').Task>;
}): JSX.Element;
```

- Persistence: `localStorage` only, under `PREFS_KEY`. A missing, unparseable or wrong-`version` value
  yields `defaultPrefs` without throwing.
- Reordering uses `@dnd-kit/sortable` restricted to the horizontal axis; `select` and `name` are pinned and
  non-draggable, and the popover offers *Move up* / *Move down* buttons for keyboard users.
- Header sorting cycles ascending → descending → default, `Shift+click` appends a key, and the header sets
  `aria-sort` to `ascending`, `descending` or `none`.
- Density sets the fixed row height: `comfortable` 32 px, `compact` 26 px.

## Steps
1. Create `web/src/store/useUiPrefs.ts` with the document, `defaultPrefs` matching the JSON in doc 09 §3.3,
   a deep merge in `patch`, and a 500 ms debounced write that is suppressed while `setDragging(true)`.
2. Read `localStorage[PREFS_KEY]` once at module load inside `try`/`catch`; any failure yields
   `defaultPrefs`, and unknown members already present are preserved on write.
3. Edit `TaskGrid.tsx` to take `columnOrder`, `columnVisibility`, `columnSizing` and `sorting` from the
   prefs store and to write every change back through `patch`.
4. Add resizing with `columnResizeMode: 'onChange'` and the CSS-variable technique named in doc 09 §3.4, so
   a 10 000-row grid does not re-render per frame. Double-click on the grab zone auto-fits to content.
5. Add header drag-reordering with `@dnd-kit/sortable`, horizontal axis only, with `select` and `name`
   pinned.
6. Create `ColumnsMenu.tsx`: a search box, one checkbox per column, *Move up* / *Move down* per row and
   *Reset to defaults* calling `resetGrid`.
7. Edit `Toolbar.tsx` to render `ColumnsMenu` in place of the disabled placeholder.
8. Create `web/src/store/useUiPrefs.test.ts`: defaults when storage is empty; a corrupt JSON value falls
   back without throwing; `patch` deep-merges and leaves unknown members intact; no write is scheduled
   while dragging and exactly one write lands 500 ms after the last change; `resetGrid` restores
   `DEFAULT_COLUMN_ORDER`.
9. Edit `web/src/components/TaskGrid/TaskGrid.test.tsx`: `TestTimestampSortUsesInstants` now mounts with
   the persisted default `addedOn` descending — the first click on `Added` clears the sort back to
   server order (doc 09 §3.4's ascending → descending → default cycle) and the second click ascends, so
   the instant-based comparator is still proven. Add coverage that hiding, reordering and resizing a
   column survive a remount of the grid, that header cells expose `aria-sort` with the `1`/`2` badge on
   multi-sort, and that `select`/`name` cannot leave their pinned positions (no drag, move buttons
   disabled in the popover).
10. Add the `Columns ▾` popover strings to `web/src/locales/en/common.json`.
11. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestDefaultsWhenStorageEmpty`, `TestCorruptStorageFallsBack` and `TestPatchPreservesUnknownMembers`
      pass.
- [ ] `TestNoWriteDuringDrag` and `TestSingleDebouncedWrite` pass.
- [ ] Hiding, reordering and resizing a column survives a remount of the grid.
- [ ] Header cells expose `aria-sort`, and multi-sort shows the `1`/`2` badge.
- [ ] `select` and `name` cannot be dragged out of their pinned positions.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo PREFS_OK
```
Expected: Vitest reports `Test Files  N passed (N)` where `N` equals the number of pre-existing
`web/src` test files plus one for `src/store/useUiPrefs.test.ts` (9 at the time of writing); every test
named above appears as passing, and the final line of stdout is exactly `PREFS_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT call `GET /prefs` or `PUT /prefs`; the endpoints arrive with M6's preferences task (T107) and this
  task persists to `localStorage` only, exactly as doc 09 `## Open questions` states.
- Do NOT add column virtualisation, and do NOT change the fixed row heights.
- Do NOT persist the selection, the current filter or any task data.
- Do NOT move the theme member's owner: T039 applies the class, this store only records the choice.
- Do NOT add a settings screen; T053 owns the Density and Theme controls that write into this document.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

Plan-repair evidence only, not implementation proof. With the required default sort applied
(`useState<SortingState>([{ id: "addedOn", desc: true }])` on `d165163`, after `npm ci --prefix web`):

```text
 FAIL  src/components/TaskGrid/TaskGrid.test.tsx > TestTimestampSortUsesInstants
- Expected ["earlier","later"]
+ Received ["later","earlier"]
```

That is the correct behavior under doc 09 §3.4 — one click on a descending column clears the sort —
so the existing test, not the sort cycle, had to move. The repair adds the test file and the locale
file to the Files table; both are required by the task's own rules.

## Blocked

Resolved by the plan repair: `web/src/components/TaskGrid/TaskGrid.test.tsx` and
`web/src/locales/en/common.json` are now in the Files table, and the Verification suite count tracks
the pre-existing files instead of a hard-coded number.

Original defect: `defaultPrefs` must match doc 09 §3.3, whose `grid.sorting` is
`[{"id":"addedOn","desc":true}]`, and Step 3 wires `sorting` straight through to the table state. That
makes `TestTimestampSortUsesInstants` fail — one click on `Added` clears the descending sort back to
server order `[later, earlier]`, never the asserted `[earlier, later]` — and three acceptance criteria
(remount persistence, `aria-sort`/badge, pinned columns) had no test file to live in. The popover
strings likewise required `common.json`, which doc 09 §10.2's no-hardcoded-strings rule puts outside a
silent defaultValue workaround.
