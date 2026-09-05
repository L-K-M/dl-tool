package store

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/oklog/ulid/v2"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

const (
	initialSchemaVersion = int64(1)
	newerSchemaVersion   = int64(999)
	daysPerWeek          = 7
	hoursPerDay          = 24
	bandwidthCellCount   = daysPerWeek * hoursPerDay
	generatedIDCount     = 10_000
)

var (
	expectedTables = []string{
		"api_tokens", "bandwidth_schedule", "categories", "engines", "feed_items",
		"feeds", "indexers", "jobs", "notification_channels", "rule_matches",
		"rule_seen_episodes", "rules", "search_jobs", "search_results", "sessions",
		"settings", "tags", "task_events", "task_files", "task_tags", "task_trackers",
		"tasks", "ui_prefs", "users", "watch_folders",
	}
	expectedIndexes = []string{
		"idx_api_tokens_user", "idx_bandwidth_schedule_cell", "idx_feed_items_hash",
		"idx_feed_items_identity", "idx_feed_items_norm", "idx_feed_items_pub",
		"idx_feed_items_read", "idx_feeds_next_fetch", "idx_indexers_definition",
		"idx_jobs_claim", "idx_notification_channels_enabled", "idx_rule_matches_hash",
		"idx_rule_matches_key", "idx_rule_matches_rule", "idx_search_jobs_created",
		"idx_search_results_job", "idx_sessions_expires", "idx_sessions_user",
		"idx_task_events_at", "idx_task_events_task", "idx_task_files_idx",
		"idx_task_tags_tag", "idx_task_trackers_url", "idx_tasks_category",
		"idx_tasks_engine_ref", "idx_tasks_infohash_v1", "idx_tasks_infohash_v2",
		"idx_tasks_state", "idx_tasks_updated", "idx_ui_prefs_key",
	}
)

func TestPragmas(t *testing.T) {
	db, _, _ := openTestStore(t)

	var journalMode string
	require.NoError(t, db.GetContext(t.Context(), &journalMode, "PRAGMA journal_mode"))
	require.Equal(t, "wal", journalMode)

	var busyTimeout int
	require.NoError(t, db.GetContext(t.Context(), &busyTimeout, "PRAGMA busy_timeout"))
	require.Equal(t, 5_000, busyTimeout)

	var synchronous int
	require.NoError(t, db.GetContext(t.Context(), &synchronous, "PRAGMA synchronous"))
	require.Equal(t, 1, synchronous)

	var foreignKeys int
	require.NoError(t, db.GetContext(t.Context(), &foreignKeys, "PRAGMA foreign_keys"))
	require.Equal(t, 1, foreignKeys)
	require.Equal(t, databaseConnectionLimit, db.Stats().MaxOpenConnections)
}

func TestInitialMigration(t *testing.T) {
	db, _, _ := openTestStore(t)

	version, err := SchemaVersion(t.Context(), db)
	require.NoError(t, err)
	require.Equal(t, initialSchemaVersion, version)

	var tables []string
	require.NoError(t, db.SelectContext(t.Context(), &tables, `SELECT name
FROM sqlite_schema
WHERE type = 'table' AND name NOT GLOB 'sqlite_*' AND name <> 'goose_db_version'
ORDER BY name`))
	require.Equal(t, expectedTables, tables)

	var indexes []string
	require.NoError(t, db.SelectContext(t.Context(), &indexes, `SELECT name
FROM sqlite_schema
WHERE type = 'index' AND name NOT GLOB 'sqlite_*'
ORDER BY name`))
	require.Equal(t, expectedIndexes, indexes)
}

func TestMigrationDownThenUp(t *testing.T) {
	db, _, _ := openTestStore(t)

	require.NoError(t, goose.DownContext(t.Context(), db.DB, migrationsDirectory))
	version, err := SchemaVersion(t.Context(), db)
	require.NoError(t, err)
	require.Zero(t, version)

	require.NoError(t, goose.UpContext(t.Context(), db.DB, migrationsDirectory))
	version, err = SchemaVersion(t.Context(), db)
	require.NoError(t, err)
	require.Equal(t, initialSchemaVersion, version)

	var cells int
	require.NoError(t, db.GetContext(t.Context(), &cells, "SELECT COUNT(id) FROM bandwidth_schedule"))
	require.Equal(t, bandwidthCellCount, cells)
}

func TestTaskFilePriorityConstraint(t *testing.T) {
	db, _, _ := openTestStore(t)
	_, err := db.ExecContext(t.Context(), `INSERT INTO tasks
(id, engine, source_kind, name, state, destination, added_at, created_at, updated_at)
VALUES (?, 'aria2', 'http', 'fixture', 'queued', '/data', 0, 0, 0)`, "tsk_fixture")
	require.NoError(t, err)

	for index, priority := range []int{0, 1, 6, 7} {
		_, err := db.ExecContext(t.Context(), `INSERT INTO task_files
(id, task_id, file_index, path, priority, created_at, updated_at)
VALUES (?, 'tsk_fixture', ?, ?, ?, 0, 0)`, NewID(PrefixTaskFile), index, fmt.Sprint(index), priority)
		require.NoError(t, err)
	}

	for index, priority := range []int{-1, 2, 5, 8} {
		_, err := db.ExecContext(t.Context(), `INSERT INTO task_files
(id, task_id, file_index, path, priority, created_at, updated_at)
VALUES (?, 'tsk_fixture', ?, ?, ?, 0, 0)`, NewID(PrefixTaskFile), index+10, fmt.Sprint(index), priority)
		require.ErrorContains(t, err, "CHECK constraint failed")
	}
}

func TestInitialSeedIDs(t *testing.T) {
	db, _, _ := openTestStore(t)

	var settings []seededSetting
	require.NoError(t, db.SelectContext(t.Context(), &settings, `SELECT id, key, value_json, created_at, updated_at
FROM settings
ORDER BY id`))
	require.Equal(t, []seededSetting{
		{ID: "set_00000000000000000000000001", Key: "max_active_total", ValueJSON: "5"},
		{ID: "set_00000000000000000000000002", Key: "max_active_per_engine", ValueJSON: "3"},
		{ID: "set_00000000000000000000000003", Key: "min_free_space", ValueJSON: "{}"},
	}, settings)
	assertUniquePrefixedIDs(t, settingIDs(settings), PrefixSetting)

	var cells []seededBandwidthCell
	require.NoError(t, db.SelectContext(t.Context(), &cells, `SELECT id, day, hour, mode, created_at, updated_at
FROM bandwidth_schedule
ORDER BY day, hour`))
	require.Len(t, cells, bandwidthCellCount)

	cellIDs := make([]string, 0, len(cells))
	for index, cell := range cells {
		expectedDay := index / hoursPerDay
		expectedHour := index % hoursPerDay
		expectedID := fmt.Sprintf("bws_00000000000000000000000%03d", index)

		require.Equal(t, expectedID, cell.ID)
		require.Equal(t, expectedDay, cell.Day)
		require.Equal(t, expectedHour, cell.Hour)
		require.Equal(t, "default", cell.Mode)
		require.Zero(t, cell.CreatedAt)
		require.Zero(t, cell.UpdatedAt)

		cellIDs = append(cellIDs, cell.ID)
	}
	assertUniquePrefixedIDs(t, cellIDs, PrefixBandwidthCell)
}

func TestSchemaNewerThanBinaryRefuses(t *testing.T) {
	db, dbPath, backupDir := openTestStoreWithoutCleanup(t)
	_, err := db.ExecContext(
		t.Context(),
		"UPDATE goose_db_version SET version_id = ? WHERE version_id = ? AND is_applied = 1",
		newerSchemaVersion,
		initialSchemaVersion,
	)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	opened, err := Open(t.Context(), dbPath, backupDir)
	require.Nil(t, opened)
	require.ErrorContains(t, err, "applied schema version 999")
	require.ErrorContains(t, err, "embedded version 1")
	assertNoBackupArtifacts(t, backupDir)
}

func TestNetworkFilesystemRefused(t *testing.T) {
	for _, filesystem := range []string{"nfs", "nfs4", "cifs", "smb3", "fuse.sshfs"} {
		t.Run(filesystem, func(t *testing.T) {
			dbPath, backupDir := testStorePaths(t)
			mountInfo := fmt.Sprintf("1 0 0:1 / / rw - %s source rw\n", filesystem)

			db, err := open(t.Context(), dbPath, backupDir, strings.NewReader(mountInfo))
			require.Nil(t, db)
			require.ErrorContains(t, err, "config_network_fs")
			require.ErrorContains(t, err, filepath.Dir(dbPath))
			require.ErrorContains(t, err, filesystem)
			require.NoFileExists(t, dbPath)
		})
	}

	t.Run("resolved directory symlink", func(t *testing.T) {
		root := t.TempDir()
		mountPoint := filepath.Join(root, "network-config")
		require.NoError(t, os.Mkdir(mountPoint, databaseDirectoryMode))

		configLink := filepath.Join(root, "config")
		require.NoError(t, os.Symlink(mountPoint, configLink))
		dbPath := filepath.Join(configLink, "dl-tool.db")
		mountInfo := fmt.Sprintf(
			"1 0 0:1 / / rw - ext4 /dev/root rw\n2 1 0:2 / %s rw - nfs source rw\n",
			mountPoint,
		)

		db, err := open(t.Context(), dbPath, filepath.Join(root, "backups"), strings.NewReader(mountInfo))
		require.Nil(t, db)
		require.ErrorContains(t, err, "nfs")
		require.NoFileExists(t, filepath.Join(mountPoint, "dl-tool.db"))
	})
}

func TestUnrecognisedDatabaseRefused(t *testing.T) {
	tests := []struct {
		name       string
		objectName string
		createSQL  string
		checkSQL   string
	}{
		{
			name:       "table",
			objectName: "foreign_table",
			createSQL:  "CREATE TABLE foreign_table (marker TEXT NOT NULL); INSERT INTO foreign_table (marker) VALUES ('kept')",
			checkSQL:   "SELECT marker FROM foreign_table",
		},
		{
			name:       "view",
			objectName: "foreign_view",
			createSQL:  "CREATE VIEW foreign_view AS SELECT 'kept' AS marker",
			checkSQL:   "SELECT marker FROM foreign_view",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dbPath, backupDir := testStorePaths(t)
			rawDB := openRawDatabase(t, dbPath)
			_, err := rawDB.ExecContext(t.Context(), test.createSQL)
			require.NoError(t, err)
			require.NoError(t, rawDB.Close())

			opened, err := Open(t.Context(), dbPath, backupDir)
			require.Nil(t, opened)
			require.ErrorContains(t, err, test.objectName)
			require.ErrorContains(t, err, "move the foreign database file aside")

			rawDB = openRawDatabase(t, dbPath)
			t.Cleanup(func() {
				require.NoError(t, rawDB.Close())
			})

			var marker string
			require.NoError(t, rawDB.GetContext(t.Context(), &marker, test.checkSQL))
			require.Equal(t, "kept", marker)

			var gooseTables int
			require.NoError(t, rawDB.GetContext(
				t.Context(),
				&gooseTables,
				"SELECT COUNT(name) FROM sqlite_schema WHERE type = 'table' AND name = ?",
				gooseVersionTable,
			))
			require.Zero(t, gooseTables)
		})
	}
}

func TestZeroVersionSkipsBackup(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{name: "nonexistent", prepare: func(*testing.T, string) {}},
		{name: "empty file", prepare: createEmptyDatabase},
		{name: "goose version zero", prepare: createGooseVersionZero},
		{name: "empty goose history", prepare: createEmptyGooseVersionTable},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dbPath, backupDir := testStorePaths(t)
			test.prepare(t, dbPath)

			db, err := Open(t.Context(), dbPath, backupDir)
			require.NoError(t, err)
			require.NoError(t, db.Close())
			assertNoBackupArtifacts(t, backupDir)
		})
	}
}

func TestCurrentVersionSkipsBackup(t *testing.T) {
	db, dbPath, backupDir := openTestStoreWithoutCleanup(t)
	require.NoError(t, db.Close())

	db, err := Open(t.Context(), dbPath, backupDir)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	assertNoBackupArtifacts(t, backupDir)
}

func TestIntegrityCheckResult(t *testing.T) {
	dbPath := "/config/dl-tool.db"
	require.NoError(t, validateIntegrityResults(dbPath, []string{"ok"}))

	for _, results := range [][]string{nil, {}, {"corrupt"}, {"OK"}, {"ok", "ok"}} {
		err := validateIntegrityResults(dbPath, results)
		require.ErrorContains(t, err, integrityCheckFailureCode)
		require.ErrorContains(t, err, dbPath)
	}
}

func TestPreMigrationBackup(t *testing.T) {
	db, dbPath, backupDir := openTestStore(t)
	timestamps := []struct {
		start time.Time
		name  string
	}{
		{
			start: time.Date(2026, time.September, 1, 12, 0, 0, 123_456_789, time.UTC),
			name:  "dl-tool.db.pre-migration-1-to-2.20260901T120000.123456789Z.bak",
		},
		{
			start: time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC),
			name:  "dl-tool.db.pre-migration-1-to-2.20260901T120000.000000000Z.bak",
		},
	}

	for _, test := range timestamps {
		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))

		path, err := preMigrationBackup(
			t.Context(),
			db,
			dbPath,
			backupDir,
			initialSchemaVersion,
			initialSchemaVersion+1,
			test.start,
			logger,
		)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(backupDir, test.name), path)
		require.FileExists(t, path)
		require.Contains(t, logs.String(), path)
		require.NoError(t, checkDatabaseFileIntegrity(t.Context(), path))
		require.Equal(t, databaseFileMode, fileMode(t, path))
	}

	entries, err := os.ReadDir(backupDir)
	require.NoError(t, err)
	require.Len(t, entries, len(timestamps))
	for _, entry := range entries {
		require.True(t, strings.HasSuffix(entry.Name(), ".bak"), entry.Name())
	}
}

func TestDatabaseAndSidecarPermissions(t *testing.T) {
	t.Setenv("UMASK", "002")
	oldUmask := syscall.Umask(0o002)
	t.Cleanup(func() {
		syscall.Umask(oldUmask)
	})

	dbPath, backupDir := testStorePaths(t)
	configDir := filepath.Dir(dbPath)
	require.NoError(t, os.MkdirAll(configDir, 0o777))
	require.NoError(t, os.Chmod(configDir, 0o777))

	db, err := Open(t.Context(), dbPath, backupDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	require.Equal(t, databaseDirectoryMode, fileMode(t, configDir))
	require.Equal(t, databaseFileMode, fileMode(t, dbPath))

	tx, err := db.BeginTxx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), "UPDATE settings SET updated_at = 1 WHERE key = ?", "max_active_total")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	require.Equal(t, databaseFileMode, fileMode(t, dbPath+"-wal"))
	require.Equal(t, databaseFileMode, fileMode(t, dbPath+"-shm"))

	existingDBPath, existingBackupDir := testStorePaths(t)
	createEmptyDatabase(t, existingDBPath)
	require.NoError(t, os.Chmod(existingDBPath, 0o666))
	existingDB, err := Open(t.Context(), existingDBPath, existingBackupDir)
	require.NoError(t, err)
	require.Equal(t, databaseFileMode, fileMode(t, existingDBPath))
	require.NoError(t, existingDB.Close())
}

func TestDatabasePathIsLiteral(t *testing.T) {
	for _, name := range []string{"state?.db", "state#history.db", "state%3f.db", "state%2fother.db", "state?mode=memory", "stäte 1.db"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dbPath := filepath.Join(root, name)
			backupDir := filepath.Join(root, "backups?")
			db, err := Open(t.Context(), dbPath, backupDir)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })

			var openedPath string
			require.NoError(t, db.GetContext(t.Context(), &openedPath, "SELECT file FROM pragma_database_list WHERE name = 'main'"))
			require.Equal(t, dbPath, openedPath)
			require.Equal(t, databaseFileMode, fileMode(t, openedPath))

			backup, err := preMigrationBackup(t.Context(), db, dbPath, backupDir,
				initialSchemaVersion, initialSchemaVersion+1, time.Now().UTC(), slog.Default())
			require.NoError(t, err)
			require.NoError(t, checkDatabaseFileIntegrity(t.Context(), backup))
			entries, err := os.ReadDir(backupDir)
			require.NoError(t, err)
			require.Len(t, entries, 1, "integrity checks must not create alternate files")
		})
	}
}

func TestDatabasePathLeadingSlashes(t *testing.T) {
	for _, prefix := range []string{"/", "///"} {
		dbPath, backupDir := testStorePaths(t)
		dbPath = prefix + dbPath
		db, err := Open(t.Context(), dbPath, backupDir)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })

		var openedPath string
		require.NoError(t, db.GetContext(t.Context(), &openedPath, "SELECT file FROM pragma_database_list WHERE name = 'main'"))
		require.Equal(t, filepath.Clean(dbPath), openedPath)
		require.NoError(t, checkDatabaseFileIntegrity(t.Context(), dbPath))
	}
}

func TestBackupIntegrityChecksLiteralPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt?name.db")
	require.NoError(t, os.WriteFile(path, []byte("not a database"), databaseFileMode))
	require.Error(t, checkDatabaseFileIntegrity(t.Context(), path))
}

func TestUnsafeDatabasePathRefused(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		dbPath, backupDir := testStorePaths(t)
		targetPath := filepath.Join(t.TempDir(), "target.db")
		require.NoError(t, os.WriteFile(targetPath, []byte("untouched"), databaseFileMode))
		require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), databaseDirectoryMode))
		require.NoError(t, os.Symlink(targetPath, dbPath))

		db, err := Open(t.Context(), dbPath, backupDir)
		require.Nil(t, db)
		require.ErrorContains(t, err, "symlink")
		contents, readErr := os.ReadFile(targetPath)
		require.NoError(t, readErr)
		require.Equal(t, "untouched", string(contents))
	})

	t.Run("non-regular", func(t *testing.T) {
		dbPath, backupDir := testStorePaths(t)
		require.NoError(t, os.MkdirAll(dbPath, databaseDirectoryMode))

		db, err := Open(t.Context(), dbPath, backupDir)
		require.Nil(t, db)
		require.ErrorContains(t, err, "not a regular file")
	})
}

func TestNewID(t *testing.T) {
	ids := make(map[string]struct{}, generatedIDCount)
	for range generatedIDCount {
		id := NewID(PrefixTask)
		assertPrefixedULID(t, id, PrefixTask)

		_, exists := ids[id]
		require.False(t, exists, id)
		ids[id] = struct{}{}
	}
}

type seededSetting struct {
	ID        string `db:"id"`
	Key       string `db:"key"`
	ValueJSON string `db:"value_json"`
	CreatedAt int64  `db:"created_at"`
	UpdatedAt int64  `db:"updated_at"`
}

type seededBandwidthCell struct {
	ID        string `db:"id"`
	Day       int    `db:"day"`
	Hour      int    `db:"hour"`
	Mode      string `db:"mode"`
	CreatedAt int64  `db:"created_at"`
	UpdatedAt int64  `db:"updated_at"`
}

func openTestStore(t *testing.T) (*sqlx.DB, string, string) {
	t.Helper()

	db, dbPath, backupDir := openTestStoreWithoutCleanup(t)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	return db, dbPath, backupDir
}

func openTestStoreWithoutCleanup(t *testing.T) (*sqlx.DB, string, string) {
	t.Helper()

	dbPath, backupDir := testStorePaths(t)
	db, err := Open(t.Context(), dbPath, backupDir)
	require.NoError(t, err)

	return db, dbPath, backupDir
}

func testStorePaths(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()

	return filepath.Join(root, "config", "dl-tool.db"), filepath.Join(root, "backups")
}

func openRawDatabase(t *testing.T, dbPath string) *sqlx.DB {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), databaseDirectoryMode))
	db, err := sqlx.Open(sqliteDriver, dbPath)
	require.NoError(t, err)
	require.NoError(t, db.PingContext(t.Context()))

	return db
}

func createEmptyDatabase(t *testing.T, dbPath string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), databaseDirectoryMode))
	require.NoError(t, os.WriteFile(dbPath, nil, databaseFileMode))
}

func createGooseVersionZero(t *testing.T, dbPath string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), databaseDirectoryMode))
	db := openRawDatabase(t, dbPath)
	require.NoError(t, prepareMigrations())
	version, err := goose.EnsureDBVersionContext(t.Context(), db.DB)
	require.NoError(t, err)
	require.Zero(t, version)
	require.NoError(t, db.Close())
}

func createEmptyGooseVersionTable(t *testing.T, dbPath string) {
	t.Helper()

	createGooseVersionZero(t, dbPath)
	db := openRawDatabase(t, dbPath)
	_, err := db.ExecContext(t.Context(), "DELETE FROM goose_db_version")
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

func settingIDs(settings []seededSetting) []string {
	ids := make([]string, 0, len(settings))
	for _, setting := range settings {
		ids = append(ids, setting.ID)
	}

	return ids
}

func assertUniquePrefixedIDs(t *testing.T, ids []string, prefix string) {
	t.Helper()

	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		assertPrefixedULID(t, id, prefix)

		_, exists := seen[id]
		require.False(t, exists, id)
		seen[id] = struct{}{}
	}
}

func assertPrefixedULID(t *testing.T, id, prefix string) {
	t.Helper()

	require.Len(t, id, len(prefix)+ulid.EncodedSize)
	require.True(t, strings.HasPrefix(id, prefix), id)
	_, err := ulid.ParseStrict(strings.TrimPrefix(id, prefix))
	require.NoError(t, err)
}

func assertNoBackupArtifacts(t *testing.T, backupDir string) {
	t.Helper()

	entries, err := os.ReadDir(backupDir)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	require.NoError(t, err)
	require.Empty(t, entries)
}

func fileMode(t *testing.T, path string) fs.FileMode {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)

	return info.Mode().Perm()
}
