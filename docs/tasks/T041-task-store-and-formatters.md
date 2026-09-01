# T041 — Build the live task store and the locale-aware formatters

| Field | Value |
|---|---|
| **ID** | T041 |
| **Milestone** | M3 |
| **Status** | todo |
| **Depends on** | T014, T025, T039 |
| **Blocks** | T042, T044, T048, T051 |
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
/** The absolute RFC 3339 rendering used in every tooltip. */
export function formatAbsolute(rfc3339: string, locale?: string): string;
```

## Steps
1. Create `web/src/lib/format.ts` with the seven functions above. Use `Intl.NumberFormat`,
   `Intl.DateTimeFormat` and `Intl.RelativeTimeFormat` only; write no manual unit table beyond choosing
   which `Intl` unit name applies at each magnitude.
2. Create `web/src/lib/format.test.ts` asserting: `formatBytes(442381537280)` is `412 GB`;
   `formatBytes(null)` and `formatBytes(0)` are `—`; `formatRate(0)` is `—`; `formatEta(null)` is `∞`;
   `formatRatio(10000)` is `∞`; and every assertion passes with the locale forced to `en`.
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
- [ ] `TestApplySyncFullUpdateReplacesMap`, `TestApplySyncDeltaMergesFields`,
      `TestApplySyncRemovesTasksAndSelection`, `TestSeqGapReplacesMap` and
      `TestUnchangedTaskKeepsIdentity` all pass.
- [ ] `TestFormatBytesMatchesSpecExamples` and `TestNullAndZeroRenderings` pass.
- [ ] No file in this task imports `EventSource`, `fetch` or the `api` client.
- [ ] `selectFilterCounts` returns all seven keys, including zero counts.
- [ ] `connection` starts at `'connecting'` and is changed only through `setConnection`.
- [ ] `applySync` is the only exported function that writes `tasks`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo STORE_OK
```
Expected: Vitest reports `Test Files  5 passed (5)` including `src/store/useTasks.test.ts` and
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
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
