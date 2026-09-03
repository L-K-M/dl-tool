package store

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/pressly/goose/v3"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
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
	directory := filepath.Dir(dbPath)
	if err := os.MkdirAll(directory, databaseDirectoryMode); err != nil {
		return fmt.Errorf("store: create database directory %q: %w", directory, err)
	}

	if err := os.Chmod(directory, databaseDirectoryMode); err != nil {
		return fmt.Errorf("store: secure database directory %q: %w", directory, err)
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

func openDatabase(ctx context.Context, dbPath string) (*sqlx.DB, error) {
	// Pre-create securely so SQLite derives secure WAL and SHM modes.
	if err := createDatabaseFileIfMissing(dbPath); err != nil {
		return nil, err
	}

	dsn := fmt.Sprintf(databaseDSNFormat, dbPath)
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
	db, err := sqlx.Open(sqliteDriver, "file:"+dbPath+"?mode=ro")
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
