// GET /prefs and PUT /prefs serve the single UI preference document of
// docs/05-api-contract.md section 11.4: an open JSON object whose top-level
// members each live in one ui_prefs row (docs/04-data-model.md section 3.6).
// The server stores and returns members it does not model verbatim, so the
// SPA can add a preference without a server change.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/store"
)

// maxPrefsBodyBytes caps the stored preference document at 64 KiB, the plan's
// size contract in docs/05-api-contract.md section 11.4.
const maxPrefsBodyBytes int64 = 64 << 10

// PrefsBody is one preference document. The server stores unknown members
// verbatim and returns them unchanged, so the SPA can add a preference
// without a server change — except that numbers decode as float64, so
// integer members beyond 2^53 do not survive a round trip exactly. version
// is an integer the SPA owns; the server never inspects it. There is no
// PATCH: PUT replaces wholesale.
type PrefsBody map[string]any

// PrefsOutput returns the stored document or an empty object when the
// account has none; the SPA owns the defaults it patches over
// (docs/09-web-ui-spec.md section 3.3).
type PrefsOutput struct {
	Body PrefsBody
}

// PutPrefsInput replaces the document wholesale. The operation's middleware
// has already bounded the body to 64 KiB and required a JSON object top
// level before Huma decodes it.
type PutPrefsInput struct {
	Body PrefsBody
}

// PrefsHandlers serves the ui_prefs document, one row per top-level member
// keyed (user_id, key).
type PrefsHandlers struct {
	Store *store.SettingsStore
}

// NewPrefsHandlers builds the preference handlers over db, like
// NewCategoryHandlers.
func NewPrefsHandlers(db *sqlx.DB) *PrefsHandlers {
	return &PrefsHandlers{Store: store.NewSettingsStore(db)}
}

// Get returns the assembled document for the caller's user_id.
func (h *PrefsHandlers) Get(ctx context.Context, _ *struct{}) (*PrefsOutput, error) {
	identity, ok := IdentityFrom(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("authentication required")
	}

	doc, err := h.Store.Prefs(ctx, identity.User.ID)
	if err != nil {
		return nil, internalProblem()
	}

	return &PrefsOutput{Body: doc}, nil
}

// Put replaces the caller's document with the request body.
func (h *PrefsHandlers) Put(ctx context.Context, input *PutPrefsInput) (*PrefsOutput, error) {
	identity, ok := IdentityFrom(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("authentication required")
	}

	if err := h.Store.PutPrefs(ctx, identity.User.ID, input.Body); err != nil {
		return nil, internalProblem()
	}

	return &PrefsOutput{Body: input.Body}, nil
}

// Register mounts GET /prefs and PUT /prefs on the Huma API, the
// sibling-handler pattern CategoryHandlers.Register uses.
func (h *PrefsHandlers) Register(hapi huma.API) {
	huma.Register(hapi, huma.Operation{
		OperationID:                  "prefs-get",
		Method:                       http.MethodGet,
		Path:                         "/prefs",
		Summary:                      "Return the stored preference document",
		Description:                  "The account's whole UI preference document, or an empty object when none was stored; the SPA owns the defaults it patches over. Members the server does not model come back verbatim.",
		Tags:                         []string{"ui-prefs"},
		Security:                     credentialRequired,
		RejectUnknownQueryParameters: true,
		DefaultStatus:                http.StatusOK,
	}, h.Get)

	huma.Register(hapi, huma.Operation{
		OperationID:                  "prefs-put",
		Method:                       http.MethodPut,
		Path:                         "/prefs",
		Summary:                      "Replace the stored preference document",
		Description:                  "Replaces the account's whole UI preference document with the request body, which must be a JSON object of at most 64 KiB. Members the server does not model are stored verbatim.",
		Tags:                         []string{"ui-prefs"},
		Security:                     credentialRequired,
		RejectUnknownQueryParameters: true,
		DefaultStatus:                http.StatusOK,
		Middlewares:                  huma.Middlewares{limitPrefsBody},
	}, h.Put)
}

// limitPrefsBody is the PUT operation's middleware, like acceptSubmissionForm:
// it bounds the raw body to 64 KiB with http.MaxBytesReader and requires the
// top level to decode as a JSON object, then re-attaches the body so Huma's
// ordinary JSON path parses it. The rejections of doc 05 section 11.4 are
// answered here, before the handler or the store runs.
func limitPrefsBody(ctx huma.Context, next func(huma.Context)) {
	// humachi is the only adapter this server is built on. Unwrap yields the
	// live request and writer: the re-attach below rewrites the same
	// *http.Request the inner handler reads, and the writer answers the
	// capped and non-object cases.
	r, w := humachi.Unwrap(ctx)

	r.Body = http.MaxBytesReader(w, r.Body, maxPrefsBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeProblem(w, Problem(
			SlugPayloadTooLarge,
			http.StatusRequestEntityTooLarge,
			"the preference document exceeds 65536 bytes",
		))

		return
	}

	trimmed := bytes.TrimSpace(body)
	var probe map[string]any
	if len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(trimmed, &probe) != nil {
		writeProblem(w, Problem(
			SlugValidationFailed,
			http.StatusUnprocessableEntity,
			"the preference document must be a JSON object",
		))

		return
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	next(ctx)
}
