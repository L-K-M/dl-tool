//go:build linux

package fsx

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
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
	// away as the longest existing prefix.
	if err := os.Symlink("/nonexistent-target-dl-tool", filepath.Join(root, "dangling")); err != nil {
		t.Fatalf("dangling symlink: %v", err)
	}
	if _, err := SafeJoin(root, []string{"dangling", "file"}); !errors.Is(err, ErrPathRejected) {
		t.Errorf("SafeJoin through a dangling symlink = %v, want ErrPathRejected", err)
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
