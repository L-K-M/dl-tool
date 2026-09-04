package engine

import (
	"errors"

	"github.com/L-K-M/dl-tool/internal/uri"
)

// ErrNoEngine is returned by Route when no row of the routing table matches. Callers map it to the
// tasks.error_code value "unsupported_scheme".
var ErrNoEngine = errors.New("engine: no engine accepts this uri")

// Names of the three v1 engines. These are the values of tasks.engine and of Engine.Name().
const (
	NameAria2       = "aria2"
	NameQBittorrent = "qbittorrent"
	NameYtDlp       = "ytdlp"
)

// Lengths in hex characters of the two infohash shapes: v1 is the hex of a
// 20-byte SHA-1, v2 of a 32-byte SHA-256.
const (
	infohashV1HexChars = 40
	infohashV2HexChars = 64
)

// Route returns the engine name for an already-normalised submission, evaluating the routing table
// of docs/06-download-engines.md §2 in order and stopping at the first match. mediaMatch reports
// whether a yt-dlp extractor claims the URL; pass nil to skip row 3.
func Route(n uri.Normalized, mediaMatch func(string) bool) (string, error) {
	// Row 1 (thunder/flashget/qqdl) has already run inside uri.Normalize: the
	// recovered inner URI is what n carries, and n.OriginalScheme keeps the
	// provenance for the UI.

	// Row 2: BitTorrent identity — magnet, .torrent URL or bare infohash.
	if n.Kind == uri.KindMagnet || n.Kind == uri.KindTorrent || isBareInfohash(n.URI) {
		return NameQBittorrent, nil
	}

	// Row 3: a yt-dlp extractor claims the URL; must run before the aria2 rows.
	if mediaMatch != nil && mediaMatch(n.URI) {
		return NameYtDlp, nil
	}

	// Rows 4-6: the aria2 lanes — http(s), ftp(s)/sftp and metalink.
	switch n.Kind {
	case uri.KindHTTP, uri.KindFTP, uri.KindSFTP, uri.KindMetalink:
		return NameAria2, nil
	}

	// Rows 7-9: ed2k never reaches Route (uri.Normalize rejects it first);
	// nzb://, mailto: and every other shape have no v1 engine either.
	return "", ErrNoEngine
}

// isBareInfohash reports whether s is exactly a 40- or 64-character hex string,
// upper or lower case — detected by length and alphabet only, never by
// contacting an engine.
func isBareInfohash(s string) bool {
	if len(s) != infohashV1HexChars && len(s) != infohashV2HexChars {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isHexDigit := ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
		if !isHexDigit {
			return false
		}
	}
	return true
}
