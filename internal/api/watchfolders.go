package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/fsx"
	"github.com/L-K-M/dl-tool/internal/jobs"
	"github.com/L-K-M/dl-tool/internal/store"
)

const (
	operationListWatchFolders  = "list-watch-folders"
	operationCreateWatchFolder = "create-watch-folder"
	operationPatchWatchFolder  = "patch-watch-folder"
	operationDeleteWatchFolder = "delete-watch-folder"
	operationScanWatchFolder   = "scan-watch-folder"

	watchFolderCategoryDetail = "category does not exist"
	watchFolderEmptyDetail    = "must be non-empty when provided"
	watchFolderPathSummary    = "watch folder %s %q resolves outside every configured data root"
	watchFolderPathConflict   = "a watch folder on that path already exists"

	// defaultPollIntervalS is the fallback sweep cadence doc 05 section 15
	// fixes at 10 — used when inotify registration fails — and the value a
	// create without poll_interval_s stores.
	defaultPollIntervalS = 10
)

// WatchFolderView is the object of doc 05 section 15: one watch_folders
// row with the category name joined in. The timestamps render RFC 3339 —
// the API's time format — from the stored Unix milliseconds.
type WatchFolderView struct {
	ID              string     `json:"id"`
	Path            string     `json:"path"`
	Enabled         bool       `json:"enabled"`
	Destination     string     `json:"destination"`
	Category        *string    `json:"category"`
	DeleteAfterLoad bool       `json:"delete_after_load"`
	PollIntervalS   int        `json:"poll_interval_s"`
	LastScanAt      *time.Time `json:"last_scan_at" format:"date-time"`
	LastError       *string    `json:"last_error"`
	CreatedAt       time.Time  `json:"created_at"    format:"date-time"`
	UpdatedAt       time.Time  `json:"updated_at"    format:"date-time"`
}

// CreateWatchFolderInput is the JSON body of POST /watch-folders.
// path and destination are required; every other member defaults the way
// doc 05 section 15 spells out.
type CreateWatchFolderInput struct {
	Body struct {
		Path            string  `json:"path"        required:"true" minLength:"1" doc:"Directory the loader watches; must resolve inside a configured data root"`
		Enabled         *bool   `json:"enabled,omitempty"          doc:"Default true; a disabled folder is not watched but still scans on demand"`
		Destination     string  `json:"destination" required:"true" minLength:"1" doc:"Destination of tasks the folder creates; must resolve inside a configured data root"`
		Category        *string `json:"category,omitempty"         doc:"Category name the created tasks carry; must exist"`
		DeleteAfterLoad *bool   `json:"delete_after_load,omitempty" doc:"Unlink the source .torrent after the engine accepts the task; default false"`
		PollIntervalS   *int    `json:"poll_interval_s,omitempty" minimum:"1" doc:"Polling fallback interval in seconds; default 10"`
	}
}

// PatchWatchFolderInput addresses one folder; every body member is
// optional and an omitted member stays untouched. category is a
// json.RawMessage rather than a *string because the member has three
// wire states — absent leaves the stored category, null clears it and a
// string replaces it — and *string collapses null and absent into the
// same nil.
type PatchWatchFolderInput struct {
	ID   string `path:"id" doc:"The wfd_… folder id"`
	Body struct {
		Path            *string             `json:"path,omitempty"        minLength:"1" doc:"New watched directory; must resolve inside a configured data root"`
		Enabled         *bool               `json:"enabled,omitempty"`
		Destination     *string             `json:"destination,omitempty" minLength:"1" doc:"New destination; must resolve inside a configured data root"`
		Category        watchFolderCategory `json:"category,omitempty"         doc:"Category name, or null to clear"`
		DeleteAfterLoad *bool               `json:"delete_after_load,omitempty"`
		PollIntervalS   *int                `json:"poll_interval_s,omitempty" minimum:"1" doc:"Polling fallback interval in seconds"`
	}
}

// watchFolderCategory is the tri-state category member of the patch body.
// On decode it keeps the raw JSON — absent, null or a string, the
// tri-state the patch reads — and Schema publishes the contract as
// string|null.
type watchFolderCategory json.RawMessage

// UnmarshalJSON stores the member's raw bytes verbatim.
func (c *watchFolderCategory) UnmarshalJSON(raw []byte) error {
	return (*json.RawMessage)(c).UnmarshalJSON(raw)
}

// MarshalJSON re-emits the stored raw bytes; the member is write-only,
// so this runs only for schema machinery, never a folder response.
func (c watchFolderCategory) MarshalJSON() ([]byte, error) {
	return json.RawMessage(c).MarshalJSON()
}

// Schema publishes the wire contract: a category name, or null to clear.
func (watchFolderCategory) Schema(_ huma.Registry) *huma.Schema {
	return &huma.Schema{Type: huma.TypeString, Nullable: true}
}

// DeleteWatchFolderInput addresses one folder for DELETE.
type DeleteWatchFolderInput struct {
	ID string `path:"id" doc:"The wfd_… folder id"`
}

// ScanWatchFolderInput addresses one folder for an on-demand scan.
type ScanWatchFolderInput struct {
	ID string `path:"id" doc:"The wfd_… folder id"`
}

// ListWatchFoldersOutput is the GET /watch-folders body.
type ListWatchFoldersOutput struct {
	Body struct {
		WatchFolders []WatchFolderView `json:"watch_folders"`
	}
}

// WatchFolderOutput carries 201 from Create and 200 from Patch.
type WatchFolderOutput struct {
	Status int `json:"-"`
	Body   WatchFolderView
}

// ScanWatchFolderOutput is jobs.ScanResult verbatim — the body of
// POST /watch-folders/{id}/scan.
type ScanWatchFolderOutput struct {
	Body jobs.ScanResult
}

// WatchFolderHandlers owns the /watch-folders operations of doc 05
// section 15. roots is DLTOOL_DATA_ROOTS in configured order, for the
// path and destination jail; watcher is the T083 loader Scan delegates
// to, so an on-demand scan runs the identical sweep a tick does.
type WatchFolderHandlers struct {
	settings *store.SettingsStore
	roots    []string
	watcher  *jobs.Watcher
}

// NewWatchFolderHandlers builds the watch-folder handlers over db.
func NewWatchFolderHandlers(db *sqlx.DB, roots []string, watcher *jobs.Watcher) *WatchFolderHandlers {
	return &WatchFolderHandlers{settings: store.NewSettingsStore(db), roots: roots, watcher: watcher}
}

// Register mounts the five operations on the Huma API;
// Server.registerOperations is the call site.
func (h *WatchFolderHandlers) Register(hapi huma.API) {
	huma.Register(hapi, huma.Operation{
		OperationID: operationListWatchFolders,
		Method:      http.MethodGet,
		Path:        "/watch-folders",
		Summary:     "List the watch folders",
		Description: "Every watch_folders row, enabled or not, oldest first. A disabled folder still lists so the scan button can reach it.",
		Tags:        []string{"watch-folders"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.List)

	huma.Register(hapi, huma.Operation{
		OperationID:   operationCreateWatchFolder,
		Method:        http.MethodPost,
		Path:          "/watch-folders",
		DefaultStatus: http.StatusCreated,
		Summary:       "Create a watch folder",
		Description:   "Watches one directory for dropped .torrent files, which become tasks through the ordinary creation path. path and destination are resolved against the configured roots; outside them is 403 /problems/path-rejected. A second folder on the same path is 409 /problems/conflict; an unknown category or a poll_interval_s below 1 is 422.",
		Tags:          []string{"watch-folders"},
		Security:      credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Create)

	huma.Register(hapi, huma.Operation{
		OperationID: operationPatchWatchFolder,
		Method:      http.MethodPatch,
		Path:        "/watch-folders/{id}",
		Summary:     "Update a watch folder",
		Description: "Partial update of path, enabled, destination, category, delete_after_load and poll_interval_s; omitted members are untouched and a null category clears it. Provided paths resolve against the configured roots like create's; a path another folder watches is 409 /problems/conflict.",
		Tags:        []string{"watch-folders"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Patch)

	huma.Register(hapi, huma.Operation{
		OperationID:   operationDeleteWatchFolder,
		Method:        http.MethodDelete,
		Path:          "/watch-folders/{id}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Delete a watch folder",
		Description:   "Removes the row so the loader stops watching the directory. The directory and its contents are never touched.",
		Tags:          []string{"watch-folders"},
		Security:      credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Delete)

	huma.Register(hapi, huma.Operation{
		OperationID: operationScanWatchFolder,
		Method:      http.MethodPost,
		Path:        "/watch-folders/{id}/scan",
		Summary:     "Scan a watch folder now",
		Description: "Scans that one directory immediately — the same synchronous, idempotent sweep the loader runs on a tick — and returns what it did: every file counted, the task ids created and the skipped files with their reason. A disabled folder still scans; the scan is what the button does, not the schedule.",
		Tags:        []string{"watch-folders"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Scan)
}

// List serves GET /watch-folders: every row, oldest first.
func (h *WatchFolderHandlers) List(ctx context.Context, _ *struct{}) (*ListWatchFoldersOutput, error) {
	rows, err := h.settings.ListWatchFolders(ctx)
	if err != nil {
		return nil, internalFailure(ctx, "list watch folders", err)
	}

	output := &ListWatchFoldersOutput{}
	output.Body.WatchFolders = make([]WatchFolderView, 0, len(rows))
	for _, row := range rows {
		output.Body.WatchFolders = append(output.Body.WatchFolders, watchFolderView(row))
	}

	return output, nil
}

// Create serves POST /watch-folders. path and destination are jailed to
// the configured roots and stored in resolved form, so the loader can
// never sweep — nor a created task land — outside DLTOOL_DATA_ROOTS.
// The response is the row read back.
func (h *WatchFolderHandlers) Create(ctx context.Context, in *CreateWatchFolderInput) (*WatchFolderOutput, error) {
	path, err := h.resolvePath("path", in.Body.Path)
	if err != nil {
		return nil, err
	}
	destination, err := h.resolvePath("destination", in.Body.Destination)
	if err != nil {
		return nil, err
	}

	var categoryID *string
	if in.Body.Category != nil {
		categoryID, err = h.resolveCategory(ctx, *in.Body.Category)
		if err != nil {
			return nil, err
		}
	}

	enabled := 1
	if in.Body.Enabled != nil && !*in.Body.Enabled {
		enabled = 0
	}
	deleteAfterLoad := 0
	if in.Body.DeleteAfterLoad != nil && *in.Body.DeleteAfterLoad {
		deleteAfterLoad = 1
	}
	pollInterval := defaultPollIntervalS
	if in.Body.PollIntervalS != nil {
		pollInterval = *in.Body.PollIntervalS
	}

	folder := store.WatchFolder{
		ID:              store.NewID(store.PrefixWatchFolder),
		Path:            path,
		Enabled:         enabled,
		Destination:     destination,
		DeleteAfterLoad: deleteAfterLoad,
		PollIntervalS:   pollInterval,
	}
	if err := h.settings.CreateWatchFolder(ctx, folder, categoryID); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, Problem(SlugConflict, http.StatusConflict, watchFolderPathConflict)
		}

		return nil, internalFailure(ctx, "create watch folder", err)
	}

	created, err := h.settings.GetWatchFolder(ctx, folder.ID)
	if err != nil {
		return nil, internalFailure(ctx, "read back watch folder", err)
	}

	return &WatchFolderOutput{Status: http.StatusCreated, Body: watchFolderView(created)}, nil
}

// Patch serves PATCH /watch-folders/{id}. The row is fetched first so a
// missing id is 404 before any body member is judged; each provided
// member is then validated and merged by one UpdateWatchFolder — the
// response is the row read back, and a read-back ErrNotFound is the row
// vanishing after a committed update, an internal failure rather than a
// second 404.
func (h *WatchFolderHandlers) Patch(ctx context.Context, in *PatchWatchFolderInput) (*WatchFolderOutput, error) {
	if _, err := h.settings.GetWatchFolder(ctx, in.ID); err != nil {
		return nil, FromStore(err)
	}

	patch := store.WatchFolderPatch{}
	if in.Body.Path != nil {
		resolved, err := h.resolveProvidedPath("path", *in.Body.Path)
		if err != nil {
			return nil, err
		}
		patch.Path = &resolved
	}
	if in.Body.Enabled != nil {
		flag := 0
		if *in.Body.Enabled {
			flag = 1
		}
		patch.Enabled = &flag
	}
	if in.Body.Destination != nil {
		resolved, err := h.resolveProvidedPath("destination", *in.Body.Destination)
		if err != nil {
			return nil, err
		}
		patch.Destination = &resolved
	}
	if raw := json.RawMessage(in.Body.Category); len(raw) > 0 {
		patch.CategorySet = true
		if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			var name string
			if err := json.Unmarshal(raw, &name); err != nil {
				return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, watchFolderCategoryDetail)
			}
			categoryID, err := h.resolveCategory(ctx, name)
			if err != nil {
				return nil, err
			}
			patch.CategoryID = categoryID
		}
	}
	if in.Body.DeleteAfterLoad != nil {
		flag := 0
		if *in.Body.DeleteAfterLoad {
			flag = 1
		}
		patch.DeleteAfterLoad = &flag
	}
	if in.Body.PollIntervalS != nil {
		patch.PollIntervalS = in.Body.PollIntervalS
	}

	if err := h.settings.UpdateWatchFolder(ctx, in.ID, patch); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, Problem(SlugConflict, http.StatusConflict, watchFolderPathConflict)
		}

		return nil, FromStore(err)
	}

	updated, err := h.settings.GetWatchFolder(ctx, in.ID)
	if err != nil {
		// A concurrent delete landing between the committed update and
		// the read is "the resource no longer exists", not a server bug.
		if errors.Is(err, store.ErrNotFound) {
			return nil, FromStore(err)
		}

		return nil, internalFailure(ctx, "read back watch folder", err)
	}

	return &WatchFolderOutput{Status: http.StatusOK, Body: watchFolderView(updated)}, nil
}

// Delete serves DELETE /watch-folders/{id}: the row goes and the
// directory is never touched.
func (h *WatchFolderHandlers) Delete(ctx context.Context, in *DeleteWatchFolderInput) (*struct{}, error) {
	if err := h.settings.DeleteWatchFolder(ctx, in.ID); err != nil {
		return nil, FromStore(err)
	}

	return nil, nil
}

// Scan serves POST /watch-folders/{id}/scan: one synchronous sweep of
// exactly this folder through the T083 loader, its ScanResult returned
// unchanged. A disabled folder still scans — the scan is what the
// button does, not the schedule.
func (h *WatchFolderHandlers) Scan(ctx context.Context, in *ScanWatchFolderInput) (*ScanWatchFolderOutput, error) {
	result, err := h.watcher.ScanOnce(ctx, in.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, FromStore(err)
		}
		// A directory deleted or denied between the folder's creation and
		// this scan is operator-fixable — repoint or delete the row — so
		// the answer is 422, not a server error.
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity,
				"the folder's directory could not be read: "+pathErr.Err.Error())
		}

		return nil, internalFailure(ctx, "scan watch folder "+in.ID, err)
	}

	return &ScanWatchFolderOutput{Body: result}, nil
}

// resolveCategory resolves a category name to its row id; an unknown
// name is the 422 of doc 05 section 15.
func (h *WatchFolderHandlers) resolveCategory(ctx context.Context, name string) (*string, error) {
	category, err := h.settings.CategoryByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, watchFolderCategoryDetail)
	}
	if err != nil {
		return nil, internalFailure(ctx, "resolve category", err)
	}

	return &category.ID, nil
}

// resolveProvidedPath applies the create-time requirement to a patched
// path: an explicitly empty value is a 422 — ResolveDestination would
// silently answer the first root — anything else resolves and jails the
// same way.
func (h *WatchFolderHandlers) resolveProvidedPath(field, requested string) (string, error) {
	if requested == "" {
		return "", Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "the "+field+" "+watchFolderEmptyDetail)
	}

	return h.resolvePath(field, requested)
}

// resolvePath jails a requested path or destination to the configured
// roots through the same fsx.ResolveDestination a task destination gets,
// and answers 403 /problems/path-rejected for anything outside them.
func (h *WatchFolderHandlers) resolvePath(field, requested string) (string, error) {
	resolved, err := fsx.ResolveDestination(h.roots, requested)
	if err != nil {
		return "", &huma.ErrorModel{
			Type:   SlugPathRejected,
			Title:  http.StatusText(http.StatusForbidden),
			Status: http.StatusForbidden,
			Detail: fmt.Sprintf(watchFolderPathSummary, field, requested),
			Errors: []*huma.ErrorDetail{{
				Message:  "must resolve inside a configured root",
				Location: "body." + field,
				Value:    requested,
			}},
		}
	}

	return resolved, nil
}

// watchFolderView renders one store row; the stored Unix milliseconds
// become the API's RFC 3339 timestamps.
func watchFolderView(f store.WatchFolder) WatchFolderView {
	view := WatchFolderView{
		ID:              f.ID,
		Path:            f.Path,
		Enabled:         f.Enabled != 0,
		Destination:     f.Destination,
		Category:        f.Category,
		DeleteAfterLoad: f.DeleteAfterLoad != 0,
		PollIntervalS:   f.PollIntervalS,
		LastError:       f.LastError,
		CreatedAt:       time.UnixMilli(f.CreatedAt).UTC(),
		UpdatedAt:       time.UnixMilli(f.UpdatedAt).UTC(),
	}
	if f.LastScanAt != nil {
		at := time.UnixMilli(*f.LastScanAt).UTC()
		view.LastScanAt = &at
	}

	return view
}
