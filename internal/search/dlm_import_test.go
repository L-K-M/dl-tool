// Tests for the .dlm static importer of task T059. The package clause is
// search_test — external — because the row-level assertions drive the
// real api.ImportIndexer handler, and internal/api already imports this
// package.
package search_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/L-K-M/dl-tool/internal/api"
	"github.com/L-K-M/dl-tool/internal/search"
	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

// --- fixtures -----------------------------------------------------------

func dlmFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return data
}

// dlmMember is one member of a test archive; a zero typeflag means a
// regular file.
type dlmMember struct {
	name     string
	body     []byte
	typeflag byte
	linkname string
}

func regMember(name, body string) dlmMember {
	return dlmMember{name: name, body: []byte(body)}
}

// buildDLM packs members into the gzip-compressed tar a .dlm is.
func buildDLM(t *testing.T, members ...dlmMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		tf := m.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{
			Name: m.name, Mode: 0o644, Size: int64(len(m.body)),
			Typeflag: tf, Linkname: m.linkname,
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if tf == tar.TypeReg {
			_, err := tw.Write(m.body)
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// testINFO is a complete, valid INFO object for the in-memory archives.
func testINFO(t *testing.T, module string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"name": "fixture", "displayname": "Fixture", "description": "test module",
		"version": "1.0", "module": module, "type": "search", "class": "SynoDLMSearchFixture",
	})
	require.NoError(t, err)
	return raw
}

// assertDirEmpty is the acceptance check that import writes nothing: the
// directory is empty after the call.
func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "import wrote into %s", dir)
}

// --- the two convertible shapes ----------------------------------------

// TestImportJackettDLMBecomesTorznab covers acceptance criterion 1: the
// torznab-proxy shape converts to a kind: torznab draft carrying the
// base_url and api_key settings, with the credential-remap warning.
func TestImportJackettDLMBecomesTorznab(t *testing.T) {
	res, err := search.ImportDLM(dlmFixture(t, "jackett.dlm"), "jackett.dlm")
	require.NoError(t, err)

	assert.True(t, res.Converted)
	require.NotNil(t, res.Definition)
	assert.Equal(t, "torznab", res.Kind)
	assert.Equal(t, "torznab", res.Definition.Kind)
	assert.Equal(t, "imported:dlm", res.Provenance)
	assert.Equal(t, "jackett.dlm", res.Origin)
	assert.NotEmpty(t, res.Source)

	settings := map[string]search.Setting{}
	for _, s := range res.Definition.Settings {
		settings[s.Name] = s
	}
	require.Contains(t, settings, "base_url")
	require.Contains(t, settings, "api_key")
	assert.Equal(t, "text", settings["base_url"].Type)
	assert.Equal(t, "password", settings["api_key"].Type)

	var warned bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "username") && strings.Contains(w, "base_url") && strings.Contains(w, "api_key") {
			warned = true
		}
	}
	assert.True(t, warned, "warnings %v lack the host/API-key remap message", res.Warnings)

	// The emitted draft must be a definition the loader itself accepts.
	raw, err := yaml.Marshal(res.Definition)
	require.NoError(t, err)
	_, err = search.LoadDefinition(raw)
	assert.NoError(t, err, "converted draft does not validate: %s", raw)
}

// TestImportRSSModuleBecomesRSSDefinition covers the addRSSResults shape:
// the single http literal splits into base_url and path, and its trailing
// query parameter becomes {{ .Keywords }}.
func TestImportRSSModuleBecomesRSSDefinition(t *testing.T) {
	res, err := search.ImportDLM(dlmFixture(t, "rssmodule.dlm"), "rssmodule.dlm")
	require.NoError(t, err)

	assert.True(t, res.Converted)
	require.NotNil(t, res.Definition)
	assert.Equal(t, "rss", res.Definition.Kind)
	require.NotNil(t, res.Definition.Request)
	assert.Equal(t, "http://www.mininova.org", res.Definition.Request.BaseURL)
	assert.Equal(t, "rss.php", res.Definition.Request.Path)
	assert.Equal(t, "{{ .Keywords }}", res.Definition.Request.Query["search"])
	assert.True(t, res.Definition.Caps.SeedersUnknown)

	raw, err := yaml.Marshal(res.Definition)
	require.NoError(t, err)
	_, err = search.LoadDefinition(raw)
	assert.NoError(t, err, "converted draft does not validate: %s", raw)
}

// TestRSSModuleDecodesStaticQueryPairs: a literal carrying a pre-encoded
// static pair must emit the decoded value — the runner re-encodes the
// whole query map, so storing RawQuery verbatim would double-encode it.
func TestRSSModuleDecodesStaticQueryPairs(t *testing.T) {
	archive := buildDLM(t,
		dlmMember{name: "INFO", body: testINFO(t, "search.php")},
		regMember("search.php", `<?php
			$url = "http://example.com/rss.php?cat=10%2C20&search=";
			$this->addRSSResults($curl, $url . urlencode($query));
		?>`),
	)
	res, err := search.ImportDLM(archive, "encoded.dlm")
	require.NoError(t, err)
	require.True(t, res.Converted)
	require.NotNil(t, res.Definition.Request)
	assert.Equal(t, "10,20", res.Definition.Request.Query["cat"])
	assert.Equal(t, "{{ .Keywords }}", res.Definition.Request.Query["search"])
}

// TestRSSModuleNeedsKeywordParam: a literal with no query string, or one
// whose last pair has no usable key, cannot carry {{ .Keywords }} — the
// module is not the modelled shape and falls back to metadata-only.
func TestRSSModuleNeedsKeywordParam(t *testing.T) {
	for name, url := range map[string]string{
		"no query":      "http://example.com/feed.php",
		"trailing &":    "http://example.com/rss.php?search=&",
		"value-only":    "http://example.com/rss.php?=x",
		"path-appended": "http://example.com/feed/",
	} {
		t.Run(name, func(t *testing.T) {
			archive := buildDLM(t,
				dlmMember{name: "INFO", body: testINFO(t, "search.php")},
				regMember("search.php", `<?php
					$url = "`+url+`";
					$this->addRSSResults($curl, $url . urlencode($query));
				?>`),
			)
			res, err := search.ImportDLM(archive, "nokey.dlm")
			require.NoError(t, err)
			assert.False(t, res.Converted)
			assert.Nil(t, res.Definition)
			require.Len(t, res.Warnings, 1)
			assert.Contains(t, res.Warnings[0], "dl-tool does not execute")
		})
	}
}

// TestRSSModuleIgnoresCommentURLs: a quoted URL inside a PHP comment is
// not a literal — it must not count toward, or become, the single http
// literal the conversion keys on.
func TestRSSModuleIgnoresCommentURLs(t *testing.T) {
	archive := buildDLM(t,
		dlmMember{name: "INFO", body: testINFO(t, "search.php")},
		regMember("search.php", `<?php
			// upstream mirror: "https://mirror.example.com/rss.php?search="
			$url = "http://example.com/rss.php?search=";
			$this->addRSSResults($curl, $url . urlencode($query));
		?>`),
	)
	res, err := search.ImportDLM(archive, "comment.dlm")
	require.NoError(t, err)
	require.True(t, res.Converted)
	assert.Equal(t, "http://example.com", res.Definition.Request.BaseURL)
}

// TestUnterminatedStringLiteralDoesNotPanic: a module ending in an
// unterminated string with a trailing escape previously sliced one byte
// past the member buffer in stripPHPComments — a panic whenever the read
// buffer's capacity equals its length, which a 512-byte member hits
// exactly. Attacker-reachable via a crafted .dlm upload.
func TestUnterminatedStringLiteralDoesNotPanic(t *testing.T) {
	prefix := "<?php $this->addRSSResults($curl, $x); "
	body := prefix + strings.Repeat("x", 512-len(prefix)-3) + "'a\\"
	require.Len(t, body, 512)
	archive := buildDLM(t,
		dlmMember{name: "INFO", body: testINFO(t, "search.php")},
		regMember("search.php", body),
	)
	res, err := search.ImportDLM(archive, "panic.dlm")
	require.NoError(t, err)
	assert.False(t, res.Converted)
}

// TestImportDefinitionFileTorznabKind: a user-uploaded kind: torznab
// definition reports Kind "torznab", matching ImportDLM's conversion.
func TestImportDefinitionFileTorznabKind(t *testing.T) {
	res, err := search.ImportDefinitionFile([]byte(`dlsearch: 1
id: imported-torznab
name: Imported Torznab
description: test
homepage: https://example.com/
version: "1.0"
legal_tier: user-supplied
kind: torznab
caps:
  modes: {search: [q]}
  categories: {all: 2000}
`), "imported-torznab.dlsearch.yaml")
	require.NoError(t, err)
	assert.Equal(t, "torznab", res.Kind)
	assert.Equal(t, "imported:file", res.Provenance)
}

// TestUnconvertibleModuleImportsDisabledMetadataOnly covers the fallback:
// custom PHP converts to nothing — the row the API creates is disabled and
// carries the doc 07 section 4.1 message as a warning.
func TestUnconvertibleModuleImportsDisabledMetadataOnly(t *testing.T) {
	scratch := t.TempDir()
	// Chdir so a relative-path write by the import path would land where
	// the closing empty-dir check can observe it.
	t.Chdir(scratch)
	archive := buildDLM(t,
		dlmMember{name: "INFO", body: testINFO(t, "search.php")},
		regMember("search.php", `<?php class SynoDLMSearchFixture {
			public function parse($curl, $response) {
				$doc = new DOMDocument();
				return $this->addCustom($response);
			}
		} ?>`),
	)

	res, err := search.ImportDLM(archive, "fixture.dlm")
	require.NoError(t, err)
	assert.False(t, res.Converted)
	assert.Nil(t, res.Definition)
	assert.Equal(t, "Fixture", res.Name)
	assert.Equal(t, "fixture.dlm", res.Origin)
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "dl-tool does not execute")

	h := newImportHandlers(t)
	out, herr := h.ImportIndexer(context.Background(), importInput(t, "fixture.dlm", archive))
	require.NoError(t, herr)
	assert.False(t, out.Body.Indexer.Enabled)
	require.NotNil(t, out.Body.Indexer.Provenance)
	assert.Equal(t, "imported:dlm", *out.Body.Indexer.Provenance)
	assert.Nil(t, out.Body.Indexer.DefinitionID, "a metadata-only import carries no definition")
	assertDirEmpty(t, scratch)
}

// --- rejections ---------------------------------------------------------

// TestRejectsSymlinkMember: doc 07 section 4.1 accepts regular files only.
func TestRejectsSymlinkMember(t *testing.T) {
	archive := buildDLM(t,
		dlmMember{name: "INFO", body: testINFO(t, "search.php")},
		regMember("search.php", "<?php"),
		dlmMember{name: "link.php", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	)
	_, err := search.ImportDLM(archive, "hostile.dlm")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")
	assert.Contains(t, err.Error(), "regular file")
}

// TestRejectsTraversalName: a member name containing .. is refused.
func TestRejectsTraversalName(t *testing.T) {
	archive := buildDLM(t,
		dlmMember{name: "INFO", body: testINFO(t, "search.php")},
		regMember("search.php", "<?php"),
		regMember("../escape.php", "<?php"),
	)
	_, err := search.ImportDLM(archive, "hostile.dlm")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "..")
}

// TestRejectsOversizeMember: one member over the 1 MiB per-member cap is
// refused by name.
func TestRejectsOversizeMember(t *testing.T) {
	archive := buildDLM(t,
		dlmMember{name: "INFO", body: testINFO(t, "search.php")},
		regMember("search.php", "<?php"),
		dlmMember{name: "big.php", body: bytes.Repeat([]byte("x"), 2<<20)},
	)
	_, err := search.ImportDLM(archive, "hostile.dlm")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "per-member limit")
	assert.Contains(t, err.Error(), "big.php")
}

// TestHostileFixtureRejected: the committed hostile.dlm — symlink member,
// .. name and a 5 MiB member in one archive — is refused; the
// above-capacity members push the stream past the 4 MiB total cap.
func TestHostileFixtureRejected(t *testing.T) {
	_, err := search.ImportDLM(dlmFixture(t, "hostile.dlm"), "hostile.dlm")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "uncompressed archive exceeds")
}

// TestRejectsDuplicateMember: two members with one name make the archive
// ambiguous — INFO or the module could differ between the passes — so the
// first pass rejects duplicates outright.
func TestRejectsDuplicateMember(t *testing.T) {
	archive := buildDLM(t,
		dlmMember{name: "INFO", body: testINFO(t, "search.php")},
		regMember("search.php", "<?php"),
		dlmMember{name: "INFO", body: testINFO(t, "other.php")},
	)
	_, err := search.ImportDLM(archive, "dup.dlm")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")
	assert.Contains(t, err.Error(), "INFO")
}

// TestRejectsMissingModuleMember: the member INFO.module names must exist.
func TestRejectsMissingModuleMember(t *testing.T) {
	archive := buildDLM(t,
		dlmMember{name: "INFO", body: testINFO(t, "missing.php")},
		regMember("search.php", "<?php"),
	)
	_, err := search.ImportDLM(archive, "fixture.dlm")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "INFO.module")
	assert.Contains(t, err.Error(), "missing.php")
}

// TestRejectsMalformedArchives pins the remaining section 4.1 rows: an
// upload over the compressed cap, a non-gzip body, a missing INFO, an INFO
// that is not an object, and a type other than "search".
func TestRejectsMalformedArchives(t *testing.T) {
	_, err := search.ImportDLM(bytes.Repeat([]byte("z"), search.MaxDLMCompressedBytes+1), "big.dlm")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compressed limit")

	_, err = search.ImportDLM([]byte("not a gzip stream"), "x.dlm")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gzip")

	_, err = search.ImportDLM(buildDLM(t, regMember("search.php", "<?php")), "x.dlm")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "INFO")

	_, err = search.ImportDLM(buildDLM(t,
		regMember("INFO", "[1,2]"), regMember("search.php", "<?php")), "x.dlm")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JSON object")

	info, merr := json.Marshal(map[string]any{
		"name": "f", "version": "1", "module": "search.php", "type": "host", "class": "C",
	})
	require.NoError(t, merr)
	_, err = search.ImportDLM(buildDLM(t,
		dlmMember{name: "INFO", body: info}, regMember("search.php", "<?php")), "x.dlm")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `only "search" is supported`)
}

// TestNoInterpreterIsSpawned parses every source file of the package and
// asserts it imports neither os/exec nor plugin — and nothing that could
// reach a PHP or Python runtime (acceptance criterion 5).
func TestNoInterpreterIsSpawned(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	forbidden := map[string]bool{
		"os/exec": true,
		"plugin":  true,
	}
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		checked++
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		require.NoError(t, err)
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			assert.False(t, forbidden[path], "%s imports %s — the importer must never spawn or load code", name, path)
			assert.NotContains(t, path, "php", "%s imports %s — no PHP runtime binding is permitted", name, path)
			assert.NotContains(t, path, "python", "%s imports %s — no Python runtime binding is permitted", name, path)
		}
	}
	require.Positive(t, checked, "the scan found no Go sources")
}

// --- the endpoint -------------------------------------------------------

// importForm packs one "file" part into a multipart/form-data body.
func importForm(t *testing.T, filename string, data []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = part.Write(data)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Bytes(), w.FormDataContentType()
}

func importInput(t *testing.T, filename string, data []byte) *api.ImportIndexerInput {
	t.Helper()
	body, contentType := importForm(t, filename, data)
	return &api.ImportIndexerInput{ContentType: contentType, RawBody: body}
}

// newImportHandlers builds the handler over a real migrated store, the way
// cmd/dl-tool wires it — minus the auth middleware a direct call bypasses.
func newImportHandlers(t *testing.T) *api.SearchHandlers {
	t.Helper()
	h, _ := newImportHandlersWithDB(t)
	return h
}

// newImportHandlersWithDB additionally returns the store's handle so a test
// can assert on the persisted row.
func newImportHandlersWithDB(t *testing.T) (*api.SearchHandlers, *sqlx.DB) {
	t.Helper()
	root := t.TempDir()
	db, err := store.Open(
		t.Context(),
		filepath.Join(root, "config", "dl-tool.db"),
		filepath.Join(root, "backups"),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	indexers, err := store.NewIndexerStore(db, secure.Secret("import-test-secret-key"))
	require.NoError(t, err)
	return api.NewSearchHandlers(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		api.Deps{Indexers: indexers},
	), db
}

// problemOf unwraps the handler error into its problem document.
func problemOf(t *testing.T, err error) *huma.ErrorModel {
	t.Helper()
	var em *huma.ErrorModel
	require.ErrorAs(t, err, &em)
	return em
}

// TestImportEndpointCreatesDisabledRows covers acceptance criterion 2 at
// the handler level: every import path — .dlm and .dlsearch.yaml —
// produces a row with enabled = 0, definition_source = 'imported' and the
// right provenance, with the inert source and origin on settings_json.
func TestImportEndpointCreatesDisabledRows(t *testing.T) {
	ctx := context.Background()

	t.Run("dlm", func(t *testing.T) {
		h := newImportHandlers(t)
		out, err := h.ImportIndexer(ctx, importInput(t, "jackett.dlm", dlmFixture(t, "jackett.dlm")))
		require.NoError(t, err)
		dto := out.Body.Indexer
		assert.False(t, dto.Enabled)
		require.NotNil(t, dto.Provenance)
		assert.Equal(t, "imported:dlm", *dto.Provenance)
		require.NotNil(t, dto.DefinitionSource)
		assert.Equal(t, "imported", *dto.DefinitionSource)
		assert.Equal(t, "user-supplied", dto.LegalTier)
		require.NotNil(t, dto.DefinitionID)
		assert.Equal(t, "jackett", *dto.DefinitionID)
		assert.NotEmpty(t, out.Body.Warnings)
	})

	t.Run("dlsearch.yaml", func(t *testing.T) {
		h := newImportHandlers(t)
		out, err := h.ImportIndexer(ctx, importInput(t, "def_valid_rss.dlsearch.yaml", dlmFixture(t, "def_valid_rss.yaml")))
		require.NoError(t, err)
		dto := out.Body.Indexer
		assert.False(t, dto.Enabled)
		require.NotNil(t, dto.Provenance)
		assert.Equal(t, "imported:file", *dto.Provenance)
		require.NotNil(t, dto.DefinitionSource)
		assert.Equal(t, "imported", *dto.DefinitionSource)
		assert.Equal(t, "user-supplied", dto.LegalTier)
		require.NotNil(t, dto.DefinitionID)
		assert.Equal(t, "arch-linux", *dto.DefinitionID)
	})
}

// TestImportEndpointSettingsJSON verifies the provenance document the row
// stores: the uploaded file name on origin and the inert module source,
// plus the converted draft for the .dlm path.
func TestImportEndpointSettingsJSON(t *testing.T) {
	ctx := context.Background()
	h, db := newImportHandlersWithDB(t)

	// The fixture must be read before chdir: dlmFixture resolves
	// testdata/ relative to the working directory.
	fixture := dlmFixture(t, "jackett.dlm")
	scratch := t.TempDir()
	t.Chdir(scratch)
	_, err := h.ImportIndexer(ctx, importInput(t, "jackett.dlm", fixture))
	require.NoError(t, err)

	var settingsJSON string
	require.NoError(t, db.GetContext(ctx, &settingsJSON,
		"SELECT settings_json FROM indexers WHERE definition_id = 'jackett'"))
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(settingsJSON), &doc))
	assert.Equal(t, "jackett.dlm", doc["origin"])
	assert.Equal(t, true, doc["auto_converted"])
	srcB64, ok := doc["module_source_b64"].(string)
	require.True(t, ok, "module_source_b64 missing from settings_json: %s", settingsJSON)
	src, err := base64.StdEncoding.DecodeString(srcB64)
	require.NoError(t, err)
	assert.Contains(t, string(src), "SynoDLMSearchJackett")
	defYAML, ok := doc["definition_yaml"].(string)
	require.True(t, ok, "definition_yaml missing from settings_json: %s", settingsJSON)
	assert.Contains(t, defYAML, "kind: torznab")

	// Nothing the import ran touched the filesystem: the scratch directory
	// is still empty.
	assertDirEmpty(t, scratch)
}

// TestImportEndpointCapsOrigin: an over-long client-supplied file name is
// stored truncated at 255 bytes and still valid UTF-8 — the byte cap must
// not split a multi-byte rune.
func TestImportEndpointCapsOrigin(t *testing.T) {
	ctx := context.Background()
	h, db := newImportHandlersWithDB(t)

	archive := buildDLM(t,
		dlmMember{name: "INFO", body: testINFO(t, "search.php")},
		regMember("search.php", "<?php"),
	)
	longName := strings.Repeat("é", 200) + ".dlm"
	_, err := h.ImportIndexer(ctx, importInput(t, longName, archive))
	require.NoError(t, err)

	var settingsJSON string
	require.NoError(t, db.GetContext(ctx, &settingsJSON,
		"SELECT settings_json FROM indexers LIMIT 1"))
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(settingsJSON), &doc))
	origin, ok := doc["origin"].(string)
	require.True(t, ok, "origin missing from settings_json: %s", settingsJSON)
	assert.LessOrEqual(t, len(origin), 255)
	assert.True(t, utf8.ValidString(origin), "stored origin is not valid UTF-8: %q", origin)
}

// TestImportEndpointRejections maps the multipart failures: an unknown
// extension and a bad archive are 422, an over-cap part is 413, and a
// non-multipart non-JSON content type stays 415.
func TestImportEndpointRejections(t *testing.T) {
	ctx := context.Background()
	h := newImportHandlers(t)

	out, err := h.ImportIndexer(ctx, importInput(t, "notes.txt", []byte("hello")))
	em := problemOf(t, err)
	assert.Equal(t, http.StatusUnprocessableEntity, em.Status)
	assert.Equal(t, "/problems/validation-failed", em.Type)
	assert.Nil(t, out)

	_, err = h.ImportIndexer(ctx, importInput(t, "evil.dlm", dlmFixture(t, "hostile.dlm")))
	em = problemOf(t, err)
	assert.Equal(t, http.StatusUnprocessableEntity, em.Status)
	assert.Equal(t, "/problems/validation-failed", em.Type)

	_, err = h.ImportIndexer(ctx, importInput(t, "big.dlm", bytes.Repeat([]byte("x"), (1<<20)+1)))
	em = problemOf(t, err)
	assert.Equal(t, http.StatusRequestEntityTooLarge, em.Status)
	assert.Equal(t, "/problems/payload-too-large", em.Type)

	_, err = h.ImportIndexer(ctx, &api.ImportIndexerInput{
		ContentType: "text/plain", RawBody: []byte("x"),
	})
	em = problemOf(t, err)
	assert.Equal(t, http.StatusUnsupportedMediaType, em.Status)
	assert.Equal(t, "/problems/unsupported-media-type", em.Type)
}

// TestImportEndpointDuplicateConflicts: a second import of the same
// converted module hits the definition_id unique index as 409.
func TestImportEndpointDuplicateConflicts(t *testing.T) {
	ctx := context.Background()
	h := newImportHandlers(t)

	_, err := h.ImportIndexer(ctx, importInput(t, "jackett.dlm", dlmFixture(t, "jackett.dlm")))
	require.NoError(t, err)
	_, err = h.ImportIndexer(ctx, importInput(t, "jackett.dlm", dlmFixture(t, "jackett.dlm")))
	em := problemOf(t, err)
	assert.Equal(t, http.StatusConflict, em.Status)
	assert.Equal(t, "/problems/conflict", em.Type)
}

// --- nova3 .py import (T060) --------------------------------------------

// TestPluginVersionRule pins the doc 07 section 4.3 version scan: every
// line is read, ALL spaces are stripped, the first line starting with
// #VERSION: case-insensitively wins, and a line longer than the 16 bytes
// qBittorrent reads counts as absent.
func TestPluginVersionRule(t *testing.T) {
	body := "class v(object):\n    name = \"x\"\n"
	for name, tc := range map[string]struct {
		header string
		want   string
	}{
		"normal":        {"#VERSION:1.42\n", "1.42"},
		"spaced":        {"# VERSION: 1.42\n", "1.42"},
		"mixed case":    {"#version:2.0\n", "2.0"},
		"first wins":    {"#VERSION:9.9\n#VERSION:1.0\n", "9.9"},
		"exactly 16":    {"#VERSION:1234567\n", "1234567"},
		"spaces folded": {"#VERSION: 1.0b\n", "1.0b"},
		"over 16 bytes": {"#VERSION:1.42.4.5\n", ""},
		"over 16 raw":   {"#VERSION: 1.0 beta\n", ""},
		"absent":        {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := search.ImportNovaPlugin([]byte(tc.header+body), "v.py")
			require.NoError(t, err)
			assert.Equal(t, tc.want, res.Version)
		})
	}
}

// TestPluginMetadataExtracted covers acceptance criterion 2: the name, url
// and all nine supported_categories keys are read from legacy_plugin.py,
// and the friendly names fold to their jackett.py newznab ids.
func TestPluginMetadataExtracted(t *testing.T) {
	res, err := search.ImportNovaPlugin(dlmFixture(t, "legacy_plugin.py"), "legacy_plugin.py")
	require.NoError(t, err)

	assert.False(t, res.Converted)
	assert.Nil(t, res.Definition)
	assert.Equal(t, "dlsearch", res.Kind)
	assert.Equal(t, "imported:qbt-py", res.Provenance)
	assert.Equal(t, "legacy_plugin.py", res.Origin)
	assert.NotEmpty(t, res.Source)

	assert.Equal(t, "1.42", res.Version)
	assert.Equal(t, "Legacy Tracker", res.Name)
	assert.Equal(t, "https://legacy-tracker.example", res.URL)

	require.NotNil(t, res.SiteCategories)
	for _, key := range []string{
		"all", "anime", "books", "games", "movies", "music", "pictures", "software", "tv",
	} {
		assert.Contains(t, res.SiteCategories, key)
	}
	assert.Equal(t, "100", res.SiteCategories["books"], "site value is carried verbatim")

	ids := make([]int, 0, len(res.Categories))
	for _, c := range res.Categories {
		ids = append(ids, c.ID)
	}
	// games and software share 4000 — deduplicated and sorted.
	assert.Equal(t, []int{1000, 2000, 3000, 4000, 5000, 5070, 8000}, ids)
}

// TestPluginClassNameMismatchWarns: qBittorrent resolves the class with
// getattr(module, file_stem); a class named differently imports with a
// warning, not a refusal.
func TestPluginClassNameMismatchWarns(t *testing.T) {
	res, err := search.ImportNovaPlugin(dlmFixture(t, "legacy_plugin.py"), "renamed.py")
	require.NoError(t, err)

	var warned bool
	for _, w := range res.Warnings {
		if strings.Contains(w, `"legacy_plugin"`) && strings.Contains(w, `"renamed"`) &&
			strings.Contains(w, "getattr") {
			warned = true
		}
	}
	assert.True(t, warned, "warnings %v lack the class/stem mismatch message", res.Warnings)
	// The row still needs a name: the literal one is kept.
	assert.Equal(t, "Legacy Tracker", res.Name)
}

// TestPicturesCategoryWarns: the one friendly name with no documented
// newznab id is imported unmapped and raises the mandated warning.
func TestPicturesCategoryWarns(t *testing.T) {
	res, err := search.ImportNovaPlugin(dlmFixture(t, "legacy_plugin.py"), "legacy_plugin.py")
	require.NoError(t, err)
	assert.Contains(t, res.Warnings,
		`category "pictures" has no newznab equivalent and is imported unmapped`)
}

// TestHostilePluginIsNotExecuted covers acceptance criterion 3: the
// hostile fixture's module-scope calls — a file write, a subprocess and an
// environment poke — produce none of their side effects, because nothing
// in the file ever runs.
func TestHostilePluginIsNotExecuted(t *testing.T) {
	// Read the fixture before chdir: it resolves testdata/ relative to the
	// working directory.
	fixture := dlmFixture(t, "hostile_plugin.py")
	scratch := t.TempDir()
	t.Chdir(scratch)
	t.Setenv("DLTOOL_PWNED", "clean")

	res, err := search.ImportNovaPlugin(fixture, "hostile_plugin.py")
	require.NoError(t, err)
	assert.Equal(t, "Hostile Plugin", res.Name)
	assert.Equal(t, "9.9", res.Version)

	assert.Equal(t, "clean", os.Getenv("DLTOOL_PWNED"))
	_, statErr := os.Stat("PWNED_FROM_PLUGIN.txt")
	assert.True(t, errors.Is(statErr, os.ErrNotExist), "the plugin's marker file exists")
	assertDirEmpty(t, scratch)
}

// TestPluginImportedDisabled covers acceptance criterion 4 at the handler
// level: the row a .py upload creates is disabled, carries
// imported:qbt-py provenance, and the response warns that dl-tool does not
// run Python and points at dlsearch/v1.
func TestPluginImportedDisabled(t *testing.T) {
	ctx := context.Background()
	h, db := newImportHandlersWithDB(t)

	fixture := dlmFixture(t, "legacy_plugin.py")
	out, err := h.ImportIndexer(ctx, importInput(t, "legacy_plugin.py", fixture))
	require.NoError(t, err)

	dto := out.Body.Indexer
	assert.False(t, dto.Enabled)
	assert.Equal(t, "dlsearch", dto.Kind)
	require.NotNil(t, dto.Provenance)
	assert.Equal(t, "imported:qbt-py", *dto.Provenance)
	require.NotNil(t, dto.DefinitionSource)
	assert.Equal(t, "imported", *dto.DefinitionSource)
	assert.Equal(t, "user-supplied", dto.LegalTier)
	assert.Nil(t, dto.DefinitionID, "a nova3 plugin never converts to a definition")
	assert.True(t, dto.SeedersUnknown)
	assert.Equal(t, "Legacy Tracker", dto.Name)
	require.NotNil(t, dto.URL)
	assert.Equal(t, "https://legacy-tracker.example", *dto.URL)
	// The mapped newznab ids ride on the row's categories.
	require.NotEmpty(t, dto.Categories)
	ids := make([]int, 0, len(dto.Categories))
	for _, c := range dto.Categories {
		ids = append(ids, c.ID)
	}
	assert.Equal(t, []int{1000, 2000, 3000, 4000, 5000, 5070, 8000}, ids)

	var warned bool
	for _, w := range out.Body.Warnings {
		if strings.Contains(w, "dlsearch/v1") && strings.Contains(w, "does not run Python") {
			warned = true
		}
	}
	assert.True(t, warned, "warnings %v lack the dlsearch/v1 message", out.Body.Warnings)

	// settings_json keeps the inert source, the origin, the version and
	// the verbatim site categories.
	var settingsJSON string
	require.NoError(t, db.GetContext(ctx, &settingsJSON,
		"SELECT settings_json FROM indexers LIMIT 1"))
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(settingsJSON), &doc))
	assert.Equal(t, "legacy_plugin.py", doc["origin"])
	assert.Equal(t, false, doc["auto_converted"])
	assert.Equal(t, "1.42", doc["plugin_version"])
	siteCats, ok := doc["plugin_categories"].(map[string]any)
	require.True(t, ok, "plugin_categories missing from settings_json: %s", settingsJSON)
	assert.Equal(t, "100", siteCats["books"])
	srcB64, ok := doc["module_source_b64"].(string)
	require.True(t, ok, "module_source_b64 missing from settings_json: %s", settingsJSON)
	src, err := base64.StdEncoding.DecodeString(srcB64)
	require.NoError(t, err)
	assert.Contains(t, string(src), "legacy_plugin")
}

// TestPluginImportRefusals names the rules a .py upload is rejected on:
// over the 512 KiB cap, not valid UTF-8, or no class at all — a file with
// no class is not a nova3 plugin.
func TestPluginImportRefusals(t *testing.T) {
	_, err := search.ImportNovaPlugin(
		bytes.Repeat([]byte("x"), search.MaxNovaPluginBytes+1), "big.py")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin limit")

	_, err = search.ImportNovaPlugin([]byte{0xff, 0xfe, 0x00}, "x.py")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UTF-8")

	_, err = search.ImportNovaPlugin([]byte("name = 'x'\n"), "noclass.py")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "class")

	// A name with no stem — ".py" itself — can never name the class
	// qBittorrent would resolve.
	_, err = search.ImportNovaPlugin(
		[]byte("class unnamed(object):\n\tname = 'X'\n"), ".py")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no stem")

	// A refused upload surfaces as the 422 validation problem.
	h := newImportHandlers(t)
	_, herr := h.ImportIndexer(context.Background(),
		importInput(t, "noclass.py", []byte("name = 'x'\n")))
	em := problemOf(t, herr)
	assert.Equal(t, http.StatusUnprocessableEntity, em.Status)
	assert.Equal(t, "/problems/validation-failed", em.Type)
}

// TestPluginBOMPrefixedSource: a UTF-8 BOM — which CPython accepts — must
// not hide the first line's class or #VERSION: header from the reader,
// while the stored source keeps the uploaded bytes verbatim.
func TestPluginBOMPrefixedSource(t *testing.T) {
	src := append([]byte("\xef\xbb\xbf"),
		[]byte("#VERSION:1.0\nclass bommed(object):\n\tname = 'Bom'\n")...)
	res, err := search.ImportNovaPlugin(src, "bommed.py")
	require.NoError(t, err)
	assert.Equal(t, "Bom", res.Name)
	assert.Equal(t, "1.0", res.Version)
	assert.Equal(t, src, res.Source)
}

// TestPluginCategoriesUnreadableWarns: a supported_categories spelled with
// a non-literal value — a name reference here — imports nothing but must
// say so, rather than silently losing the declaration.
func TestPluginCategoriesUnreadableWarns(t *testing.T) {
	res, err := search.ImportNovaPlugin(
		[]byte("class nounmap(object):\n\tname = 'N'\n"+
			"\tsupported_categories = CATS\n"), "nounmap.py")
	require.NoError(t, err)
	assert.Empty(t, res.SiteCategories)
	assert.Contains(t, res.Warnings,
		"supported_categories yielded no quoted-string mappings; no categories were imported")
}

// TestPluginQuoteEscapes: the literal reader honors both quote escapes in
// either quote style — as Python does — while a non-quote sequence keeps
// its backslash verbatim.
func TestPluginQuoteEscapes(t *testing.T) {
	res, err := search.ImportNovaPlugin(
		[]byte("class esc(object):\n\tname = 'E'\n"+
			"\tsupported_categories = {'movies': '20\\'s', 'tv': \"x\\ty\"}\n"),
		"esc.py")
	require.NoError(t, err)
	assert.Equal(t, "20's", res.SiteCategories["movies"])
	assert.Equal(t, `x\ty`, res.SiteCategories["tv"])
}

// TestPluginFieldsStayOnTheNovaPath pins the provenance gate on the
// plugin_* extras: a .dlm import must never grow plugin_version or
// plugin_categories keys, and a .py plugin whose declared url is not a
// valid http(s) URL warns rather than landing on the row.
func TestPluginFieldsStayOnTheNovaPath(t *testing.T) {
	ctx := context.Background()
	h, db := newImportHandlersWithDB(t)

	_, err := h.ImportIndexer(ctx,
		importInput(t, "jackett.dlm", dlmFixture(t, "jackett.dlm")))
	require.NoError(t, err)
	var settingsJSON string
	require.NoError(t, db.GetContext(ctx, &settingsJSON,
		"SELECT settings_json FROM indexers WHERE definition_id = 'jackett'"))
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(settingsJSON), &doc))
	assert.NotContains(t, doc, "plugin_version")
	assert.NotContains(t, doc, "plugin_categories")

	bad := []byte("class badurl(object):\n\tname = 'B'\n\turl = 'no-scheme.example.com'\n")
	out, err := h.ImportIndexer(ctx, importInput(t, "badurl.py", bad))
	require.NoError(t, err)
	assert.Nil(t, out.Body.Indexer.URL)
	assert.Contains(t, out.Body.Warnings,
		"the plugin's url is not an http or https URL; it is kept only in the stored source")
}
