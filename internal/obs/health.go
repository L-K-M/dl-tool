package obs

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
)

const (
	jsonContentType     = "application/json"
	problemContentType  = "application/problem+json"
	statusValueOK       = "ok"
	statusValueReady    = "ready"
	problemTypeNotReady = "/problems/not-ready"
	titleNotReady       = "Not ready"
	// detailMigrationsPending answers /readyz before MarkReady has run.
	detailMigrationsPending = "migrations have not completed"
	// detailDatabaseFailed answers /readyz when the readiness probe fails.
	detailDatabaseFailed = "the database did not answer"
	readinessQuery       = `SELECT 1`
	readinessQueryBudget = time.Second
)

// Health tracks readiness. Ready flips exactly once, after migrations
// succeed; cmd/dl-tool calls MarkReady once store.Open has returned.
type Health struct {
	db    *sqlx.DB
	ready atomic.Bool
}

// NewHealth wraps the store handle. db may be nil while the store is not
// open yet: Live never touches it and Ready treats it as not ready.
func NewHealth(db *sqlx.DB) *Health {
	return &Health{db: db}
}

// MarkReady flips the readiness gate exactly once per boot.
func (h *Health) MarkReady() {
	h.ready.Store(true)
}

// healthBody is the JSON of both 200 responses.
type healthBody struct {
	Status string `json:"status"`
}

// notReadyBody is the application/problem+json of the /readyz 503.
type notReadyBody struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// Live answers GET /healthz with 200 {"status":"ok"}. It touches nothing —
// not the database, not an engine — so a degraded dependency can never make
// the container restart (docs/05-api-contract.md section 13.1).
func (h *Health) Live(w http.ResponseWriter, _ *http.Request) {
	writeHealthBody(w, http.StatusOK, healthBody{Status: statusValueOK})
}

// Ready answers GET /readyz with 200 {"status":"ready"} once MarkReady has
// run and SELECT 1 still answers; otherwise 503 application/problem+json
// /problems/not-ready.
func (h *Health) Ready(w http.ResponseWriter, r *http.Request) {
	if !h.ready.Load() {
		writeNotReady(w, detailMigrationsPending)
		return
	}

	// A nil handle means the store never opened; treat it as a failed probe.
	if h.db == nil {
		writeNotReady(w, detailDatabaseFailed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), readinessQueryBudget)
	defer cancel()

	var probe int
	if err := h.db.GetContext(ctx, &probe, readinessQuery); err != nil {
		writeNotReady(w, detailDatabaseFailed)
		return
	}

	writeHealthBody(w, http.StatusOK, healthBody{Status: statusValueReady})
}

// writeHealthBody renders one 200 body; the fixed shape never fails to marshal.
func writeHealthBody(w http.ResponseWriter, status int, body healthBody) {
	encoded, err := json.Marshal(body)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	if _, err := w.Write(encoded); err != nil {
		// The status line is already sent; a write failure means the client went away.
		return
	}
}

// writeNotReady renders the fixed 503 /problems/not-ready document; only the
// detail varies between the pre-migration and probe-failure causes.
func writeNotReady(w http.ResponseWriter, detail string) {
	encoded, err := json.Marshal(notReadyBody{
		Type:   problemTypeNotReady,
		Title:  titleNotReady,
		Status: http.StatusServiceUnavailable,
		Detail: detail,
	})
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", problemContentType)
	w.WriteHeader(http.StatusServiceUnavailable)
	if _, err := w.Write(encoded); err != nil {
		return
	}
}
