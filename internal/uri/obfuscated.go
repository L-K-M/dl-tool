package uri

import (
	"encoding/base64"
	"strings"
)

// Sentinel affixes each obfuscated scheme wraps around the plain URL.
const (
	thunderPrefix  = "AA"
	thunderSuffix  = "ZZ"
	flashgetMarker = "[FLASHGET]"
)

// plainSchemes are the only schemes a decoded payload may begin with, lowercase.
var plainSchemes = []string{"http://", "https://", "ftp://", "ftps://", "sftp://", "ed2k://"}

// DecodeObfuscated returns the plain URL behind a thunder://, flashget:// or qqdl:// link.
// ok is false when the scheme is not one of the three or the payload does not decode to a URL.
func DecodeObfuscated(raw string) (plain string, ok bool) {
	scheme, payload, found := strings.Cut(raw, "://")
	if !found {
		return "", false
	}
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if scheme != "thunder" && scheme != "flashget" && scheme != "qqdl" {
		return "", false
	}

	// Generators append site-specific junk after the Base64 (the reference Ruby
	// implementation strips a literal "&freeznet"); cut from the first "&".
	payload = strings.TrimSpace(payload)
	if i := strings.IndexByte(payload, '&'); i >= 0 {
		payload = payload[:i]
	}
	// Links are frequently pasted with a trailing "/".
	payload = strings.TrimSuffix(payload, "/")
	// Padding is frequently missing in the wild: re-pad to a multiple of four.
	if rem := len(payload) % 4; rem != 0 {
		payload += strings.Repeat("=", 4-rem)
	}

	// Decode leniently: standard alphabet first, URL-safe on failure. A partial
	// decode is kept — the scheme-prefix check below decides acceptance, so
	// trailing garbage can never smuggle a non-URL through.
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		alt, altErr := base64.URLEncoding.DecodeString(payload)
		if altErr == nil || len(alt) > len(decoded) {
			decoded = alt
		}
	}
	if len(decoded) == 0 {
		return "", false
	}

	// Strip the sentinel prefix and suffix only when present, then trim.
	plain = string(decoded)
	switch scheme {
	case "thunder":
		plain = strings.TrimSuffix(strings.TrimPrefix(plain, thunderPrefix), thunderSuffix)
	case "flashget":
		plain = strings.TrimSuffix(strings.TrimPrefix(plain, flashgetMarker), flashgetMarker)
	}
	plain = strings.TrimSpace(plain)

	lower := strings.ToLower(plain)
	for _, p := range plainSchemes {
		if strings.HasPrefix(lower, p) {
			return plain, true
		}
	}
	return "", false
}
