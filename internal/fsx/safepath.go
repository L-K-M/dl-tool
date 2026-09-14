// Package fsx confines every path dl-tool acts on to the configured data
// roots (DLTOOL_DATA_ROOTS). T020 lands the destination resolver; T046
// extends the package with per-segment sanitisation and the hostile-path
// table of docs/12-security-and-threat-model.md section 3.4.
package fsx

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// ErrPathRejected is returned when a path resolves outside every configured
// root. The API maps it to 403 /problems/path-rejected.
var ErrPathRejected = errors.New("fsx: path rejected")

const (
	// maxSegmentBytes caps one sanitised component at 240 bytes (doc 12
	// section 3.2 step 8); extensionWindow is the trailing span in which a
	// '.' marks the extension the cap re-appends.
	maxSegmentBytes = 240
	extensionWindow = 9
	maxPathBytes    = 4096 // doc 12 section 3.3 rule 3
	maxSegmentDepth = 32   // doc 12 section 3.3 rule 3
)

// SanitiseSegment applies the eleven ordered steps of
// docs/12-security-and-threat-model.md section 3.2 to one path component.
// Step 2 — the single percent-decode of an RFC 8187 filename* — belongs to
// the caller: this function receives an already-decoded segment and never
// decodes itself, so a double decode is impossible. The steps are
// deliberately stricter than libtorrent's Linux build; /data is routinely
// re-exported over SMB (doc 12 section 3.2 deviation notice).
func SanitiseSegment(s string) string {
	if s == "" {
		return "_"
	}

	s = norm.NFC.String(s)
	s = dropFormatAndControl(s)
	s = replaceIllegal(s)
	s = strings.ToValidUTF8(s, "_")
	s = truncateWithExtension(s)
	s = strings.TrimSpace(strings.TrimRight(s, ". "))
	if isReservedStem(s) {
		s = "_" + s
	}
	if s == "" || s == "." || s == ".." {
		return "_"
	}

	return s
}

// dropFormatAndControl deletes the bidi and format codepoints of step 4 —
// U+200B–U+200F, U+202A–U+202E, U+2066–U+2069, U+FEFF — and every control
// character of step 5 (< 0x20 and 0x7F, NUL included).
func dropFormatAndControl(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E,
			r >= 0x2066 && r <= 0x2069, r == 0xFEFF:
			return -1
		case r < 0x20 || r == 0x7F:
			return -1
		}

		return r
	}, s)
}

// replaceIllegal maps each of / \ : * ? " < > | onto _ (step 6).
func replaceIllegal(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}

		return r
	}, s)
}

// truncateWithExtension caps the segment at 240 bytes of UTF-8 without
// splitting a codepoint, then re-appends the extension the original
// carried — the '.' within its last 9 characters plus what follows
// (step 8). The cap binds the stem alone: the extension rides on top of
// the 240 bytes, so "A"×300+".mkv" yields 240 bytes of A plus .mkv.
func truncateWithExtension(s string) string {
	if len(s) <= maxSegmentBytes {
		return s
	}

	_, ext := splitExtension(s)

	return truncateRunes(s, maxSegmentBytes) + ext
}

// splitExtension returns the stem and the extension — a '.' within the
// last 9 characters plus everything after it.
func splitExtension(s string) (stem, ext string) {
	if len(s) > extensionWindow {
		if idx := strings.LastIndexByte(s[len(s)-extensionWindow:], '.'); idx >= 0 {
			idx += len(s) - extensionWindow

			return s[:idx], s[idx:]
		}
	}

	return s, ""
}

// truncateRunes cuts s to at most n bytes without splitting a codepoint.
func truncateRunes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}

// reservedStems are the Windows device names of doc 12 section 3.2 step 10.
var reservedStems = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true, "CLOCK$": true,
	"CONIN$": true, "CONOUT$": true,
	"COM0": true, "COM1": true, "COM2": true, "COM3": true, "COM4": true,
	"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT0": true, "LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
	"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// isReservedStem reports whether the stem — everything before the first
// '.' — upper-cased is a Windows device name (step 10). Windows reserves
// "com9" in "com9.tar.gz" too, so the stem is cut at the first dot, not at
// the extension window's.
func isReservedStem(s string) bool {
	stem, _, _ := strings.Cut(s, ".")

	return reservedStems[strings.ToUpper(stem)]
}

// SafeJoin joins segments under root with the rules of doc 12 section 3.3
// and returns the joined path. It returns ErrPathRejected for an absolute
// segment, a "..", a path over 4096 bytes, a depth over 32, or any
// component that is a symlink.
//
// Rule 1 is the caller's contract: root must be one of the resolved
// DLTOOL_DATA_ROOTS entries — never a path the API caller supplied
// verbatim. Rules 5 and 6 (the O_CREAT|O_EXCL|O_NOFOLLOW final open and
// the case-insensitive collision map) belong to the download writer that
// will call SafeJoin per path element; dedupeName is the rule-6 piece it
// will use.
func SafeJoin(root string, segments []string) (string, error) {
	parts := make([]string, 0, len(segments))
	for _, raw := range segments {
		switch {
		case raw == "" || raw == ".":
			continue
		case raw == ".." || filepath.IsAbs(raw):
			// Rule 2: never pop silently — a path that escapes is hostile,
			// not sloppy.
			return "", ErrPathRejected
		default:
			parts = append(parts, SanitiseSegment(raw))
		}
	}
	if len(parts) > maxSegmentDepth {
		return "", ErrPathRejected
	}

	joined := root
	for _, part := range parts {
		joined = filepath.Join(joined, part)
	}
	if len(joined) > maxPathBytes {
		return "", ErrPathRejected
	}

	if err := verifyBeneath(root, parts); err != nil {
		return "", err
	}

	return joined, nil
}

// verifyBeneath proves every component of the joined path exists beneath
// the open root directory without crossing a symlink — doc 12 section 3.3
// rule 4, at the syscall layer rather than by string comparison. The
// openat2 call carries RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS|
// RESOLVE_NO_MAGICLINKS, which settles containment atomically; kernels
// answering ENOSYS — or rejecting the flag set — get the portable walk,
// which checks each component anchored at the descriptor of its verified
// parent. The runtime is Linux, so the syscalls are issued through
// golang.org/x/sys/unix directly.
func verifyBeneath(root string, parts []string) (err error) {
	rootFD, err := unix.Openat(unix.AT_FDCWD, root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("fsx: open root %s: %w", root, err)
	}
	defer func() {
		// A failed close of a read-only descriptor leaks nothing the
		// walk could have corrupted; still reported, never dropped.
		if cerr := unix.Close(rootFD); cerr != nil {
			err = errors.Join(err, fmt.Errorf("fsx: close root %s: %w", root, cerr))
		}
	}()

	// parts carry no separator, "." or ".." after SafeJoin's pass, so the
	// slash join is a plain relative path.
	rel := strings.Join(parts, "/")
	if rel == "" {
		rel = "."
	}

	fd, oerr := unix.Openat2(rootFD, rel, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	switch {
	case oerr == nil:
		if cerr := unix.Close(fd); cerr != nil {
			return fmt.Errorf("fsx: close verified path: %w", cerr)
		}

		return nil
	case errors.Is(oerr, unix.ENOSYS) || errors.Is(oerr, unix.EINVAL):
		return verifyBeneathWalk(rootFD, parts)
	case errors.Is(oerr, unix.ELOOP) || errors.Is(oerr, unix.EXDEV):
		// ELOOP: a component is a symlink or magiclink; EXDEV: the path
		// resolved above the root.
		return ErrPathRejected
	default:
		return fmt.Errorf("fsx: openat2 beneath %s: %w", root, oerr)
	}
}

// verifyBeneathWalk is the ENOSYS fallback of doc 12 section 3.3 rule 4:
// it descends one component at a time, each fstatat anchored at the
// already-verified parent descriptor, and refuses any symlink. The
// O_NOFOLLOW on each descend closes the swap window between checking a
// component and opening it; the final resolved-path comparison against the
// resolved root is the doc's closing step.
func verifyBeneathWalk(rootFD int, parts []string) (err error) {
	dirfd := rootFD
	defer func() {
		if dirfd != rootFD {
			err = errors.Join(err, unix.Close(dirfd))
		}
	}()

	for i, part := range parts {
		var st unix.Stat_t
		if err := unix.Fstatat(dirfd, part, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("fsx: lstat component %s: %w", part, err)
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFLNK:
			return ErrPathRejected
		case unix.S_IFDIR:
			if i == len(parts)-1 {
				return nil
			}
			next, oerr := unix.Openat(dirfd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if oerr != nil {
				if errors.Is(oerr, unix.ELOOP) {
					// Swapped for a symlink between the fstatat and the
					// open; the no-follow flag caught it.
					return ErrPathRejected
				}

				return fmt.Errorf("fsx: openat component %s: %w", part, oerr)
			}
			if dirfd != rootFD {
				if cerr := unix.Close(dirfd); cerr != nil {
					err = errors.Join(
						fmt.Errorf("fsx: close walked descriptor: %w", cerr),
						unix.Close(next),
					)

					return err
				}
			}
			dirfd = next
		default:
			// The last component may name a file; an intermediate one may not.
			if i < len(parts)-1 {
				return fmt.Errorf("fsx: component %s: %w", part, unix.ENOTDIR)
			}
		}
	}

	return nil
}

// dedupeName is the collision rule of doc 12 section 3.3 rule 6: taken is
// the per-download map keyed on the case-folded name, and a repeat gets
// " (2)", " (3)" … inserted before the extension, so a torrent carrying
// both Movie.mkv and movie.mkv cannot use the second name as an overwrite
// primitive on a case-insensitive export.
func dedupeName(name string, taken map[string]int) string {
	key := nameFolder.String(name)
	n := taken[key]
	taken[key] = n + 1
	if n == 0 {
		return name
	}

	stem, ext := name, ""
	if idx := strings.LastIndexByte(name, '.'); idx > 0 {
		stem, ext = name[:idx], name[idx:]
	}

	return stem + " (" + strconv.Itoa(n+1) + ")" + ext
}

// nameFolder produces the case-folded key the collision map is indexed by
// — full Unicode case folding, so NFKC-distinct accents still collide only
// when a case-insensitive filesystem would fold them together.
var nameFolder = cases.Fold()

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
