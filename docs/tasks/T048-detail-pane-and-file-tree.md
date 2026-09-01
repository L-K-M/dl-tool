# T048 — Build the detail pane, its tabs and the file tree

| Field | Value |
|---|---|
| **ID** | T048 |
| **Milestone** | M3 |
| **Status** | todo |
| **Depends on** | T024, T032, T034, T035, T042 |
| **Blocks** | T049, T052, T104 |
| **Parallel-safe** | no — it also edits the shared files `web/src/App.tsx`, `web/src/locales/en/common.json` |
| **Implements** | — (renders [FR-018](../02-requirements.md#fr-018-manage-trackers-and-list-peers-for-bittorrent-tasks), [FR-044](../02-requirements.md#fr-044-report-the-effective-destination) and [FR-150](../02-requirements.md#fr-150-record-a-per-task-event-log), covered by T034, T083 and T024) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md) |
| **Est. size** | 3 new files, ~420 LOC. The file tree ships here because the Files tab and T049's selection step are one component. |

## Goal
Selecting a row opens a resizable bottom pane with the six tabs of doc 09 §6, live from the store and from
the per-task endpoints. The Files tab renders a tri-state tree whose priority select carries exactly
`Skip`, `Normal`, `High` and `Maximum`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/09-web-ui-spec.md` §6 Detail pane](../09-web-ui-spec.md#6-detail-pane) — the six tabs and every
   field each one carries.
2. [`docs/09-web-ui-spec.md` §5 File-selection dialog](../09-web-ui-spec.md#5-file-selection-dialog) — the
   tri-state rule, the four priorities and the running total.
3. [`docs/05-api-contract.md` §5.8 files](../05-api-contract.md#58-get-tasksidfiles-and-patch-tasksidfiles),
   [§5.9 Trackers and peers](../05-api-contract.md#59-trackers-and-peers) and
   [§5.10 `GET /tasks/{id}/events`](../05-api-contract.md#510-get-tasksidevents).
4. [`docs/09-web-ui-spec.md` §10.6 Toasts and optimistic updates](../09-web-ui-spec.md#106-toasts-and-optimistic-updates)
   — file selection is optimistic, force recheck is not.
5. [`docs/09-web-ui-spec.md` §10.4 Accessibility](../09-web-ui-spec.md#104-accessibility) — the tablist and
   tree roles.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/DetailPane/DetailPane.tsx` | create | The pane, its tablist and the six panels. |
| `web/src/components/FileTree/FileTree.tsx` | create | The tri-state, prioritised file tree. |
| `web/src/components/DetailPane/DetailPane.test.tsx` | create | Tab visibility, field rendering and tree behaviour. |
| `web/src/App.tsx` | edit | Mount the pane under the grid. |
| `web/src/locales/en/common.json` | edit | Tab labels and detail field labels. |

No other file may be modified.

## Interface contract

```tsx
// web/src/components/DetailPane/DetailPane.tsx
export type DetailTab = 'general' | 'transfer' | 'trackers' | 'peers' | 'files' | 'log';

/** Tabs that do not apply to the selected task are hidden, never disabled.
 *  trackers and peers require source_kind of 'magnet' or 'torrent'. */
export function visibleTabs(task: Task): DetailTab[];

export function DetailPane(): JSX.Element;
```

```tsx
// web/src/components/FileTree/FileTree.tsx
export type FilePriority = 'skip' | 'normal' | 'high' | 'maximum';

export interface FileNode {
  index: number | null;      // null for a folder
  path: string;              // relative to the task destination
  name: string;
  size: number | null;
  progress?: number;
  selected: boolean | 'mixed';
  priority: FilePriority | null;   // null for an aria2 task, which has no priorities
  children?: FileNode[];
}

export interface FileTreeProps {
  nodes: FileNode[];
  /** Read-only for a single-file HTTP or FTP task. */
  readOnly?: boolean;
  filter?: string;                       // substring, plus ext:mkv and >100MB
  onChange: (changes: { index: number; selected?: boolean; priority?: FilePriority }[]) => void;
}

/** Builds the tree from a flat files[] list, folding common path prefixes into folders. */
export function buildTree(files: { index: number; path: string; size_bytes: number | null;
  selected: boolean; priority: FilePriority | null; progress?: number }[]): FileNode[];

export function FileTree(props: FileTreeProps): JSX.Element;
```

The priority select carries exactly four options in this order, and there is no `Low`:

| Label | Wire value |
|---|---|
| Skip | `skip` |
| Normal | `normal` |
| High | `high` |
| Maximum | `maximum` |

`selected: false` and `priority: 'skip'` are one concept: setting either sets both, and the checkbox and
the select can never disagree. Folder rows use a real `<input type="checkbox">` whose `.indeterminate` is
assigned imperatively, or the shadcn/ui `checkbox` with `checked="indeterminate"`.

## Steps
1. Create `FileTree.tsx` with `buildTree`, the tri-state checkbox, the priority select and the filter
   including the `ext:` and `>size` shortcuts. Rows are `role="treeitem"` with `aria-expanded`,
   `aria-level` and `aria-checked="true|false|mixed"` inside a `role="tree"`.
2. Render the footer running total, `aria-live="polite"`, as
   `Selected N of M files · X of Y wanted · Z free`.
3. Create `DetailPane.tsx` on shadcn/ui's `tabs`: `role="tablist"` with roving tabindex, the selected tab
   persisted through `useUiPrefs.patch({ detailTab })`, and a drag handle sizing the pane from 160 px to
   70 % of the viewport.
4. General: every field of doc 09 §6, with `requested_destination` shown only when it differs from
   `destination`, and the five-dot health indicator derived from `connected_seeders` and `total_peers`.
5. Transfer: the progress bar, the byte and rate fields, the limits, the queue position, extraction
   progress while `state = "extracting"`, and a 60-second inline `<svg><polyline>` sparkline of both rates.
6. Trackers and Peers: the grids of doc 09 §6 from `GET /tasks/{id}/trackers` and `/peers`, hidden for
   non-BitTorrent tasks; add and remove trackers through the same endpoints.
7. Files: `FileTree` fed by `GET /tasks/{id}/files`, changes sent optimistically through
   `PATCH /tasks/{id}/files`, read-only for a single-file HTTP or FTP task.
8. Log: `GET /tasks/{id}/events`, newest first, each row an absolute timestamp with a relative tooltip, a
   level badge, the code and the message, plus a `Copy all` button.
9. With more than one row selected, render the aggregate line `N tasks · X · ↓ Y` and no tabs; with nothing
   selected, collapse to the muted line of doc 09 §6.
10. Create `DetailPane.test.tsx`: `visibleTabs` hides Trackers and Peers for an `http` task; General shows
    `requested_destination` only when it differs; a folder checkbox toggles every descendant; setting
    `Skip` unticks the row and unticking sets `Skip`; a priority change posts the `PATCH` body of doc 05
    §5.8.
11. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestVisibleTabsHidesBitTorrentTabs`, `TestRequestedDestinationOnlyWhenDifferent` pass.
- [ ] `TestFolderCheckboxSetsDescendants` and `TestSkipAndUnselectAreOneConcept` pass.
- [ ] `TestPriorityPatchBody` asserts `{"files":[{"index":0,"priority":"high"}]}`.
- [ ] The priority select offers exactly four options and never the integer values.
- [ ] The Log tab lists newest first and renders the event `code` as its own column.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo DETAIL_OK
```
Expected: Vitest reports `Test Files  9 passed (9)` including
`src/components/DetailPane/DetailPane.test.tsx`, every test named above appears as passing, and the final
line of stdout is exactly `DETAIL_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT build the file-selection dialog or its multi-item paging; T049 wraps this tree for that.
- Do NOT add a `Low` priority or expose the integers `0`, `1`, `6`, `7` in the UI.
- Do NOT make force recheck or remove-with-data optimistic.
- Do NOT add a pieces map, availability bar or any field doc 09 §3.1 defers to v2.
- Do NOT poll a per-task endpoint on a timer; refetch on selection change and on the task's own delta.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
