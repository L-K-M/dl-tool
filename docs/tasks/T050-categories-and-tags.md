# T050 — Serve category CRUD, the tag list and category path resolution

| Field | Value |
|---|---|
| **ID** | T050 |
| **Milestone** | M3 |
| **Status** | todo |
| **Depends on** | T017, T020, T021 |
| **Blocks** | T049, T053, T073, T107, T119 |
| **Parallel-safe** | no — extends `internal/store/settings.go`, `internal/api/tasks.go` and `internal/api/server.go` |
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
| `internal/store/settings.go` | edit | Category and tag queries, the `default_destination` read, `ErrConflict`. |
| `internal/api/tasks.go` | edit | Resolve a missing destination from the category save path. |
| `internal/api/server.go` | edit | Set a `categories` field via `NewCategoryHandlers(db, cfg.DataRoots)` in `NewServer`; call `s.categories.Register(s.API)` in `registerOperations`. |
| `internal/api/tasks_test.go` | edit | Move the `TestListTasksFilterAndSort` category seed's `save_path` inside the environment's data root; destination resolution makes the column a live input. |

No other file may be modified, apart from the two generated files of
[`docs/13-testing-and-verification.md` §7.1](../13-testing-and-verification.md), this task file's
`## Evidence` section, and this task's row in the task index.

## Interface contract

```go
package store

// Category, Tag and their queries live in settings.go beside the engines
// queries: T027's Files row makes that file the home of every
// configuration-table query, and its existing methods are all
// (s *SettingsStore). (Doc 14 §2.4 names models.go the home of row
// structs; the file this task extends is outside that rule's reach — it is
// not in the Files table — and the implemented stores already colocate row
// structs with their queries: Engine here, Task in tasks.go.)

// ErrConflict is the sentinel a duplicate-name write returns; the handlers
// map it to 409 /problems/conflict. isUniqueViolation already detects the
// driver's SQLITE_CONSTRAINT_UNIQUE for it.
var ErrConflict = errors.New("store: conflict")

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

func (s *SettingsStore) ListCategories(ctx context.Context) ([]Category, error)

// CategoryByName resolves one row by its unique name, carrying the same
// task_count the list does — the PATCH handler builds its 200 response
// from it. ErrNotFound means no category carries it.
func (s *SettingsStore) CategoryByName(ctx context.Context, name string) (Category, error)

// CreateCategory inserts one row; a name already taken is ErrConflict.
func (s *SettingsStore) CreateCategory(ctx context.Context, c Category) error

// UpdateCategory writes the addressed row's name and save_path; a nil
// argument leaves that column untouched, so the merge happens in the
// UPDATE itself and two concurrent PATCHes cannot lose each other's field.
// ErrNotFound means name addresses no row; ErrConflict means newName
// belongs to another row.
func (s *SettingsStore) UpdateCategory(ctx context.Context, name string, newName, savePath *string) error

// DeleteCategory removes the row. ON DELETE SET NULL uncategorises its
// tasks and watch folders; no task row and no file is touched. ErrNotFound
// means name addresses no row.
func (s *SettingsStore) DeleteCategory(ctx context.Context, name string) error

// ListTags returns every row of tags sorted by name, including tags with no tasks. task_count
// counts every non-removed task carrying the tag.
func (s *SettingsStore) ListTags(ctx context.Context) ([]Tag, error)

// DefaultDestination returns the default_destination settings row's value
// (docs/11-config-reference.md §5). The migration seeds no row: an absent
// row or an empty value returns "" with a nil error, and the caller's
// first-root fallback applies.
func (s *SettingsStore) DefaultDestination(ctx context.Context) (string, error)
```

```go
package api

type CategoryDTO struct {
	Name      string `json:"name"`
	SavePath  string `json:"save_path"`
	TaskCount int    `json:"task_count"`
}

type TagDTO struct {
	Name      string `json:"name"`
	TaskCount int    `json:"task_count"`
}

type CreateCategoryInput struct {
	Body struct {
		Name     string `json:"name"      required:"true" minLength:"1"`
		SavePath string `json:"save_path" required:"true"`
	}
}

// The PATCH fields are pointers: an omitted field is nil and stays
// untouched, while an explicit "" still answers 422 (doc 05 §8.1) —
// string+omitempty cannot tell the two apart.
type PatchCategoryInput struct {
	Name string `path:"name"`
	Body struct {
		NewName  *string `json:"new_name,omitempty"`
		SavePath *string `json:"save_path,omitempty"`
	}
}
type DeleteCategoryInput struct {
	Name string `path:"name"`
}

type ListCategoriesOutput struct {
	Body struct {
		Categories []CategoryDTO `json:"categories"`
	}
}

// CategoryOutput carries 201 from Create and 200 from Patch.
type CategoryOutput struct {
	Status int `json:"-"`
	Body   CategoryDTO
}

type ListTagsOutput struct {
	Body struct {
		Tags []TagDTO `json:"tags"`
	}
}

// CategoryHandlers wraps db in store.NewSettingsStore exactly like
// NewSettingsHandlers does; roots is DLTOOL_DATA_ROOTS in configured
// order, for the save_path check. NewTaskHandlers gains a SettingsStore
// field over the same db — built inside the constructor like its
// TaskStore, so its signature and call site do not change — for the
// DefaultDestination read.
func NewCategoryHandlers(db *sqlx.DB, roots []string) *CategoryHandlers
func (h *CategoryHandlers) Register(api huma.API)

func (h *CategoryHandlers) List(ctx context.Context, in *struct{}) (*ListCategoriesOutput, error)
func (h *CategoryHandlers) Create(ctx context.Context, in *CreateCategoryInput) (*CategoryOutput, error)
func (h *CategoryHandlers) Patch(ctx context.Context, in *PatchCategoryInput) (*CategoryOutput, error)
func (h *CategoryHandlers) Delete(ctx context.Context, in *DeleteCategoryInput) (*struct{}, error)
func (h *CategoryHandlers) ListTags(ctx context.Context, in *struct{}) (*ListTagsOutput, error)
```

The `Register` mounts five operations — `GET`/`POST /categories`, `PATCH`/`DELETE /categories/{name}`
and `GET /tags` — the four category operations tagged `categories`, the tag list tagged `tags`, all
`credentialRequired`, with `DefaultStatus: http.StatusNoContent` on DELETE. Error mapping: `store.ErrNotFound` → `404` (FromStore),
`store.ErrConflict` → `409 /problems/conflict`, `fsx.ErrPathRejected` → `403 /problems/path-rejected`, an
empty or `/`-carrying name → `422 /problems/validation-failed`.

Destination resolution, added to the create path in `internal/api/tasks.go`. The category read switches
to `SettingsStore.CategoryByName` so the save_path is in hand (its `ErrNotFound` maps to the existing
422 for an unknown category; `queryCategoryIDByName` leaves with it). The raw candidate — explicit
destination, else the category save_path, else `DefaultDestination`, else `""` — goes through the one
`fsx.ResolveDestination` call, so every out-of-roots answer is `403 /problems/path-rejected`:

| Request | Effective `destination` | `requested_destination` |
|---|---|---|
| explicit `destination` | that path, through `fsx.ResolveDestination` | the request verbatim when the resolved path differs — an alias or a `..` that folds, or a `create_subfolder` move — else `null` |
| none, category with a `save_path` | the category `save_path`, through `fsx.ResolveDestination` | `null` — nothing was requested |
| none, no category | the `default_destination` settings row read through `SettingsStore.DefaultDestination`, through `fsx.ResolveDestination`; the migration seeds no row and an unset or empty value falls back to the first root | `null` — nothing was requested |

Statuses, exactly doc 05 §8.1: `200`/`201`/`204` ·
`403 /problems/path-rejected` for a `save_path` outside the roots · `404` · `409 /problems/conflict` on a
duplicate name · `422` for an empty name or a name containing `/`.

## Steps
1. Add `Category`, `Tag`, `ErrConflict`, the five category methods, `ListTags` and `DefaultDestination` to
   `internal/store/settings.go` as `(s *SettingsStore)` methods, computing `task_count` with a `LEFT JOIN`
   over non-`removed` tasks in one statement, never one query per row.
2. Create `internal/api/categories.go` with `CategoryHandlers`, its constructor and `Register`, mirroring
   the shape T027 used for `SettingsHandlers`.
3. Validate `save_path` through `fsx.ResolveDestination` against the configured roots, rejecting anything
   outside with `403 /problems/path-rejected`; store the resolved path, the same treatment a task
   destination gets.
4. Implement `PATCH` by validating each provided field — a `new_name` that is empty or carries `/` is 422,
   a provided `save_path` goes through `fsx.ResolveDestination` like create's — then `UpdateCategory`
   (`ErrNotFound` → 404, `ErrConflict` → 409), then `CategoryByName` on the effective post-update name —
   `newName` when provided, else `name` — for the 200 response; an `ErrNotFound` from that read-back is
   the row vanishing mid-request after a committed update — 500, not another 404.
5. Implement `DELETE` so tasks in the category become uncategorised and no task and no file is touched.
6. Edit `internal/api/tasks.go` to apply the resolution table above when the create body carries a category
   and no destination, setting `requested_destination` only when the resolved path differs from the
   requested one.
7. For `GET /tags`, count every non-removed task carrying the tag, including tags whose count is zero.
   Both list handlers initialise their slice, so an empty result encodes `[]` — matching the array types
   the regenerated `schema.d.ts` declares — never `null`.
8. Edit `internal/api/server.go` to construct the handlers and call `Register`: one `categories` field on
   `Server`, `NewCategoryHandlers(db, cfg.DataRoots)` in `NewServer`, `s.categories.Register(s.API)` in
   `registerOperations`.
9. Create `internal/api/categories_test.go`: create, list, rename, delete; a duplicate name is `409`; a
   name containing `/` is `422`; a `save_path` of `/etc` is `403`; creating a
   task in category `linux` with no destination resolves to `/data/linux`; deleting the category leaves its
   tasks present and uncategorised; `GET /tags` lists a tag with `task_count: 0`; both list endpoints
   answer `[]` when empty, never `null`.
10. Run `make gen` to regenerate `api/openapi.json` and `web/src/api/schema.d.ts` (docs/13 §7.1).
    Run the verification command, paste its output under `## Evidence`, and confirm scope with the
    `git status` command under `## Verification`. Only then commit everything, including the two
    regenerated files.

## Acceptance criteria
- [x] `TestCategoryCrud` and `TestDuplicateCategoryConflicts` pass.
- [x] `TestCategorySavePathResolvesDestination` asserts the destination is `/data/linux`.
- [x] `TestDeleteCategoryKeepsTasks` asserts the task row survives with `category` null.
- [x] `TestListTagsIncludesZeroCount` passes.
- [x] `GET /tags` is not paginated and is sorted by name ascending.

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
Expected: exactly the paths in the Files table plus the two generated files of
docs/13 §7.1 (`api/openapi.json`, `web/src/api/schema.d.ts`), in the sorted order the command
prints, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement `PATCH /tags/{name}` or `DELETE /tags/{name}`; FR-033 belongs to M6's T107.
- Do NOT touch the sidebar; T044 derives its category and tag nodes from the store, and a category with no
  tasks appears once M6 wires `GET /categories` into it.
- Do NOT add a per-category engine, ratio limit or automation setting; v1 has name and save path only.
- Do NOT move or delete any downloaded data when a category is renamed or deleted.
- Do NOT let a category `save_path` escape `DLTOOL_DATA_ROOTS`; it is checked like any destination.
- Do NOT validate `default_destination` at settings-write time; `PATCH /settings` is T092's. A stale
  stored value outside the roots answers `403 /problems/path-rejected` on every destination-less
  create — the create-time check is the only guard this task adds.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
`make lint && make test PKG="./internal/api/... ./internal/store/..." && echo CATEGORIES_OK` on the final
tree — gofmt and golangci-lint clean, eslint and prettier clean, both packages `ok` under `-race`, and
the final line of stdout is `CATEGORIES_OK`:

```text
$ make lint && make test PKG="./internal/api/... ./internal/store/..." && echo CATEGORIES_OK
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
ok  	github.com/L-K-M/dl-tool/internal/api	101.841s
ok  	github.com/L-K-M/dl-tool/internal/store	72.584s
CATEGORIES_OK
```

The five named tests on the same tree, verbose — every one `--- PASS`:

```text
$ go test -race -count=1 -v -run 'TestCategoryCrud|TestDuplicateCategoryConflicts|TestCategorySavePathResolvesDestination|TestDeleteCategoryKeepsTasks|TestListTagsIncludesZeroCount' ./internal/api/
--- PASS: TestCategoryCrud (0.38s)
--- PASS: TestDuplicateCategoryConflicts (0.38s)
--- PASS: TestCategorySavePathResolvesDestination (0.42s)
--- PASS: TestDeleteCategoryKeepsTasks (0.37s)
--- PASS: TestListTagsIncludesZeroCount (0.40s)
ok  	github.com/L-K-M/dl-tool/internal/api	3.076s
```

Scope check on the same tree — exactly the Files table paths plus the two generated files of
docs/13 §7.1, and nothing else:

```text
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
api/openapi.json
internal/api/categories.go
internal/api/categories_test.go
internal/api/server.go
internal/api/tasks.go
internal/api/tasks_test.go
internal/store/settings.go
web/src/api/schema.d.ts
```

`GET /tags` answers an array (never paginated) sorted by name ascending; `TestListTagsIncludesZeroCount`
lists a tag with `task_count: 0`.

## Blocked

Resolved by the plan repair. The record below was merged in pull request #169; the repair amended this
file as it prescribed:

- `internal/api/server.go` joined the `## Files` table with a wiring row (the `categories` field,
  `NewCategoryHandlers(db, cfg.DataRoots)` in `NewServer`, `s.categories.Register(s.API)` in
  `registerOperations`), matching the shape T046's repair added and T065/T068 carry natively, and step 8
  owns it.
- The `## Verification` scope check now names the two generated files of docs/13 §7.1 — registering Huma
  operations necessarily changes `api/openapi.json` and `web/src/api/schema.d.ts` — with the wording
  T046's repaired file uses, and the Files-table note carries the same carve-out.
- The resolution table's third row now names the `SettingsStore.DefaultDestination` read, the
  unset-or-empty first-root fallback (the migration seeds no `default_destination` row), and sends every
  candidate through the one `fsx.ResolveDestination` call, so an out-of-roots configured value is
  `403 /problems/path-rejected`.
- The store contract is `(s *SettingsStore)` methods, not package-level functions — the file is
  `SettingsStore` method territory — with `Category`, `Tag` and the new `ErrConflict` sentinel living in
  `settings.go` (`models.go` is outside the Files table; the implemented stores already colocate row
  structs with their queries). `RenameCategory` became `UpdateCategory`: PATCH also writes save_path.
- `PatchCategoryInput` carries `*string` fields so an omitted field is distinguishable from an explicit
  `""` — which must still answer 422 per doc 05 §8.1.
- `NewTaskHandlers` builds its `SettingsStore` over the same db inside the constructor, like its
  `TaskStore`, so the `default_destination` read changes no signature or call site; the tasks.go
  category read switches to `CategoryByName` so the save_path is in hand.

Original defect, recorded before implementation: `internal/api/server.go` was absent from the `## Files`
table while `Server.registerOperations` is the only composition point where a new operation group can
register — a `CategoryHandlers` built in `categories.go` had no caller, so `/categories` and `/tags`
would never route and no acceptance test could observe them through `server.API`. The same defect T046
recorded and pull request #161 repaired.

The second defect below met the same end: the record merged in pull request #171 and this repair
amended the file as it prescribed.

- `internal/api/tasks_test.go` joined the `## Files` table so the `TestListTasksFilterAndSort` category
  seed can move its `save_path` inside the test environment's data root — `filepath.Join(env.dataRoot,
  "linux")` in place of `/data/linux` — keeping a destination-less create inside the resolution table's
  second row instead of tripping its out-of-roots guard.

Second defect, recorded before implementation: `internal/api/tasks_test.go` is absent from the `## Files`
table while the resolution table this task adds turns the seeded category `save_path` into a live input.
`TestListTasksFilterAndSort` seeds `categories ('linux', '/data/linux')` and creates a task in that
category with no destination — correct under the old code, where the column was inert — but the new rule
resolves `/data/linux` through `fsx.ResolveDestination`, which sits outside the test environment's data
root, so the create answers `403 /problems/path-rejected` and the test cannot pass. The chosen remedy
moves the seeded `save_path` inside the environment's data root — an edit the `## Files` table does not
admit. The repair adds `internal/api/tasks_test.go` to the table with that purpose, the same defect
class this file's first `## Blocked` record carried.
