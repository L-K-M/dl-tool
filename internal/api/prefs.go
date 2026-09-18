package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/L-K-M/dl-tool/internal/store"
)

// maxPrefsBodyBytes caps the stored preference document at 64 KiB, the plan's
// size contract in docs/05-api-contract.md section 11.4.
const maxPrefsBodyBytes int64 = 64 << 10

// PrefsBody is the whole preference document. The server models it as an
// open JSON object: members it does not know are stored and returned
// verbatim, so a SPA change can add a preference without a server change.
// version is owned by the SPA; the server does not inspect it
// (docs/05-api-contract.md section 11.4).
type PrefsBody map[string]any

// PrefsOutput returns the stored document or an empty object when the
// account has none; the SPA owns the defaults it patches over
// (docs/09-web-ui-spec.md section 3.3).
type PrefsOutput struct {
	Body PrefsBody
}

// PutPrefsInput replaces the document wholesale. A non-object body fails
// Huma's decode into the map with 422 before the handler runs.
type PutPrefsInput struct {
	Body PrefsBody
}

// prefsHandler reads and replaces the authenticated account's ui_prefs rows,
// one row per top-level member of the document (docs/04-data-model.md
// section 3.6).
type prefsHandler struct {
	settings *store.SettingsStore
}

// Get returns the assembled document for the caller's user_id.
func (h *prefsHandler) Get(ctx context.Context, _ *struct{}) (*PrefsOutput, error) {
	identity, ok := IdentityFrom(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("authentication required")
	}

	doc, err := h.settings.Prefs(ctx, identity.User.ID)
	if err != nil {
		return nil, internalProblem()
	}

	return &PrefsOutput{Body: doc}, nil
}

// Put replaces the caller's document with the request body.
func (h *prefsHandler) Put(ctx context.Context, input *PutPrefsInput) (*PrefsOutput, error) {
	identity, ok := IdentityFrom(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("authentication required")
	}
	if input.Body == nil {
		// A JSON null decodes into a nil map without a decode error, but it
		// is not an object.
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "the preference document must be a JSON object")
	}

	if err := h.settings.PutPrefs(ctx, identity.User.ID, input.Body); err != nil {
		return nil, internalProblem()
	}

	return &PrefsOutput{Body: input.Body}, nil
}

// registerPrefsOperations mounts GET /prefs and PUT /prefs under the auth
// group.
func (s *Server) registerPrefsOperations() {
	handler := &prefsHandler{settings: store.NewSettingsStore(s.db)}

	huma.Register(s.API, huma.Operation{
		OperationID:                  "prefs-get",
		Method:                       http.MethodGet,
		Path:                         "/prefs",
		Summary:                      "Return the stored preference document",
		Tags:                         []string{"ui-prefs"},
		Security:                     credentialRequired,
		RejectUnknownQueryParameters: true,
		DefaultStatus:                http.StatusOK,
	}, handler.Get)

	huma.Register(s.API, huma.Operation{
		OperationID:                  "prefs-put",
		Method:                       http.MethodPut,
		Path:                         "/prefs",
		Summary:                      "Replace the stored preference document",
		Tags:                         []string{"ui-prefs"},
		Security:                     credentialRequired,
		RejectUnknownQueryParameters: true,
		DefaultStatus:                http.StatusOK,
		// Huma reports a body that fills the limit as too large, so the cap
		// is one byte above it for a 64 KiB document to stay legal.
		MaxBodyBytes: maxPrefsBodyBytes + 1,
	}, handler.Put)
}
