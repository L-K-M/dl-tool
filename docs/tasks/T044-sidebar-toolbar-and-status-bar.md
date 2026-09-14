# T044 — Build the sidebar tree, the toolbar and the status bar

| Field | Value |
|---|---|
| **ID** | T044 |
| **Milestone** | M3 |
| **Status** | done |
| **Depends on** | T022, T023, T041, T042 |
| **Blocks** | T045, T049, T051, T052, T063, T072, T104 |
| **Parallel-safe** | no — it also edits the shared files `web/src/App.tsx`, `web/src/App.test.tsx` and `web/src/locales/en/common.json` |
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
6. [Doc 09 §3.6 Keyboard](../09-web-ui-spec.md#36-keyboard) — this task's shortcut ownership and
   cross-component integration rules.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/Shell/Sidebar.tsx` | create | The filter, category and tag tree with counts. |
| `web/src/components/Shell/Toolbar.tsx` | create | The fixed-order action bar and the filter box. |
| `web/src/components/Shell/StatusBar.tsx` | create | The six status segments. |
| `web/src/components/Shell/Shell.test.tsx` | create | Counts, disabled states, action payloads and segments. |
| `web/src/components/TaskGrid/TaskGrid.tsx` | edit | Extend the existing listener with this task's action callbacks. |
| `web/src/components/TaskGrid/TaskGrid.test.tsx` | edit | Guard, dispatch-once and dialog-isolation regressions. |
| `web/src/App.tsx` | edit | Mount the three regions inside `AppLayout` and wire grid action callbacks. |
| `web/src/App.test.tsx` | edit | Assert the mounted chrome in `TestBootRendersLayout` instead of empty landmarks. |
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
   opens the same dialog with the box pre-ticked and says so in the body. Integrate this task's
   [§3.6 shortcuts](../09-web-ui-spec.md#36-keyboard) through typed callbacks in the existing grid
   listener, wired in `App.tsx` to the shell's filter, confirmation flow and cheat-sheet overlay.
   Keep mutations behind `useBulkAction`; no duplicate shortcut listener. Use existing dialog primitives
   and common locale strings for the overlay. Closing either dialog restores focus without clearing the
   grid selection; cancelling removal sends no request.
6. Add the theme toggle button calling `applyTheme` and `storeTheme` from T039.
7. Create `StatusBar.tsx` with the six segments in order, every number through the T041 formatters.
8. Edit `web/src/App.tsx` to render `Toolbar`, `Sidebar` and `StatusBar` in the three region slots.
   Replace `TestBootRendersLayout`'s empty-landmark assertions in `web/src/App.test.tsx` with
   assertions that each region renders its mounted chrome: the toolbar in `banner`, the navigation
   tree in `complementary` and the status segments in `contentinfo`.
9. Create `Shell.test.tsx`: counts render from a seeded store; a zero-count node is present and dimmed;
   toolbar buttons are disabled with an empty selection; `Pause` posts
   `{"ids":["tsk_…"],"action":"pause"}`; the remove dialog's delete-files box starts unticked; the status
   bar shows the rates and the active/total counts. Add `TestShellKeyboardActions` through the mounted
   grid and shell to exercise every shortcut assigned here by §3.6, including the guard, a single
   action per key, empty-selection removal, cancel-without-request and dialog focus restoration.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [x] `TestSidebarCountsComeFromStore`, `TestZeroCountNodeStaysVisible` and `TestActiveNodeHasAriaCurrent`
      pass.
- [x] `TestToolbarDisabledWithoutSelection` and `TestPausePostsActionsPayload` pass.
- [x] `TestRemoveDialogDeleteFilesUnticked` passes, including the `Shift+Delete` pre-ticked variant.
- [x] `TestStatusBarSegments` asserts all six segments in order.
- [x] `TestShellKeyboardActions` proves this task's [§3.6 actions](../09-web-ui-spec.md#36-keyboard)
  through the mounted grid and shell, including editable-target and dialog isolation, and no deletion
  before confirmation.
- [x] No component in this task calls `fetch` directly; every request goes through the T014 client.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo SHELL_OK
```
Expected: Vitest reports `Test Files  N passed (N)` where `N` equals the number of pre-existing
`web/src` test files plus one for `src/components/Shell/Shell.test.tsx` (8 at the time of writing);
every test named above appears as passing; and the final line of stdout is exactly `SHELL_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table and nothing else. Use `git status`, not
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

`npm ci --prefix web` installed the pinned dependencies; no pins changed.

### Acceptance proof

`TestShellKeyboardActions` drives Delete, Shift+Delete, Ctrl/Cmd+F, `?` and Escape through the
mounted grid and shell, covering the editable-target guard, dialog-Escape isolation,
cancel-without-request, no-op removal on empty selection and focus restoration to the invoking row.
`TestShellActionCallbacksDispatchOnce` in the grid suite proves the single keydown listener
dispatches each typed callback once, including when the name filter leaves zero visible rows while
the selection stays live. `TestRemoveDialogDeleteFilesUnticked` covers both the unticked default and
the Shift+Delete pre-ticked variant. `grep -rn 'fetch(' web/src/components/Shell` returns nothing;
every request uses the T014 client.

Supplemental command: `cd web && npx vitest run --reporter=verbose` (exit 0), excerpts:

```text
✓ src/components/Shell/Shell.test.tsx > TestSidebarCountsComeFromStore 104ms
✓ src/components/Shell/Shell.test.tsx > TestZeroCountNodeStaysVisible 27ms
✓ src/components/Shell/Shell.test.tsx > TestActiveNodeHasAriaCurrent 23ms
✓ src/components/Shell/Shell.test.tsx > TestToolbarDisabledWithoutSelection 84ms
✓ src/components/Shell/Shell.test.tsx > TestPausePostsActionsPayload 68ms
✓ src/components/Shell/Shell.test.tsx > TestRemoveDialogDeleteFilesUnticked unticked 41ms
✓ src/components/Shell/Shell.test.tsx > TestRemoveDialogDeleteFilesUnticked shiftDeletePreChecked 22ms
✓ src/components/Shell/Shell.test.tsx > TestStatusBarSegments 8ms
✓ src/components/Shell/Shell.test.tsx > TestShellKeyboardActions 1204ms
✓ src/App.test.tsx > TestBootRendersLayout 218ms
✓ src/components/TaskGrid/TaskGrid.test.tsx > TestShellActionCallbacksDispatchOnce 109ms
✓ src/components/Shell/Shell.test.tsx > TestRemoveDialogRecoversFromTransportFailure
✓ src/components/Shell/Shell.test.tsx > TestToolbarCollapsesToIconsBelow1100
✓ src/App.test.tsx > TestSignOutFailureKeepsSession
✓ src/App.test.tsx > TestSignOutSuccessClearsSession
```

### Verification

`make lint && make typecheck && make test-web && echo SHELL_OK` exited 0:

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

 Test Files  8 passed (8)
      Tests  128 passed (128)
   Start at  12:20:50
   Duration  7.06s (transform 1.19s, setup 0ms, import 5.03s, tests 12.88s, environment 2.64s)

SHELL_OK
```

Expected auth-error responses, the deliberately failed task-page request and React scheduler
warnings from the test harness are omitted above.

### Scope

`git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort`:

```text
web/src/App.test.tsx
web/src/App.tsx
web/src/components/Shell/Shell.test.tsx
web/src/components/Shell/Sidebar.tsx
web/src/components/Shell/StatusBar.tsx
web/src/components/Shell/Toolbar.tsx
web/src/components/TaskGrid/TaskGrid.test.tsx
web/src/components/TaskGrid/TaskGrid.tsx
web/src/locales/en/common.json
```

Exactly the Files table.

### Full gate

`PATH="/tmp/t039-tools:$PATH" make ci` exited 0, reusing the existing Docker CLI for compose
validation. Output excerpts:

```text
0 issues.
All matched files use Prettier code style!
go vet ./...
go test -race -count=1 ./...
ok  	github.com/L-K-M/dl-tool/internal/api	87.173s
ok  	github.com/L-K-M/dl-tool/internal/config	1.137s
ok  	github.com/L-K-M/dl-tool/internal/engine	21.298s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.224s
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	8.868s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.024s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.757s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.184s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.106s
ok  	github.com/L-K-M/dl-tool/internal/store	70.381s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.380s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.079s
 Test Files  8 passed (8)
      Tests  128 passed (128)
docker compose -f compose.yaml config -q
docker compose -f compose.yaml -f compose.dev.yaml config -q
./scripts/doclint.sh
🔍 2450 Total (in 232ms) 🔗 573 Unique ✅ 2424 OK 🚫 0 Errors 👻 26 Excluded
```

### Review record

The independent diff review against this file flagged three correctness findings; all were fixed
and covered by the suite:

- Queue-move rollback snapshotted only the selected ids; it now snapshots every task the optimistic
  patch touched so unselected rows revert on failure.
- Shell shortcuts ran after the grid's zero-row early return; Delete, Ctrl/Cmd+F, `?` and Escape now
  run before it so they work when the client-side filter empties the grid with a live selection.
- The category and tag fallback `NavLink`s could prefix-match child routes; both now use `end`.

Deferred: a suggestion that the "Remove task and files" menu item should open the dialog with the
delete-data box pre-ticked. Step 5 and doc 09 §10.6 require the Remove menu's dialog to start
unticked; only Shift+Delete pre-checks it, so both menu items open the same confirmation.

A second review round on the pushed head flagged three more findings, all fixed with regressions:

- `signOut` cleared local auth in `finally`, so a failed or unreachable logout looked signed out;
  it now clears local state only after a confirmed success and toasts `shell.signOutFailed`
  otherwise (`TestSignOutFailureKeepsSession`, `TestSignOutSuccessClearsSession`).
- A rejected `DELETE` left the removal dialog's `busy` flag set and skipped reconciliation of
  earlier successes; `confirm` now catches transport failures, resets `busy` in `finally` and still
  applies confirmed removals (`TestRemoveDialogRecoversFromTransportFailure`).
- Toolbar labels never collapsed, so right-hand controls were unreachable below the doc 09 §2.5
  1100 px breakpoint; labels now hide under `max-[1100px]` with `aria-label` preserving each name,
  icons were added for Edit and Move, and the bar scrolls horizontally as a last resort
  (`TestToolbarCollapsesToIconsBelow1100`).

The same round recorded a pre-existing defect outside this task's Files table:
`internal/engine/qbittorrent/client_test.go` `newClient` never registers `Close`, so `Connect`'s
background pollers outlive their tests. It surfaced once as push-run 34837489148 integration job
103954541708 failing `TestInspectionBaselineWaitsForSeed/unrelated_torrent` with
`unexpected request: GET /api/v2/sync/maindata` at `contract_test.go:940` — a leaked poller hitting
a reused port. The rerun and the pull_request run both passed; the leak itself needs a separate
prerequisite repair.

A third automated round on `04bca58` produced one rejected blocker and twelve applied fixes:

- Rejected (spec evidence): pre-ticking the delete-files box for the "Remove task and files" menu
  item. Doc 09 section 10.6 says the checkbox is *never* pre-checked except after `Shift+Delete`;
  both menu items therefore open the same unticked confirmation.
- Applied: `signOut` now clears the shared query cache too, excluding the session key so its active
  observer cannot refetch and re-authenticate.
- Applied: sidebar active nodes get `aria-[current=page]` styling; grid `Escape` with an empty
  selection is a no-op like `Delete`; the remove dialog ignores Escape/outside dismissal while
  deletions are in flight; removals run via `Promise.allSettled` so every id is attempted and each
  failure reports its own detail; dialog task names subscribe to the store; failed bulk results
  never toast an empty string; held-key Delete auto-repeat cannot re-dispatch; the task-pending
  shimmer respects `prefers-reduced-motion`; sidebar download labels use static i18n keys;
  uncategorised/untagged counts are memoized; the status bar hides its decorative glyphs from
  assistive tech and uses tabular numerals throughout; the Shell suite's `afterEach` clears the
  injected `<base>` and history.
- Rejected (server contract): the `/tasks/category` and `/tasks/tag` fallback routes pass `""`, and
  the API deliberately reads `?category=`/`?tag=` as the uncategorised/untagged filter
  (`internal/api/tasks.go` lines 119-123, 258, 1042-1047).
- Rejected (spec taxonomy): the Saved Searches node targets `/search` because doc 09's route table
  defines no distinct saved-searches route; M4 owns that screen.
- Rejected (spec'd structure): the empty sixth status-bar span reserves the alt-speed segment of
  doc 09 section 2.6, whose data source does not exist yet.
- Rejected (spec'd keys): doc 09 section 3.6 lists `Delete` only; `Backspace` is not a removal key.
- Deferred: aggregating per-task bulk-action failures into one toast.

## Blocked
Resolved by the plan repair: `web/src/App.test.tsx` is now in the `## Files` table with the
`TestBootRendersLayout` rewrite assigned to step 8, and the `## Verification` expectation derives the
`N passed (N)` total from the pre-existing `web/src` test files plus `Shell.test.tsx` instead of
hard-coding a number. The repair unblocked the task; the implementation landed as described above.

Original blocker: `TestBootRendersLayout` asserts `screen.getByRole("banner").textContent === ""`,
`getByRole("complementary").textContent === ""` and `getByRole("contentinfo").textContent === ""`
(`web/src/App.test.tsx`, T040 commit `6a08ec9`). T040's own task file says the regions stay "empty
landmark[s] until T042 and T044 fill them", so the assertions were written as placeholders. Step 8
mounts `Toolbar`, `Sidebar` and `StatusBar` in exactly those three regions; any spec-conforming
content makes all three `textContent`s non-empty, so the test fails and the file was outside the
`## Files` table.

The original `## Verification` block also expected `Test Files  7 passed (7)`, but `web/src` already
contained 7 test files before `Shell.test.tsx` (`App.test.tsx`, `api/client.test.ts`,
`components/TaskGrid/TaskGrid.test.tsx`, `lib/format.test.ts`, `lib/theme.test.ts`, `main.test.ts`,
`store/useTasks.test.ts`), so the correct expectation is 8. The scope check's "in that order" phrase
was dropped because `git status --porcelain` sorts paths and the `## Files` table does not.

Separate unresolved prerequisite (outside this task's Files table): the push-triggered integration
run `34837489148`, job `103954541708`, failed `TestInspectionBaselineWaitsForSeed/unrelated_torrent`
with `contract_test.go:940: unexpected request: GET /api/v2/sync/maindata`. Review traced the likely
cause to leaked qBittorrent pollers: `internal/engine/qbittorrent/client_test.go` `newClient` creates
connected clients without registering `client.Close`, so `Connect`'s background pollers outlive their
tests and hit a reused port. That file is not in this task's Files table and was deliberately not
edited in this PR. The duplicate pull_request run passed and a rerun of the failed job passed, but
the leak remains a real defect on `main` that needs a separate focused repair before this PR merges.

Resolved: the repair landed on `main` as `19da8a9` via
[#156](https://github.com/L-K-M/dl-tool/pull/156) — `newClient` now registers the client's `Close` in
`t.Cleanup` so `Connect`'s poll dies with the test. Its second review round reported zero actionable
suggestions and all 14 checks passed on `e4701f0`.
