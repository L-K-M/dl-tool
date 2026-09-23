// Tests for GET /settings/export, POST /settings/import and the
// `dl-tool restore --from` store half of T108. The API tests run the real
// server over a migrated store; the restore tests drive store.RestoreFrom
// against real files, with the process lock standing in for a running
// server.
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"golang.org/x/sys/unix"

	"github.com/L-K-M/dl-tool/internal/rss"
	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

// seedSetting upserts one settings row directly, so a fixture can set a key
// without going through PATCH /settings.
func seedSetting(t *testing.T, db *sqlx.DB, key string, value any) {
	t.Helper()

	now := time.Now().UnixMilli()
	_, err := db.ExecContext(
		t.Context(),
		`INSERT INTO settings (id, key, value_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at`,
		store.NewID(store.PrefixSetting), key, mustJSON(t, value), now, now,
	)
	if err != nil {
		t.Fatalf("seed settings key %s: %v", key, err)
	}
}

// seedExportFixture writes one member of every exported collection into the
// env, so an export has something to carry. The rule's definition_json is
// the normalized form the importer writes, so a round trip reproduces it
// byte for byte.
func seedExportFixture(t *testing.T, env *tasksTestEnv) {
	t.Helper()
	ctx := t.Context()
	data := env.dataRoot

	seedSetting(t, env.db, "download_rate_limit", 12345)
	seedSetting(t, env.db, "upload_rate_limit", 0)
	seedSetting(t, env.db, "schedule_enabled", true)
	seedSetting(t, env.db, "default_destination", data)
	seedSetting(t, env.db, "min_free_space", map[string]int64{data: 1024})
	seedSetting(t, env.db, "rss_enabled", true)
	seedSetting(t, env.db, "rss_interval_s", 1800)
	seedSetting(t, env.db, "auto_extract", true)
	seedSetting(t, env.db, "confirm_on_delete", false)
	seedSetting(t, env.db, "process_order", "by_date_created")

	categoryID := store.NewID(store.PrefixCategory)
	if _, err := env.db.ExecContext(
		ctx,
		`INSERT INTO categories (id, name, save_path, created_at, updated_at) VALUES (?, ?, ?, 0, 0)`,
		categoryID, "iso", filepath.Join(data, "iso"),
	); err != nil {
		t.Fatalf("seed category: %v", err)
	}

	indexers, err := store.NewIndexerStore(env.db, secure.Secret("export-test-secret-key"))
	if err != nil {
		t.Fatalf("build indexer store: %v", err)
	}
	indexerSettings := `{"threads":2}`
	if _, err := indexers.Create(ctx, store.Indexer{
		Name:         "Internet Archive",
		Kind:         "dlsearch",
		Enabled:      true,
		DefinitionID: ptr("internet-archive"),
		Priority:     50,
		SettingsJSON: &indexerSettings,
	}, secure.Secret("indexer-key-secret-0123456789")); err != nil {
		t.Fatalf("seed indexer: %v", err)
	}

	if _, err := env.db.ExecContext(
		ctx,
		`INSERT INTO feeds (id, url, title, enabled, refresh_interval_s, item_cap, next_fetch_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0)`,
		store.NewID(store.PrefixFeed), "https://archlinux.org/feeds/releases/",
		"Arch Linux releases", true, 0, 50, 0,
	); err != nil {
		t.Fatalf("seed feed: %v", err)
	}

	rule := rss.RuleDoc{
		Name:   "Linux ISOs",
		Match:  rss.MatchSpec{AnyOf: []string{"*linux*"}},
		Action: rss.ActionSpec{Category: "iso"},
	}
	rule.ApplyDefaults()
	ruleEnabled := true
	rule.Enabled = &ruleEnabled
	rule.Priority = 10
	definition, err := json.Marshal(rule)
	if err != nil {
		t.Fatalf("encode rule definition: %v", err)
	}
	if _, err := env.db.ExecContext(
		ctx,
		`INSERT INTO rules (id, name, enabled, priority, definition_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 0, 0)`,
		store.NewID(store.PrefixRule), "Linux ISOs", true, 10, string(definition),
	); err != nil {
		t.Fatalf("seed rule: %v", err)
	}

	if _, err := env.db.ExecContext(
		ctx,
		`INSERT INTO watch_folders
(id, path, enabled, destination, category_id, delete_after_load, poll_interval_s, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0)`,
		store.NewID(store.PrefixWatchFolder), filepath.Join(data, "watch"), true,
		filepath.Join(data, "iso"), categoryID, true, 30,
	); err != nil {
		t.Fatalf("seed watch folder: %v", err)
	}

	cells := [168]store.ScheduleMode{}
	for i := range cells {
		cells[i] = store.ScheduleDefault
	}
	cells[0] = store.ScheduleAlternative
	cells[167] = store.ScheduleNoDownload
	if err := store.NewSettingsStore(env.db).ReplaceSchedule(ctx, true, cells); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
}

// exportDocument calls GET /settings/export and decodes the document.
func exportDocument(t *testing.T, env *tasksTestEnv) ExportDocument {
	t.Helper()

	response := env.api.Get("/settings/export", "Authorization: Bearer "+env.bearer)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /settings/export = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}

	var doc ExportDocument
	if err := json.Unmarshal(response.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode export document %q: %v", response.Body.String(), err)
	}

	return doc
}

// postImport calls POST /settings/import with the test bearer credential.
func postImport(t *testing.T, env *tasksTestEnv, body any) *httptest.ResponseRecorder {
	t.Helper()

	return env.api.Post("/settings/import", body, "Authorization: Bearer "+env.bearer)
}

// decodeImportReport decodes the report body of a 200 import response.
func decodeImportReport(t *testing.T, response *httptest.ResponseRecorder) ImportReport {
	t.Helper()

	if response.Code != http.StatusOK {
		t.Fatalf("POST /settings/import = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	var report ImportReport
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode import report %q: %v", response.Body.String(), err)
	}

	return report
}

// collectionRowCounts is the per-table row census a dry-run assertion
// compares before and after.
func collectionRowCounts(t *testing.T, db *sqlx.DB) map[string]int {
	t.Helper()

	counts := map[string]int{}
	for _, table := range []string{
		"settings", "categories", "indexers", "feeds", "rules", "watch_folders", "bandwidth_schedule",
	} {
		var n int
		if err := db.GetContext(t.Context(), &n, `SELECT COUNT(*) FROM `+table); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		counts[table] = n
	}

	return counts
}

// TestExportExcludesEverySecret is the acceptance test of "an export is
// safe to attach to a bug report": every secret carrier is seeded, and none
// of its values — nor the secret member names — may appear in the document.
func TestExportExcludesEverySecret(t *testing.T) {
	env := newTasksTestEnv(t)
	ctx := t.Context()

	var userID, passwordHash string
	if err := env.db.QueryRowContext(ctx, `SELECT id, password_hash FROM users`).Scan(&userID, &passwordHash); err != nil {
		t.Fatalf("read seeded user: %v", err)
	}
	_, _, sessionID := seedLiveSession(t, env.db, userID)
	apiToken := seedAPIToken(t, env.db, userID, nil, nil)

	const indexerKey = "indexer-key-secret-0123456789"
	const engineSecret = "engine-secret-marker-9f8e7d6c"
	const channelSecret = "channel-secret-marker-5a4b3c2d"
	const extractPassword = "extract-secret-marker-1a2b3c"
	const taskMarker = "task-marker-row-must-not-export"

	indexers, err := store.NewIndexerStore(env.db, secure.Secret("export-test-secret-key"))
	if err != nil {
		t.Fatalf("build indexer store: %v", err)
	}
	if _, err := indexers.Create(ctx, store.Indexer{
		Name: "secret-bearing", Kind: "torznab", URL: ptr("https://tracker.example.com/api"), Priority: 50,
	}, secure.Secret(indexerKey)); err != nil {
		t.Fatalf("seed indexer: %v", err)
	}

	if _, err := env.db.ExecContext(
		ctx,
		`INSERT INTO engines (id, kind, name, enabled, url, secret_enc, created_at, updated_at)
VALUES (?, 'aria2', 'aria2', 1, 'http://localhost:6800/jsonrpc', ?, 0, 0)`,
		store.NewID("eng_"), engineSecret,
	); err != nil {
		t.Fatalf("seed engine: %v", err)
	}
	if _, err := env.db.ExecContext(
		ctx,
		`INSERT INTO notification_channels (id, kind, name, enabled, config_json, secret_enc, created_at, updated_at)
VALUES (?, 'webhook', 'hook', 1, '{}', ?, 0, 0)`,
		store.NewID("nch_"), channelSecret,
	); err != nil {
		t.Fatalf("seed notification channel: %v", err)
	}
	seedSetting(t, env.db, "extract_passwords", []string{extractPassword})
	if _, err := env.db.ExecContext(
		ctx,
		`INSERT INTO tasks (id, engine, source_kind, name, destination, state, added_at, created_at, updated_at)
VALUES (?, 'aria2', 'http', ?, ?, 'queued', 0, 0, 0)`,
		store.NewID(store.PrefixTask), taskMarker, env.dataRoot,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	doc := exportDocument(t, env)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode export document: %v", err)
	}
	// The secret-bearing indexer itself must be present, otherwise the
	// exclusion checks below pass vacuously.
	if !strings.Contains(string(raw), "secret-bearing") {
		t.Fatal("seeded indexer missing from export; the secret checks would pass vacuously")
	}

	for _, marker := range []string{
		passwordHash, sessionID, apiToken, indexerKey, engineSecret, channelSecret,
		extractPassword, taskMarker,
	} {
		if strings.Contains(string(raw), marker) {
			t.Errorf("export document contains secret marker %q", marker)
		}
	}
	for _, absent := range []string{
		`"api_key"`, `"password_hash"`, `"extract_passwords"`, `"secret_enc"`, `"sessions"`, `"tasks"`,
	} {
		if strings.Contains(string(raw), absent) {
			t.Errorf("export document contains excluded member %s", absent)
		}
	}
	if doc.DocumentVersion != exportDocumentVersion {
		t.Errorf("document_version = %d, want %d", doc.DocumentVersion, exportDocumentVersion)
	}
	if doc.SchemaVersion == 0 {
		t.Error("schema_version = 0, want the applied migration version")
	}
	if len(doc.Schedule.Cells) != 168 {
		t.Errorf("schedule cells = %d, want 168", len(doc.Schedule.Cells))
	}
}

// TestExportImportRoundTrip exports a populated instance, imports the
// document into an empty one — whose data roots include the source's, so
// the recorded paths resolve — and compares a second export member for
// member.
func TestExportImportRoundTrip(t *testing.T) {
	source := newTasksTestEnv(t)
	seedExportFixture(t, source)

	targetData := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(targetData, 0o755); err != nil {
		t.Fatalf("make target data root: %v", err)
	}
	target := newTasksTestEnvWithRoots(t, targetData, []string{targetData, source.dataRoot})

	doc := exportDocument(t, source)
	report := decodeImportReport(t, postImport(t, target, map[string]any{
		"document": doc,
		"dry_run":  false,
	}))
	if report.Totals.Rejected != 0 || len(report.Rejected) != 0 {
		t.Fatalf("import rejected rows: %+v", report.Rejected)
	}
	if report.Totals.Created == 0 {
		t.Error("import created nothing, want the fixture rows")
	}

	reexported := exportDocument(t, target)
	doc.ExportedAt = time.Time{}
	reexported.ExportedAt = time.Time{}

	before, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode source document: %v", err)
	}
	after, err := json.Marshal(reexported)
	if err != nil {
		t.Fatalf("encode re-exported document: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("round-trip mismatch:\nexported:   %s\nreexported: %s", before, after)
	}
}

// TestDryRunWritesNothing proves a dry run leaves every collection
// untouched while reporting exactly the counts the committing call then
// produces from the same starting state.
func TestDryRunWritesNothing(t *testing.T) {
	source := newTasksTestEnv(t)
	seedExportFixture(t, source)

	targetData := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(targetData, 0o755); err != nil {
		t.Fatalf("make target data root: %v", err)
	}
	target := newTasksTestEnvWithRoots(t, targetData, []string{targetData, source.dataRoot})
	doc := exportDocument(t, source)

	before := collectionRowCounts(t, target.db)
	dry := decodeImportReport(t, postImport(t, target, map[string]any{
		"document": doc,
		"dry_run":  true,
	}))
	if !dry.DryRun {
		t.Error("dry_run report flag = false, want true")
	}
	if dry.Totals.Created == 0 {
		t.Error("dry run reports no creations, want the fixture rows counted")
	}
	if after := collectionRowCounts(t, target.db); !maps.Equal(before, after) {
		t.Errorf("dry run wrote rows: before %v, after %v", before, after)
	}

	committed := decodeImportReport(t, postImport(t, target, map[string]any{
		"document": doc,
		"dry_run":  false,
	}))
	if committed.DryRun {
		t.Error("committing report flag dry_run = true, want false")
	}
	if committed.Totals != dry.Totals {
		t.Errorf("committing totals %+v differ from dry-run totals %+v", committed.Totals, dry.Totals)
	}
	if len(committed.Rejected) != len(dry.Rejected) {
		t.Errorf("committing rejected %+v differs from dry-run rejected %+v", committed.Rejected, dry.Rejected)
	}
	for collection, counts := range committed.Collections {
		if dry.Collections[collection] != counts {
			t.Errorf("collection %s: committing counts %+v differ from dry-run %+v",
				collection, counts, dry.Collections[collection])
		}
	}
}

// TestImportIsTransactional proves a rejected member never lands while the
// accepted members of the same document still commit — and that the
// rejected row is reported with its problem type.
func TestImportIsTransactional(t *testing.T) {
	source := newTasksTestEnv(t)
	seedExportFixture(t, source)

	targetData := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(targetData, 0o755); err != nil {
		t.Fatalf("make target data root: %v", err)
	}
	target := newTasksTestEnvWithRoots(t, targetData, []string{targetData, source.dataRoot})
	doc := exportDocument(t, source)
	doc.WatchFolders = append(doc.WatchFolders, ExportWatch{
		Path:          "/outside/every/root",
		Enabled:       true,
		Destination:   filepath.Join(source.dataRoot, "iso"),
		PollIntervalS: 10,
	})

	report := decodeImportReport(t, postImport(t, target, map[string]any{
		"document": doc,
		"dry_run":  false,
	}))
	if report.Collections["watch_folders"].Rejected != 1 || report.Totals.Rejected != 1 || len(report.Rejected) != 1 {
		t.Fatalf("report %+v, want exactly one rejected watch_folders row", report)
	}
	row := report.Rejected[0]
	if row.Collection != "watch_folders" || row.Type != SlugPathRejected || row.Key != "/outside/every/root" {
		t.Errorf("rejected row %+v, want watch_folders path-rejected on the outside path", row)
	}

	var orphan int
	if err := target.db.GetContext(
		t.Context(), &orphan,
		`SELECT COUNT(*) FROM watch_folders WHERE path = ?`, "/outside/every/root",
	); err != nil {
		t.Fatalf("count rejected watch folder: %v", err)
	}
	if orphan != 0 {
		t.Error("rejected watch folder landed in the database")
	}
	var accepted int
	if err := target.db.GetContext(
		t.Context(), &accepted,
		`SELECT COUNT(*) FROM categories WHERE name = 'iso'`,
	); err != nil {
		t.Fatalf("count accepted category: %v", err)
	}
	if accepted != 1 {
		t.Error("accepted category did not land")
	}
}

// TestImportConflictModes proves skip keeps the stored row and overwrite
// replaces it, on the categories.name conflict key.
func TestImportConflictModes(t *testing.T) {
	source := newTasksTestEnv(t)
	seedExportFixture(t, source)

	targetData := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(targetData, 0o755); err != nil {
		t.Fatalf("make target data root: %v", err)
	}
	target := newTasksTestEnvWithRoots(t, targetData, []string{targetData, source.dataRoot})
	ctx := t.Context()

	existingPath := filepath.Join(targetData, "existing")
	if _, err := target.db.ExecContext(
		ctx,
		`INSERT INTO categories (id, name, save_path, created_at, updated_at) VALUES (?, 'iso', ?, 0, 0)`,
		store.NewID(store.PrefixCategory), existingPath,
	); err != nil {
		t.Fatalf("seed conflicting category: %v", err)
	}

	doc := exportDocument(t, source)
	skip := decodeImportReport(t, postImport(t, target, map[string]any{
		"document":    doc,
		"dry_run":     false,
		"on_conflict": "skip",
	}))
	if skip.Collections["categories"].Skipped != 1 {
		t.Fatalf("skip report %+v, want categories.skipped = 1", skip.Collections["categories"])
	}
	var stored string
	if err := target.db.GetContext(ctx, &stored, `SELECT save_path FROM categories WHERE name = 'iso'`); err != nil {
		t.Fatalf("read category: %v", err)
	}
	if stored != existingPath {
		t.Errorf("skip overwrote save_path to %q, want %q", stored, existingPath)
	}

	over := decodeImportReport(t, postImport(t, target, map[string]any{
		"document":    doc,
		"dry_run":     false,
		"on_conflict": "overwrite",
	}))
	if over.Collections["categories"].Updated != 1 {
		t.Fatalf("overwrite report %+v, want categories.updated = 1", over.Collections["categories"])
	}
	if err := target.db.GetContext(ctx, &stored, `SELECT save_path FROM categories WHERE name = 'iso'`); err != nil {
		t.Fatalf("read category after overwrite: %v", err)
	}
	want := filepath.Join(source.dataRoot, "iso")
	if stored != want {
		t.Errorf("overwrite left save_path %q, want %q", stored, want)
	}
}

// TestNewerDocumentVersionConflict proves a document_version the binary
// does not understand is refused with 409 /problems/conflict.
func TestNewerDocumentVersionConflict(t *testing.T) {
	env := newTasksTestEnv(t)

	doc := map[string]any{
		"document_version": exportDocumentVersion + 1,
		"exported_at":      time.Now().UTC().Format(time.RFC3339),
		"schema_version":   1,
		"settings":         map[string]any{},
		"categories":       []any{},
		"indexers":         []any{},
		"feeds":            []any{},
		"rules":            []any{},
		"watch_folders":    []any{},
		"schedule":         map[string]any{"enabled": false, "cells": make([]int, 168)},
	}
	response := postImport(t, env, map[string]any{"document": doc})
	assertProblem(t, response, http.StatusConflict, SlugConflict)
}

// TestImportRejectsMalformedDocument proves a malformed document is 422
// /problems/validation-failed with errors[].location naming the collection
// and index.
func TestImportRejectsMalformedDocument(t *testing.T) {
	env := newTasksTestEnv(t)

	doc := map[string]any{
		"document_version": exportDocumentVersion,
		"exported_at":      time.Now().UTC().Format(time.RFC3339),
		"schema_version":   1,
		"settings":         map[string]any{},
		"categories":       []any{map[string]any{"name": "", "save_path": "/x"}},
		"indexers":         []any{},
		"feeds":            []any{},
		"rules":            []any{},
		"watch_folders":    []any{},
		"schedule":         map[string]any{"enabled": false, "cells": make([]int, 168)},
	}
	response := postImport(t, env, map[string]any{"document": doc})
	problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	found := false
	for _, detail := range problem.Errors {
		if strings.Contains(detail.Location, "categories[0]") {
			found = true
		}
	}
	if !found {
		t.Errorf("errors %+v name no categories[0] location", problem.Errors)
	}
}

// --- restore tests: store.RestoreFrom against real files ---

// restoreFixture is a closed-over migrated database plus its paths. db is
// kept open only until the fixture is populated; tests close it before a
// succeeding restore so no handle outlives the rename.
type restoreFixture struct {
	dbPath    string
	configDir string
	backupDir string
	db        *sqlx.DB
}

func newRestoreFixture(t *testing.T) *restoreFixture {
	t.Helper()

	root := t.TempDir()
	fixture := &restoreFixture{
		configDir: filepath.Join(root, "config"),
		backupDir: filepath.Join(root, "backups"),
	}
	fixture.dbPath = filepath.Join(fixture.configDir, "dl-tool.db")

	db, err := store.Open(t.Context(), fixture.dbPath, fixture.backupDir)
	if err != nil {
		t.Fatalf("open live store: %v", err)
	}
	fixture.db = db
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close live store: %v", err)
		}
	})

	return fixture
}

// seedMarkerCategory writes the row every refusal test asserts survives.
func (f *restoreFixture) seedMarker(t *testing.T) {
	t.Helper()

	if _, err := f.db.ExecContext(
		t.Context(),
		`INSERT INTO categories (id, name, save_path, created_at, updated_at) VALUES (?, 'committed-marker', '/data', 0, 0)`,
		store.NewID(store.PrefixCategory),
	); err != nil {
		t.Fatalf("seed marker category: %v", err)
	}
}

// markerIntact proves the live database still answers with the seeded row.
func (f *restoreFixture) markerIntact(t *testing.T) {
	t.Helper()

	var n int
	if err := f.db.GetContext(
		t.Context(), &n,
		`SELECT COUNT(*) FROM categories WHERE name = 'committed-marker'`,
	); err != nil {
		t.Fatalf("read marker after refusal: %v", err)
	}
	if n != 1 {
		t.Error("live database lost its marker row")
	}
}

// makeBackup produces a self-contained copy of db at path, the artifact
// VACUUM INTO leaves: no sidecars, every committed byte inside the file.
func makeBackup(t *testing.T, db *sqlx.DB, path string) {
	t.Helper()

	if _, err := db.ExecContext(t.Context(), "VACUUM INTO ?", path); err != nil {
		t.Fatalf("vacuum into %q: %v", path, err)
	}
}

// TestRestoreRefusesRunningServer takes the stable process lock the way a
// live server holds it and proves the restore refuses with
// restore_server_running, naming the recorded PID.
func TestRestoreRefusesRunningServer(t *testing.T) {
	fixture := newRestoreFixture(t)
	fixture.seedMarker(t)

	lockPath := fixture.dbPath + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open process lock: %v", err)
	}
	t.Cleanup(func() {
		if err := lockFile.Close(); err != nil {
			t.Errorf("close process lock: %v", err)
		}
	})
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("flock process lock: %v", err)
	}
	if _, err := lockFile.WriteString("4242\n"); err != nil {
		t.Fatalf("record pid in process lock: %v", err)
	}

	backup := filepath.Join(fixture.configDir, "backup.db")
	makeBackup(t, fixture.db, backup)

	_, err = store.RestoreFrom(t.Context(), fixture.dbPath, fixture.configDir, backup)
	if !errors.Is(err, store.ErrRestoreServerRunning) {
		t.Fatalf("RestoreFrom err = %v, want restore_server_running", err)
	}
	if !strings.Contains(err.Error(), "4242") {
		t.Errorf("refusal %q does not name the recorded PID", err)
	}
	fixture.markerIntact(t)
}

// TestRestoreRejectsForeignSource covers the restore_source_rejected gate:
// a path outside DLTOOL_CONFIG_DIR and the live database, its lock and its
// sidecar names are all refused, the live database untouched.
func TestRestoreRejectsForeignSource(t *testing.T) {
	fixture := newRestoreFixture(t)
	fixture.seedMarker(t)

	outside := filepath.Join(t.TempDir(), "outside.db")
	makeBackup(t, fixture.db, outside)

	// The sidecar names are refused whether the files exist (a live WAL
	// store has them) or not (EvalSymlinks fails the same gate).
	for name, src := range map[string]string{
		"outside config dir": outside,
		"the live database":  fixture.dbPath,
		"the process lock":   fixture.dbPath + ".lock",
		"the wal sidecar":    fixture.dbPath + "-wal",
		"the shm sidecar":    fixture.dbPath + "-shm",
		"a directory":        fixture.configDir,
	} {
		if _, err := store.RestoreFrom(t.Context(), fixture.dbPath, fixture.configDir, src); !errors.Is(err, store.ErrRestoreSourceRejected) {
			t.Errorf("restore from %s: err = %v, want restore_source_rejected", name, err)
		}
	}

	// A symlink inside the config directory pointing out of it resolves
	// out and is rejected the same way.
	link := filepath.Join(fixture.configDir, "linked.db")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if _, err := store.RestoreFrom(t.Context(), fixture.dbPath, fixture.configDir, link); !errors.Is(err, store.ErrRestoreSourceRejected) {
		t.Errorf("restore through symlink: err = %v, want restore_source_rejected", err)
	}

	fixture.markerIntact(t)
}

// TestRestoreRefusesSchemaTooNew builds a backup whose applied schema is
// one past the embedded maximum and proves the refusal names both versions
// while the live database stays untouched.
func TestRestoreRefusesSchemaTooNew(t *testing.T) {
	fixture := newRestoreFixture(t)
	fixture.seedMarker(t)
	ctx := t.Context()

	var embedded int64
	if err := fixture.db.GetContext(
		ctx, &embedded,
		`SELECT MAX(version_id) FROM goose_db_version WHERE is_applied = 1`,
	); err != nil {
		t.Fatalf("read embedded version: %v", err)
	}

	backup := filepath.Join(fixture.configDir, "future.db")
	makeBackup(t, fixture.db, backup)
	bdb, err := sqlx.Open("sqlite", "file:"+backup)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	if _, err := bdb.ExecContext(
		ctx,
		`UPDATE goose_db_version SET version_id = ? WHERE version_id = ?`,
		embedded+1, embedded,
	); err != nil {
		t.Fatalf("bump backup schema version: %v", err)
	}
	if err := bdb.Close(); err != nil {
		t.Fatalf("close backup: %v", err)
	}

	_, err = store.RestoreFrom(ctx, fixture.dbPath, fixture.configDir, backup)
	if !errors.Is(err, store.ErrRestoreSchemaTooNew) {
		t.Fatalf("RestoreFrom err = %v, want restore_schema_too_new", err)
	}
	for _, want := range []string{"schema version", "embedded maximum"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not print %q", err, want)
		}
	}
	fixture.markerIntact(t)
}

// TestRestoreRefusesCorrupt proves a file that is not a database is refused
// with restore_integrity_failed and the live database stays untouched.
func TestRestoreRefusesCorrupt(t *testing.T) {
	fixture := newRestoreFixture(t)
	fixture.seedMarker(t)

	corrupt := filepath.Join(fixture.configDir, "corrupt.db")
	if err := os.WriteFile(corrupt, []byte("definitely not a sqlite database"), 0o600); err != nil {
		t.Fatalf("write corrupt source: %v", err)
	}

	if _, err := store.RestoreFrom(t.Context(), fixture.dbPath, fixture.configDir, corrupt); !errors.Is(err, store.ErrRestoreIntegrity) {
		t.Fatalf("RestoreFrom err = %v, want restore_integrity_failed", err)
	}
	fixture.markerIntact(t)
}

// TestRestoreAcceptsOlderSchema restores a pre-migration backup — a valid
// database whose only table is goose_db_version at version 0 — and proves
// the next open migrates it forward.
func TestRestoreAcceptsOlderSchema(t *testing.T) {
	fixture := newRestoreFixture(t)
	fixture.seedMarker(t)
	ctx := t.Context()

	current, err := store.SchemaVersion(ctx, fixture.db)
	if err != nil {
		t.Fatalf("read live schema version: %v", err)
	}

	older := filepath.Join(fixture.configDir, "older.db")
	odb, err := sqlx.Open("sqlite", "file:"+older)
	if err != nil {
		t.Fatalf("open older backup: %v", err)
	}
	if _, err := odb.ExecContext(
		ctx,
		`CREATE TABLE goose_db_version (id INTEGER PRIMARY KEY, version_id INTEGER NOT NULL, is_applied INTEGER NOT NULL, tstamp TIMESTAMP);
INSERT INTO goose_db_version (version_id, is_applied) VALUES (0, 1)`,
	); err != nil {
		t.Fatalf("build older backup: %v", err)
	}
	if err := odb.Close(); err != nil {
		t.Fatalf("close older backup: %v", err)
	}

	if err := fixture.db.Close(); err != nil {
		t.Fatalf("close live store: %v", err)
	}

	tasks, err := store.RestoreFrom(ctx, fixture.dbPath, fixture.configDir, older)
	if err != nil {
		t.Fatalf("RestoreFrom older backup: %v", err)
	}
	if tasks != 0 {
		t.Errorf("restored task count = %d, want 0 for a pre-tasks schema", tasks)
	}

	// The replaced live database survives as <name>.replaced-<UTC>.bak.
	matches, err := filepath.Glob(
		filepath.Join(fixture.configDir, filepath.Base(fixture.dbPath)+".replaced-*.bak"),
	)
	if err != nil || len(matches) == 0 {
		t.Fatalf("no replaced-database backup: %v, matches %v", err, matches)
	}
	bak, err := sqlx.Open("sqlite", "file:"+matches[0]+"?mode=ro")
	if err != nil {
		t.Fatalf("open replaced backup: %v", err)
	}
	var marker int
	if err := bak.GetContext(ctx, &marker, `SELECT COUNT(*) FROM categories WHERE name = 'committed-marker'`); err != nil {
		t.Fatalf("read marker from replaced backup: %v", err)
	}
	if marker != 1 {
		t.Error("replaced-database backup lost the marker row")
	}
	if err := bak.Close(); err != nil {
		t.Fatalf("close replaced backup: %v", err)
	}

	// The next open migrates the older schema forward to the embedded
	// maximum.
	restored, err := store.Open(ctx, fixture.dbPath, fixture.backupDir)
	if err != nil {
		t.Fatalf("open restored database: %v", err)
	}
	t.Cleanup(func() {
		if err := restored.Close(); err != nil {
			t.Errorf("close restored store: %v", err)
		}
	})
	version, err := store.SchemaVersion(ctx, restored)
	if err != nil {
		t.Fatalf("read restored schema version: %v", err)
	}
	if version != current {
		t.Errorf("restored schema migrated to %d, want the embedded %d", version, current)
	}
	var taskRows int
	if err := restored.GetContext(ctx, &taskRows, `SELECT COUNT(*) FROM tasks`); err != nil {
		t.Fatalf("read tasks on migrated restore: %v", err)
	}
	if taskRows != 0 {
		t.Errorf("migrated restore holds %d tasks, want 0 from the pre-tasks schema", taskRows)
	}
}

// TestRestoreFailureLeavesOriginal injects a failure inside the preserve
// step — an uncommitted writer on a second handle makes
// wal_checkpoint(TRUNCATE) report busy — and proves the original database
// is intact and no staged copy is left behind.
func TestRestoreFailureLeavesOriginal(t *testing.T) {
	fixture := newRestoreFixture(t)
	fixture.seedMarker(t)
	ctx := t.Context()

	backup := filepath.Join(fixture.configDir, "backup.db")
	makeBackup(t, fixture.db, backup)

	blocker, err := sqlx.Open("sqlite", "file:"+fixture.dbPath)
	if err != nil {
		t.Fatalf("open blocking handle: %v", err)
	}
	blocker.SetMaxOpenConns(1)
	tx, err := blocker.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatalf("begin blocking transaction: %v", err)
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO categories (id, name, save_path, created_at, updated_at) VALUES (?, 'pending', '/data', 0, 0)`,
		store.NewID(store.PrefixCategory),
	); err != nil {
		t.Fatalf("write inside blocking transaction: %v", err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("rollback blocking transaction: %v", err)
		}
		if err := blocker.Close(); err != nil {
			t.Errorf("close blocking handle: %v", err)
		}
	})

	if _, err := store.RestoreFrom(ctx, fixture.dbPath, fixture.configDir, backup); err == nil {
		t.Fatal("RestoreFrom succeeded despite the checkpoint-blocking writer")
	}

	fixture.markerIntact(t)
	staged, err := filepath.Glob(filepath.Join(fixture.configDir, "*.restore-*.tmp"))
	if err != nil {
		t.Fatalf("glob staged copies: %v", err)
	}
	if len(staged) != 0 {
		t.Errorf("staged copies left behind: %v", staged)
	}
}

// TestRestoreReportsTaskCount restores a backup containing tasks and
// proves the reported count is the source's, not the live database's.
func TestRestoreReportsTaskCount(t *testing.T) {
	fixture := newRestoreFixture(t)
	fixture.seedMarker(t)
	ctx := t.Context()

	scratchPath := filepath.Join(t.TempDir(), "source.db")
	source, err := store.Open(ctx, scratchPath, fixture.backupDir)
	if err != nil {
		t.Fatalf("open scratch source: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := source.ExecContext(
			ctx,
			`INSERT INTO tasks (id, engine, source_kind, name, destination, state, added_at, created_at, updated_at)
VALUES (?, 'aria2', 'http', ?, '/data', 'queued', 0, 0, 0)`,
			store.NewID(store.PrefixTask), "task",
		); err != nil {
			t.Fatalf("seed source task: %v", err)
		}
	}
	backup := filepath.Join(fixture.configDir, "source.bak")
	makeBackup(t, source, backup)
	if err := source.Close(); err != nil {
		t.Fatalf("close scratch source: %v", err)
	}
	if err := fixture.db.Close(); err != nil {
		t.Fatalf("close live store: %v", err)
	}

	tasks, err := store.RestoreFrom(ctx, fixture.dbPath, fixture.configDir, backup)
	if err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}
	if tasks != 2 {
		t.Errorf("restored task count = %d, want 2", tasks)
	}

	restored, err := sqlx.Open("sqlite", "file:"+fixture.dbPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open restored database: %v", err)
	}
	var n int
	if err := restored.GetContext(ctx, &n, `SELECT COUNT(*) FROM tasks`); err != nil {
		t.Fatalf("count restored tasks: %v", err)
	}
	if err := restored.Close(); err != nil {
		t.Fatalf("close restored database: %v", err)
	}
	if n != 2 {
		t.Errorf("restored database holds %d tasks, want 2", n)
	}
}

// TestImportRowRejections exercises the three row-level rejection paths
// through the API: a settings value past int64's maximum, a category
// name duplicated inside one document — conflict-matched by name, so
// skipped under the default mode rather than written — and a watch
// folder naming a category that exists nowhere.
func TestImportRowRejections(t *testing.T) {
	env := newTasksTestEnv(t)
	root := env.dataRoot

	doc := map[string]any{
		"document_version": exportDocumentVersion,
		"exported_at":      time.Now().UTC().Format(time.RFC3339),
		"schema_version":   1,
		"settings":         map[string]any{"max_active_total": float64(1 << 63)},
		"categories": []any{
			map[string]any{"name": "dup", "save_path": filepath.Join(root, "one")},
			map[string]any{"name": "dup", "save_path": filepath.Join(root, "two")},
		},
		"indexers": []any{},
		"feeds":    []any{},
		"rules":    []any{},
		"watch_folders": []any{
			map[string]any{
				"path": filepath.Join(root, "watch"), "enabled": true,
				"destination": filepath.Join(root, "dest"), "category": "ghost",
				"delete_after_load": false, "poll_interval_s": 10,
			},
		},
		"schedule": map[string]any{"enabled": false, "cells": make([]int, 168)},
	}

	report := decodeImportReport(t, postImport(t, env, map[string]any{
		"document": doc,
		"dry_run":  false,
	}))

	if got := report.Collections["settings"].Rejected; got != 1 {
		t.Errorf("settings rejected = %d, want 1 for the overflowing max_active_total", got)
	}
	if got := report.Collections["categories"].Skipped; got != 1 {
		t.Errorf("categories skipped = %d, want 1 for the in-document duplicate", got)
	}
	if got := report.Collections["watch_folders"].Rejected; got != 1 {
		t.Errorf("watch_folders rejected = %d, want 1 for the unknown category", got)
	}
	var overflow, unknownCategory bool
	for _, row := range report.Rejected {
		switch {
		case row.Collection == "settings" && row.Key == "max_active_total" && row.Type == SlugValidationFailed:
			overflow = true
		case row.Collection == "watch_folders" && row.Type == SlugValidationFailed:
			unknownCategory = true
		}
	}
	if !overflow {
		t.Errorf("no validation-failed rejection in %+v for the overflowing setting", report.Rejected)
	}
	if !unknownCategory {
		t.Errorf("no validation-failed rejection in %+v for the unknown category", report.Rejected)
	}

	var n int
	if err := env.db.GetContext(t.Context(), &n, `SELECT COUNT(*) FROM categories WHERE name = 'dup'`); err != nil {
		t.Fatalf("count dup categories: %v", err)
	}
	if n != 1 {
		t.Errorf("categories named dup = %d, want exactly the accepted row", n)
	}
}

// TestRejectRowErrorMapsSchemaConstraints drives rejectRowError with two
// real SQLite failures produced inside a scratch transaction: a UNIQUE
// violation maps to /problems/conflict, a CHECK violation to
// /problems/validation-failed, and both roll back the tentative
// created/updated count the importer already booked for the row.
func TestRejectRowErrorMapsSchemaConstraints(t *testing.T) {
	env := newTasksTestEnv(t)
	ctx := t.Context()

	report := &ImportReport{Collections: map[string]Counts{}}
	tx, err := env.db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatalf("begin scratch transaction: %v", err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("rollback scratch transaction: %v", err)
		}
	})

	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(
		ctx, queryInsertCategory, store.NewID(store.PrefixCategory), "dup", "/data", now, now,
	); err != nil {
		t.Fatalf("insert first category: %v", err)
	}
	countRow(report, collectionCategories, false, conflictSkip)
	_, uniqueErr := tx.ExecContext(
		ctx, queryInsertCategory, store.NewID(store.PrefixCategory), "dup", "/data", now, now,
	)
	if uniqueErr == nil {
		t.Fatal("duplicate category insert did not fail")
	}
	if !rejectRowError(report, collectionCategories, "dup", false, uniqueErr) {
		t.Fatalf("rejectRowError declined a UNIQUE violation: %v", uniqueErr)
	}

	countRow(report, collectionWatchFolders, true, conflictOverwrite)
	_, checkErr := tx.ExecContext(
		ctx, queryInsertWatchFolder,
		store.NewID(store.PrefixWatchFolder), "/watch", 5, "/dest", nil, 0, 10, now, now,
	)
	if checkErr == nil {
		t.Fatal("watch_folders enabled=5 did not fail its CHECK")
	}
	if !rejectRowError(report, collectionWatchFolders, "/watch", true, checkErr) {
		t.Fatalf("rejectRowError declined a CHECK violation: %v", checkErr)
	}

	categories := report.Collections[collectionCategories]
	if categories.Created != 0 || categories.Rejected != 1 {
		t.Errorf("categories counts = %+v, want created 0 rejected 1", categories)
	}
	folders := report.Collections[collectionWatchFolders]
	if folders.Updated != 0 || folders.Rejected != 1 {
		t.Errorf("watch_folders counts = %+v, want updated 0 rejected 1", folders)
	}
	if len(report.Rejected) != 2 {
		t.Fatalf("rejected rows = %+v, want 2", report.Rejected)
	}
	if report.Rejected[0].Type != SlugConflict {
		t.Errorf("UNIQUE rejection type = %q, want %q", report.Rejected[0].Type, SlugConflict)
	}
	if report.Rejected[1].Type != SlugValidationFailed {
		t.Errorf("CHECK rejection type = %q, want %q", report.Rejected[1].Type, SlugValidationFailed)
	}
}

// TestRestoreRefusesWhileServerLockHeld proves the two ends of the
// process lock meet: a server-side holder — stage S3's
// AcquireProcessLock — trips the restore's restore_server_running gate,
// a second server acquire fails with database_locked, and releasing the
// lock frees the restore.
func TestRestoreRefusesWhileServerLockHeld(t *testing.T) {
	fixture := newRestoreFixture(t)
	fixture.seedMarker(t)
	ctx := t.Context()

	backup := filepath.Join(fixture.configDir, "backup.db")
	makeBackup(t, fixture.db, backup)

	serverLock, err := store.AcquireProcessLock(fixture.dbPath)
	if err != nil {
		t.Fatalf("acquire process lock: %v", err)
	}

	if _, err := store.AcquireProcessLock(fixture.dbPath); !errors.Is(err, store.ErrDatabaseLocked) {
		t.Errorf("second server acquire: err = %v, want database_locked", err)
	}
	if _, err := store.RestoreFrom(ctx, fixture.dbPath, fixture.configDir, backup); !errors.Is(err, store.ErrRestoreServerRunning) {
		t.Fatalf("restore under held lock: err = %v, want restore_server_running", err)
	}

	serverLock.Release()
	if _, err := store.RestoreFrom(ctx, fixture.dbPath, fixture.configDir, backup); err != nil {
		t.Fatalf("restore after lock release: %v", err)
	}
}

// TestRestoreReplacesCorruptDatabase covers the disaster case: the live
// database is unreadable, VACUUM INTO cannot preserve it, and the
// restore still completes — the wreck is byte-copied into the
// .replaced-*.bak and the staged backup installed.
func TestRestoreReplacesCorruptDatabase(t *testing.T) {
	fixture := newRestoreFixture(t)
	fixture.seedMarker(t)
	ctx := t.Context()

	backup := filepath.Join(fixture.configDir, "backup.db")
	makeBackup(t, fixture.db, backup)

	// Truncate the live database to garbage; its handle is never used
	// again, the file replacement is what matters.
	if err := os.WriteFile(fixture.dbPath, []byte("corrupt beyond readability"), 0o600); err != nil {
		t.Fatalf("corrupt live database: %v", err)
	}

	tasks, err := store.RestoreFrom(ctx, fixture.dbPath, fixture.configDir, backup)
	if err != nil {
		t.Fatalf("RestoreFrom over a corrupt database: %v", err)
	}
	if tasks != 0 {
		t.Errorf("restored task count = %d, want 0", tasks)
	}

	matches, err := filepath.Glob(
		filepath.Join(fixture.configDir, filepath.Base(fixture.dbPath)+".replaced-*.bak"),
	)
	if err != nil || len(matches) == 0 {
		t.Fatalf("no replaced-database preserve: %v, matches %v", err, matches)
	}

	restored, err := sqlx.Open("sqlite", "file:"+fixture.dbPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open restored database: %v", err)
	}
	var n int
	if err := restored.GetContext(ctx, &n, `SELECT COUNT(*) FROM categories WHERE name = 'committed-marker'`); err != nil {
		t.Fatalf("read marker from restored database: %v", err)
	}
	if err := restored.Close(); err != nil {
		t.Fatalf("close restored database: %v", err)
	}
	if n != 1 {
		t.Error("restored database lost the backup's marker row")
	}
}
