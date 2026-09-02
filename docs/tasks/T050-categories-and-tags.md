# T050 — Serve category CRUD, the tag list and category path resolution

| Field | Value |
|---|---|
| **ID** | T050 |
| **Milestone** | M3 |
| **Status** | todo |
| **Depends on** | T017, T020, T021 |
| **Blocks** | T053, T073, T107, T119 |
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
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
