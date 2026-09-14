# T041 — Build the live task store and the locale-aware formatters

| Field | Value |
|---|---|
| **ID** | T041 |
| **Milestone** | M3 |
| **Status** | done |
| **Depends on** | T014, T025, T039 |
| **Blocks** | T042, T044, T051, T063 |
| **Parallel-safe** | yes — touches only `web/src/store/` and `web/src/lib/` |
| **Implements** | — (client half of [FR-016](../02-requirements.md#fr-016-stream-task-changes-as-rid-deltas-over-sse) and [FR-017](../02-requirements.md#fr-017-serve-the-identical-delta-payload-by-polling), both covered by T025) |
| **Decisions** | [ADR-0006](../decisions/0006-sse-with-rid-deltas.md) |
| **Est. size** | 4 new files, ~300 LOC. The two modules ship together because no grid cell can render without both. |

## Goal
One `zustand` store holds every task as a `Map<string, Task>` and applies a `sync` payload — full update,
delta, removal or `seq_gap` — in a single reducer. Every byte, rate, duration, ratio and date reaching the
UI is formatted through `Intl`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §6.1 `GET /events`](../05-api-contract.md#61-get-events) — the payload table
   and the client rules at the end of the section.
2. [`docs/05-api-contract.md` §3 The canonical Task object](../05-api-contract.md#3-the-canonical-task-object)
   — every field, its nullability, and the rule that a delta carries only changed fields.
3. [`docs/09-web-ui-spec.md` §3.9 Virtualisation](../09-web-ui-spec.md#39-virtualisation) — merge into a
   `Map`, never rebuild the array identity per tick.
4. [`docs/09-web-ui-spec.md` §10.2 i18n](../09-web-ui-spec.md#102-i18n) — `Intl` only, no hand-rolled
   formatters.
5. [`docs/02-requirements.md` FR-013](../02-requirements.md#fr-013-resolve-the-sidebar-filter-sets) — the
   filter sets the count selector reproduces.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/store/useTasks.ts` | create | The task map, the reducer, selection and the selectors. |
| `web/src/store/useTasks.test.ts` | create | Reducer transitions, exactly the five cases in doc 13 §6. |
| `web/src/lib/format.ts` | create | `Intl`-backed byte, rate, duration, ratio and date formatting. |
| `web/src/lib/format.test.ts` | create | Formatter output, including the null and zero renderings. |

No other file may be modified.

## Interface contract

```ts
// web/src/store/useTasks.ts
import type { paths } from '../api/schema';

export type Task = paths['/tasks/{id}']['get']['responses'][200]['content']['application/json'];
export type SyncMessage = paths['/sync']['get']['responses'][200]['content']['application/json'];
export type Stats = SyncMessage['stats'];
export type SidebarFilter =
  'all' | 'downloading' | 'completed' | 'active' | 'inactive' | 'stopped' | 'error';

export interface TasksState {
  rid: number;
  tasks: ReadonlyMap<string, Task>;
  stats: Stats;
  selection: ReadonlySet<string>;
  /** Written only by T051's transport; the status bar and the toolbar read it. */
  connection: 'connecting' | 'live' | 'polling' | 'offline';
  /** The only writer of live task state. See the merge rules below. */
  applySync: (msg: SyncMessage) => void;
  /** Seeds the map from a GET /tasks page without touching rid. */
  hydrate: (tasks: Task[]) => void;
  setSelection: (ids: Iterable<string>) => void;
  clearSelection: () => void;
  setConnection: (c: TasksState['connection']) => void;
  reset: () => void;
}

export const useTasks: import('zustand').UseBoundStore<import('zustand').StoreApi<TasksState>>;

export const selectTask = (id: string) => (s: TasksState): Task | undefined => s.tasks.get(id);
export const selectStats = (s: TasksState): Stats => s.stats;
export const selectFilterCounts = (s: TasksState): Record<SidebarFilter, number>;
export const selectCategoryCounts = (s: TasksState): Map<string | null, number>;
export const selectTagCounts = (s: TasksState): Map<string, number>;
```

Merge rules, from doc 05 §6.1:

| Condition | Effect |
|---|---|
| `full_update === true` or `seq_gap === true` | Replace the map with `msg.tasks`, then set `rid`. |
| otherwise | For each entry, `next.set(id, {...prev.get(id), ...patch})`; an unknown id is inserted verbatim. |
| always | Delete every id in `tasks_removed`, drop it from `selection`, store `stats` and `rid`. |

A new `Map` is constructed per tick, but untouched `Task` object identities are preserved so only changed
rows re-render.

```ts
// web/src/lib/format.ts
/** Divisor 1024 with Intl short unit labels, which is what produces doc 09 §2.2's `412 GB`
 *  for 442381537280 bytes. `null` and 0 render as the em dash `—`. */
export function formatBytes(bytes: number | null, locale?: string): string;
/** `12.4 MB/s` via Intl unit `megabyte-per-second`; 0 renders as `—`. */
export function formatRate(bytesPerSecond: number, locale?: string): string;
/** `6m 12s`; `null` renders as `∞`. */
export function formatEta(seconds: number | null, locale?: string): string;
/** Two fraction digits; above 9999 renders as `∞`. */
export function formatRatio(ratio: number, locale?: string): string;
/** `78.4%` from a 0..1 progress value. */
export function formatPercent(progress: number, locale?: string): string;
/** Intl.RelativeTimeFormat under 7 days, Intl.DateTimeFormat after. */
export function formatWhen(rfc3339: string, now?: Date, locale?: string): string;
/** Localized absolute timestamp tooltip per doc 09 §10.2. */
export function formatAbsolute(rfc3339: string, locale?: string): string;
```

## Steps
1. Create `web/src/lib/format.ts` with the seven functions above. Use `Intl.NumberFormat`,
   `Intl.DateTimeFormat` and `Intl.RelativeTimeFormat` only; write no manual unit table beyond choosing
   which `Intl` unit name applies at each magnitude.
2. Create `web/src/lib/format.test.ts` asserting: `formatBytes(442381537280)` is `412 GB`;
   `formatBytes(null)` and `formatBytes(0)` are `—`; `formatRate(0)` is `—`; `formatEta(null)` is `∞`;
   `formatRatio(10000)` is `∞`; and these assertions pass with the locale forced to `en`.
   Add `TestFormatAbsoluteUsesLocale`: for the same RFC 3339 input, assert that `formatAbsolute`
   in `en` and in `de` each match `Intl.DateTimeFormat` with the options in doc 09 §10.2,
   and that the two `formatAbsolute` outputs differ from each other.
3. Create `web/src/store/useTasks.ts` with the state, the reducer and the selectors above, using
   `zustand`'s `create` with no middleware.
4. Implement `applySync` exactly as the merge table specifies, and keep it pure — no fetch, no timer.
5. Implement `selectFilterCounts` over the map using the sets in FR-013, and the category and tag count
   selectors from `task.category` and `task.tags`.
6. Create `web/src/store/useTasks.test.ts` covering the five cases doc 13 §6 requires: a full snapshot; an
   incremental delta that preserves unchanged fields; a removal that also clears the selection; an
   out-of-order `rid` carrying `seq_gap: true` that replaces the map; and a second full update after a
   reconnect. Assert object identity is preserved for a task no delta touched.
7. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [x] `TestApplySyncFullUpdateReplacesMap`, `TestApplySyncDeltaMergesFields`,
      `TestApplySyncRemovesTasksAndSelection`, `TestSeqGapReplacesMap` and
      `TestUnchangedTaskKeepsIdentity` all pass.
- [x] `TestFormatBytesMatchesSpecExamples`, `TestNullAndZeroRenderings` and
      `TestFormatAbsoluteUsesLocale` pass.
- [x] No file in this task imports `EventSource`, `fetch` or the `api` client.
- [x] `selectFilterCounts` returns all seven keys, including zero counts.
- [x] `connection` starts at `'connecting'` and is changed only through `setConnection`.
- [x] `applySync` is the only exported function that writes `tasks`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo STORE_OK
```
Expected: Vitest reports `Test Files  6 passed (6)` including `src/store/useTasks.test.ts` and
`src/lib/format.test.ts`, every test named above appears as passing, and the final line of stdout is
exactly `STORE_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT open the event stream, implement reconnect or poll `GET /sync`; T051 owns the transport.
- Do NOT render anything; this task ships no component.
- Do NOT recompute sidebar filter membership for the grid's row set — the server resolves it through
  `GET /tasks?state=`; the selectors here feed the sidebar counts only.
- Do NOT persist any part of the store; T045 owns the preference document.
- Do NOT hand-roll a byte or date formatter, and do NOT add `date-fns`, `dayjs` or `numeral`.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
Installed dependencies with `npm ci --prefix web`. Code is limited to the four Files paths;
no dependency or generated-file changes.

Acceptance coverage:
- Reducer transitions and identity: the five named tests plus `TestReconnectFullUpdateReplacesMap`.
- Snapshot selection cleanup: `TestSnapshotPrunesMissingSelection` covers both replacement flags.
- Formatters: the three named tests plus magnitude, duration and relative-date boundary tests.
- Import boundary and sole task writer: `TestStoreHasNoTransportImportsOrExtraTaskWriters`.
- All filter keys and memberships: `TestSidebarCountsCoverEveryFilterAndState`.
- Connection ownership: `TestConnectionChangesOnlyThroughSetter`.
- Hydration/reset delegation: `TestHydrateAndResetDelegateTaskWritesToApplySync`.

Final tree, including both review fixes, passed the exact Verification command (exit 0).
Before the source-path fix, the store suite launched from the repository root failed with
`ENOENT: no such file or directory, open 'src/store/useTasks.ts'`. All 12 store tests now pass
from both the repository root (`vitest --root web`) and `web/`.

Before snapshot selection cleanup, `TestSnapshotPrunesMissingSelection` failed with
`expected Set{ 'gone', 'kept' } to deeply equal Set{ 'kept' }`.
The fix prunes absent selections only on authoritative replacement; delta removal behavior remains
covered. Deferred presentation/identity suggestions and rejected findings are recorded in PR #147.

Verification output:

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

 RUN  v4.1.11 /home/paseo/.paseo/worktrees/0a6udotz/loop-t041-1-1789344293/web

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
   Start at  00:45:52
   Duration  2.31s (transform 781ms, setup 0ms, import 2.71s, tests 2.58s, environment 1.69s)

STORE_OK
```

Named assertions, `cd web && npx vitest run src/store/useTasks.test.ts src/lib/format.test.ts --reporter=verbose`
(exit 0):

```text
 RUN  v4.1.11 /home/paseo/.paseo/worktrees/0a6udotz/loop-t041-1-1789344293/web

 ✓ src/lib/format.test.ts > TestFormatBytesMatchesSpecExamples 31ms
 ✓ src/lib/format.test.ts > TestNullAndZeroRenderings 1ms
 ✓ src/lib/format.test.ts > TestFormatAbsoluteUsesLocale 8ms
 ✓ src/lib/format.test.ts > TestMagnitudeBoundariesAndLocales 6ms
 ✓ src/lib/format.test.ts > TestDurationRatioAndPercentUseIntl 2ms
 ✓ src/lib/format.test.ts > TestRelativeDateUnitsAndSevenDayBoundary 7ms
 ✓ src/store/useTasks.test.ts > TestApplySyncFullUpdateReplacesMap 3ms
 ✓ src/store/useTasks.test.ts > TestApplySyncDeltaMergesFields 1ms
 ✓ src/store/useTasks.test.ts > TestApplySyncRemovesTasksAndSelection 1ms
 ✓ src/store/useTasks.test.ts > TestSeqGapReplacesMap 0ms
 ✓ src/store/useTasks.test.ts > TestUnchangedTaskKeepsIdentity 1ms
 ✓ src/store/useTasks.test.ts > TestReconnectFullUpdateReplacesMap 0ms
 ✓ src/store/useTasks.test.ts > TestSnapshotPrunesMissingSelection 0ms
 ✓ src/store/useTasks.test.ts > TestSidebarCountsCoverEveryFilterAndState 1ms
 ✓ src/store/useTasks.test.ts > TestCategoryAndTagCounts 1ms
 ✓ src/store/useTasks.test.ts > TestConnectionChangesOnlyThroughSetter 2ms
 ✓ src/store/useTasks.test.ts > TestHydrateAndResetDelegateTaskWritesToApplySync 1ms
 ✓ src/store/useTasks.test.ts > TestStoreHasNoTransportImportsOrExtraTaskWriters 35ms

 Test Files  2 passed (2)
      Tests  18 passed (18)
   Start at  00:45:55
   Duration  973ms (transform 187ms, setup 0ms, import 619ms, tests 105ms, environment 429ms)
```

Scope: incremental commits leave the working tree clean. Copied the four committed implementation
files into a disposable worktree at baseline `e455ee2` and ran the exact `git status` scope command
there, preserving the task branch and its reviewed ancestry:

```text
web/src/lib/format.test.ts
web/src/lib/format.ts
web/src/store/useTasks.test.ts
web/src/store/useTasks.ts
```

`PATH=/tmp/t039-tools:$PATH make ci` exited 0 using the existing Docker CLI. Output:

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
go vet ./...
cd web && npx tsc --noEmit -p tsconfig.json
go test -race -count=1 ./...
?   	github.com/L-K-M/dl-tool/cmd/dl-tool	[no test files]
ok  	github.com/L-K-M/dl-tool/internal/api	87.782s
ok  	github.com/L-K-M/dl-tool/internal/config	1.125s
ok  	github.com/L-K-M/dl-tool/internal/engine	21.412s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.211s
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	8.946s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.019s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.479s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.189s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.073s
ok  	github.com/L-K-M/dl-tool/internal/store	70.927s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.396s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.083s
?   	github.com/L-K-M/dl-tool/web/node_modules/flatted/golang/pkg/flatted	[no test files]
cd web && npx vitest run

 RUN  v4.1.11 /home/paseo/.paseo/worktrees/0a6udotz/loop-t041-1-1789344293/web

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
   Start at  00:47:40
   Duration  2.24s (transform 549ms, setup 0ms, import 2.49s, tests 2.54s, environment 1.63s)

docker compose -f compose.yaml config -q
docker compose -f compose.yaml -f compose.dev.yaml config -q
./scripts/doclint.sh
🔍 2425 Total (in 222ms) 🔗 572 Unique ✅ 2399 OK 🚫 0 Errors 👻 26 Excluded
```

<details>
<summary>Historical plan-repair evidence, before implementation</summary>

Absolute-date plan repair only, not implementation evidence. This contract check failed before
repair with `AssertionError: Added tooltip contradicts Intl-only date requirement` and passed after:

```bash
python3 - <<'PY'
from pathlib import Path

ui = Path('docs/09-web-ui-spec.md').read_text()
task = Path('docs/tasks/T041-task-store-and-formatters.md').read_text()
contract = task.split('## Evidence')[0]
added = next(line for line in ui.splitlines() if '| `addedOn` |' in line)
assert 'tooltip is the RFC 3339 string' not in added, 'Added tooltip contradicts Intl-only date requirement'
assert 'localized absolute' in added
assert "dateStyle: 'full', timeStyle: 'long'" in ui
assert 'absolute RFC 3339 rendering' not in contract
assert 'TestFormatAbsoluteUsesLocale' in contract
index = Path('docs/tasks/00-task-index.md').read_text()
rows = [line for line in index.splitlines() if line.startswith(('| [T041]', '| T041 |'))]
assert len(rows) == 2 and all('| todo |' in line for line in rows)
assert '| **Status** | todo |' in task
print('DATE_CONTRACT_OK; T041 remains todo in task and both index rows')
PY
```

```text
DATE_CONTRACT_OK; T041 remains todo in task and both index rows
```

After `npm ci --prefix web`, `PATH=/tmp/t039-tools:$PATH make ci` exited 0
using the existing Docker CLI. An initial 120-second tool timeout interrupted Go tests;
the complete rerun passed. The same checks passed after clarifying the locale-test assertions.
Latest excerpts:

```text
0 issues.
All matched files use Prettier code style!
 Test Files  4 passed (4)
      Tests  71 passed (71)
docker compose -f compose.yaml config -q
docker compose -f compose.yaml -f compose.dev.yaml config -q
./scripts/doclint.sh
🔍 2425 Total (in 235ms) 🔗 572 Unique ✅ 2399 OK 🚫 0 Errors 👻 26 Excluded
```

Task Verification remains pending: the two T041 suites do not exist yet.

Previous plan repair only, not T041 implementation evidence. After `npm ci --prefix web`,
comparing the existing suites plus the Files table against Verification reproduced:

```text
existing=4, required=2, total=6, documented=5
AssertionError: T041 verification count contradicts existing + required suites
```

The same check after the count correction passed:

```text
existing=4, required=2, total=6, documented=6
```

`make ci` exited 0 with `/tmp/t039-tools` on `PATH` for the existing Docker CLI.
The first run without it stopped at `compose-check` (`docker: command not found`).
Successful rerun excerpts:

```text
0 issues.
All matched files use Prettier code style!
 Test Files  4 passed (4)
      Tests  71 passed (71)
docker compose -f compose.yaml config -q
docker compose -f compose.yaml -f compose.dev.yaml config -q
./scripts/doclint.sh
🔍 2422 Total (in 216ms) 🔗 572 Unique ✅ 2396 OK 🚫 0 Errors 👻 26 Excluded
```

Task Verification remains pending until implementation adds the required suites.

</details>

## Blocked
No current blockers. Resolved planning history follows.

### Absolute-date contract (resolved)

On origin/main `e4a0cb8`, the RFC 3339 tooltip contract contradicted the Intl-only
requirement. This plan repair makes the tooltip follow
[UI §10.2](../09-web-ui-spec.md#102-i18n), preserving
[NFR-008](../02-requirements.md#nfr-008-ship-translation-plumbing-with-english-only)
without a passthrough exception or a hand-rolled formatter. The task contract and
required locale test now match. No implementation; T041 remains `todo`.

### Previous: suite count (resolved)

Resolved by this plan repair: Verification now includes all four existing suites and both
required suites. T041 remains unimplemented and `todo`.

Original blocker on origin/main `6a08ec9`: Verification required five passing test files,
but the baseline already had four:

- `web/src/main.test.ts`
- `web/src/api/client.test.ts`
- `web/src/lib/theme.test.ts`
- `web/src/App.test.tsx`

Adding both required suites makes six. The correction changes only the stale count;
no suite is deleted, skipped or excluded.

Original worker baseline: after `npm ci --prefix web`, `make test-web` exited 0.
Output excerpt:

```text
 Test Files  4 passed (4)
      Tests  71 passed (71)
   Start at  22:41:32
   Duration  2.30s (transform 556ms, setup 0ms, import 1.97s, tests 2.46s, environment 1.09s)
```

No implementation changes. Acceptance and both index rows remain unchanged.
The original worker did not run task Verification or `make ci`; this is baseline evidence only.
