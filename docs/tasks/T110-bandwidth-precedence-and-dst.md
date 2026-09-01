# T110 — Resolve the bandwidth precedence chain and the schedule time zone

| Field | Value |
|---|---|
| **ID** | T110 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T079, T080, T081, T082 |
| **Blocks** | T118 |
| **Parallel-safe** | no — extends `internal/engine/bandwidth.go` and `internal/jobs/cron.go` |
| **Implements** | [FR-096](../02-requirements.md#fr-096-combine-schedule-global-and-per-task-limits-by-minimum), [FR-097](../02-requirements.md#fr-097-evaluate-the-schedule-in-the-container-time-zone) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md), [ADR-0017](../decisions/0017-exclusive-control-of-engines.md) |
| **Est. size** | 0 new files, ~280 LOC |

## Goal
A task's effective rate is `min(schedule cell limit, global limit, per-task limit)` per direction, with `0`
meaning unlimited and excluded from the minimum. A `No Download` cell pauses rather than throttling. The
grid is evaluated in the container's `TZ`: the repeated hour applies twice and the skipped hour not at all.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §10 Bandwidth precedence and fan-out](../06-download-engines.md#10-bandwidth-precedence-and-fan-out)
2. [`docs/02-requirements.md` FR-096](../02-requirements.md#fr-096-combine-schedule-global-and-per-task-limits-by-minimum)
3. [`docs/02-requirements.md` FR-097](../02-requirements.md#fr-097-evaluate-the-schedule-in-the-container-time-zone)
4. [`docs/11-config-reference.md` §3 Container-level variables (entrypoint, not the application)](../11-config-reference.md#3-container-level-variables-entrypoint-not-the-application)
5. [`docs/09-web-ui-spec.md` §9.1 The 24×7 schedule grid](../09-web-ui-spec.md#91-the-247-schedule-grid)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/bandwidth.go` | modify | Add `Effective` and route `ApplyGlobal` and `ApplyTask` through it. |
| `internal/engine/bandwidth_test.go` | modify | The precedence table, including the worked example of FR-096. |
| `internal/jobs/cron.go` | modify | Evaluate the cell in `time.Local` with the DST rules. |
| `internal/jobs/cron_test.go` | modify | Both Europe/Zurich transitions against an injected clock. |
| `internal/api/settings_schedule.go` | modify | Report the active zone name and the active cell. |

No other file may be modified.

## Interface contract

```go
package engine

// Effective resolves one direction. This is the single implementation of the precedence chain
// stated in docs/06-download-engines.md §10; no handler, adapter or job may reimplement it.
//
//	effective = min(cell, global, task)
//
// evaluated in that order, in bytes per second. 0 means unlimited and is EXCLUDED from the min();
// if every term is 0 the result is 0, meaning unlimited. A ModeNoDownload cell is not a rate at
// all: Effective reports pause true and the caller pauses the task instead of sending a rate.
func Effective(mode Mode, cell, global, task int64) (rate int64, pause bool)

// Resolve computes both directions for one task and is what ApplyTask calls.
//
//	ModeDefault     → cell limits are download_rate_limit / upload_rate_limit
//	ModeAlternative → cell limits are alt_download_rate_limit / alt_upload_rate_limit
//	ModeNoDownload  → pause; no rate is sent to any engine
func (g *Governor) Resolve(ctx context.Context, t store.Task) (l Limits, pause bool)
```

The worked case of [FR-096](../02-requirements.md#fr-096-combine-schedule-global-and-per-task-limits-by-minimum),
which the test asserts literally:

| Term | Value |
|---|---|
| Alternative-speed cell | `5242880` |
| Global limit | `10485760` |
| Per-task limit | `1048576` |
| **Sent to the engine** | **`1048576`** |
| Cell then set to `0` (`No Download`) | The task is **paused**; no rate is sent |

```go
package jobs

// activeCell resolves the grid index for now in time.Local, the container's TZ.
//
// Daylight saving is handled by evaluating the wall-clock hour of now, not by arithmetic on a UTC
// instant:
//   - on the repeated hour of a fall-back transition the same cell is applied twice, once per
//     wall-clock occurrence;
//   - on the skipped hour of a spring-forward transition that cell is never applied.
// Neither case is special-cased in code: reading now.Hour() and now.Weekday() in time.Local gives
// exactly this behaviour, and the tests exist to prove it stays that way.
func activeCell(cells [168]store.ScheduleMode, now time.Time) store.ScheduleMode
```

`GET /settings/schedule` reports `timezone` as `time.Local.String()` and `active_mode` as the cell in force
at the moment of the call, so the UI can display both beside the grid and in the status bar.

## Steps
1. Add `Effective` to `internal/engine/bandwidth.go` with the documented `min()` semantics and the
   zero-means-unlimited exclusion.
2. Add `Resolve`, selecting the default or alternative pair from the mode, and route `ApplyGlobal` and
   `ApplyTask` through it so no other code path computes a rate.
3. Make `ModeNoDownload` return `pause true` and never a near-zero rate; the caller pauses through the T081
   park-and-release bookkeeping.
4. Add `activeCell` to `internal/jobs/cron.go`, reading `now.Weekday()` and `now.Hour()` in `time.Local`
   with Monday as day 0, and make `EvaluateSchedule` call it.
5. Edit `internal/api/settings_schedule.go` to report `timezone` and the currently `active_mode`.
6. Extend `internal/engine/bandwidth_test.go` with a table covering: the worked case above; every term `0`
   yielding unlimited; one term `0` being excluded rather than winning the minimum; the per-task term
   winning; the global term winning; the cell term winning; and `ModeNoDownload` returning `pause`.
7. Extend `internal/jobs/cron_test.go` with an injected clock in `Europe/Zurich`: step through the
   fall-back transition and assert the cell for that wall-clock hour is applied on both occurrences; step
   through the spring-forward transition and assert the skipped hour's cell is never applied; assert the
   schedule response carries `Europe/Zurich`.
8. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `Effective` is the only implementation of the precedence chain in the repository.
- [ ] The worked case yields `1048576`, the per-task value.
- [ ] `0` is excluded from the minimum, not treated as the smallest value.
- [ ] All three terms `0` yields `0`, meaning unlimited.
- [ ] A `No Download` cell pauses; no engine ever receives a near-zero rate.
- [ ] The repeated DST hour applies its cell twice and the skipped hour not at all, in `Europe/Zurich`.
- [ ] `GET /settings/schedule` reports the active time-zone name.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/engine/... ./internal/jobs/... ./internal/api/..." && echo PRECEDENCE_OK
```
Expected: `ok` lines for `github.com/L-K-M/dl-tool/internal/engine`,
`github.com/L-K-M/dl-tool/internal/jobs` and `github.com/L-K-M/dl-tool/internal/api`, with
`TestEffectiveWorkedCase`, `TestZeroExcludedFromMin`, `TestAllZeroIsUnlimited`,
`TestNoDownloadPausesNotThrottles`, `TestDSTRepeatedHourAppliedTwice`, `TestDSTSkippedHourNeverApplied` and
`TestScheduleReportsTimezone` each reported as `--- PASS`. The final line of stdout is exactly
`PRECEDENCE_OK`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT re-implement the fan-out calls; T079 owns `ApplyGlobal` and T082 owns `ApplyTask`.
- Do NOT add the schedule endpoints or the grid storage; T080 owns them.
- Do NOT special-case a DST transition with arithmetic on a UTC instant; evaluate the wall clock in
  `time.Local` and let the standard library do it.
- Do NOT accept a time zone from a client or a settings key; the container's `TZ` is the only source.
- Do NOT throttle a `No Download` cell to 1 byte/s or any other small value.
- Do NOT apply any of this to a transfer dl-tool did not create; ADR-0017 leaves foreign transfers alone.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
