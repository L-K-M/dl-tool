package search

import (
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/L-K-M/dl-tool/definitions"
	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

// bundledTestKey is the throwaway key the test indexer store seals under; it
// stands in for cfg.SecretKey. Bundled rows carry no API key, so it only
// satisfies NewIndexerStore's non-empty requirement.
const bundledTestKey = "bundled-test-secret-key"

// piracyProbes is the FR-052 tripwire: no file under definitions/ may name a
// public tracker. The list lives here, not in a definition, so the bundled
// set stays free of even the mention.
var piracyProbes = []string{
	"1337x", "bitsearch", "demonoid", "eztv", "glodls", "iptorrents",
	"kickass", "limetorrents", "nyaa", "piratebay", "rarbg",
	"solidtorrents", "thepiratebay", "torlock", "torrent9",
	"torrentgalaxy", "torrentleech", "torrentz", "yts", "zooqle",
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// userDefYAML renders a minimal but fully valid rss definition; the id is the
// only variable part.
func userDefYAML(id string) string {
	return `dlsearch: 1
id: ` + id + `
name: User Engine ` + id + `
description: "A user-supplied engine."
homepage: https://example.com/
version: "1.0.0"
legal_tier: user-supplied
kind: rss
caps:
  modes: {search: [q]}
  categories: {all: 8000}
request:
  base_url: https://example.com/
  path: feed.xml
response:
  rows: "rss > channel > item"
  fields:
    title:    {path: "title"}
    size:     {path: "size", type: bytes}
    category: {const: "all"}
    download: {path: "link"}
`
}

func writeUserDef(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write user definition: %v", err)
	}
	return path
}

func TestBundledSetIsExactlyFour(t *testing.T) {
	entries, err := fs.ReadDir(definitions.FS, "engines")
	if err != nil {
		t.Fatalf("read embedded engines: %v", err)
	}

	got := []string{}
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	want := make([]string, 0, len(BundledIDs))
	for _, id := range BundledIDs {
		want = append(want, id+".yaml")
	}
	slices.Sort(got)
	slices.Sort(want)

	if !slices.Equal(got, want) {
		t.Fatalf("bundled engines = %v, want exactly %v", got, want)
	}
	if len(BundledIDs) != 4 {
		t.Fatalf("BundledIDs has %d entries; FR-052 ships exactly four", len(BundledIDs))
	}
}

func TestEveryBundledDefinitionValidates(t *testing.T) {
	for _, id := range BundledIDs {
		data, err := definitions.FS.ReadFile("engines/" + id + ".yaml")
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if _, err := LoadDefinition(data); err != nil {
			t.Errorf("bundled definition %s does not validate: %v", id, err)
		}
	}
}

func TestNoBundledDefinitionUsesKindHTML(t *testing.T) {
	reg, err := NewRegistry(testLogger(), t.TempDir())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	for _, def := range reg.List() {
		if def.Kind == "html" {
			t.Errorf("bundled definition %s uses kind html, forbidden by section 6.1", def.ID)
		}
	}
}

func TestUserDefinitionCollidingIDRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeUserDef(t, dir, "arch-linux.dlsearch.yaml", userDefYAML("arch-linux"))

	reg, err := NewRegistry(testLogger(), dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	errs := reg.Errors()
	if _, ok := errs[path]; !ok {
		t.Fatalf("colliding file not recorded in Errors(); got %v", errs)
	}
	if got := errs[path].Error(); !strings.Contains(got, "id already in use: arch-linux") {
		t.Fatalf("collision error = %q, want %q", got, "id already in use: arch-linux")
	}

	def, ok := reg.Get("arch-linux")
	if !ok {
		t.Fatal("bundled arch-linux missing after the collision")
	}
	if def.Name != "Arch Linux Releases" {
		t.Fatalf("arch-linux resolved to the user file, not the bundled document: %+v", def)
	}
	if src := reg.Source("arch-linux"); src != "bundled" {
		t.Fatalf("Source(arch-linux) = %q, want bundled", src)
	}
}

func TestInvalidUserDefinitionDoesNotBlockOthers(t *testing.T) {
	dir := t.TempDir()
	badPath := writeUserDef(t, dir, "broken.dlsearch.yaml", "dlsearch: 1\nid: [not a scalar\n")
	writeUserDef(t, dir, "good.dlsearch.yaml", userDefYAML("good-engine"))
	writeUserDef(t, dir, "not-a-definition.yaml", userDefYAML("wrong-suffix"))

	reg, err := NewRegistry(testLogger(), dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	if _, ok := reg.Get("good-engine"); !ok {
		t.Fatal("valid user definition was blocked by the invalid sibling")
	}
	if _, ok := reg.Get("wrong-suffix"); ok {
		t.Fatal("a file without the .dlsearch.yaml suffix was loaded")
	}
	if _, ok := reg.Errors()[badPath]; !ok {
		t.Fatalf("invalid file not recorded in Errors(); got %v", reg.Errors())
	}
	if len(reg.List()) != len(BundledIDs)+1 {
		t.Fatalf("List() = %d definitions, want the four bundled plus good-engine", len(reg.List()))
	}
}

func TestSeedIndexersIsIdempotent(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(
		t.Context(),
		filepath.Join(root, "config", "dl-tool.db"),
		filepath.Join(root, "backups"),
	)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	indexers, err := store.NewIndexerStore(db, secure.Secret(bundledTestKey))
	if err != nil {
		t.Fatalf("indexer store: %v", err)
	}

	reg, err := NewRegistry(testLogger(), t.TempDir())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	created, err := reg.SeedIndexers(t.Context(), indexers)
	if err != nil {
		t.Fatalf("first SeedIndexers: %v", err)
	}
	if created != len(BundledIDs) {
		t.Fatalf("first SeedIndexers created %d rows, want %d", created, len(BundledIDs))
	}

	rows, err := indexers.List(t.Context(), false)
	if err != nil {
		t.Fatalf("list indexers: %v", err)
	}
	seen := map[string]store.Indexer{}
	for _, row := range rows {
		if row.DefinitionID == nil {
			continue
		}
		seen[*row.DefinitionID] = row
	}
	for _, id := range BundledIDs {
		row, ok := seen[id]
		if !ok {
			t.Errorf("no indexer row seeded for %s", id)
			continue
		}
		if !row.Enabled {
			t.Errorf("%s: enabled = false, want true", id)
		}
		if row.LegalTier != "legitimate" {
			t.Errorf("%s: legal_tier = %q, want legitimate", id, row.LegalTier)
		}
		if row.DefinitionSource == nil || *row.DefinitionSource != "bundled" {
			t.Errorf("%s: definition_source = %v, want bundled", id, row.DefinitionSource)
		}
		if row.Priority != store.DefaultIndexerPriority {
			t.Errorf("%s: priority = %d, want %d", id, row.Priority, store.DefaultIndexerPriority)
		}
		if row.Provenance == nil || *row.Provenance != "shipped with dl-tool" {
			t.Errorf("%s: provenance = %v, want 'shipped with dl-tool'", id, row.Provenance)
		}
		if !row.SeedersUnknown {
			t.Errorf("%s: seeders_unknown = false, want true from caps", id)
		}
	}

	// The second boot: the unique index on definition_id turns every insert
	// into ErrConflict, which SeedIndexers treats as already seeded.
	created, err = reg.SeedIndexers(t.Context(), indexers)
	if err != nil {
		t.Fatalf("second SeedIndexers: %v", err)
	}
	if created != 0 {
		t.Fatalf("second SeedIndexers created %d rows, want 0", created)
	}
}

func TestUserDefinitionOversizeRejected(t *testing.T) {
	dir := t.TempDir()
	// One byte over the cap, so the bounded read stops inside the file.
	path := writeUserDef(t, dir, "fat.dlsearch.yaml",
		userDefYAML("fat-engine")+strings.Repeat("#", MaxDefinitionBytes))

	reg, err := NewRegistry(testLogger(), dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	errs := reg.Errors()
	if _, ok := errs[path]; !ok {
		t.Fatalf("oversize file not recorded in Errors(); got %v", errs)
	}
	if got := errs[path].Error(); !strings.Contains(got, "over the") || !strings.Contains(got, "byte") {
		t.Fatalf("oversize error = %q, want the byte limit named", got)
	}
	if _, ok := reg.Get("fat-engine"); ok {
		t.Fatal("oversize definition reached the registry")
	}
}

func TestUserDefinitionSymlinkToNonRegularRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evil.dlsearch.yaml")
	// lstat sees the symlink's own tiny size; the target is a device node,
	// not a regular file — the read must refuse it rather than follow it.
	if err := os.Symlink("/dev/null", path); err != nil {
		t.Skipf("symlink unavailable on this platform: %v", err)
	}

	reg, err := NewRegistry(testLogger(), dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	errs := reg.Errors()
	if _, ok := errs[path]; !ok {
		t.Fatalf("symlinked file not recorded in Errors(); got %v", errs)
	}
	if got := errs[path].Error(); !strings.Contains(got, "not a regular file") {
		t.Fatalf("symlink error = %q, want a non-regular-file rejection", got)
	}
}

func TestNoPiracyIndexerNamesInRepository(t *testing.T) {
	// The repository root is two levels up from this package's directory.
	root := filepath.Join("..", "..", "definitions")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		name := strings.ToLower(entry.Name())
		for _, probe := range piracyProbes {
			if strings.Contains(name, probe) {
				t.Errorf("%s names the public tracker %q", path, probe)
			}
		}
		if !strings.HasSuffix(name, ".yaml") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			return nil
		}
		def, err := LoadDefinition(data)
		if err != nil {
			t.Errorf("%s does not validate: %v", path, err)
			return nil
		}
		// A tracker could hide behind a neutral id, so the identity fields
		// and every host the definition points at are probed too.
		targets := []string{def.ID, def.Name, def.Homepage}
		if def.Request != nil {
			targets = append(targets, def.Request.BaseURL)
		}
		for _, e := range def.Entries {
			targets = append(targets, e.Download, e.Magnet, e.Details)
		}
		for _, probe := range piracyProbes {
			for _, target := range targets {
				if strings.Contains(strings.ToLower(target), probe) {
					t.Errorf("%s references the public tracker %q via %q", path, probe, target)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk definitions/: %v", err)
	}
}
