# T080 — Store and serve the 24×7 schedule grid

| Field | Value |
|---|---|
| **ID** | T080 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T027, T079 |
| **Blocks** | T081, T108, T110, T118 |
| **Parallel-safe** | no — it also edits the shared files `internal/api/server.go`, `internal/store/settings.go` |
| **Implements** | [FR-092](../02-requirements.md#fr-092-store-and-edit-a-247-schedule-grid) |
| **Decisions** | [ADR-0003](../decisions/0003-chi-huma-code-first-openapi.md), [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md) |
| **Est. size** | 2 new files, ~250 LOC |

## Goal
`GET /settings/schedule` returns the 168-cell grid and its enabled flag; `PUT /settings/schedule` replaces
all 168 cells in one transaction. A body that is not exactly 168 integers in `0..2` is rejected with `422`
and nothing is written.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §11.2 `GET /settings/schedule` and `PUT /settings/schedule`](../05-api-contract.md#112-get-settingsschedule-and-put-settingsschedule)
2. [`docs/04-data-model.md` §3.6 Jobs, schedule and preferences](../04-data-model.md#36-jobs-schedule-and-preferences)
3. [`docs/09-web-ui-spec.md` §9.1 The 24×7 schedule grid](../09-web-ui-spec.md#91-the-247-schedule-grid)
4. [`docs/11-config-reference.md` §5 Database-backed settings](../11-config-reference.md#5-database-backed-settings)
5. [`docs/02-requirements.md` FR-092](../02-requirements.md#fr-092-store-and-edit-a-247-schedule-grid)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/settings_schedule.go` | create | The two handlers, their Huma structs and the cell validation. |
| `internal/api/settings_schedule_test.go` | create | Round-trip, length, range, permission and transaction cases. |
| `internal/store/settings.go` | modify | Add `Schedule` and `ReplaceSchedule` over `bandwidth_schedule`. |
| `internal/api/server.go` | modify | Register `get-schedule` and `put-schedule`. |

No other file may be modified.

## Interface contract

```go
package store

// ScheduleMode is the stored bandwidth_schedule.mode value.
type ScheduleMode string

const (
	ScheduleNoDownload  ScheduleMode = "no_download"
	ScheduleDefault     ScheduleMode = "default"
	ScheduleAlternative ScheduleMode = "alternative"
)

// Schedule reads all 168 rows and returns them as cells indexed day*24+hour, day 0 = Monday.
// The table always holds exactly 168 rows, seeded by 00001_init.sql with 'default'.
func (s *SettingsStore) Schedule(ctx context.Context) (cells [168]ScheduleMode, err error)

// ReplaceSchedule writes all 168 rows in one sqlx.Tx. A partial write is impossible: either every
// cell lands or none does.
func (s *SettingsStore) ReplaceSchedule(ctx context.Context, cells [168]ScheduleMode) error
```

```go
package api

// ScheduleBody is the wire shape of doc 05 §11.2. Cells are integers, not mode strings:
// 0 = no download, 1 = default speed, 2 = alternative speed.
type ScheduleBody struct {
	Enabled    bool   `json:"enabled"`
	Cells      []int  `json:"cells" minItems:"168" maxItems:"168" doc:"168 cells indexed day*24+hour, day 0 = Monday"`
	Timezone   string `json:"timezone" readOnly:"true" doc:"IANA name of the zone the cells are evaluated in"`
	ActiveMode string `json:"active_mode" readOnly:"true" enum:"no_download,default,alternative" doc:"the cell in force at the moment of the call"`
}

type GetScheduleOutput struct{ Body ScheduleBody }
type PutScheduleInput struct{ Body ScheduleBody }
type PutScheduleOutput struct{ Body ScheduleBody }

func (h *SettingsHandlers) GetSchedule(ctx context.Context, in *struct{}) (*GetScheduleOutput, error)
func (h *SettingsHandlers) PutSchedule(ctx context.Context, in *PutScheduleInput) (*PutScheduleOutput, error)

// CellToMode and ModeToCell are the only translation between the wire integers and the stored enum.
func CellToMode(c int) (store.ScheduleMode, error)
func ModeToCell(m store.ScheduleMode) int
```

`enabled` is the `schedule_enabled` settings key, default `false`. `timezone` is `time.Local.String()`, the
container's `TZ`, and is read-only: a client cannot set it. `active_mode` is read-only too; T110 fills it
from the cell in force at the moment of the call. Until T110 lands, return the cell for the current hour. Statuses: `200` · `403` `/problems/forbidden`
· `422` `/problems/validation-failed` when `cells` is not exactly 168 elements or
holds a value outside `0..2`.

## Steps
1. Add `ScheduleMode`, `Schedule` and `ReplaceSchedule` to `internal/store/settings.go` with explicit
   column lists; `ReplaceSchedule` runs one `sqlx.Tx` with `defer tx.Rollback()` before the commit.
2. Create `internal/api/settings_schedule.go` with `ScheduleBody`, the two operations and `CellToMode` /
   `ModeToCell`.
3. Validate the array before any write: exactly 168 elements, every element in `0..2`; return `422` with an
   `errors[].location` naming the offending index.
4. Read `schedule_enabled` for `Enabled` and write it inside the same transaction as the cells.
5. Set `Timezone` from `time.Local` on both responses so the UI can display it beside the grid, and ignore
   any `timezone` a client sends.
6. Restrict `PUT` to admins using the request identity installed by T008 in `internal/api/auth.go`; `GET` is
   open to any authenticated caller.
7. Edit `internal/api/server.go` to register `get-schedule` and `put-schedule` on the existing Huma API.
8. Create `internal/api/settings_schedule_test.go` with `humatest`: write a grid containing all three values,
   read it back and assert the array is identical; assert 167 and 169 elements are `422`; assert a value of
   `3` and of `-1` are `422`; assert a rejected `PUT` left the stored grid unchanged; assert `timezone`
   is present and non-empty.
9. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] A grid containing `0`, `1` and `2` round-trips byte-identically through `PUT` then `GET`.
- [ ] `cells` of length 167 or 169 is `422` and nothing is written.
- [ ] A cell value outside `0..2` is `422` and nothing is written.
- [ ] `ReplaceSchedule` is transactional: a rejected write leaves all 168 stored rows unchanged.
- [ ] Both responses carry the active `timezone` and `active_mode`, and both are read-only.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/api/... ./internal/store/..." && echo SCHEDULE_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/api` and `ok  github.com/L-K-M/dl-tool/internal/store`,
with `TestScheduleRoundTrips`, `TestWrongLengthRejected`, `TestCellOutOfRangeRejected`,
`TestRejectedPutLeavesGridUnchanged`, `TestNonAdminPutForbidden` and `TestTimezoneReported` each reported as
`--- PASS`. The final line of stdout is exactly `SCHEDULE_OK`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT evaluate the grid or act on the active cell; T081 owns the per-minute evaluation.
- Do NOT implement the DST repeated-hour and skipped-hour rules; T110 owns them.
- Do NOT build the painting grid UI; [T118](T118-bandwidth-settings-and-schedule-grid.md) owns it, against
  doc 09 §9.1.
- Do NOT store the grid as a JSON blob in `settings`; it has its own 168-row table.
- Do NOT accept a `timezone` from the client; the container's `TZ` is the only source.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
