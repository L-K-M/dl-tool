package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/jobs"
	"github.com/L-K-M/dl-tool/internal/search"
	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

// searchTestEnv is one humatest server against a real migrated store with
// the search deps wired the way cmd/dl-tool builds them; the auth gate is
// satisfied by a seeded bearer token, which is CSRF-exempt.
type searchTestEnv struct {
	api      humatest.TestAPI
	db       *sqlx.DB
	indexers *store.IndexerStore
	bearer   string
}

// searchTestKey is the throwaway key the test store seals under; it stands
// in for cfg.SecretKey.
const searchTestKey = "search-test-secret-key"

func newSearchTestEnv(t *testing.T, hc *http.Client) *searchTestEnv {
	t.Helper()
	return newSearchTestEnvDeps(t, hc, nil, nil)
}

// newSearchTestEnvDeps is newSearchTestEnv with the registry and runner the
// test-indexer cases need; nil means the server behaves as built for the
// OpenAPI document alone.
func newSearchTestEnvDeps(t *testing.T, hc *http.Client, defs *search.Registry, runner *search.Runner) *searchTestEnv {
	t.Helper()

	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	db, err := store.Open(
		t.Context(),
		filepath.Join(configDir, "dl-tool.db"),
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

	indexers, err := store.NewIndexerStore(db, secure.Secret(searchTestKey))
	if err != nil {
		t.Fatalf("indexer store: %v", err)
	}

	server, err := NewServer(
		&config.Config{ConfigDir: configDir, SessionTTL: time.Hour},
		db,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Deps{Indexers: indexers, Defs: defs, Runner: runner, HTTP: hc, DB: db},
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(server.Shutdown)

	env := &searchTestEnv{
		api:      humatest.Wrap(t, server.API),
		db:       db,
		indexers: indexers,
	}
	env.bearer = seedLiveAPIToken(t, db, seedUser(t, db).ID)

	return env
}

// authz is the bearer header every call carries.
func (e *searchTestEnv) authz() string { return "Authorization: Bearer " + e.bearer }

func (e *searchTestEnv) createIndexer(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return e.api.Post("/indexers", body, e.authz())
}

// indexerCount runs the count query and fails the test on error.
func (e *searchTestEnv) indexerCount(t *testing.T, where string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.GetContext(t.Context(), &n, "SELECT COUNT(*) FROM indexers "+where, args...); err != nil {
		t.Fatalf("count indexers: %v", err)
	}
	return n
}

// capsStubXML is a minimal t=caps document carrying one site-specific id.
const capsStubXML = `<caps><server title="stub"/><limits max="100"/>` +
	`<searching><search available="yes" supportedParams="q"/></searching>` +
	`<categories><category id="2000" name="Movies"><subcat id="2040" name="HD"/></category>` +
	`<category id="100001" name="Site Custom"/></categories></caps>`

// TestCreateIndexerRequiresURL covers the 422 of step 6: a torznab or
// newznab indexer without url is /problems/validation-failed.
func TestCreateIndexerRequiresURL(t *testing.T) {
	env := newSearchTestEnv(t, nil)

	for _, kind := range []string{"torznab", "newznab"} {
		resp := env.createIndexer(t, map[string]any{"name": "no url", "kind": kind})
		assertProblem(t, resp, http.StatusUnprocessableEntity, SlugValidationFailed)
	}

	// A dlsearch indexer without definition_id is refused the same way.
	resp := env.createIndexer(t, map[string]any{"name": "no def", "kind": "dlsearch"})
	assertProblem(t, resp, http.StatusUnprocessableEntity, SlugValidationFailed)

	// A url the torznab client can never fetch is refused, not stored.
	resp = env.createIndexer(t, map[string]any{"name": "bad url", "kind": "torznab", "url": "ftp://x"})
	assertProblem(t, resp, http.StatusUnprocessableEntity, SlugValidationFailed)

	if got := env.indexerCount(t, ""); got != 0 {
		t.Errorf("indexer rows = %d, want 0 after refused creates", got)
	}
}

// TestAPIKeyNeverReturned pins the secrecy rules of section 9.1: the list
// reports api_key_set and never a key, a ciphertext or a [REDACTED]
// placeholder, and the stored ciphertext holds no substring of the key.
func TestAPIKeyNeverReturned(t *testing.T) {
	env := newSearchTestEnv(t, nil)

	const submittedKey = "super-secret-indexer-key-7f3a"
	resp := env.createIndexer(t, map[string]any{
		"name":          "keyed",
		"kind":          "dlsearch",
		"definition_id": "keyed-def",
		"api_key":       submittedKey,
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), submittedKey) {
		t.Fatalf("create response echoes the api key: %s", resp.Body.String())
	}
	var created struct {
		APIKeySet bool `json:"api_key_set"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create body: %v", err)
	}
	if !created.APIKeySet {
		t.Error("api_key_set = false, want true")
	}

	list := env.api.Get("/indexers", env.authz())
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", list.Code, list.Body.String())
	}
	if strings.Contains(list.Body.String(), submittedKey) || strings.Contains(list.Body.String(), "[REDACTED]") {
		t.Fatalf("list leaks the key or a placeholder: %s", list.Body.String())
	}

	var enc string
	if err := env.db.GetContext(
		t.Context(), &enc,
		"SELECT api_key_enc FROM indexers WHERE name = 'keyed'",
	); err != nil {
		t.Fatalf("read api_key_enc: %v", err)
	}
	if strings.Contains(enc, submittedKey) {
		t.Fatalf("api_key_enc holds a substring of the submitted key: %q", enc)
	}

	// The seal round-trips inside the store only.
	row, err := env.indexers.Get(t.Context(), indexersRowID(t, env.db, "keyed"))
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	opened, err := env.indexers.OpenAPIKey(row)
	if err != nil {
		t.Fatalf("open api key: %v", err)
	}
	if opened.Reveal() != submittedKey {
		t.Error("OpenAPIKey did not round-trip the submitted key")
	}
}

// indexersRowID resolves the id of the row with the given name.
func indexersRowID(t *testing.T, db *sqlx.DB, name string) string {
	t.Helper()
	var id string
	if err := db.GetContext(t.Context(), &id, "SELECT id FROM indexers WHERE name = ?", name); err != nil {
		t.Fatalf("read indexer id: %v", err)
	}
	return id
}

// TestPatchIndexerPartial covers the patch semantics: omitted fields are
// untouched and api_key: "" clears the stored key.
func TestPatchIndexerPartial(t *testing.T) {
	env := newSearchTestEnv(t, nil)

	resp := env.createIndexer(t, map[string]any{
		"name":          "before",
		"kind":          "dlsearch",
		"definition_id": "patch-def",
		"priority":      10,
		"api_key":       "to-be-cleared",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", resp.Code, resp.Body.String())
	}
	var created IndexerDTO
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create body: %v", err)
	}

	patch := env.api.Patch(
		"/indexers/"+created.ID,
		map[string]any{"name": "after", "api_key": ""},
		env.authz(),
	)
	if patch.Code != http.StatusOK {
		t.Fatalf("patch status = %d: %s", patch.Code, patch.Body.String())
	}
	var patched IndexerDTO
	if err := json.Unmarshal(patch.Body.Bytes(), &patched); err != nil {
		t.Fatalf("decode patch body: %v", err)
	}
	if patched.Name != "after" {
		t.Errorf("name = %q, want %q", patched.Name, "after")
	}
	if patched.DefinitionID == nil || *patched.DefinitionID != "patch-def" {
		t.Errorf("definition_id = %v, want %q", patched.DefinitionID, "patch-def")
	}
	if patched.Priority != 10 {
		t.Errorf("priority = %d, want 10", patched.Priority)
	}
	if patched.APIKeySet {
		t.Error("api_key_set = true after clearing patch, want false")
	}

	// A settings map cannot forge the reserved keys: allow_private_network
	// and origin are owned by the API, never by caller-supplied settings.
	patch = env.api.Patch(
		"/indexers/"+created.ID,
		map[string]any{"settings": map[string]any{
			"allow_private_network": "forged",
			"origin":                "https://attacker.example",
			"custom":                "kept",
		}},
		env.authz(),
	)
	if patch.Code != http.StatusOK {
		t.Fatalf("settings patch status = %d: %s", patch.Code, patch.Body.String())
	}
	var settingsJSON string
	if err := env.db.GetContext(
		t.Context(), &settingsJSON,
		"SELECT settings_json FROM indexers WHERE id = ?", created.ID,
	); err != nil {
		t.Fatalf("read settings_json: %v", err)
	}
	if strings.Contains(settingsJSON, "forged") || strings.Contains(settingsJSON, "attacker.example") {
		t.Errorf("settings_json carries forged reserved keys: %s", settingsJSON)
	}
	if !strings.Contains(settingsJSON, `"custom":"kept"`) {
		t.Errorf("settings_json lost the caller's own key: %s", settingsJSON)
	}
}

// TestDuplicateDefinitionIDConflicts covers the unique index on
// definition_id: the second row answers 409 /problems/conflict.
func TestDuplicateDefinitionIDConflicts(t *testing.T) {
	env := newSearchTestEnv(t, nil)

	body := map[string]any{"name": "one", "kind": "dlsearch", "definition_id": "dup-def"}
	if resp := env.createIndexer(t, body); resp.Code != http.StatusCreated {
		t.Fatalf("first create status = %d: %s", resp.Code, resp.Body.String())
	}
	resp := env.createIndexer(t, map[string]any{"name": "two", "kind": "dlsearch", "definition_id": "dup-def"})
	assertProblem(t, resp, http.StatusConflict, SlugConflict)
}

// TestCategoriesMergeCapsOverDefaults covers step 7: an enabled indexer's
// cached caps override the default tree on a shared id and append
// site-specific ids, sorted ascending; a disabled indexer's caps do not
// merge.
func TestCategoriesMergeCapsOverDefaults(t *testing.T) {
	env := newSearchTestEnv(t, nil)

	resp := env.createIndexer(t, map[string]any{
		"name": "enabled", "kind": "dlsearch", "definition_id": "caps-def", "enabled": true,
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", resp.Code, resp.Body.String())
	}
	var enabled IndexerDTO
	if err := json.Unmarshal(resp.Body.Bytes(), &enabled); err != nil {
		t.Fatalf("decode create body: %v", err)
	}
	// 3000 shadows the default root's name, 2040 a default subcategory's,
	// and 100001 is site-specific.
	caps := `[{"id":3000,"name":"AudioX"},{"id":2040,"name":"SiteHD"},{"id":100001,"name":"Site Custom"}]`
	if err := env.indexers.SetCaps(t.Context(), enabled.ID, caps, false); err != nil {
		t.Fatalf("set caps: %v", err)
	}

	resp = env.createIndexer(t, map[string]any{
		"name": "disabled", "kind": "dlsearch", "definition_id": "caps-off", "enabled": false,
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create disabled status = %d: %s", resp.Code, resp.Body.String())
	}
	var disabled IndexerDTO
	if err := json.Unmarshal(resp.Body.Bytes(), &disabled); err != nil {
		t.Fatalf("decode disabled body: %v", err)
	}
	if err := env.indexers.SetCaps(t.Context(), disabled.ID, `[{"id":999999,"name":"Disabled Custom"}]`, false); err != nil {
		t.Fatalf("set disabled caps: %v", err)
	}

	resp = env.api.Get("/indexers/categories", env.authz())
	if resp.Code != http.StatusOK {
		t.Fatalf("categories status = %d: %s", resp.Code, resp.Body.String())
	}
	var out struct {
		Categories []struct {
			ID            int    `json:"id"`
			Name          string `json:"name"`
			Subcategories []struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"subcategories"`
		} `json:"categories"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode categories body: %v", err)
	}

	var audio, custom, disabledCustom int
	prev := -1
	for _, c := range out.Categories {
		if c.ID <= prev {
			t.Fatalf("roots not sorted ascending at id %d", c.ID)
		}
		prev = c.ID
		switch c.ID {
		case 3000:
			audio++
			if c.Name != "AudioX" {
				t.Errorf("id 3000 name = %q, want the caps value AudioX", c.Name)
			}
		case 2000:
			var renamed bool
			for _, s := range c.Subcategories {
				if s.ID == 2040 && s.Name == "SiteHD" {
					renamed = true
				}
			}
			if !renamed {
				t.Errorf("subcat 2040 was not renamed in place under root 2000: %+v", c.Subcategories)
			}
		case 100001:
			custom++
		case 999999:
			disabledCustom++
		}
	}
	if audio != 1 {
		t.Errorf("id 3000 appears %d times, want 1", audio)
	}
	if custom != 1 {
		t.Errorf("id 100001 appears %d times, want 1", custom)
	}
	if disabledCustom != 0 {
		t.Errorf("disabled indexer's id 999999 appears %d times, want 0", disabledCustom)
	}
	if len(out.Categories) != 8+1 {
		t.Errorf("roots = %d, want the 8 defaults plus 1 site-specific", len(out.Categories))
	}
}

// TestMergeCategoriesDoesNotMutateDefaults pins the deep copy: a caps rename
// writes into the merged tree only, never through the shared subcategory
// backing arrays into the caller's defaults.
func TestMergeCategoriesDoesNotMutateDefaults(t *testing.T) {
	defaults := search.DefaultCategories()
	rows := []store.Indexer{{
		ID:             "idx_mut",
		CategoriesJSON: ptr(`[{"id":3000,"name":"Renamed"},{"id":2040,"name":"SubRenamed"}]`),
	}}

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	merged := mergeCategories(log, defaults, rows)
	var rootRenamed, subRenamed bool
	for _, r := range merged {
		if r.ID == 3000 && r.Name == "Renamed" {
			rootRenamed = true
		}
		for _, s := range r.Subcategories {
			if s.ID == 2040 && s.Name == "SubRenamed" {
				subRenamed = true
			}
		}
	}
	if !rootRenamed || !subRenamed {
		t.Fatalf("merged tree did not apply the caps renames: root=%v sub=%v", rootRenamed, subRenamed)
	}
	for _, r := range defaults {
		if r.ID == 3000 && r.Name != "Audio" {
			t.Errorf("defaults root 3000 mutated to %q", r.Name)
		}
		for _, s := range r.Subcategories {
			if s.ID == 2040 && s.Name != "HD" {
				t.Errorf("defaults subcat 2040 mutated to %q", s.Name)
			}
		}
	}
}

// TestImportProviderCreatesDisabledRows runs the Jackett branch of the
// wizard against a stub instance and asserts every created row starts
// disabled with a non-null provenance (acceptance criterion 3).
func TestImportProviderCreatesDisabledRows(t *testing.T) {
	env := newSearchTestEnv(t, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v2.0/indexers/all/results/torznab/api"):
			if r.URL.Query().Get("t") != "indexers" || r.URL.Query().Get("configured") != "true" {
				t.Errorf("enumeration query = %q, want t=indexers&configured=true", r.URL.RawQuery)
			}
			if r.URL.Query().Get("apikey") != "jackett-key" {
				t.Errorf("apikey = %q, want jackett-key", r.URL.Query().Get("apikey"))
			}
			_, _ = w.Write([]byte(`<indexers>` +
				`<indexer id="alpha" configured="true"><title>Alpha</title></indexer>` +
				`<indexer id="beta" configured="true"><title>Beta</title></indexer>` +
				`</indexers>`))
		case strings.HasPrefix(r.URL.Path, "/api/v2.0/indexers/alpha/"),
			strings.HasPrefix(r.URL.Path, "/api/v2.0/indexers/beta/"):
			if r.URL.Query().Get("t") != "caps" {
				t.Errorf("per-indexer query = %q, want t=caps", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(capsStubXML))
		default:
			t.Errorf("unexpected stub request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	resp := env.api.Do(
		http.MethodPost, "/indexers/import",
		"Content-Type: application/json",
		strings.NewReader(`{"torznab_url":"`+srv.URL+`/api/v2.0/indexers/all/results/torznab/api","api_key":"jackett-key"}`),
		env.authz(),
	)
	if resp.Code != http.StatusCreated {
		t.Fatalf("import status = %d: %s", resp.Code, resp.Body.String())
	}

	// Every created row is disabled with a non-null provenance.
	if got := env.indexerCount(t, ""); got != 2 {
		t.Fatalf("indexer rows = %d, want 2", got)
	}
	if got := env.indexerCount(t, "WHERE enabled = 1"); got != 0 {
		t.Errorf("enabled rows = %d, want 0", got)
	}
	if got := env.indexerCount(t, "WHERE provenance IS NULL"); got != 0 {
		t.Errorf("rows with null provenance = %d, want 0", got)
	}
	if got := env.indexerCount(
		t,
		"WHERE url = ?", srv.URL+"/api/v2.0/indexers/alpha/results/torznab/api",
	); got != 1 {
		t.Errorf("rows with alpha's built base url = %d, want 1", got)
	}
	if got := env.indexerCount(
		t,
		`WHERE settings_json LIKE '%"allow_private_network":true%' AND settings_json LIKE '%"origin":%'`,
	); got != 2 {
		t.Errorf("rows carrying allow_private_network+origin = %d, want 2", got)
	}
	if got := env.indexerCount(t, "WHERE categories_json IS NULL"); got != 0 {
		t.Errorf("rows without stored caps = %d, want 0", got)
	}

	// Re-running the wizard against the same provider is idempotent: every
	// entry dedupes on its base URL, so the second import creates nothing
	// and answers 409 /problems/conflict.
	resp = env.api.Do(
		http.MethodPost, "/indexers/import",
		"Content-Type: application/json",
		strings.NewReader(`{"torznab_url":"`+srv.URL+`/api/v2.0/indexers/all/results/torznab/api","api_key":"jackett-key"}`),
		env.authz(),
	)
	assertProblem(t, resp, http.StatusConflict, SlugConflict)
	if got := env.indexerCount(t, ""); got != 2 {
		t.Errorf("indexer rows after re-import = %d, want 2", got)
	}

	// The mixed run — the provider gained indexers since last import — is
	// the realistic workflow: delete one row so beta is unknown again, then
	// re-import expects 201, one skip warning and exactly one new row.
	if _, err := env.db.ExecContext(
		t.Context(),
		"DELETE FROM indexers WHERE url = ?", srv.URL+"/api/v2.0/indexers/beta/results/torznab/api",
	); err != nil {
		t.Fatalf("delete beta row: %v", err)
	}
	resp = env.api.Do(
		http.MethodPost, "/indexers/import",
		"Content-Type: application/json",
		strings.NewReader(`{"torznab_url":"`+srv.URL+`/api/v2.0/indexers/all/results/torznab/api","api_key":"jackett-key"}`),
		env.authz(),
	)
	if resp.Code != http.StatusCreated {
		t.Fatalf("mixed re-import status = %d: %s", resp.Code, resp.Body.String())
	}
	var imported ImportOutput
	if err := json.Unmarshal(resp.Body.Bytes(), &imported.Body); err != nil {
		t.Fatalf("decode import body: %v", err)
	}
	if len(imported.Body.Warnings) != 1 || !strings.Contains(imported.Body.Warnings[0], "already imported") {
		t.Errorf("warnings = %v, want exactly one already-imported skip", imported.Body.Warnings)
	}
	if got := env.indexerCount(t, ""); got != 2 {
		t.Errorf("indexer rows after mixed re-import = %d, want 2", got)
	}
}

// TestImportProviderSkipsProwlarrIdZero runs the Prowlarr branch: the
// synthetic self-test indexer id 0 never becomes a row.
func TestImportProviderSkipsProwlarrIdZero(t *testing.T) {
	env := newSearchTestEnv(t, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/indexer":
			if r.Header.Get("X-Api-Key") != "prowlarr-key" {
				t.Errorf("X-Api-Key = %q, want prowlarr-key", r.Header.Get("X-Api-Key"))
			}
			_, _ = w.Write([]byte(`[{"id":0,"name":"Test Release"},{"id":7,"name":"Seven"}]`))
		case "/7/api":
			_, _ = w.Write([]byte(capsStubXML))
		default:
			t.Errorf("unexpected stub request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	resp := env.api.Do(
		http.MethodPost, "/indexers/import",
		"Content-Type: application/json",
		strings.NewReader(`{"torznab_url":"`+srv.URL+`","api_key":"prowlarr-key"}`),
		env.authz(),
	)
	if resp.Code != http.StatusCreated {
		t.Fatalf("import status = %d: %s", resp.Code, resp.Body.String())
	}
	if got := env.indexerCount(t, ""); got != 1 {
		t.Fatalf("indexer rows = %d, want 1 (id 0 skipped)", got)
	}
	if got := env.indexerCount(t, "WHERE url = ?", srv.URL+"/7/api"); got != 1 {
		t.Errorf("rows with id 7's base url = %d, want 1", got)
	}
	if got := env.indexerCount(t, "WHERE url = ?", srv.URL+"/0/api"); got != 0 {
		t.Errorf("rows for the synthetic indexer = %d, want 0", got)
	}
}

// TestImportProviderEmptyEnumerationReturns422 pins the post-loop guard: a
// provider that enumerates successfully but lists zero configured indexers
// is unusable input (422), not a duplicate conflict (409).
func TestImportProviderEmptyEnumerationReturns422(t *testing.T) {
	env := newSearchTestEnv(t, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v2.0/indexers/all/results/torznab/api"):
			_, _ = w.Write([]byte(`<indexers></indexers>`))
		default:
			t.Errorf("unexpected stub request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	resp := env.api.Do(
		http.MethodPost, "/indexers/import",
		"Content-Type: application/json",
		strings.NewReader(`{"torznab_url":"`+srv.URL+`/api/v2.0/indexers/all/results/torznab/api","api_key":"k"}`),
		env.authz(),
	)
	assertProblem(t, resp, http.StatusUnprocessableEntity, SlugValidationFailed)
	if got := env.indexerCount(t, ""); got != 0 {
		t.Errorf("indexer rows = %d, want 0", got)
	}
}

// TestImportRejectsUnsupportedContentType covers the content-type switch:
// only application/json runs the wizard in this task.
func TestImportRejectsUnsupportedContentType(t *testing.T) {
	env := newSearchTestEnv(t, nil)

	resp := env.api.Do(
		http.MethodPost, "/indexers/import",
		"Content-Type: text/plain",
		strings.NewReader("not json"),
		env.authz(),
	)
	assertProblem(t, resp, http.StatusUnsupportedMediaType, SlugUnsupportedMediaType)
}

// TestCreateIndexerSSRFBlocked covers the second half of step 6: a caps
// probe the guard refuses is 403 /problems/ssrf-blocked whose detail names
// allow_private_network as the remedy.
func TestCreateIndexerSSRFBlocked(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	env := newSearchTestEnv(t, secure.NewClient(secure.NewGuard(log, false)))

	resp := env.createIndexer(t, map[string]any{
		"name": "private", "kind": "torznab", "url": "http://127.0.0.1:9117/api",
	})
	problem := assertProblem(t, resp, http.StatusForbidden, SlugSSRFBlocked)
	if !strings.Contains(problem.Detail, "allow_private_network") {
		t.Errorf("detail %q does not name allow_private_network", problem.Detail)
	}
	if got := env.indexerCount(t, ""); got != 0 {
		t.Errorf("indexer rows = %d, want 0: a refused probe creates no row", got)
	}
}

// TestCreateIndexerStoresCaps covers the success path of the create-time
// probe: the fetched categories land on the row through SetCaps.
func TestCreateIndexerStoresCaps(t *testing.T) {
	env := newSearchTestEnv(t, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("t") != "caps" {
			t.Errorf("probe query = %q, want t=caps", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(capsStubXML))
	}))
	defer srv.Close()

	resp := env.createIndexer(t, map[string]any{
		"name": "probed", "kind": "torznab", "url": srv.URL + "/api",
		"api_key": "k", "allow_private_network": true,
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", resp.Code, resp.Body.String())
	}
	var created IndexerDTO
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create body: %v", err)
	}
	if len(created.Categories) != 3 {
		t.Errorf("categories = %d, want the flattened 3 (2000, 2040, 100001)", len(created.Categories))
	}
	if created.LastTestAt == nil {
		t.Error("last_test_at is null after a successful probe")
	}
}

// probeStubDef is the user definition the dlsearch probe cases load: one rss
// engine pointing at the test server, two categories, a const category so
// caps.categories need not pin a single id.
func probeStubDef(baseURL string) string {
	return `dlsearch: 1
id: probe-stub
name: Probe Stub
description: "fixture engine for the test-indexer cases"
homepage: https://x.test/
version: "1.0.0"
legal_tier: user-supplied
kind: rss
caps:
  modes: {search: [q]}
  categories: {A: 2000, B: 3000}
  seeders_unknown: true
request:
  base_url: ` + baseURL + `
  path: rss.xml
  method: GET
response:
  rows: "rss > channel > item"
  fields:
    title:    {path: "title"}
    size:     {path: "enclosure", attr: "length", type: bytes}
    download: {path: "enclosure", attr: "url"}
    category: {const: "A"}
`
}

const probeStubRSS = `<?xml version="1.0"?><rss version="2.0"><channel><item>` +
	`<title>r1</title><enclosure url="https://x.test/a.torrent" length="7"/></item>` +
	`</channel></rss>`

// newProbeRegistry builds a registry carrying the probe-stub user definition.
func newProbeRegistry(t *testing.T, baseURL string) *search.Registry {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, "probe-stub.dlsearch.yaml"),
		[]byte(probeStubDef(baseURL)), 0o600,
	); err != nil {
		t.Fatalf("write user definition: %v", err)
	}
	reg, err := search.NewRegistry(slog.New(slog.NewJSONHandler(io.Discard, nil)), dir)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	if _, ok := reg.Get("probe-stub"); !ok {
		t.Fatalf("probe-stub did not load: %v", reg.Errors())
	}
	return reg
}

// TestTestIndexerDlsearch503 covers the acceptance case: a reachable-but-
// broken upstream is 200 with ok:false and the upstream status in error; the
// row's last_test_at and last_error are stamped.
func TestTestIndexerDlsearch503(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("upstream maintenance"))
	}))
	defer srv.Close()

	env := newSearchTestEnvDeps(t, nil, newProbeRegistry(t, srv.URL), search.NewRunner(srv.Client(), nil, "dl-tool/test"))

	resp := env.createIndexer(t, map[string]any{
		"name": "broken-dlsearch", "kind": "dlsearch", "definition_id": "probe-stub",
		"allow_private_network": true,
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", resp.Code, resp.Body.String())
	}
	var created IndexerDTO
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create body: %v", err)
	}

	resp = env.api.Do(http.MethodPost, "/indexers/"+created.ID+"/test", env.authz())
	if resp.Code != http.StatusOK {
		t.Fatalf("test status = %d, want 200 with the outcome as data: %s", resp.Code, resp.Body.String())
	}
	var out TestIndexerOutput
	if err := json.Unmarshal(resp.Body.Bytes(), &out.Body); err != nil {
		t.Fatalf("decode test body: %v", err)
	}
	if out.Body.Ok {
		t.Error("ok = true for a 503 upstream, want false")
	}
	if out.Body.Error == nil || !strings.Contains(*out.Body.Error, "503") {
		t.Errorf("error = %v, want the upstream status 503 named", out.Body.Error)
	}

	row, err := env.indexers.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.LastTestAt == nil {
		t.Error("last_test_at is null after a failed probe")
	}
	if row.LastError == nil || !strings.Contains(*row.LastError, "503") {
		t.Errorf("last_error = %v, want the upstream status recorded", row.LastError)
	}
}

// TestTestIndexerDlsearchOK covers the healthy path: ok:true, the
// definition's categories counted, and the caps cached on the row.
func TestTestIndexerDlsearchOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(probeStubRSS))
	}))
	defer srv.Close()

	env := newSearchTestEnvDeps(t, nil, newProbeRegistry(t, srv.URL), search.NewRunner(srv.Client(), nil, "dl-tool/test"))

	resp := env.createIndexer(t, map[string]any{
		"name": "healthy-dlsearch", "kind": "dlsearch", "definition_id": "probe-stub",
		"allow_private_network": true,
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", resp.Code, resp.Body.String())
	}
	var created IndexerDTO
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create body: %v", err)
	}

	resp = env.api.Do(http.MethodPost, "/indexers/"+created.ID+"/test", env.authz())
	if resp.Code != http.StatusOK {
		t.Fatalf("test status = %d: %s", resp.Code, resp.Body.String())
	}
	var out TestIndexerOutput
	if err := json.Unmarshal(resp.Body.Bytes(), &out.Body); err != nil {
		t.Fatalf("decode test body: %v", err)
	}
	if !out.Body.Ok {
		t.Fatalf("ok = false for a healthy stub: error %v", out.Body.Error)
	}
	if out.Body.CategoriesFound != 2 {
		t.Errorf("categories_found = %d, want the definition's 2", out.Body.CategoriesFound)
	}
	if out.Body.Error != nil {
		t.Errorf("error = %v, want null on success", out.Body.Error)
	}

	row, err := env.indexers.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.CategoriesJSON == nil || !strings.Contains(*row.CategoriesJSON, "2000") {
		t.Errorf("categories_json = %v, want the definition's caps cached", row.CategoriesJSON)
	}
	if row.LastError != nil {
		t.Errorf("last_error = %v, want null after a clean probe", row.LastError)
	}
}

// TestTestIndexerTorznab covers the torznab branch: exactly one t=caps
// request, ok:true with the caps-derived category count.
func TestTestIndexerTorznab(t *testing.T) {
	env := newSearchTestEnv(t, nil)

	var capsCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capsCalls.Add(1)
		if r.URL.Query().Get("t") != "caps" {
			t.Errorf("probe query = %q, want t=caps", r.URL.RawQuery)
		}
		w.Header().Set("Server", "stub/1")
		_, _ = w.Write([]byte(capsStubXML))
	}))
	defer srv.Close()

	resp := env.createIndexer(t, map[string]any{
		"name": "probed-torznab", "kind": "torznab", "url": srv.URL + "/api",
		"api_key": "k", "allow_private_network": true,
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", resp.Code, resp.Body.String())
	}
	var created IndexerDTO
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create body: %v", err)
	}
	capsCalls.Store(0)

	resp = env.api.Do(http.MethodPost, "/indexers/"+created.ID+"/test", env.authz())
	if resp.Code != http.StatusOK {
		t.Fatalf("test status = %d: %s", resp.Code, resp.Body.String())
	}
	var out TestIndexerOutput
	if err := json.Unmarshal(resp.Body.Bytes(), &out.Body); err != nil {
		t.Fatalf("decode test body: %v", err)
	}
	if !out.Body.Ok {
		t.Fatalf("ok = false for a healthy torznab stub: %v", out.Body.Error)
	}
	if got := capsCalls.Load(); got != 1 {
		t.Errorf("caps requests = %d, want exactly 1", got)
	}
	if out.Body.CategoriesFound != 3 {
		t.Errorf("categories_found = %d, want 3", out.Body.CategoriesFound)
	}
	if out.Body.Server != "stub" {
		t.Errorf("server = %q, want stub from the caps title", out.Body.Server)
	}
}

// TestTestIndexerNotFoundAndUnattempted covers the non-200 paths: an unknown
// id is 404, and a dlsearch row whose definition is not loaded — or a server
// built without a runner — is 503 /problems/engine-unavailable because the
// probe could not be attempted at all.
func TestTestIndexerNotFoundAndUnattempted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(probeStubRSS))
	}))
	defer srv.Close()

	env := newSearchTestEnvDeps(t, nil, newProbeRegistry(t, srv.URL), search.NewRunner(srv.Client(), nil, "dl-tool/test"))

	resp := env.api.Do(http.MethodPost, "/indexers/idx_missing/test", env.authz())
	assertProblem(t, resp, http.StatusNotFound, SlugNotFound)

	// A definition_id the registry never loaded means the probe cannot be
	// attempted: 503, not 200-with-ok:false.
	resp = env.createIndexer(t, map[string]any{
		"name": "unknown-def", "kind": "dlsearch", "definition_id": "not-loaded",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", resp.Code, resp.Body.String())
	}
	var created IndexerDTO
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create body: %v", err)
	}
	resp = env.api.Do(http.MethodPost, "/indexers/"+created.ID+"/test", env.authz())
	assertProblem(t, resp, http.StatusServiceUnavailable, SlugEngineUnavailable)

	// A server built without defs or runner answers the same 503 — the
	// probe cannot be attempted because no evaluator exists.
	bare := newSearchTestEnvDeps(t, nil, nil, nil)
	resp = bare.createIndexer(t, map[string]any{
		"name": "no-runner", "kind": "dlsearch", "definition_id": "any",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", resp.Code, resp.Body.String())
	}
	var bareCreated IndexerDTO
	if err := json.Unmarshal(resp.Body.Bytes(), &bareCreated); err != nil {
		t.Fatalf("decode create body: %v", err)
	}
	resp = bare.api.Do(http.MethodPost, "/indexers/"+bareCreated.ID+"/test", bare.authz())
	assertProblem(t, resp, http.StatusServiceUnavailable, SlugEngineUnavailable)
}

// TestIndexerSettingsMap covers the settings_json decode used by the probe:
// numbers keep their exact text, nulls drop, and a document with trailing
// garbage is invalid and yields empty settings.
func TestIndexerSettingsMap(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	j := `{"n": 1000000, "f": 0.00001, "flag": true, "s": "x", "z": null}`
	out := indexerSettingsMap(log, store.Indexer{ID: "i1", SettingsJSON: &j})
	if out["n"] != "1000000" {
		t.Errorf("n = %q, want verbatim 1000000", out["n"])
	}
	if out["f"] != "0.00001" {
		t.Errorf("f = %q, want verbatim 0.00001", out["f"])
	}
	if out["flag"] != "true" || out["s"] != "x" {
		t.Errorf("flag/s = %q/%q, want true/x", out["flag"], out["s"])
	}
	if _, ok := out["z"]; ok {
		t.Errorf("null key z should be dropped, got %q", out["z"])
	}

	bad := `{"a": "b"} trailing`
	if out := indexerSettingsMap(log, store.Indexer{ID: "i2", SettingsJSON: &bad}); len(out) != 0 {
		t.Errorf("trailing garbage should yield empty settings, got %v", out)
	}
}

// ---------------------------------------------------------------------
// T061: the asynchronous search lifecycle — POST, GET and DELETE /search.
// ---------------------------------------------------------------------

// torznabFeedXML renders one RSS item per title, each carrying a .torrent
// enclosure so Finalise keeps the row — a result with no acquisition handle
// is dropped before it can persist.
func torznabFeedXML(titles ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>`)
	for i, title := range titles {
		fmt.Fprintf(&b,
			`<item><title>%s</title><enclosure url="https://x.test/f%d.torrent" length="10%d"/>`+
				`<torznab:attr name="seeders" value="%d"/></item>`,
			title, i, i, i+1,
		)
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

// seedSearchIndexer inserts an enabled torznab row straight through the
// store: the API's create path probes t=caps first, which a latency stub
// would delay, and the search lifecycle needs the row, not the wizard.
// allow_private_network lets the fan-out's per-origin guard reach the
// loopback stub.
func (e *searchTestEnv) seedSearchIndexer(t *testing.T, name, baseURL string) store.Indexer {
	t.Helper()
	settings := `{"allow_private_network":true,"origin":"` + baseURL + `"}`
	row, err := e.indexers.Create(t.Context(), store.Indexer{
		Name:         name,
		Kind:         "torznab",
		Enabled:      true,
		URL:          &baseURL,
		SettingsJSON: &settings,
		Priority:     store.DefaultIndexerPriority,
	}, secure.Secret("k"))
	if err != nil {
		t.Fatalf("seed indexer %q: %v", name, err)
	}
	return row
}

// claimSearchJob claims the single queued row the way the worker's
// claimLoop does. The tests then run the real handler over it directly —
// the worker's 1 s poll would make the partial-result window
// timing-dependent instead of deterministic.
func (e *searchTestEnv) claimSearchJob(t *testing.T) store.Job {
	t.Helper()
	job, err := store.ClaimJob(t.Context(), e.db, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("claim search job: %v", err)
	}
	if job.Kind != jobs.JobKindSearch {
		t.Fatalf("claimed kind = %q, want %q", job.Kind, jobs.JobKindSearch)
	}
	return job
}

// runSearchJob executes the registered handler over one claimed row and
// returns its error so a goroutine can report it.
func (e *searchTestEnv) runSearchJob(job store.Job) error {
	h := jobs.NewSearchHandler(
		e.db,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		nil, nil,
		e.indexers,
		http.DefaultClient,
	)
	return h(context.Background(), job)
}

// getSearch polls GET /search/{id} once and decodes the body.
func (e *searchTestEnv) getSearch(t *testing.T, id string) SearchJobOutput {
	t.Helper()
	resp := e.api.Get("/search/"+id, e.authz())
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /search/%s status = %d: %s", id, resp.Code, resp.Body.String())
	}
	var out SearchJobOutput
	if err := json.Unmarshal(resp.Body.Bytes(), &out.Body); err != nil {
		t.Fatalf("decode search body: %v", err)
	}
	return out
}

// resultCount counts one job's persisted rows.
func (e *searchTestEnv) resultCount(t *testing.T, jobID string) int {
	t.Helper()
	var n int
	if err := e.db.GetContext(t.Context(), &n,
		"SELECT COUNT(*) FROM search_results WHERE search_job_id = ?", jobID); err != nil {
		t.Fatalf("count search_results: %v", err)
	}
	return n
}

// TestStartSearchReturns202AndID pins step 5: POST /search writes the job
// row and its queue entry and answers immediately — a stub that never
// answers proves no indexer is contacted on the request.
func TestStartSearchReturns202AndID(t *testing.T) {
	env := newSearchTestEnv(t, nil)

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = w.Write([]byte(torznabFeedXML("late")))
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	env.seedSearchIndexer(t, "blocking", srv.URL+"/api")

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- env.api.Post("/search", map[string]any{"query": "late"}, env.authz())
	}()
	var resp *httptest.ResponseRecorder
	select {
	case resp = <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("POST /search did not answer within 5 s — it must never wait on an indexer")
	}

	if resp.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202: %s", resp.Code, resp.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 202 body: %v", err)
	}
	if len(body) != 1 {
		keys := make([]string, 0, len(body))
		for k := range body {
			keys = append(keys, k)
		}
		t.Errorf("202 body keys = %v, want exactly [id]", keys)
	}
	id, _ := body["id"].(string)
	if !strings.HasPrefix(id, "sch_") {
		t.Fatalf("id = %q, want a sch_… job id", id)
	}

	var jobRows int
	if err := env.db.GetContext(t.Context(), &jobRows,
		"SELECT COUNT(*) FROM search_jobs WHERE id = ?", id); err != nil {
		t.Fatalf("count search_jobs: %v", err)
	}
	if jobRows != 1 {
		t.Errorf("search_jobs rows for %s = %d, want 1", id, jobRows)
	}

	var kind string
	var maxAttempts int
	if err := env.db.QueryRowContext(t.Context(),
		"SELECT kind, max_attempts FROM jobs WHERE payload_json LIKE ?", "%"+id+"%",
	).Scan(&kind, &maxAttempts); err != nil {
		t.Fatalf("read queue row: %v", err)
	}
	if kind != jobs.JobKindSearch {
		t.Errorf("jobs.kind = %q, want %q", kind, jobs.JobKindSearch)
	}
	if maxAttempts != 1 {
		t.Errorf("jobs.max_attempts = %d, want 1 — a search is re-run by the user, not retried", maxAttempts)
	}
}

// TestPollShowsPartialThenFinished is the task's acceptance case: two
// engines 50 ms and 400 ms apart, so a poll between them must report
// finished:false with the fast engine's rows already visible, and a later
// poll finished:true with both.
func TestPollShowsPartialThenFinished(t *testing.T) {
	env := newSearchTestEnv(t, nil)

	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(torznabFeedXML("fast-release")))
	}))
	defer fast.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte(torznabFeedXML("slow-release")))
	}))
	defer slow.Close()

	env.seedSearchIndexer(t, "fast", fast.URL+"/api")
	env.seedSearchIndexer(t, "slow", slow.URL+"/api")

	resp := env.api.Post("/search", map[string]any{"query": "release"}, env.authz())
	if resp.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202: %s", resp.Code, resp.Body.String())
	}
	var started StartedOutput
	if err := json.Unmarshal(resp.Body.Bytes(), &started.Body); err != nil {
		t.Fatalf("decode 202 body: %v", err)
	}
	id := started.Body.ID

	job := env.claimSearchJob(t)
	done := make(chan error, 1)
	go func() {
		done <- env.runSearchJob(job)
	}()

	var sawPartial bool
	var last SearchJobOutput
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		last = env.getSearch(t, id)
		if !last.Body.Finished && len(last.Body.Results) > 0 {
			sawPartial = true
		}
		if last.Body.Finished {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatalf("search handler: %v", err)
	}
	if !sawPartial {
		t.Error("no poll observed partial results with finished:false — engines did not write rows as they answered")
	}
	if !last.Body.Finished {
		t.Fatal("search never reported finished:true within 5 s")
	}
	if last.Body.Total != 2 || len(last.Body.Results) != 2 {
		t.Errorf("finished job total/results = %d/%d, want 2/2", last.Body.Total, len(last.Body.Results))
	}
	for _, eng := range last.Body.Engines {
		if eng.Status != store.EngineDone || eng.Count != 1 {
			t.Errorf("engine %s status/count = %s/%d, want done/1", eng.ID, eng.Status, eng.Count)
		}
	}

	// A re-delivery of the same queue row (at-least-once) replaces each
	// engine's page instead of doubling it.
	if err := env.runSearchJob(job); err != nil {
		t.Fatalf("second handler run: %v", err)
	}
	if n := env.resultCount(t, id); n != 2 {
		t.Errorf("results after re-run = %d, want 2 — a repeated execution must not duplicate rows", n)
	}
}

// TestDeleteRemovesJobAndResults covers the cascade: DELETE removes the
// search_jobs row, its results go with it, and a later GET is 404.
func TestDeleteRemovesJobAndResults(t *testing.T) {
	env := newSearchTestEnv(t, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(torznabFeedXML("gone")))
	}))
	defer srv.Close()
	env.seedSearchIndexer(t, "ephemeral", srv.URL+"/api")

	resp := env.api.Post("/search", map[string]any{"query": "gone"}, env.authz())
	if resp.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d: %s", resp.Code, resp.Body.String())
	}
	var started StartedOutput
	if err := json.Unmarshal(resp.Body.Bytes(), &started.Body); err != nil {
		t.Fatalf("decode 202 body: %v", err)
	}
	id := started.Body.ID

	if err := env.runSearchJob(env.claimSearchJob(t)); err != nil {
		t.Fatalf("search handler: %v", err)
	}
	if n := env.resultCount(t, id); n != 1 {
		t.Fatalf("results before delete = %d, want 1", n)
	}

	del := env.api.Do(http.MethodDelete, "/search/"+id, env.authz())
	if del.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204: %s", del.Code, del.Body.String())
	}
	if n := env.resultCount(t, id); n != 0 {
		t.Errorf("results after delete = %d, want 0 — the cascade must remove them", n)
	}
	var jobRows int
	if err := env.db.GetContext(t.Context(), &jobRows,
		"SELECT COUNT(*) FROM search_jobs WHERE id = ?", id); err != nil {
		t.Fatalf("count search_jobs: %v", err)
	}
	if jobRows != 0 {
		t.Errorf("search_jobs rows after delete = %d, want 0", jobRows)
	}
	assertProblem(t, env.api.Get("/search/"+id, env.authz()), http.StatusNotFound, SlugNotFound)
}

// TestUnknownJobIs404 pins the 404 /problems/not-found answer for a job id
// that never existed — GET and DELETE agree.
func TestUnknownJobIs404(t *testing.T) {
	env := newSearchTestEnv(t, nil)

	assertProblem(t, env.api.Get("/search/sch_missing", env.authz()), http.StatusNotFound, SlugNotFound)
	assertProblem(t,
		env.api.Do(http.MethodDelete, "/search/sch_missing", env.authz()),
		http.StatusNotFound, SlugNotFound)
}

// TestEmptyQueryIs422 covers the input validation: an empty or missing
// query is /problems/validation-failed, an unknown indexer id the same —
// and neither writes a search_jobs row.
func TestEmptyQueryIs422(t *testing.T) {
	env := newSearchTestEnv(t, nil)
	env.seedSearchIndexer(t, "idle", "http://127.0.0.1:1/api")

	assertProblem(t,
		env.api.Post("/search", map[string]any{"query": ""}, env.authz()),
		http.StatusUnprocessableEntity, SlugValidationFailed)
	assertProblem(t,
		env.api.Post("/search", map[string]any{}, env.authz()),
		http.StatusUnprocessableEntity, SlugValidationFailed)
	assertProblem(t,
		env.api.Post("/search",
			map[string]any{"query": "x", "indexer_ids": []string{"idx_missing"}},
			env.authz()),
		http.StatusUnprocessableEntity, SlugValidationFailed)

	var jobRows int
	if err := env.db.GetContext(t.Context(), &jobRows, "SELECT COUNT(*) FROM search_jobs"); err != nil {
		t.Fatalf("count search_jobs: %v", err)
	}
	if jobRows != 0 {
		t.Errorf("search_jobs rows = %d, want 0 after refused posts", jobRows)
	}
}

// TestNoEnabledIndexerIs503 covers the empty-selection answer: no indexers
// at all, and then one disabled indexer — the default selection is every
// enabled indexer, so both answer 503 /problems/engine-unavailable.
func TestNoEnabledIndexerIs503(t *testing.T) {
	env := newSearchTestEnv(t, nil)

	assertProblem(t,
		env.api.Post("/search", map[string]any{"query": "x"}, env.authz()),
		http.StatusServiceUnavailable, SlugEngineUnavailable)

	url := "http://127.0.0.1:1/api"
	if _, err := env.indexers.Create(t.Context(), store.Indexer{
		Name:     "off",
		Kind:     "torznab",
		Enabled:  false,
		URL:      &url,
		Priority: store.DefaultIndexerPriority,
	}, secure.Secret("k")); err != nil {
		t.Fatalf("seed disabled indexer: %v", err)
	}
	assertProblem(t,
		env.api.Post("/search", map[string]any{"query": "x"}, env.authz()),
		http.StatusServiceUnavailable, SlugEngineUnavailable)
}
