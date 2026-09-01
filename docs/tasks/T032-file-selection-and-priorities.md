# T032 — Select and prioritise the files of a task

| Field | Value |
|---|---|
| **ID** | T032 |
| **Milestone** | M2 |
| **Status** | todo |
| **Depends on** | T021, T029, T030 |
| **Blocks** | T033, T038, T048 |
| **Parallel-safe** | no — extends `internal/store/tasks.go` and `internal/api/server.go` |
| **Implements** | [FR-007](../02-requirements.md#fr-007-select-and-prioritise-individual-files) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| **Est. size** | 3 new files, ~400 LOC |

## Goal
`GET /api/v1/tasks/{id}/files` lists a task's files with their selection and priority, and
`PATCH /api/v1/tasks/{id}/files` changes them on the running engine and in `task_files`. The wire
vocabulary is `skip`, `normal`, `high`, `maximum`; the stored integers are `0`, `1`, `6`, `7`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §5.8 `GET /tasks/{id}/files` and `PATCH /tasks/{id}/files`](../05-api-contract.md#58-get-tasksidfiles-and-patch-tasksidfiles)
2. [`docs/06-download-engines.md` §1.1 File priority vocabulary](../06-download-engines.md#11-file-priority-vocabulary)
3. [`docs/06-download-engines.md` §5.7 Files, priorities, trackers, peers, lifecycle](../06-download-engines.md#57-files-priorities-trackers-peers-lifecycle)
4. [`docs/04-data-model.md` §4.3 `task_files.priority`](../04-data-model.md#43-task_filespriority)
5. [`docs/04-data-model.md` §3.3 Tasks](../04-data-model.md#33-tasks)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/qbittorrent/files.go` | create | `Files` and `SetFiles` over `torrents/files` and `torrents/filePrio`. |
| `internal/api/tasks_files.go` | create | The two handlers, the DTO and the priority name mapping. |
| `internal/api/tasks_files_test.go` | create | `humatest` cases for both verbs and every rejection. |
| `internal/store/tasks.go` | modify | Add `ListFiles`, `UpsertFiles` and `UpdateFileSelection`. |
| `internal/api/server.go` | modify | Register `list-task-files` and `patch-task-files`. |

No other file may be modified.

## Interface contract

```go
package qbittorrent

// fileJSON is one element of GET /api/v2/torrents/files.
type fileJSON struct {
	Index        int     `json:"index"`    // 0-based
	Name         string  `json:"name"`     // filename including relative path
	Size         int64   `json:"size"`
	Progress     float64 `json:"progress"` // percentage/100
	Priority     int     `json:"priority"` // 0 | 1 | 6 | 7, and -1 (Mixed) on an aggregate row
	IsSeed       bool    `json:"is_seed"`
	Availability float64 `json:"availability"`
}

// Files returns one FileEntry per file. Priority is the identity mapping of 06 §1.1; a returned -1
// (Mixed) is passed through as Priority 1 and never treated as an error.
func (c *Client) Files(ctx context.Context, id string) ([]engine.FileEntry, error)

// SetFiles applies selection and priorities in one pass. Indices absent from both arguments are left
// untouched. It groups the indices by target priority and issues one POST torrents/filePrio per group
// with id as a pipe-separated list. It never sends 4 and never sends -1.
func (c *Client) SetFiles(ctx context.Context, id string, selected []int, priorities map[int]int) error
```

```go
package api

// priorityNames maps the stored integers of 04 §4.3 onto the wire vocabulary of 05 §5.8. There is no
// "low": Download Station's four-level scale collapses low onto normal.
var priorityNames = map[int]string{0: "skip", 1: "normal", 6: "high", 7: "maximum"}
var priorityValues = map[string]int{"skip": 0, "normal": 1, "high": 6, "maximum": 7}

type TaskFileDTO struct {
	Index          int     `json:"index"`
	Path           string  `json:"path"` // relative to tasks.destination
	SizeBytes      int64   `json:"size_bytes"`
	CompletedBytes int64   `json:"completed_bytes"`
	Progress       float64 `json:"progress"`
	Selected       bool    `json:"selected"`
	Priority       *string `json:"priority"` // null on an engine without per_file_priority
}

type ListTaskFilesOutput struct {
	Body struct {
		Files []TaskFileDTO `json:"files"`
	}
}

// FileSelection is one entry of the PATCH body. Selected and Priority are each optional, but at least
// one must be present. selected:false and priority:"skip" are one concept: setting either sets both.
type FileSelection struct {
	Index    int     `json:"index"    minimum:"0"`
	Selected *bool   `json:"selected,omitempty"`
	Priority *string `json:"priority,omitempty" enum:"skip,normal,high,maximum"`
}

type PatchTaskFilesInput struct {
	ID   string `path:"id"`
	Body struct {
		Files []FileSelection `json:"files" minItems:"1"`
	}
}
```

```go
package store

func (s *Store) ListFiles(ctx context.Context, taskID string) ([]TaskFile, error)
// UpsertFiles replaces the task's task_files rows from an engine listing, keyed on (task_id, file_index).
func (s *Store) UpsertFiles(ctx context.Context, taskID string, files []TaskFile) error
// UpdateFileSelection writes selected and priority for the listed indices in one transaction.
func (s *Store) UpdateFileSelection(ctx context.Context, taskID string, sel map[int]TaskFileSelection) error
```

## Steps
1. Create `internal/engine/qbittorrent/files.go` with `fileJSON`, `Files` and `SetFiles`.
2. `Files` calls `GET torrents/files?hash=<hash>`, maps each element to `engine.FileEntry` with
   `Selected = Priority != 0`, `Completed = int64(Progress * float64(Size))`, and a `*int` priority.
3. `SetFiles` normalises: every index in `priorities` keeps its value; every index in `selected` with no
   explicit priority becomes `1`; every index the engine reports that appears in neither, when `selected`
   is non-nil, becomes `0`. Reject the value `4` and any value outside `{0,1,6,7}` before any request.
4. Group indices by priority and issue one `POST torrents/filePrio` per group with `hash`, the
   pipe-separated `id` list and `priority`; map `409` to a wrapped error naming the out-of-range index.
5. Add `ListFiles`, `UpsertFiles` and `UpdateFileSelection` to `internal/store/tasks.go` with explicit
   column lists and one `sqlx.Tx` per multi-row write.
6. Create `internal/api/tasks_files.go`. `GET` reads the engine through `Engine.Files`, upserts the rows
   with `UpsertFiles`, and answers from the store so the response is stable when the engine is briefly
   down.
7. `PATCH` validates every entry, resolves `selected:false` and `priority:"skip"` onto each other, calls
   `Engine.SetFiles` once, then `UpdateFileSelection`, then returns the full list exactly as `GET` does.
8. Return `422` `/problems/validation-failed` for an unknown index, an unknown priority name, the integer
   `4`, or a task whose engine does not declare `per_file_priority`; `404` for an unknown or foreign task;
   `503` `/problems/engine-unavailable` when the engine call fails.
9. Report `Priority: nil` for every file of an engine that does not declare `per_file_priority`, and drive
   `Selected` alone — that is the aria2 shape.
10. Register `list-task-files` and `patch-task-files` in `internal/api/server.go`.
11. Create `internal/api/tasks_files_test.go` covering: a three-file listing; deselecting index 2;
    promoting index 0 to `high`; a `4` rejection; an unknown index rejection; an aria2 task returning null
    priorities; and a `PATCH` on an engine without `per_file_priority` returning `422`.

## Acceptance criteria
- [ ] `PATCH` with `{"index":2,"selected":false}` results in priority `0` at the engine and `selected = 0`
      in `task_files`.
- [ ] `PATCH` with `{"index":0,"priority":"high"}` sends `priority=6`, never `4`.
- [ ] A returned qBittorrent priority of `-1` does not produce an error.
- [ ] An aria2 task lists every file with `"priority": null` and a real `selected` value.
- [ ] Unlisted indices keep their previous selection and priority.
- [ ] Both verbs return the identical full-list body shape.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then `ok` for
`github.com/L-K-M/dl-tool/internal/engine/qbittorrent`, `.../internal/store` and `.../internal/api`, with
`TestSetFilesGroupsByPriority`, `TestRejectsPriority4`, `TestDeselectSetsSkip`,
`TestAria2FilesHaveNullPriority` and `TestPatchFilesUnknownIndex` all `PASS`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT accept `select_files` on `POST /tasks`; T033 owns the create-time selection.
- Do NOT rename a file on disk or call `torrents/renameFile`; renaming is not in v1.
- Do NOT introduce a `low` priority or the integer `4` anywhere, in the API, the DB or the adapter.
- Do NOT recompute a task's `total_bytes` from the selection here; T100 owns the metadata re-check.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
