# T081 — Apply the active schedule cell every minute

| Field | Value |
|---|---|
| **ID** | T081 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T066, T079, T080 |
| **Blocks** | T083, T110 |
| **Parallel-safe** | no — extends `internal/jobs/cron.go` and `internal/engine/bandwidth.go` |
| **Implements** | [FR-091](../02-requirements.md#fr-091-apply-alternative-speeds-to-every-engine), [FR-093](../02-requirements.md#fr-093-apply-the-active-schedule-cell-every-minute) |
| **Decisions** | [ADR-0015](../decisions/0015-db-backed-in-process-job-queue.md), [ADR-0017](../decisions/0017-exclusive-control-of-engines.md) |
| **Est. size** | 2 new files, ~300 LOC |

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
8. Attach the governor from the existing `Scheduler` construction site; do not edit `cmd/dl-tool/main.go` in
   this task, because T066's `Start(ctx)` already owns that call site.
9. Create `internal/jobs/cron_test.go` with an injected clock: assert `1 → 0 → 1` pauses then resumes the
   same ids; assert a task the user paused before the `0` cell is not resumed; assert `2` pushes the
   alternative pair to the aria2 fake as well as the qBittorrent fake; assert repeated ticks inside one cell
   issue no further engine calls; assert a disabled schedule issues none at all.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] The evaluator runs once a minute and is a no-op while `schedule_enabled` is false.
- [ ] A `2` cell changes the aria2 global limit as well as the qBittorrent one.
- [ ] A `0` cell pauses tasks; no engine receives a near-zero rate.
- [ ] Leaving a `0` cell resumes exactly the parked ids and no user-paused task.
- [ ] Repeated ticks within one cell issue no further engine calls.
- [ ] A task dl-tool did not create is never paused, resumed or rate-limited.

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
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

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
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
