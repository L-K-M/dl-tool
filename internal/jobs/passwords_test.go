package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/store"
)

// setTaskPassword writes tasks.extract_password directly — the create path
// has no store.Task field for the column yet, so a fixture writes it the
// way the upload tasks will.
func setTaskPassword(t *testing.T, db *sqlx.DB, taskID, password string) {
	t.Helper()
	_, err := db.ExecContext(
		t.Context(),
		`UPDATE tasks SET extract_password = ? WHERE id = ?`, password, taskID,
	)
	require.NoError(t, err)
}

// setPasswordList writes the extract_passwords settings row directly.
func setPasswordList(t *testing.T, db *sqlx.DB, list []string) {
	t.Helper()
	encoded, err := json.Marshal(list)
	require.NoError(t, err)
	_, err = db.ExecContext(
		t.Context(),
		`INSERT INTO settings (id, key, value_json, created_at, updated_at)
		 VALUES (?, 'extract_passwords', ?, 0, 0)
		 ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json`,
		"set_test_extract_passwords", string(encoded),
	)
	require.NoError(t, err)
}

// sharedPasswords reads back the stored extract_passwords list.
func sharedPasswords(t *testing.T, db *sqlx.DB) []string {
	t.Helper()
	list, err := store.NewSettingsStore(db).ExtractPasswords(t.Context())
	require.NoError(t, err)

	return list
}

// encryptedArchive builds a zip whose members are AES-encrypted under
// password — the listing succeeds for every candidate while `x` fails on a
// wrong one, which is what lets the candidate loop be observed.
func encryptedArchive(t *testing.T, bin, path, password string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "dir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, memberName), []byte(memberBody), 0o644))
	cmd := exec.Command(bin, "a", "-tzip", "-y", "-p"+password, path, filepath.Join(dir, "dir"))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "7zz a failed: %s", out)
}

// loggingSevenzip wraps the real binary in a shim that appends every argv
// to a log file before exec'ing it, so a test can assert which password
// candidate each `x` invocation carried.
func loggingSevenzip(t *testing.T) (bin, logPath string) {
	t.Helper()
	real := sevenzipPath(t)
	dir := t.TempDir()
	logPath = filepath.Join(dir, "argv.log")
	bin = filepath.Join(dir, "7zz")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"$DLTOOL_TEST_7ZZ_LOG\"\n" +
		"exec \"$DLTOOL_TEST_7ZZ_REAL\" \"$@\"\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
	t.Setenv("DLTOOL_TEST_7ZZ_LOG", logPath)
	t.Setenv("DLTOOL_TEST_7ZZ_REAL", real)

	return bin, logPath
}

// extractPasswordArgs returns, in invocation order, the password every
// `7zz x` call in the shim log carried ("" for the bare -p).
func extractPasswordArgs(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)

	var passwords []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "x" {
			continue
		}
		for _, field := range fields {
			if strings.HasPrefix(field, "-p") {
				passwords = append(passwords, strings.TrimPrefix(field, "-p"))
			}
		}
	}

	return passwords
}

func TestCandidateOrder(t *testing.T) {
	db := newTestDB(t)
	taskID := newCompletedTask(t, db, t.TempDir(), "payload.zip")
	setTaskPassword(t, db, taskID, "task-secret")
	setPasswordList(t, db, []string{"shared-one", "shared-two"})

	got, err := NewStorePasswords(db).Candidates(t.Context(), taskID)
	require.NoError(t, err)
	assert.Equal(t,
		[]string{"", "task-secret", "shared-one", "shared-two"}, got,
		"empty string, then the task password, then the shared list in order",
	)

	// A task with no per-task password falls back to the shared list only.
	other := newCompletedTask(t, db, t.TempDir(), "other.zip")
	got, err = NewStorePasswords(db).Candidates(t.Context(), other)
	require.NoError(t, err)
	assert.Equal(t, []string{"", "shared-one", "shared-two"}, got)

	_, err = NewStorePasswords(db).Candidates(t.Context(), "tsk_missing")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestCandidateTriedOnce(t *testing.T) {
	db := newTestDB(t)
	taskID := newCompletedTask(t, db, t.TempDir(), "payload.zip")
	setTaskPassword(t, db, taskID, "dup")
	setPasswordList(t, db, []string{"dup", "other", "other"})

	got, err := NewStorePasswords(db).Candidates(t.Context(), taskID)
	require.NoError(t, err)
	assert.Equal(t, []string{"", "dup", "other"}, got,
		"duplicates collapse to their first occurrence")

	// End to end: every candidate reaches 7zz exactly once, in order, and
	// a failed candidate is never retried.
	bin, logPath := loggingSevenzip(t)
	dest := t.TempDir()
	archive := filepath.Join(dest, "payload.zip")
	encryptedArchive(t, bin, archive, "right")

	extractTask := newCompletedTask(t, db, dest, archive)
	setTaskPassword(t, db, extractTask, "task-pw")
	setPasswordList(t, db, []string{"wrong-a", "wrong-b"})

	handler := NewExtractHandler(store.NewTaskStore(db), bin)
	require.NoError(t, handler.Handle(t.Context(), extractJobFor(extractTask)))
	assert.Equal(t, "error", taskState(t, db, extractTask))
	assert.Equal(t, codeExtractFailedWrongPassword, taskErrorCode(t, db, extractTask))

	assert.Equal(t,
		[]string{"", "task-pw", "wrong-a", "wrong-b"},
		extractPasswordArgs(t, logPath),
		"each candidate reached `7zz x` exactly once, in candidate order",
	)
}

func TestSuccessAppendsToSharedList(t *testing.T) {
	bin := sevenzipPath(t)
	db := newTestDB(t)
	dest := t.TempDir()
	archive := filepath.Join(dest, "payload.zip")
	encryptedArchive(t, bin, archive, "task-secret")

	taskID := newCompletedTask(t, db, dest, archive)
	setTaskPassword(t, db, taskID, "task-secret")
	setPasswordList(t, db, []string{"decoy"})

	handler := NewExtractHandler(store.NewTaskStore(db), bin)
	require.NoError(t, handler.Handle(t.Context(), extractJobFor(taskID)))
	assert.Equal(t, "completed", taskState(t, db, taskID),
		"error_message: %s", taskErrorDetail(t, db, taskID))
	assert.FileExists(t, filepath.Join(dest, "payload", memberName))

	assert.Equal(t, []string{"decoy", "task-secret"}, sharedPasswords(t, db),
		"the per-task password that opened the archive joined the shared list")

	// A candidate already on the list is not duplicated.
	require.NoError(t, NewStorePasswords(db).Remember(t.Context(), "task-secret"))
	assert.Equal(t, []string{"decoy", "task-secret"}, sharedPasswords(t, db))
}

func TestSharedListCappedAt16(t *testing.T) {
	db := newTestDB(t)
	settings := store.NewSettingsStore(db)

	seed := make([]string, MaxCandidates)
	for i := range seed {
		seed[i] = fmt.Sprintf("pw-%02d", i)
	}
	setPasswordList(t, db, seed)

	// The append keeps the newest entries and drops the oldest first.
	require.NoError(t, settings.AppendExtractPassword(t.Context(), "pw-new"))
	got := sharedPasswords(t, db)
	require.Len(t, got, MaxCandidates)
	assert.Equal(t, "pw-new", got[len(got)-1])
	assert.Equal(t, "pw-01", got[0], "the oldest entry is dropped first")

	// The read side caps the non-empty portion at MaxCandidates even when
	// the stored list is longer.
	oversized := make([]string, MaxCandidates+4)
	for i := range oversized {
		oversized[i] = fmt.Sprintf("pw-%02d", i)
	}
	setPasswordList(t, db, oversized)
	taskID := newCompletedTask(t, db, t.TempDir(), "payload.zip")
	candidates, err := NewStorePasswords(db).Candidates(t.Context(), taskID)
	require.NoError(t, err)
	assert.Len(t, candidates, MaxCandidates+1, "empty string plus the cap")
	assert.Equal(t, "", candidates[0])
}

func TestExhaustedListSetsWrongPassword(t *testing.T) {
	bin := sevenzipPath(t)
	db := newTestDB(t)
	dest := t.TempDir()
	archive := filepath.Join(dest, "payload.zip")
	encryptedArchive(t, bin, archive, "right")

	taskID := newCompletedTask(t, db, dest, archive)
	setTaskPassword(t, db, taskID, "wrong-task")
	setPasswordList(t, db, []string{"wrong-a"})

	handler := NewExtractHandler(store.NewTaskStore(db), bin)
	require.NoError(t, handler.Handle(t.Context(), extractJobFor(taskID)))

	assert.Equal(t, "error", taskState(t, db, taskID))
	assert.Equal(t, codeExtractFailedWrongPassword, taskErrorCode(t, db, taskID))
	assert.FileExists(t, archive, "the archive stays in place")
	assert.NoFileExists(t, filepath.Join(dest, "payload", memberName),
		"nothing is delivered")
	assert.NotContains(t, sharedPasswords(t, db), "wrong-task",
		"a candidate that failed is not remembered")
}

func TestPasswordNeverLogged(t *testing.T) {
	bin := sevenzipPath(t)
	db := newTestDB(t)
	dest := t.TempDir()
	archive := filepath.Join(dest, "payload.zip")
	marker := "pw-marker-never-logged"
	encryptedArchive(t, bin, archive, marker)

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	taskID := newCompletedTask(t, db, dest, archive)
	setTaskPassword(t, db, taskID, marker)
	setPasswordList(t, db, []string{"pw-marker-shared"})

	handler := NewExtractHandler(store.NewTaskStore(db), bin)
	require.NoError(t, handler.Handle(t.Context(), extractJobFor(taskID)))
	assert.Equal(t, "completed", taskState(t, db, taskID),
		"error_message: %s", taskErrorDetail(t, db, taskID))

	assert.NotContains(t, logs.String(), marker, "no log record carries the password")
	assert.NotContains(t, taskErrorDetail(t, db, taskID), marker)
	assert.NotContains(t, taskErrorDetail(t, db, taskID), "pw-marker-shared")

	var events []struct {
		Message    string  `db:"message"`
		DetailJSON *string `db:"detail_json"`
	}
	require.NoError(t, db.SelectContext(
		t.Context(), &events,
		`SELECT message, detail_json FROM task_events WHERE task_id = ?`, taskID,
	))
	for _, event := range events {
		assert.NotContains(t, event.Message, marker)
		if event.DetailJSON != nil {
			assert.NotContains(t, *event.DetailJSON, marker)
		}
	}

	// The shared list is the one sanctioned home for the value.
	assert.Equal(t, []string{"pw-marker-shared", marker}, sharedPasswords(t, db))
}

// TestCandidatesNeverLogOnFailure covers the error path: a failed run must
// not leak a candidate into the recorded error or the event log.
func TestCandidatesNeverLogOnFailure(t *testing.T) {
	bin := sevenzipPath(t)
	db := newTestDB(t)
	dest := t.TempDir()
	archive := filepath.Join(dest, "payload.zip")
	encryptedArchive(t, bin, archive, "pw-marker-right")

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	taskID := newCompletedTask(t, db, dest, archive)
	setTaskPassword(t, db, taskID, "pw-marker-task")
	setPasswordList(t, db, []string{"pw-marker-shared"})

	handler := NewExtractHandler(store.NewTaskStore(db), bin)
	require.NoError(t, handler.Handle(t.Context(), extractJobFor(taskID)))
	assert.Equal(t, codeExtractFailedWrongPassword, taskErrorCode(t, db, taskID))

	for _, sentinel := range []string{"pw-marker-task", "pw-marker-shared"} {
		assert.NotContains(t, logs.String(), sentinel)
		assert.NotContains(t, taskErrorDetail(t, db, taskID), sentinel)
	}
}

// TestRememberFailureIsExplicit covers the store-failure path: the error
// surfaces rather than dropping silently, and it does not carry the
// password.
func TestRememberFailureIsExplicit(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.Close())

	err := NewStorePasswords(db).Remember(context.Background(), "pw-sentinel-value")
	assert.Error(t, err)
	assert.NotContains(t, err.Error(), "pw-sentinel-value")
}
