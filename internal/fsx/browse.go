//go:build linux

// Browsing the server filesystem, jailed to the configured data roots —
// the backend of GET /fs/roots and GET /fs/browse
// (docs/05-api-contract.md section 7). Listings carry directories only;
// files never appear. Containment is checked after symlink resolution
// (section 7.2) and the directory is read through a descriptor anchored
// at the root, so a component swapped for a symlink between the check and
// the read cannot lift the listing out of the root. The build tag states
// the statfs dependency, as in space.go.
package fsx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// Entry is one directory in a listing. Files are never returned.
type Entry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Writable bool   `json:"writable"`
}

// Listing is the answer of Browse.
type Listing struct {
	Path        string  `json:"path"`
	Parent      *string `json:"parent"` // nil at a root, so the browser cannot walk upwards
	Separator   string  `json:"separator"`
	Writable    bool    `json:"writable"`
	FreeBytes   int64   `json:"free_bytes"`
	TotalBytes  int64   `json:"total_bytes"`
	Directories []Entry `json:"directories"`
}

// Browse lists the subdirectories of path. roots is DLTOOL_DATA_ROOTS in
// order. It returns ErrPathRejected outside the roots, and fs.ErrNotExist
// for a readable-root-relative path that does not exist.
func Browse(roots []string, path string, showHidden bool) (Listing, error) {
	// A NUL in a path the user typed rejects the request (doc 12 section
	// 3.2 step 5): a NUL reaching a C open() truncates the path silently.
	if strings.IndexByte(path, 0) >= 0 {
		return Listing{}, ErrPathRejected
	}

	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		return Listing{}, ErrPathRejected
	}

	for _, configured := range roots {
		resolvedRoot, err := resolveExisting(filepath.Clean(configured))
		if err != nil || !within(cleaned, resolvedRoot) {
			continue
		}

		return browseRoot(roots, resolvedRoot, cleaned, showHidden)
	}

	return Listing{}, ErrPathRejected
}

// browseRoot lists the directory the cleaned path names inside one
// resolved root. The containment check runs after symlink resolution —
// an in-root symlink stays browsable, one pointing outside answers like a
// ".." path — and the directory is then opened through a descriptor
// anchored at the root, which refuses any escape a swap could smuggle in
// after the check.
func browseRoot(roots []string, resolvedRoot, cleaned string, showHidden bool) (listing Listing, err error) {
	resolved, err := resolveExisting(cleaned)
	if err != nil {
		return Listing{}, fmt.Errorf("fsx: resolve %s: %w", cleaned, err)
	}
	if !within(resolved, resolvedRoot) {
		return Listing{}, ErrPathRejected
	}

	rel, err := filepath.Rel(resolvedRoot, cleaned)
	if err != nil {
		return Listing{}, ErrPathRejected
	}

	rootDir, err := os.OpenRoot(resolvedRoot)
	if err != nil {
		return Listing{}, fmt.Errorf("fsx: open root %s: %w", resolvedRoot, err)
	}
	defer func() {
		if cerr := rootDir.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("fsx: close root %s: %w", resolvedRoot, cerr))
		}
	}()

	// The anchored open re-resolves the path beneath the root descriptor:
	// a symlink swapped in since the resolve above is refused the moment it
	// would leave the root, and an absolute symlink is refused outright.
	dir, err := rootDir.Open(rel)
	if err != nil {
		return Listing{}, mapBrowseOpenError(resolvedRoot, cleaned, err)
	}
	defer func() {
		if cerr := dir.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("fsx: close %s: %w", resolved, cerr))
		}
	}()

	info, err := dir.Stat()
	if err != nil {
		return Listing{}, fmt.Errorf("fsx: stat %s: %w", resolved, err)
	}
	if !info.IsDir() {
		return Listing{}, fs.ErrNotExist
	}

	dirents, err := dir.ReadDir(-1)
	if err != nil {
		return Listing{}, fmt.Errorf("fsx: readdir %s: %w", resolved, err)
	}

	listing = Listing{
		Path:        resolved,
		Separator:   "/",
		Writable:    writable(resolved),
		Directories: []Entry{},
	}
	if resolved != resolvedRoot {
		parent := filepath.Dir(resolved)
		listing.Parent = &parent
	}

	space, err := FreeSpace(resolved)
	if err != nil {
		return Listing{}, fmt.Errorf("fsx: free space at %s: %w", resolved, err)
	}
	listing.FreeBytes = space.FreeBytes
	listing.TotalBytes = space.TotalBytes

	resolvedRoots := resolveRoots(roots)
	for _, dirent := range dirents {
		name := dirent.Name()
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}

		entryPath := filepath.Join(resolved, name)
		if dirent.IsDir() {
			listing.Directories = append(listing.Directories, Entry{
				Name:     name,
				Path:     entryPath,
				Writable: writable(entryPath),
			})
			continue
		}
		if dirent.Type()&os.ModeSymlink == 0 {
			continue // files and special entries are never listed
		}

		// A symlinked entry lists only when its resolved target is a
		// directory that stays inside the configured roots — never follow
		// a symlink out of a root.
		target, err := resolveExisting(entryPath)
		if err != nil || !withinAny(target, resolvedRoots) {
			continue
		}
		tinfo, err := os.Stat(entryPath)
		if err != nil || !tinfo.IsDir() {
			continue
		}
		listing.Directories = append(listing.Directories, Entry{
			Name:     name,
			Path:     entryPath,
			Writable: writable(entryPath),
		})
	}

	// Case-insensitive by name, with the raw name as the tie-break so two
	// case-variants never order nondeterministically.
	slices.SortFunc(listing.Directories, func(a, b Entry) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}

		return strings.Compare(a.Name, b.Name)
	})

	return listing, nil
}

// Roots reports one entry per configured root.
func Roots(roots []string) ([]Listing, error) {
	listings := make([]Listing, 0, len(roots))
	for _, configured := range roots {
		resolved, err := resolveExisting(filepath.Clean(configured))
		if err != nil {
			return nil, fmt.Errorf("fsx: resolve root %s: %w", configured, err)
		}
		space, err := FreeSpace(resolved)
		if err != nil {
			return nil, fmt.Errorf("fsx: free space at %s: %w", resolved, err)
		}
		listings = append(listings, Listing{
			Path:        resolved,
			Separator:   "/",
			Writable:    writable(resolved),
			FreeBytes:   space.FreeBytes,
			TotalBytes:  space.TotalBytes,
			Directories: []Entry{},
		})
	}

	return listings, nil
}

// mapBrowseOpenError translates an anchored-open failure into the
// endpoint's vocabulary: inside a root but absent or unreadable is the
// caller's not-found; anything that resolved outside the root since the
// lexical check is the rejection. A descriptor-level escape the
// resolution can no longer see fails closed as a rejection too.
func mapBrowseOpenError(resolvedRoot, cleaned string, err error) error {
	if resolved, rerr := resolveExisting(cleaned); rerr == nil && !within(resolved, resolvedRoot) {
		return ErrPathRejected
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return err
	}
	if errors.Is(err, unix.ENOTDIR) {
		return fs.ErrNotExist
	}

	return ErrPathRejected
}

// resolveRoots resolves every configured root once; roots that do not
// resolve are dropped from the containment set rather than failing the
// listing.
func resolveRoots(roots []string) []string {
	resolved := make([]string, 0, len(roots))
	for _, configured := range roots {
		r, err := resolveExisting(filepath.Clean(configured))
		if err != nil {
			continue
		}
		resolved = append(resolved, r)
	}

	return resolved
}

// withinAny reports whether path lies inside any resolved root.
func withinAny(path string, resolvedRoots []string) bool {
	for _, root := range resolvedRoots {
		if within(path, root) {
			return true
		}
	}

	return false
}

// writable reports whether the process may write to path.
func writable(path string) bool {
	return unix.Access(path, unix.W_OK) == nil
}
