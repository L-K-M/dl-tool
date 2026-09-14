package api

import (
	"context"
	"errors"
	"io/fs"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/L-K-M/dl-tool/internal/fsx"
)

const (
	operationListFSRoots = "list-fs-roots"
	operationBrowseFS    = "browse-fs"
)

// FSHandlers owns the filesystem operations of doc 05 section 7: the
// configured roots and the directory-only browse, both jailed to
// DLTOOL_DATA_ROOTS by fsx.
type FSHandlers struct {
	roots []string
}

// NewFSHandlers builds the filesystem handlers over the configured data
// roots. roots is DLTOOL_DATA_ROOTS in order — the containment set every
// request path is checked against, never a value the request supplies.
func NewFSHandlers(roots []string) *FSHandlers {
	return &FSHandlers{roots: roots}
}

// Register mounts list-fs-roots and browse-fs on the Huma API;
// Server.registerOperations is the call site.
func (h *FSHandlers) Register(hapi huma.API) {
	huma.Register(hapi, huma.Operation{
		OperationID: operationListFSRoots,
		Method:      http.MethodGet,
		Path:        "/fs/roots",
		Summary:     "List the configured data roots",
		Description: "Every DLTOOL_DATA_ROOTS entry with its writability and free space; the folder browser opens at these. The list never probes past statfs — a missing root cannot slow the page down.",
		Tags:        []string{"filesystem"},
		Security:    credentialRequired,
	}, h.ListRoots)

	huma.Register(hapi, huma.Operation{
		OperationID: operationBrowseFS,
		Method:      http.MethodGet,
		Path:        "/fs/browse",
		Summary:     "List the directories under a path",
		Description: "Directories only, jailed to the configured roots: the path is resolved with its symlinks and a resolution that leaves the roots is 403 /problems/path-rejected, never a filtered listing. parent is null at a root, so a client cannot walk upwards. show_hidden includes dot-directories.",
		Tags:        []string{"filesystem"},
		Security:    credentialRequired,
		// Same strictness as every other query-carrying operation: a
		// mistyped query key is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Browse)
}

// FSRoot is one entry of GET /fs/roots.
type FSRoot struct {
	Path       string `json:"path"`
	Writable   bool   `json:"writable"`
	FreeBytes  int64  `json:"free_bytes"`
	TotalBytes int64  `json:"total_bytes"`
}

// RootsOutput is the GET /fs/roots body.
type RootsOutput struct {
	Body struct {
		Roots []FSRoot `json:"roots"`
	}
}

// BrowseInput carries the query of GET /fs/browse.
type BrowseInput struct {
	Path       string `query:"path" required:"true" doc:"Absolute path to list; resolved with its symlinks and must land inside a configured root"`
	ShowHidden bool   `query:"show_hidden"          doc:"Include dot-directories"`
}

// BrowseOutput is the GET /fs/browse body — the fsx listing verbatim.
type BrowseOutput struct {
	Body fsx.Listing
}

// ListRoots serves GET /fs/roots.
func (h *FSHandlers) ListRoots(ctx context.Context, _ *struct{}) (*RootsOutput, error) {
	listings, err := fsx.Roots(h.roots)
	if err != nil {
		return nil, internalFailure(ctx, "list roots", err)
	}

	output := &RootsOutput{}
	output.Body.Roots = make([]FSRoot, 0, len(listings))
	for _, listing := range listings {
		output.Body.Roots = append(output.Body.Roots, FSRoot{
			Path:       listing.Path,
			Writable:   listing.Writable,
			FreeBytes:  listing.FreeBytes,
			TotalBytes: listing.TotalBytes,
		})
	}

	return output, nil
}

// Browse serves GET /fs/browse. The request path is never concatenated
// onto anything — every resolution goes through fsx (NFR-014).
func (h *FSHandlers) Browse(ctx context.Context, in *BrowseInput) (*BrowseOutput, error) {
	listing, err := fsx.Browse(h.roots, in.Path, in.ShowHidden)
	if err != nil {
		switch {
		case errors.Is(err, fsx.ErrPathRejected):
			return nil, Problem(SlugPathRejected, http.StatusForbidden, "the path is outside the configured data roots")
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission):
			return nil, Problem(SlugNotFound, http.StatusNotFound, "the path does not exist or is not a readable directory")
		default:
			return nil, internalFailure(ctx, "browse filesystem", err)
		}
	}

	return &BrowseOutput{Body: listing}, nil
}
