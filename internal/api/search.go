package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"
	"go.yaml.in/yaml/v3"

	"github.com/L-K-M/dl-tool/internal/jobs"
	"github.com/L-K-M/dl-tool/internal/search"
	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

const (
	operationListIndexers      = "list-indexers"
	operationCreateIndexer     = "create-indexer"
	operationPatchIndexer      = "patch-indexer"
	operationDeleteIndexer     = "delete-indexer"
	operationIndexerCategories = "indexer-categories"
	operationImportIndexer     = "import-indexer"
	operationTestIndexer       = "test-indexer"
	operationStartSearch       = "start-search"
	operationGetSearch         = "get-search"
	operationDeleteSearch      = "delete-search"
)

const (
	indexerKindTorznab  = "torznab"
	indexerKindNewznab  = "newznab"
	indexerKindDlsearch = "dlsearch"

	// indexerSettingAllowPrivate is the settings_json key of the
	// per-indexer private-network flag of docs/05-api-contract.md section
	// 9.1 — the flag lives in the settings document, not a column.
	indexerSettingAllowPrivate = "allow_private_network"
	// indexerSettingOrigin records the provider origin (scheme://host:port)
	// on rows the import wizard discovers, so a later task can rebuild the
	// ForOrigin guard exemption for that indexer alone. On file imports it
	// carries the uploaded file name (doc 07 section 7 provenance).
	indexerSettingOrigin = "origin"
	// indexerSettingModuleSource is the inert original module blob —
	// base64 so the bytes survive verbatim inside the JSON document. The
	// "view source" pane renders it read-only; it is never executed.
	indexerSettingModuleSource = "module_source_b64"
	// indexerSettingConverted marks whether the import auto-converted the
	// upload into a dlsearch/v1 draft (doc 07 section 7).
	indexerSettingConverted = "auto_converted"
	// indexerSettingDefinition keeps the converted dlsearch/v1 draft on
	// the row, alongside the inert original — an imported definition has
	// no file under /config/engines and nothing may be written to disk.
	indexerSettingDefinition = "definition_yaml"
	// indexerSettingPluginVersion is the #VERSION: header a .py import
	// extracted; the row has no version column, so provenance lives in
	// the settings document.
	indexerSettingPluginVersion = "plugin_version"
	// indexerSettingPluginCategories keeps a .py plugin's
	// supported_categories verbatim — the site values, alongside the
	// newznab ids on categories_json.
	indexerSettingPluginCategories = "plugin_categories"

	indexerImportedSource     = "imported"
	indexerImportedProvenance = "imported:torznab-provider"
	indexerTierUserSupplied   = "user-supplied"

	// maxImportFileBytes is the 1 MiB cap on the one "file" part of the
	// multipart import form (task T059): a .dlm upload is already limited
	// to 1 MiB compressed and a .dlsearch.yaml to 512 KiB.
	maxImportFileBytes = 1 << 20
)

// indexerInternalSettingKeys are the settings_json keys the API owns: a
// caller's settings map can never write them, and a settings replacement
// preserves them.
var indexerInternalSettingKeys = []string{
	indexerSettingAllowPrivate,
	indexerSettingOrigin,
	indexerSettingModuleSource,
	indexerSettingConverted,
	indexerSettingDefinition,
	indexerSettingPluginVersion,
	indexerSettingPluginCategories,
}

// Deps carries the process-wide search collaborators, built exactly once in
// cmd/dl-tool/main.go and passed into NewServer, so the API and the job
// worker share one of each (docs/14-conventions.md section 8.3). Runner is
// the dlsearch evaluator — its per-engine rate buckets are process state, so
// the probe path and the search-job path must share the one instance. DB is
// the queue the /search lifecycle enqueues into; it is the same handle
// NewServer receives, re-delivered here because the registration call sites
// are fixed (T061).
type Deps struct {
	Indexers *store.IndexerStore
	Defs     *search.Registry
	Runner   *search.Runner
	HTTP     *http.Client // the SSRF-guarded client of T123
	DB       *sqlx.DB
}

// IndexerDTO is the wire shape of docs/05-api-contract.md section 9.1.
// APIKeySet reports whether a key is stored; the key itself is never
// returned — no field carries it, its ciphertext, or a placeholder.
type IndexerDTO struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Kind             string            `json:"kind" enum:"torznab,newznab,dlsearch"`
	Enabled          bool              `json:"enabled"`
	URL              *string           `json:"url"`
	APIKeySet        bool              `json:"api_key_set"`
	DefinitionID     *string           `json:"definition_id"`
	DefinitionSource *string           `json:"definition_source"`
	Provenance       *string           `json:"provenance"`
	LegalTier        string            `json:"legal_tier"`
	Priority         int               `json:"priority"`
	SeedersUnknown   bool              `json:"seeders_unknown"`
	Categories       []search.Category `json:"categories"`
	LastTestAt       *string           `json:"last_test_at"` // RFC 3339 UTC
	LastError        *string           `json:"last_error"`
}

// CreateIndexerInput is the JSON body of POST /indexers.
type CreateIndexerInput struct {
	Body struct {
		Name                string            `json:"name"                  required:"true" minLength:"1" doc:"Display name"`
		Kind                string            `json:"kind"                  required:"true" enum:"torznab,newznab,dlsearch" doc:"torznab and newznab need url; dlsearch needs definition_id"`
		Enabled             *bool             `json:"enabled,omitempty"`
		URL                 *string           `json:"url,omitempty"         format:"uri" doc:"Torznab/Newznab base URL"`
		APIKey              *secure.Secret    `json:"api_key,omitempty"     format:"password" doc:"Stored sealed; never returned"`
		DefinitionID        *string           `json:"definition_id,omitempty"`
		Priority            *int              `json:"priority,omitempty"`
		AllowPrivateNetwork *bool             `json:"allow_private_network,omitempty" doc:"Lift the SSRF private-range denial for this indexer's origin"`
		Settings            map[string]string `json:"settings,omitempty"    doc:"Per-engine setting values"`
	}
}

// PatchIndexerInput addresses one indexer; every body field is a pointer so
// an omitted field is untouched and api_key: "" clears the stored key.
type PatchIndexerInput struct {
	ID   string `path:"id" doc:"The idx_… row id"`
	Body struct {
		Name                *string           `json:"name,omitempty"`
		Kind                *string           `json:"kind,omitempty"             enum:"torznab,newznab,dlsearch"`
		Enabled             *bool             `json:"enabled,omitempty"`
		URL                 *string           `json:"url,omitempty"              format:"uri"`
		APIKey              *secure.Secret    `json:"api_key,omitempty"          format:"password"`
		DefinitionID        *string           `json:"definition_id,omitempty"`
		Priority            *int              `json:"priority,omitempty"`
		AllowPrivateNetwork *bool             `json:"allow_private_network,omitempty"`
		Settings            map[string]string `json:"settings,omitempty"`
	}
}

// IndexerIDInput addresses one indexer for DELETE.
type IndexerIDInput struct {
	ID string `path:"id" doc:"The idx_… row id"`
}

// ImportIndexerInput carries both accepted bodies of POST /indexers/import
// and switches on Content-Type: application/json is the provider wizard;
// multipart/form-data is a file upload — .dlm, .dlsearch.yaml and .py.
type ImportIndexerInput struct {
	ContentType string `header:"Content-Type"`
	RawBody     []byte
}

// ListIndexersOutput is the GET /indexers body.
type ListIndexersOutput struct {
	Body struct {
		Indexers []IndexerDTO `json:"indexers"`
	}
}

// IndexerOutput carries 201 from CreateIndexer and 200 from PatchIndexer.
type IndexerOutput struct {
	Status int `json:"-"`
	Body   IndexerDTO
}

// CategoriesOutput is the GET /indexers/categories body: the newznab tree
// merged with every enabled indexer's cached caps.
type CategoriesOutput struct {
	Body struct {
		Categories []search.Category `json:"categories"`
	}
}

// ImportOutput is the POST /indexers/import body: the first created indexer
// plus the per-indexer warnings the wizard collected.
type ImportOutput struct {
	Body struct {
		Indexer  IndexerDTO `json:"indexer"`
		Warnings []string   `json:"warnings"`
	}
}

// TestIndexerOutput is 200 whether or not the probe succeeded, mirroring
// 05-api-contract.md section 9.1.
type TestIndexerOutput struct {
	Body struct {
		Ok              bool    `json:"ok"`
		ElapsedMS       int64   `json:"elapsed_ms"`
		CategoriesFound int     `json:"categories_found"`
		Server          string  `json:"server"`
		Error           *string `json:"error"`
	}
}

// StartSearchInput is the JSON body of POST /search (doc 05 section 9.2).
type StartSearchInput struct {
	Body struct {
		Query      string   `json:"query"      required:"true" minLength:"1" maxLength:"512" doc:"Search text"`
		IndexerIDs []string `json:"indexer_ids,omitempty" doc:"Enabled indexers to run; default is every enabled indexer"`
		Categories []int    `json:"categories,omitempty" doc:"Newznab category ids; empty means no category filter"`
	}
}

// GetSearchInput addresses one search job and carries the results page
// query: sort, limit and cursor apply to results only (doc 05 section 1.4).
type GetSearchInput struct {
	ID     string `path:"id"     doc:"The sch_… job id"`
	Sort   string `query:"sort"  enum:"seeders,title,size_bytes,leechers,published_at,indexer,-seeders,-title,-size_bytes,-leechers,-published_at,-indexer" doc:"seeders, title, size_bytes, leechers, published_at or indexer, with a leading - to reverse; default -seeders"`
	Limit  int    `query:"limit" minimum:"1" maximum:"500" default:"100" doc:"Page size of the results page"`
	Cursor string `query:"cursor" doc:"Opaque page token from a previous response"`
}

// SearchIDInput addresses one search job for DELETE.
type SearchIDInput struct {
	ID string `path:"id" doc:"The sch_… job id"`
}

// StartedOutput is the 202 body of POST /search: the opaque job id only, so
// the response can never imply the search already ran.
type StartedOutput struct {
	Status int `json:"-"`
	Body   struct {
		ID string `json:"id"`
	}
}

// SearchJobOutput is the GET /search/{id} body of doc 05 section 9.2: the
// job state, the per-engine status array and one page of results.
type SearchJobOutput struct {
	Body struct {
		ID         string               `json:"id"`
		Query      string               `json:"query"`
		Finished   bool                 `json:"finished"`
		Total      int                  `json:"total"        doc:"Result count across all pages of this job, ignoring the cursor"`
		Engines    []store.EngineStatus `json:"engines"     doc:"One entry per selected indexer: queued, searching, done or error"`
		Results    []SearchResultDTO    `json:"results"     doc:"One page of results; sort, limit and cursor apply to this array only"`
		NextCursor *string              `json:"next_cursor" doc:"Opaque cursor for the next results page; null when this is the last page"`
	}
}

// SearchResultDTO is the wire shape of one search_results row. It carries
// metadata and the opaque res_ id only: download_url, magnet_uri and
// details_url are server-only acquisition data and have no field here
// (docs/07-search-and-indexers.md section 5 rule 6).
type SearchResultDTO struct {
	ID                     string   `json:"id"`
	IndexerID              string   `json:"indexer_id"`
	IndexerName            string   `json:"indexer_name"`
	Title                  string   `json:"title"`
	InfoHash               *string  `json:"info_hash"`
	SizeBytes              *int64   `json:"size_bytes"`
	Seeders                *int     `json:"seeders"`
	Leechers               *int     `json:"leechers"`
	Grabs                  *int     `json:"grabs"`
	PublishedAt            *string  `json:"published_at"` // RFC 3339 UTC
	CategoryIDs            []int    `json:"category_ids"`
	CategoryDesc           *string  `json:"category_desc"`
	DownloadVolumeFactor   float64  `json:"download_volume_factor"`
	UploadVolumeFactor     float64  `json:"upload_volume_factor"`
	MinimumRatio           *float64 `json:"minimum_ratio"`
	MinimumSeedTimeSeconds *int     `json:"minimum_seed_time_seconds"`
	IMDBID                 *string  `json:"imdb_id"`
	TMDBID                 *string  `json:"tmdb_id"`
	TVDBID                 *string  `json:"tvdb_id"`
	Year                   *int     `json:"year"`
	Genre                  *string  `json:"genre"`
	Language               *string  `json:"language"`
	Publisher              *string  `json:"publisher"`
	Author                 *string  `json:"author"`
	Album                  *string  `json:"album"`
	Artist                 *string  `json:"artist"`
}

// importRequestBody declares the two media types of docs/05-api-contract.md
// section 9.1 for the generated document; Huma's RawBody support adds its
// own application/octet-stream entry beside them.
func importRequestBody() *huma.RequestBody {
	return &huma.RequestBody{
		Content: map[string]*huma.MediaType{
			"application/json": {
				Schema: &huma.Schema{
					Type:        "object",
					Description: "The provider wizard body: enumerate a Prowlarr or Jackett instance into one disabled row per upstream indexer.",
					Properties: map[string]*huma.Schema{
						"torznab_url": {Type: "string", Format: "uri", Description: "The instance's base or Torznab URL"},
						"api_key":     {Type: "string", Format: "password", Description: "The instance API key, stored sealed on every created row"},
					},
					Required: []string{"torznab_url", "api_key"},
				},
			},
			"multipart/form-data": {
				Schema: &huma.Schema{
					Type:        "object",
					Description: "A .dlsearch.yaml, .dlm or .py engine file upload in one file part, at most 1 MiB; a .py nova3 plugin imports metadata only and is never executed.",
					Properties: map[string]*huma.Schema{
						"file": {Type: "string", Format: "binary"},
					},
				},
			},
		},
	}
}

// SearchHandlers owns the /indexers operations of docs/05-api-contract.md
// section 9.1. indexers, defs, runner and hc arrive through Deps; nil means
// the server was built for the OpenAPI document alone — the store-backed
// handlers answer 500, while TestIndexer answers 503 engine-unavailable.
type SearchHandlers struct {
	log      *slog.Logger
	indexers *store.IndexerStore
	defs     *search.Registry
	runner   *search.Runner
	hc       *http.Client
	db       *sqlx.DB
}

// NewSearchHandlers builds the indexer handlers over the shared deps.
func NewSearchHandlers(log *slog.Logger, d Deps) *SearchHandlers {
	return &SearchHandlers{log: log, indexers: d.Indexers, defs: d.Defs, runner: d.Runner, hc: d.HTTP, db: d.DB}
}

// RegisterSearchRoutes is the single registration point for every /indexers
// and /search operation. NewServer calls it once; later M4 tasks add their
// operations inside it and never touch server.go again.
func RegisterSearchRoutes(api huma.API, h *SearchHandlers) {
	huma.Register(api, huma.Operation{
		OperationID: operationListIndexers,
		Method:      http.MethodGet,
		Path:        "/indexers",
		Summary:     "List the indexers",
		Description: "Every indexer row, sorted by priority then name, served from the database only — no indexer is contacted. api_key_set reports whether a key is stored; the key is never returned.",
		Tags:        []string{"indexers"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.ListIndexers)

	huma.Register(api, huma.Operation{
		OperationID:   operationCreateIndexer,
		Method:        http.MethodPost,
		Path:          "/indexers",
		DefaultStatus: http.StatusCreated,
		Summary:       "Add an indexer",
		Description:   "Creates one indexer row. A torznab or newznab indexer without url is 422; its t=caps is probed through the SSRF guard and a denial is 403 /problems/ssrf-blocked whose detail names allow_private_network as the remedy. A duplicate definition_id is 409 /problems/conflict.",
		Tags:          []string{"indexers"},
		Security:      credentialRequired,
	}, h.CreateIndexer)

	huma.Register(api, huma.Operation{
		OperationID: operationPatchIndexer,
		Method:      http.MethodPatch,
		Path:        "/indexers/{id}",
		Summary:     "Update an indexer",
		Description: "Partial update of name, kind, enabled, url, definition_id, priority, allow_private_network and settings; omitted fields are untouched. api_key replaces the stored key and api_key: \"\" clears it. A duplicate definition_id is 409 /problems/conflict.",
		Tags:        []string{"indexers"},
		Security:    credentialRequired,
	}, h.PatchIndexer)

	huma.Register(api, huma.Operation{
		OperationID:   operationDeleteIndexer,
		Method:        http.MethodDelete,
		Path:          "/indexers/{id}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Delete an indexer",
		Description:   "Removes the indexer row; its search results cascade with it. No other row and no file is touched.",
		Tags:          []string{"indexers"},
		Security:      credentialRequired,
	}, h.DeleteIndexer)

	huma.Register(api, huma.Operation{
		OperationID: operationTestIndexer,
		Method:      http.MethodPost,
		Path:        "/indexers/{id}/test",
		Summary:     "Test an indexer",
		Description: "Performs exactly one capability probe — t=caps for a torznab or newznab indexer, one definition request for a dlsearch engine — and reports the outcome as data. A reachable-but-broken indexer is 200 with ok:false and the upstream status in error; 503 /problems/engine-unavailable means the probe could not be attempted at all.",
		Tags:        []string{"indexers"},
		Security:    credentialRequired,
	}, h.TestIndexer)

	huma.Register(api, huma.Operation{
		OperationID: operationIndexerCategories,
		Method:      http.MethodGet,
		Path:        "/indexers/categories",
		Summary:     "List the newznab categories",
		Description: "The standard newznab tree merged with every enabled indexer's cached caps, de-duplicated on category id and sorted ascending.",
		Tags:        []string{"indexers"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.IndexerCategories)

	huma.Register(api, huma.Operation{
		OperationID:   operationImportIndexer,
		Method:        http.MethodPost,
		Path:          "/indexers/import",
		DefaultStatus: http.StatusCreated,
		Summary:       "Import indexers",
		Description:   "With an application/json body {torznab_url, api_key} this is the provider wizard: it enumerates a Prowlarr or Jackett instance and creates one disabled row per upstream indexer. multipart/form-data carries one file part — a .dlsearch.yaml, .dlm or .py import; every row it creates is disabled.",
		Tags:          []string{"indexers"},
		Security:      credentialRequired,
		RequestBody:   importRequestBody(),
		// Multipart framing adds bytes around the file part, so the
		// body cap must clear the 1 MiB part cap, not merely match it.
		MaxBodyBytes: max(secure.MetadataFetchCap, maxImportFileBytes+4096),
	}, h.ImportIndexer)

	huma.Register(api, huma.Operation{
		OperationID:   operationStartSearch,
		Method:        http.MethodPost,
		Path:          "/search",
		DefaultStatus: http.StatusAccepted,
		Summary:       "Start a search",
		Description:   "Enqueues one asynchronous search job and answers 202 with its id immediately — no indexer is contacted on this request. indexer_ids defaults to every enabled indexer; an unknown or disabled id is 422, and 503 /problems/engine-unavailable means no indexer is enabled at all.",
		Tags:          []string{"search"},
		Security:      credentialRequired,
		// Same strictness as GetSearch: a stray query key is 422, not ignored.
		RejectUnknownQueryParameters: true,
	}, h.StartSearch)

	huma.Register(api, huma.Operation{
		OperationID: operationGetSearch,
		Method:      http.MethodGet,
		Path:        "/search/{id}",
		Summary:     "Poll a search",
		Description: "One search job: finished, the live per-engine status array and one cursor-paginated page of results — sort, limit and cursor apply to results only. A deleted or unknown id is 404 /problems/not-found.",
		Tags:        []string{"search"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.GetSearch)

	huma.Register(api, huma.Operation{
		OperationID:   operationDeleteSearch,
		Method:        http.MethodDelete,
		Path:          "/search/{id}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Delete a search",
		Description:   "Removes the search job; its results cascade with it and its engine status is forgotten. An unknown id is 404 /problems/not-found.",
		Tags:          []string{"search"},
		Security:      credentialRequired,
	}, h.DeleteSearch)
}

// indexerStore returns the shared store or the generic internal problem when
// the server was built without one (the openapi subcommand).
func (h *SearchHandlers) indexerStore(ctx context.Context) (*store.IndexerStore, error) {
	if h.indexers == nil {
		return nil, internalFailure(ctx, "indexers", errors.New("no indexer store"))
	}
	return h.indexers, nil
}

// ListIndexers serves GET /indexers from the database alone.
func (h *SearchHandlers) ListIndexers(ctx context.Context, _ *struct{}) (*ListIndexersOutput, error) {
	st, err := h.indexerStore(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := st.List(ctx, false)
	if err != nil {
		return nil, internalFailure(ctx, "list indexers", err)
	}

	out := &ListIndexersOutput{}
	out.Body.Indexers = make([]IndexerDTO, 0, len(rows))
	for _, row := range rows {
		out.Body.Indexers = append(out.Body.Indexers, toIndexerDTO(h.log, row))
	}
	return out, nil
}

// CreateIndexer serves POST /indexers. A torznab/newznab row is probed with
// t=caps before the insert: a guard denial is 403 /problems/ssrf-blocked, a
// reachable-but-broken endpoint still creates the row and lands the error on
// last_error — broken is data, not a transport failure (doc 05 section 9.1).
func (h *SearchHandlers) CreateIndexer(ctx context.Context, in *CreateIndexerInput) (*IndexerOutput, error) {
	st, err := h.indexerStore(ctx)
	if err != nil {
		return nil, err
	}
	body := in.Body

	indexerURL := optionalString(body.URL)
	if (body.Kind == indexerKindTorznab || body.Kind == indexerKindNewznab) && indexerURL == nil {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "a torznab or newznab indexer needs url")
	}
	if body.Kind == indexerKindDlsearch && optionalString(body.DefinitionID) == nil {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "a dlsearch indexer needs definition_id")
	}
	if indexerURL != nil {
		if err := checkIndexerURL(*indexerURL); err != nil {
			return nil, err
		}
	}

	allowPrivate := body.AllowPrivateNetwork != nil && *body.AllowPrivateNetwork
	settingsJSON, err := indexerSettingsJSON(body.Settings, allowPrivate, "")
	if err != nil {
		return nil, internalFailure(ctx, "create indexer settings", err)
	}

	var apiKey secure.Secret
	if body.APIKey != nil {
		apiKey = *body.APIKey
	}

	// The caps probe runs before the insert so a guard denial creates no
	// row; any other failure is recorded on the row once it exists.
	var caps *search.Caps
	var probeErr error
	if indexerURL != nil && (body.Kind == indexerKindTorznab || body.Kind == indexerKindNewznab) {
		var c search.Caps
		c, probeErr = h.probeCaps(ctx, *indexerURL, apiKey, allowPrivate)
		if probeErr != nil {
			if errors.Is(probeErr, secure.ErrSSRFBlocked) {
				return nil, Problem(
					SlugSSRFBlocked, http.StatusForbidden,
					"the caps probe was refused by the SSRF guard; set allow_private_network for an indexer on a private-network address",
				)
			}
		} else {
			caps = &c
		}
	}

	row := store.Indexer{
		Name:         body.Name,
		Kind:         body.Kind,
		URL:          indexerURL,
		DefinitionID: body.DefinitionID,
		Priority:     store.DefaultIndexerPriority,
		SettingsJSON: &settingsJSON,
	}
	if body.Enabled != nil {
		row.Enabled = *body.Enabled
	}
	if body.Priority != nil {
		row.Priority = *body.Priority
	}

	created, err := st.Create(ctx, row, apiKey)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, Problem(SlugConflict, http.StatusConflict, "an indexer with that definition_id already exists")
		}
		return nil, internalFailure(ctx, "create indexer", err)
	}

	if indexerURL != nil && (body.Kind == indexerKindTorznab || body.Kind == indexerKindNewznab) {
		if err := h.recordProbe(ctx, created.ID, caps, probeErr); err != nil {
			return nil, internalFailure(ctx, "record indexer probe", err)
		}
		// The response reports the probe outcome: categories and
		// last_test_at live on the row recordProbe just wrote.
		created, err = st.Get(ctx, created.ID)
		if err != nil {
			return nil, internalFailure(ctx, "read back indexer", err)
		}
	}

	return &IndexerOutput{Status: http.StatusCreated, Body: toIndexerDTO(h.log, created)}, nil
}

// PatchIndexer serves PATCH /indexers/{id}: the non-nil body fields form the
// patch, api_key: "" clears the stored key, and the settings document keeps
// its internal keys across a settings replacement.
func (h *SearchHandlers) PatchIndexer(ctx context.Context, in *PatchIndexerInput) (*IndexerOutput, error) {
	st, err := h.indexerStore(ctx)
	if err != nil {
		return nil, err
	}
	row, err := st.Get(ctx, in.ID)
	if err != nil {
		return nil, FromStore(err)
	}
	body := in.Body

	kind := row.Kind
	if body.Kind != nil {
		kind = *body.Kind
	}
	indexerURL := row.URL
	if body.URL != nil {
		indexerURL = body.URL
	}
	definitionID := row.DefinitionID
	if body.DefinitionID != nil {
		definitionID = body.DefinitionID
	}
	if (kind == indexerKindTorznab || kind == indexerKindNewznab) && optionalString(indexerURL) == nil {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "a torznab or newznab indexer needs url")
	}
	if kind == indexerKindDlsearch && optionalString(definitionID) == nil {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "a dlsearch indexer needs definition_id")
	}
	if indexerURL != nil && *indexerURL != "" {
		if err := checkIndexerURL(*indexerURL); err != nil {
			return nil, err
		}
	}

	patch := store.IndexerPatch{
		Name:         body.Name,
		URL:          body.URL,
		DefinitionID: body.DefinitionID,
		Kind:         body.Kind,
		Enabled:      body.Enabled,
		Priority:     body.Priority,
		APIKey:       body.APIKey,
	}
	if body.Settings != nil || body.AllowPrivateNetwork != nil {
		settingsJSON, err := mergeIndexerSettings(h.log, row.SettingsJSON, body.Settings, body.AllowPrivateNetwork)
		if err != nil {
			return nil, internalFailure(ctx, "patch indexer settings", err)
		}
		patch.SettingsJSON = &settingsJSON
	}

	updated, err := st.Update(ctx, in.ID, patch)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, Problem(SlugConflict, http.StatusConflict, "an indexer with that definition_id already exists")
		}
		return nil, FromStore(err)
	}

	return &IndexerOutput{Status: http.StatusOK, Body: toIndexerDTO(h.log, updated)}, nil
}

// DeleteIndexer serves DELETE /indexers/{id}.
func (h *SearchHandlers) DeleteIndexer(ctx context.Context, in *IndexerIDInput) (*struct{}, error) {
	st, err := h.indexerStore(ctx)
	if err != nil {
		return nil, err
	}
	if err := st.Delete(ctx, in.ID); err != nil {
		return nil, FromStore(err)
	}
	return nil, nil
}

// TestIndexer serves POST /indexers/{id}/test: exactly one probe whose
// outcome is data — a reachable-but-broken indexer is 200 with ok:false and
// the upstream status in error. 503 /problems/engine-unavailable is only for
// a probe dl-tool could not attempt, and an SSRF denial is 403 naming the
// allow_private_network remedy (doc 05 section 9.1).
func (h *SearchHandlers) TestIndexer(ctx context.Context, in *IndexerIDInput) (*TestIndexerOutput, error) {
	st, err := h.indexerStore(ctx)
	if err != nil {
		return nil, err
	}
	row, err := st.Get(ctx, in.ID)
	if err != nil {
		return nil, FromStore(err)
	}

	var res search.ProbeResult
	switch row.Kind {
	case indexerKindTorznab, indexerKindNewznab:
		res, err = h.probeTorznabIndexer(ctx, st, row)
	case indexerKindDlsearch:
		res, err = h.probeDlsearchIndexer(ctx, st, row)
	default:
		return nil, internalFailure(ctx, "test indexer", fmt.Errorf("unknown indexer kind %q", row.Kind))
	}
	if err != nil {
		return nil, err
	}

	out := &TestIndexerOutput{}
	out.Body.Ok = res.Ok
	out.Body.ElapsedMS = res.ElapsedMS
	out.Body.CategoriesFound = res.CategoriesFound
	out.Body.Server = res.Server
	if res.Error != "" {
		out.Body.Error = &res.Error
	}
	return out, nil
}

// probeTorznabIndexer runs the t=caps probe for a torznab or newznab row and
// stamps the outcome on it — the same pair the create-time probe writes.
func (h *SearchHandlers) probeTorznabIndexer(ctx context.Context, st *store.IndexerStore, row store.Indexer) (search.ProbeResult, error) {
	res := search.ProbeResult{}
	if row.URL == nil || *row.URL == "" {
		return res, Problem(SlugEngineUnavailable, http.StatusServiceUnavailable, "the indexer has no url to probe")
	}
	apiKey, err := st.OpenAPIKey(row)
	if err != nil {
		return res, internalFailure(ctx, "open indexer key", err)
	}
	start := time.Now()
	caps, probeErr := h.probeCaps(ctx, *row.URL, apiKey, indexerAllowPrivate(row))
	res.ElapsedMS = time.Since(start).Milliseconds()

	var good *search.Caps
	if probeErr == nil {
		good = &caps
		res.Ok = true
		res.CategoriesFound = len(search.FlattenCategories(caps.Categories))
		res.Server = caps.ServerTitle
	} else {
		res.Error = probeErr.Error()
	}
	// A guard denial is a policy refusal, not a probe outcome — like
	// CreateIndexer it stamps nothing on the row, matching the dlsearch
	// branch which returns before recording.
	if errors.Is(probeErr, secure.ErrSSRFBlocked) {
		return res, Problem(
			SlugSSRFBlocked, http.StatusForbidden,
			"the caps probe was refused by the SSRF guard; set allow_private_network for an indexer on a private-network address",
		)
	}
	if err := h.recordProbe(ctx, row.ID, good, probeErr); err != nil {
		return res, internalFailure(ctx, "record indexer test", err)
	}
	return res, nil
}

// probeDlsearchIndexer resolves the row's definition and runs the runner's
// one-request probe. A definition that is not loaded or a runner that is not
// configured means the probe could not be attempted — 503.
func (h *SearchHandlers) probeDlsearchIndexer(ctx context.Context, st *store.IndexerStore, row store.Indexer) (search.ProbeResult, error) {
	res := search.ProbeResult{}
	if h.defs == nil || h.runner == nil {
		return res, Problem(SlugEngineUnavailable, http.StatusServiceUnavailable, "the search runner is not configured")
	}
	if row.DefinitionID == nil || *row.DefinitionID == "" {
		return res, Problem(SlugEngineUnavailable, http.StatusServiceUnavailable, "the indexer has no definition_id")
	}
	def, ok := h.defs.Get(*row.DefinitionID)
	if !ok {
		return res, Problem(SlugEngineUnavailable, http.StatusServiceUnavailable, "the indexer's definition is not loaded")
	}

	cfg := indexerSettingsMap(h.log, row)
	apiKey, err := st.OpenAPIKey(row)
	if err != nil {
		return res, internalFailure(ctx, "open indexer key", err)
	}
	if apiKey.Reveal() != "" {
		cfg["api_key"] = apiKey.Reveal()
	}

	res, err = h.runner.Probe(ctx, def, cfg)
	if err != nil {
		if errors.Is(err, secure.ErrSSRFBlocked) {
			return res, Problem(
				SlugSSRFBlocked, http.StatusForbidden,
				"the definition request was refused by the SSRF guard; set allow_private_network for an indexer on a private-network address",
			)
		}
		// The cause may embed the built request URL — including a setting
		// interpolated into its query — so it goes to the server log, not
		// the problem detail.
		h.log.Warn("dlsearch probe could not be attempted", "indexer_id", row.ID, "error", err)
		return res, Problem(SlugEngineUnavailable, http.StatusServiceUnavailable, "the probe could not be attempted")
	}
	if err := h.recordDlsearchProbe(ctx, row.ID, def, res); err != nil {
		return res, internalFailure(ctx, "record indexer test", err)
	}
	return res, nil
}

// recordDlsearchProbe stamps a dlsearch probe outcome on the row: on success
// the definition's caps land on categories_json like a fetched torznab caps
// document would, and last_test_at moves either way.
func (h *SearchHandlers) recordDlsearchProbe(ctx context.Context, id string, def *search.Definition, res search.ProbeResult) error {
	now := time.Now().UnixMilli()
	if !res.Ok {
		return h.indexers.RecordTest(ctx, id, now, &res.Error)
	}
	flat, err := json.Marshal(defCategories(def))
	if err != nil {
		return err
	}
	if err := h.indexers.SetCaps(ctx, id, string(flat), def.Caps.SeedersUnknown); err != nil {
		return err
	}
	return h.indexers.RecordTest(ctx, id, now, nil)
}

// defCategories renders a definition's caps.categories as the flat list an
// indexer row caches: the newznab id is the id, the site value the name.
func defCategories(def *search.Definition) []search.Category {
	type pair struct {
		site string
		id   int
	}
	pairs := make([]pair, 0, len(def.Caps.Categories))
	for site, id := range def.Caps.Categories {
		pairs = append(pairs, pair{site, id})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].id < pairs[j].id })
	out := make([]search.Category, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, search.Category{ID: p.id, Name: p.site})
	}
	return out
}

// indexerAllowPrivate reads the reserved flag out of settings_json.
func indexerAllowPrivate(row store.Indexer) bool {
	if row.SettingsJSON == nil {
		return false
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(*row.SettingsJSON), &doc); err != nil {
		return false
	}
	v, _ := doc[indexerSettingAllowPrivate].(bool)
	return v
}

// indexerSettingsMap decodes settings_json into the string map the runner's
// Scope.Config reads — the reserved keys pass through so the runner sees
// allow_private_network. The single decode lives in the store package
// (T061): the probe path and the search-job fan-out share it.
func indexerSettingsMap(log *slog.Logger, row store.Indexer) map[string]string {
	return store.IndexerSettingsMap(log, row)
}

// IndexerCategories serves GET /indexers/categories: the default newznab
// tree with every enabled indexer's cached caps merged over it, de-duplicated
// on category id and sorted ascending.
func (h *SearchHandlers) IndexerCategories(ctx context.Context, _ *struct{}) (*CategoriesOutput, error) {
	st, err := h.indexerStore(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := st.List(ctx, true)
	if err != nil {
		return nil, internalFailure(ctx, "list enabled indexers", err)
	}

	out := &CategoriesOutput{}
	out.Body.Categories = mergeCategories(h.log, search.DefaultCategories(), rows)
	return out, nil
}

// ImportIndexer serves POST /indexers/import. application/json runs the
// provider wizard; multipart/form-data carries one "file" part whose name
// picks the importer — .dlm, .dlsearch.yaml or .py. Every other media type
// is 415.
func (h *SearchHandlers) ImportIndexer(ctx context.Context, in *ImportIndexerInput) (*ImportOutput, error) {
	st, err := h.indexerStore(ctx)
	if err != nil {
		return nil, err
	}
	mediaType, params, err := mime.ParseMediaType(in.ContentType)
	if err != nil {
		return nil, Problem(
			SlugUnsupportedMediaType, http.StatusUnsupportedMediaType,
			"POST /indexers/import accepts application/json {torznab_url, api_key} or multipart/form-data with one .dlm, .dlsearch.yaml or .py file part",
		)
	}
	if mediaType == "multipart/form-data" {
		return h.importIndexerFile(ctx, st, in.RawBody, params)
	}
	if mediaType != "application/json" {
		return nil, Problem(
			SlugUnsupportedMediaType, http.StatusUnsupportedMediaType,
			"POST /indexers/import accepts application/json {torznab_url, api_key} or multipart/form-data with one .dlm, .dlsearch.yaml or .py file part",
		)
	}

	var body struct {
		TorznabURL string `json:"torznab_url"`
		APIKey     string `json:"api_key"`
	}
	if err := json.Unmarshal(in.RawBody, &body); err != nil {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "the import body is not valid JSON")
	}
	u, err := url.Parse(body.TorznabURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "torznab_url must be an http or https URL")
	}
	if body.APIKey == "" {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "api_key is required")
	}

	// Provider hosts are normally private-network addresses on the Compose
	// network (07-search-and-indexers.md section 2.7), so the enumeration
	// and per-indexer caps fetches run through a private-allowing guard
	// scoped to this one origin.
	hc := secure.NewClient(secure.NewGuard(h.log, true).ForOrigin(u))
	apiKey := secure.Secret(body.APIKey)
	entries, err := search.EnumerateProvider(ctx, hc, body.TorznabURL, apiKey)
	if err != nil {
		if errors.Is(err, secure.ErrSSRFBlocked) {
			return nil, Problem(SlugSSRFBlocked, http.StatusForbidden, "the provider enumeration was refused by the SSRF guard")
		}
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "the provider did not answer the enumeration request")
	}
	if len(entries) == 0 {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "the provider reports no configured indexers")
	}

	settingsJSON, err := indexerSettingsJSON(nil, true, u.Scheme+"://"+u.Host)
	if err != nil {
		return nil, internalFailure(ctx, "import indexer settings", err)
	}

	// Re-running the wizard against the same provider is a normal workflow;
	// the per-indexer base URL is the dedup key, so a second import skips
	// known rows instead of doubling the list.
	existing, err := st.List(ctx, false)
	if err != nil {
		return nil, internalFailure(ctx, "list indexers for import", err)
	}
	known := make(map[string]bool, len(existing))
	for _, row := range existing {
		if row.URL != nil {
			known[*row.URL] = true
		}
	}

	out := &ImportOutput{}
	out.Body.Warnings = []string{}
	var first *store.Indexer
	var badSkips int
	for _, entry := range entries {
		if known[entry.BaseURL] {
			out.Body.Warnings = append(out.Body.Warnings, entry.Name+": already imported; skipped")
			continue
		}
		if !search.ValidBaseURL(entry.BaseURL) {
			badSkips++
			out.Body.Warnings = append(out.Body.Warnings, entry.Name+": unusable base URL; skipped")
			continue
		}
		row := store.Indexer{
			Name:             entry.Name,
			Kind:             indexerKindTorznab,
			Enabled:          false,
			URL:              &entry.BaseURL,
			DefinitionSource: ptr(indexerImportedSource),
			Provenance:       ptr(indexerImportedProvenance),
			LegalTier:        indexerTierUserSupplied,
			Priority:         store.DefaultIndexerPriority,
			SettingsJSON:     &settingsJSON,
		}
		created, err := st.Create(ctx, row, apiKey)
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				out.Body.Warnings = append(out.Body.Warnings, entry.Name+": conflicts with an existing row; skipped")
				continue
			}
			return nil, internalFailure(ctx, "import indexer", err)
		}
		known[entry.BaseURL] = true
		if first == nil {
			first = &created
		}

		// Store the flattened caps tree; a failed fetch keeps the row and
		// lands on last_error plus a warning — the indexer list answers
		// even where one upstream indexer is broken.
		var caps *search.Caps
		c, probeErr := h.probeCapsOn(ctx, hc, entry.BaseURL, apiKey)
		if probeErr == nil {
			caps = &c
		}
		if err := h.recordProbe(ctx, created.ID, caps, probeErr); err != nil {
			return nil, internalFailure(ctx, "record indexer probe", err)
		}
		if probeErr != nil {
			out.Body.Warnings = append(out.Body.Warnings, entry.Name+": caps fetch failed")
		}
	}

	if first == nil {
		// Nothing usable came back at all. An empty enumeration and
		// all-unusable input are validation failures; only pure duplicates
		// are a real conflict.
		if len(entries) == 0 {
			return nil, Problem(
				SlugValidationFailed,
				http.StatusUnprocessableEntity,
				"provider returned no indexers",
			)
		}
		detail := "every indexer from this provider was skipped"
		if len(out.Body.Warnings) > 0 {
			detail = strings.Join(out.Body.Warnings, "; ")
		}
		if badSkips > 0 {
			return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, detail)
		}
		return nil, Problem(SlugConflict, http.StatusConflict, detail)
	}
	// The response reports the probe outcome: re-read the first row so its
	// categories and last_test_at are the post-probe values.
	fresh, err := st.Get(ctx, first.ID)
	if err != nil {
		return nil, internalFailure(ctx, "read back indexer", err)
	}
	out.Body.Indexer = toIndexerDTO(h.log, fresh)
	return out, nil
}

// importIndexerFile is the multipart branch of POST /indexers/import: the
// form carries exactly one "file" part, capped at 1 MiB, and the file name
// picks the importer — .dlm goes through the static analyser,
// .dlsearch.yaml through the definition loader, and .py through the
// literal-only nova3 reader. The created row is always disabled and always
// records where it came from (doc 07 section 7).
func (h *SearchHandlers) importIndexerFile(ctx context.Context, st *store.IndexerStore, body []byte, params map[string]string) (*ImportOutput, error) {
	if params["boundary"] == "" {
		return nil, Problem(
			SlugValidationFailed, http.StatusUnprocessableEntity,
			"the multipart content type carries no boundary",
		)
	}

	var file UploadedFile
	seen := false
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, Problem(
				SlugValidationFailed, http.StatusUnprocessableEntity,
				"the multipart form is malformed",
			)
		}
		if part.FormName() != filePart {
			continue
		}
		if seen {
			return nil, Problem(
				SlugValidationFailed, http.StatusUnprocessableEntity,
				"the import form carries more than one file part",
			)
		}
		seen = true
		content, err := readFormPart(part, maxImportFileBytes)
		if err != nil {
			if errors.Is(err, ErrPayloadTooLarge) {
				return nil, Problem(
					SlugPayloadTooLarge, http.StatusRequestEntityTooLarge,
					fmt.Sprintf("the uploaded file exceeds the %d-byte import cap", maxImportFileBytes),
				)
			}
			return nil, Problem(
				SlugValidationFailed, http.StatusUnprocessableEntity,
				"the file part could not be read",
			)
		}
		file = UploadedFile{Name: part.FileName(), Bytes: content}
	}
	if !seen {
		return nil, Problem(
			SlugValidationFailed, http.StatusUnprocessableEntity,
			"the import form carries no file part",
		)
	}

	var res search.ImportResult
	var err error
	switch name := strings.ToLower(file.Name); {
	case strings.HasSuffix(name, ".dlm"):
		res, err = search.ImportDLM(file.Bytes, file.Name)
	case strings.HasSuffix(name, ".dlsearch.yaml"):
		res, err = search.ImportDefinitionFile(file.Bytes, file.Name)
	case strings.HasSuffix(name, ".py"):
		res, err = search.ImportNovaPlugin(file.Bytes, file.Name)
	default:
		return nil, Problem(
			SlugValidationFailed, http.StatusUnprocessableEntity,
			"the file part must be a .dlm, .dlsearch.yaml or .py upload",
		)
	}
	if err != nil {
		// Importer rejections name the rule they hit; that name is the
		// detail the 422 contract wants.
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, err.Error())
	}

	settingsJSON, err := importedIndexerSettings(res)
	if err != nil {
		return nil, internalFailure(ctx, "import indexer settings", err)
	}
	row := store.Indexer{
		Name:             res.Name,
		Kind:             indexerKindDlsearch,
		Enabled:          false,
		DefinitionSource: ptr(indexerImportedSource),
		Provenance:       ptr(res.Provenance),
		LegalTier:        indexerTierUserSupplied,
		Priority:         store.DefaultIndexerPriority,
		// An unconverted import has no engine at all — it cannot report
		// seeders, which is exactly what the flag means.
		SeedersUnknown: true,
		SettingsJSON:   &settingsJSON,
	}
	if res.Definition != nil {
		row.DefinitionID = &res.Definition.ID
		row.SeedersUnknown = res.Definition.Caps.SeedersUnknown
	}
	// A .py import has no definition; the row itself carries the plugin's
	// declared site URL and its supported_categories folded to newznab
	// ids, so the list shows what the plugin claimed (doc 07 section 4.3).
	// The provenance check, not the extension, is the gate: only the
	// nova3 reader may populate these fields on an ImportResult.
	if res.Provenance == search.ProvenanceQbtPy {
		if res.URL != "" {
			if u, perr := url.Parse(res.URL); perr == nil && u.Host != "" &&
				(u.Scheme == "http" || u.Scheme == "https") {
				row.URL = &res.URL
			} else {
				res.Warnings = append(res.Warnings,
					"the plugin's url is not an http or https URL; it is kept only in the stored source")
			}
		}
		if len(res.Categories) > 0 {
			flat, cerr := json.Marshal(res.Categories)
			if cerr != nil {
				return nil, internalFailure(ctx, "import indexer categories", cerr)
			}
			row.CategoriesJSON = ptr(string(flat))
		}
	}

	created, err := st.Create(ctx, row, "")
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, Problem(SlugConflict, http.StatusConflict, "an indexer with that definition_id already exists")
		}
		return nil, internalFailure(ctx, "import indexer", err)
	}

	out := &ImportOutput{}
	out.Body.Warnings = res.Warnings
	out.Body.Indexer = toIndexerDTO(h.log, created)
	return out, nil
}

// importedIndexerSettings renders the settings_json document of a file
// import: the reserved flag and origin keys, the inert original module as
// base64, the auto-converted flag, and — when the import produced a
// definition — the draft YAML the "view source" pane shows. All four keys
// are API-owned; mergeIndexerSettings never lets a caller write them.
func importedIndexerSettings(res search.ImportResult) (string, error) {
	// Origin is the client-supplied file name — basename'd by
	// mime/multipart already, but still capped before it is stored.
	origin := strings.ToValidUTF8(res.Origin, "")
	if len(origin) > 255 {
		origin = strings.ToValidUTF8(origin[:255], "")
	}
	doc := map[string]any{
		indexerSettingAllowPrivate: false,
		indexerSettingOrigin:       origin,
		indexerSettingConverted:    res.Converted,
		indexerSettingModuleSource: base64.StdEncoding.EncodeToString(res.Source),
	}
	if res.Definition != nil {
		defYAML, err := yaml.Marshal(res.Definition)
		if err != nil {
			return "", fmt.Errorf("marshal imported definition: %w", err)
		}
		doc[indexerSettingDefinition] = string(defYAML)
	}
	// A .py import's provenance extras: the version has no row column and
	// the site values of supported_categories are kept verbatim beside the
	// mapped newznab ids. Gated on the provenance the nova3 importer sets
	// so no other import path can write plugin_* keys.
	if res.Provenance == search.ProvenanceQbtPy {
		if res.Version != "" {
			doc[indexerSettingPluginVersion] = res.Version
		}
		if len(res.SiteCategories) > 0 {
			doc[indexerSettingPluginCategories] = res.SiteCategories
		}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// checkIndexerURL rejects a url the torznab client can never fetch,
// deferring to the client's own shape rule so the two can never drift.
func checkIndexerURL(raw string) error {
	if !search.ValidBaseURL(raw) {
		return Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "url must be an absolute http or https URL")
	}
	return nil
}

// probeCaps fetches t=caps for one torznab/newznab base URL. allowPrivate
// rebuilds the guarded client with the private-range lift scoped to that
// URL's origin — the flag exempts this indexer's host and port only.
func (h *SearchHandlers) probeCaps(ctx context.Context, rawURL string, apiKey secure.Secret, allowPrivate bool) (search.Caps, error) {
	hc := h.hc
	if allowPrivate {
		u, err := url.Parse(rawURL)
		if err != nil {
			return search.Caps{}, fmt.Errorf("parse indexer url: %w", err)
		}
		hc = secure.NewClient(secure.NewGuard(h.log, true).ForOrigin(u))
	}
	if hc == nil {
		return search.Caps{}, errors.New("no outbound client")
	}
	return h.probeCapsOn(ctx, hc, rawURL, apiKey)
}

// probeCapsOn runs the t=caps fetch through the client the caller chose.
func (h *SearchHandlers) probeCapsOn(ctx context.Context, hc *http.Client, rawURL string, apiKey secure.Secret) (search.Caps, error) {
	client, err := search.NewTorznabClient(hc, rawURL, apiKey, "", indexerUserAgent())
	if err != nil {
		return search.Caps{}, err
	}
	return client.Caps(ctx)
}

// recordProbe lands one probe outcome on the row: the flattened caps tree on
// success, the error text on last_error on failure.
func (h *SearchHandlers) recordProbe(ctx context.Context, id string, caps *search.Caps, probeErr error) error {
	now := time.Now().UnixMilli()
	if probeErr != nil {
		msg := probeErr.Error()
		return h.indexers.RecordTest(ctx, id, now, &msg)
	}
	flat, err := json.Marshal(search.FlattenCategories(caps.Categories))
	if err != nil {
		return err
	}
	if err := h.indexers.SetCaps(ctx, id, string(flat), false); err != nil {
		return err
	}
	return h.indexers.RecordTest(ctx, id, now, nil)
}

// indexerUserAgent is the User-Agent the caps probes send.
func indexerUserAgent() string { return "dl-tool/" + Version }

// optionalString treats an absent or all-whitespace pointer as nil.
func optionalString(s *string) *string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	return s
}

func ptr[T any](v T) *T { return &v }

// indexerSettingsJSON renders the settings_json document: the per-engine
// settings plus the allow_private_network flag and, on imported rows, the
// provider origin. The reserved keys are written last so a caller's
// settings map can never forge them.
func indexerSettingsJSON(settings map[string]string, allowPrivate bool, origin string) (string, error) {
	doc := make(map[string]any, len(settings)+2)
	for k, v := range settings {
		doc[k] = v
	}
	doc[indexerSettingAllowPrivate] = allowPrivate
	if origin != "" {
		doc[indexerSettingOrigin] = origin
	} else {
		delete(doc, indexerSettingOrigin)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// mergeIndexerSettings applies a PATCH's settings and allow_private_network
// onto the stored document. A provided settings map replaces the per-engine
// keys but keeps the internal keys (origin, the flag unless overridden). A
// stored document that is not valid JSON is treated as empty and logged:
// one corrupt row must not become unpatchable.
func mergeIndexerSettings(log *slog.Logger, existing *string, settings map[string]string, allowPrivate *bool) (string, error) {
	doc := map[string]any{}
	if existing != nil && *existing != "" {
		if err := json.Unmarshal([]byte(*existing), &doc); err != nil {
			log.Warn("indexer settings_json is not valid JSON; treating it as empty", "error", err)
			doc = map[string]any{}
		}
	}
	if settings != nil {
		internal := map[string]any{}
		for _, k := range indexerInternalSettingKeys {
			if v, ok := doc[k]; ok {
				internal[k] = v
			}
		}
		doc = make(map[string]any, len(settings)+len(internal))
		for k, v := range settings {
			// The reserved keys are the API's, never the caller's: a
			// settings map carrying them cannot forge the private-range
			// lift, an import origin the row does not have, or the inert
			// module blob and draft an imported row carries.
			if slices.Contains(indexerInternalSettingKeys, k) {
				continue
			}
			doc[k] = v
		}
		for k, v := range internal {
			doc[k] = v
		}
	}
	if allowPrivate != nil {
		doc[indexerSettingAllowPrivate] = *allowPrivate
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// mergeCategories overlays every enabled indexer's cached flat categories on
// the default tree: an id a default root or subcategory already carries keeps
// its position with the indexer's name; an unknown id appends as a root.
// Roots and subcategories sort ascending by id. When two enabled indexers
// shadow the same id the row the store sorts last wins — rows arrive ordered
// by priority then name, so the highest (priority, name) pair prevails.
func mergeCategories(log *slog.Logger, defaults []search.Category, rows []store.Indexer) []search.Category {
	// Deep-copy the tree: a caps rename writes into Subcategories, and the
	// caller's defaults slice must never be mutated.
	roots := make([]search.Category, len(defaults))
	for i, r := range defaults {
		roots[i] = r
		roots[i].Subcategories = append([]search.Category(nil), r.Subcategories...)
	}
	type position struct{ root, sub int }
	positions := map[int]position{}
	for i, r := range roots {
		positions[r.ID] = position{i, -1}
		for j, s := range r.Subcategories {
			positions[s.ID] = position{i, j}
		}
	}
	for _, row := range rows {
		if row.CategoriesJSON == nil {
			continue
		}
		var cats []search.Category
		if err := json.Unmarshal([]byte(*row.CategoriesJSON), &cats); err != nil {
			log.Warn("indexer categories_json is not valid JSON; skipped in the merged tree", "indexer_id", row.ID)
			continue
		}
		for _, c := range cats {
			if p, ok := positions[c.ID]; ok {
				if p.sub < 0 {
					roots[p.root].Name = c.Name
				} else {
					roots[p.root].Subcategories[p.sub].Name = c.Name
				}
				continue
			}
			positions[c.ID] = position{len(roots), -1}
			roots = append(roots, search.Category{ID: c.ID, Name: c.Name})
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].ID < roots[j].ID })
	for i := range roots {
		sort.Slice(roots[i].Subcategories, func(a, b int) bool {
			return roots[i].Subcategories[a].ID < roots[i].Subcategories[b].ID
		})
	}
	return roots
}

// toIndexerDTO renders one store row. APIKeySet comes from a non-nil
// api_key_enc — the key is never opened, copied or returned. A
// categories_json that does not parse degrades to an empty list with a
// log line: only an out-of-band write can produce one, since SetCaps
// stores json.Marshal output.
func toIndexerDTO(log *slog.Logger, row store.Indexer) IndexerDTO {
	dto := IndexerDTO{
		ID:               row.ID,
		Name:             row.Name,
		Kind:             row.Kind,
		Enabled:          row.Enabled,
		URL:              row.URL,
		APIKeySet:        row.APIKeyEnc != nil,
		DefinitionID:     row.DefinitionID,
		DefinitionSource: row.DefinitionSource,
		Provenance:       row.Provenance,
		LegalTier:        row.LegalTier,
		Priority:         row.Priority,
		SeedersUnknown:   row.SeedersUnknown,
		Categories:       []search.Category{},
		LastError:        row.LastError,
	}
	if row.CategoriesJSON != nil {
		var cats []search.Category
		if err := json.Unmarshal([]byte(*row.CategoriesJSON), &cats); err == nil {
			dto.Categories = cats
		} else {
			log.Warn("indexer categories_json is not valid JSON; reporting an empty list", "indexer_id", row.ID)
		}
	}
	if row.LastTestAt != nil {
		s := time.UnixMilli(*row.LastTestAt).UTC().Format(time.RFC3339)
		dto.LastTestAt = &s
	}
	return dto
}

// searchDB returns the queue handle, or the generic internal problem when
// the server was built for the OpenAPI document alone.
func (h *SearchHandlers) searchDB(ctx context.Context) (*sqlx.DB, error) {
	if h.db == nil {
		return nil, internalFailure(ctx, "search jobs", errors.New("no database"))
	}
	return h.db, nil
}

// StartSearch serves POST /search. It validates the selection, writes the
// search_jobs row, seeds the tracker with every engine queued and enqueues
// one jobs row of kind "search" — the 202 carries the id only, and no
// indexer is contacted on this request (doc 05 section 9.2).
func (h *SearchHandlers) StartSearch(ctx context.Context, in *StartSearchInput) (*StartedOutput, error) {
	db, err := h.searchDB(ctx)
	if err != nil {
		return nil, err
	}
	st, err := h.indexerStore(ctx)
	if err != nil {
		return nil, err
	}

	// An explicit indexer_ids list is resolved row by row — an unknown id
	// is 422 — while the empty list defaults to every enabled indexer, and
	// no enabled indexer at all is 503 /problems/engine-unavailable.
	var selected []store.Indexer
	if len(in.Body.IndexerIDs) == 0 {
		selected, err = st.List(ctx, true)
		if err != nil {
			return nil, internalFailure(ctx, "list enabled indexers", err)
		}
		if len(selected) == 0 {
			return nil, Problem(
				SlugEngineUnavailable, http.StatusServiceUnavailable,
				"no indexer is enabled",
			)
		}
	} else {
		// The explicit list resolves row by row: an unknown id is 422, a
		// disabled one the same — enabled is the operator's off-switch and
		// the fan-out must not contact an indexer that was turned off —
		// and a repeated id is deduped so an engine never appears twice.
		selected = make([]store.Indexer, 0, len(in.Body.IndexerIDs))
		seen := make(map[string]struct{}, len(in.Body.IndexerIDs))
		for _, id := range in.Body.IndexerIDs {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			row, err := st.Get(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				return nil, Problem(
					SlugValidationFailed, http.StatusUnprocessableEntity,
					fmt.Sprintf("unknown indexer id %q", id),
				)
			}
			if err != nil {
				return nil, internalFailure(ctx, "resolve indexer", err)
			}
			if !row.Enabled {
				return nil, Problem(
					SlugValidationFailed, http.StatusUnprocessableEntity,
					fmt.Sprintf("indexer %q is disabled", id),
				)
			}
			selected = append(selected, row)
		}
	}

	ids := make([]string, len(selected))
	engines := make([]store.EngineStatus, len(selected))
	for i, row := range selected {
		ids[i] = row.ID
		engines[i] = store.EngineStatus{ID: row.ID, Name: row.Name, Status: store.EngineQueued}
	}
	idsJSON, err := json.Marshal(ids)
	if err != nil {
		return nil, internalFailure(ctx, "encode indexer ids", err)
	}
	var categoriesJSON *string
	if len(in.Body.Categories) > 0 {
		raw, err := json.Marshal(in.Body.Categories)
		if err != nil {
			return nil, internalFailure(ctx, "encode categories", err)
		}
		categoriesJSON = ptr(string(raw))
	}

	job, err := store.CreateSearchJob(ctx, db, store.SearchJob{
		Query:          in.Body.Query,
		IndexerIDsJSON: string(idsJSON),
		CategoriesJSON: categoriesJSON,
	})
	if err != nil {
		return nil, internalFailure(ctx, "create search job", err)
	}

	// The tracker is seeded before the enqueue so the first poll can never
	// observe a claimed-but-unknown engine list.
	store.Searches.Start(job.ID, engines)

	payload := jobs.SearchPayload{
		SearchJobID: job.ID,
		Query:       in.Body.Query,
		IndexerIDs:  ids,
		Categories:  in.Body.Categories,
	}
	// One statement carries max_attempts 1: a polling worker can never claim
	// the row between an insert and a clamp update and read the DDL
	// default. On failure the search_jobs row and tracker entry roll back —
	// an enqueue that failed must not leave a job polling queued forever.
	if _, err := store.EnqueueSearchJob(ctx, db, payload, time.Now().UnixMilli()); err != nil {
		store.Searches.Forget(job.ID)
		if derr := store.DeleteSearchJob(context.WithoutCancel(ctx), db, job.ID); derr != nil {
			h.log.WarnContext(ctx, "search job cleanup after enqueue failure failed",
				"search_job_id", job.ID, "err", derr)
		}
		return nil, internalFailure(ctx, "enqueue search job", err)
	}

	out := &StartedOutput{Status: http.StatusAccepted}
	out.Body.ID = job.ID
	return out, nil
}

// GetSearch serves GET /search/{id}: the job row, the per-engine status
// array — the tracker snapshot while the job is live, reconstructed from
// stored counts once a restart forgot it — and one page of results.
func (h *SearchHandlers) GetSearch(ctx context.Context, in *GetSearchInput) (*SearchJobOutput, error) {
	db, err := h.searchDB(ctx)
	if err != nil {
		return nil, err
	}
	st, err := h.indexerStore(ctx)
	if err != nil {
		return nil, err
	}

	job, err := store.GetSearchJob(ctx, db, in.ID)
	if err != nil {
		return nil, FromStore(err)
	}

	// One indexer list read serves the engine names and indexer_name on
	// every result row.
	names, err := h.indexerNames(ctx, st)
	if err != nil {
		return nil, err
	}

	engines, ok := store.Searches.Snapshot(job.ID)
	if !ok {
		engines, err = reconstructEngines(ctx, db, job, names)
		if err != nil {
			return nil, err
		}
	}

	rows, nextCursor, total, err := store.ListResults(ctx, db, job.ID, in.Sort, in.Limit, in.Cursor)
	if err != nil {
		return nil, searchListProblem(ctx, err)
	}

	out := &SearchJobOutput{}
	out.Body.ID = job.ID
	out.Body.Query = job.Query
	out.Body.Finished = job.Finished
	// total is the live result count — the job row's own total column is
	// only stamped at finish, and a poll mid-run must see partial results
	// counted (doc 05 section 9.2).
	out.Body.Total = total
	// The arrays are always non-null on the wire — [] for an empty state,
	// so a consumer never maps over null.
	if engines == nil {
		engines = []store.EngineStatus{}
	}
	out.Body.Engines = engines
	out.Body.Results = toSearchResultDTOs(rows, names)
	if nextCursor != "" {
		out.Body.NextCursor = &nextCursor
	}
	return out, nil
}

// DeleteSearch serves DELETE /search/{id}: the job row goes, its results
// cascade, and the tracker forgets the in-flight engine status.
func (h *SearchHandlers) DeleteSearch(ctx context.Context, in *SearchIDInput) (*struct{}, error) {
	db, err := h.searchDB(ctx)
	if err != nil {
		return nil, err
	}
	if err := store.DeleteSearchJob(ctx, db, in.ID); err != nil {
		return nil, FromStore(err)
	}
	// Drop the queue row too, so a still-pending job is never claimed for a
	// search that no longer exists. Best-effort: a row already running exits
	// quietly once the search_jobs row is gone.
	if err := store.DeleteSearchQueueRow(ctx, db, in.ID); err != nil {
		h.log.WarnContext(ctx, "search queue row delete failed", "search_job_id", in.ID, "err", err)
	}
	store.Searches.Forget(in.ID)
	return nil, nil
}

// indexerNames maps every indexer id to its display name — one list read
// covers the engines array and indexer_name on every result row.
func (h *SearchHandlers) indexerNames(ctx context.Context, st *store.IndexerStore) (map[string]string, error) {
	all, err := st.List(ctx, false)
	if err != nil {
		return nil, internalFailure(ctx, "list indexers", err)
	}
	names := make(map[string]string, len(all))
	for _, row := range all {
		names[row.ID] = row.Name
	}
	return names, nil
}

// reconstructEngines rebuilds a job's engines array once the in-memory
// tracker has forgotten it — a restart. Per-engine error detail is volatile
// and gone by then; a stored row count marks the engine done, and on an
// unfinished job a count-less engine reports queued — the state the
// re-claimed queue row will start it in.
func reconstructEngines(ctx context.Context, db *sqlx.DB, job store.SearchJob, names map[string]string) ([]store.EngineStatus, error) {
	var ids []string
	if err := json.Unmarshal([]byte(job.IndexerIDsJSON), &ids); err != nil {
		return nil, internalFailure(ctx, "decode search job indexers", err)
	}
	counts, err := store.CountResultsByIndexer(ctx, db, job.ID)
	if err != nil {
		return nil, internalFailure(ctx, "count search results", err)
	}

	engines := make([]store.EngineStatus, 0, len(ids))
	for _, id := range ids {
		name, ok := names[id]
		if !ok {
			name = id
		}
		status := store.EngineQueued
		if job.Finished || counts[id] > 0 {
			status = store.EngineDone
		}
		engines = append(engines, store.EngineStatus{ID: id, Name: name, Status: status, Count: counts[id]})
	}
	return engines, nil
}

// searchListProblem maps the store's list errors onto the registered
// problem slugs: an unknown sort key or a foreign cursor is 422 with the
// offending field located in errors[]; anything else is internal.
func searchListProblem(ctx context.Context, err error) error {
	var field, detail string
	switch {
	case errors.Is(err, store.ErrInvalidSort):
		field = "query.sort"
		detail = "the sort key is not a sortable column"
	case errors.Is(err, store.ErrStaleCursor):
		field = "query.cursor"
		detail = "the cursor was issued for a different job or sort"
	default:
		return internalFailure(ctx, "list search results", err)
	}

	return &huma.ErrorModel{
		Type:   SlugValidationFailed,
		Title:  http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity,
		Detail: detail,
		Errors: []*huma.ErrorDetail{{Message: detail, Location: field}},
	}
}

// toSearchResultDTOs renders one page of stored rows: unix milliseconds to
// RFC 3339 at the boundary, category_ids decoded, indexer_name resolved
// from the id map. Acquisition fields have no counterpart here by design.
func toSearchResultDTOs(rows []store.SearchResultRow, names map[string]string) []SearchResultDTO {
	out := make([]SearchResultDTO, 0, len(rows))
	for _, r := range rows {
		dto := SearchResultDTO{
			ID:                     r.ID,
			IndexerID:              r.IndexerID,
			IndexerName:            r.IndexerID,
			Title:                  r.Title,
			InfoHash:               r.InfoHash,
			SizeBytes:              r.SizeBytes,
			Seeders:                r.Seeders,
			Leechers:               r.Leechers,
			Grabs:                  r.Grabs,
			CategoryIDs:            []int{},
			CategoryDesc:           r.CategoryDesc,
			DownloadVolumeFactor:   r.DownloadVolumeFactor,
			UploadVolumeFactor:     r.UploadVolumeFactor,
			MinimumRatio:           r.MinimumRatio,
			MinimumSeedTimeSeconds: r.MinimumSeedTimeSeconds,
			IMDBID:                 r.IMDBID,
			TMDBID:                 r.TMDBID,
			TVDBID:                 r.TVDBID,
			Year:                   r.Year,
			Genre:                  r.Genre,
			Language:               r.Language,
			Publisher:              r.Publisher,
			Author:                 r.Author,
			Album:                  r.Album,
			Artist:                 r.Artist,
		}
		if name, ok := names[r.IndexerID]; ok {
			dto.IndexerName = name
		}
		if r.PublishedAt != nil {
			s := time.UnixMilli(*r.PublishedAt).UTC().Format(time.RFC3339)
			dto.PublishedAt = &s
		}
		if r.CategoryIDsJSON != nil {
			var ids []int
			if err := json.Unmarshal([]byte(*r.CategoryIDsJSON), &ids); err == nil {
				dto.CategoryIDs = ids
			}
		}
		out = append(out, dto)
	}
	return out
}
