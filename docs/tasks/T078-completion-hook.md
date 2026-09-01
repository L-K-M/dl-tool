# T078 — Run the completion hook as an argument vector

| Field | Value |
|---|---|
| **ID** | T078 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T074 |
| **Blocks** | T108 |
| **Parallel-safe** | yes — adds `internal/jobs/hook.go` |
| **Implements** | [FR-105](../02-requirements.md#fr-105-run-a-completion-hook-only-when-explicitly-enabled), [NFR-015](../02-requirements.md#nfr-015-never-interpolate-configuration-into-a-shell) |
| **Decisions** | [ADR-0010](../decisions/0010-never-execute-third-party-definitions.md), [ADR-0011](../decisions/0011-alpine-runtime-with-puid-pgid.md) |
| **Est. size** | 2 new files, ~220 LOC |

## Goal
When the operator has placed an executable at `<DLTOOL_CONFIG_DIR>/hooks/on-complete`, a finished task runs
it once as an argument vector with a fixed environment and a wall-clock timeout. The hook is off when the
file is absent, and its command can never be set, read or edited through the HTTP API.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/02-requirements.md` FR-105](../02-requirements.md#fr-105-run-a-completion-hook-only-when-explicitly-enabled)
2. [`docs/12-security-and-threat-model.md` §6.7 Open redirects, configuration lock, exposure](../12-security-and-threat-model.md#67-open-redirects-configuration-lock-exposure)
3. [`docs/11-config-reference.md` §2 `DLTOOL_` variables (application)](../11-config-reference.md#2-dltool_-variables-application)
4. [`docs/14-conventions.md` §4 The `task_events` code vocabulary](../14-conventions.md#4-the-task_events-code-vocabulary)
5. [`docs/tasks/T074-auto-extract-archives.md`](T074-auto-extract-archives.md)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/jobs/hook.go` | create | `Hook`, its discovery, the argv, the fixed environment and the timeout. |
| `internal/jobs/hook_test.go` | create | Off-by-default, argv, environment, timeout and API-refusal cases. |
| `internal/jobs/postprocess.go` | modify | Run the hook as the chain's last step. |
| `internal/api/settings.go` | modify | Reject any settings key naming a hook command with `422`. |

No other file may be modified.

## Interface contract

```go
package jobs

// HookPath is the only place a completion hook may live: an executable file inside the config
// directory. There is no environment variable and no settings key for it, so a compromised API
// session cannot introduce or change the command that runs.
//
//	filepath.Join(cfg.ConfigDir, "hooks", "on-complete")
//
// The hook is enabled exactly when that path exists, is a regular file and is executable by the
// dropped PUID/PGID. Absent means off, which is the default.
func HookPath(configDir string) string

// HookTimeout is the wall clock the child gets before its process group is killed.
const HookTimeout = 60 * time.Second

// Hook runs the completion hook for one task.
type Hook struct{ /* path string; store *store.Store */ }

// NewHook returns a Hook, or ok false when no executable hook is installed.
func NewHook(configDir string, st *store.Store) (h *Hook, ok bool)

// Run executes the hook exactly once for the task, as an argument vector, never through a shell:
//
//	exec.CommandContext(ctx, h.path, taskID, state, name, destination, contentPath)
//
// The child's environment is fixed and complete — it inherits nothing:
//
//	PATH=/usr/local/bin:/usr/bin:/bin
//	DLTOOL_TASK_ID, DLTOOL_TASK_NAME, DLTOOL_TASK_STATE,
//	DLTOOL_TASK_DESTINATION, DLTOOL_TASK_CONTENT_PATH, DLTOOL_TASK_TOTAL_BYTES
//
// No secret, token, password, session or engine credential is ever placed in the argv or the
// environment. stdout and stderr are captured, capped at 8 KiB each and written to the task event.
func (h *Hook) Run(ctx context.Context, t store.Task) error

// ErrHookTimeout is returned when the child outlived HookTimeout; its process group was killed.
var ErrHookTimeout = errors.New("jobs: completion hook timed out")
```

The hook's exit status never changes the task's state: a non-zero exit writes one `task_events` row with
code `postprocess.hook.failed` and level `warn`, and the chain continues. A successful run writes
`postprocess.hook.completed`.

## Steps
1. Create `internal/jobs/hook.go` with `HookPath`, `HookTimeout`, `Hook`, `NewHook`, `Run` and
   `ErrHookTimeout`.
2. Implement discovery: `os.Stat` the path, require a regular file with an executable bit, and return
   `ok = false` otherwise. Never create the file and never change its mode.
3. Build the command with `exec.CommandContext` and the argv above. The string `sh -c` must appear nowhere
   in the package, and no argument may be assembled by string concatenation of task-supplied text.
4. Set `cmd.Env` to exactly the fixed list above — assign it, never append to `os.Environ()`.
5. Give the command a `HookTimeout` context, put the child in its own process group and kill that group on
   expiry, returning `ErrHookTimeout`.
6. Capture stdout and stderr into 8 KiB caps and record them in the `task_events` row's `detail_json`.
7. Edit `internal/jobs/postprocess.go` to call `Run` as the chain's last step, after extract, move and
   notify, and to ignore a non-zero exit for the purposes of task state.
8. Edit `internal/api/settings.go` so a `PATCH /settings` body carrying any key whose name contains `hook`
   is rejected with `422` `/problems/validation-failed` and the hook path is never returned by
   `GET /settings`.
9. Create `internal/jobs/hook_test.go`: assert `NewHook` reports `ok:false` on an empty config directory;
   assert the child receives its arguments as separate argv entries and not as one shell string; assert the
   environment is exactly the fixed list; assert a sleeping hook is killed after `HookTimeout` and yields
   `ErrHookTimeout`; assert a non-zero exit leaves the task `completed`; assert `PATCH /settings` with a
   hook key returns `422`.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] With no file at `<config>/hooks/on-complete`, no process is ever spawned.
- [ ] The child receives six argv entries; a task name containing `;` and `$(id)` reaches it verbatim and is
      not interpreted.
- [ ] The child's environment is exactly the seven documented variables and nothing inherited.
- [ ] A hook exceeding `HookTimeout` has its process group killed and yields `ErrHookTimeout`.
- [ ] A non-zero exit writes `postprocess.hook.failed` and leaves the task `completed`.
- [ ] `PATCH /settings` with any hook-named key returns `422` and changes nothing.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/jobs/... ./internal/api/..." && echo HOOK_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/jobs` and `ok  github.com/L-K-M/dl-tool/internal/api`, with
`TestHookOffByDefault`, `TestArgvNotShellString`, `TestFixedEnvironment`, `TestHookTimeoutKillsGroup`,
`TestNonZeroExitKeepsCompleted` and `TestSettingsRejectsHookKey` each reported as `--- PASS`. The final line
of stdout is exactly `HOOK_OK`. No `FAIL`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT add a `DLTOOL_HOOK_*` environment variable or a `hook` settings key; the file's presence is the
  whole configuration surface, and that is what keeps the API off the execution path.
- Do NOT run the hook through `sh`, `bash`, `exec.Command("sh", "-c", …)` or any shell.
- Do NOT pass an engine credential, session token, API token or extraction password to the child.
- Do NOT let the hook's exit status change `tasks.state` or `tasks.error_code`.
- Do NOT add a scripting runtime or a plugin loader; ADR-0010 forbids executing third-party code that
  dl-tool itself supplies.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
