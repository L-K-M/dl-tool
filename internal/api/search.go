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
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.yaml.in/yaml/v3"

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
}

// Deps carries the process-wide search collaborators, built exactly once in
// cmd/dl-tool/main.go and passed into NewServer, so the API and the job
// worker share one of each (docs/14-conventions.md section 8.3). Runner is
// the dlsearch evaluator — its per-engine rate buckets are process state, so
// the probe path and the search-job path must share the one instance.
type Deps struct {
	Indexers *store.IndexerStore
	Defs     *search.Registry
	Runner   *search.Runner
	HTTP     *http.Client // the SSRF-guarded client of T123
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
// multipart/form-data is a file upload — .dlm and .dlsearch.yaml land in
// T059, .py in T060.
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
					Description: "A .dlsearch.yaml or .dlm engine file upload in one file part, at most 1 MiB; .py lands with T060.",
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
}

// NewSearchHandlers builds the indexer handlers over the shared deps.
func NewSearchHandlers(log *slog.Logger, d Deps) *SearchHandlers {
	return &SearchHandlers{log: log, indexers: d.Indexers, defs: d.Defs, runner: d.Runner, hc: d.HTTP}
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
		Description:   "With an application/json body {torznab_url, api_key} this is the provider wizard: it enumerates a Prowlarr or Jackett instance and creates one disabled row per upstream indexer. multipart/form-data carries one file part — a .dlsearch.yaml or .dlm import, .py with T060; every row it creates is disabled.",
		Tags:          []string{"indexers"},
		Security:      credentialRequired,
		RequestBody:   importRequestBody(),
		// Multipart framing adds bytes around the file part, so the
		// body cap must clear the 1 MiB part cap, not merely match it.
		MaxBodyBytes: max(secure.MetadataFetchCap, maxImportFileBytes+4096),
	}, h.ImportIndexer)
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
// allow_private_network. A stored document that is not valid JSON is treated
// as empty, matching mergeIndexerSettings.
func indexerSettingsMap(log *slog.Logger, row store.Indexer) map[string]string {
	out := map[string]string{}
	if row.SettingsJSON == nil || *row.SettingsJSON == "" {
		return out
	}
	var doc map[string]any
	if !json.Valid([]byte(*row.SettingsJSON)) {
		log.Warn("indexer settings_json is not valid JSON; probing with empty settings", "indexer_id", row.ID)
		return out
	}
	dec := json.NewDecoder(strings.NewReader(*row.SettingsJSON))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		log.Warn("indexer settings_json is not valid JSON; probing with empty settings", "indexer_id", row.ID)
		return out
	}
	for k, v := range doc {
		switch t := v.(type) {
		case string:
			out[k] = t
		case bool:
			out[k] = strconv.FormatBool(t)
		case json.Number:
			// verbatim — a float64 would render 1e+06 or lose precision
			out[k] = t.String()
		case nil:
			// JSON null carries no value; the key is dropped.
		default:
			out[k] = fmt.Sprint(t)
		}
	}
	return out
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
// picks the importer — .dlm and .dlsearch.yaml here, .py with T060. Every
// other media type is 415.
func (h *SearchHandlers) ImportIndexer(ctx context.Context, in *ImportIndexerInput) (*ImportOutput, error) {
	st, err := h.indexerStore(ctx)
	if err != nil {
		return nil, err
	}
	mediaType, params, err := mime.ParseMediaType(in.ContentType)
	if err != nil {
		return nil, Problem(
			SlugUnsupportedMediaType, http.StatusUnsupportedMediaType,
			"POST /indexers/import accepts application/json {torznab_url, api_key} or multipart/form-data with one .dlm or .dlsearch.yaml file part",
		)
	}
	if mediaType == "multipart/form-data" {
		return h.importIndexerFile(ctx, st, in.RawBody, params)
	}
	if mediaType != "application/json" {
		return nil, Problem(
			SlugUnsupportedMediaType, http.StatusUnsupportedMediaType,
			"POST /indexers/import accepts application/json {torznab_url, api_key} or multipart/form-data with one .dlm or .dlsearch.yaml file part",
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
// .dlsearch.yaml through the definition loader. The created row is always
// disabled and always records where it came from (doc 07 section 7).
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
	default:
		return nil, Problem(
			SlugValidationFailed, http.StatusUnprocessableEntity,
			"the file part must be a .dlm or .dlsearch.yaml upload",
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
	origin := res.Origin
	if len(origin) > 255 {
		origin = origin[:255]
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
