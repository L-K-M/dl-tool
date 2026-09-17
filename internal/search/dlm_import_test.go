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

	"github.com/danielgtaylor/huma/v2"
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
	)
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
	root := t.TempDir()
	db, err := store.Open(
		ctx, filepath.Join(root, "config", "dl-tool.db"), filepath.Join(root, "backups"))
	require.NoError(t, err)
	defer func() { assert.NoError(t, db.Close()) }()
	indexers, err := store.NewIndexerStore(db, secure.Secret("import-test-secret-key"))
	require.NoError(t, err)
	h := api.NewSearchHandlers(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		api.Deps{Indexers: indexers},
	)

	// The fixture must be read before chdir: dlmFixture resolves
	// testdata/ relative to the working directory.
	fixture := dlmFixture(t, "jackett.dlm")
	scratch := t.TempDir()
	t.Chdir(scratch)
	_, err = h.ImportIndexer(ctx, importInput(t, "jackett.dlm", fixture))
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
