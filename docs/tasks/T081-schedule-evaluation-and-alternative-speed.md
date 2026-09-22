# T081 — Apply the active schedule cell every minute

| Field | Value |
|---|---|
| **ID** | T081 |
| **Milestone** | M6 |
| **Status** | done |
| **Depends on** | T066, T079, T080 |
| **Blocks** | T083, T110 |
| **Parallel-safe** | no — extends `internal/jobs/cron.go`, `internal/engine/bandwidth.go` and `cmd/dl-tool/main.go` |
| **Implements** | [FR-091](../02-requirements.md#fr-091-apply-alternative-speeds-to-every-engine), [FR-093](../02-requirements.md#fr-093-apply-the-active-schedule-cell-every-minute) |
| **Decisions** | [ADR-0015](../decisions/0015-db-backed-in-process-job-queue.md), [ADR-0017](../decisions/0017-exclusive-control-of-engines.md) |
| **Est. size** | 1 new file, ~300 LOC plus the composition-root wiring this repair assigns to `cmd/dl-tool/main.go` |

## Goal
While the schedule is enabled, a cron entry evaluates the active cell once a minute and fans the result out
to every engine: `default` pushes the global limits, `alternative` pushes the alternative pair to **all**
engines including aria2 and yt-dlp, and `no_download` pauses every task dl-tool started — resuming exactly
those, and only those, when the cell changes.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §10 Bandwidth precedence and fan-out](../06-download-engines.md#10-bandwidth-precedence-and-fan-out)
2. [`docs/02-requirements.md` FR-093](../02-requirements.md#fr-093-apply-the-active-schedule-cell-every-minute)
3. [`docs/04-data-model.md` §3.6 Jobs, schedule and preferences](../04-data-model.md#36-jobs-schedule-and-preferences)
4. [`docs/14-conventions.md` §4 The `task_events` code vocabulary](../14-conventions.md#4-the-task_events-code-vocabulary)
5. [`docs/tasks/T079-global-bandwidth-governor.md`](T079-global-bandwidth-governor.md)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/jobs/cron.go` | modify | Add the once-a-minute bandwidth entry to T066's `Scheduler`. |
| `internal/jobs/cron_test.go` | create | Boundary, idempotence, user-pause and alternative-speed cases. |
| `internal/engine/bandwidth.go` | modify | Add `ApplyMode`, the paused-set bookkeeping and `Mode()`. |
| `internal/store/tasks.go` | modify | Add `ScheduleParked` and `ListScheduleParked`. |
| `cmd/dl-tool/main.go` | modify | Attach the parked-set store to the governor and the live governor to the scheduler. |

No other file may be modified.

## Interface contract

```go
package jobs

// Scheduler is T066's type. This task adds one field and one entry; it does not replace the type.
//
//	gov  *engine.Governor  // set by WithGovernor
//	now  func() time.Time  // injectable clock, time.Now by default

// WithGovernor attaches the bandwidth governor and registers the "* * * * *" entry that calls
// EvaluateSchedule. The entry is registered whether or not the schedule is enabled; the evaluator
// itself is the no-op while the schedule_enabled settings key is false.
func (s *Scheduler) WithGovernor(gov *engine.Governor) *Scheduler

// EvaluateSchedule reads the cell for now, resolves the mode and calls Governor.ApplyMode. It is
// idempotent: calling it repeatedly within one cell changes nothing at the engines.
func (s *Scheduler) EvaluateSchedule(ctx context.Context, now time.Time) error
```

```go
package engine

// ApplyMode makes m the active mode.
//
//	ModeDefault     → ApplyGlobal(download_rate_limit, upload_rate_limit), then resume the parked set
//	ModeAlternative → ApplyGlobal(alt_download_rate_limit, alt_upload_rate_limit), then resume the parked set
//	ModeNoDownload  → pause every task dl-tool started and park it; the limits are not changed
//
// A No Download cell PAUSES; it never throttles to a near-zero rate. Alternative speed is not an
// engine feature: it is a second global limit value pushed through the same SetRateLimits calls, so
// it reaches HTTP, FTP, SFTP, BitTorrent and media-site tasks alike.
func (g *Governor) ApplyMode(ctx context.Context, m Mode) error

// Mode returns the mode last applied.
func (g *Governor) Mode() Mode

// WithTasks attaches the TaskStore ApplyMode uses for the parked set; it returns g so the
// composition root chains it off T079's NewGovernor(reg, st), whose signature does not change.
// A nil store leaves the engine-side fan-out working, but ApplyMode must reject a `0` cell before
// pausing any engine, since without the store the parked ids could never be resumed; production
// always attaches it.
func (g *Governor) WithTasks(ts *store.TaskStore) *Governor
```

```go
package store

// ScheduleParked pauses the task and records that the schedule, not the user, paused it, by
// appending a task_events row with code "task.schedule.paused".
func (s *TaskStore) ScheduleParked(ctx context.Context, taskID string) error

// ListScheduleParked returns the ids of tasks that are currently paused AND whose most recent
// task_events row has code "task.schedule.paused". A task the user paused has "task.paused" as its
// most recent row and is therefore never resumed by the scheduler.
func (s *TaskStore) ListScheduleParked(ctx context.Context) ([]string, error)
```

The parked set lives in `task_events`, not in a new column: `task.schedule.paused` on park and
`task.schedule.resumed` on release, so the set survives a restart with no schema change.

## Steps
1. Edit `internal/jobs/cron.go` to add `WithGovernor`, the injectable clock and `EvaluateSchedule`, and to
   register one further `github.com/robfig/cron/v3 v3.0.1` entry with the spec `* * * * *`.
2. Return early from `EvaluateSchedule` when `schedule_enabled` is false, without touching any engine.
3. Resolve the cell as `day*24+hour` with Monday as day 0, from `now` in `time.Local`.
4. Add `ApplyMode` and `Mode` to `internal/engine/bandwidth.go`; make `ApplyMode` a no-op when the mode is
   unchanged, so the minute tick is idempotent.
5. Implement `ModeNoDownload` as a pause of every task in `downloading`, `checking` or `queued` that dl-tool
   started, each recorded through `ScheduleParked`. Seeding tasks and tasks dl-tool did not create are not
   touched.
6. Implement the release path: on a change away from `ModeNoDownload`, resume exactly the ids returned by
   `ListScheduleParked` and append `task.schedule.resumed` to each.
7. Add `ScheduleParked` and `ListScheduleParked` to `internal/store/tasks.go` with explicit column lists.
8. Edit `cmd/dl-tool/main.go`: attach the parked-set store at the existing governor construction —
   `engine.NewGovernor(server.Engines, store.NewSettingsStore(db)).WithTasks(store.NewTaskStore(db))`
   — and move the `jobs.NewScheduler(db, logger)` construction with its `Start(runCtx)` goroutine
   below the `LoadAndApply` block, calling `scheduler.WithGovernor(governor)` between construction
   and `Start`. Attaching before the cron goroutine runs needs no timing argument: the `gov` field
   is set before any entry can observe it.
9. Create `internal/jobs/cron_test.go` with an injected clock: assert `1 → 0 → 1` pauses then resumes the
   same ids; assert a task the user paused before the `0` cell is not resumed; assert `2` pushes the
   alternative pair to the aria2 fake as well as the qBittorrent fake; assert repeated ticks inside one cell
   issue no further engine calls; assert a disabled schedule issues none at all.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [x] The evaluator runs once a minute and is a no-op while `schedule_enabled` is false.
  `scheduleEvalSpec = "* * * * *"` is registered by `Start` for any attached governor;
  `TestDisabledScheduleDoesNothing` asserts the disabled tick issues zero engine calls and
  leaves `Governor.Mode()` unset.
- [x] A `2` cell changes the aria2 global limit as well as the qBittorrent one.
  `TestAlternativeReachesAria2` asserts both fakes record the identical `("", 2048, 1024)`
  `SetRateLimits` call after the `("", 100, 50)` default pair — one absolute value through
  the same call, no `toggleSpeedLimitsMode`.
- [x] A `0` cell pauses tasks; no engine receives a near-zero rate.
  `TestNoDownloadPausesAndResumesSameSet` asserts `Pause` on the live handles and
  `TestTickWithinCellIsIdempotent` asserts `rateCallCount() == 0` across the whole cell —
  `ModeNoDownload` never calls `applyGlobal`.
- [x] Leaving a `0` cell resumes exactly the parked ids and no user-paused task.
  `TestNoDownloadPausesAndResumesSameSet` resumes the four parked ids and
  `TestUserPausedTaskNotResumed` keeps the operator's `task.paused` row paused.
- [x] Repeated ticks within one cell issue no further engine calls.
  `TestTickWithinCellIsIdempotent` runs two ticks inside one `0` cell and two inside one `1`
  cell and asserts the call counts and event count never move.
- [x] A task dl-tool did not create is never paused, resumed or rate-limited.
  The parked set and the pause fan-out are enumerated exclusively from the `tasks` table
  (`SelectQueuedCandidates` + `ListNonTerminalByEngine`), which only ever holds dl-tool's
  rows; the pause assertions match the recorded engine ids exactly, so nothing outside the
  row set is ever addressed.
- [x] With no TaskStore attached, a `0` cell makes `ApplyMode` return an error and issue no
  pause call to any engine.
  `TestNoDownloadWithoutTaskStoreFailsClosed` asserts the error surfaces through
  `EvaluateSchedule`, `pauseList` stays empty and `Mode()` stays unset.
- [x] Review check (not assertable from `internal/jobs/cron_test.go`) — `cmd/dl-tool/main.go` builds
  the governor with `.WithTasks(store.NewTaskStore(db))` and hands the same instance to
  `scheduler.WithGovernor`, so a `2` cell reaches the production engines and a `0` cell records
  `task.schedule.paused` rows (docs/14-conventions.md §8.3).

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/jobs/... ./internal/engine/..." && echo SCHED_EVAL_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/jobs` and `ok  github.com/L-K-M/dl-tool/internal/engine`,
with `TestNoDownloadPausesAndResumesSameSet`, `TestUserPausedTaskNotResumed`,
`TestAlternativeReachesAria2`, `TestTickWithinCellIsIdempotent` and `TestDisabledScheduleDoesNothing` each
reported as `--- PASS`. The final line of stdout is exactly `SCHED_EVAL_OK`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement the DST repeated-hour and skipped-hour semantics or report the zone name; T110 owns both.
- Do NOT implement the `min()` chain against per-task limits; T110 owns it.
- Do NOT add the schedule endpoints; T080 owns them.
- Do NOT call `transfer/toggleSpeedLimitsMode` or `transfer/speedLimitsMode`; dl-tool pushes one absolute
  value it computed itself.
- Do NOT add a new `tasks` column for the parked set; the `task_events` codes are the record.
- Do NOT change the `rss_poll` entry T066 registered, and do NOT add the watch-folder entry; T083 owns it.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
`make lint && make test PKG="./internal/jobs/... ./internal/engine/..." && echo SCHED_EVAL_OK`:

```
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
go test -race -count=1 ./internal/jobs/... ./internal/engine/...
ok  	github.com/L-K-M/dl-tool/internal/jobs	22.094s
ok  	github.com/L-K-M/dl-tool/internal/engine	30.525s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.289s
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	9.111s
SCHED_EVAL_OK
```

The five named tests plus the nil-store acceptance case and the two review-added
regressions (startup evaluation, appended-event parked membership), from a `-v`
run of the same tree:

```
=== RUN   TestNoDownloadPausesAndResumesSameSet
--- PASS: TestNoDownloadPausesAndResumesSameSet (0.60s)
=== RUN   TestUserPausedTaskNotResumed
--- PASS: TestUserPausedTaskNotResumed (0.63s)
=== RUN   TestAlternativeReachesAria2
--- PASS: TestAlternativeReachesAria2 (0.52s)
=== RUN   TestTickWithinCellIsIdempotent
--- PASS: TestTickWithinCellIsIdempotent (0.51s)
=== RUN   TestDisabledScheduleDoesNothing
--- PASS: TestDisabledScheduleDoesNothing (0.39s)
=== RUN   TestAppendedEventKeepsParkedMembership
--- PASS: TestAppendedEventKeepsParkedMembership (0.49s)
=== RUN   TestStartAppliesCellImmediately
--- PASS: TestStartAppliesCellImmediately (0.53s)
=== RUN   TestNoDownloadWithoutTaskStoreFailsClosed
--- PASS: TestNoDownloadWithoutTaskStoreFailsClosed (0.48s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/jobs	5.215s
```

Scope:

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
cmd/dl-tool/main.go
internal/engine/bandwidth.go
internal/jobs/cron.go
internal/jobs/cron_test.go
internal/store/tasks.go
```

Exactly the Files table and nothing else.

Notes for the record, neither a `## Blocked`:

- **i18next keys.** docs/14-conventions.md §4 asks for one key per new code in
  `web/src/locales/en/*.json`, which sits outside this task's Files table — the T024
  fallback applies: record them here. `task.schedule.paused` and `task.schedule.resumed`
  each need an `event` key in `web/src/locales/en/errors.json` when a task that owns the
  file lands; until then the event log falls back to the stored message.
- **F125 remains open.** A task *created* inside a `0` cell is not re-parked —
  `ApplyMode` is deliberately a no-op on an unchanged mode, per step 4, and admission
  gating belongs to T098/the plan-level decision F125 records. Disclosed, not resolved.
- **Park enumeration.** `ApplyMode` builds the pausable set from
  `SelectQueuedCandidates` (every `queued` row, handle or not) and
  `ListNonTerminalByEngine` (`downloading`/`checking`), so a queued task is parked
  without an engine call while a live transfer is paused engine-side first. Engine
  `ErrNotFound` on pause is tolerated — a forgotten handle is gone, not a failure.

## Blocked

The contract attaches the bandwidth governor to the Scheduler with `WithGovernor(gov *engine.Governor)`,
but no file in the `## Files` table can supply one — and step 8 forbids the only file that can. The only
`*engine.Registry` in the process is `server.Engines`, built inside `api.NewServer` from `cfg`
(`internal/api/server.go:240`); the only `*engine.Governor` is built from it at `cmd/dl-tool/main.go:260`.
The only `Scheduler` construction site is `cmd/dl-tool/main.go:232` — the "existing `Scheduler`
construction site" step 8 names — which (a) is in a file step 8 forbids editing and (b) runs *before* the
governor exists, so even a hand-off cannot precede `Start`. `NewScheduler` cannot build the governor
itself: `internal/jobs` holds only the `*sqlx.DB`, it cannot import `internal/api` (that package already
imports `internal/jobs` in `internal/api/search.go` — an import cycle), and rebuilding adapters from
`db` needs `cfg` and the concrete adapter packages, which docs/14-conventions.md §§8.1/8.3 reserve for
the composition root. `NewScheduler(db, log)` also cannot gain a governor parameter: its signature is
pinned by the uneditable call at main.go:232. docs/14-conventions.md §8.3 counts a setter with no caller
as not done, and every in-scope alternative is an improvisation the plan never specified:

- **An empty-registry governor** built inside `NewScheduler` gives `WithGovernor` a caller, but
  `ApplyGlobal` fans out to zero engines and `ModeNoDownload` pauses nothing engine-side — rows read
  `paused` while transfers keep writing. That is the "built and never wired" failure §8.3 exists to
  catch, hidden behind green unit tests that inject fakes.
- **A package-global governor** that `NewGovernor` publishes and `EvaluateSchedule` resolves per tick is
  the only in-table mechanism that can reach the live instance, but it replaces the documented attach
  (production would never call `WithGovernor`), adds a process singleton the contract does not describe,
  and still cannot attach eagerly: the scheduler is constructed at main.go:232, the governor at
  main.go:260, so the contract's own field shape (`gov *engine.Governor`, "set by WithGovernor") holds
  no value at the time the one permitted call site runs.

Rerunnable evidence on this commit:

```bash
# The only production registry and governor; both call sites live outside the Files table.
git grep -nE "engine.NewRegistry|engine.NewGovernor" HEAD -- '*.go' | grep -v _test | sed 's|^[^:]*:||'
# → internal/api/server.go:240 builds the registry; cmd/dl-tool/main.go:260 builds the governor.

# No other file constructs a Scheduler.
git grep -nE "NewScheduler" HEAD -- '*.go' | grep -v _test | sed 's|^[^:]*:||'
# → cmd/dl-tool/main.go:232 is the sole non-test construction site.

# main.go:232 constructs the scheduler before main.go:260 builds the governor.
grep -n "NewScheduler\|scheduler\.Start\|NewGovernor" cmd/dl-tool/main.go

# api already imports jobs, so jobs cannot reach server.Engines through api (import cycle).
grep -rn "internal/jobs" internal/api/*.go | grep -v _test
# → internal/api/search.go:25

# The Governor's only store collaborator; SettingsStore exposes no TaskStore for the parked set.
grep -n "func NewGovernor\|type SettingsStore struct\|func (s \*TaskStore) Settings" \
    internal/engine/bandwidth.go internal/store/settings.go
# → NewGovernor(reg *Registry, st *store.SettingsStore); SettingsStore.db is unexported;
#   only TaskStore.Settings() exists — the wrong direction.
```

A second gap rides the same missing caller: the contract moves the paused-set bookkeeping into
`internal/engine/bandwidth.go` (`ApplyMode` calls `ScheduleParked`/`ListScheduleParked` and scans the
pausable states), which needs `*store.TaskStore`. `NewGovernor(reg, st)` is pinned by the main.go:260
call and `SettingsStore.db` is unexported, so the Governor's tasks collaborator needs a delivery call
site (e.g. `gov.WithTasks(...)`) invoked by the same caller that would invoke `WithGovernor` — which
does not exist either.

Remedies for the owner — the choice changes the composition root or the contract, so it is not made
here (deciding files: this task's `## Files` table, docs/14-conventions.md §8.3):

1. Add `cmd/dl-tool/main.go` to this task's `## Files` table. The repair is a few lines: construct the
   scheduler with `jobs.NewScheduler(db, logger)` after `engine.NewGovernor` (or keep the order and call
   `scheduler.WithGovernor(governor)` after it), and attach the tasks collaborator at the same site —
   e.g. widen `NewGovernor(reg, st, ts)` or add `governor.WithTasks(store.NewTaskStore(db))`. Same repair
   class the owner applied to T071.
2. Alternatively add `internal/api/server.go` to the table and move scheduler construction into
   `NewServer`, which already owns `engines` and a `store.NewTaskStore(db)`. A larger repair: it rewrites
   T066's call site at main.go:232 — so this remedy also needs `cmd/dl-tool/main.go` in the Files table
   to retire the old construction site.
3. Alternatively prescribe the package-global publish explicitly (`NewGovernor` records the process
   governor; `EvaluateSchedule` resolves it per tick; `NewScheduler` attaches the tasks store), amending
   the contract to say so. Keeps step 8's "no main.go edit" promise at the price of an uncontracted
   process singleton and a `WithGovernor` that production never calls.

Status stays `todo` pending plan repair — do not re-dispatch; the owner must pick remedy 1–3 or amend
the contract first.

**Repair applied 2026-09-22.** The ruling picked remedy 1, the smallest interpretation consistent with
the accepted ADRs and the merged code: this task's `## Files` table now carries `cmd/dl-tool/main.go`,
and the contract pins the seams — `Governor.WithTasks` delivers the `*store.TaskStore` the parked-set
bookkeeping needs while leaving T079's `NewGovernor(reg, st)` signature untouched — the nil-store
rejection the contract mandates is covered by a new acceptance item exercised from
`internal/jobs/cron_test.go`, so `internal/engine/bandwidth_test.go` stays out of scope — and step 8
now attaches the live instance with
`scheduler.WithGovernor(governor)` instead of forbidding the file. The gap is registered as F671 and
marked resolved by this repair, as is the stale '2 new files' count in the `Est. size` row (F502).
The index row stays `todo`; the next loop iteration implements the task.
