# T055 — Store indexers and import a Prowlarr or Jackett instance

| Field | Value |
|---|---|
| **ID** | T055 |
| **Milestone** | M4 |
| **Status** | todo |
| **Depends on** | T006, T007, T008, T054 |
| **Blocks** | T057, T058, T059, T061 |
| **Parallel-safe** | no — creates `internal/api/search.go` and registers routes in `internal/api/server.go` |
| **Implements** | — (storage and CRUD behind [FR-050](../02-requirements.md#fr-050-query-torznab-and-newznab-indexers) and [FR-056](../02-requirements.md#fr-056-test-an-indexer-on-demand), covered by T054 and T058) |
| **Decisions** | [ADR-0008](../decisions/0008-torznab-first-declarative-yaml-second.md), [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md) |
| **Est. size** | 3 new files, ~520 LOC |

## Goal
`indexers` rows are created, listed, patched and deleted over `/api/v1/indexers`, the API key is sealed at
rest and never returned, `GET /indexers/categories` serves the newznab tree, and
`POST /indexers/import` with `{torznab_url, api_key}` enumerates a Jackett or Prowlarr instance into one
disabled `idx_…` row per upstream indexer.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §9.1 Indexer CRUD](../05-api-contract.md#91-indexer-crud) — the JSON object,
   the accepted fields, the status codes and the import response.
2. [`docs/04-data-model.md` §3.4 Search](../04-data-model.md#34-search) — the `indexers` DDL and its unique
   index, and [§4.5 `indexers.kind`](../04-data-model.md#45-indexerskind).
3. [`docs/07-search-and-indexers.md` §2.6 Provider URL shapes](../07-search-and-indexers.md#26-provider-url-shapes)
   and [§2.7 Adding a Prowlarr or Jackett instance as a provider](../07-search-and-indexers.md#27-adding-a-prowlarr-or-jackett-instance-as-a-provider).
4. [`docs/07-search-and-indexers.md` §2.3 Standard newznab category IDs](../07-search-and-indexers.md#23-standard-newznab-category-ids)
   — the nine roots and their subcategories, reproduced as data.
5. [`docs/14-conventions.md` §2.4 SQL and sqlx](../14-conventions.md#24-sql-and-sqlx) — explicit column
   lists, `?` placeholders, one transaction per multi-statement write.
6. [`docs/tasks/T054-torznab-client-and-ssrf-guard.md`](T054-torznab-client-and-ssrf-guard.md) — `Caps`,
   `Category`, `NewTorznabClient` and `secure.NewClient`.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/store/indexers.go` | create | Row struct, CRUD, and the sealing of `api_key_enc`. |
| `internal/search/torznab.go` | modify | Add `EnumerateProvider` and `DefaultCategories`. |
| `internal/api/search.go` | create | `/indexers` CRUD, `/indexers/categories`, the JSON branch of `/indexers/import`. |
| `internal/api/search_test.go` | create | `humatest` cases for CRUD, secret redaction and enumeration. |
| `internal/api/server.go` | modify | Call `RegisterSearchRoutes` once, from `NewServer`. |

No other file may be modified.

## Interface contract

```go
package store

// Indexer mirrors the indexers DDL of 04-data-model.md section 3.4. APIKeyEnc never
// leaves this package in clear; readers call OpenAPIKey.
type Indexer struct {
	ID               string  `db:"id"                json:"id"`
	Name             string  `db:"name"              json:"name"`
	Kind             string  `db:"kind"              json:"kind"` // torznab | newznab | dlsearch
	Enabled          bool    `db:"enabled"           json:"enabled"`
	URL              *string `db:"url"               json:"url"`
	APIKeyEnc        *string `db:"api_key_enc"       json:"-"`
	DefinitionID     *string `db:"definition_id"     json:"definition_id"`
	DefinitionSource *string `db:"definition_source" json:"definition_source"` // bundled | user | imported
	Provenance       *string `db:"provenance"        json:"provenance"`
	LegalTier        string  `db:"legal_tier"        json:"legal_tier"`
	Priority         int     `db:"priority"          json:"priority"`
	SeedersUnknown   bool    `db:"seeders_unknown"   json:"seeders_unknown"`
	SettingsJSON     *string `db:"settings_json"     json:"-"`
	CategoriesJSON   *string `db:"categories_json"   json:"-"`
	LastTestAt       *int64  `db:"last_test_at"      json:"-"`
	LastError        *string `db:"last_error"        json:"last_error"`
	CreatedAt        int64   `db:"created_at"        json:"-"`
	UpdatedAt        int64   `db:"updated_at"        json:"-"`
}

// IndexerStore seals api_key_enc with AES-256-GCM under a 32-byte key derived by
// HKDF-SHA256 from cfg.SessionKey with the info string "dl-tool/indexer-api-key/v1".
// No new environment variable and no new secret carrier is introduced.
type IndexerStore struct{ /* db, aead */ }

func NewIndexerStore(db *sqlx.DB, sessionKey secure.Secret) (*IndexerStore, error)

func (s *IndexerStore) Create(ctx context.Context, in Indexer, apiKey secure.Secret) (Indexer, error)
func (s *IndexerStore) Get(ctx context.Context, id string) (Indexer, error)             // ErrNotFound
func (s *IndexerStore) List(ctx context.Context, enabledOnly bool) ([]Indexer, error)   // ORDER BY priority, name
func (s *IndexerStore) Update(ctx context.Context, id string, patch IndexerPatch) (Indexer, error)
func (s *IndexerStore) Delete(ctx context.Context, id string) error
func (s *IndexerStore) SetCaps(ctx context.Context, id string, categoriesJSON string, seedersUnknown bool) error
func (s *IndexerStore) RecordTest(ctx context.Context, id string, at int64, lastErr *string) error
func (s *IndexerStore) OpenAPIKey(row Indexer) (secure.Secret, error)

// IndexerPatch carries only the fields 05-api-contract.md section 9.1 accepts; a nil
// field is unchanged. APIKey set to a pointer to "" clears the stored key.
type IndexerPatch struct {
	Name, URL, DefinitionID *string
	Kind, Provenance        *string
	Enabled                 *bool
	Priority                *int
	APIKey                  *secure.Secret
	SettingsJSON            *string
}
```

```go
package search

// DefaultCategories is the newznab tree of doc 07 section 2.3, roots 1000..8000 with
// their documented subcategories. It is the fallback for GET /indexers/categories when
// no indexer has cached caps.
func DefaultCategories() []Category

// ProviderIndexer is one upstream indexer discovered on a Jackett or Prowlarr instance.
type ProviderIndexer struct {
	RemoteID string // Jackett indexer id, or the Prowlarr integer id as text
	Name     string
	BaseURL  string // fully built per-indexer Torznab base URL
}

// EnumerateProvider detects the provider from host and path, issues the enumeration
// request of doc 07 section 2.7 and returns one entry per configured indexer. Prowlarr
// id 0 is skipped: it is a synthetic self-test indexer.
func EnumerateProvider(ctx context.Context, hc *http.Client, host string, apiKey secure.Secret) ([]ProviderIndexer, error)
```

```go
package api

type IndexerDTO struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	Kind             string           `json:"kind"`
	Enabled          bool             `json:"enabled"`
	URL              *string          `json:"url"`
	APIKeySet        bool             `json:"api_key_set"`
	DefinitionID     *string          `json:"definition_id"`
	DefinitionSource *string          `json:"definition_source"`
	Provenance       *string          `json:"provenance"`
	LegalTier        string           `json:"legal_tier"`
	Priority         int              `json:"priority"`
	SeedersUnknown   bool             `json:"seeders_unknown"`
	Categories       []search.Category `json:"categories"`
	LastTestAt       *string          `json:"last_test_at"` // RFC 3339 UTC
	LastError        *string          `json:"last_error"`
}

func (h *SearchHandlers) ListIndexers(ctx context.Context, in *struct{}) (*ListIndexersOutput, error)
func (h *SearchHandlers) CreateIndexer(ctx context.Context, in *CreateIndexerInput) (*IndexerOutput, error)
func (h *SearchHandlers) PatchIndexer(ctx context.Context, in *PatchIndexerInput) (*IndexerOutput, error)
func (h *SearchHandlers) DeleteIndexer(ctx context.Context, in *IndexerIDInput) (*struct{}, error)
func (h *SearchHandlers) IndexerCategories(ctx context.Context, in *struct{}) (*CategoriesOutput, error)
// ImportIndexer carries both accepted bodies of POST /indexers/import and switches on
// Content-Type: application/json is the provider wizard implemented here;
// multipart/form-data is a file upload, implemented by T059 and T060.
type ImportIndexerInput struct {
	ContentType string `header:"Content-Type"`
	RawBody     []byte
}

type ImportOutput struct {
	Body struct {
		Indexer  IndexerDTO `json:"indexer"`
		Warnings []string   `json:"warnings"`
	}
}

func (h *SearchHandlers) ImportIndexer(ctx context.Context, in *ImportIndexerInput) (*ImportOutput, error)
```

```go
// RegisterSearchRoutes is the single registration point for every /indexers and /search
// operation. NewServer calls it once; later M4 tasks add their operations inside it and
// never touch server.go again.
func RegisterSearchRoutes(api huma.API, h *SearchHandlers)
```

Operation ids registered here: `list-indexers`, `create-indexer`, `patch-indexer`, `delete-indexer`,
`indexer-categories`, `import-indexer`. Writes are admin-only.

## Steps
1. Create `internal/store/indexers.go` with `Indexer`, `IndexerPatch` and `NewIndexerStore`, deriving the
   AEAD key with `golang.org/x/crypto/hkdf` over `cfg.SessionKey` and the info string above.
2. Write `Create` with `store.NewID(store.PrefixIndexer)`, explicit column lists and `?` placeholders; map a
   `definition_id` uniqueness violation to a sentinel the handler turns into `409 /problems/conflict`.
3. Implement `Get`, `List`, `Update`, `Delete`, `SetCaps`, `RecordTest` and `OpenAPIKey`; `Update` builds its
   `SET` clause only from non-nil patch fields and always stamps `updated_at`.
4. Add `DefaultCategories` and `EnumerateProvider` to `internal/search/torznab.go`. Jackett is detected by the
   `/api/v2.0/indexers/` path segment and enumerated with `t=indexers&configured=true`; Prowlarr by
   `GET {host}/api/v1/indexer` carrying `X-Api-Key`.
5. Create `internal/api/search.go` with `SearchHandlers`, the six handlers above and `toIndexerDTO`, which
   sets `APIKeySet` from a non-nil `api_key_enc` and never reads the key.
6. `CreateIndexer` rejects `kind: torznab` or `newznab` without `url` with `422 /problems/validation-failed`,
   and returns `403 /problems/ssrf-blocked` when the caps probe fails the guard.
7. `IndexerCategories` merges every enabled indexer's cached `categories_json` over `DefaultCategories()`,
   de-duplicating on category id and sorting ascending.
8. The `application/json` branch of `ImportIndexer` calls `EnumerateProvider`, then for each entry fetches `t=caps`, stores the flattened tree
   with `SetCaps` and creates the row with `enabled = false`, `legal_tier = 'user-supplied'`,
   `definition_source = 'imported'` and `provenance = 'imported:torznab-provider'`, with the originating
   host recorded in `settings_json.origin`. The settings document also carries
   `allow_private_network: true`, because a provider on the Compose network is private.
9. Add `RegisterSearchRoutes` to `internal/api/search.go` with the six operations under `/indexers`, and
   call it once from `NewServer` in `internal/api/server.go`.
10. Create `internal/api/search_test.go` with `humatest`: `TestCreateIndexerRequiresURL`,
    `TestAPIKeyNeverReturned`, `TestPatchIndexerPartial`, `TestDuplicateDefinitionIDConflicts`,
    `TestCategoriesMergeCapsOverDefaults`, `TestImportProviderCreatesDisabledRows`,
    `TestImportProviderSkipsProwlarrIdZero` and `TestImportRejectsUnsupportedContentType`, each against a stub upstream `httptest` server.
11. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `GET /indexers` returns `api_key_set` and never a key, a ciphertext or a `[REDACTED]` placeholder in place of one.
- [ ] `SELECT api_key_enc FROM indexers` holds no substring of the submitted key in any test row.
- [ ] `TestImportProviderCreatesDisabledRows` asserts every created row has `enabled = 0` and a non-null `provenance`.
- [ ] `TestImportProviderSkipsProwlarrIdZero` asserts no row is created for the synthetic indexer.
- [ ] A second indexer with the same `definition_id` returns `409` with `type` `/problems/conflict`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/api/... ./internal/store/... ./internal/search/..." && echo INDEXERS_OK
```
Expected: three `ok  	github.com/L-K-M/dl-tool/internal/...` lines, every test named in step 10 listed as
passing, and the final line of stdout exactly `INDEXERS_OK`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT add a column to `indexers`; the per-indexer private-network flag lives in `settings_json`.
- Do NOT implement `POST /indexers/{id}/test`; T058 owns it.
- Do NOT accept a multipart upload here; T059 owns `.dlm` and `.dlsearch.yaml` uploads and T060 owns `.py`.
- Do NOT seed the bundled engines; T057 owns seeding.
- Do NOT call an indexer during `GET /indexers`; the list is served from the database only.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
