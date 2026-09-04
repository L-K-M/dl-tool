package uri

import (
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

const (
	btihPrefix = "urn:btih:"
	btmhPrefix = "urn:btmh:"

	// btmhSHA256Tag is the multihash tag of a BitTorrent v2 infohash:
	// 0x12 = sha2-256, 0x20 = 32 bytes, followed by 64 hex digits.
	btmhSHA256Tag = "1220"

	infohashV1HexChars    = 40 // hex of a 20-byte SHA-1
	infohashV1Base32Chars = 32 // base32 of the same 20 bytes
	infohashV2HexChars    = 64 // hex of a 32-byte SHA-256
)

// ParseMagnet extracts the BEP 9 parameters of a magnet: URI. Every repeatable key is read
// with url.Values, never with a single-valued map.
func ParseMagnet(raw string) (Normalized, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		// url.Error quotes the whole magnet with %q, and tracker URLs inside
		// it can carry credentials — same redaction rule as classifyTransport.
		if ue, ok := err.(*url.Error); ok {
			if strings.Contains(raw, "@") {
				return Normalized{}, errors.New("parse magnet: invalid uri (credentials redacted)")
			}
			err = ue.Err
		}
		return Normalized{}, fmt.Errorf("parse magnet: %w", err)
	}
	if u.Scheme != "magnet" {
		return Normalized{}, fmt.Errorf("scheme %q: %w", u.Scheme, ErrUnsupportedScheme)
	}

	n := Normalized{Kind: KindMagnet, URI: strings.TrimSpace(raw)}
	q := u.Query()
	// Percent-decoded values are attacker-controlled text: raw CRLF/NUL must
	// never reach a log line or the UI (same guard as DecodeObfuscated).
	dn := q.Get("dn")
	if hasControlChar(dn) {
		return Normalized{}, errors.New("magnet: control character in dn")
	}
	n.DisplayName = dn
	// Drop empty values: a "tr=" or "x.pe=" pair must not become a junk row.
	for _, tr := range q["tr"] {
		if tr == "" {
			continue
		}
		if hasControlChar(tr) {
			return Normalized{}, errors.New("magnet: control character in tr")
		}
		n.Trackers = append(n.Trackers, tr)
	}
	for _, pe := range q["x.pe"] {
		if pe == "" {
			continue
		}
		if hasControlChar(pe) {
			return Normalized{}, errors.New("magnet: control character in x.pe")
		}
		n.PeerHints = append(n.PeerHints, pe)
	}
	// "ws" web seeds (BEP 19) repeat like "tr" but Normalized carries no field for them.

	for _, xt := range q["xt"] {
		lower := strings.ToLower(xt)
		switch {
		case strings.HasPrefix(lower, btihPrefix):
			v1, err := parseBTIH(xt[len(btihPrefix):])
			if err != nil {
				return Normalized{}, err
			}
			n.InfohashV1 = v1
		case strings.HasPrefix(lower, btmhPrefix):
			if v2, ok := parseBTMH(lower[len(btmhPrefix):]); ok {
				n.InfohashV2 = v2
			}
		}
		// Other exact topics (urn:sha1:, ...) carry no BitTorrent identity.
	}

	if n.InfohashV1 == "" && n.InfohashV2 == "" {
		return Normalized{}, errors.New("magnet: no usable xt exact topic")
	}
	return n, nil
}

// hasControlChar reports whether s holds a raw control character.
func hasControlChar(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0
}

// parseBTIH normalises a btih value to 40 lowercase hex characters, accepting the
// 40-character hex form and the 32-character base32 form (both BEP 9).
func parseBTIH(value string) (string, error) {
	switch len(value) {
	case infohashV1HexChars:
		if _, err := hex.DecodeString(value); err != nil {
			return "", fmt.Errorf("magnet: btih is not hex: %w", err)
		}
		return strings.ToLower(value), nil
	case infohashV1Base32Chars:
		raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(value))
		if err != nil {
			return "", fmt.Errorf("magnet: btih is not base32: %w", err)
		}
		return hex.EncodeToString(raw), nil
	default:
		return "", fmt.Errorf("magnet: btih has %d characters, want %d hex or %d base32",
			len(value), infohashV1HexChars, infohashV1Base32Chars)
	}
}

// parseBTMH extracts the 64 hex digits after the "1220" sha2-256 multihash tag. ok is
// false for any other multihash — dl-tool cannot key on it.
func parseBTMH(value string) (string, bool) {
	if len(value) != len(btmhSHA256Tag)+infohashV2HexChars || !strings.HasPrefix(value, btmhSHA256Tag) {
		return "", false
	}
	digest := value[len(btmhSHA256Tag):]
	if _, err := hex.DecodeString(digest); err != nil {
		return "", false
	}
	return digest, true
}
