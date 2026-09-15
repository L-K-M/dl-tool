package api

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/sys/unix"

	"github.com/L-K-M/dl-tool/internal/fsx"
)

const (
	operationListFSRoots = "list-fs-roots"
	operationBrowseFS    = "browse-fs"
	operationMkdirFS     = "mkdir-fs"
	operationFSFreeSpace = "free-space-fs"
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
		// Same strictness as browse: a mistyped query key is 422.
		RejectUnknownQueryParameters: true,
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

	huma.Register(hapi, huma.Operation{
		OperationID:   operationMkdirFS,
		Method:        http.MethodPost,
		Path:          "/fs/mkdir",
		DefaultStatus: http.StatusCreated,
		Summary:       "Create one directory inside a data root",
		Description:   "Creates name inside the resolved path with the process umask — mkdir sets no mode of its own. The path must resolve inside the configured roots (403 /problems/path-rejected); a name containing a separator or .. is 422 /problems/validation-failed and an existing name is 409 /problems/conflict.",
		Tags:          []string{"filesystem"},
		Security:      credentialRequired,
		// Same strictness as browse: a mistyped query key is 422.
		RejectUnknownQueryParameters: true,
	}, h.Mkdir)

	huma.Register(hapi, huma.Operation{
		OperationID: operationFSFreeSpace,
		Method:      http.MethodGet,
		Path:        "/fs/free-space",
		Summary:     "Report free and total bytes for a path",
		Description: "The statfs answer for the filesystem holding the path, in plain integer bytes — never KB, never a float. The path is resolved against the configured roots the same way browse resolves it; outside them is 403 /problems/path-rejected. A path that does not exist yet reports the filesystem of its nearest existing ancestor, so the browser can quote space for a destination still to be created.",
		Tags:        []string{"filesystem"},
		Security:    credentialRequired,
		// Same strictness as browse: a mistyped query key is 422.
		RejectUnknownQueryParameters: true,
	}, h.FreeSpace)
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

// MkdirInput is the POST /fs/mkdir body: path is the directory to create
// in, name the single new component.
type MkdirInput struct {
	Body struct {
		Path string `json:"path" required:"true" doc:"Directory to create in; resolved with its symlinks and must land inside a configured root"`
		Name string `json:"name" required:"true" doc:"Single new path component — never a path itself"`
	}
}

// MkdirOutput is the POST /fs/mkdir answer, HTTP 201.
type MkdirOutput struct {
	Status int `json:"-" enum:"201" doc:"Created"`
	Body   struct {
		Path     string `json:"path"`
		Writable bool   `json:"writable"`
	}
}

// FreeSpaceInput carries the query of GET /fs/free-space.
type FreeSpaceInput struct {
	Path string `query:"path" required:"true" doc:"Absolute path to report on; resolved with its symlinks and must land inside a configured root"`
}

// FreeSpaceOutput is the GET /fs/free-space body — plain integer bytes.
type FreeSpaceOutput struct {
	Body struct {
		Path       string `json:"path"`
		FreeBytes  int64  `json:"free_bytes"`
		TotalBytes int64  `json:"total_bytes"`
	}
}

// Mkdir serves POST /fs/mkdir. name is one path component — a separator
// or a ".." is a validation failure, not a sanitisation job — and the
// create runs anchored at the resolved configured root through
// fsx.MkdirBeneath, so a component of the parent swapped for a symlink
// after the containment check cannot lift the new directory out of the
// root: the anchored open refuses the escape instead of redirecting the
// create. The process umask decides the mode.
func (h *FSHandlers) Mkdir(ctx context.Context, in *MkdirInput) (*MkdirOutput, error) {
	name := in.Body.Name
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') || strings.ContainsRune(name, 0) {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity,
			"name must be a single path component")
	}

	root, resolved, err := fsx.ResolveDestinationRoot(h.roots, in.Body.Path)
	if err != nil {
		return nil, Problem(SlugPathRejected, http.StatusForbidden,
			"the path is outside the configured data roots")
	}

	// The parent is already proven inside the roots, so a SafeJoin
	// rejection can only come from the name — a validation answer.
	joined, err := fsx.SafeJoin(resolved, []string{name})
	if err != nil {
		switch {
		case errors.Is(err, fsx.ErrPathRejected):
			return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity,
				"name must be a single path component")
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission), errors.Is(err, unix.ENOTDIR):
			return nil, Problem(SlugNotFound, http.StatusNotFound,
				"the path does not exist or is not a readable directory")
		default:
			return nil, internalFailure(ctx, "resolve mkdir name", err)
		}
	}

	// 0o777 before umask: mkdir applies the process umask and sets no
	// mode of its own (doc 05 section 7.1).
	merr := fsx.MkdirBeneath(root, resolved, filepath.Base(joined), 0o777)
	if merr != nil {
		switch {
		case errors.Is(merr, fsx.ErrPathRejected):
			// A component of the parent resolved to a symlink that leaves
			// the root between the containment check and the anchored
			// open — the swap the root-anchored create exists to refuse.
			return nil, Problem(SlugPathRejected, http.StatusForbidden,
				"the path is outside the configured data roots")
		case errors.Is(merr, fs.ErrExist):
			return nil, Problem(SlugConflict, http.StatusConflict,
				"a file or directory with that name already exists")
		case errors.Is(merr, unix.ENAMETOOLONG), errors.Is(merr, unix.EINVAL):
			// The sanitised name is capped under ext4's 255, but a
			// filesystem with a tighter NAME_MAX (eCryptfs, some FUSE)
			// still answers ENAMETOOLONG — a client-input answer, not a
			// server fault.
			return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity,
				"name is too long or invalid for this filesystem")
		case errors.Is(merr, fs.ErrNotExist), errors.Is(merr, fs.ErrPermission), errors.Is(merr, unix.ENOTDIR):
			return nil, Problem(SlugNotFound, http.StatusNotFound,
				"the path does not exist or is not a readable directory")
		default:
			return nil, internalFailure(ctx, "mkdir", merr)
		}
	}

	output := &MkdirOutput{Status: http.StatusCreated}
	output.Body.Path = joined
	output.Body.Writable = unix.Access(joined, unix.W_OK) == nil

	return output, nil
}

// FreeSpace serves GET /fs/free-space: the fsx statfs answer in bytes,
// unchanged.
func (h *FSHandlers) FreeSpace(ctx context.Context, in *FreeSpaceInput) (*FreeSpaceOutput, error) {
	resolved, err := fsx.ResolveDestination(h.roots, in.Path)
	if err != nil {
		return nil, Problem(SlugPathRejected, http.StatusForbidden,
			"the path is outside the configured data roots")
	}

	// fsx climbs a missing leaf to its nearest existing ancestor (a
	// destination may not exist yet), but fails closed — with an
	// unwrapped error — when that ancestor or the resolved path itself
	// is a regular file. Classify it here so the endpoint answers the
	// same 404 its siblings do for a path that cannot be a directory.
	if info, err := os.Stat(resolved); err == nil && !info.IsDir() ||
		errors.Is(err, unix.ENOTDIR) ||
		errors.Is(err, fs.ErrPermission) {
		return nil, Problem(SlugNotFound, http.StatusNotFound,
			"the path does not exist or is not a readable directory")
	}

	space, err := fsx.FreeSpace(resolved)
	if err != nil {
		// The resolved path's ancestors may vanish or refuse the stat
		// between resolution and statfs — the same not-found the sibling
		// endpoints answer for a directory that is gone or unreadable.
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) ||
			errors.Is(err, unix.ENOTDIR) {
			return nil, Problem(SlugNotFound, http.StatusNotFound,
				"the path does not exist or is not a readable directory")
		}

		return nil, internalFailure(ctx, "free space", err)
	}

	output := &FreeSpaceOutput{}
	output.Body.Path = resolved
	output.Body.FreeBytes = space.FreeBytes
	output.Body.TotalBytes = space.TotalBytes

	return output, nil
}
