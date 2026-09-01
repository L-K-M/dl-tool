# T049 — Build the add-task dialog and the file-selection step

| Field | Value |
|---|---|
| **ID** | T049 |
| **Milestone** | M3 |
| **Status** | todo |
| **Depends on** | T020, T031, T033, T044, T047, T048 |
| **Blocks** | T052, T104 |
| **Parallel-safe** | no — extends T044's `Toolbar.tsx` |
| **Implements** | — (renders [FR-001](../02-requirements.md#fr-001-add-tasks-from-a-batch-of-pasted-uris), [FR-005](../02-requirements.md#fr-005-add-tasks-from-an-uploaded-file), [FR-006](../02-requirements.md#fr-006-inspect-a-submission-before-committing-it) and [FR-009](../02-requirements.md#fr-009-supply-ftp-credentials-for-a-single-task), covered by T020, T031 and T033) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md) |
| **Est. size** | 3 new files, ~440 LOC. The selection step is the second page of the same dialog flow and cannot be reached without it. |

## Goal
`+ Add` opens Download Station's Create Download Task dialog: destination with `Select` and a free-space
line, a URI textarea with per-line badges, a dropzone for `.torrent` and `.txt`, the two verbatim
checkboxes, and `More options`. Ticking *Show dialog to select files for download* turns `Create` into
`Next` and opens the selection step over `POST /tasks/inspect`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/09-web-ui-spec.md` §4 Add-task dialog](../09-web-ui-spec.md#4-add-task-dialog) — the wireframe,
   the control table, the four behaviour rules and the drop predicate.
2. [`docs/09-web-ui-spec.md` §5 File-selection dialog](../09-web-ui-spec.md#5-file-selection-dialog) — the
   footer total, multi-item paging and the magnet metadata state.
3. [`docs/05-api-contract.md` §5.2 `POST /tasks`](../05-api-contract.md#52-post-tasks) — every body field,
   the multipart form and the partial-success response.
4. [`docs/05-api-contract.md` §5.3 `POST /tasks/inspect`](../05-api-contract.md#53-post-tasksinspect) — the
   manifest, `metadata_pending` and `rejected[]`.
5. [`docs/09-web-ui-spec.md` §10.6 Toasts and optimistic updates](../09-web-ui-spec.md#106-toasts-and-optimistic-updates)
   — a magnet is never added optimistically.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/AddTask/AddTaskDialog.tsx` | create | The create dialog, its dropzone and clipboard detection. |
| `web/src/components/AddTask/FileSelectionDialog.tsx` | create | The selection step over T048's `FileTree`. |
| `web/src/components/AddTask/AddTaskDialog.test.tsx` | create | Badges, uploads, drops, submission bodies. |
| `web/src/components/Shell/Toolbar.tsx` | edit | Wire the `+ Add` split button to the dialog. |
| `web/src/locales/en/dialogs.json` | edit | Add-dialog and selection-step strings. |

No other file may be modified.

## Interface contract

```tsx
// web/src/components/AddTask/AddTaskDialog.tsx
export interface AddTaskDraft {
  uris: string[];
  files: File[];                     // .torrent and .metalink parts; a .txt is expanded client-side
  destination: string;
  category: string | null;
  tags: string[];
  paused: boolean;
  sequential: boolean;
  create_subfolder: boolean;
  ftp_credentials: { username: string; password: string } | null;
  extract_password: string | null;
  dl_limit: number;                  // bytes per second, 0 = unlimited
  ul_limit: number;
}

/** Per-line badge of doc 09 §4: a recognised scheme, an unrecognised one, or a duplicate of a task
 *  already in the store, matched on normalised URI or infohash. */
export type LineBadge = 'ok' | 'unknown' | 'duplicate';
export function classifyLine(line: string, known: ReadonlySet<string>): LineBadge;

/** qBittorrent's own drop predicate, doc 09 §4 behaviour 2. Both tests are case-insensitive. */
export function isDroppableText(str: string): boolean;

export function AddTaskDialog(props: { open: boolean; onOpenChange: (o: boolean) => void;
  initialUris?: string[] }): JSX.Element;
```

```tsx
// web/src/components/AddTask/FileSelectionDialog.tsx
export interface Manifest {
  source_uri: string; kind: string; name: string;
  total_size: number | null; file_count: number | null; metadata_pending: boolean;
  infohash_v1: string | null; infohash_v2: string | null;
  files: { index: number; path: string; size: number | null }[] | null;
}

export function FileSelectionDialog(props: {
  manifests: Manifest[];
  draft: AddTaskDraft;
  onBack: () => void;
  onCreate: (draft: AddTaskDraft,
             selection: { index: number; selected: boolean; priority: string }[]) => void;
}): JSX.Element;
```

Verbatim labels, which must not be reworded: `Authentication required (for ftp:// only)`,
`Show dialog to select files for download`, `Save in subfolder named after the task`, and the tooltip
*"The subfolder will be named as the same list name displayed here."*

Submission: JSON `POST /api/v1/tasks` when only URIs are present; the multipart form of doc 05 §5.2 with a
`payload` part plus one `file` part per uploaded `.torrent` otherwise. A `.txt` is read client-side and its
lines appended to the textarea, so it never leaves the browser as a file part.

## Steps
1. Create `AddTaskDialog.tsx` on shadcn/ui's `dialog` with the exact field order of the doc 09 §4
   wireframe, `More options` collapsed by default.
2. Destination is read-only text plus a `Select` button opening T047's `FolderBrowserDialog`, with the free
   space line refreshed from `GET /fs/free-space?path=` on every change, and the last destination taken
   from `useUiPrefs.lastDestination` when the setting allows.
3. Implement `classifyLine` and render the gutter badge and the `12 / 50` counter. Over fifty lines show
   `62 / 50 — the first 50 will be created` and offer *Split into batches*; never truncate silently.
4. Implement the dropzone with native `dragover`/`drop` plus a hidden `<input type="file" multiple>`
   accepting `.torrent` and `.txt`; reject `.nzb` with `NZB is not supported in v1` and abort the whole
   drop with a toast when a directory is dropped.
5. Implement `isDroppableText` exactly as doc 09 §4 behaviour 2 specifies, and use it for window drops, for
   paste onto the grid, and for the clipboard toast on window `focus`. Never open a modal from the
   clipboard; store the last handled value in `sessionStorage`.
6. Disable the FTP credentials block unless a line starts with `ftp://`, and disable the file-selection
   checkbox unless a `.torrent` file or a magnet is present, each with the doc 09 §4 tooltip.
7. On `Next`, post `POST /tasks/inspect` and open `FileSelectionDialog` with the returned manifests; a
   `metadata_pending` manifest offers *add paused* instead of a selection.
8. Create `FileSelectionDialog.tsx` wrapping T048's `FileTree`, with `All` / `None` / `Invert` acting on the
   filtered set, the footer running total, `◂ Torrent i of n ▸` paging, and `Create` disabled while the
   wanted size exceeds the free space.
9. Submit through the T014 client; close the dialog immediately and show optimistic `queued` rows for URI
   submissions only, rolling back and naming the offending URI on failure; render `rejected[]` entries as
   one toast each.
10. Edit `Toolbar.tsx` so `+ Add` opens the dialog, with the menu items *Add URLs…*, *Add .torrent file…*
    and *Add from clipboard*.
11. Create `AddTaskDialog.test.tsx`: badge classification for a magnet, a bare 40-hex infohash, a
    32-character base32 infohash and rubbish; a `.txt` drop appends its lines; a `.nzb` drop is refused; the
    JSON body carries `paused`, `sequential`, `create_subfolder` and byte-per-second limits; a `.torrent`
    upload produces a multipart request with a `payload` part; `Ctrl+Enter` submits and `Enter` does not.
12. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestClassifyLineBadges`, `TestIsDroppableTextPredicate` and `TestTxtDropAppendsLines` pass.
- [ ] `TestJsonSubmissionBody` and `TestMultipartSubmissionForTorrent` pass.
- [ ] `TestFiftyLineSoftWarning` asserts the counter text and that no line is dropped.
- [ ] The three verbatim labels appear character for character as doc 09 §4 gives them.
- [ ] Limits are entered and sent in bytes per second; no field is labelled KB/s.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo ADD_DIALOG_OK
```
Expected: Vitest reports `Test Files  10 passed (10)` including
`src/components/AddTask/AddTaskDialog.test.tsx`, every test named above appears as passing, and the final
line of stdout is exactly `ADD_DIALOG_OK`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT parse a `.torrent` in the browser, and do NOT add `parse-torrent`; `POST /tasks/inspect` does it.
- Do NOT add a watch-folder control here; M6 owns watch folders.
- Do NOT create a category from this dialog beyond selecting an existing one plus the inline create that
  posts `POST /categories` (T050); no category management UI lives here.
- Do NOT add optimistic rows for magnet submissions or for uploads.
- Do NOT accept `.nzb`, `ed2k:` or a file-hosting premium field; all three are out of v1 scope.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
