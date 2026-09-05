package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/store"
)

const (
	operationListEngines = "list-engines"
	operationTestEngine  = "test-engine"

	// engineProbeDeadline bounds the Health call of POST /engines/{id}/test
	// so a black-holed engine cannot hold the request open. The engine's
	// own per-call timeout is the second bound; whichever fires first.
	engineProbeDeadline = 5 * time.Second

	unknownEngineDetail = "the addressed engine does not exist"
	unregisteredEngine  = "the %s engine is not registered in this process"
)

// EngineDTO is one entry of GET /engines (docs/05-api-contract.md section
// 11.3). Secrets are never present in any form: the store query never
// selects them and no adapter carries one here.
type EngineDTO struct {
	ID           string   `json:"id"             doc:"eng_ aria2 | eng_qbittorrent | eng_ytdlp"`
	Kind         string   `json:"kind"           doc:"aria2 | qbittorrent | ytdlp" enum:"aria2,qbittorrent,ytdlp"`
	Name         string   `json:"name"`
	Enabled      bool     `json:"enabled"`
	URL          *string  `json:"url"            doc:"The engine's RPC or base URL; null for yt-dlp"`
	Connected    bool     `json:"connected"      doc:"True when the last recorded probe succeeded and its engine is registered"`
	Version      *string  `json:"version"`
	Capabilities []string `json:"capabilities"  doc:"The declared set of the engine's adapter, never a guess"`
	LastSeenAt   *string  `json:"last_seen_at"   format:"date-time" doc:"RFC 3339 UTC of the last successful probe"`
	LastError    *string  `json:"last_error"`
}

// ListEnginesOutput is the GET /engines body.
type ListEnginesOutput struct {
	Body struct {
		Engines []EngineDTO `json:"engines"`
	}
}

// TestEngineInput addresses one engine by id.
type TestEngineInput struct {
	ID string `path:"id" doc:"The eng_ id of the engine"`
}

// TestEngineOutput is 200 whether or not the probe succeeded: a failed
// probe is a result, not an error, so the UI can render the transport
// error inline.
type TestEngineOutput struct {
	Body struct {
		Ok        bool    `json:"ok"`
		Version   *string `json:"version"`
		ElapsedMS int64   `json:"elapsed_ms" doc:"Probe duration in milliseconds; at least 1"`
		Error     *string `json:"error"      doc:"The transport error when the probe failed"`
	}
}

// SettingsHandlers owns the settings-adjacent operations of doc 05 section
// 11: the engines list and probe here; GET/PATCH /settings arrive with
// T092 on this same struct.
type SettingsHandlers struct {
	settings *store.SettingsStore
	engines  *engine.Registry
}

// NewSettingsHandlers builds the settings handlers. db is the store the
// engines rows live in; engines is the routing-time registry the probe
// resolves each row's engine through — the same instance NewServer hands
// the task handlers, so a test registering a stand-in reaches both.
func NewSettingsHandlers(db *sqlx.DB, engines *engine.Registry) *SettingsHandlers {
	return &SettingsHandlers{settings: store.NewSettingsStore(db), engines: engines}
}

// registerOperations mounts list-engines and test-engine on the Huma API;
// Server.registerOperations is the call site.
func (h *SettingsHandlers) registerOperations(hapi huma.API) {
	huma.Register(hapi, huma.Operation{
		OperationID: operationListEngines,
		Method:      http.MethodGet,
		Path:        "/engines",
		Summary:     "List engines and their connection state",
		Description: "Every configured engine with its declared capabilities, last probe outcome and resolved version. The list never probes: a dead engine cannot slow the page down, so connected reflects the last recorded probe, not a live dial.",
		Tags:        []string{"engines"},
		Security:    credentialRequired,
	}, h.ListEngines)

	huma.Register(hapi, huma.Operation{
		OperationID: operationTestEngine,
		Method:      http.MethodPost,
		Path:        "/engines/{id}/test",
		Summary:     "Probe one engine",
		Description: "Runs a bounded Health call against the engine and records the outcome. A failed probe is still 200 with ok false and the transport error in error; only an unknown engine id is 404.",
		Tags:        []string{"engines"},
		Security:    credentialRequired,
	}, h.TestEngine)
}

// ListEngines serves GET /engines: one entry per engines row, with the
// capabilities of the row's registered adapter. Connected comes from the
// stored probe history — last_seen_at set and last_error cleared by a
// success — plus the engine being registered in this process, so a row
// left behind by a disabled lane never renders as reachable.
func (h *SettingsHandlers) ListEngines(ctx context.Context, _ *struct{}) (*ListEnginesOutput, error) {
	rows, err := h.settings.ListEngines(ctx)
	if err != nil {
		return nil, internalFailure(ctx, "list engines", err)
	}

	output := &ListEnginesOutput{}
	output.Body.Engines = make([]EngineDTO, 0, len(rows))
	for _, row := range rows {
		output.Body.Engines = append(output.Body.Engines, h.renderEngine(row))
	}

	return output, nil
}

// renderEngine merges one stored row with its registered adapter: the row
// owns identity and probe history, the adapter owns the declared
// capabilities. A row whose engine is not registered still lists — with
// empty capabilities and connected false.
func (h *SettingsHandlers) renderEngine(row store.Engine) EngineDTO {
	e, registered := h.engines.Get(row.Kind)

	dto := EngineDTO{
		ID:         row.ID,
		Kind:       row.Kind,
		Name:       row.Name,
		Enabled:    row.Enabled == 1,
		URL:        row.URL,
		Version:    row.Version,
		LastSeenAt: unixMilliToRFC3339(row.LastSeenAt),
		LastError:  row.LastError,
	}
	if registered {
		declared := e.Capabilities()
		capabilities := make([]string, 0, len(declared))
		for _, capability := range declared {
			capabilities = append(capabilities, string(capability))
		}
		dto.Capabilities = capabilities
		// A cleared last_error with a stamped last_seen_at is a recorded
		// success (TouchEngine writes them as one).
		dto.Connected = row.LastSeenAt != nil && row.LastError == nil
	}
	if dto.Capabilities == nil {
		dto.Capabilities = []string{}
	}

	return dto
}

// TestEngine serves POST /engines/{id}/test: one bounded probe of the
// addressed engine, its outcome recorded through TouchEngine so the list
// stays current. A failed probe is a 200 result; only an unknown id is
// 404.
func (h *SettingsHandlers) TestEngine(ctx context.Context, in *TestEngineInput) (*TestEngineOutput, error) {
	row, err := h.settings.EngineByID(ctx, in.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, Problem(SlugNotFound, http.StatusNotFound, unknownEngineDetail)
		}

		return nil, internalFailure(ctx, "resolve engine", err)
	}

	output := &TestEngineOutput{}
	started := time.Now()

	e, registered := h.engines.Get(row.Kind)
	if !registered {
		detail := fmt.Sprintf(unregisteredEngine, row.Kind)
		output.Body.Error = &detail
	} else {
		probeCtx, cancel := context.WithTimeout(ctx, engineProbeDeadline)
		version, healthErr := e.Health(probeCtx)
		cancel()

		if healthErr != nil {
			detail := healthErr.Error()
			output.Body.Error = &detail
		} else {
			output.Body.Ok = true
			output.Body.Version = &version
		}
	}
	output.Body.ElapsedMS = elapsedMillis(time.Since(started))

	// Record the outcome whatever it was, so last_seen_at, version and
	// last_error stay current for the list endpoint.
	var versionArg, lastErrArg *string
	if output.Body.Ok {
		versionArg = output.Body.Version
	} else {
		lastErrArg = output.Body.Error
	}
	if err := h.settings.TouchEngine(ctx, row.ID, versionArg, lastErrArg, time.Now().UnixMilli()); err != nil {
		return nil, internalFailure(ctx, "record probe", err)
	}

	return output, nil
}

// elapsedMillis reports a probe duration in whole milliseconds, rounded up
// so even an in-process stand-in that answers in microseconds reports 1
// rather than a zero the caller would read as unmeasured.
func elapsedMillis(d time.Duration) int64 {
	if d < time.Millisecond {
		return 1
	}

	return d.Milliseconds()
}

// unixMilliToRFC3339 renders an optional Unix-millisecond column as an
// RFC 3339 UTC string; nil stays nil.
func unixMilliToRFC3339(ms *int64) *string {
	if ms == nil {
		return nil
	}

	rendered := time.UnixMilli(*ms).UTC().Format(time.RFC3339)

	return &rendered
}
