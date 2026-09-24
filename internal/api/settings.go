package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/store"
)

const (
	operationListEngines   = "list-engines"
	operationTestEngine    = "test-engine"
	operationGetSettings   = "get-settings"
	operationPatchSettings = "patch-settings"

	// RedactedPlaceholder is the only form a secret takes on the wire
	// (docs/11-config-reference.md section 6).
	RedactedPlaceholder = "__redacted__"

	// engineProbeDeadline bounds the Health call of POST /engines/{id}/test
	// so a black-holed engine cannot hold the request open. The engine's
	// own per-call timeout is the second bound; whichever fires first.
	engineProbeDeadline = 5 * time.Second

	unknownEngineDetail = "the addressed engine does not exist"
	unregisteredEngine  = "the %s engine is not registered in this process"

	// A conformance summary is distinct from a health failure in last_error.
	conformancePrefix = "conformance: "
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

// SettingsBody is the flat settings document of doc 05 section 11.1 —
// exactly the fifteen keys of doc 11 section 5. ExtractPasswords is typed
// string because the wire value is always the literal "__redacted__",
// never the stored list.
type SettingsBody struct {
	DownloadRateLimit  int64            `json:"download_rate_limit"     doc:"Bytes/s cap, 0 = unlimited"`
	UploadRateLimit    int64            `json:"upload_rate_limit"       doc:"Bytes/s cap, 0 = unlimited"`
	AltDownloadRate    int64            `json:"alt_download_rate_limit" doc:"Bytes/s cap while the alternative cell is in force"`
	AltUploadRate      int64            `json:"alt_upload_rate_limit"   doc:"Bytes/s cap while the alternative cell is in force"`
	ScheduleEnabled    bool             `json:"schedule_enabled"`
	DefaultDestination string           `json:"default_destination"     doc:"Absolute path inside a data root"`
	MinFreeSpace       map[string]int64 `json:"min_free_space"          doc:"Sparse map of absolute root path to bytes; roots absent fall back to the documented default"`
	MaxActiveTotal     int              `json:"max_active_total"        doc:"0 = unlimited"`
	MaxActivePerEngine int              `json:"max_active_per_engine"   doc:"0 = unlimited"`
	ProcessOrder       string           `json:"process_order"           enum:"by_date_created"`
	RSSEnabled         bool             `json:"rss_enabled"`
	RSSIntervalS       int              `json:"rss_interval_s"          doc:"Seconds, minimum 300"`
	AutoExtract        bool             `json:"auto_extract"`
	ExtractPasswords   string           `json:"extract_passwords"       enum:"__redacted__" doc:"Always the redaction placeholder; PATCHing it back is a no-op"`
	ConfirmOnDelete    bool             `json:"confirm_on_delete"`
}

// GetSettingsOutput carries the flat settings object of doc 05 section
// 11.1.
type GetSettingsOutput struct{ Body SettingsBody }

// SettingsPatch is the PATCH /settings body: a subset of the documented
// keys, each member's raw JSON kept intact so the store can enforce the
// bare-integer grammar (`4`, never `"4"` or `4.0`) and distinguish an
// absent member from an explicit null — both shapes a decoded map would
// erase. Schema declares the fifteen members so generated clients get a
// typed contract while the wire handling keeps the raw bytes.
type SettingsPatch map[string]json.RawMessage

// Schema implements huma.SchemaProvider: the declared object names the
// fifteen documented properties with each member's documented type, so
// generated clients get a typed contract. Huma-side rejections still
// surface as 422 /problems/validation-failed through installErrorFactory,
// and the members that huma cannot express — canonical path keys, the
// bare-integer grammar, the absent-vs-null distinction — keep their raw
// bytes for the store's own checks. AdditionalProperties stays open so an
// unknown key reaches PutSettings and returns the documented 422.
func (SettingsPatch) Schema(r huma.Registry) *huma.Schema {
	minimum := func(v float64) *float64 { return &v }
	nonNegativeInt := &huma.Schema{Type: huma.TypeInteger, Minimum: minimum(0)}
	boolean := &huma.Schema{Type: huma.TypeBoolean}
	str := &huma.Schema{Type: huma.TypeString}
	memberTypes := map[string]*huma.Schema{
		"download_rate_limit":     nonNegativeInt,
		"upload_rate_limit":       nonNegativeInt,
		"alt_download_rate_limit": nonNegativeInt,
		"alt_upload_rate_limit":   nonNegativeInt,
		"schedule_enabled":        boolean,
		"default_destination":     str,
		"min_free_space": {
			Type:                 huma.TypeObject,
			AdditionalProperties: &huma.Schema{Type: huma.TypeInteger, Minimum: minimum(0)},
		},
		"max_active_total":      nonNegativeInt,
		"max_active_per_engine": nonNegativeInt,
		"process_order":         {Type: huma.TypeString, Enum: []any{"by_date_created"}},
		"rss_enabled":           boolean,
		"rss_interval_s":        {Type: huma.TypeInteger, Minimum: minimum(300)},
		"auto_extract":          boolean,
		"extract_passwords": {OneOf: []*huma.Schema{
			{Type: huma.TypeString, Enum: []any{RedactedPlaceholder}},
			{Type: huma.TypeArray, Items: str},
		}},
		"confirm_on_delete": boolean,
	}

	// The property names come from the store's canonical list, so a key
	// added there can never silently vanish from the contract — it would
	// surface as an opaque member here rather than disappearing.
	props := make(map[string]*huma.Schema, len(memberTypes))
	for _, key := range store.SettingsKeys() {
		props[key] = memberTypes[key]
		if props[key] == nil {
			props[key] = &huma.Schema{}
		}
	}
	return &huma.Schema{
		Type:       huma.TypeObject,
		Properties: props,
	}
}

// PatchSettingsInput carries the subset of keys PATCH /settings writes.
type PatchSettingsInput struct {
	Body SettingsPatch
}

// SettingsHandlers owns the settings-adjacent operations of doc 05 section
// 11: the engines list and probe here, the settings document, and the
// schedule grid in settings_schedule.go.
type SettingsHandlers struct {
	settings   *store.SettingsStore
	engines    *engine.Registry
	loadPolicy func(context.Context) (engine.Policy, error)
	// roots are the configured data roots; the first one renders the
	// documented default of default_destination while the row is unset
	// (docs/11-config-reference.md section 5) and the whole list bounds
	// what PATCH accepts for that key.
	roots []string
}

// NewSettingsHandlers builds the settings handlers. db is the store the
// engines rows live in; engines is the routing-time registry the probe
// resolves each row's engine through — the same instance NewServer hands
// the task handlers, so a test registering a stand-in reaches both. roots
// is variadic only so the conformance suite's two-argument call keeps
// working — the first slice is used and the rest ignored; NewServer
// passes cfg.DataRoots.
func NewSettingsHandlers(db *sqlx.DB, engines *engine.Registry, roots ...[]string) *SettingsHandlers {
	h := &SettingsHandlers{settings: store.NewSettingsStore(db), engines: engines, loadPolicy: admissionPolicyLoader(db, nil)}
	if len(roots) > 0 {
		h.roots = roots[0]
	}
	return h
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

	huma.Register(hapi, huma.Operation{
		OperationID: operationGetSettings,
		Method:      http.MethodGet,
		Path:        "/settings",
		Summary:     "Read the settings",
		Description: "Every user-changeable setting as one flat object — exactly the fifteen keys of the config reference, with extract_passwords rendered as \"__redacted__\" and min_free_space as the stored sparse map ({} while unset).",
		Tags:        []string{"settings"},
		Security:    credentialRequired,
	}, h.GetSettings)

	huma.Register(hapi, huma.Operation{
		OperationID: operationPatchSettings,
		Method:      http.MethodPatch,
		Path:        "/settings",
		Summary:     "Update settings",
		Description: "Accepts any subset of the settings keys; each supplied member replaces its stored value wholesale, so a min_free_space patch replaces the whole map. An extract_passwords member equal to \"__redacted__\" is a no-op; the placeholder is invalid for every other key. 422 /problems/validation-failed for an unknown key, a malformed shape or an out-of-range value.",
		Tags:        []string{"settings"},
		Security:    credentialRequired,
	}, h.PatchSettings)
}

// ListEngines serves GET /engines: one entry per engines row, with the
// capabilities of the row's registered adapter. Connected comes from the
// stored probe history, distinguishing conformance warnings from failed
// health probes, plus the engine being registered in this process, so a row
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
	}
	dto.Connected = engineConnected(row, registered)
	if dto.Capabilities == nil {
		dto.Capabilities = []string{}
	}

	return dto
}

// engineConnected is the shared connectivity rule of GET /engines and
// GET /system/info: a row reports connected when its engine is registered
// in this process and the last recorded probe succeeded. A successful
// health probe may still carry competing-automation conformance warnings
// in last_error — those preserve health.
func engineConnected(row store.Engine, registered bool) bool {
	return registered && row.LastSeenAt != nil &&
		(row.LastError == nil || strings.HasPrefix(*row.LastError, conformancePrefix))
}

// GetSettings serves GET /settings: the flat settings document of doc 05
// section 11.1 with extract_passwords rendered as "__redacted__".
func (h *SettingsHandlers) GetSettings(ctx context.Context, _ *struct{}) (*GetSettingsOutput, error) {
	settings, err := h.settings.GetSettings(ctx)
	if err != nil {
		return nil, internalFailure(ctx, "read settings", err)
	}

	return &GetSettingsOutput{Body: h.renderSettings(settings)}, nil
}

// PatchSettings serves PATCH /settings: each supplied member replaces its
// stored value wholesale in one transaction and the answer re-reads the
// stored document rather than echoing the request. The store's two
// sentinel errors are the 422 of doc 05 section 11.1.
func (h *SettingsHandlers) PatchSettings(ctx context.Context, in *PatchSettingsInput) (*GetSettingsOutput, error) {
	if raw, ok := in.Body["default_destination"]; ok {
		var destination string
		// Fail closed: with no configured roots nothing can satisfy the
		// documented "inside a data root" domain. A non-string member
		// defers to the store's own out-of-range error — same 422.
		if err := json.Unmarshal(raw, &destination); err == nil &&
			(len(h.roots) == 0 || !destinationInRoots(destination, h.roots)) {
			return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "default_destination must be inside a configured data root")
		}
	}
	settings, err := h.settings.PutSettings(ctx, in.Body)
	switch {
	case errors.Is(err, store.ErrUnknownSettingKey):
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "the patch names a key that is not a setting")
	case errors.Is(err, store.ErrSettingOutOfRange):
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "the patch carries a value outside its key's domain")
	case err != nil:
		return nil, internalFailure(ctx, "update settings", err)
	}

	return &GetSettingsOutput{Body: h.renderSettings(settings)}, nil
}

// destinationInRoots reports whether destination is one of the configured
// data roots or a path inside one — the domain doc 11 section 5 gives
// default_destination. A non-string member (the unmarshal failed above)
// falls through to the store's own out-of-range error.
func destinationInRoots(destination string, roots []string) bool {
	for _, root := range roots {
		// The store requires a canonical destination, so the roots must
		// be compared in canonical form too — a root carrying a trailing
		// separator would otherwise reject every patch.
		root = filepath.Clean(root)
		if destination == root || strings.HasPrefix(destination, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// renderSettings renders the typed store.Settings into the flat wire
// object — exactly the fifteen keys of doc 11 section 5. The secret
// member is the literal "__redacted__", never the stored list, never an
// array and never the empty string.
func (h *SettingsHandlers) renderSettings(s store.Settings) SettingsBody {
	destination := s.DefaultDestination
	if destination == "" && len(h.roots) > 0 {
		// The documented default is the first DLTOOL_DATA_ROOTS entry;
		// the store cannot know the roots, so the row stays unset and the
		// render substitutes the fallback here.
		destination = h.roots[0]
	}
	minFree := s.MinFreeSpace
	if minFree == nil {
		// A nil map would marshal as null; the wire shape is always an
		// object, {} while nothing is stored.
		minFree = map[string]int64{}
	}

	return SettingsBody{
		DownloadRateLimit:  s.DownloadRateLimit,
		UploadRateLimit:    s.UploadRateLimit,
		AltDownloadRate:    s.AltDownloadRate,
		AltUploadRate:      s.AltUploadRate,
		ScheduleEnabled:    s.ScheduleEnabled,
		DefaultDestination: destination,
		MinFreeSpace:       minFree,
		MaxActiveTotal:     s.MaxActiveTotal,
		MaxActivePerEngine: s.MaxActivePerEngine,
		ProcessOrder:       s.ProcessOrder,
		RSSEnabled:         s.RSSEnabled,
		RSSIntervalS:       s.RSSIntervalS,
		AutoExtract:        s.AutoExtract,
		ExtractPasswords:   RedactedPlaceholder,
		ConfirmOnDelete:    s.ConfirmOnDelete,
	}
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

	var outcome engineProbeOutcome
	e, registered := h.engines.Get(row.Kind)
	if !registered {
		outcome.healthErr = fmt.Errorf(unregisteredEngine, row.Kind)
	} else {
		probeCtx, cancel := context.WithTimeout(ctx, engineProbeDeadline)
		outcome = probeEngineConformance(probeCtx, e, h.loadPolicy, logFromContext(ctx))
		cancel()
	}
	if outcome.healthErr != nil {
		detail := outcome.healthErr.Error()
		output.Body.Error = &detail
	} else {
		output.Body.Ok = true
		output.Body.Version = &outcome.version
	}
	output.Body.ElapsedMS = elapsedMillis(time.Since(started))

	if err := recordEngineProbe(ctx, h.settings, row.ID, outcome); err != nil {
		return nil, internalFailure(ctx, "record probe", err)
	}

	return output, nil
}

type engineProbeOutcome struct {
	version   string
	healthErr error
	summary   *string
}

// probeEngineConformance shares the boot and correction sequence under the caller's deadline.
func probeEngineConformance(ctx context.Context, e engine.Engine, load func(context.Context) (engine.Policy, error), log *slog.Logger) engineProbeOutcome {
	version, err := e.Health(ctx)
	outcome := engineProbeOutcome{version: version, healthErr: err}
	if err != nil {
		return outcome
	}
	conformer, ok := e.(interface {
		Conform(context.Context, int) ([]engine.ConformanceCheck, error)
	})
	if !ok {
		return outcome
	}

	policy, err := load(ctx)
	var checks []engine.ConformanceCheck
	if err != nil {
		checks = []engine.ConformanceCheck{{Key: settingMaxActiveTotal, Want: "non-negative integer", Got: "unreadable", Warn: true, Severity: "warn"}}
	} else {
		checks, err = conformer.Conform(ctx, policy.Limits.MaxActiveTotal)
		if err != nil && len(checks) == 0 {
			checks = []engine.ConformanceCheck{{Key: "probe", Want: "reachable", Got: "unavailable", Warn: true, Severity: "warn"}}
		}
	}
	var summary []string
	for _, check := range checks {
		if check.Severity == "ok" {
			continue
		}
		// Quote values so paths or daemon responses cannot inject log/summary lines.
		summary = append(summary, fmt.Sprintf("%s want=%q got=%q (%s)", check.Key, check.Want, check.Got, check.Severity))
		log.Warn("engine conformance", slog.String("engine", e.Name()), slog.String("key", check.Key), slog.String("want", check.Want), slog.String("got", check.Got))
	}
	if len(summary) > 0 {
		message := conformancePrefix + strings.Join(summary, "; ")
		outcome.summary = &message
	}
	return outcome
}

// TouchEngine separates health and warning writes. Write the warning last so it survives success.
func recordEngineProbe(ctx context.Context, settings *store.SettingsStore, id string, outcome engineProbeOutcome) error {
	at := time.Now().UnixMilli()
	if outcome.healthErr != nil {
		message := outcome.healthErr.Error()
		return settings.TouchEngine(ctx, id, nil, &message, at)
	}
	if err := settings.TouchEngine(ctx, id, &outcome.version, nil, at); err != nil {
		return err
	}
	if outcome.summary != nil {
		return settings.TouchEngine(ctx, id, nil, outcome.summary, at)
	}
	return nil
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
