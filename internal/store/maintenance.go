// Database backup and retention: the on-demand and nightly VACUUM INTO
// snapshot of docs/04-data-model.md section 6 and the three scheduled
// deletes of section 7.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// BackupsDirName is the ConfigDir subdirectory every backup lives in — the one
// derivation site, so the store.Open join, the handler and the scheduler attach
// cannot spell the directory two ways.
const BackupsDirName = "backups"

// BackupKeepCount is the retention window of docs/04-data-model.md section 6:
// POST /system/backup and the nightly entry each keep the newest seven
// snapshots. One constant serves both call sites so they cannot drift.
const BackupKeepCount = 7

// backupGlob matches only the dl-tool.db.<UTC>.bak family: the
// timestamp-shaped middle segment excludes the dl-tool.db.pre-migration-*.bak
// and dl-tool.db.replaced-*.bak names the retention job must never count or
// prune (docs/04-data-model.md section 6). The `T[0-9]*Z` tail accepts both
// the fractional form backupTimestampFormat writes and the second-precision
// form of the doc's example.
const backupGlob = "dl-tool.db.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9]*Z.bak"

// backupTempGlob matches the CreateTemp names BackupInto stages under; a
// leftover is a crashed or interrupted run's partial output.
const backupTempGlob = "dl-tool.db.*.tmp"

// backupTempStaleAge is how old a staged temporary must be before
// PruneBackups removes it: a younger file may still belong to a run in
// flight, so it is never touched.
const backupTempStaleAge = time.Hour

// Retention windows of docs/04-data-model.md section 7. now is injected into
// each prune so the tests are deterministic.
const (
	taskEventsRetention = 90 * 24 * time.Hour
	doneJobsRetention   = 7 * 24 * time.Hour
	searchJobsRetention = 24 * time.Hour
)

const (
	queryPruneTaskEvents = `DELETE FROM task_events WHERE at < ?`
	// A done row's updated_at is its completion stamp (CompleteJob writes
	// both), so the window measures time spent finished: a job created
	// long ago but completed just now is never pruned under it, where
	// created_at would delete it at once.
	queryPruneDoneJobs = `DELETE FROM jobs WHERE state = 'done' AND updated_at < ?`
	// search_results rows follow their job through ON DELETE CASCADE —
	// there is deliberately no separate delete for them.
	queryPruneSearchJobs = `DELETE FROM search_jobs WHERE created_at < ?`
)

// ErrBackupRunning maps to 409 /problems/conflict.
var ErrBackupRunning = errors.New("store: a backup is already running")

// BackupResult describes one completed snapshot.
type BackupResult struct {
	Path      string    `db:"-"`
	SizeBytes int64     `db:"-"`
	CreatedAt time.Time `db:"-"`
}

// MaintenanceStore is the domain store for backup and retention — the
// TaskStore/SettingsStore shape, not a package-wide aggregate. The composition
// root shares one instance between NewSystemHandlers and the scheduler's
// WithMaintenance attach, so the ErrBackupRunning lock spans the cron entry
// and POST /system/backup.
type MaintenanceStore struct {
	db *sqlx.DB
	// backupMu serializes BackupInto; TryLock turns a concurrent call into
	// ErrBackupRunning instead of a queue.
	backupMu sync.Mutex
}

// NewMaintenanceStore builds the backup and retention store over db.
func NewMaintenanceStore(db *sqlx.DB) *MaintenanceStore {
	return &MaintenanceStore{db: db}
}

// BackupInto writes a consistent snapshot into dir using SQLite's VACUUM INTO.
//
// It generates the name dl-tool.db.<UTC>.bak — where <UTC> is the existing
// backupTimestampFormat ("20060102T150405.000000000Z") so two runs in one
// second never collide — creates the unique temporary target inside dir with
// O_CREATE|O_EXCL and mode 0600 and closes it before running the statement
// (VACUUM INTO requires a missing or empty target), then integrity-checks the
// output, enforces 0600, fsyncs it, renames it into place and fsyncs the
// directory. An interrupted statement never produces a file that looks like a
// good backup. It returns ErrBackupRunning when another backup holds the
// in-process lock.
func (s *MaintenanceStore) BackupInto(ctx context.Context, dir string) (BackupResult, error) {
	if !s.backupMu.TryLock() {
		return BackupResult{}, ErrBackupRunning
	}
	defer s.backupMu.Unlock()

	if err := os.MkdirAll(dir, databaseDirectoryMode); err != nil {
		return BackupResult{}, fmt.Errorf("store: create backup directory %q: %w", dir, err)
	}
	if err := os.Chmod(dir, databaseDirectoryMode); err != nil {
		return BackupResult{}, fmt.Errorf("store: secure backup directory %q: %w", dir, err)
	}

	temporaryFile, err := os.CreateTemp(dir, "dl-tool.db.*.tmp")
	if err != nil {
		return BackupResult{}, fmt.Errorf("store: create temporary backup: %w", err)
	}
	temporaryPath := temporaryFile.Name()
	if err := temporaryFile.Close(); err != nil {
		return BackupResult{}, removeTemporaryBackup(temporaryPath, fmt.Errorf("close temporary backup: %w", err))
	}

	createdAt := time.Now().UTC()
	finalPath := filepath.Join(dir, "dl-tool.db."+createdAt.Format(backupTimestampFormat)+".bak")

	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", temporaryPath); err != nil {
		return BackupResult{}, removeTemporaryBackup(
			temporaryPath, fmt.Errorf("store: create backup %q: %w", temporaryPath, err))
	}
	if err := checkDatabaseFileIntegrity(ctx, temporaryPath); err != nil {
		return BackupResult{}, removeTemporaryBackup(temporaryPath, err)
	}
	if err := os.Chmod(temporaryPath, databaseFileMode); err != nil {
		return BackupResult{}, removeTemporaryBackup(
			temporaryPath, fmt.Errorf("store: secure backup %q: %w", temporaryPath, err))
	}
	if err := syncPath(temporaryPath); err != nil {
		return BackupResult{}, removeTemporaryBackup(temporaryPath, err)
	}
	if err := renameNoReplace(temporaryPath, finalPath); err != nil {
		return BackupResult{}, removeTemporaryBackup(
			temporaryPath, fmt.Errorf("store: rename backup to %q: %w", finalPath, err))
	}
	// Past the rename the snapshot is complete and verified; a directory
	// sync failure is reported but the file stays — removing a good backup
	// would be worse than reporting the durability miss.
	if err := syncPath(dir); err != nil {
		return BackupResult{}, err
	}

	info, err := os.Stat(finalPath)
	if err != nil {
		return BackupResult{}, fmt.Errorf("store: stat backup %q: %w", finalPath, err)
	}

	return BackupResult{Path: finalPath, SizeBytes: info.Size(), CreatedAt: createdAt}, nil
}

// PruneBackups deletes all but the newest keep files matching exactly
// dl-tool.db.<UTC>.bak in dir — the timestamp-shaped glob whose middle segment
// excludes the dl-tool.db.pre-migration-*.bak and dl-tool.db.replaced-*.bak
// families, which this job must never count or prune (docs/04-data-model.md
// section 6). deleted counts only those backup files. It also removes staged
// temporaries at least backupTempStaleAge old — crash leftovers — while never
// touching a *.tmp file younger than one hour.
func (s *MaintenanceStore) PruneBackups(ctx context.Context, dir string, keep int) (deleted int, err error) {
	if keep < 0 {
		return 0, fmt.Errorf("store: backup keep count %d is negative", keep)
	}

	names, err := filepath.Glob(filepath.Join(dir, backupGlob))
	if err != nil {
		return 0, fmt.Errorf("store: list backups in %q: %w", dir, err)
	}
	// Glob sorts ascending; the timestamp format sorts lexicographically,
	// so descending order puts the newest files first.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	for index, path := range names {
		if index < keep {
			continue
		}
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return deleted, fmt.Errorf("store: remove old backup %q: %w", path, err)
		}
		deleted++
	}

	tempNames, err := filepath.Glob(filepath.Join(dir, backupTempGlob))
	if err != nil {
		return deleted, fmt.Errorf("store: list temporary backups in %q: %w", dir, err)
	}
	for _, path := range tempNames {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return deleted, fmt.Errorf("store: inspect temporary backup %q: %w", path, err)
		}
		if time.Since(info.ModTime()) < backupTempStaleAge {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return deleted, fmt.Errorf("store: remove stale temporary backup %q: %w", path, err)
		}
	}

	return deleted, nil
}

// PruneTaskEvents deletes the task_events rows whose at predates now minus
// the 90-day window of docs/04-data-model.md section 7.
func (s *MaintenanceStore) PruneTaskEvents(ctx context.Context, now time.Time) (int64, error) {
	return s.prune(ctx, queryPruneTaskEvents, "task events", now.Add(-taskEventsRetention).UnixMilli())
}

// PruneDoneJobs deletes the jobs rows in state 'done' whose updated_at
// predates now minus the 7-day window; 'failed' rows are the dead-letter
// queue and are kept (docs/04-data-model.md section 7).
func (s *MaintenanceStore) PruneDoneJobs(ctx context.Context, now time.Time) (int64, error) {
	return s.prune(ctx, queryPruneDoneJobs, "done jobs", now.Add(-doneJobsRetention).UnixMilli())
}

// PruneSearchJobs deletes the search_jobs rows whose created_at predates now
// minus the 24-hour window; their search_results follow through the ON
// DELETE CASCADE (docs/04-data-model.md section 7).
func (s *MaintenanceStore) PruneSearchJobs(ctx context.Context, now time.Time) (int64, error) {
	return s.prune(ctx, queryPruneSearchJobs, "search jobs", now.Add(-searchJobsRetention).UnixMilli())
}

// prune runs one retention delete and reports the rows it removed.
func (s *MaintenanceStore) prune(ctx context.Context, query, table string, cutoff int64) (int64, error) {
	result, err := s.db.ExecContext(ctx, query, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune %s: %w", table, err)
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune %s: read rows affected: %w", table, err)
	}

	return deleted, nil
}
