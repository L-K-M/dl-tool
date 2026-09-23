# T107 — Reach the tag and watch-folder tables over HTTP

| Field | Value |
|---|---|
| **ID** | T107 |
| **Milestone** | M6 |
| **Status** | done |
| **Depends on** | T050, T083, T084 |
| **Blocks** | T108, T119 |
| **Parallel-safe** | no — it also edits the shared files `internal/api/server.go`, `internal/store/settings.go` |
| **Implements** | [FR-033](../02-requirements.md#fr-033-list-rename-and-delete-tags), [FR-046](../02-requirements.md#fr-046-manage-watch-folders-and-scan-one-on-demand), [FR-031](../02-requirements.md#fr-031-assign-free-form-tags-and-filter-by-them) |
| **Decisions** | [ADR-0003](../decisions/0003-chi-huma-code-first-openapi.md) |
| **Est. size** | 3 new files, ~330 LOC |

## Goal
The tag and watch-folder tables become reachable: `PATCH`/`DELETE /tags/{name}` rename and detach a
tag without deleting a task, and `/watch-folders` plus `/watch-folders/{id}/scan` manage and trigger
the loader built in T083. The `GET`/`PUT /prefs` pair moved forward to [T129](T129-ui-prefs-document.md), which
M4 needed for the saved-search document — this task keeps the tag and watch-folder operations.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §8.2 Tags](../05-api-contract.md#82-tags)
2. [`docs/05-api-contract.md` §15 Watch folders](../05-api-contract.md#15-watch-folders)
3. [`docs/04-data-model.md` §8 Table reachability](../04-data-model.md#8-table-reachability)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/tags.go` | create | `PATCH`/`DELETE /tags/{name}`. |
| `internal/api/watchfolders.go` | create | Watch-folder CRUD and `POST /watch-folders/{id}/scan`. |
| `internal/api/reachability_test.go` | create | `humatest` cases for both groups. |
| `internal/store/settings.go` | modify | Add `RenameTag`, `DeleteTag` and the watch-folder writes. |
| `internal/api/server.go` | modify | Register the seven operations — two tag verbs and five watch-folder verbs. |

No other file may be modified.

## Interface contract

```go
package api

// TagView is one row of GET /tags.
type TagView struct {
	Name      string `json:"name"`
	TaskCount int    `json:"task_count"`
}

type PatchTagInput struct {
	Name string `path:"name"` // percent-encoded
	Body struct {
		NewName string `json:"new_name" required:"true" minLength:"1"`
	}
}

func (h *TagHandlers) PatchTag(ctx context.Context, in *PatchTagInput) (*TagOutput, error)
func (h *TagHandlers) DeleteTag(ctx context.Context, in *DeleteTagInput) (*struct{}, error)

// WatchFolderView is the object of doc 05 §15.
type WatchFolderView struct {
	ID              string     `json:"id"` // wfd_ + ULID
	Path            string     `json:"path"`
	Enabled         bool       `json:"enabled"`
	Destination     string     `json:"destination"`
	Category        *string    `json:"category"`
	DeleteAfterLoad bool       `json:"delete_after_load"`
	PollIntervalS   int        `json:"poll_interval_s"`
	LastScanAt      *time.Time `json:"last_scan_at"`
	LastError       *string    `json:"last_error"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// Scan runs jobs.Watcher.ScanOnce synchronously and returns its ScanResult unchanged.
func (h *WatchFolderHandlers) Scan(ctx context.Context, in *ScanInput) (*ScanOutput, error)
```

```go
package store

// RenameTag renames the row in place, so every task carrying it carries the new name at once; the
// tag id is unchanged and no task row is touched. A rename onto an existing name is a conflict,
// never a silent merge.
func (s *SettingsStore) RenameTag(ctx context.Context, name, newName string) error

// DeleteTag detaches the tag from every task and deletes the row. NO TASK IS EVER DELETED.
func (s *SettingsStore) DeleteTag(ctx context.Context, name string) error
```

Statuses: tags `200`/`204` · `404` · `409` · `422` for an empty `new_name` or a name containing `,` or
`/`. Watch folders
take `403` `/problems/path-rejected` for a `path` or `destination` outside the configured roots, `409` on a
duplicate `path`, and `422` for an unknown category or a `poll_interval_s` below `1`.

## Steps
1. Add `RenameTag`, `DeleteTag` and the watch-folder create, update and delete writes
   to `internal/store/settings.go`, each with an explicit column list and the multi-row writes in one
   `sqlx.Tx`.
2. Create `internal/api/tags.go` with the tag pair.
3. Decode the percent-encoded name for `PATCH` and `DELETE /tags/{name}`, and return `409` on a
   rename onto an existing tag.
4. Create `internal/api/watchfolders.go` with the CRUD verbs and `Scan`.
5. Validate `path` and `destination` against the configured roots, defaulting `poll_interval_s` to `10`.
6. Implement `Scan` by calling `jobs.Watcher.ScanOnce` and returning its result unchanged; a disabled folder
   still scans on demand.
7. Edit `internal/api/server.go` to register the seven operations — two tag verbs and five
   watch-folder verbs.
8. Create `internal/api/reachability_test.go`: tag three tasks, rename the tag and assert all three carry the new
   name, then delete it and assert the three tasks still exist with no tags; assert a rename onto an
   existing tag is `409`; create a watch folder, drop a fixture `.torrent`
   into it, `POST .../scan` and assert the task appears without waiting for the interval; assert a watch
   folder outside every root is `403` `/problems/path-rejected`.
9. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [x] Renaming a tag changes it for every task at once and deletes no task.
- [x] Deleting a tag detaches it everywhere and deletes no task.
- [x] `POST /watch-folders/{id}/scan` creates the task synchronously, without waiting for the interval.
- [x] A watch folder outside the configured roots is `403` `/problems/path-rejected`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/api/... ./internal/store/..." && echo REACHABILITY_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/api` and `ok  github.com/L-K-M/dl-tool/internal/store`,
with `TestRenameTagKeepsTasks`, `TestDeleteTagKeepsTasks`, `TestRenameOntoExistingIsConflict`,
`TestScanCreatesTaskImmediately` and `TestWatchFolderOutsideRootsRejected` each reported as `--- PASS`. The
final line of stdout is exactly `REACHABILITY_OK`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add `POST /tags`; tags are created implicitly by `POST /tasks` and `PATCH /tasks/{id}`.
- Do NOT re-implement the watch-folder scanner or its inotify fallback; T083 owns `internal/jobs/watch.go`.
- Do NOT add `GET /tags` or category CRUD; T050 owns both.
- Do NOT implement `GET`/`PUT`/`PATCH /prefs`; [T129](T129-ui-prefs-document.md) owns the whole
  preference-document pair.
- Do NOT let `DELETE /tags/{name}` remove a task under any circumstance.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

```
$ make lint && make test PKG="./internal/api/... ./internal/store/..." && echo REACHABILITY_OK
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
go test -race -count=1 ./internal/api/... ./internal/store/...
ok  	github.com/L-K-M/dl-tool/internal/api	168.087s
ok  	github.com/L-K-M/dl-tool/internal/store	75.616s
REACHABILITY_OK
```

Named tests (verbose run, all `--- PASS`):

```
$ go test -race -count=1 -v -run 'TestRenameTagKeepsTasks|TestDeleteTagKeepsTasks|TestRenameOntoExistingIsConflict|TestScanCreatesTaskImmediately|TestWatchFolderOutsideRootsRejected' ./internal/api/
--- PASS: TestRenameTagKeepsTasks (0.51s)
--- PASS: TestDeleteTagKeepsTasks (0.46s)
--- PASS: TestRenameOntoExistingIsConflict (0.37s)
--- PASS: TestScanCreatesTaskImmediately (0.43s)
--- PASS: TestWatchFolderOutsideRootsRejected (0.42s)
ok  	github.com/L-K-M/dl-tool/internal/api	3.345s
```

Scope check:

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
api/openapi.json
internal/api/reachability_test.go
internal/api/server.go
internal/api/tags.go
internal/api/watchfolders.go
internal/store/settings.go
web/src/api/schema.d.ts
```

`api/openapi.json` and `web/src/api/schema.d.ts` are the `make gen` output of the seven newly registered
Huma operations. `docs/13-testing-and-verification.md` §7.1 makes them part of the Files table verbatim:
"api/openapi.json and web/src/api/schema.d.ts are part of the `Files` table of every task that registers,
removes or changes a Huma operation or one of its request/response structs, whether or not that table
lists them by name. Such a task runs `make gen` and commits both files in its own commit." Both were
regenerated by `scripts/gen.sh`, not hand-edited.

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
