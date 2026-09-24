package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

const (
	operationGetAccount   = "get-account"
	operationPatchAccount = "patch-account"

	// The doc 05 section 12 password floor is declared on the schema as
	// minLength, so a short value is a 422 before the handler runs.
	accountWrongCurrentDetail  = "the current password does not match"
	accountEmptyUsernameDetail = "username must not be empty"
	accountEmptyLocaleDetail   = "locale must not be empty"
	accountNeedsCurrentDetail  = "changing the password requires current_password in the same body"
)

// AccountHandlers owns the two /account operations of doc 05 section 12.
// There is exactly one account (ADR-0019): the identity's own user row is
// the whole resource.
type AccountHandlers struct {
	db *sqlx.DB
}

// NewAccountHandlers builds the account handlers over db, exactly like
// NewTokenHandlers wraps its own UserStore.
func NewAccountHandlers(db *sqlx.DB) *AccountHandlers {
	return &AccountHandlers{db: db}
}

// Register mounts the two operations on the Huma API;
// Server.registerOperations is the call site.
func (h *AccountHandlers) Register(hapi huma.API) {
	huma.Register(hapi, huma.Operation{
		OperationID: operationGetAccount,
		Method:      http.MethodGet,
		Path:        "/account",
		Summary:     "Read the operator account",
		Description: "The single operator account: id, username, enabled, locale and timestamps — the same shape GET /auth/me reports as its user member. No password material is ever returned.",
		Tags:        []string{"account"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.GetAccount)

	huma.Register(hapi, huma.Operation{
		OperationID: operationPatchAccount,
		Method:      http.MethodPatch,
		Path:        "/account",
		Summary:     "Update the operator account",
		Description: "Partial update of username, locale and password; omitted fields are untouched. A password change requires current_password in the same body — verified before any write, 403 /problems/forbidden on mismatch — and revokes every session except the caller's; API tokens are unaffected. A password under 12 characters, a username that normalises to the empty string or a locale that trims to the empty string is 422 /problems/validation-failed.",
		Tags:        []string{"account"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.PatchAccount)
}

// GetAccountOutput is the account object of doc 05 section 12, unwrapped —
// the same shape GET /auth/me reports as its user member.
type GetAccountOutput struct {
	Body userBody
}

// PatchAccountInput is the JSON body of PATCH /account. Every member is a
// pointer so an absent key is distinguishable from a zero value; the
// password floor is declared as minLength, which makes a short value a
// 422 before the handler runs.
type PatchAccountInput struct {
	Body struct {
		Username        *string `json:"username,omitempty"         doc:"New account username"`
		Locale          *string `json:"locale,omitempty"           doc:"Preferred UI locale"`
		Password        *string `json:"password,omitempty"         minLength:"12" doc:"New password; requires current_password in the same body"`
		CurrentPassword *string `json:"current_password,omitempty" doc:"Verified before any write when present; required whenever password is present"`
	}
}

// PatchAccountOutput is the account object read back after the writes.
type PatchAccountOutput struct {
	Body userBody
}

// GetAccount returns the account object of doc 05 §12 — the same shape
// GET /auth/me reports as its user member; no password material in either
// direction.
func (h *AccountHandlers) GetAccount(ctx context.Context, _ *struct{}) (*GetAccountOutput, error) {
	identity, ok := IdentityFrom(ctx)
	if !ok {
		// The middleware guarantees a credential; this guards wiring.
		return nil, unauthenticated(noCredentialDetail)
	}

	return &GetAccountOutput{Body: newUserBody(identity.User)}, nil
}

// PatchAccount verifies current_password before any write (403
// /problems/forbidden on mismatch), applies username, locale and
// password_hash, and on a password change revokes every session except the
// caller's — a token-authenticated caller holds no session, so every
// session is revoked. API tokens are unaffected (doc 05 §12).
func (h *AccountHandlers) PatchAccount(ctx context.Context, in *PatchAccountInput) (*PatchAccountOutput, error) {
	identity, ok := IdentityFrom(ctx)
	if !ok {
		// The middleware guarantees a credential; this guards wiring.
		return nil, unauthenticated(noCredentialDetail)
	}

	if in.Body.Password != nil && in.Body.CurrentPassword == nil {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, accountNeedsCurrentDetail)
	}
	if in.Body.CurrentPassword != nil {
		// Verification precedes every write: a mismatched current_password
		// is 403 /problems/forbidden and nothing is stored.
		matches, _, err := secure.VerifyPassword(identity.User.PasswordHash, *in.Body.CurrentPassword)
		if err != nil {
			return nil, internalFailure(ctx, "verify current password", err)
		}
		if !matches {
			return nil, Problem(SlugForbidden, http.StatusForbidden, accountWrongCurrentDetail)
		}
	}

	if in.Body.Username != nil || in.Body.Locale != nil {
		username := identity.User.Username
		if in.Body.Username != nil {
			// Doc 05 §12: a username that already normalises to the empty
			// string is a 422. Trimming is the whole normalisation; the
			// stored value is the trimmed one.
			username = strings.TrimSpace(*in.Body.Username)
			if username == "" {
				return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, accountEmptyUsernameDetail)
			}
		}
		locale := identity.User.Locale
		if in.Body.Locale != nil {
			locale = strings.TrimSpace(*in.Body.Locale)
			if locale == "" {
				return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, accountEmptyLocaleDetail)
			}
		}
		if err := store.UpdateUserProfile(ctx, h.db, identity.User.ID, username, locale); err != nil {
			return nil, FromStore(err)
		}
	}

	if in.Body.Password != nil {
		hash, err := secure.HashPassword(*in.Body.Password)
		if err != nil {
			return nil, internalFailure(ctx, "hash password", err)
		}
		// Revoke before rotating: the store functions each commit their own
		// statement, so this order keeps the dangerous partial state
		// unreachable — a failed hash write leaves the password unchanged
		// with other sessions revoked, while the reverse could store the new
		// hash with an attacker's sessions still valid and turn the retry
		// into a 403. The caller's own session survives either way;
		// token-authenticated callers carry no session id, so every session
		// is revoked. API tokens are unaffected.
		if _, err := store.DeleteOtherSessions(ctx, h.db, identity.User.ID, identity.SessionID); err != nil {
			return nil, internalFailure(ctx, "revoke other sessions", err)
		}
		if err := store.UpdatePasswordHash(ctx, h.db, identity.User.ID, hash); err != nil {
			return nil, FromStore(err)
		}
	}

	updated, err := store.UserByID(ctx, h.db, identity.User.ID)
	if err != nil {
		return nil, internalFailure(ctx, "read back account", err)
	}

	return &PatchAccountOutput{Body: newUserBody(updated)}, nil
}
