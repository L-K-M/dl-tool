// Package fsx confines every path dl-tool acts on to the configured data
// roots (DLTOOL_DATA_ROOTS). T020 lands the destination resolver; T046
// extends the package with per-segment sanitisation and the hostile-path
// table of docs/12-security-and-threat-model.md section 3.4.
package fsx

import (
	"errors"
	"path/filepath"
	"strings"
)

// ErrPathRejected is returned when a path resolves outside every configured
// root. The API maps it to 403 /problems/path-rejected.
var ErrPathRejected = errors.New("fsx: path rejected")

// ResolveDestination resolves requested against the configured roots,
// following symlinks, and returns the cleaned absolute path. roots is
// DLTOOL_DATA_ROOTS in order. An empty requested returns the first root.
//
// Containment is judged only after symlink resolution, so a destination that
// walks through a link pointing outside the roots is rejected even though
// its textual form stays inside. Resolution happens once, here: a component
// swapped for a symlink after this check (TOCTOU) is not detected, so T046
// must re-anchor containment on an open root descriptor (os.Root / openat)
// before engines write. The request is never joined onto a root: the
// caller's path is resolved on its own and then checked, so no request
// input can build a path by concatenation.
func ResolveDestination(roots []string, requested string) (string, error) {
	if len(roots) == 0 {
		return "", ErrPathRejected
	}
	if requested == "" {
		return filepath.Clean(roots[0]), nil
	}

	// Abs cleans as well, so any ".." in the request is folded away before
	// anything is compared or resolved.
	abs, err := filepath.Abs(requested)
	if err != nil {
		return "", ErrPathRejected
	}
	resolved, err := resolveExisting(abs)
	if err != nil {
		return "", ErrPathRejected
	}

	for _, root := range roots {
		resolvedRoot, err := resolveExisting(filepath.Clean(root))
		if err != nil {
			continue
		}
		if within(resolved, resolvedRoot) {
			return resolved, nil
		}
	}

	return "", ErrPathRejected
}

// within reports whether path is root itself or lies beneath it. Both sides
// arrive cleaned and symlink-resolved; the separator keeps /data/iso2 from
// matching the root /data/iso.
func within(path, root string) bool {
	if path == root {
		return true
	}

	// A cleaned root only ends in a separator when it is the filesystem (or
	// volume) root, e.g. "/"; appending another separator would then match
	// nothing and every destination under it would be rejected.
	sep := string(filepath.Separator)
	if strings.HasSuffix(root, sep) {
		return strings.HasPrefix(path, root)
	}

	return strings.HasPrefix(path, root+sep)
}

// resolveExisting runs filepath.EvalSymlinks on path and, when trailing
// components do not exist yet — a destination the transfer will create —
// resolves the deepest existing ancestor and re-attaches the missing tail
// lexically. The tail cannot hide a symlink (nothing below the first missing
// component exists), and Abs has already cleaned the path, so it cannot
// smuggle ".." past the containment check either.
func resolveExisting(path string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, nil
	}

	parent, tail := filepath.Split(path)
	parent = filepath.Clean(parent)
	if parent == path {
		// path is the filesystem root; nothing left to fall back to.
		return "", ErrPathRejected
	}

	resolvedParent, err := resolveExisting(parent)
	if err != nil {
		return "", err
	}

	return filepath.Join(resolvedParent, tail), nil
}
