package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/fsx"
	"github.com/L-K-M/dl-tool/internal/rss"
	"github.com/L-K-M/dl-tool/internal/search"
	"github.com/L-K-M/dl-tool/internal/store"
)

const (
	operationExportSettings = "export-settings"
	operationImportSettings = "import-settings"

	// exportDocumentVersion is the document_version this binary writes and
	// the newest it accepts on import (docs/05-api-contract.md
	// section 11.5).
	exportDocumentVersion = 1

	collectionSettings     = "settings"
	collectionCategories   = "categories"
	collectionIndexers     = "indexers"
	collectionFeeds        = "feeds"
	collectionRules        = "rules"
	collectionWatchFolders = "watch_folders"
	collectionSchedule     = "schedule"

	conflictSkip      = "skip"
	conflictOverwrite = "overwrite"

	// rssIntervalSMinimum is the 5-minute floor doc 11 section 5 puts on
	// the global RSS poll interval.
	rssIntervalSMinimum = 300
)

// exportSettingKeys is the flat settings object of docs/11-config-reference.md
// section 5 minus extract_passwords — the only secret key. Internal keys such
// as watch_folder_loaded_* are excluded at the query level, so a key added
// for machinery can never leak into a bug-report attachment.
var exportSettingKeys = []string{
	"download_rate_limit", "upload_rate_limit",
	"alt_download_rate_limit", "alt_upload_rate_limit",
	"schedule_enabled", "default_destination", "min_free_space",
	"max_active_total", "max_active_per_engine",
	"process_order", "rss_enabled", "rss_interval_s",
	"auto_extract", "confirm_on_delete",
}

// queryExportSettings reads only the documented keys; the whitelist, not a
// blacklist, so a later internal key needs no exclusion here.
var queryExportSettings = `SELECT key, value_json FROM settings WHERE key IN ('` +
	strings.Join(exportSettingKeys, `','`) + `')`

// The export reads of doc 05 section 11.5: exactly the member columns, so
// the secret columns — indexers.api_key_enc and every engine, channel,
// session, token and task table — are never selected at all rather than
// filtered out afterwards.
const (
	queryExportCategories = `SELECT name, save_path FROM categories ORDER BY name`
	queryExportIndexers   = `SELECT name, kind, enabled, url, definition_id, priority, settings_json
FROM indexers ORDER BY priority, name`
	queryExportFeeds = `SELECT url, title, enabled, refresh_interval_s, item_cap
FROM feeds ORDER BY title, id`
	queryExportRules = `SELECT name, enabled, priority, definition_json
FROM rules ORDER BY priority, name`
	queryExportWatchFolders = `SELECT w.path, w.enabled, w.destination, c.name AS category,
w.delete_after_load, w.poll_interval_s
FROM watch_folders w LEFT JOIN categories c ON c.id = w.category_id
ORDER BY w.created_at, w.id`
)

// The import writes of section 11.5, all running inside the one
// transaction applyImport opens.
const (
	queryImportSettingExists = `SELECT 1 FROM settings WHERE key = ?`
	queryUpsertSetting       = `INSERT INTO settings (id, key, value_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at`

	queryInsertCategory     = `INSERT INTO categories (id, name, save_path, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`
	queryUpdateCategoryPath = `UPDATE categories SET save_path = ?, updated_at = ? WHERE id = ?`

	queryIndexerIDByDefinition = `SELECT id FROM indexers WHERE definition_id = ?`
	queryIndexerIDByName       = `SELECT id FROM indexers WHERE name = ? ORDER BY priority, name LIMIT 1`
	queryInsertIndexer         = `INSERT INTO indexers
(id, name, kind, enabled, url, definition_id, priority, settings_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	queryUpdateIndexer = `UPDATE indexers
SET name = ?, kind = ?, enabled = ?, url = ?, definition_id = ?, priority = ?, settings_json = ?, updated_at = ?
WHERE id = ?`

	queryFeedIDByURL = `SELECT id FROM feeds WHERE url = ?`
	queryInsertFeed  = `INSERT INTO feeds (id, url, title, enabled, refresh_interval_s, item_cap, next_fetch_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	queryUpdateFeed   = `UPDATE feeds SET title = ?, enabled = ?, refresh_interval_s = ?, item_cap = ?, updated_at = ? WHERE id = ?`
	queryRuleIDByName = `SELECT id FROM rules WHERE name = ?`
	queryInsertRule   = `INSERT INTO rules (id, name, enabled, priority, definition_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`
	queryUpdateRule = `UPDATE rules SET enabled = ?, priority = ?, definition_json = ?, updated_at = ? WHERE id = ?`

	queryWatchFolderIDByPath = `SELECT id FROM watch_folders WHERE path = ?`
	queryInsertWatchFolder   = `INSERT INTO watch_folders
(id, path, enabled, destination, category_id, delete_after_load, poll_interval_s, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	queryUpdateWatchFolder = `UPDATE watch_folders
SET enabled = ?, destination = ?, category_id = ?, delete_after_load = ?, poll_interval_s = ?, updated_at = ?
WHERE id = ?`

	queryReplaceScheduleCell = `UPDATE bandwidth_schedule SET mode = ?, updated_at = ? WHERE day = ? AND hour = ?`
)

// ExportDocument is the portable settings document of doc 05 §11.5. Exactly
// these members, in this order, and no others.
type ExportDocument struct {
	DocumentVersion int              `json:"document_version" required:"true" minimum:"1"` // currently 1
	ExportedAt      time.Time        `json:"exported_at"       required:"true" format:"date-time"`
	SchemaVersion   int64            `json:"schema_version"    required:"true"`
	Settings        map[string]any   `json:"settings"          required:"true" nullable:"false"` // secrets OMITTED, not "__redacted__"
	Categories      []ExportCategory `json:"categories"        required:"true" nullable:"false"` // name, save_path
	Indexers        []ExportIndexer  `json:"indexers"          required:"true" nullable:"false"` // never api_key
	Feeds           []ExportFeed     `json:"feeds"             required:"true" nullable:"false"`
	Rules           []ExportRule     `json:"rules"             required:"true" nullable:"false"`
	WatchFolders    []ExportWatch    `json:"watch_folders"     required:"true" nullable:"false"`
	Schedule        ExportSchedule   `json:"schedule"          required:"true"` // enabled + the 168 cells
}

// Excluded from every export, with no option to include them: sessions, the
// account row and therefore its password_hash, api_tokens,
// notification_channels.secret_enc, engines.secret_enc, indexers.api_key,
// tasks and every task-derived table. An export is safe to attach to a bug
// report.

// ExportCategory is one categories member: name, save_path.
type ExportCategory struct {
	Name     string `json:"name"      db:"name"      required:"true" minLength:"1"`
	SavePath string `json:"save_path" db:"save_path" required:"true" minLength:"1"`
}

// ExportIndexer is one indexers member: name, kind, enabled, url,
// definition_id, priority, settings — never api_key.
type ExportIndexer struct {
	Name         string         `json:"name"          required:"true" minLength:"1"`
	Kind         string         `json:"kind"          required:"true" enum:"torznab,newznab,dlsearch"`
	Enabled      bool           `json:"enabled"`
	URL          *string        `json:"url"           required:"true"`
	DefinitionID *string        `json:"definition_id" required:"true"`
	Priority     int            `json:"priority"`
	Settings     map[string]any `json:"settings"      required:"true" nullable:"false"`
}

// ExportFeed is one feeds member: url, title, enabled, refresh_interval_s,
// item_cap.
type ExportFeed struct {
	URL              string  `json:"url"                required:"true" minLength:"1"`
	Title            *string `json:"title"              required:"true"`
	Enabled          bool    `json:"enabled"`
	RefreshIntervalS int     `json:"refresh_interval_s" minimum:"0"`
	ItemCap          int     `json:"item_cap"           minimum:"0"`
}

// ExportRule is one rules member: name, enabled, priority, definition.
type ExportRule struct {
	Name       string         `json:"name"       required:"true" minLength:"1"`
	Enabled    bool           `json:"enabled"`
	Priority   int            `json:"priority"`
	Definition map[string]any `json:"definition" required:"true" nullable:"false"`
}

// ExportWatch is one watch_folders member: path, enabled, destination,
// category, delete_after_load, poll_interval_s.
type ExportWatch struct {
	Path            string  `json:"path"            required:"true" minLength:"1"`
	Enabled         bool    `json:"enabled"`
	Destination     string  `json:"destination"     required:"true" minLength:"1"`
	Category        *string `json:"category"        required:"true"`
	DeleteAfterLoad bool    `json:"delete_after_load"`
	PollIntervalS   int     `json:"poll_interval_s" minimum:"1"`
}

// ExportSchedule is the schedule member of section 11.5: enabled and the
// 168 cells, exactly as section 11.2 renders them.
type ExportSchedule struct {
	Enabled bool  `json:"enabled"`
	Cells   []int `json:"cells" required:"true" nullable:"false" minItems:"168" maxItems:"168" minimum:"0" maximum:"2"`
}

// ImportInput is the POST /settings/import body.
type ImportInput struct {
	Body struct {
		Document   ExportDocument `json:"document"    required:"true"`
		DryRun     *bool          `json:"dry_run,omitempty"`                           // default true
		OnConflict string         `json:"on_conflict,omitempty" enum:"skip,overwrite"` // "skip" (default) | "overwrite"
	}
}

// ImportReport is what both a dry run and a committing call return. A dry
// run is side-effect free and returns exactly the report a committing call
// would produce.
type ImportReport struct {
	DryRun          bool              `json:"dry_run"`
	DocumentVersion int               `json:"document_version"`
	Totals          Counts            `json:"totals"`
	Collections     map[string]Counts `json:"collections"`
	Rejected        []RejectedRow     `json:"rejected"`
}

// Counts is the per-collection and grand-total ledger of created, updated,
// skipped and rejected rows.
type Counts struct {
	Created  int `json:"created"`
	Updated  int `json:"updated"`
	Skipped  int `json:"skipped"`
	Rejected int `json:"rejected"`
}

// RejectedRow is one refused member; Type is an RFC 9457 problem type,
// e.g. "/problems/path-rejected".
type RejectedRow struct {
	Collection string `json:"collection"`
	Key        string `json:"key"`
	Type       string `json:"type"`
	Detail     string `json:"detail"`
}

// ExportSettingsOutput is the GET /settings/export body.
type ExportSettingsOutput struct{ Body ExportDocument }

// ImportSettingsOutput is the POST /settings/import body: the report.
type ImportSettingsOutput struct{ Body ImportReport }

// SettingsExportHandlers owns the two operations of doc 05 section 11.5:
// the export builder and the dry-run-first, transactional importer.
type SettingsExportHandlers struct {
	db       *sqlx.DB
	settings *store.SettingsStore
	roots    []string
}

// NewSettingsExportHandlers builds the handlers; roots is the configured
// DLTOOL_DATA_ROOTS set every imported path is re-jailed against.
func NewSettingsExportHandlers(db *sqlx.DB, roots []string) *SettingsExportHandlers {
	return &SettingsExportHandlers{db: db, settings: store.NewSettingsStore(db), roots: roots}
}

// Register mounts export-settings and import-settings on the Huma API;
// Server.registerOperations is the call site.
func (h *SettingsExportHandlers) Register(hapi huma.API) {
	huma.Register(hapi, huma.Operation{
		OperationID: operationExportSettings,
		Method:      http.MethodGet,
		Path:        "/settings/export",
		Summary:     "Export the portable settings document",
		Description: "The versioned document of doc 05 section 11.5: settings, categories, indexers, feeds, rules, watch folders and the schedule. Sessions, the account row, API tokens, engine and channel secrets, indexer API keys and every task table are excluded at the query level — the document is safe to attach to a bug report.",
		Tags:        []string{"settings"},
		Security:    credentialRequired,
	}, h.Export)

	huma.Register(hapi, huma.Operation{
		OperationID: operationImportSettings,
		Method:      http.MethodPost,
		Path:        "/settings/import",
		Summary:     "Import a portable settings document",
		Description: "Applies an export document, dry-run first. dry_run defaults to true and is side-effect free while reporting exactly what a commit would do; a committing call writes every accepted row in one transaction. on_conflict is skip (keep the existing row) or overwrite; matching is by categories.name, indexers.definition_id else name, feeds.url, rules.name and watch_folders.path. Paths are re-validated against this host's data roots and failures are rejected, never rewritten. A document_version newer than the binary understands is 409.",
		Tags:        []string{"settings"},
		Security:    credentialRequired,
		Middlewares: huma.Middlewares{limitImportBody},
	}, h.Import)
}

// maxImportBodyBytes caps the import document at 64 KiB, the prefs cap of
// doc 05 section 11.4 applied to section 11.5's body.
const maxImportBodyBytes int64 = 64 << 10

// limitImportBody is the POST operation's middleware, the limitPrefsBody
// pattern: it bounds the raw body to 64 KiB with http.MaxBytesReader and
// re-attaches it so Huma's ordinary JSON path parses and validates it. A
// malformed body is Huma's 422 with errors[].location; the cap is answered
// here as 413 before the handler runs.
func limitImportBody(ctx huma.Context, next func(huma.Context)) {
	r, w := humachi.Unwrap(ctx)

	r.Body = http.MaxBytesReader(w, r.Body, maxImportBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeProblem(w, Problem(
			SlugPayloadTooLarge,
			http.StatusRequestEntityTooLarge,
			"the import document exceeds 65536 bytes",
		))

		return
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	next(ctx)
}

// Export serves GET /settings/export.
func (h *SettingsExportHandlers) Export(ctx context.Context, _ *struct{}) (*ExportSettingsOutput, error) {
	doc, err := h.buildExport(ctx)
	if err != nil {
		return nil, internalFailure(ctx, "export settings", err)
	}

	return &ExportSettingsOutput{Body: doc}, nil
}

// buildExport reads the seven collections; every read selects only the
// exported columns, so no secret column ever reaches the document.
func (h *SettingsExportHandlers) buildExport(ctx context.Context) (ExportDocument, error) {
	doc := ExportDocument{
		DocumentVersion: exportDocumentVersion,
		ExportedAt:      time.Now().UTC(),
		Settings:        map[string]any{},
		Categories:      []ExportCategory{},
		Indexers:        []ExportIndexer{},
		Feeds:           []ExportFeed{},
		Rules:           []ExportRule{},
		WatchFolders:    []ExportWatch{},
	}

	version, err := store.SchemaVersion(ctx, h.db)
	if err != nil {
		return doc, fmt.Errorf("read schema version: %w", err)
	}
	doc.SchemaVersion = version

	var settingRows []struct {
		Key   string `db:"key"`
		Value string `db:"value_json"`
	}
	if err := h.db.SelectContext(ctx, &settingRows, queryExportSettings); err != nil {
		return doc, fmt.Errorf("read settings: %w", err)
	}
	for _, row := range settingRows {
		var value any
		if err := json.Unmarshal([]byte(row.Value), &value); err != nil {
			return doc, fmt.Errorf("decode settings key %s: %w", row.Key, err)
		}
		doc.Settings[row.Key] = value
	}

	var categoryRows []ExportCategory
	if err := h.db.SelectContext(ctx, &categoryRows, queryExportCategories); err != nil {
		return doc, fmt.Errorf("read categories: %w", err)
	}
	doc.Categories = append(doc.Categories, categoryRows...)

	var indexerRows []struct {
		Name         string  `db:"name"`
		Kind         string  `db:"kind"`
		Enabled      bool    `db:"enabled"`
		URL          *string `db:"url"`
		DefinitionID *string `db:"definition_id"`
		Priority     int     `db:"priority"`
		SettingsJSON *string `db:"settings_json"`
	}
	if err := h.db.SelectContext(ctx, &indexerRows, queryExportIndexers); err != nil {
		return doc, fmt.Errorf("read indexers: %w", err)
	}
	for _, row := range indexerRows {
		settings := map[string]any{}
		if row.SettingsJSON != nil {
			if err := json.Unmarshal([]byte(*row.SettingsJSON), &settings); err != nil {
				return doc, fmt.Errorf("decode indexer %q settings: %w", row.Name, err)
			}
		}
		doc.Indexers = append(doc.Indexers, ExportIndexer{
			Name:         row.Name,
			Kind:         row.Kind,
			Enabled:      row.Enabled,
			URL:          row.URL,
			DefinitionID: row.DefinitionID,
			Priority:     row.Priority,
			Settings:     settings,
		})
	}

	var feedRows []struct {
		URL              string  `db:"url"`
		Title            *string `db:"title"`
		Enabled          bool    `db:"enabled"`
		RefreshIntervalS int     `db:"refresh_interval_s"`
		ItemCap          int     `db:"item_cap"`
	}
	if err := h.db.SelectContext(ctx, &feedRows, queryExportFeeds); err != nil {
		return doc, fmt.Errorf("read feeds: %w", err)
	}
	for _, row := range feedRows {
		doc.Feeds = append(doc.Feeds, ExportFeed{
			URL:              row.URL,
			Title:            row.Title,
			Enabled:          row.Enabled,
			RefreshIntervalS: row.RefreshIntervalS,
			ItemCap:          row.ItemCap,
		})
	}

	var ruleRows []struct {
		Name           string `db:"name"`
		Enabled        bool   `db:"enabled"`
		Priority       int    `db:"priority"`
		DefinitionJSON string `db:"definition_json"`
	}
	if err := h.db.SelectContext(ctx, &ruleRows, queryExportRules); err != nil {
		return doc, fmt.Errorf("read rules: %w", err)
	}
	for _, row := range ruleRows {
		var definition map[string]any
		if err := json.Unmarshal([]byte(row.DefinitionJSON), &definition); err != nil {
			return doc, fmt.Errorf("decode rule %q definition: %w", row.Name, err)
		}
		doc.Rules = append(doc.Rules, ExportRule{
			Name:       row.Name,
			Enabled:    row.Enabled,
			Priority:   row.Priority,
			Definition: definition,
		})
	}

	var watchRows []struct {
		Path            string  `db:"path"`
		Enabled         bool    `db:"enabled"`
		Destination     string  `db:"destination"`
		Category        *string `db:"category"`
		DeleteAfterLoad bool    `db:"delete_after_load"`
		PollIntervalS   int     `db:"poll_interval_s"`
	}
	if err := h.db.SelectContext(ctx, &watchRows, queryExportWatchFolders); err != nil {
		return doc, fmt.Errorf("read watch folders: %w", err)
	}
	for _, row := range watchRows {
		doc.WatchFolders = append(doc.WatchFolders, ExportWatch{
			Path:            row.Path,
			Enabled:         row.Enabled,
			Destination:     row.Destination,
			Category:        row.Category,
			DeleteAfterLoad: row.DeleteAfterLoad,
			PollIntervalS:   row.PollIntervalS,
		})
	}

	modes, enabled, err := h.settings.ScheduleSnapshot(ctx)
	if err != nil {
		return doc, fmt.Errorf("read bandwidth schedule: %w", err)
	}
	doc.Schedule = ExportSchedule{Enabled: enabled, Cells: make([]int, len(modes))}
	for i, mode := range modes {
		doc.Schedule.Cells[i] = ModeToCell(mode)
	}

	return doc, nil
}

// Import serves POST /settings/import. A document_version newer than the
// binary understands is 409; everything else runs inside one transaction —
// dry-run rolls it back, commit lands every accepted row or none.
func (h *SettingsExportHandlers) Import(ctx context.Context, in *ImportInput) (*ImportSettingsOutput, error) {
	doc := in.Body.Document
	if doc.DocumentVersion > exportDocumentVersion {
		return nil, Problem(SlugConflict, http.StatusConflict,
			fmt.Sprintf("document_version %d is newer than this binary understands (%d)", doc.DocumentVersion, exportDocumentVersion))
	}
	dryRun := in.Body.DryRun == nil || *in.Body.DryRun
	onConflict := in.Body.OnConflict
	if onConflict == "" {
		onConflict = conflictSkip
	}

	report := ImportReport{
		DryRun:          dryRun,
		DocumentVersion: doc.DocumentVersion,
		Rejected:        []RejectedRow{},
		Collections:     map[string]Counts{},
	}
	for _, collection := range []string{
		collectionSettings, collectionCategories, collectionIndexers, collectionFeeds,
		collectionRules, collectionWatchFolders, collectionSchedule,
	} {
		report.Collections[collection] = Counts{}
	}

	tx, err := h.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, internalFailure(ctx, "begin settings import", err)
	}
	// Rolls back on any early return; after Commit this is sql.ErrTxDone,
	// which is the expected outcome and not worth a warning.
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			logFromContext(ctx).Warn("settings import rollback failed", "err", err)
		}
	}()

	if err := h.applyImport(ctx, tx, &doc, onConflict, &report); err != nil {
		return nil, internalFailure(ctx, "apply settings import", err)
	}

	if !dryRun {
		if err := tx.Commit(); err != nil {
			return nil, internalFailure(ctx, "commit settings import", err)
		}
	}

	for _, counts := range report.Collections {
		report.Totals.Created += counts.Created
		report.Totals.Updated += counts.Updated
		report.Totals.Skipped += counts.Skipped
		report.Totals.Rejected += counts.Rejected
	}

	return &ImportSettingsOutput{Body: report}, nil
}

// applyImport walks the collections in dependency order — categories before
// the watch folders that name them — accumulating the report and writing
// every accepted row into tx.
func (h *SettingsExportHandlers) applyImport(
	ctx context.Context,
	tx *sqlx.Tx,
	doc *ExportDocument,
	onConflict string,
	report *ImportReport,
) error {
	if err := h.importSettings(ctx, tx, doc.Settings, report); err != nil {
		return err
	}
	if err := h.importCategories(ctx, tx, doc.Categories, onConflict, report); err != nil {
		return err
	}
	if err := h.importIndexers(ctx, tx, doc.Indexers, onConflict, report); err != nil {
		return err
	}
	if err := h.importFeeds(ctx, tx, doc.Feeds, onConflict, report); err != nil {
		return err
	}
	if err := h.importRules(ctx, tx, doc.Rules, onConflict, report); err != nil {
		return err
	}
	if err := h.importWatchFolders(ctx, tx, doc.WatchFolders, onConflict, report); err != nil {
		return err
	}

	return h.importSchedule(ctx, tx, doc.Schedule, report)
}

// reject records one refused member: the per-collection rejected count, a
// RejectedRow and nothing else — a rejected row never aborts the import.
func reject(report *ImportReport, collection, key, typ, detail string) {
	counts := report.Collections[collection]
	counts.Rejected++
	report.Collections[collection] = counts
	report.Rejected = append(report.Rejected, RejectedRow{
		Collection: collection,
		Key:        key,
		Type:       typ,
		Detail:     detail,
	})
}

// countRow applies the on_conflict outcome of one matched lookup: an
// existing row is skipped or counted for update, a missing one for insert.
// The write itself happens in the caller's branch.
func countRow(report *ImportReport, collection string, exists bool, onConflict string) (write bool) {
	counts := report.Collections[collection]
	switch {
	case exists && onConflict == conflictSkip:
		counts.Skipped++
	case exists:
		counts.Updated++
	default:
		counts.Created++
	}
	report.Collections[collection] = counts

	return !exists || onConflict == conflictOverwrite
}

// importSettings applies the flat settings member key by key. Settings is
// scalar state, not a conflicting row — the conflict keys of doc 05 section
// 11.5 name only the five row collections, so every recognised key writes:
// a stored key counts updated, a new one created. A key outside the
// exported set — extract_passwords included — is rejected, and a
// default_destination that fails the data-roots check is path-rejected.
func (h *SettingsExportHandlers) importSettings(
	ctx context.Context,
	tx *sqlx.Tx,
	settings map[string]any,
	report *ImportReport,
) error {
	for _, key := range slices.Sorted(maps.Keys(settings)) {
		value := settings[key]
		encoded, rejectType, detail := h.canonicalSettingValue(key, value)
		if detail != "" {
			reject(report, collectionSettings, key, rejectType, detail)
			continue
		}

		var exists int
		err := tx.GetContext(ctx, &exists, queryImportSettingExists, key)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("match settings key %s: %w", key, err)
		}
		counts := report.Collections[collectionSettings]
		if err == nil {
			counts.Updated++
		} else {
			counts.Created++
		}
		report.Collections[collectionSettings] = counts

		now := time.Now().UnixMilli()
		if _, err := tx.ExecContext(ctx, queryUpsertSetting, store.NewID(store.PrefixSetting), key, encoded, now, now); err != nil {
			return fmt.Errorf("write settings key %s: %w", key, err)
		}
	}

	return nil
}

// canonicalSettingValue re-encodes one settings value to its stored JSON
// after the key's shape check, and re-jails default_destination against the
// importing host's roots. An empty detail means the value is acceptable;
// a non-empty detail is the rejection's message and rejectType its problem
// type — path-rejected for a failed roots check, validation-failed for
// everything else, including keys outside the exported set.
func (h *SettingsExportHandlers) canonicalSettingValue(key string, value any) (encoded, rejectType, detail string) {
	validation := func(detail string) (string, string, string) {
		return "", SlugValidationFailed, detail
	}
	switch key {
	case "download_rate_limit", "upload_rate_limit",
		"alt_download_rate_limit", "alt_upload_rate_limit",
		"max_active_total", "max_active_per_engine":
		n, ok := importJSONInt(value)
		if !ok || n < 0 {
			return validation("want a non-negative integer")
		}
		return fmt.Sprintf("%d", n), "", ""
	case "rss_interval_s":
		n, ok := importJSONInt(value)
		if !ok || n < rssIntervalSMinimum {
			return validation(fmt.Sprintf("want an integer of at least %d seconds", rssIntervalSMinimum))
		}
		return fmt.Sprintf("%d", n), "", ""
	case "schedule_enabled", "rss_enabled", "auto_extract", "confirm_on_delete":
		b, ok := value.(bool)
		if !ok {
			return validation("want a boolean")
		}
		return fmt.Sprintf("%t", b), "", ""
	case "process_order":
		if value != "by_date_created" {
			return validation("want the enum value \"by_date_created\"")
		}
		return `"by_date_created"`, "", ""
	case "default_destination":
		raw, ok := value.(string)
		if !ok {
			return validation("want an absolute path string")
		}
		resolved, err := fsx.ResolveDestination(h.roots, raw)
		if err != nil {
			return "", SlugPathRejected, "the path is outside every configured data root"
		}
		encoded, err := json.Marshal(resolved)
		if err != nil {
			return validation(fmt.Sprintf("encode destination: %v", err))
		}
		return string(encoded), "", ""
	case "min_free_space":
		return canonicalMinFreeSpace(value)
	default:
		return validation("unknown settings key")
	}
}

// canonicalMinFreeSpace validates the sparse root→bytes map of doc 11
// section 5: keys are absolute canonical paths (they need not be
// configured roots), values non-negative integers.
func canonicalMinFreeSpace(value any) (encoded, rejectType, detail string) {
	m, ok := value.(map[string]any)
	if !ok {
		return "", SlugValidationFailed, "want an object of absolute root path to bytes"
	}
	canonical := make(map[string]int64, len(m))
	for root, raw := range m {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return "", SlugValidationFailed, fmt.Sprintf("key %q is not an absolute canonical path", root)
		}
		n, ok := importJSONInt(raw)
		if !ok || n < 0 {
			return "", SlugValidationFailed, fmt.Sprintf("value for %q is not a non-negative integer", root)
		}
		canonical[root] = n
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", SlugValidationFailed, fmt.Sprintf("encode min_free_space: %v", err)
	}

	return string(raw), "", ""
}

// importJSONInt decodes a JSON number into int64, refusing fractions and
// out-of-range magnitudes.
func importJSONInt(value any) (int64, bool) {
	f, ok := value.(float64)
	if !ok || f != math.Trunc(f) || f < math.MinInt64 || f > math.MaxInt64 {
		return 0, false
	}

	return int64(f), true
}

// importCategories applies the categories member; save_path is re-jailed
// against the importing host's roots and a failure is path-rejected, never
// rewritten.
func (h *SettingsExportHandlers) importCategories(
	ctx context.Context,
	tx *sqlx.Tx,
	categories []ExportCategory,
	onConflict string,
	report *ImportReport,
) error {
	for _, category := range categories {
		if !validCategoryName(category.Name) {
			reject(report, collectionCategories, category.Name, SlugValidationFailed, "want a non-empty name without '/'")
			continue
		}
		savePath, err := fsx.ResolveDestination(h.roots, category.SavePath)
		if err != nil {
			reject(report, collectionCategories, category.Name, SlugPathRejected, "save_path is outside every configured data root")
			continue
		}

		var id string
		err = tx.GetContext(ctx, &id, queryCategoryIDByName, category.Name)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("match category %q: %w", category.Name, err)
		}
		matched := err == nil
		if !countRow(report, collectionCategories, matched, onConflict) {
			continue
		}
		now := time.Now().UnixMilli()
		if matched {
			if _, err := tx.ExecContext(ctx, queryUpdateCategoryPath, savePath, now, id); err != nil {
				return fmt.Errorf("update category %q: %w", category.Name, err)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, queryInsertCategory, store.NewID(store.PrefixCategory), category.Name, savePath, now, now); err != nil {
			return fmt.Errorf("insert category %q: %w", category.Name, err)
		}
	}

	return nil
}

// importIndexers applies the indexers member; the conflict key is
// definition_id when the member carries one, else name. api_key is not an
// export member, so an imported row keeps the stored key on overwrite and
// starts without one on insert.
func (h *SettingsExportHandlers) importIndexers(
	ctx context.Context,
	tx *sqlx.Tx,
	indexers []ExportIndexer,
	onConflict string,
	report *ImportReport,
) error {
	for _, indexer := range indexers {
		key := indexer.Name
		switch indexer.Kind {
		case indexerKindTorznab, indexerKindNewznab:
			if indexer.URL == nil || *indexer.URL == "" {
				reject(report, collectionIndexers, key, SlugValidationFailed, "a torznab or newznab indexer needs url")
				continue
			}
			if !search.ValidBaseURL(*indexer.URL) {
				reject(report, collectionIndexers, key, SlugValidationFailed, "url must be an absolute http or https URL")
				continue
			}
		case indexerKindDlsearch:
			if indexer.DefinitionID == nil || *indexer.DefinitionID == "" {
				reject(report, collectionIndexers, key, SlugValidationFailed, "a dlsearch indexer needs definition_id")
				continue
			}
		}
		settingsJSON, err := json.Marshal(indexer.Settings)
		if err != nil {
			return fmt.Errorf("encode indexer %q settings: %w", indexer.Name, err)
		}

		var id string
		matched := false
		if indexer.DefinitionID != nil && *indexer.DefinitionID != "" {
			err = tx.GetContext(ctx, &id, queryIndexerIDByDefinition, *indexer.DefinitionID)
		} else {
			err = tx.GetContext(ctx, &id, queryIndexerIDByName, indexer.Name)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("match indexer %q: %w", indexer.Name, err)
		}
		matched = err == nil
		if !countRow(report, collectionIndexers, matched, onConflict) {
			continue
		}
		now := time.Now().UnixMilli()
		if matched {
			if _, err := tx.ExecContext(
				ctx, queryUpdateIndexer,
				indexer.Name, indexer.Kind, indexer.Enabled, indexer.URL, indexer.DefinitionID,
				indexer.Priority, string(settingsJSON), now, id,
			); err != nil {
				return fmt.Errorf("update indexer %q: %w", indexer.Name, err)
			}
			continue
		}
		if _, err := tx.ExecContext(
			ctx, queryInsertIndexer,
			store.NewID(store.PrefixIndexer), indexer.Name, indexer.Kind, indexer.Enabled,
			indexer.URL, indexer.DefinitionID, indexer.Priority, string(settingsJSON), now, now,
		); err != nil {
			return fmt.Errorf("insert indexer %q: %w", indexer.Name, err)
		}
	}

	return nil
}

// importFeeds applies the feeds member; url is the conflict key and must be
// an absolute http or https URL — a __redacted__ rendering is a rendered
// form, not a fetchable address.
func (h *SettingsExportHandlers) importFeeds(
	ctx context.Context,
	tx *sqlx.Tx,
	feeds []ExportFeed,
	onConflict string,
	report *ImportReport,
) error {
	for _, feed := range feeds {
		if strings.Contains(feed.URL, redactedValue) {
			reject(report, collectionFeeds, feed.URL, SlugValidationFailed, "a __redacted__ url is not a fetchable address")
			continue
		}
		if err := checkFeedURL(feed.URL); err != nil {
			reject(report, collectionFeeds, feed.URL, SlugValidationFailed, feedURLDetail)
			continue
		}
		if feed.RefreshIntervalS != 0 && feed.RefreshIntervalS < feedRefreshIntervalMinimum {
			reject(report, collectionFeeds, feed.URL, SlugValidationFailed, feedRefreshIntervalDetail)
			continue
		}

		var id string
		err := tx.GetContext(ctx, &id, queryFeedIDByURL, feed.URL)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("match feed %q: %w", feed.URL, err)
		}
		matched := err == nil
		if !countRow(report, collectionFeeds, matched, onConflict) {
			continue
		}
		now := time.Now().UnixMilli()
		if matched {
			if _, err := tx.ExecContext(
				ctx, queryUpdateFeed,
				feed.Title, feed.Enabled, feed.RefreshIntervalS, feed.ItemCap, now, id,
			); err != nil {
				return fmt.Errorf("update feed %q: %w", feed.URL, err)
			}
			continue
		}
		if _, err := tx.ExecContext(
			ctx, queryInsertFeed,
			store.NewID(store.PrefixFeed), feed.URL, feed.Title, feed.Enabled,
			feed.RefreshIntervalS, feed.ItemCap, now, now, now,
		); err != nil {
			return fmt.Errorf("insert feed %q: %w", feed.URL, err)
		}
	}

	return nil
}

// importRules applies the rules member; name is the conflict key. The
// definition re-validates like a create's — the document's name, enabled
// and priority members are the mirrored columns, so the stored
// definition_json is rewritten to carry them before it lands.
func (h *SettingsExportHandlers) importRules(
	ctx context.Context,
	tx *sqlx.Tx,
	rules []ExportRule,
	onConflict string,
	report *ImportReport,
) error {
	for _, rule := range rules {
		raw, err := json.Marshal(rule.Definition)
		if err != nil {
			return fmt.Errorf("encode rule %q definition: %w", rule.Name, err)
		}
		var doc rss.RuleDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			reject(report, collectionRules, rule.Name, SlugValidationFailed, "the definition is not a rule document")
			continue
		}
		if doc.Name != "" && doc.Name != rule.Name {
			reject(report, collectionRules, rule.Name, SlugValidationFailed, "definition.name must equal the entry's name")
			continue
		}
		doc.Name = rule.Name
		doc.ApplyDefaults()
		enabled := rule.Enabled
		doc.Enabled = &enabled
		doc.Priority = rule.Priority
		if errs := doc.Validate(); len(errs) > 0 {
			messages := make([]string, 0, len(errs))
			for _, fieldErr := range errs {
				messages = append(messages, fieldErr.Location+": "+fieldErr.Message)
			}
			reject(report, collectionRules, rule.Name, SlugValidationFailed, strings.Join(messages, "; "))
			continue
		}
		definitionJSON, err := json.Marshal(doc)
		if err != nil {
			return fmt.Errorf("encode rule %q document: %w", rule.Name, err)
		}

		var id string
		err = tx.GetContext(ctx, &id, queryRuleIDByName, rule.Name)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("match rule %q: %w", rule.Name, err)
		}
		matched := err == nil
		if !countRow(report, collectionRules, matched, onConflict) {
			continue
		}
		now := time.Now().UnixMilli()
		if matched {
			if _, err := tx.ExecContext(
				ctx, queryUpdateRule, rule.Enabled, rule.Priority, string(definitionJSON), now, id,
			); err != nil {
				return fmt.Errorf("update rule %q: %w", rule.Name, err)
			}
			continue
		}
		if _, err := tx.ExecContext(
			ctx, queryInsertRule,
			store.NewID(store.PrefixRule), rule.Name, rule.Enabled, rule.Priority, string(definitionJSON), now, now,
		); err != nil {
			return fmt.Errorf("insert rule %q: %w", rule.Name, err)
		}
	}

	return nil
}

// importWatchFolders applies the watch_folders member; path and
// destination are re-jailed against the importing host's roots, and
// category resolves against the categories already applied in this
// transaction.
func (h *SettingsExportHandlers) importWatchFolders(
	ctx context.Context,
	tx *sqlx.Tx,
	folders []ExportWatch,
	onConflict string,
	report *ImportReport,
) error {
	for _, folder := range folders {
		path, err := fsx.ResolveDestination(h.roots, folder.Path)
		if err != nil {
			reject(report, collectionWatchFolders, folder.Path, SlugPathRejected, "path is outside every configured data root")
			continue
		}
		destination, err := fsx.ResolveDestination(h.roots, folder.Destination)
		if err != nil {
			reject(report, collectionWatchFolders, folder.Path, SlugPathRejected, "destination is outside every configured data root")
			continue
		}
		var categoryID *string
		if folder.Category != nil && *folder.Category != "" {
			var id string
			err := tx.GetContext(ctx, &id, queryCategoryIDByName, *folder.Category)
			if errors.Is(err, sql.ErrNoRows) {
				reject(report, collectionWatchFolders, folder.Path, SlugValidationFailed, "category is not a categories member or an existing row")
				continue
			}
			if err != nil {
				return fmt.Errorf("resolve watch folder %q category: %w", folder.Path, err)
			}
			categoryID = &id
		}

		var id string
		err = tx.GetContext(ctx, &id, queryWatchFolderIDByPath, path)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("match watch folder %q: %w", folder.Path, err)
		}
		matched := err == nil
		if !countRow(report, collectionWatchFolders, matched, onConflict) {
			continue
		}
		now := time.Now().UnixMilli()
		if matched {
			if _, err := tx.ExecContext(
				ctx, queryUpdateWatchFolder,
				folder.Enabled, destination, categoryID, folder.DeleteAfterLoad, folder.PollIntervalS, now, id,
			); err != nil {
				return fmt.Errorf("update watch folder %q: %w", folder.Path, err)
			}
			continue
		}
		if _, err := tx.ExecContext(
			ctx, queryInsertWatchFolder,
			store.NewID(store.PrefixWatchFolder), path, folder.Enabled, destination, categoryID,
			folder.DeleteAfterLoad, folder.PollIntervalS, now, now,
		); err != nil {
			return fmt.Errorf("insert watch folder %q: %w", folder.Path, err)
		}
	}

	return nil
}

// importSchedule applies the schedule member. Like settings it is scalar
// state outside the conflict keys — it always writes and always counts the
// one stored grid as updated. The cells land beside the schedule_enabled
// flag inside the import's transaction.
func (h *SettingsExportHandlers) importSchedule(
	ctx context.Context,
	tx *sqlx.Tx,
	schedule ExportSchedule,
	report *ImportReport,
) error {
	counts := report.Collections[collectionSchedule]
	counts.Updated++
	report.Collections[collectionSchedule] = counts

	if len(schedule.Cells) != 168 {
		return fmt.Errorf("import schedule: %d cells, want 168", len(schedule.Cells))
	}

	now := time.Now().UnixMilli()
	enabledJSON, err := json.Marshal(schedule.Enabled)
	if err != nil {
		return fmt.Errorf("encode schedule_enabled: %w", err)
	}
	if _, err := tx.ExecContext(
		ctx, queryUpsertSetting,
		store.NewID(store.PrefixSetting), "schedule_enabled", string(enabledJSON), now, now,
	); err != nil {
		return fmt.Errorf("write schedule_enabled: %w", err)
	}
	for i, cell := range schedule.Cells {
		mode, err := CellToMode(cell)
		if err != nil {
			return fmt.Errorf("import schedule cell %d: %w", i, err)
		}
		res, err := tx.ExecContext(ctx, queryReplaceScheduleCell, string(mode), now, i/24, i%24)
		if err != nil {
			return fmt.Errorf("write schedule cell %d: %w", i, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("write schedule cell %d: read rows affected: %w", i, err)
		}
		if affected != 1 {
			return fmt.Errorf("write schedule cell %d: updated %d rows, want 1", i, affected)
		}
	}

	return nil
}
