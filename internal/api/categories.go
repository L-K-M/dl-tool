package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/fsx"
	"github.com/L-K-M/dl-tool/internal/store"
)

const (
	operationListCategories = "list-categories"
	operationCreateCategory = "create-category"
	operationPatchCategory  = "patch-category"
	operationDeleteCategory = "delete-category"
	operationListTags       = "list-tags"

	categoryNameDetail      = "the name must be non-empty and carry no /"
	categoryConflictDetail  = "a category with that name already exists"
	categorySavePathSummary = "save path %q resolves outside every configured data root"
)

// CategoryDTO is one entry of GET /categories and the object POST and
// PATCH return (docs/05-api-contract.md section 8.1). The name is the
// identity: the row's cat_ id never leaves the store.
type CategoryDTO struct {
	Name      string `json:"name"`
	SavePath  string `json:"save_path"`
	TaskCount int    `json:"task_count"`
}

// TagDTO is one entry of GET /tags (docs/05-api-contract.md section 8.2).
type TagDTO struct {
	Name      string `json:"name"`
	TaskCount int    `json:"task_count"`
}

// CreateCategoryInput is the JSON body of POST /categories.
type CreateCategoryInput struct {
	Body struct {
		Name     string `json:"name"      required:"true" minLength:"1" doc:"Unique category name; never carries a /"`
		SavePath string `json:"save_path" required:"true" minLength:"1" doc:"Default destination of tasks created in this category; must resolve inside a configured data root"`
	}
}

// PatchCategoryInput addresses one category by name. The body fields are
// pointers: an omitted field is nil and stays untouched, while an
// explicit "" on either field still answers 422 (doc 05 section 8.1) —
// string+omitempty cannot tell the two apart.
type PatchCategoryInput struct {
	Name string `path:"name" doc:"The category's current name"`
	Body struct {
		NewName  *string `json:"new_name,omitempty" doc:"Rename; must be non-empty and carry no /"`
		SavePath *string `json:"save_path,omitempty" doc:"New default destination; must resolve inside a configured data root"`
	}
}

// DeleteCategoryInput addresses one category by name.
type DeleteCategoryInput struct {
	Name string `path:"name" doc:"The category's name"`
}

// ListCategoriesOutput is the GET /categories body.
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

// ListTagsOutput is the GET /tags body.
type ListTagsOutput struct {
	Body struct {
		Tags []TagDTO `json:"tags"`
	}
}

// CategoryHandlers owns the category operations of doc 05 section 8.1 and
// the tag list of section 8.2. roots is DLTOOL_DATA_ROOTS in configured
// order, for the save_path check — the same jail a task destination gets.
type CategoryHandlers struct {
	settings *store.SettingsStore
	roots    []string
}

// NewCategoryHandlers builds the category handlers over db, exactly like
// NewSettingsHandlers wraps its own SettingsStore.
func NewCategoryHandlers(db *sqlx.DB, roots []string) *CategoryHandlers {
	return &CategoryHandlers{settings: store.NewSettingsStore(db), roots: roots}
}

// Register mounts the five operations on the Huma API;
// Server.registerOperations is the call site.
func (h *CategoryHandlers) Register(hapi huma.API) {
	huma.Register(hapi, huma.Operation{
		OperationID: operationListCategories,
		Method:      http.MethodGet,
		Path:        "/categories",
		Summary:     "List the categories",
		Description: "Every category with the count of its non-removed tasks, sorted by name. A category with no tasks still lists.",
		Tags:        []string{"categories"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.List)

	huma.Register(hapi, huma.Operation{
		OperationID:   operationCreateCategory,
		Method:        http.MethodPost,
		Path:          "/categories",
		DefaultStatus: http.StatusCreated,
		Summary:       "Create a category",
		Description:   "Creates one global category whose save_path becomes the effective destination of a task created in it with no explicit destination. The save_path is resolved against the configured roots like any destination: outside them is 403 /problems/path-rejected. A duplicate name is 409 /problems/conflict.",
		Tags:          []string{"categories"},
		Security:      credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Create)

	huma.Register(hapi, huma.Operation{
		OperationID: operationPatchCategory,
		Method:      http.MethodPatch,
		Path:        "/categories/{name}",
		Summary:     "Update a category",
		Description: "Renames the category or replaces its save_path; omitted fields are untouched. Renaming onto an existing name is 409 /problems/conflict, never a silent merge. No task row and no file is touched: tasks keep their resolved destination, only future creates see the new name or path.",
		Tags:        []string{"categories"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Patch)

	huma.Register(hapi, huma.Operation{
		OperationID:   operationDeleteCategory,
		Method:        http.MethodDelete,
		Path:          "/categories/{name}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Delete a category",
		Description:   "Removes the category row; its tasks and watch folders become uncategorised through ON DELETE SET NULL. No task is removed and no file is touched.",
		Tags:          []string{"categories"},
		Security:      credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Delete)

	huma.Register(hapi, huma.Operation{
		OperationID: operationListTags,
		Method:      http.MethodGet,
		Path:        "/tags",
		Summary:     "List the tags",
		Description: "Every tag with the count of its non-removed tasks, sorted by name ascending and not paginated. A tag with no tasks still lists with task_count 0.",
		Tags:        []string{"tags"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.ListTags)
}

// List serves GET /categories.
func (h *CategoryHandlers) List(ctx context.Context, _ *struct{}) (*ListCategoriesOutput, error) {
	rows, err := h.settings.ListCategories(ctx)
	if err != nil {
		return nil, internalFailure(ctx, "list categories", err)
	}

	output := &ListCategoriesOutput{}
	output.Body.Categories = make([]CategoryDTO, 0, len(rows))
	for _, row := range rows {
		output.Body.Categories = append(output.Body.Categories, categoryDTO(row))
	}

	return output, nil
}

// Create serves POST /categories. The save_path is jailed to the
// configured roots like any destination and stored in resolved form, so a
// category can never point a task outside DLTOOL_DATA_ROOTS.
func (h *CategoryHandlers) Create(ctx context.Context, in *CreateCategoryInput) (*CategoryOutput, error) {
	if !validCategoryName(in.Body.Name) {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, categoryNameDetail)
	}

	resolved, err := h.resolveSavePath(in.Body.SavePath)
	if err != nil {
		return nil, err
	}

	category := store.Category{
		ID:       store.NewID(store.PrefixCategory),
		Name:     in.Body.Name,
		SavePath: resolved,
	}
	if err := h.settings.CreateCategory(ctx, category); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, Problem(SlugConflict, http.StatusConflict, categoryConflictDetail)
		}

		return nil, internalFailure(ctx, "create category", err)
	}

	return &CategoryOutput{Status: http.StatusCreated, Body: categoryDTO(category)}, nil
}

// Patch serves PATCH /categories/{name}. Each provided field is validated
// before the write — a new_name that is empty or carries / is 422, a
// provided save_path goes through the same roots resolution as create's —
// then one UpdateCategory merges them (store.ErrNotFound is 404,
// store.ErrConflict is 409). The response is the row read back under its
// effective name; a read-back ErrNotFound is the row vanishing after a
// committed update, an internal failure rather than a second 404.
func (h *CategoryHandlers) Patch(ctx context.Context, in *PatchCategoryInput) (*CategoryOutput, error) {
	if !validCategoryName(in.Name) {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, categoryNameDetail)
	}
	if in.Body.NewName != nil && !validCategoryName(*in.Body.NewName) {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, categoryNameDetail)
	}

	var resolvedSavePath *string
	if in.Body.SavePath != nil {
		if *in.Body.SavePath == "" {
			return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "the save_path must be non-empty")
		}
		resolved, err := h.resolveSavePath(*in.Body.SavePath)
		if err != nil {
			return nil, err
		}
		resolvedSavePath = &resolved
	}

	if err := h.settings.UpdateCategory(ctx, in.Name, in.Body.NewName, resolvedSavePath); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, Problem(SlugConflict, http.StatusConflict, categoryConflictDetail)
		}

		return nil, FromStore(err)
	}

	effective := in.Name
	if in.Body.NewName != nil {
		effective = *in.Body.NewName
	}
	category, err := h.settings.CategoryByName(ctx, effective)
	if err != nil {
		return nil, internalFailure(ctx, "read back category", err)
	}

	return &CategoryOutput{Status: http.StatusOK, Body: categoryDTO(category)}, nil
}

// Delete serves DELETE /categories/{name}: the row goes, its tasks become
// uncategorised and no task and no file is touched.
func (h *CategoryHandlers) Delete(ctx context.Context, in *DeleteCategoryInput) (*struct{}, error) {
	if !validCategoryName(in.Name) {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, categoryNameDetail)
	}

	if err := h.settings.DeleteCategory(ctx, in.Name); err != nil {
		return nil, FromStore(err)
	}

	return nil, nil
}

// ListTags serves GET /tags: every tag with its non-removed task count,
// sorted by name — including tags whose count is zero.
func (h *CategoryHandlers) ListTags(ctx context.Context, _ *struct{}) (*ListTagsOutput, error) {
	rows, err := h.settings.ListTags(ctx)
	if err != nil {
		return nil, internalFailure(ctx, "list tags", err)
	}

	output := &ListTagsOutput{}
	output.Body.Tags = make([]TagDTO, 0, len(rows))
	for _, row := range rows {
		output.Body.Tags = append(output.Body.Tags, TagDTO{Name: row.Name, TaskCount: row.TaskCount})
	}

	return output, nil
}

// validCategoryName enforces the doc 05 section 8.1 name rule shared by
// create and patch: non-empty and carrying no / — a / would split the
// path segment the name is addressed through.
func validCategoryName(name string) bool {
	return name != "" && !strings.ContainsRune(name, '/')
}

// resolveSavePath jails a requested save_path to the configured roots
// through the same fsx.ResolveDestination a task destination gets, and
// answers 403 /problems/path-rejected for anything outside them.
func (h *CategoryHandlers) resolveSavePath(requested string) (string, error) {
	resolved, err := fsx.ResolveDestination(h.roots, requested)
	if err != nil {
		return "", &huma.ErrorModel{
			Type:   SlugPathRejected,
			Title:  http.StatusText(http.StatusForbidden),
			Status: http.StatusForbidden,
			Detail: fmt.Sprintf(categorySavePathSummary, requested),
			Errors: []*huma.ErrorDetail{{
				Message:  "must resolve inside a configured root",
				Location: "body.save_path",
				Value:    requested,
			}},
		}
	}

	return resolved, nil
}

// categoryDTO renders one store row; the id stays behind the name.
func categoryDTO(c store.Category) CategoryDTO {
	return CategoryDTO{Name: c.Name, SavePath: c.SavePath, TaskCount: c.TaskCount}
}
