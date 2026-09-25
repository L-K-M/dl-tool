package ytdlp

import (
	"context"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestExtractorTableCompiles is the permanent re-verification of the
// generated output: every non-comment line of extractor_patterns.txt must
// compile under Go's regexp, so a stale regen or a hand edit fails here
// instead of silently rotting.
func TestExtractorTableCompiles(t *testing.T) {
	for i, line := range strings.Split(patternTable, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if _, err := regexp.Compile(line); err != nil {
			t.Errorf("extractor_patterns.txt line %d: %v", i+1, err)
		}
	}
}

// TestResidualOverridesCoverTable asserts every extractor the generator
// could not transpile has a ResidualOverrides entry — the drift check that
// makes a regenerating pin bump fail loudly when it strands a name. A
// surplus override (a name a regen moved back into the table) is logged,
// not failed — ADR-0022 records that as a note, not an error.
func TestResidualOverridesCoverTable(t *testing.T) {
	data, err := os.ReadFile("extractors_residual.txt")
	if err != nil {
		t.Fatalf("read extractors_residual.txt: %v", err)
	}
	residual := make(map[string]bool)
	for i, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		residual[line] = true
		if len(ResidualOverrides[line]) == 0 {
			t.Errorf("extractors_residual.txt line %d: %q has no ResidualOverrides entry", i+1, line)
		}
	}
	for name := range ResidualOverrides {
		if !residual[name] {
			t.Logf("ResidualOverrides surplus: %q is not in extractors_residual.txt", name)
		}
	}
}

func loadCache(t *testing.T) *ExtractorCache {
	t.Helper()
	cache, err := LoadExtractors()
	if err != nil {
		t.Fatalf("LoadExtractors: %v", err)
	}
	if !cache.Loaded() {
		t.Fatal("LoadExtractors returned an unloaded cache for a non-empty table")
	}
	return cache
}

// TestMatchOverrideHosts routes through the hand-maintained suffixes: all
// three of these extractors sit in the residual set.
func TestMatchOverrideHosts(t *testing.T) {
	cache := loadCache(t)
	for _, uri := range []string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ",
		"https://vimeo.com/123456789",
	} {
		if !cache.Match(uri) {
			t.Errorf("Match(%q) = false, want true via ResidualOverrides", uri)
		}
	}
}

// TestMatchGeneratedTable proves the transpiled table answers: Twitter's
// pattern survives transpile and compilation.
func TestMatchGeneratedTable(t *testing.T) {
	cache := loadCache(t)
	if !cache.Match("https://x.com/nasa/status/1234567890") {
		t.Error("Match(x.com status URL) = false, want true via the generated table")
	}
}

// TestMatchPathScopedOverrides proves the host/fragment grammar: a residual
// extractor that claims only specific paths on a shared host routes inside
// the claim, while a document, portal, profile or archived non-media page
// on the same host stays on the plain-download lane.
func TestMatchPathScopedOverrides(t *testing.T) {
	cache := loadCache(t)
	for _, tc := range []struct {
		uri  string
		want bool
	}{
		{"https://tenant.sharepoint.com/:v:/r/sites/team/video", true},
		{"https://tenant.sharepoint.com/stream.aspx?id=abc", true},
		{"https://tenant.sharepoint.com/sites/team/report.docx", false},
		{"https://web.archive.org/web/20200101000000/https://www.youtube.com/watch?v=dQw4w9WgXcQ", true},
		{"https://web.archive.org/web/20200101000000/https://example.com/page.html", false},
		{"https://www.imdb.com/list/ls123456789/", true},
		{"https://www.imdb.com/title/tt0111161/", false},
		{"https://vk.com/video/playlist/-123_456", true},
		{"https://vk.com/durov", false},
		{"https://open.spotify.com/track/abc", true},
		{"https://spotify.com/us/account/overview/", false},
		{"https://www.amazon.com/gp/video/detail/B0ABC", true},
		{"https://www.amazon.com/s?k=widget", false},
		{"https://music.amazon.de/albums/B0ABC", true},
		{"https://www.lequipe.fr/video/x", true},
		{"https://www.lequipe.fr/Football/Article/x", false},
	} {
		if got := cache.Match(tc.uri); got != tc.want {
			t.Errorf("Match(%q) = %v, want %v", tc.uri, got, tc.want)
		}
	}
}

// TestMatchCaseNormalizedAuthority lowercases scheme and authority before
// the table match, mirroring how yt-dlp normalizes netloc before _VALID_URL.
// The override lane already lowercases the hostname on its own.
func TestMatchCaseNormalizedAuthority(t *testing.T) {
	cache := loadCache(t)
	for _, uri := range []string{
		"HTTPS://X.COM/nasa/status/1234567890",
		"https://WWW.X.COM/nasa/status/1234567890",
	} {
		if !cache.Match(uri) {
			t.Errorf("Match(%q) = false, want true via the generated table", uri)
		}
	}
}

// TestMatchRejectsPlainDownload is also the generic check: only Generic's
// pattern would claim an arbitrary file URL, so a non-match proves generic
// was never generated into the table — and the URL still falls through to
// aria2.
func TestMatchRejectsPlainDownload(t *testing.T) {
	cache := loadCache(t)
	if cache.Match("https://releases.ubuntu.com/24.04/ubuntu-24.04.iso") {
		t.Error("Match(ubuntu ISO) = true, want false: generic must not be in the table")
	}
}

// TestMatchSuffixBoundary proves the override lookup is a label-boundary
// suffix match, never a raw strings.HasSuffix.
func TestMatchSuffixBoundary(t *testing.T) {
	cache := loadCache(t)
	for _, uri := range []string{
		"https://evilyoutube.com/x",
		"https://notyoutu.be/x",
	} {
		if cache.Match(uri) {
			t.Errorf("Match(%q) = true, want false: suffix match crossed a label boundary", uri)
		}
	}
}

// TestMatchZeroValue covers the empty cache: it answers false for
// everything and reports not loaded.
func TestMatchZeroValue(t *testing.T) {
	var cache ExtractorCache
	if cache.Loaded() {
		t.Error("zero-value cache reports loaded")
	}
	if cache.Match("https://www.youtube.com/watch?v=dQw4w9WgXcQ") {
		t.Error("zero-value cache matched")
	}
	var nilCache *ExtractorCache
	if nilCache.Match("https://www.youtube.com/watch?v=dQw4w9WgXcQ") || nilCache.Loaded() {
		t.Error("nil cache should answer false everywhere")
	}
}

// TestEngineAcceptsUsesCache proves the accept gate of the tasks create
// path reads the same cache: before Connect the stub answer is false, and
// after it a YouTube URL is claimed — an engine-forced submission is no
// longer refused.
func TestEngineAcceptsUsesCache(t *testing.T) {
	e := NewEngine(Config{BinaryPath: "yt-dlp"}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	const youtubeWatch = "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	if e.Accepts(youtubeWatch) {
		t.Fatal("Accepts answered true before Connect loaded the cache")
	}
	if err := e.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !e.Accepts(youtubeWatch) {
		t.Error("Accepts answered false after Connect loaded the cache")
	}
	if e.Accepts("https://releases.ubuntu.com/24.04/ubuntu-24.04.iso") {
		t.Error("Accepts claimed a plain download after Connect")
	}
}
