//go:build linux

package fsx

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// TestSanitiseSegmentTable carries every row of the hostile-path table of
// docs/12-security-and-threat-model.md section 3.4 verbatim; each subtest
// is named for its row number. Rows whose "expected output" is a safeJoin
// behaviour rather than a sanitised name drive SafeJoin, dedupeName or
// Browse instead — the row text says which.
func TestSanitiseSegmentTable(t *testing.T) {
	root := t.TempDir()

	// The single-segment rows: input on the left, the name §3.2 must
	// produce on the right.
	sanitiseRows := []struct {
		row  int
		in   string
		want string
	}{
		{1, "normal.mkv", "normal.mkv"},
		{2, "..", "_"},
		{4, "...", "_"},
		{5, "/etc/passwd", "_etc_passwd"},
		{7, `C:\Windows\system32.exe`, "C__Windows_system32.exe"},
		{8, `\\?\C:\x`, "____C__x"},
		{9, "CON", "_CON"},
		{10, "nul.txt", "_nul.txt"},
		{11, "com9.tar.gz", "_com9.tar.gz"},
		{12, "clock$", "_clock$"},
		{13, "file.txt.", "file.txt"},
		{14, "file ", "file"},
		{15, "evil\u202Egnp.exe", "evilgnp.exe"},
		{16, "a\u200Bb.mkv", "ab.mkv"},
		{18, "tab\there", "tabhere"},
		// Doc 12 §3.4 writes the input as `"a<b>c\|d?e*f:g"`; inside a
		// table code span `\|` is the GFM escape for a literal `|`, so
		// the segment under test carries a bare pipe.
		{19, `"a<b>c|d?e*f:g"`, "_a_b_c_d_e_f_g_"},
		{22, "", "_"},
		// NFD: e followed by U+0301 combining acute must compose to U+00E9.
		{23, "re\u0301sume\u0301.txt", "r\u00e9sum\u00e9.txt"},
		// Cyrillic а (U+0430) is a homoglyph, not a hazard: kept verbatim.
		{24, "\u0430dmin.txt", "\u0430dmin.txt"},
		{25, ".hidden", ".hidden"},
		{26, "-rf", "-rf"},
		{28, "/", "_"},
	}
	for _, tc := range sanitiseRows {
		t.Run(fmt.Sprintf("row %02d", tc.row), func(t *testing.T) {
			if got := SanitiseSegment(tc.in); got != tc.want {
				t.Errorf("SanitiseSegment(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	t.Run("row 03 dot segment dropped by safeJoin", func(t *testing.T) {
		got, err := SafeJoin(root, []string{"."})
		if err != nil {
			t.Fatalf("SafeJoin(%q, [.]) = error %v", root, err)
		}
		if got != root {
			t.Errorf("SafeJoin(%q, [.]) = %q, want the root itself", root, got)
		}
	})

	t.Run("row 06 filename-star double decode rejected", func(t *testing.T) {
		// The filename* value is percent-decoded exactly once; what decodes
		// to a traversal is rejected wholesale, never sanitised into a
		// plausible name. PathUnescape, not QueryUnescape: RFC 5987 has no
		// '+'-to-space rule.
		decoded, err := url.PathUnescape("..%2F..%2Fetc%2Fshadow")
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if decoded != "../../etc/shadow" {
			t.Fatalf("decoded = %q", decoded)
		}
		if _, err := SafeJoin(root, strings.Split(decoded, "/")); !errors.Is(err, ErrPathRejected) {
			t.Errorf("SafeJoin traversal = %v, want ErrPathRejected", err)
		}
		// The doubly-encoded form keeps its literal %2F after the single
		// decode — only a second pass would manufacture the traversal.
		once, err := url.PathUnescape("..%252F..%252Fetc%252Fshadow")
		if err != nil {
			t.Fatalf("decode double-encoded: %v", err)
		}
		if want := "..%2F..%2Fetc%2Fshadow"; once != want {
			t.Fatalf("one decode of double-encoded payload = %q, want %q", once, want)
		}
	})

	t.Run("row 17 NUL deleted in segment and rejected in a typed path", func(t *testing.T) {
		if got := SanitiseSegment("bad\x00name"); got != "badname" {
			t.Errorf("SanitiseSegment NUL = %q, want %q", got, "badname")
		}
		if _, err := Browse([]string{root}, root+"/bad\x00dir", false); !errors.Is(err, ErrPathRejected) {
			t.Errorf("Browse NUL path = %v, want ErrPathRejected", err)
		}
	})

	t.Run("row 20 length cap keeps 240 bytes then the extension", func(t *testing.T) {
		got := SanitiseSegment(strings.Repeat("A", 300) + ".mkv")
		want := strings.Repeat("A", maxSegmentBytes) + ".mkv"
		if got != want {
			t.Errorf("SanitiseSegment long name = %q, want %d bytes of A + .mkv", got, maxSegmentBytes)
		}
	})

	t.Run("row 21 cap counts bytes not runes", func(t *testing.T) {
		got := SanitiseSegment(strings.Repeat("日", 200) + ".mkv")
		if !utf8.ValidString(got) {
			t.Fatalf("result is not valid UTF-8: %q", got)
		}
		stem, ext := splitExtension(got)
		if ext != ".mkv" {
			t.Errorf("extension = %q, want .mkv", ext)
		}
		if len(stem) > maxSegmentBytes {
			t.Errorf("stem = %d bytes, want <= %d", len(stem), maxSegmentBytes)
		}
	})

	t.Run("row 27 torrent path of dot-dot rejects", func(t *testing.T) {
		if _, err := SafeJoin(root, []string{"..", "..", "x"}); !errors.Is(err, ErrPathRejected) {
			t.Errorf("SafeJoin dot-dot segments = %v, want ErrPathRejected", err)
		}
	})

	t.Run("row 29 symlink component rejected never created", func(t *testing.T) {
		if err := os.Symlink("/etc", filepath.Join(root, "link")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if _, err := SafeJoin(root, []string{"link"}); !errors.Is(err, ErrPathRejected) {
			t.Errorf("SafeJoin symlink component = %v, want ErrPathRejected", err)
		}
	})

	t.Run("row 30 case-insensitive collision deduped", func(t *testing.T) {
		taken := map[string]int{}
		if got := dedupeName("Movie.mkv", taken); got != "Movie.mkv" {
			t.Errorf("first name = %q, want Movie.mkv", got)
		}
		if got := dedupeName("movie.mkv", taken); got != "movie (2).mkv" {
			t.Errorf("folded repeat = %q, want %q", got, "movie (2).mkv")
		}
		if got := dedupeName("MOVIE.MKV", taken); got != "MOVIE (3).MKV" {
			t.Errorf("third folded repeat = %q, want %q", got, "MOVIE (3).MKV")
		}
	})

	t.Run("generated candidate cannot collide with a later genuine name", func(t *testing.T) {
		taken := map[string]int{}
		got := []string{
			dedupeName("Movie.mkv", taken),
			dedupeName("movie.mkv", taken),
			dedupeName("movie (2).mkv", taken),
		}
		seen := map[string]bool{}
		for _, name := range got {
			folded := strings.ToLower(name)
			if seen[folded] {
				t.Fatalf("dedupeName produced a folded collision in %v", got)
			}
			seen[folded] = true
		}
	})
}

// TestSafeJoinAllowsUnbuiltTail pins the download-writer contract: SafeJoin
// runs before rule 5 creates the final component, so a missing tail is not
// an error — only the existing prefix is verified.
func TestSafeJoinAllowsUnbuiltTail(t *testing.T) {
	root := t.TempDir()

	got, err := SafeJoin(root, []string{"newdir", "newfile.mkv"})
	if err != nil {
		t.Fatalf("SafeJoin unbuilt tail = error %v, want the joined path", err)
	}
	if want := filepath.Join(root, "newdir", "newfile.mkv"); got != want {
		t.Errorf("SafeJoin unbuilt tail = %q, want %q", got, want)
	}

	if err := os.Mkdir(filepath.Join(root, "existing"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink("/etc", filepath.Join(root, "existing", "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := SafeJoin(root, []string{"existing", "link", "new"}); !errors.Is(err, ErrPathRejected) {
		t.Errorf("SafeJoin through a prefix symlink = %v, want ErrPathRejected", err)
	}

	// A dangling symlink reports ENOENT through openat2, same as a
	// genuinely missing tail; it must still be rejected, not verified
	// away as the longest existing prefix. Nesting one below an existing
	// directory pins the case prefix-trimming would have accepted.
	if err := os.Symlink("/nonexistent-target-dl-tool", filepath.Join(root, "existing", "dangling")); err != nil {
		t.Fatalf("nested dangling symlink: %v", err)
	}
	if _, err := SafeJoin(root, []string{"existing", "dangling", "file"}); !errors.Is(err, ErrPathRejected) {
		t.Errorf("SafeJoin through a dangling symlink below an existing dir = %v, want ErrPathRejected", err)
	}
	if err := os.Symlink("/nonexistent-target-dl-tool", filepath.Join(root, "dangling")); err != nil {
		t.Fatalf("top-level dangling symlink: %v", err)
	}
	if _, err := SafeJoin(root, []string{"dangling", "file"}); !errors.Is(err, ErrPathRejected) {
		t.Errorf("SafeJoin through a top-level dangling symlink = %v, want ErrPathRejected", err)
	}
}

// TestBrowseFollowsInRootSymlink proves an absolute symlink whose target
// stays inside the same root is browsed at its resolved location, not
// refused or reinterpreted by the anchored open.
func TestBrowseFollowsInRootSymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(real, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	listing, err := Browse([]string{root}, filepath.Join(root, "link"), false)
	if err != nil {
		t.Fatalf("Browse through in-root symlink = error %v", err)
	}
	if listing.Path != real {
		t.Errorf("listing path = %q, want resolved %q", listing.Path, real)
	}
	if len(listing.Directories) != 1 || listing.Directories[0].Name != "sub" {
		t.Errorf("directories = %+v, want the real directory's sub", listing.Directories)
	}
}

// TestBrowseHidesCrossRootSymlink pins listing to what browsing can reach:
// a symlink in one root whose target lives in another root is not listed,
// because browsing it resolves outside the root the path lexically sits in.
func TestBrowseHidesCrossRootSymlink(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootB, "b-dir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(filepath.Join(rootB, "b-dir"), filepath.Join(rootA, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	listing, err := Browse([]string{rootA, rootB}, rootA, false)
	if err != nil {
		t.Fatalf("Browse root A = error %v", err)
	}
	for _, dir := range listing.Directories {
		if dir.Name == "link" {
			t.Error("cross-root symlink listed although browsing it is rejected")
		}
	}
}

// TestMkdirBeneathCreatesInsideRoot covers the ordinary create: directly
// at the root and below a nested resolved parent.
func TestMkdirBeneathCreatesInsideRoot(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := MkdirBeneath(root, root, "top", 0o777); err != nil {
		t.Fatalf("MkdirBeneath at root = error %v", err)
	}
	if err := MkdirBeneath(root, nested, "leaf", 0o777); err != nil {
		t.Fatalf("MkdirBeneath nested = error %v", err)
	}
	for _, dir := range []string{filepath.Join(root, "top"), filepath.Join(nested, "leaf")} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Errorf("%s missing or not a directory: %v", dir, err)
		}
	}

	if err := MkdirBeneath(root, root, "top", 0o777); !errors.Is(err, fs.ErrExist) {
		t.Errorf("repeat MkdirBeneath = %v, want fs.ErrExist", err)
	}
}

// TestMkdirBeneathRefusesSwappedParent is the deterministic parent-swap
// regression: the parent is resolved inside the root, then replaced by a
// symlink pointing outside before the anchored create runs — the window a
// ResolveDestination-time check cannot see. The anchored open must refuse
// with ErrPathRejected and nothing may appear at the symlink's target.
// Opening the resolved path itself — the shape this fix replaces — would
// have followed the link and created the directory outside the root.
func TestMkdirBeneathRefusesSwappedParent(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "data")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside, filepath.Join(root, "victim")} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	resolvedRoot, resolved, err := ResolveDestinationRoot([]string{root}, filepath.Join(root, "victim"))
	if err != nil {
		t.Fatalf("ResolveDestinationRoot = error %v", err)
	}

	// The swap lands after resolution, exactly where an attacker with
	// write access to the data root would place it.
	if err := os.Remove(resolved); err != nil {
		t.Fatalf("remove parent: %v", err)
	}
	if err := os.Symlink(outside, resolved); err != nil {
		t.Fatalf("swap parent for symlink: %v", err)
	}

	if err := MkdirBeneath(resolvedRoot, resolved, "payload", 0o777); !errors.Is(err, ErrPathRejected) {
		t.Fatalf("MkdirBeneath swapped parent = %v, want ErrPathRejected", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "payload")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the create escaped the root: outside payload stat err = %v", err)
	}
}

// TestMkdirBeneathRefusesSwappedComponent proves the same refusal when an
// intermediate component — not the leaf parent itself — is the one
// swapped for an outside-pointing symlink.
func TestMkdirBeneathRefusesSwappedComponent(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "data")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside, filepath.Join(root, "victim", "deep")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	resolvedRoot, resolved, err := ResolveDestinationRoot([]string{root}, filepath.Join(root, "victim", "deep"))
	if err != nil {
		t.Fatalf("ResolveDestinationRoot = error %v", err)
	}

	if err := os.RemoveAll(filepath.Join(root, "victim")); err != nil {
		t.Fatalf("remove intermediate component: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "victim")); err != nil {
		t.Fatalf("swap intermediate for symlink: %v", err)
	}

	if err := MkdirBeneath(resolvedRoot, resolved, "payload", 0o777); !errors.Is(err, ErrPathRejected) {
		t.Fatalf("MkdirBeneath swapped component = %v, want ErrPathRejected", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "payload")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the create escaped the root: outside payload stat err = %v", err)
	}
}

// TestMkdirBeneathMissingParent reports the parent that vanished outright
// — not swapped, simply gone — with fs.ErrNotExist so the endpoint can
// answer its 404. A parent that exists as a plain file answers ENOTDIR,
// which the endpoint maps the same way.
func TestMkdirBeneathMissingParent(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if err := MkdirBeneath(root, filepath.Join(root, "gone"), "leaf", 0o777); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("MkdirBeneath missing parent = %v, want fs.ErrNotExist", err)
	}
	if err := MkdirBeneath(root, filepath.Join(root, "file.txt"), "leaf", 0o777); !errors.Is(err, unix.ENOTDIR) {
		t.Errorf("MkdirBeneath file parent = %v, want ENOTDIR", err)
	}

	// A dir outside the root and a leaf carrying a NUL are both refused
	// outright: the relative form escapes the anchor, and a NUL would
	// only reach mkdirat as EINVAL — hostile input is ErrPathRejected,
	// not a 500.
	outside := t.TempDir()
	if err := MkdirBeneath(root, outside, "leaf", 0o777); !errors.Is(err, ErrPathRejected) {
		t.Errorf("MkdirBeneath outside dir = %v, want ErrPathRejected", err)
	}
	if err := MkdirBeneath(root, root, "a\x00b", 0o777); !errors.Is(err, ErrPathRejected) {
		t.Errorf("MkdirBeneath NUL leaf = %v, want ErrPathRejected", err)
	}
}

// TestOpenBeneathDirWalk exercises the ENOSYS fallback directly — the
// openat2 path hides it on kernels that answer openat2. The contract is
// the same one openBeneathDir promises: an owned descriptor on the
// directory, ErrPathRejected on a symlink, raw ENOENT on a gap.
func TestOpenBeneathDirWalk(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "data")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	rootFD, err := unix.Openat(unix.AT_FDCWD, root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer func() {
		if err := unix.Close(rootFD); err != nil {
			t.Errorf("close root descriptor: %v", err)
		}
	}()

	// "." must still hand back a descriptor the caller owns — returning
	// rootFD itself would double-close under the caller's cleanup.
	fd, err := openBeneathDirWalk(rootFD, ".")
	if err != nil {
		t.Fatalf("walk \".\" = error %v", err)
	}
	if fd == rootFD {
		t.Fatal("walk \".\" returned the caller's descriptor, want an owned one")
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR {
		t.Errorf("walk \".\" stat: mode %o, err %v", st.Mode, err)
	}
	if err := unix.Close(fd); err != nil {
		t.Errorf("close walked descriptor: %v", err)
	}

	fd, err = openBeneathDirWalk(rootFD, "a/b")
	if err != nil {
		t.Fatalf("walk nested = error %v", err)
	}
	if err := unix.Close(fd); err != nil {
		t.Errorf("close walked descriptor: %v", err)
	}

	if _, err := openBeneathDirWalk(rootFD, "link"); !errors.Is(err, ErrPathRejected) {
		t.Errorf("walk symlink = %v, want ErrPathRejected", err)
	}
	if _, err := openBeneathDirWalk(rootFD, "link/deep"); !errors.Is(err, ErrPathRejected) {
		t.Errorf("walk through symlink = %v, want ErrPathRejected", err)
	}
	if _, err := openBeneathDirWalk(rootFD, "gone"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("walk missing = %v, want fs.ErrNotExist", err)
	}
	if _, err := openBeneathDirWalk(rootFD, "gone/deeper"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("walk missing interior = %v, want fs.ErrNotExist", err)
	}
	if _, err := openBeneathDirWalk(rootFD, "a/../../b"); !errors.Is(err, ErrPathRejected) {
		t.Errorf("walk dot-dot = %v, want ErrPathRejected", err)
	}
	fd, err = openBeneathDirWalk(rootFD, "")
	if err != nil {
		t.Fatalf("walk empty rel = %v, want an owned descriptor on the root", err)
	}
	if fd == rootFD {
		t.Error("walk empty rel returned the caller's descriptor, want an owned one")
	}
	if err := unix.Close(fd); err != nil {
		t.Errorf("close walked descriptor: %v", err)
	}
}

// TestOpenBeneathDirWalkReleasesTheWalkOnFailure pins the descriptor
// contract of the error path: a refusal that lands mid-descent must not
// leak the deepest verified descriptor — the walk cursor and the named
// return share a variable only on success.
func TestOpenBeneathDirWalkReleasesTheWalkOnFailure(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "data")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// The swapped component sits beneath a real directory, so the refusal
	// lands after the walk already owns a descended descriptor.
	if err := os.Symlink(outside, filepath.Join(root, "a", "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	rootFD, err := unix.Openat(unix.AT_FDCWD, root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer func() {
		if err := unix.Close(rootFD); err != nil {
			t.Errorf("close root descriptor: %v", err)
		}
	}()

	before := countOpenFDs(t)
	for range 8 {
		if _, err := openBeneathDirWalk(rootFD, "a/link/deep"); !errors.Is(err, ErrPathRejected) {
			t.Fatalf("walk through a swapped component = %v, want ErrPathRejected", err)
		}
		if _, err := openBeneathDirWalk(rootFD, "a/gone/deep"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("walk through a missing interior = %v, want fs.ErrNotExist", err)
		}
	}
	if after := countOpenFDs(t); after != before {
		t.Errorf("fd count grew from %d to %d across failing walks — the error path leaks its deepest descriptor", before, after)
	}
}

// countOpenFDs reads the process descriptor table. The package is
// Linux-only — openat2 — so /proc is always there.
func countOpenFDs(t *testing.T) int {
	t.Helper()

	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read fd table: %v", err)
	}
	return len(entries)
}
