package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/L-K-M/dl-tool/internal/uri"
)

// TestRoute walks the nine rows of the routing table (docs/06-download-engines.md
// section 2) plus the extra fixtures of T016. Every accepted row is driven
// through uri.Normalize first, so the test proves normalisation and routing
// agree. Rows Normalize rejects with ErrUnsupportedScheme are fed to Route
// directly: the bare-infohash rows must still route to qbittorrent (row 2 of
// the table), while every other rejected shape must get ErrNoEngine back.
func TestRoute(t *testing.T) {
	// One matcher shared by the two row-3 fixtures: it claims exactly one HTTPS
	// host, proving a mediaMatch hit wins over the plain HTTPS row and that
	// every other HTTPS URL still routes to aria2.
	mediaMatcher := func(u string) bool {
		return strings.HasPrefix(u, "https://media.example.org/")
	}

	tests := []struct {
		name string
		// raw is driven through uri.Normalize when normalizeRejects is false.
		raw string
		// normalizeRejects marks rows uri.Normalize refuses with
		// ErrUnsupportedScheme; the router must refuse them too.
		normalizeRejects bool
		want             string // wanted engine name; "" together with wantNoEngine
		wantNoEngine     bool
		mediaMatch       func(string) bool
	}{
		// Row 1: thunder:// decodes to an HTTPS link, re-routed from row 2 → row 4.
		{name: "row 1 thunder decoding to https", raw: "thunder://QUFodHRwczovL2V4YW1wbGUub3JnL2YuaXNvWlo=", want: NameAria2},
		// Row 1: flashget:// decodes to an FTP link, re-routed from row 2 → row 5.
		{name: "row 1 flashget decoding to ftp", raw: "flashget://W0ZMQVNIR0VUXWZ0cDovL2V4YW1wbGUub3JnL2YuaXNvW0ZMQVNIR0VUXQ==", want: NameAria2},

		// Row 2: BitTorrent identity → qbittorrent.
		{name: "row 2 magnet", raw: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", want: NameQBittorrent},
		{name: "row 2 torrent url", raw: "https://example.org/x.torrent", want: NameQBittorrent},
		{name: "row 2 bare 40-hex infohash", raw: "0123456789abcdef0123456789abcdef01234567", normalizeRejects: true, wantNoEngine: false, want: NameQBittorrent},
		{name: "row 2 bare 64-hex infohash", raw: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", normalizeRejects: true, want: NameQBittorrent},

		// Row 3: a mediaMatch hit must win over the HTTPS row; under the same
		// matcher every other HTTPS URL still routes to aria2.
		{name: "row 3 media match wins over https", raw: "https://media.example.org/watch?v=x", mediaMatch: mediaMatcher, want: NameYtDlp},
		{name: "row 3 non-media https still aria2", raw: "https://example.org/f.iso", mediaMatch: mediaMatcher, want: NameAria2},

		// Row 4: http/https → aria2.
		{name: "row 4 https", raw: "https://example.org/f.iso", want: NameAria2},
		{name: "row 4 http", raw: "http://example.org/f.iso", want: NameAria2},

		// Row 5: ftp/ftps/sftp → aria2.
		{name: "row 5 ftp", raw: "ftp://example.org/f.iso", want: NameAria2},
		{name: "row 5 ftps", raw: "ftps://example.org/f.iso", want: NameAria2},
		{name: "row 5 sftp", raw: "sftp://example.org/f.iso", want: NameAria2},

		// Row 6: metalink suffixes → aria2.
		{name: "row 6 metalink", raw: "https://example.org/f.metalink", want: NameAria2},
		{name: "row 6 meta4", raw: "https://example.org/f.meta4", want: NameAria2},

		// Row 7: ed2k is already rejected by uri.Normalize.
		{name: "row 7 ed2k rejected by normalisation", raw: "ed2k://|file|movie.avi|733183104|31D6CFE0D16AE931B73CC5977D0AF08C|/", normalizeRejects: true, wantNoEngine: true},
		{name: "row 7 ed2k behind thunder rejected too", raw: "thunder://QUFlZDJrOi8vfGZpbGV8bW92aWUuYXZpfDczMzE4MzEwNHwzMUQ2Q0ZFMEQxNkFFOTMxQjczQzU5RDdFMEMwODlDMHwvWlo=", normalizeRejects: true, wantNoEngine: true},

		// Row 8: nzb has no v1 engine.
		{name: "row 8 nzb scheme", raw: "nzb://example.org/f.nzb", normalizeRejects: true, wantNoEngine: true},

		// Row 9: anything else.
		{name: "row 9 mailto", raw: "mailto:someone@example.org", normalizeRejects: true, wantNoEngine: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := uri.Normalize(tt.raw)
			if tt.normalizeRejects {
				if !errors.Is(err, uri.ErrUnsupportedScheme) {
					t.Fatalf("Normalize(%q) error = %v, want errors.Is uri.ErrUnsupportedScheme", tt.raw, err)
				}
				// Normalisation refuses the input; feed the raw shape to
				// Route directly to confirm the router refuses it too.
				n = uri.Normalized{URI: tt.raw}
			} else if err != nil {
				t.Fatalf("Normalize(%q) error = %v", tt.raw, err)
			}

			got, err := Route(n, tt.mediaMatch)
			if tt.wantNoEngine {
				if !errors.Is(err, ErrNoEngine) {
					t.Fatalf("Route(%q) error = %v, want errors.Is ErrNoEngine", tt.raw, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Route(%q) error = %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("Route(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
