package api

import (
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/L-K-M/dl-tool/internal/store"
)

// Slugs registered by docs/05-api-contract.md section 1.3. No handler may invent another.
const (
	SlugSetupRequired        = "/problems/setup-required"
	SlugUnauthenticated      = "/problems/unauthenticated"
	SlugForbidden            = "/problems/forbidden"
	SlugConfigLocked         = "/problems/config-locked"
	SlugCSRFTokenMissing     = "/problems/csrf-token-missing"
	SlugPathRejected         = "/problems/path-rejected"
	SlugSSRFBlocked          = "/problems/ssrf-blocked"
	SlugNotFound             = "/problems/not-found"
	SlugConflict             = "/problems/conflict"
	SlugConcurrencyLimit     = "/problems/concurrency-limit"
	SlugSetupAlreadyComplete = "/problems/setup-already-complete"
	SlugPayloadTooLarge      = "/problems/payload-too-large"
	SlugUnsupportedMediaType = "/problems/unsupported-media-type"
	SlugValidationFailed     = "/problems/validation-failed"
	SlugUnsupportedScheme    = "/problems/unsupported-scheme"
	SlugRateLimited          = "/problems/rate-limited"
	SlugInternal             = "/problems/internal"
	SlugEngineUnavailable    = "/problems/engine-unavailable"
	SlugNotReady             = "/problems/not-ready"
)

// Problem returns an RFC 9457 error carrying one of the registered slugs.
// slug is the value of the response "type" member, for example "/problems/not-found".
func Problem(slug string, status int, detail string) error {
	return &huma.ErrorModel{
		Type:   slug,
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
	}
}

// FromStore maps a store sentinel to a problem. The detail stays generic: the
// wire never carries a secret or an out-of-root path.
func FromStore(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return Problem(SlugNotFound, http.StatusNotFound, "the addressed row does not exist")
	}

	return Problem(SlugInternal, http.StatusInternalServerError, "an internal error occurred")
}

// installErrorFactory routes every Huma-generated error (validation, content
// negotiation, unreadable bodies) through the slug registry, so no error
// response leaves the API without a registered "type" member.
func installErrorFactory() {
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		return &huma.ErrorModel{
			Type:   slugForStatus(status),
			Title:  http.StatusText(status),
			Status: status,
			Detail: msg,
			Errors: errorDetails(errs),
		}
	}
}

// slugForStatus maps an HTTP status to its registry slug. Statuses the
// registry has no entry for collapse onto the nearest registered shape of
// their class (4xx → validation-failed, 5xx → internal) instead of
// inventing a slug.
func slugForStatus(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return SlugValidationFailed
	case http.StatusUnauthorized:
		return SlugUnauthenticated
	case http.StatusForbidden:
		return SlugForbidden
	case http.StatusNotFound:
		return SlugNotFound
	case http.StatusConflict:
		return SlugConflict
	case http.StatusRequestEntityTooLarge:
		return SlugPayloadTooLarge
	case http.StatusUnsupportedMediaType:
		return SlugUnsupportedMediaType
	case http.StatusTooManyRequests:
		return SlugRateLimited
	case http.StatusServiceUnavailable:
		return SlugEngineUnavailable
	default:
		if status >= http.StatusBadRequest && status < http.StatusInternalServerError {
			return SlugValidationFailed
		}

		return SlugInternal
	}
}

// errorDetails keeps field explanations, never submitted values. Even an
// unrelated unknown property can make Huma attach the entire credential body.
func errorDetails(errs []error) []*huma.ErrorDetail {
	details := make([]*huma.ErrorDetail, 0, len(errs))
	for _, err := range errs {
		var detailer huma.ErrorDetailer
		switch {
		case errors.As(err, &detailer):
			original := detailer.ErrorDetail()
			if original == nil {
				continue
			}
			detail := *original
			detail.Value = nil
			details = append(details, &detail)
		case err != nil:
			details = append(details, &huma.ErrorDetail{Message: err.Error()})
		}
	}

	return details
}
