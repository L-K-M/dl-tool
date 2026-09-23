package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/oklog/ulid/v2"
	"github.com/pressly/goose/v3"
	"golang.org/x/sys/unix"
	"modernc.org/sqlite"
)

const (
	sqliteDriver              = "sqlite"
	sqliteDialect             = "sqlite3"
	migrationsDirectory       = "migrations"
	gooseVersionTable         = "goose_db_version"
	mountInfoPath             = "/proc/self/mountinfo"
	databaseDirectoryMode     = fs.FileMode(0o700)
	databaseFileMode          = fs.FileMode(0o600)
	databaseConnectionLimit   = 1
	backupTimestampFormat     = "20060102T150405.000000000Z"
	integrityCheckFailureCode = "integrity_check_failed"
	databaseDSNFormat         = "file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_txlock=immediate"
)

const (
	querySchemaVersion = `SELECT MAX(version_id) FROM goose_db_version WHERE is_applied = 1`
	querySchemaObjects = `SELECT name
FROM sqlite_schema
WHERE NOT (type = 'table' AND name = ?) AND name NOT GLOB 'sqlite_*'
ORDER BY name`
	queryIntegrityCheck = `PRAGMA integrity_check`
)

var (
	//go:embed migrations/*.sql
	embedMigrations embed.FS

	// ErrNotFound is returned when no row with the given ID exists.
	ErrNotFound = errors.New("store: not found")

	migrationMu sync.Mutex // Goose configuration is process-wide.
)

// Open prepares, migrates and verifies the SQLite database.
func Open(ctx context.Context, dbPath, backupDir string) (*sqlx.DB, error) {
	mountInfo, err := os.ReadFile(mountInfoPath)
	if err != nil {
		return nil, fmt.Errorf("store: read mount information: %w", err)
	}

	return open(ctx, dbPath, backupDir, bytes.NewReader(mountInfo))
}

func open(ctx context.Context, dbPath, backupDir string, mountInfo io.Reader) (*sqlx.DB, error) {
	if err := prepareDatabasePath(dbPath); err != nil {
		return nil, err
	}

	if err := refuseNetworkFilesystem(filepath.Dir(dbPath), mountInfo); err != nil {
		return nil, err
	}

	db, err := openDatabase(ctx, dbPath)
	if err != nil {
		return nil, err
	}

	migrationMu.Lock()
	defer migrationMu.Unlock()

	if err := prepareMigrations(); err != nil {
		return closeAfterError(db, err)
	}

	schema, err := readSchemaState(ctx, db)
	if err != nil {
		return closeAfterError(db, err)
	}

	embeddedVersion, err := highestEmbeddedVersion()
	if err != nil {
		return closeAfterError(db, err)
	}

	if schema.version > embeddedVersion {
		err := fmt.Errorf(
			"store: applied schema version %d is newer than embedded version %d",
			schema.version,
			embeddedVersion,
		)

		return closeAfterError(db, err)
	}

	if err := migrate(ctx, db, dbPath, backupDir, schema, embeddedVersion); err != nil {
		return closeAfterError(db, err)
	}

	if err := checkIntegrity(ctx, db, dbPath); err != nil {
		return closeAfterError(db, err)
	}

	return db, nil
}

func prepareDatabasePath(dbPath string) error {
	if err := prepareDatabaseDirectory(filepath.Dir(dbPath)); err != nil {
		return err
	}

	info, err := os.Lstat(dbPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: inspect database path %q: %w", dbPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("store: database path %q is a symlink", dbPath)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("store: database path %q is not a regular file", dbPath)
	}

	if err := os.Chmod(dbPath, databaseFileMode); err != nil {
		return fmt.Errorf("store: secure database file %q: %w", dbPath, err)
	}

	return nil
}

// prepareDatabaseDirectory creates the database directory with the
// 0700 data mode, or tightens an existing one. The process-lock path
// needs it too: the lock sits beside the database, and the lock is
// acquired before Open ever runs on a first boot.
func prepareDatabaseDirectory(directory string) error {
	if err := os.MkdirAll(directory, databaseDirectoryMode); err != nil {
		return fmt.Errorf("store: create database directory %q: %w", directory, err)
	}
	if err := os.Chmod(directory, databaseDirectoryMode); err != nil {
		return fmt.Errorf("store: secure database directory %q: %w", directory, err)
	}

	return nil
}

func openDatabase(ctx context.Context, dbPath string) (*sqlx.DB, error) {
	// Pre-create securely so SQLite derives secure WAL and SHM modes.
	if err := createDatabaseFileIfMissing(dbPath); err != nil {
		return nil, err
	}

	dsn := fmt.Sprintf(databaseDSNFormat, escapedDatabasePath(dbPath))
	db, err := sqlx.Open(sqliteDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open database %q: %w", dbPath, err)
	}

	db.SetMaxOpenConns(databaseConnectionLimit)
	db.SetMaxIdleConns(databaseConnectionLimit)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		return closeAfterError(db, fmt.Errorf("store: connect to database %q: %w", dbPath, err))
	}

	if err := secureOpenedDatabase(dbPath); err != nil {
		return closeAfterError(db, err)
	}

	return db, nil
}

// escapedDatabasePath keeps filename punctuation out of SQLite URI options.
func escapedDatabasePath(dbPath string) string {
	// Linux treats leading slashes alike; SQLite interprets // as an authority.
	if strings.HasPrefix(dbPath, "//") {
		dbPath = "/" + strings.TrimLeft(dbPath, "/")
	}
	return (&url.URL{Path: dbPath}).EscapedPath()
}

func createDatabaseFileIfMissing(dbPath string) error {
	file, err := os.OpenFile(dbPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, databaseFileMode)
	if errors.Is(err, os.ErrExist) {
		return secureOpenedDatabase(dbPath)
	}
	if err != nil {
		return fmt.Errorf("store: create database file %q: %w", dbPath, err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("store: close new database file %q: %w", dbPath, err)
	}

	return nil
}

func secureOpenedDatabase(dbPath string) error {
	info, err := os.Lstat(dbPath)
	if err != nil {
		return fmt.Errorf("store: inspect opened database %q: %w", dbPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("store: opened database path %q is not a regular file", dbPath)
	}

	if err := os.Chmod(dbPath, databaseFileMode); err != nil {
		return fmt.Errorf("store: secure opened database %q: %w", dbPath, err)
	}

	return nil
}

func prepareMigrations() error {
	goose.SetBaseFS(embedMigrations)
	if err := goose.SetDialect(sqliteDialect); err != nil {
		return fmt.Errorf("store: set migration dialect: %w", err)
	}

	return nil
}

func highestEmbeddedVersion() (int64, error) {
	migrations, err := goose.CollectMigrations(migrationsDirectory, 0, goose.MaxVersion)
	if err != nil {
		return 0, fmt.Errorf("store: collect embedded migrations: %w", err)
	}

	latest, err := migrations.Last()
	if err != nil {
		return 0, fmt.Errorf("store: find highest embedded migration: %w", err)
	}

	return latest.Version, nil
}

func migrate(
	ctx context.Context,
	db *sqlx.DB,
	dbPath string,
	backupDir string,
	schema schemaState,
	embeddedVersion int64,
) error {
	if schema.version == embeddedVersion {
		return nil
	}

	if schema.version > 0 {
		if _, err := preMigrationBackup(
			ctx,
			db,
			dbPath,
			backupDir,
			schema.version,
			embeddedVersion,
			time.Now().UTC(),
			slog.Default(),
		); err != nil {
			return err
		}
	}

	// Goose cannot initialize an existing empty version table.
	if schema.versionTable && !schema.appliedRowPresent {
		if _, err := db.ExecContext(
			ctx,
			"INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, ?)",
			0,
			true,
		); err != nil {
			return fmt.Errorf("store: initialize empty goose version history: %w", err)
		}
	}

	if err := goose.UpContext(ctx, db.DB, migrationsDirectory); err != nil {
		return fmt.Errorf("store: apply migrations: %w", err)
	}

	return nil
}

// SchemaVersion returns the newest applied goose migration.
func SchemaVersion(ctx context.Context, db *sqlx.DB) (int64, error) {
	schema, err := readSchemaState(ctx, db)
	if err != nil {
		return 0, err
	}

	return schema.version, nil
}

type schemaState struct {
	version           int64
	versionTable      bool
	appliedRowPresent bool
}

func readSchemaState(ctx context.Context, db *sqlx.DB) (schemaState, error) {
	var tableName string
	err := db.GetContext(
		ctx,
		&tableName,
		`SELECT name
FROM sqlite_schema
WHERE type = 'table' AND name = ?`,
		gooseVersionTable,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return schemaState{}, refuseUnrecognisedSchema(ctx, db)
	}
	if err != nil {
		return schemaState{}, fmt.Errorf("store: inspect goose version table: %w", err)
	}

	var version sql.NullInt64
	if err := db.GetContext(ctx, &version, querySchemaVersion); err != nil {
		return schemaState{}, fmt.Errorf("store: read schema version: %w", err)
	}

	state := schemaState{
		versionTable:      true,
		appliedRowPresent: version.Valid,
	}
	if version.Valid {
		state.version = version.Int64
	}
	if state.version < 0 {
		return schemaState{}, fmt.Errorf("store: invalid applied schema version %d", state.version)
	}
	if state.version > 0 {
		return state, nil
	}

	if err := refuseUnrecognisedSchema(ctx, db); err != nil {
		return schemaState{}, err
	}

	return state, nil
}

func refuseUnrecognisedSchema(ctx context.Context, db *sqlx.DB) error {
	var objects []string
	if err := db.SelectContext(ctx, &objects, querySchemaObjects, gooseVersionTable); err != nil {
		return fmt.Errorf("store: inspect schema objects: %w", err)
	}
	if len(objects) == 0 {
		return nil
	}

	return fmt.Errorf(
		"store: unrecognised schema objects %q; move the foreign database file aside",
		objects,
	)
}

func checkIntegrity(ctx context.Context, db *sqlx.DB, dbPath string) error {
	var results []string
	if err := db.SelectContext(ctx, &results, queryIntegrityCheck); err != nil {
		return fmt.Errorf("%s: database %q: %w", integrityCheckFailureCode, dbPath, err)
	}

	return validateIntegrityResults(dbPath, results)
}

func validateIntegrityResults(dbPath string, results []string) error {
	if len(results) == 1 && results[0] == "ok" {
		return nil
	}

	return fmt.Errorf(
		"%s: database %q returned %d result rows: %q",
		integrityCheckFailureCode,
		dbPath,
		len(results),
		results,
	)
}

func preMigrationBackup(
	ctx context.Context,
	db *sqlx.DB,
	dbPath string,
	backupDir string,
	fromVersion int64,
	toVersion int64,
	startedAt time.Time,
	logger *slog.Logger,
) (string, error) {
	if err := os.MkdirAll(backupDir, databaseDirectoryMode); err != nil {
		return "", fmt.Errorf("store: create backup directory %q: %w", backupDir, err)
	}
	if err := os.Chmod(backupDir, databaseDirectoryMode); err != nil {
		return "", fmt.Errorf("store: secure backup directory %q: %w", backupDir, err)
	}

	temporaryFile, err := os.CreateTemp(backupDir, ".dl-tool-pre-migration-*.tmp")
	if err != nil {
		return "", fmt.Errorf("store: create temporary migration backup: %w", err)
	}
	temporaryPath := temporaryFile.Name()

	if err := temporaryFile.Close(); err != nil {
		return "", removeTemporaryBackup(temporaryPath, fmt.Errorf("close temporary backup: %w", err))
	}

	finalName := fmt.Sprintf(
		"%s.pre-migration-%d-to-%d.%s.bak",
		filepath.Base(dbPath),
		fromVersion,
		toVersion,
		startedAt.UTC().Format(backupTimestampFormat),
	)
	finalPath := filepath.Join(backupDir, finalName)

	if err := createMigrationBackup(ctx, db, temporaryPath, finalPath, backupDir); err != nil {
		return "", removeTemporaryBackup(temporaryPath, err)
	}

	logger.InfoContext(
		ctx,
		"created pre-migration database backup",
		"path",
		finalPath,
		"from_version",
		fromVersion,
		"to_version",
		toVersion,
	)

	return finalPath, nil
}

func createMigrationBackup(
	ctx context.Context,
	db *sqlx.DB,
	temporaryPath string,
	finalPath string,
	backupDir string,
) error {
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", temporaryPath); err != nil {
		return fmt.Errorf("store: create migration backup %q: %w", temporaryPath, err)
	}

	if err := checkDatabaseFileIntegrity(ctx, temporaryPath); err != nil {
		return err
	}
	if err := os.Chmod(temporaryPath, databaseFileMode); err != nil {
		return fmt.Errorf("store: secure migration backup %q: %w", temporaryPath, err)
	}
	if err := syncPath(temporaryPath); err != nil {
		return err
	}
	if err := renameNoReplace(temporaryPath, finalPath); err != nil {
		return fmt.Errorf("store: rename migration backup to %q: %w", finalPath, err)
	}
	if err := syncPath(backupDir); err != nil {
		return err
	}

	return nil
}

func checkDatabaseFileIntegrity(ctx context.Context, dbPath string) error {
	db, err := sqlx.Open(sqliteDriver, "file:"+escapedDatabasePath(dbPath)+"?mode=ro")
	if err != nil {
		return fmt.Errorf("store: open migration backup %q: %w", dbPath, err)
	}
	db.SetMaxOpenConns(databaseConnectionLimit)
	db.SetMaxIdleConns(databaseConnectionLimit)

	checkErr := checkIntegrity(ctx, db, dbPath)
	closeErr := db.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("store: close migration backup %q: %w", dbPath, closeErr)
	}

	return errors.Join(checkErr, closeErr)
}

func renameNoReplace(oldPath, newPath string) error {
	return unix.Renameat2(
		unix.AT_FDCWD,
		oldPath,
		unix.AT_FDCWD,
		newPath,
		unix.RENAME_NOREPLACE,
	)
}

func syncPath(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("store: open %q for sync: %w", path, err)
	}

	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		syncErr = fmt.Errorf("store: sync %q: %w", path, syncErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("store: close synced path %q: %w", path, closeErr)
	}

	return errors.Join(syncErr, closeErr)
}

func removeTemporaryBackup(path string, cause error) error {
	removeErr := os.Remove(path)
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		removeErr = fmt.Errorf("remove temporary backup %q: %w", path, removeErr)
	} else {
		removeErr = nil
	}

	return errors.Join(cause, removeErr)
}

func closeAfterError(db *sqlx.DB, cause error) (*sqlx.DB, error) {
	closeErr := db.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("store: close database after failure: %w", closeErr)
	}

	return nil, errors.Join(cause, closeErr)
}

func refuseNetworkFilesystem(directory string, mountInfo io.Reader) error {
	resolvedDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return fmt.Errorf("store: resolve database directory %q: %w", directory, err)
	}

	filesystem, err := filesystemType(resolvedDirectory, mountInfo)
	if err != nil {
		return err
	}
	if !isNetworkFilesystem(filesystem) {
		return nil
	}

	return fmt.Errorf(
		"config_network_fs: database directory %q is on unsupported filesystem %q",
		directory,
		filesystem,
	)
}

func filesystemType(directory string, mountInfo io.Reader) (string, error) {
	absoluteDirectory, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("store: resolve database directory %q: %w", directory, err)
	}
	absoluteDirectory = filepath.Clean(absoluteDirectory)

	data, err := io.ReadAll(mountInfo)
	if err != nil {
		return "", fmt.Errorf("store: read mount information: %w", err)
	}

	bestMount := ""
	bestFilesystem := ""

	// The longest match handles nested mounts and bind mounts.
	for _, line := range strings.Split(string(data), "\n") {
		mountPoint, filesystem, ok := parseMountInfoLine(line)
		if !ok || !pathContains(mountPoint, absoluteDirectory) {
			continue
		}
		if len(mountPoint) < len(bestMount) {
			continue
		}

		bestMount = mountPoint
		bestFilesystem = filesystem
	}

	if bestMount == "" {
		return "", fmt.Errorf("store: no mount information found for database directory %q", directory)
	}

	return bestFilesystem, nil
}

func parseMountInfoLine(line string) (string, string, bool) {
	fields := strings.Fields(line)
	separator := -1
	for index, field := range fields {
		if field == "-" {
			separator = index
			break
		}
	}
	if len(fields) < 5 || separator < 0 || separator+1 >= len(fields) {
		return "", "", false
	}

	mountPoint := filepath.Clean(unescapeMountInfo(fields[4]))

	return mountPoint, fields[separator+1], true
}

func unescapeMountInfo(value string) string {
	replacer := strings.NewReplacer(
		`\040`, " ",
		`\011`, "\t",
		`\012`, "\n",
		`\134`, `\`,
	)

	return replacer.Replace(value)
}

func pathContains(parent, child string) bool {
	if parent == child || parent == string(filepath.Separator) {
		return true
	}

	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

func isNetworkFilesystem(filesystem string) bool {
	switch filesystem {
	case "nfs", "nfs4", "cifs", "smb3":
		return true
	default:
		return strings.HasPrefix(filesystem, "fuse.")
	}
}

// databaseLockSuffix is the stable process-lock path of
// docs/17-operations-and-runbook.md section 1.3: the server and the
// restore CLI both flock DLTOOL_DB_PATH + ".lock" for the process
// lifetime, and the file is never replaced or removed.
const databaseLockSuffix = ".lock"

const (
	// restoreDSNFormat opens the live database for the preserve and
	// checkpoint steps. busy_timeout is absent on purpose: the process
	// lock means nothing else should hold a handle, so a blocked
	// checkpoint refuses fast rather than stalling a CLI run.
	restoreDSNFormat = "file:%s?_pragma=journal_mode(WAL)"
	// restoreReadOnlyDSNFormat opens a backup or a restored database
	// without touching it.
	restoreReadOnlyDSNFormat = "file:%s?mode=ro"
)

const (
	queryCheckpoint   = `PRAGMA wal_checkpoint(TRUNCATE)`
	queryTasksPresent = `SELECT name FROM sqlite_schema WHERE type = 'table' AND name = 'tasks'`
	queryTaskCount    = `SELECT COUNT(*) FROM tasks`
)

var (
	// ErrDatabaseLocked is the stage-S3 boot refusal: another process
	// already holds the stable process lock beside DLTOOL_DB_PATH.
	ErrDatabaseLocked = errors.New("store: database_locked")
	// ErrRestoreServerRunning refuses a restore while the process lock is
	// held — by the server or by a second restore.
	ErrRestoreServerRunning = errors.New("store: restore_server_running")
	// ErrRestoreSourceRejected refuses a source that is not a regular file
	// inside the config directory, or that names the live database, its
	// lock or a WAL sidecar.
	ErrRestoreSourceRejected = errors.New("store: restore_source_rejected")
	// ErrRestoreSchemaTooNew refuses a backup whose applied schema exceeds
	// the embedded migration maximum; older schemas are accepted and
	// migrate forward at the next boot.
	ErrRestoreSchemaTooNew = errors.New("store: restore_schema_too_new")
	// ErrRestoreIntegrity refuses a backup that cannot be opened as a
	// database at all, or whose PRAGMA integrity_check is not "ok".
	ErrRestoreIntegrity = errors.New("store: restore_integrity_failed")
)

// RestoreFrom replaces the live database with the backup at src, following
// the staged procedure of docs/17-operations-and-runbook.md section 3.4.
// The four gates run in order and each is a named refusal:
//
//	restore_server_running — flock(LOCK_EX|LOCK_NB) on the stable process
//	  lock fails; the error names the PID recorded by the lock holder.
//	restore_source_rejected — src does not resolve to a regular file inside
//	  configDir, or names the live database, its lock or its sidecars.
//	restore_schema_too_new — the backup's applied schema exceeds the
//	  highest embedded migration. An older schema is accepted.
//	restore_integrity_failed — the backup cannot be opened as a database,
//	  or its PRAGMA integrity_check is not "ok".
//
// Every gate completes before the live database changes. On success the
// source is copied to a unique "<name>.restore-<ULID>.tmp" beside dbPath
// with O_EXCL, mode 0600 and fsync, then integrity-checked; the live
// database is preserved through VACUUM INTO a unique
// "<name>.replaced-<UTC>.bak" that is integrity-checked, fsynced, renamed
// into place and directory-fsynced; the live database is checkpointed and
// closed and its stale -wal and -shm sidecars removed; the staged copy is
// atomically renamed over dbPath and the directory fsynced. Any failure
// before the final rename removes only the staged copy — a crash yields
// either the complete old file or the complete checked replacement.
// RestoreFrom returns the restored task count.
func RestoreFrom(ctx context.Context, dbPath, configDir, src string) (tasks int, err error) {
	lock, err := acquireDatabaseLock(dbPath+databaseLockSuffix, ErrRestoreServerRunning)
	if err != nil {
		return 0, err
	}
	defer lock.release()

	source, err := resolveRestoreSource(dbPath, configDir, src)
	if err != nil {
		return 0, err
	}

	// Goose's base FS is process-wide, so the embedded-maximum read shares
	// open's serialization.
	migrationMu.Lock()
	embeddedVersion, err := func() (int64, error) {
		if err := prepareMigrations(); err != nil {
			return 0, err
		}

		return highestEmbeddedVersion()
	}()
	migrationMu.Unlock()
	if err != nil {
		return 0, err
	}
	if err := checkRestoreSource(ctx, source, embeddedVersion); err != nil {
		return 0, err
	}

	databaseDir := filepath.Dir(dbPath)
	staged, err := stageRestoreSource(ctx, source, databaseDir, filepath.Base(dbPath))
	if err != nil {
		return 0, err
	}
	// The schema gate ran against the source path; re-run it against the
	// staged bytes so a source swapped between check and copy cannot slip
	// a too-new schema through.
	if err := checkStagedSchema(ctx, staged, embeddedVersion); err != nil {
		if removeErr := os.Remove(staged); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("store: remove staged restore %q: %w", staged, removeErr))
		}

		return 0, err
	}
	// Every failure before the atomic rename removes only the staged copy;
	// the live database is never left without a complete file.
	renamed := false
	defer func() {
		if renamed {
			return
		}
		if removeErr := os.Remove(staged); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("store: remove staged restore %q: %w", staged, removeErr))
		}
	}()

	if info, statErr := os.Lstat(dbPath); statErr == nil && info.Mode().IsRegular() {
		if err := preserveLiveDatabase(ctx, dbPath, databaseDir); err != nil {
			return 0, err
		}
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return 0, fmt.Errorf("store: inspect live database %q: %w", dbPath, statErr)
	}

	// The -wal and -shm sidecars must be gone before the staged copy takes
	// the name whether or not a live file existed: a killed shutdown can
	// leave them behind, and SQLite would apply a stale WAL to the
	// restored file. The checkpoint above truncates but does not remove
	// them, so this loop is the single removal point for every path.
	for _, sidecar := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("store: remove stale sidecar %q: %w", sidecar, err)
		}
	}

	if err := os.Rename(staged, dbPath); err != nil {
		return 0, fmt.Errorf("store: install restored database %q: %w", dbPath, err)
	}
	renamed = true

	if err := syncPath(databaseDir); err != nil {
		return 0, err
	}

	count, err := restoredTaskCount(ctx, dbPath)
	if err != nil {
		return 0, err
	}

	return count, nil
}

// databaseLock is the held process lock; closing the descriptor releases
// the flock.
type databaseLock struct{ file *os.File }

// sqliteConstraintPrimary is the low byte of every SQLITE_CONSTRAINT_*
// extended result code — UNIQUE, NOT NULL, CHECK and FOREIGN KEY all
// share it, so the low byte detects a constraint violation without
// enumerating each extended code.
const sqliteConstraintPrimary = 19

// IsUniqueViolation reports whether err is a SQLite UNIQUE constraint
// failure; it exports the store's existing check for callers outside the
// package, such as the settings importer.
func IsUniqueViolation(err error) bool {
	return isUniqueViolation(err)
}

// IsConstraintViolation reports whether err is a SQLite constraint
// failure, keeping the driver knowledge inside the store so an importer
// can turn a malformed row into a per-row rejection instead of aborting
// the whole transaction.
func IsConstraintViolation(err error) bool {
	var sqliteErr *sqlite.Error

	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqliteConstraintPrimary
}

// ProcessLock is the held stable process lock of
// docs/17-operations-and-runbook.md section 1.3 stage S3. The server
// keeps it for the process lifetime — the descriptor is never closed and
// the file never unlinked, so a second server or a restore run against a
// live instance fails fast instead of racing the database file.
type ProcessLock struct {
	lock *databaseLock
}

// AcquireProcessLock opens <dbPath>.lock mode 0600, takes
// flock(LOCK_EX|LOCK_NB) and records this process's PID for the lifetime
// of the returned handle. A held lock refuses with ErrDatabaseLocked
// naming the recorded PID.
func AcquireProcessLock(dbPath string) (*ProcessLock, error) {
	lock, err := acquireDatabaseLock(dbPath+databaseLockSuffix, ErrDatabaseLocked)
	if err != nil {
		return nil, err
	}

	return &ProcessLock{lock: lock}, nil
}

// Release drops the flock by closing the descriptor. The server path
// never calls it — the lock is held for the process lifetime — but it
// exists for tests and embedders.
func (l *ProcessLock) Release() {
	l.lock.release()
}

// acquireDatabaseLock opens the stable lock file mode 0600 and takes
// flock(LOCK_EX|LOCK_NB). A held lock refuses with the busy error,
// naming the PID the holder recorded — ErrDatabaseLocked on the server
// path, ErrRestoreServerRunning on the restore path.
func acquireDatabaseLock(path string, busy error) (*databaseLock, error) {
	// The lock is taken before Open, so on a first boot its directory
	// does not exist yet — create it rather than failing the acquire
	// with a misleading busy error.
	if err := prepareDatabaseDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}

	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, databaseFileMode)
	if err != nil {
		return nil, fmt.Errorf("store: open process lock %q: %w", path, err)
	}
	if err := os.Chmod(path, databaseFileMode); err != nil {
		return nil, errors.Join(
			fmt.Errorf("store: secure process lock %q: %w", path, err),
			file.Close(),
		)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(
			fmt.Errorf("%w: process %s holds the database lock %q",
				busy, databaseLockHolder(path), path),
			file.Close(),
		)
	}

	// Record the holder like stage S3 does, so a later refused attempt can
	// name it.
	if err := file.Truncate(0); err != nil {
		return nil, errors.Join(
			fmt.Errorf("store: truncate process lock %q: %w", path, err),
			file.Close(),
		)
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		return nil, errors.Join(
			fmt.Errorf("store: record pid in process lock %q: %w", path, err),
			file.Close(),
		)
	}

	return &databaseLock{file: file}, nil
}

// release drops the flock by closing the descriptor; the lock file itself
// stays, per the section 1.3 "never unlink or replace" rule.
func (l *databaseLock) release() {
	if err := l.file.Close(); err != nil {
		slog.Warn("store: process lock release failed", "err", err)
	}
}

// databaseLockHolder reads the PID the lock holder recorded, best-effort:
// the flock failure is reported even when the file cannot be read.
func databaseLockHolder(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "unknown"
	}
	if pid := strings.TrimSpace(string(data)); pid != "" {
		return pid
	}

	return "unknown"
}

// resolveRestoreSource applies the restore_source_rejected gate: src must
// resolve to a regular file inside configDir and must not name the live
// database, its lock or a WAL sidecar. Symlinks are resolved before the
// containment check, so a link inside the directory pointing out of it is
// rejected exactly like a direct path.
func resolveRestoreSource(dbPath, configDir, src string) (string, error) {
	resolvedDir, err := filepath.Abs(configDir)
	if err != nil {
		return "", fmt.Errorf("%w: resolve config directory %q: %v", ErrRestoreSourceRejected, configDir, err)
	}
	resolvedDir, err = filepath.EvalSymlinks(resolvedDir)
	if err != nil {
		return "", fmt.Errorf("%w: config directory %q does not resolve: %v", ErrRestoreSourceRejected, configDir, err)
	}

	absolute, err := filepath.Abs(src)
	if err != nil {
		return "", fmt.Errorf("%w: source %q does not resolve: %v", ErrRestoreSourceRejected, src, err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("%w: source %q does not resolve: %v", ErrRestoreSourceRejected, src, err)
	}
	if !pathContains(resolvedDir, resolved) {
		return "", fmt.Errorf("%w: source %q resolves outside the config directory %q", ErrRestoreSourceRejected, src, resolvedDir)
	}
	for _, forbidden := range resolvedDatabasePaths(dbPath) {
		if resolved == forbidden {
			return "", fmt.Errorf("%w: source %q names the live database, its lock or a sidecar", ErrRestoreSourceRejected, src)
		}
	}
	if info, err := os.Stat(resolved); err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: source %q is not a regular file", ErrRestoreSourceRejected, src)
	}

	return resolved, nil
}

// resolvedDatabasePaths returns the resolved live database path and its
// lock and WAL sidecar paths — the names a restore source may not take.
func resolvedDatabasePaths(dbPath string) []string {
	dir, err := filepath.EvalSymlinks(filepath.Dir(dbPath))
	if err != nil {
		abs, absErr := filepath.Abs(dbPath)
		if absErr != nil {
			return nil
		}
		dir = filepath.Dir(abs)
	}
	base := filepath.Join(dir, filepath.Base(dbPath))

	return []string{base, base + databaseLockSuffix, base + "-wal", base + "-shm"}
}

// checkRestoreSource runs the schema and integrity gates against the
// backup, opened read-only: an unreadable or foreign file is
// restore_integrity_failed, a newer applied schema is
// restore_schema_too_new naming both versions, and a failed
// integrity_check is restore_integrity_failed.
func checkRestoreSource(ctx context.Context, source string, embeddedVersion int64) error {
	db, err := sqlx.Open(sqliteDriver, fmt.Sprintf(restoreReadOnlyDSNFormat, escapedDatabasePath(source)))
	if err != nil {
		return fmt.Errorf("%w: open backup %q: %v", ErrRestoreIntegrity, source, err)
	}
	db.SetMaxOpenConns(databaseConnectionLimit)
	db.SetMaxIdleConns(databaseConnectionLimit)

	state, stateErr := readSchemaState(ctx, db)
	if stateErr != nil {
		return errors.Join(
			fmt.Errorf("%w: backup %q: %v", ErrRestoreIntegrity, source, stateErr),
			db.Close(),
		)
	}
	if state.version > embeddedVersion {
		return errors.Join(
			fmt.Errorf("%w: backup %q schema version %d exceeds embedded maximum %d",
				ErrRestoreSchemaTooNew, source, state.version, embeddedVersion),
			db.Close(),
		)
	}
	if err := checkIntegrity(ctx, db, source); err != nil {
		return errors.Join(
			fmt.Errorf("%w: backup %q: %v", ErrRestoreIntegrity, source, err),
			db.Close(),
		)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("store: close backup %q: %w", source, err)
	}

	return nil
}

// checkStagedSchema re-reads the applied schema version of the staged
// copy — the bytes that will actually be installed — so the
// restore_schema_too_new gate survives a source swap between the gate
// and the copy.
func checkStagedSchema(ctx context.Context, staged string, embeddedVersion int64) error {
	db, err := sqlx.Open(sqliteDriver, fmt.Sprintf(restoreReadOnlyDSNFormat, escapedDatabasePath(staged)))
	if err != nil {
		return fmt.Errorf("%w: open staged restore %q: %v", ErrRestoreIntegrity, staged, err)
	}
	db.SetMaxOpenConns(databaseConnectionLimit)
	db.SetMaxIdleConns(databaseConnectionLimit)

	state, stateErr := readSchemaState(ctx, db)
	closeErr := db.Close()
	if stateErr != nil {
		return errors.Join(
			fmt.Errorf("%w: staged restore %q: %v", ErrRestoreIntegrity, staged, stateErr),
			closeErr,
		)
	}
	if state.version > embeddedVersion {
		return fmt.Errorf("%w: staged restore %q schema version %d exceeds embedded maximum %d",
			ErrRestoreSchemaTooNew, staged, state.version, embeddedVersion)
	}
	if closeErr != nil {
		return fmt.Errorf("store: close staged restore %q: %w", staged, closeErr)
	}

	return nil
}

// stageRestoreSource copies the checked backup to a unique
// "<name>.restore-<ULID>.tmp" beside the live database: O_EXCL so a
// colliding name is an error rather than a silent overwrite, mode 0600,
// fsync, then an integrity check of the copy that landed.
func stageRestoreSource(ctx context.Context, source, databaseDir, baseName string) (string, error) {
	staged := filepath.Join(
		databaseDir,
		fmt.Sprintf("%s.restore-%s.tmp", baseName, ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String()),
	)

	in, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("store: open restore source %q: %w", source, err)
	}
	out, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, databaseFileMode)
	if err != nil {
		return "", errors.Join(
			fmt.Errorf("store: create staged restore %q: %w", staged, err),
			in.Close(),
		)
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	if err := errors.Join(copyErr, syncErr, in.Close(), out.Close()); err != nil {
		if removeErr := os.Remove(staged); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("store: remove staged restore %q: %w", staged, removeErr))
		}

		return "", fmt.Errorf("store: stage restore source %q: %w", source, err)
	}

	if err := checkDatabaseFileIntegrity(ctx, staged); err != nil {
		if removeErr := os.Remove(staged); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("store: remove staged restore %q: %w", staged, removeErr))
		}

		return "", fmt.Errorf("%w: staged copy %q: %v", ErrRestoreIntegrity, staged, err)
	}

	return staged, nil
}

// preserveLiveDatabase runs section 3.4 steps 2 and 3 against the live
// database: VACUUM INTO a unique temporary name that is integrity-checked,
// fsynced and renamed to "<name>.replaced-<UTC>.bak" with the directory
// fsynced; then wal_checkpoint(TRUNCATE), close and removal of the stale
// -wal and -shm sidecars. A failure leaves the complete old database —
// and, once created, its .bak — in place.
func preserveLiveDatabase(ctx context.Context, dbPath, databaseDir string) error {
	db, err := sqlx.Open(sqliteDriver, fmt.Sprintf(restoreDSNFormat, escapedDatabasePath(dbPath)))
	if err != nil {
		return fmt.Errorf("store: open live database %q: %w", dbPath, err)
	}
	db.SetMaxOpenConns(databaseConnectionLimit)
	db.SetMaxIdleConns(databaseConnectionLimit)

	backupPath, err := backupReplacedDatabase(ctx, db, dbPath, databaseDir)
	vacuumFailed := err != nil
	if vacuumFailed {
		// A corrupt live database is a common reason to restore and
		// VACUUM INTO cannot read one — degrade to a byte copy of the
		// file rather than aborting the restore outright.
		var copyErr error
		backupPath, copyErr = copyReplacedDatabase(dbPath, databaseDir)
		if copyErr != nil {
			return errors.Join(err, copyErr, db.Close())
		}
		slog.Warn("store: vacuum preserve failed; preserved live database (and any WAL) by file copy",
			"path", backupPath, "err", err)
	}

	var checkpoint struct {
		Busy         int `db:"busy"`
		Log          int `db:"log"`
		Checkpointed int `db:"checkpointed"`
	}
	checkpointErr := db.GetContext(ctx, &checkpoint, queryCheckpoint)
	switch {
	case checkpointErr == nil && checkpoint.Busy != 0:
		return errors.Join(
			fmt.Errorf("store: checkpoint live database %q: another handle still holds it open", dbPath),
			db.Close(),
		)
	case checkpointErr != nil && vacuumFailed:
		// The database was already shown unreadable; the byte-copy
		// preserve has secured what exists, so a failed checkpoint adds
		// nothing and must not abort the restore.
		slog.Warn("store: checkpoint of unreadable live database failed", "path", dbPath, "err", checkpointErr)
	case checkpointErr != nil:
		return errors.Join(fmt.Errorf("store: checkpoint live database %q: %w", dbPath, checkpointErr), db.Close())
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("store: close live database %q: %w", dbPath, err)
	}

	slog.Info("store: preserved replaced database", "path", backupPath)

	return nil
}

// backupReplacedDatabase runs the VACUUM INTO preserve of section 3.4
// step 2 and returns the final backup path: unique temporary name,
// integrity check, mode 0600, fsync, rename into place, fsync the
// directory. The temporary name is removed on failure; a completed .bak
// always stays.
func backupReplacedDatabase(ctx context.Context, db *sqlx.DB, dbPath, databaseDir string) (string, error) {
	temporaryFile, err := os.CreateTemp(databaseDir, "."+filepath.Base(dbPath)+".replaced-*.tmp")
	if err != nil {
		return "", fmt.Errorf("store: create temporary replaced-database backup: %w", err)
	}
	temporaryPath := temporaryFile.Name()
	if err := temporaryFile.Close(); err != nil {
		return "", removeTemporaryBackup(temporaryPath, fmt.Errorf("close temporary backup: %w", err))
	}
	// VACUUM INTO creates its output; CreateTemp only reserved a unique
	// name, so free it — some SQLite builds refuse to write an existing
	// file, even an empty one.
	if err := os.Remove(temporaryPath); err != nil {
		return "", fmt.Errorf("store: clear temporary backup name %q: %w", temporaryPath, err)
	}

	finalPath := filepath.Join(
		databaseDir,
		fmt.Sprintf("%s.replaced-%s.bak", filepath.Base(dbPath), time.Now().UTC().Format(backupTimestampFormat)),
	)

	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", temporaryPath); err != nil {
		return "", removeTemporaryBackup(temporaryPath, fmt.Errorf("store: preserve live database %q: %w", dbPath, err))
	}
	if err := checkDatabaseFileIntegrity(ctx, temporaryPath); err != nil {
		return "", removeTemporaryBackup(temporaryPath, err)
	}
	if err := os.Chmod(temporaryPath, databaseFileMode); err != nil {
		return "", removeTemporaryBackup(temporaryPath, fmt.Errorf("store: secure replaced-database backup: %w", err))
	}
	if err := syncPath(temporaryPath); err != nil {
		return "", removeTemporaryBackup(temporaryPath, err)
	}
	if err := renameNoReplace(temporaryPath, finalPath); err != nil {
		return "", removeTemporaryBackup(temporaryPath, fmt.Errorf("store: rename replaced-database backup to %q: %w", finalPath, err))
	}
	if err := syncPath(databaseDir); err != nil {
		return "", err
	}

	return finalPath, nil
}

// copyReplacedDatabase is the preserve fallback for a live database
// VACUUM INTO cannot read — a corrupt file is the common reason to
// restore at all. It byte-copies dbPath to the same
// "<name>.replaced-<UTC>.bak" target under the same durability rules:
// unique temporary name, mode 0600, fsync, atomic rename, directory
// fsync. Whatever bytes exist are kept; nothing here can make a corrupt
// file readable.
//
// The copy runs before the checkpoint, so the newest committed frames
// may exist only in dbPath's -wal — and the sidecar-removal loop then
// deletes that WAL. The fallback therefore preserves a live -wal beside
// the backup as "<name>.replaced-<UTC>.bak-wal", keeping those commits
// recoverable; -shm is skipped because SQLite rebuilds it.
func copyReplacedDatabase(dbPath, databaseDir string) (string, error) {
	temporaryFile, err := os.CreateTemp(databaseDir, "."+filepath.Base(dbPath)+".replaced-*.tmp")
	if err != nil {
		return "", fmt.Errorf("store: create temporary replaced-database backup: %w", err)
	}
	temporaryPath := temporaryFile.Name()

	in, err := os.Open(dbPath)
	if err != nil {
		return "", errors.Join(
			fmt.Errorf("store: open live database %q for copy preserve: %w", dbPath, err),
			temporaryFile.Close(),
			removeTemporaryBackup(temporaryPath, nil),
		)
	}
	_, copyErr := io.Copy(temporaryFile, in)
	syncErr := temporaryFile.Sync()
	if err := errors.Join(copyErr, syncErr, in.Close(), temporaryFile.Close()); err != nil {
		return "", removeTemporaryBackup(temporaryPath, fmt.Errorf("store: copy preserve live database %q: %w", dbPath, err))
	}
	if err := os.Chmod(temporaryPath, databaseFileMode); err != nil {
		return "", removeTemporaryBackup(temporaryPath, fmt.Errorf("store: secure replaced-database backup: %w", err))
	}

	finalPath := filepath.Join(
		databaseDir,
		fmt.Sprintf("%s.replaced-%s.bak", filepath.Base(dbPath), time.Now().UTC().Format(backupTimestampFormat)),
	)
	if err := renameNoReplace(temporaryPath, finalPath); err != nil {
		return "", removeTemporaryBackup(temporaryPath, fmt.Errorf("store: rename replaced-database backup to %q: %w", finalPath, err))
	}
	if err := preserveWALBeside(dbPath, databaseDir, finalPath); err != nil {
		return "", err
	}
	if err := syncPath(databaseDir); err != nil {
		return "", err
	}

	return finalPath, nil
}

// preserveWALBeside byte-copies source's -wal sidecar to
// "<finalPath>-wal" when one exists, under the same temporary-name,
// fsync and atomic-rename rules as the main copy. A missing WAL is a
// no-op; any other failure aborts the restore rather than silently
// dropping committed frames.
func preserveWALBeside(source, databaseDir, finalPath string) error {
	wal, err := os.Open(source + "-wal")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: open live WAL %q for copy preserve: %w", source+"-wal", err)
	}

	temporaryFile, err := os.CreateTemp(databaseDir, "."+filepath.Base(source)+".replaced-wal-*.tmp")
	if err != nil {
		return errors.Join(fmt.Errorf("store: create temporary WAL backup: %w", err), wal.Close())
	}
	temporaryPath := temporaryFile.Name()

	_, copyErr := io.Copy(temporaryFile, wal)
	syncErr := temporaryFile.Sync()
	walCloseErr := wal.Close()
	if err := errors.Join(copyErr, syncErr, temporaryFile.Close()); err != nil {
		return removeTemporaryBackup(temporaryPath, fmt.Errorf("store: copy preserve WAL %q: %w", source+"-wal", err))
	}
	// The copied frames are already durable; a close hiccup on the
	// read-only live WAL must not abort the restore.
	if walCloseErr != nil {
		slog.Warn("store: closing live WAL after copy preserve", "path", source+"-wal", "err", walCloseErr)
	}
	if err := os.Chmod(temporaryPath, databaseFileMode); err != nil {
		return removeTemporaryBackup(temporaryPath, fmt.Errorf("store: secure WAL backup: %w", err))
	}
	if err := renameNoReplace(temporaryPath, finalPath+"-wal"); err != nil {
		return removeTemporaryBackup(temporaryPath, fmt.Errorf("store: rename WAL backup to %q: %w", finalPath+"-wal", err))
	}

	return nil
}

// restoredTaskCount reads the task count of the freshly installed
// database, opened read-only; a backup whose schema predates the tasks
// table reports 0.
func restoredTaskCount(ctx context.Context, dbPath string) (int, error) {
	db, err := sqlx.Open(sqliteDriver, fmt.Sprintf(restoreReadOnlyDSNFormat, escapedDatabasePath(dbPath)))
	if err != nil {
		return 0, fmt.Errorf("store: open restored database %q: %w", dbPath, err)
	}
	db.SetMaxOpenConns(databaseConnectionLimit)
	db.SetMaxIdleConns(databaseConnectionLimit)

	var tableName string
	err = db.GetContext(ctx, &tableName, queryTasksPresent)
	tasks := 0
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A pre-tasks schema; the next boot migrates it forward.
		err = nil
	case err == nil:
		err = db.GetContext(ctx, &tasks, queryTaskCount)
	}
	closeErr := db.Close()
	if err != nil {
		return 0, errors.Join(fmt.Errorf("store: count restored tasks in %q: %w", dbPath, err), closeErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("store: close restored database %q: %w", dbPath, closeErr)
	}

	return tasks, nil
}
