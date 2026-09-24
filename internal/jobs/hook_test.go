package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/store"
)

// TestMain doubles as the completion-hook child: the fixed environment
// marks the process as a hook invocation, so it reports its argv and
// environment verbatim and exits instead of running the suite. The
// re-exec is the only probe that sees the child's environment before a
// script interpreter adds its own variables.
func TestMain(m *testing.M) {
	if os.Getenv("DLTOOL_TASK_ID") != "" && os.Getenv("DLTOOL_TASK_STATE") != "" {
		fmt.Println("---ARGV---")
		for _, arg := range os.Args {
			fmt.Println(arg)
		}
		fmt.Println("---ENV---")
		for _, entry := range os.Environ() {
			fmt.Println(entry)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// installHook writes a hook file at the one legal location with the
// given mode and returns its path.
func installHook(t *testing.T, configDir, body string, mode os.FileMode) string {
	t.Helper()

	path := HookPath(configDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), mode))

	return path
}

// installProbeHook links the running test binary in as the hook, so the
// child reports its own argv and environment through the captured
// stdout.
func installProbeHook(t *testing.T, configDir string) string {
	t.Helper()

	exe, err := os.Executable()
	require.NoError(t, err)

	path := HookPath(configDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.Symlink(exe, path))

	return path
}

// hookDetail returns the detail_json of the task's event row carrying
// code, failing the test when the row is absent.
func hookDetail(t *testing.T, db *sqlx.DB, taskID, code string) string {
	t.Helper()

	var detail *string
	require.NoError(t, db.GetContext(
		t.Context(), &detail,
		`SELECT detail_json FROM task_events WHERE task_id = ? AND code = ?`, taskID, code,
	))
	require.NotNil(t, detail, "expected a task_events row with code %s", code)

	return *detail
}

// hookLevel returns the level of the task's event row carrying code.
func hookLevel(t *testing.T, db *sqlx.DB, taskID, code string) string {
	t.Helper()

	var level string
	require.NoError(t, db.GetContext(
		t.Context(), &level,
		`SELECT level FROM task_events WHERE task_id = ? AND code = ?`, taskID, code,
	))

	return level
}

// probeReport decodes the captured stdout the probe hook printed: the
// child's argv entries and its environment as a map.
func probeReport(t *testing.T, stdout string) (argv []string, env map[string]string) {
	t.Helper()

	require.True(t, strings.HasPrefix(stdout, "---ARGV---\n"), "probe output %q", stdout)
	parts := strings.SplitN(strings.TrimPrefix(stdout, "---ARGV---\n"), "---ENV---\n", 2)
	require.Len(t, parts, 2, "probe output missing the environment section: %q", stdout)

	argv = strings.Split(strings.TrimRight(parts[0], "\n"), "\n")

	env = map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(parts[1], "\n"), "\n") {
		key, value, found := strings.Cut(line, "=")
		require.True(t, found, "env emitted a malformed line %q", line)
		env[key] = value
	}

	return argv, env
}

// newHookTask inserts one completed task with the given fields and
// returns the stored row.
func newHookTask(t *testing.T, db *sqlx.DB, name, destination string, contentPath *string, totalBytes *int64) store.Task {
	t.Helper()

	task, err := store.NewTaskStore(db).Create(t.Context(), store.Task{
		Engine:      "aria2",
		SourceKind:  "http",
		Name:        name,
		State:       "completed",
		Destination: destination,
		ContentPath: contentPath,
		TotalBytes:  totalBytes,
	})
	require.NoError(t, err)

	return task
}

// TestHookOffByDefault pins the three-state switch: absent is off and
// silent, present but not executable is off with a warn naming the path,
// and a file made executable after the fact runs on the next completion —
// the switch is re-evaluated per finished task, never cached.
func TestHookOffByDefault(t *testing.T) {
	db := newTestDB(t)
	configDir := t.TempDir()

	hook, ok := NewHook(configDir, db)
	assert.False(t, ok, "an empty config directory must not yield a hook")
	assert.Nil(t, hook)

	dest := t.TempDir()
	contentPath := filepath.Join(dest, "payload.bin")
	task := newHookTask(t, db, "payload.bin", dest, &contentPath, nil)

	chain := NewChain(db, store.NewTaskStore(db))
	chain.SetConfigDir(configDir)

	require.NoError(t, chain.OnCompleted(t.Context(), task.ID))
	for _, code := range []string{eventHookCompleted, eventHookFailed, eventHookSkipped} {
		assert.NotContains(t, eventCodes(t, db, task.ID), code,
			"no hook event may be written while no file is installed")
	}

	// Present but not executable: off, with a warn naming the path.
	path := installHook(t, configDir, "#!/bin/sh\nexit 0\n", 0o644)

	hook, ok = NewHook(configDir, db)
	assert.False(t, ok, "a non-executable file must not yield a hook")
	assert.Nil(t, hook)

	require.NoError(t, chain.OnCompleted(t.Context(), task.ID))
	assert.Equal(t, "warn", hookLevel(t, db, task.ID, eventHookSkipped))

	var row struct {
		Message string `db:"message"`
		Detail  string `db:"detail_json"`
	}
	require.NoError(t, db.GetContext(
		t.Context(), &row,
		`SELECT message, detail_json FROM task_events WHERE task_id = ? AND code = ?`,
		task.ID, eventHookSkipped,
	))
	assert.Contains(t, row.Message, path, "the warn must name the unusable file")
	assert.Contains(t, row.Detail, path)

	// Fixing the mode mid-run switches the hook on at the next
	// completion — no restart, no cached verdict.
	require.NoError(t, os.Chmod(path, 0o755))
	require.NoError(t, chain.OnCompleted(t.Context(), task.ID))
	assert.Contains(t, eventCodes(t, db, task.ID), eventHookCompleted)
}

// TestArgvNotShellString pins the argument-vector contract: the child
// receives six argv entries — the hook path plus five task fields — and
// task-supplied text carrying shell metacharacters reaches it verbatim,
// never interpreted.
func TestArgvNotShellString(t *testing.T) {
	db := newTestDB(t)
	configDir := t.TempDir()
	path := installProbeHook(t, configDir)

	dest := t.TempDir()
	contentPath := filepath.Join(dest, "payload.bin")
	total := int64(4096)
	name := `semi;colon $(id) $HOME ` + "`reboot`"
	task := newHookTask(t, db, name, dest, &contentPath, &total)

	hook, ok := NewHook(configDir, db)
	require.True(t, ok)
	require.NoError(t, hook.Run(t.Context(), task))

	var detail struct {
		Stdout string `json:"stdout"`
	}
	require.NoError(t, json.Unmarshal([]byte(hookDetail(t, db, task.ID, eventHookCompleted)), &detail))

	argv, _ := probeReport(t, detail.Stdout)
	require.Equal(t, []string{path, task.ID, "completed", name, dest, contentPath}, argv,
		"the child must receive six argv entries, each verbatim")
	assert.NotContains(t, detail.Stdout, "uid=",
		"$(id) in the task name must not have been interpreted")
}

// TestFixedEnvironment pins the child's environment: exactly the fixed
// PATH plus the six DLTOOL_TASK_* values, and nothing inherited from the
// dl-tool process.
func TestFixedEnvironment(t *testing.T) {
	db := newTestDB(t)
	configDir := t.TempDir()
	installProbeHook(t, configDir)

	dest := t.TempDir()
	contentPath := filepath.Join(dest, "payload.bin")
	total := int64(4096)
	task := newHookTask(t, db, "environment probe", dest, &contentPath, &total)

	hook, ok := NewHook(configDir, db)
	require.True(t, ok)
	require.NoError(t, hook.Run(t.Context(), task))

	var detail struct {
		Stdout string `json:"stdout"`
	}
	require.NoError(t, json.Unmarshal([]byte(hookDetail(t, db, task.ID, eventHookCompleted)), &detail))

	_, env := probeReport(t, detail.Stdout)
	assert.Equal(t, map[string]string{
		"PATH":                     "/usr/local/bin:/usr/bin:/bin",
		"DLTOOL_TASK_ID":           task.ID,
		"DLTOOL_TASK_NAME":         "environment probe",
		"DLTOOL_TASK_STATE":        "completed",
		"DLTOOL_TASK_DESTINATION":  dest,
		"DLTOOL_TASK_CONTENT_PATH": contentPath,
		"DLTOOL_TASK_TOTAL_BYTES":  "4096",
	}, env, "the child environment must be exactly the fixed list")
}

// TestHookTimeoutKillsGroup pins the wall clock: a hook that outlives
// its timeout has its whole process group killed and yields
// ErrHookTimeout. The script parks behind a background child holding the
// output pipes, so a kill of only the direct child would still hang
// Wait for the full sleep — a fast return proves the group died.
func TestHookTimeoutKillsGroup(t *testing.T) {
	db := newTestDB(t)
	configDir := t.TempDir()
	installHook(t, configDir, "#!/bin/sh\nsleep 60 &\nwait\n", 0o755)

	dest := t.TempDir()
	contentPath := filepath.Join(dest, "payload.bin")
	task := newHookTask(t, db, "sleeper", dest, &contentPath, nil)

	hook := &Hook{
		path:    HookPath(configDir),
		db:      db,
		timeout: 150 * time.Millisecond,
	}

	start := time.Now()
	err := hook.Run(t.Context(), task)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrHookTimeout), "want ErrHookTimeout, got %v", err)
	assert.Less(t, elapsed, 10*time.Second,
		"the run must return at the timeout, not when the children would exit")

	assert.Equal(t, "warn", hookLevel(t, db, task.ID, eventHookFailed))
	var detail struct {
		Timeout bool `json:"timeout"`
	}
	require.NoError(t, json.Unmarshal([]byte(hookDetail(t, db, task.ID, eventHookFailed)), &detail))
	assert.True(t, detail.Timeout)
}

// TestNonZeroExitKeepsCompleted pins the verdict isolation: a hook that
// exits non-zero writes one postprocess.hook.failed warn row — carrying
// its captured output — and the task stays completed.
func TestNonZeroExitKeepsCompleted(t *testing.T) {
	db := newTestDB(t)
	configDir := t.TempDir()
	installHook(t, configDir, "#!/bin/sh\necho hook complained >&2\nexit 3\n", 0o755)

	dest := t.TempDir()
	contentPath := filepath.Join(dest, "payload.bin")
	task := newHookTask(t, db, "failing hook target", dest, &contentPath, nil)

	chain := NewChain(db, store.NewTaskStore(db))
	chain.SetConfigDir(configDir)
	require.NoError(t, chain.OnCompleted(t.Context(), task.ID))

	assert.Equal(t, "completed", taskState(t, db, task.ID),
		"a hook failure must never change the task's state")
	assert.Equal(t, "warn", hookLevel(t, db, task.ID, eventHookFailed))

	var detail struct {
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}
	require.NoError(t, json.Unmarshal([]byte(hookDetail(t, db, task.ID, eventHookFailed)), &detail))
	assert.Equal(t, 3, detail.ExitCode)
	assert.Contains(t, detail.Stderr, "hook complained")
}
