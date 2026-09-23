package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/store"
)

const (
	operationPatchTag  = "patch-tag"
	operationDeleteTag = "delete-tag"

	tagNameDetail      = "the name must be non-empty and carry no , or /"
	tagConflictDetail  = "a tag with that name already exists"
	tagBadEscapeDetail = "the name is not valid percent-encoding"
)

// PatchTagInput addresses one tag by name; the path segment is
// percent-encoded the way doc 05 section 8.2 addresses it. new_name is
// required: a rename with nothing to rename to is a 422.
type PatchTagInput struct {
	Name string `path:"name" doc:"The tag's current name, percent-encoded"`
	Body struct {
		NewName string `json:"new_name" required:"true" minLength:"1" doc:"Rename; must be non-empty and carry no , or /"`
	}
}

// DeleteTagInput addresses one tag by name for DELETE.
type DeleteTagInput struct {
	Name string `path:"name" doc:"The tag's name, percent-encoded"`
}

// TagOutput is the 200 of PATCH /tags/{name}: the renamed row with its
// task count, the same shape GET /tags lists.
type TagOutput struct {
	Body TagDTO
}

// TagHandlers owns the rename and delete verbs of doc 05 section 8.2 —
// the list lives on CategoryHandlers with the categories it shares a
// file with, and there is no create: POST /tasks and PATCH /tasks/{id}
// mint tag rows implicitly.
type TagHandlers struct {
	settings *store.SettingsStore
}

// NewTagHandlers builds the tag handlers over db.
func NewTagHandlers(db *sqlx.DB) *TagHandlers {
	return &TagHandlers{settings: store.NewSettingsStore(db)}
}

// Register mounts the two operations on the Huma API;
// Server.registerOperations is the call site.
func (h *TagHandlers) Register(hapi huma.API) {
	huma.Register(hapi, huma.Operation{
		OperationID: operationPatchTag,
		Method:      http.MethodPatch,
		Path:        "/tags/{name}",
		Summary:     "Rename a tag",
		Description: "Renames the tag in place, so every task carrying it carries the new name at once; the tag id is unchanged and no task row is touched. Renaming onto an existing name is 409 /problems/conflict, never a silent merge.",
		Tags:        []string{"tags"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.PatchTag)

	huma.Register(hapi, huma.Operation{
		OperationID:   operationDeleteTag,
		Method:        http.MethodDelete,
		Path:          "/tags/{name}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Delete a tag",
		Description:   "Detaches the tag from every task and deletes the row. No task is ever deleted by this call.",
		Tags:          []string{"tags"},
		Security:      credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.DeleteTag)
}

// PatchTag serves PATCH /tags/{name}. The response is the row read back
// under its new name; a read-back ErrNotFound is the row vanishing after
// a committed rename, an internal failure rather than a second 404.
func (h *TagHandlers) PatchTag(ctx context.Context, in *PatchTagInput) (*TagOutput, error) {
	name, err := tagPathName(in.Name)
	if err != nil {
		return nil, err
	}
	if !validTagName(in.Body.NewName) {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, tagNameDetail)
	}

	if err := h.settings.RenameTag(ctx, name, in.Body.NewName); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, Problem(SlugConflict, http.StatusConflict, tagConflictDetail)
		}

		return nil, FromStore(err)
	}

	tag, err := h.settings.TagByName(ctx, in.Body.NewName)
	if err != nil {
		return nil, internalFailure(ctx, "read back tag", err)
	}

	return &TagOutput{Body: TagDTO{Name: tag.Name, TaskCount: tag.TaskCount}}, nil
}

// DeleteTag serves DELETE /tags/{name}: the link rows and the tag row go
// in one transaction and no task is touched.
func (h *TagHandlers) DeleteTag(ctx context.Context, in *DeleteTagInput) (*struct{}, error) {
	name, err := tagPathName(in.Name)
	if err != nil {
		return nil, err
	}

	if err := h.settings.DeleteTag(ctx, name); err != nil {
		return nil, FromStore(err)
	}

	return nil, nil
}

// tagPathName decodes the percent-encoded path segment doc 05 section
// 8.2 addresses a tag through — chi routes on the escaped form, so the
// decoding is this layer's — and applies the name rule to the decoded
// value: a %2F or %2C in the segment is a 422, not a different tag.
func tagPathName(segment string) (string, error) {
	name, err := url.PathUnescape(segment)
	if err != nil {
		return "", Problem(SlugValidationFailed, http.StatusUnprocessableEntity, tagBadEscapeDetail)
	}
	if !validTagName(name) {
		return "", Problem(SlugValidationFailed, http.StatusUnprocessableEntity, tagNameDetail)
	}

	return name, nil
}

// validTagName enforces the doc 05 section 8.2 name rule: non-empty and
// carrying no , or / — a , splits the tag list a task patch parses and a
// / would split the path segment the name is addressed through.
func validTagName(name string) bool {
	return name != "" && !strings.ContainsAny(name, ",/")
}
