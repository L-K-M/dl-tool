package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/config"
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
		Deps{Indexers: indexers, HTTP: hc},
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
