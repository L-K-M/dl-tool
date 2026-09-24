package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/store"
)

// postBackup fires POST /system/backup with the env's bearer credential.
func postBackup(env *tasksTestEnv) *httptest.ResponseRecorder {
	return env.api.Post("/system/backup", "Authorization: Bearer "+env.bearer)
}

// TestCreateBackupServesConsistentSnapshot is the doc 05 section 13 contract:
// 201, a path under ConfigDir/backups naming a dl-tool.db.<UTC>.bak file,
// size_bytes and an RFC 3339 created_at — and the file itself opens and
// answers PRAGMA integrity_check with "ok".
func TestCreateBackupServesConsistentSnapshot(t *testing.T) {
	env := newTasksTestEnv(t)

	response := postBackup(env)
	require.Equal(t, http.StatusCreated, response.Code, response.Body.String())

	var body struct {
		Path      string `json:"path"`
		SizeBytes int64  `json:"size_bytes"`
		CreatedAt string `json:"created_at"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))

	wantDir := filepath.Join(filepath.Dir(env.dataRoot), "config", store.BackupsDirName)
	require.Equal(t, wantDir, filepath.Dir(body.Path))
	require.Regexp(t, `^dl-tool\.db\.\d{8}T\d{6}\.\d{9}Z\.bak$`, filepath.Base(body.Path))
	require.FileExists(t, body.Path)

	info, err := os.Stat(body.Path)
	require.NoError(t, err)
	require.Equal(t, info.Size(), body.SizeBytes)
	require.Equal(t, fs.FileMode(0o600), info.Mode().Perm())
	_, err = time.Parse(time.RFC3339, body.CreatedAt)
	require.NoError(t, err)

	backup, err := sqlx.Open("sqlite", "file:"+body.Path+"?mode=ro")
	require.NoError(t, err)
	var checks []string
	require.NoError(t, backup.SelectContext(t.Context(), &checks, "PRAGMA integrity_check"))
	require.Equal(t, []string{"ok"}, checks)
	require.NoError(t, backup.Close())

	// Unauthenticated requests are refused like every /api/v1 route.
	response = env.api.Post("/system/backup")
	assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)
}

// TestConcurrentBackupIs409 pins the ErrBackupRunning mapping: a second
// request while a backup holds the store lock is 409 /problems/conflict and
// writes no file.
func TestConcurrentBackupIs409(t *testing.T) {
	env := newTasksTestEnv(t)

	// Pad the database so the first request's VACUUM INTO is still running
	// when the second arrives — on an empty database the lock window is
	// microseconds wide and no request could land inside it.
	pad := strings.Repeat("x", 256*1024)
	for i := 0; i < 200; i++ {
		_, err := env.db.ExecContext(t.Context(), `INSERT INTO jobs
(id, kind, payload_json, run_after, created_at, updated_at)
VALUES (?, 'pad', ?, 0, 0, 0)`, fmt.Sprintf("job_pad%04d", i), pad)
		require.NoError(t, err)
	}

	var conflict *httptest.ResponseRecorder
	for attempt := 0; attempt < 30 && conflict == nil; attempt++ {
		first := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			first <- postBackup(env)
		}()
		second := postBackup(env)
		other := <-first

		switch {
		case second.Code == http.StatusConflict:
			require.Equal(t, http.StatusCreated, other.Code, other.Body.String())
			conflict = second
		case other.Code == http.StatusConflict:
			require.Equal(t, http.StatusCreated, second.Code, second.Body.String())
			conflict = other
		default:
			require.Equal(t, http.StatusCreated, second.Code, second.Body.String())
			require.Equal(t, http.StatusCreated, other.Code, other.Body.String())
		}
	}

	require.NotNil(t, conflict, "thirty overlapped pairs never produced a conflict")
	problem := assertProblem(t, conflict, http.StatusConflict, SlugConflict)
	require.Contains(t, problem.Detail, "backup")
}

// TestFailedBackupLeavesNoFile: a request context cancelled under the
// statement fails the backup — and leaves nothing matching
// dl-tool.db.*.bak (and no staged *.tmp) behind.
func TestFailedBackupLeavesNoFile(t *testing.T) {
	env := newTasksTestEnv(t)
	dir := t.TempDir()
	handlers := NewSystemHandlers(store.NewMaintenanceStore(env.db), dir)

	// Pad the database so the cancel below lands mid-statement — on the
	// effectively empty env database VACUUM INTO can finish before the
	// goroutine even starts, and a completed backup is not a failed one.
	pad := strings.Repeat("x", 256*1024)
	for i := 0; i < 200; i++ {
		_, err := env.db.ExecContext(t.Context(), `INSERT INTO jobs
(id, kind, payload_json, run_after, created_at, updated_at)
VALUES (?, 'pad', ?, 0, 0, 0)`, fmt.Sprintf("job_pad%04d", i), pad)
		require.NoError(t, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := handlers.CreateBackup(ctx, &struct{}{})
		done <- err
	}()
	// Whether the cancel lands before or mid-statement, the contract is
	// the same: the call fails and nothing backup-shaped remains.
	cancel()

	err := <-done
	require.Error(t, err)
	var model *huma.ErrorModel
	require.ErrorAs(t, err, &model)
	require.Equal(t, http.StatusInternalServerError, model.Status)
	require.Equal(t, SlugInternal, model.Type)

	matches, globErr := filepath.Glob(filepath.Join(dir, "dl-tool.db.*"))
	require.NoError(t, globErr)
	require.Empty(t, matches, "a failed backup must leave no file in the backup directory")
}
