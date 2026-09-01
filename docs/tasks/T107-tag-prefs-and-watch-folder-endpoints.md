# T107 — Reach the tag, preference and watch-folder tables over HTTP

| Field | Value |
|---|---|
| **ID** | T107 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T050, T083, T084 |
| **Blocks** | T108 |
| **Parallel-safe** | no — it also edits the shared files `internal/api/server.go`, `internal/store/settings.go` |
| **Implements** | [FR-033](../02-requirements.md#fr-033-list-rename-and-delete-tags), [FR-046](../02-requirements.md#fr-046-manage-watch-folders-and-scan-one-on-demand), [FR-144](../02-requirements.md#fr-144-persist-server-side-ui-preferences-per-user), [FR-031](../02-requirements.md#fr-031-assign-free-form-tags-and-filter-by-them) |
| **Decisions** | [ADR-0003](../decisions/0003-chi-huma-code-first-openapi.md) |
| **Est. size** | 3 new files, ~390 LOC |

## Goal
Every table in the data model is reachable: `PATCH`/`DELETE /tags/{name}` rename and detach a tag without
deleting a task, `GET`/`PUT /prefs` carry one server-side preference document per user, and
`/watch-folders` plus `/watch-folders/{id}/scan` manage and trigger the loader built in T083.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §8.2 Tags](../05-api-contract.md#82-tags)
2. [`docs/05-api-contract.md` §11.4 `GET /prefs` and `PUT /prefs`](../05-api-contract.md#114-get-prefs-and-put-prefs)
3. [`docs/05-api-contract.md` §15 Watch folders](../05-api-contract.md#15-watch-folders)
4. [`docs/04-data-model.md` §8 Table reachability](../04-data-model.md#8-table-reachability)
5. [`docs/09-web-ui-spec.md` §3.3 Persistence](../09-web-ui-spec.md#33-persistence)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/prefs.go` | create | `GET`/`PUT /prefs` and `PATCH`/`DELETE /tags/{name}`. |
| `internal/api/watchfolders.go` | create | Watch-folder CRUD and `POST /watch-folders/{id}/scan`. |
| `internal/api/prefs_test.go` | create | `humatest` cases for all three groups. |
| `internal/store/settings.go` | modify | Add `RenameTag`, `DeleteTag`, `Prefs`, `PutPrefs` and the watch-folder writes. |
| `internal/api/server.go` | modify | Register the seven operations. |

No other file may be modified.

## Interface contract

```go
package api

// PrefsBody is one preference document. The server stores unknown members verbatim and returns
// them unchanged, so the SPA can add a preference without a server change. version is an integer
// the SPA owns; the server never inspects it. There is no PATCH: PUT replaces wholesale.
type PrefsBody map[string]any

type PrefsOutput struct{ Body PrefsBody }
type PutPrefsInput struct{ Body PrefsBody }

func (h *PrefsHandlers) Get(ctx context.Context, in *struct{}) (*PrefsOutput, error)
func (h *PrefsHandlers) Put(ctx context.Context, in *PutPrefsInput) (*PrefsOutput, error)

// TagView is one row of GET /tags. For a non-admin, TaskCount counts that caller's tasks only.
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

func (h *PrefsHandlers) PatchTag(ctx context.Context, in *PatchTagInput) (*TagOutput, error)
func (h *PrefsHandlers) DeleteTag(ctx context.Context, in *DeleteTagInput) (*struct{}, error)

// WatchFolderView is the object of doc 05 §15.
type WatchFolderView struct {
	ID              string     `json:"id"` // wfd_ + ULID
	Path            string     `json:"path"`
	Enabled         bool       `json:"enabled"`
	OwnerID         string     `json:"owner_id"`
	OwnerUsername   string     `json:"owner_username"`
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

// Prefs returns the caller's ui_prefs rows assembled into one document, or the server defaults.
func (s *SettingsStore) Prefs(ctx context.Context, userID string) (map[string]any, error)

// PutPrefs replaces the document wholesale in one transaction, keyed on the document's top-level
// members. It never reads or writes another user's rows.
func (s *SettingsStore) PutPrefs(ctx context.Context, userID string, doc map[string]any) error
```

Statuses: tags `200`/`204` · `403` `/problems/forbidden` (`PATCH` and `DELETE` are admin-only, `GET` is
open) · `404` · `409` · `422` for an empty `new_name` or a name containing `,` or `/`. Prefs `200` · `401` ·
`413` `/problems/payload-too-large` above 64 KiB · `422` when the body is not a JSON object. Watch folders
are admin-only throughout, with `403` `/problems/path-rejected` for a `path` or `destination` outside the
roots or outside the named owner's jail, `409` on a duplicate `path`, and `422` for an unknown category, an
unknown `owner_id` or `poll_interval_s` below `1`.

## Steps
1. Add `RenameTag`, `DeleteTag`, `Prefs`, `PutPrefs` and the watch-folder create, update and delete writes
   to `internal/store/settings.go`, each with an explicit column list and the multi-row writes in one
   `sqlx.Tx`.
2. Create `internal/api/prefs.go` with the prefs pair and the tag pair. `PUT /prefs` replaces the document;
   unknown members are stored and returned verbatim; the caller's identity is the only key.
3. Cap the prefs body at 64 KiB and return `413` above it; reject a non-object body with `422`.
4. Make `PATCH` and `DELETE /tags/{name}` admin-only, decode the percent-encoded name, and return `409` on a
   rename onto an existing tag.
5. Create `internal/api/watchfolders.go` with the CRUD verbs and `Scan`, all admin-only.
6. Validate `path` and `destination` against the configured roots **and** the named owner's jail, defaulting
   `owner_id` to the calling admin and `poll_interval_s` to `10`.
7. Implement `Scan` by calling `jobs.Watcher.ScanOnce` and returning its result unchanged; a disabled folder
   still scans on demand.
8. Edit `internal/api/server.go` to register the seven operations.
9. Create `internal/api/prefs_test.go`: tag three tasks, rename the tag and assert all three carry the new
   name, then delete it and assert the three tasks still exist with no tags; assert a rename onto an
   existing tag is `409`; store a column order, widths and visibility, read them back as a second client for
   the same user and assert equality under `go-cmp`, and assert a second user's document is unaffected;
   assert an unknown prefs member survives a round trip; create a watch folder, drop a fixture `.torrent`
   into it, `POST .../scan` and assert the task appears without waiting for the interval; assert a watch
   folder outside every root is `403` `/problems/path-rejected`.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] Renaming a tag changes it for every task at once and deletes no task.
- [ ] Deleting a tag detaches it everywhere and deletes no task.
- [ ] A preference document round-trips per user, including unknown members, and never leaks across users.
- [ ] A prefs body above 64 KiB is `413`.
- [ ] `POST /watch-folders/{id}/scan` creates the task synchronously, without waiting for the interval.
- [ ] A watch folder outside the configured roots is `403` `/problems/path-rejected`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/api/... ./internal/store/..." && echo REACHABILITY_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/api` and `ok  github.com/L-K-M/dl-tool/internal/store`,
with `TestRenameTagKeepsTasks`, `TestDeleteTagKeepsTasks`, `TestRenameOntoExistingIsConflict`,
`TestPrefsRoundTripPerUser`, `TestPrefsUnknownMemberPreserved`, `TestPrefsTooLarge`,
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
- Do NOT add a `PATCH /prefs`; the document is replaced wholesale and that is deliberate.
- Do NOT validate or normalise unknown prefs members; store and return them verbatim.
- Do NOT let `DELETE /tags/{name}` remove a task under any circumstance.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
