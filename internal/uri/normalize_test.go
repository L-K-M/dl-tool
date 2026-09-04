package uri

import (
	"errors"
	"testing"
)

// btihV1Hex and btihV1Base32 are the same 20-byte hash in the two BEP 9 spellings.
const (
	btihV1Hex    = "0b1ec1a478f2b3c793bf88a45e9c0d6d81f2a3b4"
	btihV1Base32 = "BMPMDJDY6KZ4PE57RCSF5HANNWA7FI5U"
	btmhV2Digest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" // sha256("test")
)

func TestDecodeObfuscated(t *testing.T) {
	decodes := []struct {
		name string
		raw  string
		want string
	}{
		{"thunder worked example", "thunder://QUFodHRwOi8vZXhhbXBsZS5vcmcvZmlsZS5pc29aWg==", "http://example.org/file.iso"},
		{"flashget worked example", "flashget://W0ZMQVNIR0VUXWh0dHA6Ly9leGFtcGxlLm9yZy9maWxlLmlzb1tGTEFTSEdFVF0=", "http://example.org/file.iso"},
		{"qqdl worked example", "qqdl://aHR0cDovL2V4YW1wbGUub3JnL2ZpbGUuaXNv", "http://example.org/file.iso"},
		{"independent thunder fixture", "thunder://QUFodHRwOi8vd3d3LmZyZWUtei5uZXQvMS5yYXJaWg==", "http://www.free-z.net/1.rar"},
		{"fr-003 fixture", "thunder://QUFodHRwOi8vd3d3LmV4YW1wbGUuY29tL2ZpbGUuemlwWlo=", "http://www.example.com/file.zip"},
		{"missing padding", "thunder://QUFodHRwOi8vZXhhbXBsZS5vcmcvZmlsZS5pc29aWg", "http://example.org/file.iso"},
		{"trailing slash", "thunder://QUFodHRwOi8vZXhhbXBsZS5vcmcvZmlsZS5pc29aWg==/", "http://example.org/file.iso"},
		{"url-safe alphabet", "thunder://QUFodHRwOi8vZXhhbXBsZS5vcmcvZmlsZXMvPj4-ZGF0YTw8PFpa", "http://example.org/files/>>>data<<<"},
		{"freeznet junk suffix", "thunder://QUFodHRwOi8vd3d3LmZyZWUtei5uZXQvMS5yYXJaWg==&freeznet", "http://www.free-z.net/1.rar"},
		{"ed2k behind thunder is a valid decode", "thunder://QUFlZDJrOi8vfGZpbGV8bW92aWUuYXZpfDczMzE4MzEwNHwzMUQ2Q0ZFMEQxNkFFOTMxQjczQzU5RDdFMEMwODlDMHwvWlo=", "ed2k://|file|movie.avi|733183104|31D6CFE0D16AE931B73C59D7E0C089C0|/"},
		{"uppercase scheme", "THUNDER://QUFodHRwOi8vZXhhbXBsZS5vcmcvZmlsZS5pc29aWg==", "http://example.org/file.iso"},
	}
	for _, tt := range decodes {
		t.Run(tt.name, func(t *testing.T) {
			plain, ok := DecodeObfuscated(tt.raw)
			if !ok {
				t.Fatalf("DecodeObfuscated(%q) ok = false, want %q", tt.raw, tt.want)
			}
			if plain != tt.want {
				t.Errorf("DecodeObfuscated(%q) = %q, want %q", tt.raw, plain, tt.want)
			}
		})
	}

	rejects := []struct {
		name string
		raw  string
	}{
		{"not an obfuscated scheme", "http://example.org/file.iso"},
		{"payload decodes to a non-URL", "qqdl://aGVsbG8gd29ybGQ="}, // "hello world"
		{"payload is not base64", "thunder://%%%"},
		{"empty payload", "thunder://"},
		{"no scheme separator", "thunder:QUFodHRw"},
	}
	for _, tt := range rejects {
		t.Run(tt.name, func(t *testing.T) {
			if plain, ok := DecodeObfuscated(tt.raw); ok {
				t.Errorf("DecodeObfuscated(%q) = %q, true; want ok = false", tt.raw, plain)
			}
		})
	}
}

func TestNormalizeClassifies(t *testing.T) {
	classifies := []struct {
		name       string
		raw        string
		wantKind   Kind
		wantURI    string
		wantScheme string
	}{
		{"http", "http://example.org/file.iso", KindHTTP, "http://example.org/file.iso", ""},
		{"https", "https://example.org/file.iso", KindHTTP, "https://example.org/file.iso", ""},
		{"ftp", "ftp://example.org/file.iso", KindFTP, "ftp://example.org/file.iso", ""},
		{"ftps", "ftps://example.org/file.iso", KindFTP, "ftps://example.org/file.iso", ""},
		{"sftp", "sftp://example.org/file.iso", KindSFTP, "sftp://example.org/file.iso", ""},
		{"http path ending .torrent", "https://example.org/file.torrent", KindTorrent, "https://example.org/file.torrent", ""},
		{"uppercase .TORRENT suffix", "https://example.org/File.TORRENT", KindTorrent, "https://example.org/File.TORRENT", ""},
		{"http path ending .metalink", "https://example.org/file.metalink", KindMetalink, "https://example.org/file.metalink", ""},
		{"http path ending .meta4", "https://example.org/file.meta4", KindMetalink, "https://example.org/file.meta4", ""},
		{"magnet", "magnet:?xt=urn:btih:" + btihV1Hex, KindMagnet, "magnet:?xt=urn:btih:" + btihV1Hex, ""},
		{"thunder wraps http", "thunder://QUFodHRwOi8vd3d3LmV4YW1wbGUuY29tL2ZpbGUuemlwWlo=", KindHTTP, "http://www.example.com/file.zip", "thunder"},
		{"flashget wraps ftp", "flashget://W0ZMQVNIR0VUXWZ0cDovL2V4YW1wbGUub3JnL2ZpbGUuaXNvW0ZMQVNIR0VUXQ==", KindFTP, "ftp://example.org/file.iso", "flashget"},
		{"userinfo never reaches the URI", "https://user:secret@example.org/file.iso", KindHTTP, "https://example.org/file.iso", ""},
		{"userinfo stripped after decode", "thunder://QUFodHRwczovL3VzZXI6c2VjcmV0QGV4YW1wbGUub3JnL2ZpbGUuaXNvWlo=", KindHTTP, "https://example.org/file.iso", "thunder"},
		{"surrounding whitespace", "  https://example.org/file.iso  ", KindHTTP, "https://example.org/file.iso", ""},
	}
	for _, tt := range classifies {
		t.Run(tt.name, func(t *testing.T) {
			n, err := Normalize(tt.raw)
			if err != nil {
				t.Fatalf("Normalize(%q) error = %v", tt.raw, err)
			}
			if n.Kind != tt.wantKind {
				t.Errorf("Normalize(%q).Kind = %q, want %q", tt.raw, n.Kind, tt.wantKind)
			}
			if n.URI != tt.wantURI {
				t.Errorf("Normalize(%q).URI = %q, want %q", tt.raw, n.URI, tt.wantURI)
			}
			if n.OriginalScheme != tt.wantScheme {
				t.Errorf("Normalize(%q).OriginalScheme = %q, want %q", tt.raw, n.OriginalScheme, tt.wantScheme)
			}
		})
	}

	rejects := []struct {
		name string
		raw  string
	}{
		{"ed2k fixture", "ed2k://|file|The_Two_Towers-The_Purist_Edit-Trailer.avi|14997504|965c013e991ee246d63d45ea71954c4d|/"},
		{"ed2k behind thunder", "thunder://QUFlZDJrOi8vfGZpbGV8bW92aWUuYXZpfDczMzE4MzEwNHwzMUQ2Q0ZFMEQxNkFFOTMxQjczQzU5RDdFMEMwODlDMHwvWlo="},
		{"nzb", "nzb://x"},
		{"unknown scheme", "gopher://example.org/file"},
		{"no scheme", "example.org/file.iso"},
		{"empty", ""},
	}
	for _, tt := range rejects {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Normalize(tt.raw)
			if !errors.Is(err, ErrUnsupportedScheme) {
				t.Errorf("Normalize(%q) error = %v, want errors.Is ErrUnsupportedScheme", tt.raw, err)
			}
		})
	}
}

func TestParseMagnet(t *testing.T) {
	t.Run("40-hex btih is lowercased", func(t *testing.T) {
		n, err := ParseMagnet("magnet:?xt=urn:btih:0B1EC1A478F2B3C793BF88A45E9C0D6D81F2A3B4")
		if err != nil {
			t.Fatalf("ParseMagnet error = %v", err)
		}
		if n.InfohashV1 != btihV1Hex {
			t.Errorf("InfohashV1 = %q, want %q", n.InfohashV1, btihV1Hex)
		}
	})

	t.Run("base32 btih equals its hexadecimal twin", func(t *testing.T) {
		n, err := ParseMagnet("magnet:?xt=urn:btih:" + btihV1Base32)
		if err != nil {
			t.Fatalf("ParseMagnet error = %v", err)
		}
		if n.InfohashV1 != btihV1Hex {
			t.Errorf("InfohashV1 = %q, want hex twin %q", n.InfohashV1, btihV1Hex)
		}
	})

	t.Run("btmh v2 fills InfohashV2", func(t *testing.T) {
		n, err := ParseMagnet("magnet:?xt=urn:btmh:1220" + btmhV2Digest)
		if err != nil {
			t.Fatalf("ParseMagnet error = %v", err)
		}
		if n.InfohashV2 != btmhV2Digest {
			t.Errorf("InfohashV2 = %q, want %q", n.InfohashV2, btmhV2Digest)
		}
		if n.InfohashV1 != "" {
			t.Errorf("InfohashV1 = %q, want empty for a v2-only magnet", n.InfohashV1)
		}
	})

	t.Run("hybrid magnet fills both infohashes", func(t *testing.T) {
		raw := "magnet:?xt=urn:btih:" + btihV1Hex + "&xt=urn:btmh:1220" + btmhV2Digest
		n, err := ParseMagnet(raw)
		if err != nil {
			t.Fatalf("ParseMagnet error = %v", err)
		}
		if len(n.InfohashV1) != 40 || n.InfohashV1 != btihV1Hex {
			t.Errorf("InfohashV1 = %q, want 40 lowercase hex %q", n.InfohashV1, btihV1Hex)
		}
		if len(n.InfohashV2) != 64 || n.InfohashV2 != btmhV2Digest {
			t.Errorf("InfohashV2 = %q, want 64 lowercase hex %q", n.InfohashV2, btmhV2Digest)
		}
	})

	t.Run("repeatable keys all survive", func(t *testing.T) {
		raw := "magnet:?xt=urn:btih:" + btihV1Hex +
			"&dn=Some+Release" +
			"&tr=udp%3A%2F%2Ftracker.one%3A1337%2Fannounce" +
			"&tr=https%3A%2F%2Ftracker.two%2Fannounce" +
			"&x.pe=1.2.3.4:6881" +
			"&x.pe=%5B2001:db8::1%5D:6881"
		n, err := ParseMagnet(raw)
		if err != nil {
			t.Fatalf("ParseMagnet error = %v", err)
		}
		if n.DisplayName != "Some Release" {
			t.Errorf("DisplayName = %q, want %q", n.DisplayName, "Some Release")
		}
		wantTrackers := []string{"udp://tracker.one:1337/announce", "https://tracker.two/announce"}
		if len(n.Trackers) != len(wantTrackers) {
			t.Fatalf("Trackers = %v, want %v", n.Trackers, wantTrackers)
		}
		for i, tr := range wantTrackers {
			if n.Trackers[i] != tr {
				t.Errorf("Trackers[%d] = %q, want %q", i, n.Trackers[i], tr)
			}
		}
		wantPeers := []string{"1.2.3.4:6881", "[2001:db8::1]:6881"}
		if len(n.PeerHints) != len(wantPeers) {
			t.Fatalf("PeerHints = %v, want %v", n.PeerHints, wantPeers)
		}
		for i, pe := range wantPeers {
			if n.PeerHints[i] != pe {
				t.Errorf("PeerHints[%d] = %q, want %q", i, n.PeerHints[i], pe)
			}
		}
		if n.URI != raw {
			t.Errorf("URI = %q, want the verbatim magnet %q", n.URI, raw)
		}
	})

	rejects := []struct {
		name string
		raw  string
	}{
		{"no xt", "magnet:?dn=orphan"},
		{"btih of neither length", "magnet:?xt=urn:btih:xyz"},
		{"btih hex with non-hex digits", "magnet:?xt=urn:btih:zz1ec1a478f2b3c793bf88a45e9c0d6d81f2a3b4"},
		{"btmh with a non-sha2-256 multihash tag", "magnet:?xt=urn:btmh:2200" + btmhV2Digest},
	}
	for _, tt := range rejects {
		t.Run(tt.name, func(t *testing.T) {
			if n, err := ParseMagnet(tt.raw); err == nil {
				t.Errorf("ParseMagnet(%q) = %+v, nil; want error", tt.raw, n)
			}
		})
	}

	t.Run("non-magnet scheme is ErrUnsupportedScheme", func(t *testing.T) {
		_, err := ParseMagnet("http://example.org/file.iso")
		if !errors.Is(err, ErrUnsupportedScheme) {
			t.Errorf("ParseMagnet error = %v, want errors.Is ErrUnsupportedScheme", err)
		}
	})
}

func TestParseED2K(t *testing.T) {
	t.Run("verbatim fixture", func(t *testing.T) {
		link, err := ParseED2K("ed2k://|file|The_Two_Towers-The_Purist_Edit-Trailer.avi|14997504|965c013e991ee246d63d45ea71954c4d|/")
		if err != nil {
			t.Fatalf("ParseED2K error = %v", err)
		}
		want := ED2K{
			Filename:  "The_Two_Towers-The_Purist_Edit-Trailer.avi",
			SizeBytes: 14997504,
			Hash:      "965c013e991ee246d63d45ea71954c4d",
		}
		if link != want {
			t.Errorf("ParseED2K = %+v, want %+v", link, want)
		}
	})

	t.Run("hash is lowercased", func(t *testing.T) {
		link, err := ParseED2K("ed2k://|file|movie.avi|733183104|31D6CFE0D16AE931B73C59D7E0C089C0|/")
		if err != nil {
			t.Fatalf("ParseED2K error = %v", err)
		}
		if link.Hash != "31d6cfe0d16ae931b73c59d7e0c089c0" {
			t.Errorf("Hash = %q, want lowercase", link.Hash)
		}
	})

	rejects := []struct {
		name string
		raw  string
	}{
		{"server link is not a file", "ed2k://|server|1.2.3.4|4661|/"},
		{"size is not a number", "ed2k://|file|x.avi|notanumber|965c013e991ee246d63d45ea71954c4d|/"},
		{"hash too short", "ed2k://|file|x.avi|12|abcd|/"},
		{"hash is not hex", "ed2k://|file|x.avi|12|zz5c013e991ee246d63d45ea71954c4d|/"},
		{"too few segments", "ed2k://|file|x.avi|12"},
	}
	for _, tt := range rejects {
		t.Run(tt.name, func(t *testing.T) {
			if link, err := ParseED2K(tt.raw); err == nil {
				t.Errorf("ParseED2K(%q) = %+v, nil; want error", tt.raw, link)
			}
		})
	}
}
