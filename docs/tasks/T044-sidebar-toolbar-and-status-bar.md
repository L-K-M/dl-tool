# T044 — Build the sidebar tree, the toolbar and the status bar

| Field | Value |
|---|---|
| **ID** | T044 |
| **Milestone** | M3 |
| **Status** | todo |
| **Depends on** | T022, T023, T041, T042 |
| **Blocks** | T045, T049, T051, T052, T053, T104 |
| **Parallel-safe** | no — it also edits the shared files `web/src/App.tsx`, `web/src/locales/en/common.json` |
| **Implements** | — (renders [FR-013](../02-requirements.md#fr-013-resolve-the-sidebar-filter-sets), [FR-014](../02-requirements.md#fr-014-apply-lifecycle-and-queue-actions-to-a-selection) and [FR-015](../02-requirements.md#fr-015-remove-a-task-with-or-without-its-data), covered by T021, T022 and T023) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md) |
| **Est. size** | 4 new files, ~400 LOC. The three chrome regions share one layout and one test file. |

## Goal
The shell shows Download Station's sidebar taxonomy with live counts, a fixed-order toolbar whose
selection-dependent buttons drive `POST /tasks/actions` and `DELETE /tasks/{id}`, and a status bar
reporting connection, rates, counts, free space and the schedule.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/09-web-ui-spec.md` §2.4 Sidebar tree](../09-web-ui-spec.md#24-sidebar-tree) — node labels, count
   semantics, the dimming rule and the landmark markup.
2. [`docs/09-web-ui-spec.md` §2.5 Toolbar](../09-web-ui-spec.md#25-toolbar) — the fixed button order and the
   disabled-state rule.
3. [`docs/09-web-ui-spec.md` §2.6 Status bar](../09-web-ui-spec.md#26-status-bar) — the six segments.
4. [`docs/05-api-contract.md` §5.7 `POST /tasks/actions`](../05-api-contract.md#57-post-tasksactions) and
   [§5.6 `DELETE /tasks/{id}`](../05-api-contract.md#56-delete-tasksid) — action names and the two query flags.
5. [`docs/09-web-ui-spec.md` §10.6 Toasts and optimistic updates](../09-web-ui-spec.md#106-toasts-and-optimistic-updates)
   — which mutations are optimistic and which are never.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/Shell/Sidebar.tsx` | create | The filter, category and tag tree with counts. |
| `web/src/components/Shell/Toolbar.tsx` | create | The fixed-order action bar and the filter box. |
| `web/src/components/Shell/StatusBar.tsx` | create | The six status segments. |
| `web/src/components/Shell/Shell.test.tsx` | create | Counts, disabled states, action payloads and segments. |
| `web/src/App.tsx` | edit | Mount the three regions inside `AppLayout`. |
| `web/src/locales/en/common.json` | edit | Sidebar, toolbar and status-bar strings. |

No other file may be modified.

## Interface contract

```tsx
// web/src/components/Shell/Sidebar.tsx
export const DOWNLOAD_NODES = [
  { filter: 'all',         to: '/' },
  { filter: 'downloading', to: '/tasks/downloading' },
  { filter: 'completed',   to: '/tasks/completed' },
  { filter: 'active',      to: '/tasks/active' },
  { filter: 'inactive',    to: '/tasks/inactive' },
  { filter: 'stopped',     to: '/tasks/stopped' },
  { filter: 'error',       to: '/tasks/error' },
] as const;

export function Sidebar(): JSX.Element;
```

```tsx
// web/src/components/Shell/Toolbar.tsx
export type BulkAction =
  | 'pause' | 'resume' | 'remove' | 'recheck' | 'force_complete'
  | 'queue_top' | 'queue_up' | 'queue_down' | 'queue_bottom';

/** POST /api/v1/tasks/actions with {ids, action}. Optimistic for pause, resume and the queue moves. */
export function useBulkAction(): (action: BulkAction, ids: string[]) => Promise<void>;

/** The client-side name filter of doc 09 §2.5 item 7; 250 ms debounce. */
export function useNameFilter(): { value: string; set: (v: string) => void };

export function Toolbar(): JSX.Element;
```

```tsx
// web/src/components/Shell/StatusBar.tsx
export function StatusBar(): JSX.Element;
```

Sidebar markup, per group:

```html
<nav aria-labelledby="nav-download"><h2 id="nav-download" class="sr-only">Download</h2>
  <a href="/tasks/downloading" aria-current="page">Downloading <span class="count">6</span></a>
```

- Counts come from `selectFilterCounts`, `selectCategoryCounts` and `selectTagCounts` (T041), which read
  the SSE-fed store. Never issue one request per node.
- A zero-count `DOWNLOAD` node stays in place at 45 % opacity; it is never hidden.
- Status-bar segment 1 is `role="status" aria-live="polite"`; segment 2 uses tabular numerals.
- Free space comes from `GET /api/v1/fs/free-space?path=` for the selected task's destination, and falls
  back to the default destination. Until T047 serves that endpoint the segment renders `—`.

## Steps
1. Create `Sidebar.tsx` with the exact node labels and order of doc 09 §2.4, the `CATEGORIES` and `TAGS`
   groups derived from the store selectors, and `aria-current="page"` on the active node.
2. Create `Toolbar.tsx` with the eight ordered items of doc 09 §2.5. `+ Add` and `Columns ▾` render
   disabled with the tooltip `Coming with the add dialog` and are wired by T049 and T045.
3. Give every selection-dependent control both `disabled` and `aria-disabled="true"` when the selection is
   empty, with a tooltip naming the reason.
4. Implement `useBulkAction` over `POST /api/v1/tasks/actions`; make pause, resume and the four queue moves
   optimistic with `onMutate`/`onError`/`onSettled`, and make remove non-optimistic.
5. Wire `Remove ▾` to a confirmation dialog naming the affected tasks with an unticked
   `Also delete downloaded files` box, which maps to `DELETE /tasks/{id}?delete_data=`. `Shift+Delete`
   opens the same dialog with the box pre-ticked and says so in the body.
6. Add the theme toggle button calling `applyTheme` and `storeTheme` from T039.
7. Create `StatusBar.tsx` with the six segments in order, every number through the T041 formatters.
8. Edit `web/src/App.tsx` to render `Toolbar`, `Sidebar` and `StatusBar` in the three region slots.
9. Create `Shell.test.tsx`: counts render from a seeded store; a zero-count node is present and dimmed;
   toolbar buttons are disabled with an empty selection; `Pause` posts
   `{"ids":["tsk_…"],"action":"pause"}`; the remove dialog's delete-files box starts unticked; the status
   bar shows the rates and the active/total counts.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestSidebarCountsComeFromStore`, `TestZeroCountNodeStaysVisible` and `TestActiveNodeHasAriaCurrent`
      pass.
- [ ] `TestToolbarDisabledWithoutSelection` and `TestPausePostsActionsPayload` pass.
- [ ] `TestRemoveDialogDeleteFilesUnticked` passes, including the `Shift+Delete` pre-ticked variant.
- [ ] `TestStatusBarSegments` asserts all six segments in order.
- [ ] No component in this task calls `fetch` directly; every request goes through the T014 client.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo SHELL_OK
```
Expected: Vitest reports `Test Files  7 passed (7)` including `src/components/Shell/Shell.test.tsx`, every
test named above appears as passing, and the final line of stdout is exactly `SHELL_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement the add dialog behind `+ Add`; T049 owns it.
- Do NOT implement the `Columns ▾` popover or persist the sidebar width; T045 owns both.
- Do NOT add search, RSS or settings navigation targets beyond the links doc 09 §2.4 lists; M4, M5 and T053
  own those screens.
- Do NOT hide zero-count `DOWNLOAD` nodes, and do NOT reorder the toolbar.
- Do NOT surface `force_complete` in the toolbar; doc 09 leaves that an open question.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
