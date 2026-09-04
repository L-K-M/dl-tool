// Package uri turns a pasted download URI into a classified, canonical form.
package uri

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ed2kHashChars is the length of an ed2k hash: hex of a 16-byte MD4 root.
const ed2kHashChars = 32

// Kind mirrors the tasks.source_kind enum in docs/04-data-model.md.
type Kind string

const (
	// KindHTTP is an http:// or https:// download.
	KindHTTP Kind = "http"
	// KindFTP is an ftp:// or ftps:// download.
	KindFTP Kind = "ftp"
	// KindSFTP is an sftp:// download.
	KindSFTP Kind = "sftp"
	// KindMagnet is a magnet: URI.
	KindMagnet Kind = "magnet"
	// KindTorrent is a URI whose path ends in .torrent.
	KindTorrent Kind = "torrent"
	// KindMetalink is a URI whose path ends in .metalink or .meta4.
	KindMetalink Kind = "metalink"
	// KindMedia is a URI for the yt-dlp lane; the router (T016) assigns it, Normalize never does.
	KindMedia Kind = "media"
)

// ErrUnsupportedScheme is returned for ed2k:// and nzb sources.
var ErrUnsupportedScheme = errors.New("uri: unsupported scheme")

// Normalized is one classified, canonical submission.
type Normalized struct {
	Kind           Kind
	URI            string // the plain, canonical URI handed to the engine
	OriginalScheme string // "thunder" | "flashget" | "qqdl" | "" — provenance for the UI
	DisplayName    string // magnet dn, or ""
	InfohashV1     string // lowercase hex, 40 chars
	InfohashV2     string // lowercase hex, 64 chars
	Trackers       []string
	PeerHints      []string // magnet x.pe values, "host:port"
}

// Normalize decodes obfuscated schemes, lowercases infohashes and classifies the URI.
func Normalize(raw string) (Normalized, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Normalized{}, errors.New("uri: empty input")
	}

	scheme := schemeOf(raw)
	n := Normalized{URI: raw}
	if plain, ok := DecodeObfuscated(raw); ok {
		n.OriginalScheme = scheme
		n.URI = plain
		scheme = schemeOf(plain)
	}

	switch scheme {
	case "magnet":
		m, err := ParseMagnet(n.URI)
		if err != nil {
			return Normalized{}, err
		}
		m.OriginalScheme = n.OriginalScheme
		return m, nil
	case "ed2k":
		// Parsed for display by ParseED2K, never downloaded: any ed2k
		// submission, valid or malformed, is the same unsupported scheme.
		return Normalized{}, fmt.Errorf("%w: ed2k is not supported in v1", ErrUnsupportedScheme)
	case "http", "https":
		return classifyTransport(n, KindHTTP)
	case "ftp", "ftps":
		return classifyTransport(n, KindFTP)
	case "sftp":
		return classifyTransport(n, KindSFTP)
	default:
		return Normalized{}, fmt.Errorf("scheme %q: %w", scheme, ErrUnsupportedScheme)
	}
}

// ED2K is a parsed ed2k:// link, kept for display only. dl-tool never downloads one.
type ED2K struct {
	Filename  string
	SizeBytes int64
	Hash      string // 32 hex characters, an MD4 root hash
}

// ParseED2K parses ed2k://|file|<name>|<size>|<hash>|/ . Callers reject the submission with
// ErrUnsupportedScheme and the message "ed2k is not supported in v1".
func ParseED2K(raw string) (ED2K, error) {
	parts := strings.Split(strings.TrimSpace(raw), "|")
	if len(parts) < 5 || !strings.EqualFold(parts[0], "ed2k://") || parts[1] != "file" {
		return ED2K{}, errors.New("ed2k: not an ed2k://|file| link")
	}

	size, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return ED2K{}, fmt.Errorf("ed2k: size %q: %w", parts[3], err)
	}

	hash := strings.ToLower(parts[4])
	if len(hash) != ed2kHashChars {
		return ED2K{}, fmt.Errorf("ed2k: hash has %d characters, want %d", len(hash), ed2kHashChars)
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return ED2K{}, fmt.Errorf("ed2k: hash is not hex: %w", err)
	}

	return ED2K{Filename: parts[2], SizeBytes: size, Hash: hash}, nil
}

// classifyTransport strips userinfo — a password must never reach storage or a log — and
// refines the transport Kind by file suffix.
func classifyTransport(n Normalized, kind Kind) (Normalized, error) {
	u, err := url.Parse(n.URI)
	if err != nil {
		// url.Error renders the URL with %q and its cause can echo input
		// fragments (invalid URL escape "%re"), so when the paste could carry
		// credentials, fall back to a static message: a password must never
		// reach a log.
		if ue, ok := err.(*url.Error); ok {
			if strings.Contains(n.URI, "@") {
				return Normalized{}, errors.New("parse uri: invalid uri (credentials redacted)")
			}
			err = ue.Err
		}
		return Normalized{}, fmt.Errorf("parse uri: %w", err)
	}
	if u.User != nil {
		u.User = nil
		n.URI = u.String()
	}
	if u.Hostname() == "" {
		return Normalized{}, fmt.Errorf("uri: missing host in %s uri", kind)
	}

	n.Kind = kind
	path := strings.ToLower(u.Path)
	switch {
	case strings.HasSuffix(path, ".torrent"):
		n.Kind = KindTorrent
	case strings.HasSuffix(path, ".metalink"), strings.HasSuffix(path, ".meta4"):
		n.Kind = KindMetalink
	}
	return n, nil
}

// schemeOf returns the lowercased scheme of raw, or "" when raw has no colon. Both
// "scheme://" (http-style) and "scheme:" (magnet-style) are recognised.
func schemeOf(raw string) string {
	scheme, _, found := strings.Cut(raw, ":")
	if !found {
		return ""
	}
	return strings.ToLower(scheme)
}
