# T047 — Serve mkdir and free space, and build the folder browser dialog

| Field | Value |
|---|---|
| **ID** | T047 |
| **Milestone** | M3 |
| **Status** | todo |
| **Depends on** | T046, T099 |
| **Blocks** | T049, T053 |
| **Parallel-safe** | no — extends T046's `internal/api/fs.go` |
| **Implements** | [FR-041](../02-requirements.md#fr-041-create-a-directory-and-report-free-space) |
| **Decisions** | [ADR-0012](../decisions/0012-single-data-mount.md) |
| **Est. size** | 3 new files, ~370 LOC |

## Goal
`POST /fs/mkdir` creates one directory under the caller's jail with the process umask, `GET /fs/free-space`
reports plain integer bytes, and every `Select` button in the product opens one nested dialog that browses
the server, creates folders and refuses unwritable directories.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §7.1 Endpoints](../05-api-contract.md#71-endpoints) — the `mkdir` and
   `free-space` shapes, `201`, and the `409` on an existing name.
2. [`docs/09-web-ui-spec.md` §4.1 Server-side folder browser](../09-web-ui-spec.md#41-server-side-folder-browser)
   — the wireframe, the two error strings, the lock badge and the keyboard model.
3. [`docs/tasks/T046-filesystem-roots-and-browse.md`](T046-filesystem-roots-and-browse.md) — `fsx.Browse`,
   `fsx.SafeJoin` and the jail resolution this task reuses.
4. [`docs/tasks/T099-disk-space-reservation.md`](T099-disk-space-reservation.md) — `fsx.FreeSpace` and
   `fsx.Space`, which report bytes and never KB.
5. [`docs/09-web-ui-spec.md` §10.4 Accessibility](../09-web-ui-spec.md#104-accessibility) — dialogs use the
   shadcn/ui `dialog`; never hand-roll a modal.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/fs.go` | edit | Add the `Mkdir` and `FreeSpace` operations. |
| `internal/api/fs_test.go` | edit | Umask, conflict, jail and integer-byte cases. |
| `web/src/components/FolderBrowser/FolderBrowserDialog.tsx` | create | The nested destination picker. |
| `web/src/components/FolderBrowser/FolderBrowserDialog.test.tsx` | create | Navigation, errors, lock badge and `New folder`. |
| `web/src/locales/en/dialogs.json` | create | Dialog strings for this and later dialogs. |

No other file may be modified.

## Interface contract

```go
package api

type MkdirInput struct {
	Body struct {
		Path string `json:"path" required:"true"`
		Name string `json:"name" required:"true"`
	}
}
type MkdirOutput struct {
	Status int `json:"-"` // 201
	Body   struct {
		Path     string `json:"path"`
		Writable bool   `json:"writable"`
	}
}

type FreeSpaceInput struct {
	Path string `query:"path" required:"true"`
}
type FreeSpaceOutput struct {
	Body struct {
		Path       string `json:"path"`
		FreeBytes  int64  `json:"free_bytes"`
		TotalBytes int64  `json:"total_bytes"`
	}
}

func (h *FSHandlers) Mkdir(ctx context.Context, in *MkdirInput) (*MkdirOutput, error)
func (h *FSHandlers) FreeSpace(ctx context.Context, in *FreeSpaceInput) (*FreeSpaceOutput, error)
```

`name` goes through `fsx.SafeJoin(resolvedPath, []string{name})`; a `name` containing `/` or `..` is
`422 /problems/validation-failed`, an existing name is `409 /problems/conflict`, a path outside the jail is
`403 /problems/path-rejected`. `mkdir` applies the process umask and sets no mode of its own.

```tsx
// web/src/components/FolderBrowser/FolderBrowserDialog.tsx
export interface FolderBrowserDialogProps {
  open: boolean;
  /** Where to open. Falls back to the first entry of GET /fs/roots. */
  initialPath?: string;
  onSelect: (path: string) => void;
  onOpenChange: (open: boolean) => void;
}

export function FolderBrowserDialog(props: FolderBrowserDialogProps): JSX.Element;
```

Behaviour, from doc 09 §4.1:

| Event | Result |
|---|---|
| `403 /problems/path-rejected` | Render `That folder is outside the allowed download roots.` and keep the previous listing on screen. |
| `404` | Render `Folder not found or unreadable.` |
| `writable: false` | Show the lock badge; `Select` is disabled with a tooltip. |
| `↑` `↓` | Move; `Enter` or `→` descends, `Backspace` or `←` ascends, `Esc` cancels. |
| Manual `Path` field | Validated on blur through `GET /fs/browse`. |

## Steps
1. Edit `internal/api/fs.go` to add `Mkdir` and `FreeSpace`, registered in `Register` beside the T046
   operations, resolving the caller's jail the same way.
2. Implement `FreeSpace` over `fsx.FreeSpace`, returning `fsx.Space` values unchanged: plain integer bytes,
   never kibibytes and never a float.
3. Edit `internal/api/fs_test.go`: `mkdir` creates the directory and returns `201`; the mode reflects the
   process umask; a second identical call is `409 /problems/conflict`; `name` of `a/b` and `..` are `422`;
   `mkdir` outside the jail is `403`; `free-space` returns non-zero integers for a root.
4. Create `web/src/locales/en/dialogs.json` with the folder-browser strings, including the two error
   sentences verbatim.
5. Create `FolderBrowserDialog.tsx` on shadcn/ui's `dialog`, with a breadcrumb, the directory list, the
   manual `Path` field, the free-space line and a `New folder` button posting `POST /fs/mkdir`.
6. Fetch listings with the T014 client only: `GET /fs/roots` on open with no `initialPath`, then
   `GET /fs/browse?path=` per navigation, and `GET /fs/free-space?path=` per selected directory.
7. Implement the keyboard model and the error handling in the table above; the dialog has its own focus
   trap because it is opened from inside another dialog.
8. Create `FolderBrowserDialog.test.tsx` with `msw`: descending updates the breadcrumb; a `403` keeps the
   previous listing and shows the roots message; a `404` shows the not-found message; an unwritable row
   shows the lock and disables `Select`; `New folder` posts `{"path":…,"name":…}` and re-lists.
9. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestMkdirCreatesWithUmask`, `TestMkdirConflict`, `TestMkdirRejectsSeparatorInName` pass.
- [ ] `TestFreeSpaceReturnsIntegerBytes` asserts the JSON numbers have no fractional part.
- [ ] `TestBrowserKeepsListingOnPathRejected` and `TestUnwritableFolderCannotBeSelected` pass.
- [ ] The dialog issues no request outside `/api/v1/fs/*`.
- [ ] Both error strings match doc 09 §4.1 word for word.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test && echo BROWSER_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/api`, Vitest reports
`src/components/FolderBrowser/FolderBrowserDialog.test.tsx` passing, every test named above appears as
passing, and the final line of stdout is exactly `BROWSER_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT list files, only directories; T046 fixed that contract.
- Do NOT let the dialog write a destination anywhere; it returns a path through `onSelect` and nothing else.
- Do NOT add a free-text destination field to any caller; doc 09 §4 keeps the destination read-only.
- Do NOT implement the per-user jail rules again in the client; the server already filters the roots.
- Do NOT add a delete, rename or upload action to the browser.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
