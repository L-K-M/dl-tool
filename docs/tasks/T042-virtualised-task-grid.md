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
- [ ] `TestEntrypointRendersTaskGridUnderBase` and `TestTaskRoutesRequestServerFilters` pass in the
  existing entrypoint and App suites, preserving their root, auth, CSRF, layout and routing guarantees.
- [ ] `TestRendersDefaultColumns`, `TestStatusSortsByOrdinal`, `TestShiftClickSelectsRange` pass.
- [ ] `TestAriaRowcountIsTotalNotDomRows` passes with 10 000 tasks in the store.
- [ ] `TestGridKeyboardNavigationAndSelection` proves this task's
  [keyboard subset](../09-web-ui-spec.md#36-keyboard), including the editable-target guard and leaving
  later-owned shortcuts unintercepted.
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

### Entrypoint test-scope repair verification

Reproduced the draft's failure on `d99ca34` after `npm ci --prefix web`:

```text
 FAIL  src/main.test.ts > renders the application root
AssertionError: expected 'Loading tasks…' to be 'Downloads' // Object.is equality
 Test Files  1 failed | 5 passed (6)
      Tests  1 failed | 88 passed (89)
make: *** [Makefile:44: test-web] Error 1
```

The install reported two high-severity audit findings; no pins changed. The repair removes the draft's
unfinished grid changes, leaving production code and tests identical to main at `2ab1c65`.
Only this canonical task contract changes in the final PR diff. Implementation remains pending.

This scope regression check failed before the repair and passed afterward:

```bash
python3 - <<'PY'
from pathlib import Path

text = Path('docs/tasks/T042-virtualised-task-grid.md').read_text()
files = text.split('## Files\n', 1)[1].split('No other file', 1)[0]
for path in ('web/src/main.test.ts', 'web/src/App.test.tsx'):
    assert f'| `{path}` | edit |' in files, f'T042 omits required integration test: {path}'
acceptance = text.split('## Acceptance criteria\n', 1)[1].split('## Verification', 1)[0]
assert 'TestEntrypointRendersTaskGridUnderBase' in acceptance
assert 'TestTaskRoutesRequestServerFilters' in acceptance
assert '| **Status** | todo |' in text
assert '- [x]' not in acceptance
rows = [line for line in Path('docs/tasks/00-task-index.md').read_text().splitlines()
        if line.startswith(('| [T042]', '| T042 |'))]
assert len(rows) == 2 and all('| todo |' in line for line in rows)
print('ENTRYPOINT_SCOPE_OK: both tests authorized, integration acceptance required, T042 todo')
PY
```

Before repair (exit 1, excerpt):

```text
AssertionError: T042 omits required integration test: web/src/main.test.ts
```

After repair (exit 0):

```text
ENTRYPOINT_SCOPE_OK: both tests authorized, integration acceptance required, T042 todo
```

On the repaired baseline, `make lint && make typecheck && make test-web && echo GRID_OK` exited 0.
Output excerpts:

```text
0 issues.
All matched files use Prettier code style!
 Test Files  6 passed (6)
      Tests  89 passed (89)
   Start at  03:23:11
   Duration  2.32s (transform 760ms, setup 0ms, import 2.84s, tests 2.56s, environment 1.60s)

GRID_OK
```

This is baseline proof only: the required seventh suite and new integration assertions do not exist
until T042 is implemented. No acceptance completion is claimed.

`PATH="/tmp/t039-tools:$PATH" make ci` exited 0, using the existing Docker CLI for compose validation.
Output excerpts:

```text
0 issues.
All matched files use Prettier code style!
go vet ./...
cd web && npx tsc --noEmit -p tsconfig.json
go test -race -count=1 ./...
 Test Files  6 passed (6)
      Tests  89 passed (89)
docker compose -f compose.yaml config -q
docker compose -f compose.yaml -f compose.dev.yaml config -q
./scripts/doclint.sh
🔍 2446 Total (in 245ms) 🔗 574 Unique ✅ 2418 OK 🚫 0 Errors 👻 28 Excluded
```

### Superseded implementation attempt: scope blocked

Original draft [PR #151](https://github.com/L-K-M/dl-tool/pull/151), implementation commit `7458842`.
The recovery converted that PR to a plan-only repair and removed its scaffold from the final diff.
The output below records the failed attempt, not the repaired baseline.
The scaffold is incomplete. No acceptance box is checked; `TaskGrid.test.tsx` does not exist yet.
Task status and both index rows remain `todo`. `npm ci --prefix web` succeeded with unchanged pins
and two high-severity audit findings.

Ran the exact Verification command on this implementation tree:

```bash
make lint && make typecheck && make test-web && echo GRID_OK
```

Output excerpts (exit 2; repeated MSW stack traces omitted):

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

InternalError: [MSW] Cannot bypass a request when using the "error" strategy for the "onUnhandledRequest" option.

 FAIL  src/main.test.ts > renders the application root
AssertionError: expected 'Loading tasks…' to be 'Downloads' // Object.is equality

Expected: "Downloads"
Received: "Loading tasks…"

 ❯ src/main.test.ts:60:54
     58|   await screen.findByRole("main");
     59|   expect(mount).toHaveBeenCalledWith(host);
     60|   expect(screen.getByTestId("app-root").textContent).toBe("Downloads");
       |                                                      ^
     61|   expect(host.contains(screen.getByTestId("app-root"))).toBe(true);
     62|   expect(bootRequests).toBe(1);

 Test Files  1 failed | 5 passed (6)
      Tests  1 failed | 88 passed (89)
   Start at  03:14:54
   Duration  2.44s (transform 773ms, setup 0ms, import 2.64s, tests 2.86s, environment 1.68s)

make: *** [Makefile:44: test-web] Error 1
```

`GRID_OK` was not printed. `make ci` was not run locally: execution stopped at the scope blocker below.
No grid acceptance or browser verification is claimed. Review and merge remain pending.

The prescribed `git status` scope command printed nothing because incremental progress was already
committed. `git diff --name-only origin/main...HEAD -- . ':(exclude)docs'` confirmed the actual code scope:

```text
web/src/App.tsx
web/src/components/TaskGrid/TaskCardList.tsx
web/src/components/TaskGrid/TaskGrid.tsx
web/src/locales/en/grid.json
```

`git diff --check` passed. Only the task document changed after this Verification run.
`make doclint` then exited 0:

```text
./scripts/doclint.sh
🔍 2446 Total (in 228ms) 🔗 574 Unique ✅ 2418 OK 🚫 0 Errors 👻 28 Excluded
```

### Historical plan-repair evidence

Implementation Verification pending. When implementing the grid, replace the entire `## Evidence`
section, including all repair and superseded-attempt subsections, with fresh Verification and `make ci`
output. The following is plan-repair evidence only, not proof of grid rendering.

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

### Current baseline verification

On `49fb1fa`, before adding grid code, ran:

```bash
make lint && make typecheck && make test-web && echo GRID_OK
```

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

 RUN  v4.1.11 /home/paseo/.paseo/worktrees/0a6udotz/loop-t042-1-1789351229/web

GET http://localhost:3000/api/v1/auth/me 401 (Unauthorized)
GET http://localhost:3000/api/v1/auth/me 401 (Unauthorized)
GET http://localhost:3000/api/v1/auth/me 401 (Unauthorized)
GET http://localhost:3000/api/v1/auth/me 401 (Unauthorized)
GET http://localhost:3000/api/v1/auth/me 401 (Unauthorized)
GET http://localhost:3000/api/v1/auth/me 401 (Unauthorized)
GET http://localhost:3000/api/v1/auth/me 401 (Unauthorized)
GET http://localhost:3000/api/v1/auth/me 401 (Unauthorized)
GET http://localhost:3000/api/v1/auth/me 401 (Unauthorized)
GET http://localhost:3000/api/v1/auth/me 401 (Unauthorized)
GET http://localhost:3000/api/v1/auth/me 401 (Unauthorized)
POST http://localhost:3000/api/v1/auth/setup 409 (Conflict)
GET http://localhost:3000/api/v1/auth/me 401 (Unauthorized)
POST http://localhost:3000/api/v1/auth/login 401 (Unauthorized)
GET http://localhost:3000/api/v1/auth/me 401 (Unauthorized)
POST http://localhost:3000/api/v1/auth/login 429 (Too Many Requests)
GET http://localhost:3000/api/v1/auth/me 503 (Service Unavailable)

 Test Files  6 passed (6)
      Tests  89 passed (89)
   Start at  02:01:45
   Duration  2.34s (transform 961ms, setup 0ms, import 2.81s, tests 2.56s, environment 1.72s)

GRID_OK
```

This is baseline evidence only. No grid acceptance test exists yet. `npm ci --prefix web`
installed the pinned dependencies and reported two high-severity audit findings; no pins changed.

### Suite-count repair verification

The following contract check failed before the repair and passed afterward:

```bash
python3 - <<'PY'
from pathlib import Path
import re
p = Path('docs/tasks/T042-virtualised-task-grid.md').read_text()
verification = p.split('## Verification\n', 1)[1].split('## Out of scope', 1)[0]
existing = list(Path('web/src').rglob('*.test.ts')) + list(Path('web/src').rglob('*.test.tsx'))
required = re.findall(r'\| `(web/[^`]+\.test\.tsx?)` \| create \|', p)
expected = len(set(map(str, existing)) | set(required))
actual = int(re.search(r'Test Files  (\d+) passed', verification)[1])
print(f'existing={len(existing)}, required new={len(required)}, expected={expected}, documented={actual}', flush=True)
assert actual == expected, 'T042 suite count excludes its required new test file'
PY
```

```text
existing=6, required new=1, expected=7, documented=6
AssertionError: T042 suite count excludes its required new test file
```

```text
existing=6, required new=1, expected=7, documented=7
```

Fresh `npm ci --prefix web` installed the pinned dependencies and again reported two high-severity
findings; no pins changed. The Verification command exited 0 on the unchanged implementation:

```text
 Test Files  6 passed (6)
      Tests  89 passed (89)
   Start at  02:05:41
   Duration  2.34s (transform 862ms, setup 0ms, import 3.02s, tests 2.58s, environment 1.62s)

GRID_OK
```

This is baseline proof, not grid acceptance. `make ci` initially failed because it ran alongside the
Verification command and golangci-lint rejected concurrent execution. Rerunning sequentially with
`PATH="/tmp/t039-tools:$PATH" make ci` reused the existing Docker CLI and exited 0. Output excerpts:

```text
0 issues.
All matched files use Prettier code style!
 Test Files  6 passed (6)
      Tests  89 passed (89)
docker compose -f compose.yaml config -q
docker compose -f compose.yaml -f compose.dev.yaml config -q
./scripts/doclint.sh
🔍 2431 Total (in 223ms) 🔗 573 Unique ✅ 2404 OK 🚫 0 Errors 👻 27 Excluded
```

Only this task document changed. The task and both index rows remain `todo`.

### Keyboard ownership repair verification

The original worker stopped because §3.6 required unbuilt targets forbidden by T042's scope.
The following plan-contract check failed before the repair and passed afterward:

```bash
python3 - <<'PY'
from pathlib import Path

spec = Path('docs/09-web-ui-spec.md').read_text()
keyboard = spec.split('### 3.6 Keyboard\n', 1)[1].split('### 3.7', 1)[0]
assert 'Implementation ownership' in keyboard, 'Keyboard contract has no staged implementation ownership'
for task, test in [('T042', 'TestGridKeyboardNavigationAndSelection'),
                   ('T044', 'TestShellKeyboardActions'),
                   ('T048', 'TestKeyboardOpensFocusedTask')]:
    text = next(Path('docs/tasks').glob(task + '-*.md')).read_text()
    assert '| **Status** | todo |' in text
    assert test in text.split('## Acceptance criteria\n', 1)[1].split('## Verification', 1)[0], task
    assert '#36-keyboard' in text
    if task != 'T042':
        files = text.split('## Files\n', 1)[1].split('No other file', 1)[0]
        assert '`web/src/components/TaskGrid/TaskGrid.tsx` | edit |' in files, task
        assert '`web/src/components/TaskGrid/TaskGrid.test.tsx` | edit |' in files, task
index = Path('docs/tasks/00-task-index.md').read_text()
rows = [line for line in index.splitlines() if line.startswith(('| [T042]', '| T042 |'))]
assert len(rows) == 2 and all('| todo |' in line for line in rows)
print('KEYBOARD_PLAN_OK: ownership, integration scope, acceptance tests, T042 todo')
PY
```

```text
AssertionError: Keyboard contract has no staged implementation ownership
KEYBOARD_PLAN_OK: ownership, integration scope, acceptance tests, T042 todo
```

`npm ci --prefix web` succeeded with the unchanged pins and two high-severity audit findings.
The first CI invocation was interrupted by the command harness's 120-second timeout during Go tests.
Rerunning `PATH="/tmp/t039-tools:$PATH" make ci` with a longer timeout exited 0, reusing the existing
Docker CLI for compose validation. Output excerpts:

```text
0 issues.
All matched files use Prettier code style!
 Test Files  6 passed (6)
      Tests  89 passed (89)
docker compose -f compose.yaml config -q
docker compose -f compose.yaml -f compose.dev.yaml config -q
./scripts/doclint.sh
🔍 2445 Total (in 224ms) 🔗 573 Unique ✅ 2418 OK 🚫 0 Errors 👻 27 Excluded
```

`git diff --check` passed. Only doc 09 and the T042, T044 and T048 task documents changed.
No implementation or acceptance completion is claimed; all three tasks remain `todo`.

## Blocked

### Resolved: entrypoint test requires the replaced placeholder

`web/src/main.test.ts:60` asserts that the entire authenticated app text equals `Downloads`.
It mocks only `/auth/me`, not the task list. T042 replaces that placeholder with an API-backed grid;
the unchanged test now fails with `Loading tasks…`. Both this suite and `web/src/App.test.tsx` also
reject the newly required `/tasks` requests through MSW's strict unhandled-request policy.

The Files table now authorizes both suites; step 8 and the acceptance criteria require their integration
proof without weakening existing guarantees. No shared scope exception or production workaround is needed.

Recovery removes the unfinished scaffold from PR #151 through an appended commit and merges only this
plan repair. Neither test implementation changes here. T042 and both index rows remain `todo`, with
acceptance unchecked. Retry implementation from main; the discarded scaffold is not acceptance evidence.

### Resolved: keyboard actions require forbidden components

Step 6 requires the keyboard model in [doc 09 §3.6](../09-web-ui-spec.md#36-keyboard):
`Enter`/`F2` opens the focused task's detail pane; `Ctrl/Cmd+F` focuses the toolbar filter.
This task's Out of scope section forbids building the detail pane and toolbar, assigning them to
T048 and T044. Both depend on T042 in the current index. Neither component nor an action interface
exists in the current implementation: `web/src/App.tsx` renders an empty header and task placeholder.

Checked on the current main baseline with:

```bash
rg --files web/src/components
rg -n 'onKeyDown|keydown|detail|filter box|cheat|shortcut|remove' web/src/App.tsx web/src/components
```

The file list contains only `Auth/` and `ui/` components. The search finds authentication error-detail
strings and the generic context-menu shortcut primitive, not task keyboard actions or their targets.

The recovery-authorized plan repair assigns staged ownership in
[doc 09 §3.6](../09-web-ui-spec.md#36-keyboard). T044 and T048 now include grid integration scope and
acceptance tests; T104 retains the complete keyboard gate. This changes delivery order, not the final
keyboard requirement. No implementation was added. The task and both index rows remain `todo`;
acceptance boxes remain unchecked. The original worker ran only dependency installation, diff checks
and doclint before stopping. Fresh repair verification is recorded in Evidence; it is not T042 completion.

Resolved: the Verification suite count now includes the required new `TaskGrid.test.tsx`
without removing or excluding existing suites. No code was added; the task and both index rows
remain `todo`. Grid implementation and acceptance verification remain pending.

Previous blocker, resolved by the plan repair: [doc 09 §3.9](../09-web-ui-spec.md#39-virtualisation) now separates table
row density from fixed mobile card height while preserving no auto-measurement and the existing mobile
content and tap-target requirements. This repairs the missing exception, not the implementation.
T042 and both index rows remain `todo`; implementation Verification is still pending.
