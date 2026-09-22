package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

const (
	operationListTokens  = "list-api-tokens"
	operationCreateToken = "create-api-token"
	operationRevokeToken = "revoke-api-token"

	// apiTokenSecretBytes is the entropy behind a minted API token: 16 bytes
	// from crypto/rand render as the 32 hex characters of the "dlt_" value
	// (docs/05-api-contract.md section 12) — the 128-bit floor of
	// docs/12-security-and-threat-model.md section 6.1.
	apiTokenSecretBytes = 16

	staleTokenCursorDetail = "the cursor is not a page token of this list"
)

// TokenView is the only shape ever returned for an existing token. There is no secret member.
type TokenView struct {
	ID         string     `json:"id"          doc:"The tok_ id of the token"`
	Name       string     `json:"name"        doc:"Display label"`
	Prefix     string     `json:"prefix"      doc:"First 8 characters of the token value, display only"`
	LastUsedAt *time.Time `json:"last_used_at" doc:"Last authenticated request, or null"`
	ExpiresAt  *time.Time `json:"expires_at"   doc:"RFC 3339 expiry, or null when the token never expires"`
	CreatedAt  time.Time  `json:"created_at"`
}

// ListTokensInput is the query of GET /api-tokens (docs/05-api-contract.md
// sections 1.4 and 12).
type ListTokensInput struct {
	Limit  int    `query:"limit"  minimum:"1" maximum:"500" default:"100" doc:"Page size"`
	Cursor string `query:"cursor" doc:"Opaque page token from a previous response"`
}

// CreateTokenInput is the JSON body of POST /api-tokens.
type CreateTokenInput struct {
	Body struct {
		Name      string     `json:"name"       required:"true" minLength:"1" maxLength:"256" doc:"Display label shown in the token list"`
		ExpiresAt *time.Time `json:"expires_at,omitempty" nullable:"true" doc:"RFC 3339 expiry; omit or send null for a token that never expires"`
	}
}

// RevokeTokenInput addresses one token by id.
type RevokeTokenInput struct {
	ID string `path:"id" doc:"The tok_ id of the token"`
}

// CreateTokenOutput is the ONLY response that ever carries Token. The value
// is not stored in clear text, is never logged, and is never returned again
// by any endpoint.
type CreateTokenOutput struct {
	Body struct {
		TokenView
		Token string `json:"token" doc:"The bearer token, revealed exactly once: dlt_ plus 32 hex characters"`
	}
}

// ListTokensOutput is the cursor pagination envelope of doc 05 section 1.4.
type ListTokensOutput struct {
	Body struct {
		Items      []TokenView `json:"items"       doc:"One page of live tokens, newest first; never carries a token value"`
		NextCursor *string     `json:"next_cursor" doc:"Token for the next page; null on the last page"`
		Total      int         `json:"total"       doc:"Live tokens of the account, ignoring the cursor"`
	}
}

// TokenHandlers owns the three /api-tokens operations of doc 05 section 12.
type TokenHandlers struct {
	users *store.UserStore
}

// NewTokenHandlers builds the token handlers over db, exactly like
// NewCategoryHandlers wraps its own SettingsStore.
func NewTokenHandlers(db *sqlx.DB) *TokenHandlers {
	return &TokenHandlers{users: store.NewUserStore(db)}
}

// Register mounts the three operations on the Huma API;
// Server.registerOperations is the call site.
func (h *TokenHandlers) Register(hapi huma.API) {
	huma.Register(hapi, huma.Operation{
		OperationID: operationListTokens,
		Method:      http.MethodGet,
		Path:        "/api-tokens",
		Summary:     "List the API tokens",
		Description: "Every live token of the operator account, cursor-paginated newest first. Items carry id, name, prefix and timestamps only — a token value is never returned again after creation. Revoked tokens are the audit trail and do not list.",
		Tags:        []string{"account"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.List)

	huma.Register(hapi, huma.Operation{
		OperationID:   operationCreateToken,
		Method:        http.MethodPost,
		Path:          "/api-tokens",
		DefaultStatus: http.StatusCreated,
		Summary:       "Issue an API token",
		Description:   "Mints a bearer token and returns its value exactly once; only the SHA-256 hash and the 8-character prefix are stored. The value is never logged and no later response repeats it.",
		Tags:          []string{"account"},
		Security:      credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Create)

	huma.Register(hapi, huma.Operation{
		OperationID:   operationRevokeToken,
		Method:        http.MethodDelete,
		Path:          "/api-tokens/{id}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Revoke an API token",
		Description:   "Sets revoked_at immediately, so the next request carrying the token answers 401. The row is kept as the audit trail; an unknown or already-revoked id is 404 /problems/not-found.",
		Tags:          []string{"account"},
		Security:      credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Revoke)
}

// List serves GET /api-tokens.
func (h *TokenHandlers) List(ctx context.Context, in *ListTokensInput) (*ListTokensOutput, error) {
	identity, ok := IdentityFrom(ctx)
	if !ok {
		// The middleware guarantees a credential; this guards wiring.
		return nil, unauthenticated(noCredentialDetail)
	}

	rows, nextCursor, total, err := h.users.ListAPITokens(ctx, identity.User.ID, in.Limit, in.Cursor)
	if err != nil {
		if errors.Is(err, store.ErrStaleCursor) {
			return nil, staleTokenCursorProblem()
		}

		return nil, internalFailure(ctx, "list api tokens", err)
	}

	output := &ListTokensOutput{}
	output.Body.Items = make([]TokenView, 0, len(rows))
	for _, row := range rows {
		output.Body.Items = append(output.Body.Items, tokenView(row))
	}
	output.Body.Total = total
	if nextCursor != "" {
		output.Body.NextCursor = &nextCursor
	}

	return output, nil
}

// Create serves POST /api-tokens. The minted value is handed to the
// response and nowhere else: the store receives only its hash and prefix,
// and the log records the tok_ id, never the value.
func (h *TokenHandlers) Create(ctx context.Context, in *CreateTokenInput) (*CreateTokenOutput, error) {
	identity, ok := IdentityFrom(ctx)
	if !ok {
		// The middleware guarantees a credential; this guards wiring.
		return nil, unauthenticated(noCredentialDetail)
	}

	value, err := newAPITokenValue()
	if err != nil {
		return nil, internalFailure(ctx, "mint api token", err)
	}

	var expiresAt *int64
	if in.Body.ExpiresAt != nil {
		ms := in.Body.ExpiresAt.UnixMilli()
		expiresAt = &ms
	}

	row := store.APIToken{
		ID:        store.NewID(store.PrefixAPIToken),
		UserID:    identity.User.ID,
		Name:      in.Body.Name,
		TokenHash: secure.HashToken(value),
		Prefix:    value[:8],
		ExpiresAt: expiresAt,
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := h.users.CreateAPIToken(ctx, row); err != nil {
		return nil, internalFailure(ctx, "create api token", err)
	}

	// The id and prefix are safe to log; the value and its hash never are
	// (docs/02-requirements.md NFR-016).
	logFromContext(ctx).Info("api token issued",
		slog.String("token_id", row.ID),
		slog.String("prefix", row.Prefix),
	)

	output := &CreateTokenOutput{}
	output.Body.TokenView = tokenView(row)
	output.Body.Token = value

	return output, nil
}

// Revoke serves DELETE /api-tokens/{id}: the row keeps the audit trail but
// its next bearer use answers 401.
func (h *TokenHandlers) Revoke(ctx context.Context, in *RevokeTokenInput) (*struct{}, error) {
	identity, ok := IdentityFrom(ctx)
	if !ok {
		// The middleware guarantees a credential; this guards wiring.
		return nil, unauthenticated(noCredentialDetail)
	}

	if err := h.users.RevokeAPIToken(ctx, identity.User.ID, in.ID); err != nil {
		return nil, FromStore(err)
	}

	logFromContext(ctx).Info("api token revoked", slog.String("token_id", in.ID))

	return nil, nil
}

// newAPITokenValue mints the bearer secret: apiTokenSecretBytes from
// crypto/rand rendered as "dlt_" plus 32 hex characters — the value the
// creation response reveals exactly once.
func newAPITokenValue() (string, error) {
	buffer := make([]byte, apiTokenSecretBytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("mint api token: %w", err)
	}

	return apiTokenPrefix + hex.EncodeToString(buffer), nil
}

// tokenView renders one store row. The hash never reaches the view — the
// type has no member that could carry it.
func tokenView(t store.APIToken) TokenView {
	return TokenView{
		ID:         t.ID,
		Name:       t.Name,
		Prefix:     t.Prefix,
		LastUsedAt: unixMilliToTime(t.LastUsedAt),
		ExpiresAt:  unixMilliToTime(t.ExpiresAt),
		CreatedAt:  time.UnixMilli(t.CreatedAt).UTC(),
	}
}

// unixMilliToTime renders an optional Unix-millisecond column as a time;
// nil stays nil. It is the time.Time sibling of unixMilliToRFC3339.
func unixMilliToTime(ms *int64) *time.Time {
	if ms == nil {
		return nil
	}

	rendered := time.UnixMilli(*ms).UTC()

	return &rendered
}

// staleTokenCursorProblem maps the store's ErrStaleCursor onto the
// registered validation slug with the offending field located in errors[] —
// the same shape feedItemsProblem gives the feed list.
func staleTokenCursorProblem() error {
	return &huma.ErrorModel{
		Type:   SlugValidationFailed,
		Title:  http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity,
		Detail: staleTokenCursorDetail,
		Errors: []*huma.ErrorDetail{{Message: staleTokenCursorDetail, Location: "query.cursor"}},
	}
}
