package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/jmoiron/sqlx"
	"golang.org/x/sys/unix"

	"github.com/L-K-M/dl-tool/internal/store"
)

// hookFilename is the only name a completion hook may carry.
const hookFilename = "on-complete"

// HookPath is the only place a completion hook may live: an executable
// file inside the config directory. There is no environment variable and
// no settings key for it, so a compromised API session cannot introduce
// or change the command that runs (FR-105).
func HookPath(configDir string) string {
	return filepath.Join(configDir, "hooks", hookFilename)
}

// HookTimeout is the wall clock the child gets before its process group
// is killed.
const HookTimeout = 60 * time.Second

// ErrHookTimeout is returned when the child outlived HookTimeout; its
// process group was killed.
var ErrHookTimeout = errors.New("jobs: completion hook timed out")

// The postprocess.hook.* task_events codes of docs/14-conventions.md
// section 4. skipped is the warn the three-state switch owes a
// present-but-unusable hook file: absent is silent, installed and
// executable runs.
const (
	eventHookCompleted = "postprocess.hook.completed"
	eventHookFailed    = "postprocess.hook.failed"
	eventHookSkipped   = "postprocess.hook.skipped"
)

// hookOutputCap bounds each captured stream recorded into the task
// event's detail_json — 8 KiB of head, never an unbounded dump.
const hookOutputCap = 8 << 10

// hookPathEnv is the child's whole PATH: a fixed, minimal search path so
// the hook sees the same base tools regardless of how dl-tool was
// launched. The parent environment is never inherited.
const hookPathEnv = "PATH=/usr/local/bin:/usr/bin:/bin"

// hookWaitDelay bounds how long Wait may spend draining the output pipes
// after the process is done or the deadline fired: a hook that
// daemonizes — setsid or double-fork — escapes the group kill while
// still holding the inherited write ends, and without the bound Wait
// would hang past the deadline instead of yielding ErrHookTimeout.
const hookWaitDelay = 5 * time.Second

// discoverHook is the per-finished-task evaluation of the three-state
// switch. present reports that something sits at HookPath at all — the
// warn case — and runnable that it is a regular file the dropped
// PUID/PGID may actually execute, checked against the real uid/gid, not
// just any execute bit. The file is never created and its mode never
// changed; the operator owns it entirely.
func discoverHook(configDir string) (present, runnable bool) {
	info, err := os.Stat(HookPath(configDir))
	if err != nil {
		return false, false
	}
	if !info.Mode().IsRegular() {
		return true, false
	}

	return true, unix.Access(HookPath(configDir), unix.X_OK) == nil
}

// Hook runs the completion hook for one task.
type Hook struct {
	path string
	db   *sqlx.DB
	// timeout is the run's wall clock — HookTimeout in production,
	// overridable so the in-package tests do not wait out a minute.
	timeout time.Duration
}

// NewHook returns a Hook, or ok false when no executable hook is
// installed. Discovery runs on every call: the chain invokes it per
// finished task, so installing or fixing the file mid-run takes effect
// on the next completion without a restart.
func NewHook(configDir string, db *sqlx.DB) (h *Hook, ok bool) {
	if _, runnable := discoverHook(configDir); !runnable {
		return nil, false
	}

	return &Hook{path: HookPath(configDir), db: db, timeout: HookTimeout}, true
}

// Run executes the hook exactly once for the task, as an argument
// vector, never through a shell:
//
//	exec.CommandContext(ctx, h.path, taskID, state, name, destination, contentPath)
//
// The child's environment is fixed and complete — it inherits nothing.
// No secret, token, password, session or engine credential is ever
// placed in the argv or the environment. stdout and stderr are captured,
// capped at 8 KiB each and written to the task event.
//
// The verdict never changes the task's state: a non-zero exit writes one
// postprocess.hook.failed warn row and returns nil; a successful run
// writes postprocess.hook.completed. ErrHookTimeout is returned when the
// child outlived the wall clock — the failed row is still written first.
// An error return means the verdict itself could not be recorded.
func (h *Hook) Run(ctx context.Context, t store.Task) error {
	timeout := h.timeout
	if timeout <= 0 {
		timeout = HookTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	contentPath := ""
	if t.ContentPath != nil {
		contentPath = *t.ContentPath
	}
	cmd := exec.CommandContext(
		runCtx, h.path,
		t.ID, t.State, t.Name, t.Destination, contentPath,
	)
	// Setpgid plus the group kill: the deadline must take down the whole
	// tree the hook spawned, not just the direct child — the same recipe
	// the extractor follows.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = hookWaitDelay
	cmd.Env = hookEnv(t)
	var stdout, stderr hookBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	tasks := store.NewTaskStore(h.db)
	detail := hookEventDetail{
		Path:   h.path,
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		// The hook's own wall clock fired — not the caller's context —
		// and the group kill already landed. The failure is recorded
		// like any other, then reported as ErrHookTimeout.
		detail.Timeout = true
		if err := tasks.AppendEvent(
			ctx, t.ID, "warn", eventHookFailed,
			fmt.Sprintf("completion hook exceeded the %ds wall clock", int64(timeout/time.Second)),
			detail,
		); err != nil {
			return fmt.Errorf("jobs: record hook timeout of task %q: %w", t.ID, err)
		}

		return ErrHookTimeout
	case runErr != nil:
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) && exitErr.ExitCode() >= 0 {
			code := exitErr.ExitCode()
			detail.ExitCode = &code
		} else {
			// A start failure or a signal death carries no exit status;
			// the raw error is the record.
			detail.Error = runErr.Error()
		}
		return tasks.AppendEvent(
			ctx, t.ID, "warn", eventHookFailed,
			hookFailureMessage(runErr), detail,
		)
	default:
		zero := 0
		detail.ExitCode = &zero
		return tasks.AppendEvent(
			ctx, t.ID, "info", eventHookCompleted,
			"completion hook finished", detail,
		)
	}
}

// hookFailureMessage renders the warn row's text: the exit status where
// the child produced one, the error itself where it did not.
func hookFailureMessage(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
		return fmt.Sprintf("completion hook exited with status %d", exitErr.ExitCode())
	}

	return fmt.Sprintf("completion hook failed: %s", err)
}

// hookEventDetail is the detail_json of the hook's task_events rows:
// the captured streams plus the exit state the run ended in.
type hookEventDetail struct {
	Path     string `json:"path"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Timeout  bool   `json:"timeout,omitempty"`
	Error    string `json:"error,omitempty"`
}

// hookEnv is the child's complete environment, assigned never appended:
// the fixed PATH plus the six DLTOOL_TASK_* values, and nothing
// inherited — no secret, token or engine credential can leak into a
// hook-spawned process. A NULL column renders as the empty string so the
// variable is always present.
func hookEnv(t store.Task) []string {
	contentPath := ""
	if t.ContentPath != nil {
		contentPath = *t.ContentPath
	}
	totalBytes := ""
	if t.TotalBytes != nil {
		totalBytes = strconv.FormatInt(*t.TotalBytes, 10)
	}

	return []string{
		hookPathEnv,
		"DLTOOL_TASK_ID=" + t.ID,
		"DLTOOL_TASK_NAME=" + t.Name,
		"DLTOOL_TASK_STATE=" + t.State,
		"DLTOOL_TASK_DESTINATION=" + t.Destination,
		"DLTOOL_TASK_CONTENT_PATH=" + contentPath,
		"DLTOOL_TASK_TOTAL_BYTES=" + totalBytes,
	}
}

// hookBuffer is the bounded sink for the child's captured output: the
// task event records the head of each stream, never an unbounded dump a
// chatty hook could produce.
type hookBuffer struct {
	buf bytes.Buffer
}

func (b *hookBuffer) Write(p []byte) (int, error) {
	remaining := hookOutputCap - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			b.buf.Write(p[:remaining])
		} else {
			b.buf.Write(p)
		}
	}

	return len(p), nil
}

func (b *hookBuffer) String() string { return b.buf.String() }

// runHook is the chain's completion-hook step (T078): the three-state
// switch is re-evaluated on every pass — installing or fixing the file
// mid-run takes effect on the next completion, and a present-but-unusable
// file is reported each time so the misconfiguration stays visible. A
// hook verdict is already recorded as a task event, so only an
// event-write failure propagates.
func (c *Chain) runHook(ctx context.Context, task store.Task) error {
	if c.configDir == "" {
		// A chain never handed the config directory — the tests that
		// build one without SetConfigDir — has no hook surface at all.
		return nil
	}

	hook, ok := NewHook(c.configDir, c.db)
	if ok {
		if err := hook.Run(ctx, task); err != nil && !errors.Is(err, ErrHookTimeout) {
			return err
		}
		return nil
	}

	if present, _ := discoverHook(c.configDir); present {
		return c.tasks.AppendEvent(
			ctx, task.ID, "warn", eventHookSkipped,
			fmt.Sprintf("completion hook %s is present but not executable", HookPath(c.configDir)),
			map[string]string{"path": HookPath(c.configDir)},
		)
	}

	return nil
}
