// Package fsx confines every path dl-tool acts on to the configured data
// roots (DLTOOL_DATA_ROOTS). T020 lands the destination resolver; T046
// extends the package with per-segment sanitisation and the hostile-path
// table of docs/12-security-and-threat-model.md section 3.4.
package fsx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
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
	case errors.Is(oerr, unix.ENOENT):
		// Rule 5 has the writer create the final component after the join
		// is accepted, so a missing tail is expected — but ENOENT cannot
		// be told apart from a dangling symlink at this layer, and the two
		// must not disagree with the ENOSYS path. The lstat walk
		// distinguishes them: it returns nil at the first genuinely
		// missing component and rejects a symlink wherever it sits.
		return verifyBeneathWalk(rootFD, parts)
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
			if errors.Is(err, unix.ENOENT) {
				// The tail is not built yet — rule 5 creates it after the
				// join is accepted; the prefix up to dirfd is verified.
				return nil
			}

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
			// dirfd moves to next before prev is closed: on a close error
			// the deferred cleanup still owns next, and prev — already
			// released by the failed close — is never closed twice.
			prev := dirfd
			dirfd = next
			if prev != rootFD {
				if cerr := unix.Close(prev); cerr != nil {
					return fmt.Errorf("fsx: close walked descriptor: %w", cerr)
				}
			}
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
	// A fresh caser per call: x/text documents a Caser as not safe to
	// share between goroutines, and the download writer dedupes files
	// concurrently.
	fold := cases.Fold()

	key := fold.String(name)
	n := taken[key]
	taken[key] = n + 1
	if n == 0 {
		return name
	}

	stem, ext := name, ""
	if idx := strings.LastIndexByte(name, '.'); idx > 0 {
		stem, ext = name[:idx], name[idx:]
	}

	// Every generated name is registered too: a torrent carrying a literal
	// "movie (2).mkv" after "movie.mkv" deduped onto it would otherwise
	// recreate the collision this rule exists to prevent.
	for i := n + 1; ; i++ {
		candidate := stem + " (" + strconv.Itoa(i) + ")" + ext
		if ckey := fold.String(candidate); taken[ckey] == 0 {
			taken[ckey] = 1

			return candidate
		}
	}
}

// ResolveDestination resolves requested against the configured roots,
// following symlinks, and returns the cleaned absolute path. roots is
// DLTOOL_DATA_ROOTS in order. An empty requested returns the first root.
//
// Containment is judged only after symlink resolution, so a destination that
// walks through a link pointing outside the roots is rejected even though
// its textual form stays inside. Resolution happens once, here: a component
// swapped for a symlink after this check (TOCTOU) is not detected, so
// callers that act on the resolved path must re-anchor on the owning
// resolved root — the pair ResolveDestinationRoot returns — and operate
// through a confined descriptor (MkdirBeneath, or the openat machinery of
// doc 12 section 3.3) rather than opening the resolved path itself. The
// request is never joined onto a root: the caller's path is resolved on its
// own and then checked, so no request input can build a path by
// concatenation.
func ResolveDestination(roots []string, requested string) (string, error) {
	_, resolved, err := ResolveDestinationRoot(roots, requested)

	return resolved, err
}

// ResolveDestinationRoot is ResolveDestination plus the resolved configured
// root that owns the answer. A descriptor opened on the root is a stable
// anchor: opening the returned resolved path directly would re-run
// resolution at open time, and a component swapped for a symlink after this
// call would redirect the operation out of the roots (doc 12 section 3.3's
// swap window). An empty request returns the first root cleaned but
// unresolved — ResolveDestination's contract — and the pair stays
// self-consistent for MkdirBeneath.
func ResolveDestinationRoot(roots []string, requested string) (root, resolved string, err error) {
	if len(roots) == 0 {
		return "", "", ErrPathRejected
	}
	if requested == "" {
		cleaned := filepath.Clean(roots[0])

		return cleaned, cleaned, nil
	}

	// Abs cleans as well, so any ".." in the request is folded away before
	// anything is compared or resolved.
	abs, err := filepath.Abs(requested)
	if err != nil {
		return "", "", ErrPathRejected
	}
	resolved, err = resolveExisting(abs)
	if err != nil {
		return "", "", ErrPathRejected
	}

	for _, configured := range roots {
		resolvedRoot, err := resolveExisting(filepath.Clean(configured))
		if err != nil {
			continue
		}
		if within(resolved, resolvedRoot) {
			return resolvedRoot, resolved, nil
		}
	}

	return "", "", ErrPathRejected
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

// MkdirBeneath creates leaf inside dir with dir confined beneath root —
// the write-side twin of verifyBeneath (doc 12 section 3.3). root and dir
// are ResolveDestinationRoot's pair: root is the resolved configured data
// root, dir the resolved path the request selected. The parent is opened
// through the root's descriptor, never through its own path, so a
// component of dir swapped for a symlink after resolution cannot redirect
// the create out of the root: the confined resolution answers ELOOP/EXDEV,
// mapped here to ErrPathRejected. leaf is one sanitised component — the
// caller's SafeJoin output — so the mkdirat itself has nothing left to
// resolve. Errors arrive as *os.PathError over the raw errno, so
// errors.Is(merr, fs.ErrExist) and friends work exactly as they do on
// os.Mkdir's answer.
func MkdirBeneath(root, dir, leaf string, perm fs.FileMode) (err error) {
	// Both paths come back from the resolver cleaned and evaluated, so the
	// relative form is pure descent — "." for the root itself, never a
	// leading "..". Anything else means the pair was not produced by
	// ResolveDestinationRoot and is refused, not trusted. The leaf gets
	// the same guard: mkdirat has no RESOLVE_BENEATH, so a separator or
	// ".." in it would escape dirFD unconditionally.
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return ErrPathRejected
	}
	if leaf == "" || leaf == "." || leaf == ".." || strings.ContainsRune(leaf, '/') || strings.ContainsRune(leaf, 0) {
		return ErrPathRejected
	}

	rootFD, err := unix.Openat(unix.AT_FDCWD, root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("fsx: open root %s: %w", root, err)
	}
	defer func() {
		if cerr := unix.Close(rootFD); cerr != nil {
			err = errors.Join(err, fmt.Errorf("fsx: close root %s: %w", root, cerr))
		}
	}()

	dirFD, err := openBeneathDir(rootFD, rel)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := unix.Close(dirFD); cerr != nil {
			err = errors.Join(err, fmt.Errorf("fsx: close mkdir parent: %w", cerr))
		}
	}()

	if err := unix.Mkdirat(dirFD, leaf, uint32(perm)); err != nil {
		return &os.PathError{Op: "mkdir", Path: leaf, Err: err}
	}

	return nil
}

// openBeneathDir opens the directory rel beneath rootFD and returns its
// descriptor, refusing symlinks and resolutions that leave the root — the
// same guarantee openat2 gives verifyBeneath, applied to a whole relative
// path at once. rel is "." or a clean relative path produced by
// filepath.Rel over resolved paths. ELOOP (a symlink or magiclink
// component) and EXDEV (a resolution above the root) map to
// ErrPathRejected; kernels without openat2 get the component walk, which
// enforces the same refusal through O_NOFOLLOW descends.
func openBeneathDir(rootFD int, rel string) (int, error) {
	// RESOLVE_BENEATH can answer EAGAIN when a component moved mid-walk
	// and the kernel cannot prove the resolution stayed confined; the
	// lookup is retried rather than surfaced — a transient race must not
	// turn into a 500 — but the refusal of ELOOP and EXDEV stays absolute.
	var fd int
	var oerr error
	for range 10 {
		fd, oerr = unix.Openat2(rootFD, rel, &unix.OpenHow{
			Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
			Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
		})
		if !errors.Is(oerr, unix.EAGAIN) {
			break
		}
	}
	switch {
	case oerr == nil:
		return fd, nil
	case errors.Is(oerr, unix.EAGAIN):
		// The kernel could not prove confinement across every retry — a
		// component kept moving under sustained churn. Refuse rather than
		// surfacing a 500; the refusal of ELOOP and EXDEV stays absolute.
		return -1, ErrPathRejected
	case errors.Is(oerr, unix.ELOOP) || errors.Is(oerr, unix.EXDEV):
		return -1, ErrPathRejected
	case errors.Is(oerr, unix.ENOSYS) || errors.Is(oerr, unix.EINVAL):
		return openBeneathDirWalk(rootFD, rel)
	default:
		return -1, &os.PathError{Op: "openat", Path: rel, Err: oerr}
	}
}

// openBeneathDirWalk is the ENOSYS fallback of openBeneathDir: it descends
// rel one component at a time, each open anchored at the verified parent
// descriptor with O_NOFOLLOW — the same swap window verifyBeneathWalk
// closes for the read side. The parent of a mkdir must exist the whole way,
// so unlike the listing's walk every component must be a directory: a
// missing one reports its raw errno for the caller's 404, a symlink one
// ErrPathRejected.
func openBeneathDirWalk(rootFD int, rel string) (dirfd int, err error) {
	if rel == "" {
		rel = "."
	}

	// cur tracks the deepest descriptor the walk owns. The named dirfd is
	// assigned only by the successful return: a `return -1, …` must not
	// clobber the descriptor the deferred cleanup still has to close.
	cur := rootFD
	owns := false
	defer func() {
		// On the failure path the deepest verified descriptor is released
		// here; on success the caller owns it. rootFD is never closed —
		// it belongs to the caller.
		if err != nil && owns {
			err = errors.Join(err, unix.Close(cur))
		}
	}()

	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		if part == ".." {
			// O_NOFOLLOW stops a symlink, not an ascent — a ".." in the
			// relative form escapes rootFD no matter what it resolves to.
			return -1, ErrPathRejected
		}
		// A symlink answers ENOTDIR, not ELOOP, to a no-follow
		// O_DIRECTORY open, so the type is settled by fstatat first —
		// the same pair verifyBeneathWalk uses. The O_NOFOLLOW on the
		// descend itself catches a swap landing between the two calls.
		var st unix.Stat_t
		if serr := unix.Fstatat(cur, part, &st, unix.AT_SYMLINK_NOFOLLOW); serr != nil {
			return -1, &os.PathError{Op: "openat", Path: part, Err: serr}
		}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return -1, ErrPathRejected
		}
		// A "." opens a fresh descriptor on the same directory, which
		// keeps the contract uniform: the walk always returns a
		// descriptor the caller owns, never rootFD itself — the caller
		// closes what it gets, and a shared descriptor would be closed
		// twice.
		next, oerr := unix.Openat(cur, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if oerr != nil {
			if errors.Is(oerr, unix.ELOOP) {
				// Swapped for a symlink between the fstatat and the
				// open; the no-follow flag caught it.
				return -1, ErrPathRejected
			}

			return -1, &os.PathError{Op: "openat", Path: part, Err: oerr}
		}
		// cur moves to next before prev is closed: on a close error the
		// deferred cleanup owns next, and prev — already released by the
		// failed close — is never closed twice (verifyBeneathWalk's order).
		prev := cur
		cur = next
		if owns {
			if cerr := unix.Close(prev); cerr != nil {
				return -1, fmt.Errorf("fsx: close walked descriptor: %w", cerr)
			}
		}
		owns = true
	}

	return cur, nil
}
