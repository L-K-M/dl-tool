// The /system operations of docs/05-api-contract.md section 13. T091 adds
// POST /system/backup; GET /system/info (T092) and GET /system/logs (T096)
// join this file.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/obs"
	"github.com/L-K-M/dl-tool/internal/store"
)

// CreateBackupOutput is 201 on success. There is no request body.
type CreateBackupOutput struct {
	Status int `json:"-"`
	Body   struct {
		Path      string `json:"path"`
		SizeBytes int64  `json:"size_bytes"`
		CreatedAt string `json:"created_at"`
	}
}

// The member shapes of the GET /system/info body (doc 05 section 13).
type (
	// DatabaseInfo reports the live database file: the configured path,
	// its size on disk and the newest applied goose migration.
	DatabaseInfo struct {
		Path          string `json:"path"`
		SizeBytes     int64  `json:"size_bytes"`
		SchemaVersion int64  `json:"schema_version"`
	}

	// EngineBrief is one engines row reduced to what the status bar needs.
	// It carries no url: a configured engine address can embed credentials.
	EngineBrief struct {
		Kind      string  `json:"kind" enum:"aria2,qbittorrent,ytdlp"`
		Connected bool    `json:"connected"`
		Version   *string `json:"version"`
	}

	// TaskCounts is the live tasks table grouped by state; removed
	// tombstones are not tasks and are excluded from both members.
	TaskCounts struct {
		Total   int            `json:"total"`
		ByState map[string]int `json:"by_state" doc:"state to count, only states present in the table"`
	}

	// ScheduleBrief mirrors the read-only members of GET
	// /settings/schedule: the enabled flag, the cell in force at the
	// moment of the call and the container's TZ — the same vocabulary
	// and the same evaluation doc 05 section 11.2 defines.
	ScheduleBrief struct {
		Enabled    bool   `json:"enabled"`
		ActiveMode string `json:"active_mode" enum:"no_download,default,alternative"`
		Timezone   string `json:"timezone"`
	}

	// LimitsBrief reports the two admission ceilings of doc 11 section 5.
	LimitsBrief struct {
		MaxActiveTotal     int `json:"max_active_total"`
		MaxActivePerEngine int `json:"max_active_per_engine"`
	}

	// JobCounts is the jobs table grouped by the states the operator
	// acts on; done rows are history, not load.
	JobCounts struct {
		Pending int `json:"pending"`
		Running int `json:"running"`
		Failed  int `json:"failed"`
	}
)

// SystemLogsInput is the query of GET /system/logs: the level floor, the
// optional since bound and the cursor pagination envelope of doc 05
// sections 1.4 and 13.
type SystemLogsInput struct {
	Level  string `query:"level"  enum:"debug,info,warn,error" default:"info" doc:"Minimum level of the returned records"`
	Since  string `query:"since"  doc:"RFC 3339 instant; only records at or after it are returned"`
	Limit  int    `query:"limit"  minimum:"1" maximum:"500"   default:"100"  doc:"Page size"`
	Cursor string `query:"cursor" doc:"Opaque page token from a previous response"`
}

// SystemLogsOutput is the cursor pagination envelope of doc 05 section
// 1.4 carrying obs.Record rows — already redacted before they were
// stored, so the page shows exactly what stdout and the log file hold.
type SystemLogsOutput struct {
	Body struct {
		Items      []obs.Record `json:"items"       doc:"Log records, newest first"`
		NextCursor *string      `json:"next_cursor" doc:"Token for the next page; null on the last page"`
		Total      int          `json:"total"       doc:"Records matching level and since, ignoring the cursor"`
	}
}

// logCursor is the page token of GET /system/logs: base64 JSON carrying
// the At of the previous page's last record — the same envelope shape the
// store's task-event cursor uses (docs/05-api-contract.md section 1.4).
type logCursor struct {
	At string `json:"at"`
}

// encodeLogCursor renders a page token for GET /system/logs.
func encodeLogCursor(at time.Time) (string, error) {
	encoded, err := json.Marshal(logCursor{At: at.UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return "", fmt.Errorf("marshal log cursor: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

// decodeLogCursor parses a page token. A token that is not base64 JSON of
// the cursor shape, or carries an unreadable At, belongs to no page —
// the wire outcome is the same 422 as any other stale cursor.
func decodeLogCursor(token string) (time.Time, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, errors.New("token is not valid base64")
	}
	var cursor logCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		return time.Time{}, errors.New("token is not a page cursor")
	}
	at, err := time.Parse(time.RFC3339Nano, cursor.At)
	if err != nil {
		return time.Time{}, errors.New("token carries no record instant")
	}

	return at, nil
}

// SystemInfoOutput is the GET /system/info body: the twelve members of
// doc 05 section 13, no others.
type SystemInfoOutput struct {
	Body struct {
		Version   string        `json:"version"    doc:"Build version stamped at link time"`
		Commit    string        `json:"commit"     doc:"VCS revision from the binary's build info"`
		BuiltAt   string        `json:"built_at"   format:"date-time" doc:"VCS commit time from the binary's build info"`
		GoVersion string        `json:"go_version"`
		StartedAt string        `json:"started_at" format:"date-time"`
		UptimeS   int64         `json:"uptime_s"`
		Database  DatabaseInfo  `json:"database"`
		Engines   []EngineBrief `json:"engines"`
		Tasks     TaskCounts    `json:"tasks"`
		Schedule  ScheduleBrief `json:"schedule"`
		Limits    LimitsBrief   `json:"limits"`
		Jobs      JobCounts     `json:"jobs"`
	}
}

const (
	queryTaskStateCounts = `SELECT state, COUNT(*) AS count FROM tasks WHERE state <> 'removed' GROUP BY state`
	queryJobStateCounts  = `SELECT state, COUNT(*) AS count FROM jobs WHERE state IN ('pending','running','failed') GROUP BY state`
)

// processStartedAt anchors started_at and uptime_s: the package load is
// the closest observable instant to process start the API can report
// without a call from main.
var processStartedAt = time.Now()

// buildStamp reads the toolchain-stamped VCS settings once: the binary
// carries commit and commit time in its own build info, and the only
// ldflags variable the build stamps (main.version) reaches this package
// through api.Version — commit and built_at have no ldflags path.
var buildStamp = sync.OnceValues(func() (commit, builtAt string) {
	commit = "unknown"
	info, ok := debug.ReadBuildInfo()
	if ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				commit = setting.Value
			case "vcs.time":
				builtAt = setting.Value
			}
		}
	}
	if builtAt == "" {
		// The schema declares format date-time, so an absent VCS stamp
		// must still render a valid RFC 3339 value.
		builtAt = time.Time{}.UTC().Format(time.RFC3339)
	}

	return commit, builtAt
})

// SystemHandlers serves the /system operations of doc 05 section 13.
type SystemHandlers struct {
	maintenance *store.MaintenanceStore
	backupDir   string

	// The GET /system/info collaborators, attached by NewServer through
	// attachInfo: the database for the counts and schema version, its
	// path and size for database, the settings store for schedule and
	// limits, and the registry for engine connectivity.
	db       *sqlx.DB
	dbPath   string
	settings *store.SettingsStore
	engines  *engine.Registry

	// logs is the process log recorder GET /system/logs reads, attached
	// by NewServer through attachLogs.
	logs *obs.Recorder
}

// NewSystemHandlers takes the one MaintenanceStore and the backup directory —
// filepath.Join(cfg.ConfigDir, store.BackupsDirName), resolved once.
func NewSystemHandlers(m *store.MaintenanceStore, backupDir string) *SystemHandlers {
	return &SystemHandlers{maintenance: m, backupDir: backupDir}
}

// attachInfo wires the collaborators GET /system/info needs. NewServer is
// the call site, so the two-argument constructor T091's tests use keeps
// working unchanged.
func (h *SystemHandlers) attachInfo(db *sqlx.DB, dbPath string, engines *engine.Registry) {
	h.db = db
	h.dbPath = dbPath
	h.settings = store.NewSettingsStore(db)
	h.engines = engines
}

// attachLogs wires the recorder GET /system/logs reads. The process
// logger's own handler is the *obs.Recorder obs.NewLogger installed; a
// logger built any other way — a unit test's io.Discard logger — leaves
// the recorder nil and the operation serves an empty page.
func (h *SystemHandlers) attachLogs(log *slog.Logger) {
	if log == nil {
		return
	}
	if recorder, ok := log.Handler().(*obs.Recorder); ok {
		h.logs = recorder
	}
}

// CreateBackup runs VACUUM INTO into the backup directory and prunes to the
// newest store.BackupKeepCount snapshots — the same work the nightly cron
// entry does.
func (h *SystemHandlers) CreateBackup(ctx context.Context, _ *struct{}) (*CreateBackupOutput, error) {
	result, err := h.maintenance.BackupInto(ctx, h.backupDir)
	if errors.Is(err, store.ErrBackupRunning) {
		return nil, Problem(SlugConflict, http.StatusConflict, "a backup is already running")
	}
	if err != nil {
		return nil, internalFailure(ctx, "create backup", err)
	}
	if _, err := h.maintenance.PruneBackups(ctx, h.backupDir, store.BackupKeepCount); err != nil {
		return nil, internalFailure(ctx, "prune backups", err)
	}

	output := &CreateBackupOutput{Status: http.StatusCreated}
	output.Body.Path = result.Path
	output.Body.SizeBytes = result.SizeBytes
	output.Body.CreatedAt = result.CreatedAt.UTC().Format(time.RFC3339)

	return output, nil
}

// GetSystemInfo serves GET /system/info (doc 05 section 13). The process
// members always answer; the store-backed members report their zero
// values on a nil-db build — the openapi subcommand and router-only tests
// — which has no database to count.
func (h *SystemHandlers) GetSystemInfo(ctx context.Context, _ *struct{}) (*SystemInfoOutput, error) {
	output := &SystemInfoOutput{}
	output.Body.Version = Version
	output.Body.Commit, output.Body.BuiltAt = buildStamp()
	output.Body.GoVersion = runtime.Version()
	output.Body.StartedAt = processStartedAt.UTC().Format(time.RFC3339)
	output.Body.UptimeS = int64(time.Since(processStartedAt).Seconds())
	output.Body.Database.Path = h.dbPath
	output.Body.Engines = []EngineBrief{}
	output.Body.Tasks.ByState = map[string]int{}
	// The nil-db path below (router-only builds, openapi generation)
	// still emits in-contract values: an enum member, not ""; the
	// documented defaults, not zeros that would read as "unlimited".
	output.Body.Schedule = ScheduleBrief{
		ActiveMode: "default",
		Timezone:   localZoneName(),
	}
	output.Body.Limits = LimitsBrief{
		MaxActiveTotal:     defaultMaxActiveTotal,
		MaxActivePerEngine: defaultMaxActivePerEngine,
	}

	if h.db == nil {
		return output, nil
	}

	// Size is best-effort: a transient stat failure (restore in progress,
	// path raced) must not take down the whole status payload.
	if stat, err := os.Stat(h.dbPath); err == nil {
		output.Body.Database.SizeBytes = stat.Size()
	}
	schemaVersion, err := store.SchemaVersion(ctx, h.db)
	if err != nil {
		return nil, internalFailure(ctx, "read schema version", err)
	}
	output.Body.Database.SchemaVersion = schemaVersion

	engineRows, err := h.settings.ListEngines(ctx)
	if err != nil {
		return nil, internalFailure(ctx, "list engines", err)
	}
	for _, row := range engineRows {
		_, registered := h.engines.Get(row.Kind)
		output.Body.Engines = append(output.Body.Engines, EngineBrief{
			Kind:      row.Kind,
			Connected: engineConnected(row, registered),
			Version:   row.Version,
		})
	}

	var taskRows []struct {
		State string `db:"state"`
		Count int    `db:"count"`
	}
	if err := h.db.SelectContext(ctx, &taskRows, queryTaskStateCounts); err != nil {
		return nil, internalFailure(ctx, "count tasks", err)
	}
	for _, row := range taskRows {
		output.Body.Tasks.ByState[row.State] = row.Count
		output.Body.Tasks.Total += row.Count
	}

	cells, scheduleEnabled, err := h.settings.ScheduleSnapshot(ctx)
	if err != nil {
		return nil, internalFailure(ctx, "read schedule", err)
	}
	output.Body.Schedule.Enabled = scheduleEnabled
	output.Body.Schedule.ActiveMode = string(cells[activeScheduleIndex(time.Now())])

	maxTotal, err := h.settings.GetInt64(ctx, settingMaxActiveTotal, defaultMaxActiveTotal)
	if err != nil {
		return nil, internalFailure(ctx, "read max_active_total", err)
	}
	maxPerEngine, err := h.settings.GetInt64(ctx, settingMaxActivePerEngine, defaultMaxActivePerEngine)
	if err != nil {
		return nil, internalFailure(ctx, "read max_active_per_engine", err)
	}
	output.Body.Limits = LimitsBrief{
		MaxActiveTotal:     int(maxTotal),
		MaxActivePerEngine: int(maxPerEngine),
	}

	var jobRows []struct {
		State string `db:"state"`
		Count int    `db:"count"`
	}
	if err := h.db.SelectContext(ctx, &jobRows, queryJobStateCounts); err != nil {
		return nil, internalFailure(ctx, "count jobs", err)
	}
	for _, row := range jobRows {
		switch row.State {
		case "pending":
			output.Body.Jobs.Pending = row.Count
		case "running":
			output.Body.Jobs.Running = row.Count
		case "failed":
			output.Body.Jobs.Failed = row.Count
		}
	}

	return output, nil
}

// systemLogLevel maps the level query's enum to its slog floor; the enum
// tag already rejected anything else, and the absent case carries huma's
// info default.
func systemLogLevel(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// GetSystemLogs serves GET /system/logs (doc 05 section 13): one
// cursor-paginated page of the process log recorder, newest first. Every
// record was redacted before it was stored, so no value on the page can
// carry a secret. A malformed since or a cursor this endpoint never
// issued is 422 /problems/validation-failed; on a nil-recorder build —
// a logger not built by obs.NewLogger — the page is empty rather than an
// error.
func (h *SystemHandlers) GetSystemLogs(ctx context.Context, in *SystemLogsInput) (*SystemLogsOutput, error) {
	var after time.Time
	if in.Since != "" {
		parsed, err := time.Parse(time.RFC3339, in.Since)
		if err != nil {
			return nil, invalidParameter("since", "is not an RFC 3339 instant")
		}
		after = parsed
	}

	var cursor time.Time
	if in.Cursor != "" {
		parsed, err := decodeLogCursor(in.Cursor)
		if err != nil {
			return nil, invalidParameter("cursor", staleCursorDetail)
		}
		cursor = parsed
	}

	output := &SystemLogsOutput{}
	output.Body.Items = []obs.Record{}
	if h.logs == nil {
		return output, nil
	}

	level := systemLogLevel(in.Level)
	// Page is one locked pass, so items and total describe the same ring
	// snapshot even while the process logger keeps writing.
	records, next, total := h.logs.Page(level, after, cursor, in.Limit)
	if len(records) > 0 {
		output.Body.Items = records
	}
	output.Body.Total = total

	if !next.IsZero() {
		token, err := encodeLogCursor(next)
		if err != nil {
			return nil, internalFailure(ctx, "encode log cursor", err)
		}
		output.Body.NextCursor = &token
	}

	return output, nil
}

// invalidParameter is the 422 a bad query parameter answers with, the
// huma-generated shape mirrored for values the handler parses itself.
func invalidParameter(name, detail string) error {
	return &huma.ErrorModel{
		Type:   SlugValidationFailed,
		Title:  http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity,
		Detail: detail,
		Errors: []*huma.ErrorDetail{{
			Message:  detail,
			Location: "query." + name,
		}},
	}
}

// Register mounts POST /system/backup and GET /system/info on the Huma
// API.
func (h *SystemHandlers) Register(hapi huma.API) {
	huma.Register(hapi, huma.Operation{
		OperationID:   "create-backup",
		Method:        http.MethodPost,
		Path:          "/system/backup",
		DefaultStatus: http.StatusCreated,
		Summary:       "Back up the database",
		Description:   "Writes a consistent snapshot with VACUUM INTO and returns its path and size, retaining the newest seven files. 409 /problems/conflict while another backup is running; 500 /problems/internal on any other failure.",
		Security:      credentialRequired,
	}, h.CreateBackup)

	huma.Register(hapi, huma.Operation{
		OperationID: "get-system-info",
		Method:      http.MethodGet,
		Path:        "/system/info",
		Summary:     "Read system information",
		Description: "The running build, the database path, size and schema version, the configured engines' last probe state, task and job counts, the schedule flag and cell in force, and the admission limits. No field ever carries a secret.",
		Security:    credentialRequired,
	}, h.GetSystemInfo)
}
