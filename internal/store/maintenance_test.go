package store

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/require"
)

// backupFileName is the dl-tool.db.<UTC>.bak shape of docs/04-data-model.md
// section 6 as backupTimestampFormat renders it.
func backupFileName(day string) string {
	return "dl-tool.db.202609" + day + "T120000.000000000Z.bak"
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	return names
}

func seedMaintenanceTask(t *testing.T, db *sqlx.DB, id string) {
	t.Helper()

	_, err := db.ExecContext(t.Context(), `INSERT INTO tasks
(id, engine, source_kind, name, state, destination, added_at, created_at, updated_at)
VALUES (?, 'aria2', 'http', 'fixture', 'queued', '/data', 0, 0, 0)`, id)
	require.NoError(t, err)
}

// TestBackupIntoIsConsistent is the FR-142 snapshot check: the produced file
// opens on its own connection and answers PRAGMA integrity_check with "ok",
// carries the source's data, and lands at mode 0600.
func TestBackupIntoIsConsistent(t *testing.T) {
	db, _, backupDir := openTestStore(t)
	ctx := t.Context()

	seedMaintenanceTask(t, db, "tsk_backup")

	result, err := NewMaintenanceStore(db).BackupInto(ctx, backupDir)
	require.NoError(t, err)

	require.Equal(t, backupDir, filepath.Dir(result.Path))
	require.Regexp(t, `^dl-tool\.db\.\d{8}T\d{6}\.\d{9}Z\.bak$`, filepath.Base(result.Path))
	require.Equal(t, fs.FileMode(0o600), fileMode(t, result.Path),
		"the produced backup must be mode 0600")

	info, err := os.Stat(result.Path)
	require.NoError(t, err)
	require.Equal(t, info.Size(), result.SizeBytes)
	require.False(t, result.CreatedAt.IsZero())

	backup, err := sqlx.Open(sqliteDriver, "file:"+escapedDatabasePath(result.Path)+"?mode=ro")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backup.Close()) })

	var checks []string
	require.NoError(t, backup.SelectContext(ctx, &checks, queryIntegrityCheck))
	require.Equal(t, []string{"ok"}, checks)

	var tasks int
	require.NoError(t, backup.GetContext(ctx, &tasks,
		`SELECT COUNT(*) FROM tasks WHERE id = ?`, "tsk_backup"))
	require.Equal(t, 1, tasks, "the snapshot must carry the source's rows")
}

// TestBackupNamesNeverCollide: two backups started in the same second still
// produce two different file names — the nanosecond fraction of
// backupTimestampFormat is what separates them.
func TestBackupNamesNeverCollide(t *testing.T) {
	db, _, backupDir := openTestStore(t)
	m := NewMaintenanceStore(db)

	first, err := m.BackupInto(t.Context(), backupDir)
	require.NoError(t, err)

	// A true same-timestamp collision is a renameNoReplace refusal, never
	// a silent overwrite — retry past it so a coarse-grained test clock
	// cannot flake the assertion.
	var second BackupResult
	for attempt := 0; attempt < 10; attempt++ {
		second, err = m.BackupInto(t.Context(), backupDir)
		if err == nil {
			break
		}
		t.Logf("backup attempt %d failed (expected timestamp collision): %v", attempt+1, err)
	}
	require.NoError(t, err)
	require.NotEqual(t, first.Path, second.Path)
}

// TestBackupIntoRejectsConcurrentRun pins the TryLock contract: a call made
// while another backup runs returns ErrBackupRunning at once instead of
// blocking on the mutex.
func TestBackupIntoRejectsConcurrentRun(t *testing.T) {
	db, _, backupDir := openTestStore(t)
	m := NewMaintenanceStore(db)

	m.backupMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := m.BackupInto(t.Context(), backupDir)
		done <- err
	}()

	select {
	case err := <-done:
		m.backupMu.Unlock()
		require.ErrorIs(t, err, ErrBackupRunning)
	case <-time.After(5 * time.Second):
		m.backupMu.Unlock()
		t.Fatal("BackupInto blocked behind a held lock instead of returning ErrBackupRunning")
	}

	// The refused call returns before creating anything — no directory,
	// no temporary, no file.
	assertNoBackupArtifacts(t, backupDir)
}

// TestBackupIntoCancelledLeavesNoArtifacts: a context cancelled before the
// statement runs still creates and closes the staged temporary first, so the
// cleanup path is what removes it — nothing matching a good backup, and no
// stray *.tmp, may remain.
func TestBackupIntoCancelledLeavesNoArtifacts(t *testing.T) {
	db, _, _ := openTestStore(t)
	dir := t.TempDir()
	m := NewMaintenanceStore(db)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := m.BackupInto(ctx, dir)
	require.Error(t, err)

	matches, globErr := filepath.Glob(filepath.Join(dir, "dl-tool.db.*"))
	require.NoError(t, globErr)
	require.Empty(t, matches, "a failed backup must leave no file in the backup directory")
}

// TestPruneBackupsKeepsSeven pins the retention window of doc 04 section 6:
// the newest seven dl-tool.db.<UTC>.bak files survive, the excluded
// pre-migration and replaced families are never counted or pruned, and a
// staged temporary goes only once it is at least an hour old.
func TestPruneBackupsKeepsSeven(t *testing.T) {
	db, _, _ := openTestStore(t)
	dir := t.TempDir()
	m := NewMaintenanceStore(db)

	// Eight nightly files; the oldest must go.
	for day := 1; day <= 8; day++ {
		name := backupFileName(fmt.Sprintf("%02d", day))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600))
	}
	// The two excluded families share the directory.
	for _, name := range []string{
		"dl-tool.db.pre-migration-1-to-2.20260901T120000.000000000Z.bak",
		"dl-tool.db.replaced-20260901T120000.000000000Z.bak",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600))
	}
	// Staged temporaries: the stale one is crash debris and is collected;
	// the young one may belong to a run in flight and stays.
	freshTmp := filepath.Join(dir, "dl-tool.db.fresh.tmp")
	staleTmp := filepath.Join(dir, "dl-tool.db.stale.tmp")
	require.NoError(t, os.WriteFile(freshTmp, []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(staleTmp, []byte("x"), 0o600))
	stale := time.Now().Add(-2 * backupTempStaleAge)
	require.NoError(t, os.Chtimes(staleTmp, stale, stale))

	deleted, err := m.PruneBackups(t.Context(), dir, BackupKeepCount)
	require.NoError(t, err)
	require.Equal(t, 1, deleted,
		"only the eighth nightly file is pruned; the stale *.tmp sweep is not counted")

	want := []string{
		"dl-tool.db.pre-migration-1-to-2.20260901T120000.000000000Z.bak",
		"dl-tool.db.replaced-20260901T120000.000000000Z.bak",
		"dl-tool.db.fresh.tmp",
	}
	for day := 2; day <= 8; day++ {
		want = append(want, backupFileName(fmt.Sprintf("%02d", day)))
	}
	sort.Strings(want)
	require.Equal(t, want, listDir(t, dir))
}

// TestPruneTaskEventsRespectsWindow: the 90-day window of doc 04 section 7 —
// a row older than 90 days goes, one exactly 89 days old stays, and one
// exactly at the cutoff stays too: the comparison is strict ("older than").
func TestPruneTaskEventsRespectsWindow(t *testing.T) {
	db, _, _ := openTestStore(t)
	ctx := t.Context()
	m := NewMaintenanceStore(db)

	seedMaintenanceTask(t, db, "tsk_events")

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	insertEvent := func(id string, age time.Duration) {
		t.Helper()
		_, err := db.ExecContext(ctx, `INSERT INTO task_events
(id, task_id, at, level, code, message, created_at, updated_at)
VALUES (?, 'tsk_events', ?, 'info', 'test.event', 'm', 0, 0)`,
			id, now.Add(-age).UnixMilli())
		require.NoError(t, err)
	}
	insertEvent("evt_old", 91*24*time.Hour)
	insertEvent("evt_cutoff", 90*24*time.Hour)
	insertEvent("evt_edge", 89*24*time.Hour)

	deleted, err := m.PruneTaskEvents(ctx, now)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)

	var ids []string
	require.NoError(t, db.SelectContext(ctx, &ids, `SELECT id FROM task_events ORDER BY id`))
	require.Equal(t, []string{"evt_cutoff", "evt_edge"}, ids,
		"a row exactly at the 90-day cutoff survives a strict older-than")
}

// TestRetentionPrunesRespectWindows covers the two remaining windows of doc
// 04 section 7: done jobs older than 7 days go while failed rows and fresh
// done rows stay, and search jobs older than 24 hours take their
// search_results with them through the cascade.
func TestRetentionPrunesRespectWindows(t *testing.T) {
	db, _, _ := openTestStore(t)
	ctx := t.Context()
	m := NewMaintenanceStore(db)

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	insertJob := func(id, state string, updatedAgo time.Duration) {
		t.Helper()
		_, err := db.ExecContext(ctx, `INSERT INTO jobs
(id, kind, payload_json, state, run_after, created_at, updated_at)
VALUES (?, 'test', '{}', ?, 0, 0, ?)`,
			id, state, now.Add(-updatedAgo).UnixMilli())
		require.NoError(t, err)
	}
	insertJob("job_done_old", "done", 8*24*time.Hour)
	insertJob("job_done_edge", "done", 7*24*time.Hour)
	insertJob("job_done_new", "done", 6*24*time.Hour)
	insertJob("job_failed_old", "failed", 30*24*time.Hour)
	insertJob("job_running_old", "running", 30*24*time.Hour)

	deleted, err := m.PruneDoneJobs(ctx, now)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)

	var jobIDs []string
	require.NoError(t, db.SelectContext(ctx, &jobIDs, `SELECT id FROM jobs ORDER BY id`))
	require.Equal(t, []string{"job_done_edge", "job_done_new", "job_failed_old", "job_running_old"}, jobIDs,
		"failed, running and exactly-at-cutoff rows are never pruned")

	// A search job and its result; deleting the job must cascade.
	_, err = db.ExecContext(ctx, `INSERT INTO indexers
(id, name, kind, created_at, updated_at)
VALUES ('idx_main', 'main', 'torznab', 0, 0)`)
	require.NoError(t, err)
	insertSearch := func(id string, createdAgo time.Duration) {
		t.Helper()
		_, err := db.ExecContext(ctx, `INSERT INTO search_jobs
(id, query, indexer_ids_json, finished, started_at, created_at, updated_at)
VALUES (?, 'q', '[]', 1, 0, ?, 0)`,
			id, now.Add(-createdAgo).UnixMilli())
		require.NoError(t, err)
	}
	insertSearch("sch_old", 25*time.Hour)
	insertSearch("sch_edge", 24*time.Hour)
	insertSearch("sch_new", time.Hour)
	_, err = db.ExecContext(ctx, `INSERT INTO search_results
(id, search_job_id, indexer_id, title, created_at, updated_at)
VALUES ('res_old', 'sch_old', 'idx_main', 'r', 0, 0),
       ('res_edge', 'sch_edge', 'idx_main', 'r', 0, 0),
       ('res_new', 'sch_new', 'idx_main', 'r', 0, 0)`)
	require.NoError(t, err)

	deleted, err = m.PruneSearchJobs(ctx, now)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)

	var results []string
	require.NoError(t, db.SelectContext(ctx, &results, `SELECT id FROM search_results ORDER BY id`))
	require.Equal(t, []string{"res_edge", "res_new"}, results,
		"the pruned job's results must follow the ON DELETE CASCADE; the exactly-24h job survives")
}

// TestPruneBackupsRejectsNegativeKeep is the guard rail: a negative keep
// would otherwise delete every backup.
func TestPruneBackupsRejectsNegativeKeep(t *testing.T) {
	db, _, _ := openTestStore(t)

	_, err := NewMaintenanceStore(db).PruneBackups(t.Context(), t.TempDir(), -1)
	require.Error(t, err)
}
