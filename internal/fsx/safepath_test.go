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
		{23, "re\u0301sume\u0301.txt", "résumé.txt"},
		// Cyrillic а (U+0430) is a homoglyph, not a hazard: kept verbatim.
		{24, "аdmin.txt", "аdmin.txt"},
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
		// plausible name.
		decoded, err := url.QueryUnescape("..%2F..%2Fetc%2Fshadow")
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if decoded != "../../etc/shadow" {
			t.Fatalf("decoded = %q", decoded)
		}
		if _, err := SafeJoin(root, strings.Split(decoded, "/")); !errors.Is(err, ErrPathRejected) {
			t.Errorf("SafeJoin traversal = %v, want ErrPathRejected", err)
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
			t.Errorf("SanitiseSegment long name = %d bytes, want 240 of A + .mkv", len(got))
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
	})
}
