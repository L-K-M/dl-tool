# T050 — Serve category CRUD, the tag list and category path resolution

| Field | Value |
|---|---|
| **ID** | T050 |
| **Milestone** | M3 |
| **Status** | todo |
| **Depends on** | T017, T020, T021 |
| **Blocks** | T049, T053, T073, T107, T119 |
| **Parallel-safe** | no — extends `internal/store/settings.go` and `internal/api/tasks.go` |
| **Implements** | [FR-030](../02-requirements.md#fr-030-manage-categories-with-a-save-path), [FR-031](../02-requirements.md#fr-031-assign-free-form-tags-and-filter-by-them) |
| **Decisions** | [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md) |
| **Est. size** | 2 new files, ~330 LOC |

## Goal
Categories are global and carry a `save_path` that becomes the effective destination of a task created in
that category with no explicit destination. `GET /tags` lists every tag with its task count.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §8.1 Categories](../05-api-contract.md#81-categories) and
   [§8.2 Tags](../05-api-contract.md#82-tags) — the shapes and every status code.
2. [`docs/04-data-model.md` §3.2 Configuration](../04-data-model.md#32-configuration) — the `categories`
   and `tags` DDL.
3. [`docs/tasks/T020-create-tasks-endpoint.md`](T020-create-tasks-endpoint.md) — where the destination is
   resolved today.
4. [`docs/14-conventions.md` §2.4 SQL and sqlx](../14-conventions.md#24-sql-and-sqlx).

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/categories.go` | create | Category CRUD and `GET /tags`. |
| `internal/api/categories_test.go` | create | CRUD, conflict and resolution cases. |
| `internal/store/settings.go` | edit | Category and tag queries. |
| `internal/api/tasks.go` | edit | Resolve a missing destination from the category save path. |

No other file may be modified.

## Interface contract

```go
package store

type Category struct {
	ID        string `db:"id"        json:"-"`
	Name      string `db:"name"      json:"name"`
	SavePath  string `db:"save_path" json:"save_path"`
	TaskCount int    `db:"task_count" json:"task_count"`
}

type Tag struct {
	Name      string `db:"name"       json:"name"`
	TaskCount int    `db:"task_count" json:"task_count"`
}

func ListCategories(ctx context.Context, db *sqlx.DB) ([]Category, error)
func CategoryByName(ctx context.Context, db *sqlx.DB, name string) (Category, error)
func CreateCategory(ctx context.Context, db *sqlx.DB, c Category) error
func RenameCategory(ctx context.Context, db *sqlx.DB, name, newName, savePath string) error
func DeleteCategory(ctx context.Context, db *sqlx.DB, name string) error

// ListTags returns every row of tags sorted by name, including tags with no tasks. task_count
// counts every non-removed task carrying the tag.
func ListTags(ctx context.Context, db *sqlx.DB) ([]Tag, error)
```

```go
package api

type CategoryDTO struct {
	Name      string `json:"name"`
	SavePath  string `json:"save_path"`
	TaskCount int    `json:"task_count"`
}

type CreateCategoryInput struct {
	Body struct {
		Name     string `json:"name"      required:"true" minLength:"1"`
		SavePath string `json:"save_path" required:"true"`
	}
}
type PatchCategoryInput struct {
	Name string `path:"name"`
	Body struct {
		NewName  string `json:"new_name,omitempty"`
		SavePath string `json:"save_path,omitempty"`
	}
}

func (h *CategoryHandlers) List(ctx context.Context, in *struct{}) (*ListCategoriesOutput, error)
func (h *CategoryHandlers) Create(ctx context.Context, in *CreateCategoryInput) (*CategoryOutput, error)
func (h *CategoryHandlers) Patch(ctx context.Context, in *PatchCategoryInput) (*CategoryOutput, error)
func (h *CategoryHandlers) Delete(ctx context.Context, in *DeleteCategoryInput) (*struct{}, error)
func (h *CategoryHandlers) ListTags(ctx context.Context, in *struct{}) (*ListTagsOutput, error)
```

Destination resolution, added to the create path in `internal/api/tasks.go`:

| Request | Effective `destination` | `requested_destination` |
|---|---|---|
| explicit `destination` | that path, through `fsx.ResolveDestination` | `null` |
| none, category with a `save_path` inside the roots | the category `save_path` | `null` |
| none, no category | the `default_destination` setting, else the first root | `null` |

Statuses, exactly doc 05 §8.1: `200`/`201`/`204` ·
`403 /problems/path-rejected` for a `save_path` outside the roots · `404` · `409 /problems/conflict` on a
duplicate name · `422` for an empty name or a name containing `/`.

## Steps
1. Add the five category functions and `ListTags` to `internal/store/settings.go`, computing `task_count`
   with a `LEFT JOIN` over non-`removed` tasks in one statement, never one query per row.
2. Create `internal/api/categories.go` with `CategoryHandlers`, its constructor and `Register`, mirroring
   the shape T027 used for `SettingsHandlers`.
3. Validate `save_path` through `fsx.ResolveDestination` against the configured roots, rejecting anything
   outside with `403 /problems/path-rejected`.
4. Implement `DELETE` so tasks in the category become uncategorised and no task and no file is touched.
5. Edit `internal/api/tasks.go` to apply the resolution table above when the create body carries a category
   and no destination, setting `requested_destination` only when the resolved path differs from the
   requested one.
6. For `GET /tags`, count every non-removed task carrying the tag, including tags whose count is zero.
7. Create `internal/api/categories_test.go`: create, list, rename, delete; a duplicate name is `409`; a
   name containing `/` is `422`; a `save_path` of `/etc` is `403`; creating a
   task in category `linux` with no destination resolves to `/data/linux`; deleting the category leaves its
   tasks present and uncategorised; `GET /tags` lists a tag with `task_count: 0`.
8. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestCategoryCrud` and `TestDuplicateCategoryConflicts` pass.
- [ ] `TestCategorySavePathResolvesDestination` asserts the destination is `/data/linux`.
- [ ] `TestDeleteCategoryKeepsTasks` asserts the task row survives with `category` null.
- [ ] `TestListTagsIncludesZeroCount` passes.
- [ ] `GET /tags` is not paginated and is sorted by name ascending.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/api/... ./internal/store/..." && echo CATEGORIES_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/api` and `ok  github.com/L-K-M/dl-tool/internal/store`,
every test named above reported as `--- PASS`, and the final line of stdout is exactly `CATEGORIES_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement `PATCH /tags/{name}` or `DELETE /tags/{name}`; FR-033 belongs to M6's T107.
- Do NOT touch the sidebar; T044 derives its category and tag nodes from the store, and a category with no
  tasks appears once M6 wires `GET /categories` into it.
- Do NOT add a per-category engine, ratio limit or automation setting; v1 has name and save path only.
- Do NOT move or delete any downloaded data when a category is renamed or deleted.
- Do NOT let a category `save_path` escape `DLTOOL_DATA_ROOTS`; it is checked like any destination.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
Stopped before implementation: `internal/api/server.go` is not in the `## Files` table, but it is the
only composition point where a new operation group can register. `Server.registerOperations` calls each
handler group's `Register` (`s.auth`, `s.tasks`, `s.settings`, `s.fs`, `s.SSE`), so a `CategoryHandlers`
built in `categories.go` has no caller: `/categories` and `/tags` would never route, `make gen` would
produce no paths for them, and no acceptance test could observe them through `server.API`.

This is the same defect T046 recorded and commit `ba88361` repaired by adding the row
`| internal/api/server.go | edit | ... |`; T065 and T068 carry it natively ("Call
`NewXHandlers(...).Register(api)` once"). The repair for this task is the same shape, and the file that
should answer it is this task file:

1. Add `internal/api/server.go` to the `## Files` table: construct `CategoryHandlers` in `NewServer`
   (the constructor takes `db` and `cfg.DataRoots`, wrapping `db` in `store.NewSettingsStore` exactly
   like `NewSettingsHandlers` does) and call `s.categories.Register(s.API)` in `registerOperations`,
   plus a matching `## Steps` entry.
2. Extend the `## Verification` scope check with the two generated files of
   `docs/13-testing-and-verification.md` §7.1 (`api/openapi.json`, `web/src/api/schema.d.ts`), the
   wording T046's repaired file already uses — registering Huma operations necessarily changes both.
3. Record the `default_destination` decision where it is governed: amend the resolution table's third
   row to name the settings read, the unset-or-empty outcome (the migration seeds no value; the row's
   existing "else the first root" fallback covers it), and the out-of-roots outcome
   (`403 /problems/path-rejected` via `fsx.ResolveDestination`), so the spec — not only this note —
   answers the implementer.

Three smaller points the repair should settle so the implementation does not re-block or guess:

- The interface contract shows package-level store functions (`ListCategories(ctx, db)`), but the file
  it extends is `SettingsStore` method territory — T027's Files row says "every later task that adds a
  settings-table query extends this file" and `ListEngines`/`EnsureEngine`/`TouchEngine`/`EngineByID`
  are all `(s *SettingsStore)` methods. The implementation would follow the file, not the sketch.
- `PatchCategoryInput` carries `new_name`/`save_path` as `string` with `omitempty`, which cannot tell an
  omitted field from an explicit `""`, while doc 05 §8.1 makes an empty name `422`. `*string` fields are
  required; treating explicit-empty as omitted would contradict §8.1 and needs a spec change first.
- The resolution table's third row needs a `default_destination` settings read the migration does not
  seed; step 3 above owns the out-of-roots outcome. The read is a `SettingsStore` method in
  `settings.go` — T027's Files row makes that file the home of new settings-table queries — rather
  than another inline query in `tasks.go`; relocating the existing `queryConcurrencySettings` is out
  of scope. Also note `store.Category`/`store.Tag` would live in
  `settings.go` (`models.go` is outside the table) and there is no `store.ErrConflict` sentinel yet
  for the `409` mapping; the implementation would add it there.
