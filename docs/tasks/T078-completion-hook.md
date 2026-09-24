# T078 — Run the completion hook as an argument vector

| Field | Value |
|---|---|
| **ID** | T078 |
| **Milestone** | M6 |
| **Status** | done |
| **Depends on** | T074, T092 |
| **Blocks** | — |
| **Parallel-safe** | no — it also edits the shared files `cmd/dl-tool/main.go`, `internal/api/settings_test.go`, `internal/jobs/postprocess.go` |
| **Implements** | [FR-105](../02-requirements.md#fr-105-run-a-completion-hook-installed-by-the-operator), [NFR-015](../02-requirements.md#nfr-015-never-interpolate-configuration-into-a-shell) |
| **Decisions** | [ADR-0010](../decisions/0010-never-execute-third-party-definitions.md), [ADR-0011](../decisions/0011-alpine-runtime-with-puid-pgid.md) |
| **Est. size** | 2 new files, ~220 LOC |

## Goal
When the operator has placed an executable at `<DLTOOL_CONFIG_DIR>/hooks/on-complete`, a finished task runs
it once as an argument vector with a fixed environment and a wall-clock timeout. The switch is three-state
and re-evaluated per finished task, never cached at boot: absent means off (the default), present and
executable means on, present but not executable means off with a `warn` naming the path — emitted by the
same per-task evaluation, so installing a broken hook at runtime still produces feedback. Its command can
never be set, read or edited through the HTTP API, and no HTTP endpoint writes any file into
`<DLTOOL_CONFIG_DIR>`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/02-requirements.md` FR-105](../02-requirements.md#fr-105-run-a-completion-hook-installed-by-the-operator)
2. [`docs/12-security-and-threat-model.md` §6.7 Open redirects, configuration lock, exposure](../12-security-and-threat-model.md#67-open-redirects-configuration-lock-exposure)
3. [`docs/11-config-reference.md` §2 `DLTOOL_` variables (application)](../11-config-reference.md#2-dltool_-variables-application)
4. [`docs/14-conventions.md` §4 The `task_events` code vocabulary](../14-conventions.md#4-the-task_events-code-vocabulary)
5. [`docs/tasks/T074-auto-extract-archives.md`](T074-auto-extract-archives.md)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/jobs/hook.go` | create | `Hook`, its discovery, the argv, the fixed environment and the timeout. |
| `internal/jobs/hook_test.go` | create | Off-by-default, argv, environment, timeout and non-zero-exit cases. |
| `internal/jobs/postprocess.go` | modify | Run the hook as the chain's last step. |
| `internal/api/settings_test.go` | modify | `TestSettingsRejectsHookKey`: a hook-named key gets the same `422` as any other unknown key, and `GET /settings` never returns the hook path. No handler change — a distinct rejection would reveal the key is special. |
| `cmd/dl-tool/main.go` | edit | Hand `cfg.ConfigDir` to the post-processing chain — `NewChain` gains the directory parameter (or a `SetConfigDir` beside `SetNotifier`; the shape is the implementer's). |

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
type Hook struct{ /* path string; db *sqlx.DB */ }

// NewHook returns a Hook, or ok false when no executable hook is installed.
func NewHook(configDir string, db *sqlx.DB) (h *Hook, ok bool)

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
   notify, and to ignore a non-zero exit for the purposes of task state. The chain needs
   `cfg.ConfigDir` for discovery — wire it at the composition root's only construction site,
   `cmd/dl-tool/main.go`'s `jobs.NewChain(db, store.NewTaskStore(db))` call, by giving `NewChain`
   the directory parameter or a `SetConfigDir` attach beside `SetNotifier`. Never precompute the
   discovery result at the root: the switch is re-evaluated per finished task, so installing a hook
   mid-run takes effect on the next completion.
8. Edit `internal/api/settings_test.go` to add `TestSettingsRejectsHookKey`, beside T092's
   `TestPatchUnknownKeyIs422`: assert a `PATCH /settings` body carrying any key whose name contains
   `hook` is rejected with `422` `/problems/validation-failed` — indistinguishably from any other
   unknown key, since a distinct error would reveal the key is special — and that `GET /settings`
   never returns the hook path. The test lives in `internal/api`, not `internal/jobs`: `internal/api`
   already imports `internal/jobs`, so an in-package assertion would be an import cycle.
9. Create `internal/jobs/hook_test.go`: assert `NewHook` reports `ok:false` on an empty config directory;
   assert the child receives its arguments as separate argv entries and not as one shell string; assert the
   environment is exactly the fixed list; assert a sleeping hook is killed after `HookTimeout` and yields
   `ErrHookTimeout`; assert a non-zero exit leaves the task `completed`.
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
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

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

```bash
$ make lint && make test PKG="./internal/jobs/... ./internal/api/..." && echo HOOK_OK
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
go test -race -count=1 ./internal/jobs/... ./internal/api/...
ok  	github.com/L-K-M/dl-tool/internal/jobs	34.877s
ok  	github.com/L-K-M/dl-tool/internal/api	208.571s
HOOK_OK
```

Named tests (run with `-v`):

```text
--- PASS: TestHookOffByDefault (0.06s)
--- PASS: TestArgvNotShellString (0.04s)
--- PASS: TestFixedEnvironment (0.06s)
--- PASS: TestHookTimeoutKillsGroup (0.19s)
--- PASS: TestNonZeroExitKeepsCompleted (0.03s)
ok  	github.com/L-K-M/dl-tool/internal/jobs	0.397s
--- PASS: TestSettingsRejectsHookKey (0.08s)
ok  	github.com/L-K-M/dl-tool/internal/api	0.098s
```

Scope check (`git status --porcelain=v1 -uall -- . ':(exclude)docs'`):

```text
cmd/dl-tool/main.go
internal/api/settings_test.go
internal/jobs/hook.go
internal/jobs/hook_test.go
internal/jobs/postprocess.go
```

Exactly the Files table, nothing else.

## Blocked — resolved

**Remedy applied verbatim.** `cmd/dl-tool/main.go` joins `## Files` so the composition root hands
`cfg.ConfigDir` to the post-processing chain — `NewChain` gains the directory parameter (or a
`SetConfigDir` beside `SetNotifier`; the shape is the implementer's) — and step 7 names the call
site while forbidding a one-time precomputed discovery (the switch re-evaluates per finished task).
The contract's `st *store.Store` now names the merged `*sqlx.DB` reality (`NewNotifier(db, …)`). On
the api side the record's `package jobs_test` alternative is not used: `newSettingsTestEnv` and the
humatest fixture live inside `internal/api` test files and are not importable from `internal/jobs`,
so `TestSettingsRejectsHookKey` lands in `internal/api/settings_test.go` beside
`TestPatchUnknownKeyIs422` — and `internal/api/settings_test.go` replaces `settings.go` in
`## Files`, since the closed key set already rejects hook-named keys indistinguishably and a
hook-specific rejection would reveal the key is special. T092 joins `Depends on`: the task now
edits its test file. The original record is preserved below.

---

### 2026-09-24 — `cfg.ConfigDir` cannot reach the chain inside the Files table

Step 7 makes the hook the chain's last step, so `OnCompleted` must call
`NewHook(configDir, …)` — the contract pins the directory to
`cfg.ConfigDir`. The chain is constructed exactly once, at the
composition root: `cmd/dl-tool/main.go` calls
`jobs.NewChain(db, store.NewTaskStore(db))` and that function is the only
place the process's `cfg` is in scope. `cmd/dl-tool/main.go` is not in
`## Files`, and every in-table route to the directory fails:

- `Chain` holds `{db, tasks, notify}` — no config field. Giving
  `NewChain` a parameter, or a `SetConfigDir` setter a call, both edit
  `cmd/dl-tool/main.go`.
- `internal/jobs` never reads the environment: `internal/config` is the
  single reader of every `DLTOOL_` name (doc 11 §2), and a second
  `os.Getenv("DLTOOL_CONFIG_DIR")` would duplicate `defaultConfigDir` —
  a hardcoded path, and a second parser for a setting the config package
  owns.
- `*sqlx.DB` exposes no DSN, and `pragma_database_list` yields
  `cfg.DBPath`, which `config.Load` validates independently of
  `ConfigDir` — "the database may live elsewhere"
  (`internal/config/config.go`) — so `filepath.Dir(dbfile)` is not the
  hooks root.
- No `settings` row carries a config path; `00001_init.sql` seeds only
  `max_active_total`, `max_active_per_engine` and `min_free_space`.
- Wiring a preconstructed `*Hook` from the root would still need
  `main.go`, and would be wrong besides: the switch must be re-evaluated
  per finished task — installing the hook mid-run takes effect on the
  next completion (doc 11 §2) — so the composition root must hand the
  chain the directory, never a one-time discovery result.
- A variadic `NewChain(db, tasks, configDir ...string)` compiles against
  the unmodified call site but leaves the production chain holding no
  directory — the hook would be dead code, the §8.3 "built and never
  wired" defect the acceptance criteria exist to forbid.

This is the same defect class T091's first record named for its own
table — work with no legal call site — and the repair that unblocked
T091 (#277) applies verbatim.

Rerunnable evidence on this commit:

```bash
# The only Chain construction site — outside the Files table.
grep -rn 'jobs.NewChain(' cmd/ internal/ --include='*.go' | grep -v _test
#   cmd/dl-tool/main.go:190: postprocess := jobs.NewChain(db, store.NewTaskStore(db))

# internal/jobs reads no environment and imports no config package.
grep -rn 'os.Getenv\|internal/config' internal/jobs/ --include='*.go' | grep -v _test
#   (no output)

# DBPath is validated independently of ConfigDir — Dir(db) is not the hooks root.
grep -n 'database may live elsewhere' internal/config/config.go
#   internal/config/config.go:278

# The seeded settings carry no path.
grep -n 'INSERT INTO settings' internal/store/migrations/00001_init.sql
```

### Remedy

Add one row to `## Files` — `cmd/dl-tool/main.go | edit` for passing
`cfg.ConfigDir` to the post-processing chain — plus a step-7 clause
naming the call site (a `NewChain` third parameter, or a `SetConfigDir`
beside `SetNotifier`; the shape is the implementer's). At the same time
the contract's `st *store.Store` should name the merged reality —
`*sqlx.DB` per F086 (`NewNotifier(db, …)`), the same drift T091's repair
corrected.

Step 8 needs no repair: T092's `PATCH /settings` already maps a
hook-named key to `422` `/problems/validation-failed` through the
unknown-key path — `TestPatchUnknownKeyIs422` already exercises
`"completion_hook"` — so the remaining edit is the explicit guard the
step prescribes. And `TestSettingsRejectsHookKey` can live in
`internal/jobs/hook_test.go` as `package jobs_test`: an external test
package may sit beside `package jobs` tests, and importing
`internal/api` — which imports `internal/jobs` — is no cycle for it.

One secondary drift the repair may settle or leave: doc 14 §4 asks for
an i18next key per new `task_events` code, and
`web/src/locales/en/errors.json` is not in the table — but
`postprocess.extract.started`, `postprocess.extract.completed` and
`postprocess.autoremoved` already ship with no key, so the message
fallback is the running precedent.

### Deferral mechanics — same shape as the first record

- The picker takes the topmost eligible `todo` row and T078 heads the
  eligible set, so leaving it `todo` re-selects it on every iteration
  and nothing below it can start. The row is set to `deferred` — the
  status the picker skips — in this file's `**Status**` cell and both
  `00-task-index.md` rows (the first T078 deferral, #250, and T091's,
  #267).
- No row depends on T078 (`Blocks: —`), so the deferral stalls nothing
  downstream.
- Reactivation trigger: flip both index rows and the `**Status**` cell
  back to `todo`, close this `## Blocked` record as resolved by the
  repairing change (mirroring the T092 record below), and add
  `cmd/dl-tool/main.go` to `## Files` — all in the same change, so the
  deferral cannot outlive its cause.

## Blocked — resolved

Resolved by T092 (merged): `PATCH /settings` exists and rejects a hook-named
key with `422` `/problems/validation-failed`, so step 8's surface is in place.
The original record is preserved below.

---

Step 8 and its acceptance criterion require `PATCH /settings` to answer `422`
`/problems/validation-failed` for a hook-named key, but no `/settings` route exists. The endpoint —
and the settings write path behind it — belongs to T092, which is still `todo` behind T091 (`todo`
but eligible: T006, T012 and T066 are all `done`). This task's `Depends on` lists only T074, so the
row the picker took is missing the edge to the task that builds the surface step 8 edits.

Rerunnable evidence on this commit:

```bash
# No /settings operation is registered — the only routes under it are T092's future work.
grep -rn 'Path:.*"/settings' internal/api/        # no output
grep -n '"/settings"' api/openapi.json            # no output
# PATCH /settings is T092's, and three task files forbid building it elsewhere.
grep -n 'PATCH /settings' docs/tasks/T092-settings-and-system-info.md
grep -rn 'T092 owns' docs/tasks/T027-list-and-test-engines.md \
    docs/tasks/T079-global-bandwidth-governor.md docs/tasks/T117-rss-settings-section.md
```

The Files table does name `internal/api/settings.go`, and `SettingsHandlers.registerOperations` is
already wired into `internal/api/server.go`, so a `patch-settings` registration could be added inside
the table — but every in-scope shape is an improvisation the plan never specified:

- **A stub that returns `422` for every key**, because no write path exists to call. Doc 05 §11.1
  defines `PATCH /settings` as accepting a valid subset with `200`; a reject-all operation registers
  the contract's path while inverting its success case — a `PATCH {"auto_extract":true}` would be
  told the key is unknown. It also lands `patch-settings` in `api/openapi.json`, which T092 then has
  to reconcile with its own registration step.
- **The real write path**, which T092's interface contract places in `internal/store/settings.go`
  (`PutSettings`) — outside this Files table — and which cannot be shrunk to a verbatim upsert:
  `internal/api/server.go`'s `parseNonNegativeSettingInt` assumes the write side rejects negative
  `max_active_*` values ("the write side rejects it, so the read side must too"), and
  `extract_passwords` needs the `"__redacted__"` no-op rule, so an interim writer either reimplements
  T092's validation grammar or wedges the admission pass and lets a client overwrite the stored
  secret with the placeholder literal.

Related: step 9 puts `TestSettingsRejectsHookKey` in `internal/jobs/hook_test.go`, and `package jobs`
test files cannot reach the API — `internal/api` already imports `internal/jobs`
(`internal/api/search.go`), so an in-package test would be an import cycle. The PATCH assert would
need a `jobs_test` file or a home in `internal/api`, which the Files table does not list.

Remedies for the owner — the choice changes the dependency graph or T092's scope, so it is not made
here (deciding files: `docs/tasks/T092-settings-and-system-info.md`, Doc 05 §11.1):

1. Add `T092` to this task's `Depends on`. T078 then runs after T091 and T092 land, and step 8 is
   exercised against the real endpoint. T091 is itself unblocked, so the stall is two tasks.
2. Move step 8's PATCH half, the `PATCH /settings` acceptance criterion and
   `TestSettingsRejectsHookKey` into T092, whose step 8 already tests "an unknown key returning
   `422`" — the exact mechanism FR-105's verify prescribes ("the same body shape as any other
   unknown settings key"). T078 then drops `internal/api/settings.go` from its Files table and keeps
   the hook.
3. Prescribe a deferral-register interim — a registered `patch-settings` that rejects every key
   until T092 lands — if the hook should ship ahead of the settings endpoints.

Under remedies 1 and 3, step 9's PATCH assert still cannot live in `internal/jobs/hook_test.go`:
the import cycle is ordering-independent — it must move to a `package jobs_test` file or
`internal/api` in those branches too.

### 2026-09-21 — re-verified on b080192, row set to `deferred`

The blocker is unchanged on b080192, the head of `main`:

```bash
grep -rn '"/settings' internal/api/                 # no output: no /settings route literal, any style
grep -n '"/settings' api/openapi.json               # no output: covers /settings and /settings/* paths
grep -n 'PutSettings' internal/store/ internal/api/ # no output: T092's write path is absent
grep -n 'internal/jobs' internal/api/*.go           # internal/api/search.go: the import cycle stands
```

Additions to the record:

- The picker takes the topmost eligible `todo` row, and T078 heads the eligible
  set, so leaving it `todo` re-selects it on every iteration and nothing below it
  can start. The row is set to `deferred`, the status the picker skips, so the
  queue can proceed. This chooses none of the remedies above: the owner still
  decides, and un-deferring is a one-word flip back to `todo`. FR-105 stays a
  `must`; the deferral parks the task rather than waiving the requirement, and
  M6's exit checkpoint does not exercise the hook, so the gap stays visible only
  through this record.
- FR-105's verify prescribes "the same body shape as any other unknown settings
  key". T092's design already delivers it: `PATCH /settings` accepts a closed key
  set and maps `store.ErrUnknownSettingKey` to `422`
  `/problems/validation-failed`, with `TestPatchUnknownKeyIs422` named in its
  verification. Under remedy 2 the PATCH half of this task shrinks to naming a
  hook-shaped key in that test; no hook-specific rejection is wanted, because a
  distinct rejection is what would reveal the key is special.
- Reactivation trigger: flip both T078 rows in `00-task-index.md` — the `## M6`
  milestone-table row and the `## Roster` detail-table row — back to `todo` in
  the same change that lands the chosen remedy — under remedy 2, T092's
  completion — so the deferral cannot outlive its cause. T092's
  `## Cross-task notes` section carries the reverse link. No row depends on
  T078, so the deferral stalls nothing downstream.
- This deferral change also corrects T078's `Parallel` cell in the roster
  detail table from `yes` to `no`, matching this file's `Parallel-safe` field;
  the correction is unrelated to the deferral itself.
