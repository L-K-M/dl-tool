# T049 — Build the add-task dialog and the file-selection step

| Field | Value |
|---|---|
| **ID** | T049 |
| **Milestone** | M3 |
| **Status** | done |
| **Depends on** | T020, T031, T033, T044, T047, T048, T050 |
| **Blocks** | T052, T104 |
| **Parallel-safe** | no — extends T044's `Toolbar.tsx` and `Shell.test.tsx` |
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
5. [`docs/05-api-contract.md` §5.5 `PATCH /tasks/{id}`](../05-api-contract.md#55-patch-tasksid) —
   `dl_limit`/`ul_limit` are patched onto each created task; the create body has no limit fields.
6. [`docs/05-api-contract.md` §8.1 Categories](../05-api-contract.md#81-categories) and
   [§8.2 Tags](../05-api-contract.md#82-tags) — the combobox reads `GET /categories`, its inline create
   posts `POST /categories` and the Tags control seeds from `GET /tags`.
7. [`docs/09-web-ui-spec.md` §10.6 Toasts and optimistic updates](../09-web-ui-spec.md#106-toasts-and-optimistic-updates)
   — a magnet is never added optimistically.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/AddTask/AddTaskDialog.tsx` | create | The create dialog, its dropzone and clipboard detection. |
| `web/src/components/AddTask/FileSelectionDialog.tsx` | create | The selection step over T048's `FileTree`. |
| `web/src/components/AddTask/AddTaskDialog.test.tsx` | create | Badges, uploads, drops, submission bodies. |
| `web/src/components/Shell/Toolbar.tsx` | edit | Wire the `+ Add` split button to the dialog. |
| `web/src/components/Shell/Shell.test.tsx` | edit | Update the `+ Add` placeholder assertions to the wired behaviour. |
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
lines appended to the textarea, so it never leaves the browser as a file part. `dl_limit` and `ul_limit`
are not create-body fields (doc 05 §5.2); a non-zero limit is patched onto every created task with
`PATCH /api/v1/tasks/{id}` after the create response (doc 05 §5.5). The chosen `category` and the `tags`
do travel in the create body itself (doc 05 §5.2: the category must already exist, tags are created on
demand).

## Steps
1. Create `AddTaskDialog.tsx` on shadcn/ui's `dialog` with the exact field order of the doc 09 §4
   wireframe, `More options` collapsed by default. Under `More options`: the Category combobox lists
   `GET /categories` and offers the inline create that posts `POST /categories`; Tags is a multi-select
   with free entry seeded from `GET /tags`; the Download/Upload limit fields take integers in bytes per
   second, `0` meaning unlimited.
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
   one toast each. A non-zero `dl_limit`/`ul_limit` is applied with `PATCH /tasks/{id}` on each created
   task after the response — the create body has no limit fields (doc 05 §5.5). A failed PATCH never
   rolls back a created task: the row stays, the limit stays unset and a toast names the task.
10. Edit `Toolbar.tsx` so `+ Add` opens the dialog, with the menu items *Add URLs…*, *Add .torrent file…*
    and *Add from clipboard*. In `Shell.test.tsx`, replace `TestToolbarDisabledWithoutSelection`'s
    disabled-and-`Coming with the add dialog` assertions on `+ Add` with assertions of the wired
    behaviour, renaming the test when `+ Add` is no longer part of its disabled checks.
11. Create `AddTaskDialog.test.tsx`: badge classification for a magnet, a bare 40-hex infohash, a
    32-character base32 infohash and rubbish; a `.txt` drop appends its lines; a `.nzb` drop is refused; the
    JSON body carries `paused`, `sequential`, `create_subfolder`, `category` and `tags`, and non-zero
    byte-per-second limits arrive as `PATCH /tasks/{id}` calls on each created task; a `.torrent` upload
    produces a multipart request with a `payload` part; `Ctrl+Enter` submits and `Enter` does not.
12. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [x] `TestClassifyLineBadges`, `TestIsDroppableTextPredicate` and `TestTxtDropAppendsLines` pass.
- [x] `TestJsonSubmissionBody` and `TestMultipartSubmissionForTorrent` pass.
- [x] `TestFiftyLineSoftWarning` asserts the counter text and that no line is dropped.
- [x] The three verbatim labels appear character for character as doc 09 §4 gives them.
- [x] Limits are entered and sent in bytes per second; no field is labelled KB/s.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo ADD_DIALOG_OK
```
Expected: Vitest reports `Test Files  N passed (N)` where `N` equals the number of pre-existing `web/src`
test files plus one for `src/components/AddTask/AddTaskDialog.test.tsx`; every test named above appears
as passing; and the final line of stdout is exactly `ADD_DIALOG_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

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
`make lint && make typecheck && make test-web && echo ADD_DIALOG_OK` on the
final tree — gofmt and golangci-lint clean, eslint and prettier clean,
`tsc --noEmit` clean, Vitest 179/179 across 12 files including the nine
`AddTaskDialog.test.tsx` tests, and the final line of stdout is
`ADD_DIALOG_OK`:

```text
$ make lint && make typecheck && make test-web && echo ADD_DIALOG_OK
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
 ✓ src/api/client.test.ts (12 tests)
 ✓ src/lib/format.test.ts (6 tests)
 ✓ src/store/useUiPrefs.test.ts (8 tests)
 ✓ src/store/useTasks.test.ts (12 tests)
 ✓ src/main.test.ts (1 test)
 ✓ src/components/FolderBrowser/FolderBrowserDialog.test.tsx (9 tests)
 ✓ src/components/DetailPane/DetailPane.test.tsx (13 tests)
 ✓ src/components/AddTask/AddTaskDialog.test.tsx (9 tests)
 ✓ src/lib/theme.test.ts (9 tests)
 ✓ src/components/Shell/Shell.test.tsx (12 tests)
 ✓ src/components/TaskGrid/TaskGrid.test.tsx (33 tests)
 ✓ src/App.test.tsx (55 tests)
 Test Files  12 passed (12)
      Tests  179 passed (179)
ADD_DIALOG_OK
```

Scope check on the same tree — exactly the Files table paths and nothing else:

```text
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
web/src/components/AddTask/AddTaskDialog.test.tsx
web/src/components/AddTask/AddTaskDialog.tsx
web/src/components/AddTask/FileSelectionDialog.tsx
web/src/components/Shell/Shell.test.tsx
web/src/components/Shell/Toolbar.tsx
web/src/locales/en/dialogs.json
```

`TestJsonSubmissionBody` captures the create body — `paused`, `sequential`,
`create_subfolder`, `category` and `tags` travel in the JSON — and both
`PATCH /tasks/{id}` calls carrying `{dl_limit:1024, ul_limit:2048}`.
`TestMultipartSubmissionForTorrent` asserts a `multipart/form-data` request
whose `payload` part is the JSON and whose `file` part is the dropped
`.torrent`. `TestCtrlEnterSubmits` counts POSTs: Enter submits nothing,
Ctrl+Enter submits once. `make ci` also passes on the final tree: `go vet`
clean, both `docker compose config -q` runs clean and `doclint` reports
`✅ 2440 OK 🚫 0 Errors`.

## Blocked
Resolved by the plan repair: `web/src/components/Shell/Shell.test.tsx` is now in the `## Files` table with
the placeholder-assertion update assigned to step 10; T050 is now a dependency, so `GET`/`POST /categories`
and `GET /tags` exist for the Category combobox and Tags control; the `## Verification` expectation derives
the `N passed (N)` total from the pre-existing `web/src` test files plus `AddTaskDialog.test.tsx` instead of
hard-coding a number; and `dl_limit`/`ul_limit` are specified as `PATCH /tasks/{id}` calls after creation,
matching doc 05 §5.5. T049 remains unimplemented and `todo`.

Original blocker: `TestToolbarDisabledWithoutSelection` asserted the `+ Add` button is `disabled` with
`getAttribute("title")` of `"Coming with the add dialog"` (`web/src/components/Shell/Shell.test.tsx`,
written by T044 commit `d165163` / PR #155 as a placeholder while the dialog did not exist), while step 10
requires `+ Add` to open the dialog — and the file was outside the `## Files` table. Three further plan
defects were recorded with it:

1. `## Verification` expected `Test Files  10 passed (10)`, but `web/src` already contained 11 test files;
   adding `AddTaskDialog.test.tsx` makes Vitest report `Test Files  12 passed (12)`. Same staleness as the
   T044/T045 suite-count fixes.
2. Doc 09 §4 specifies the Category control as "Existing categories plus inline create". T050 was still
   `todo` and not a dependency: `/categories` was absent from `api/openapi.json` and
   `web/src/api/schema.d.ts`, so the typed client could not call it and a direct `fetch` is forbidden. The
   in-use-categories alternative was rejected: no category can exist before T050 serves `POST /categories`
   (the create body and `PATCH` both validate against the categories table), so the combobox would have been
   permanently empty and doc 09 §4's inline create would have had no implementing task.
3. `AddTaskDraft` and step 11 carried `dl_limit`/`ul_limit` and wanted "byte-per-second limits" in the JSON
   submission body, but `CreateTasksBody` has `additionalProperties: false` and no limit fields
   (`internal/api/tasks.go`, `api/openapi.json`) — `POST /tasks` would answer `422`. The documented way to
   set limits is `PATCH /tasks/{id}` (doc 05 §5.5: "applied immediately, including to a running task").
