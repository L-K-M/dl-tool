# T091 — Back up the database on demand and prune on a schedule

| Field | Value |
|---|---|
| **ID** | T091 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T006, T012, T066 |
| **Blocks** | T092, T121 |
| **Parallel-safe** | no — it edits `internal/jobs/cron.go`, `internal/api/server.go` and `cmd/dl-tool/main.go` |
| **Implements** | [FR-142](../02-requirements.md#fr-142-produce-consistent-backups), [NFR-026](../02-requirements.md#nfr-026-store-data-durably-in-one-sqlite-database) |
| **Decisions** | [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md), [ADR-0015](../decisions/0015-db-backed-in-process-job-queue.md) |
| **Est. size** | 4 new files, ~480 LOC |

## Goal
`POST /api/v1/system/backup` writes a consistent snapshot with `VACUUM INTO` and returns its path and size.
The same statement runs nightly, keeping the newest seven files, alongside the retention deletes of doc 04
§7. A partial file is never left where a good backup should be.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/04-data-model.md` §6 Backup and restore](../04-data-model.md#6-backup-and-restore) — the statement, the must-not-exist rule, the rename recipe.
2. [`docs/04-data-model.md` §7 Retention](../04-data-model.md#7-retention) — the five windows and their jobs.
3. [`docs/05-api-contract.md` §13 System endpoints](../05-api-contract.md#13-system-endpoints) — the `POST /system/backup` request, response and status codes.
4. [`docs/17-operations-and-runbook.md` §3.2 The nightly job](../17-operations-and-runbook.md#32-the-nightly-job) — the two failure modes.
5. [`docs/14-conventions.md` §2.4 SQL and sqlx](../14-conventions.md#24-sql-and-sqlx).

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/store/maintenance.go` | create | `BackupsDirName`, `BackupInto`, `PruneBackups` and the five retention deletes. |
| `internal/store/maintenance_test.go` | create | The four store-level cases `## Verification` names. |
| `internal/api/system.go` | create | The `POST /system/backup` handler; later system routes join this file. |
| `internal/api/system_test.go` | create | Success, conflict and partial-file cases. |
| `internal/jobs/cron.go` | edit | Add `Scheduler.WithMaintenance`, the nightly backup and retention entries and the hourly search prune. |
| `internal/api/server.go` | edit | Register `create-backup`; build and export the one `MaintenanceStore`. |
| `cmd/dl-tool/main.go` | edit | Attach the maintenance store and the backup directory to the scheduler chain. |

No other file may be modified.

## Interface contract

```go
package store

import (
	"context"
	"time"
)

// BackupResult describes one completed snapshot.
type BackupResult struct {
	Path      string    `db:"-"`
	SizeBytes int64     `db:"-"`
	CreatedAt time.Time `db:"-"`
}

// BackupsDirName is the ConfigDir subdirectory every backup lives in — the one
// derivation site, so the store.Open join, the handler and the scheduler attach
// cannot spell the directory two ways.
const BackupsDirName = "backups"

// MaintenanceStore is the domain store for backup and retention — the
// TaskStore/SettingsStore shape, not a package-wide aggregate. The composition
// root shares one instance between NewSystemHandlers and the scheduler's
// WithMaintenance attach, so the ErrBackupRunning lock spans the cron entry
// and POST /system/backup.
func NewMaintenanceStore(db *sqlx.DB) *MaintenanceStore

// BackupInto writes a consistent snapshot into dir using SQLite's VACUUM INTO.
//
// It generates the name dl-tool.db.<UTC>.bak — where <UTC> is the existing backupTimestampFormat
// ("20060102T150405.000000000Z") so two runs in one second never collide — creates the unique
// temporary target inside dir with O_CREATE|O_EXCL and mode 0600 and closes it before running the
// statement (VACUUM INTO requires a missing or empty target), then integrity-checks the output,
// enforces 0600, fsyncs it, renames it into place and fsyncs the directory. An interrupted
// statement never produces a file that looks like a good backup. It returns ErrBackupRunning when
// another backup holds the in-process lock.
func (s *MaintenanceStore) BackupInto(ctx context.Context, dir string) (BackupResult, error)

// ErrBackupRunning maps to 409 /problems/conflict.
var ErrBackupRunning = errors.New("store: a backup is already running")

// PruneBackups deletes all but the newest keep files matching exactly dl-tool.db.<UTC>.bak in dir —
// the glob dl-tool.db.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9]*Z.bak, whose timestamp-shaped
// middle segment excludes the dl-tool.db.pre-migration-*.bak and dl-tool.db.replaced-*.bak families,
// which this job must never count or prune (docs/04-data-model.md §6).
func (s *MaintenanceStore) PruneBackups(ctx context.Context, dir string, keep int) (deleted int, err error)


// Retention windows from docs/04-data-model.md §7. now is injected so the tests are deterministic.
func (s *MaintenanceStore) PruneTaskEvents(ctx context.Context, now time.Time) (int64, error)   // at < now-90d
func (s *MaintenanceStore) PruneDoneJobs(ctx context.Context, now time.Time) (int64, error)     // state='done', older than 7d
func (s *MaintenanceStore) PruneSearchJobs(ctx context.Context, now time.Time) (int64, error)   // created_at < now-24h
```

```go
package api

// CreateBackupOutput is 201 on success. There is no request body.
type CreateBackupOutput struct {
	Status int `json:"-"`
	Body   struct {
		Path      string `json:"path"`
		SizeBytes int64  `json:"size_bytes"`
		CreatedAt string `json:"created_at"` // RFC 3339 UTC
	}
}

// NewSystemHandlers takes the one MaintenanceStore and the backup directory —
// filepath.Join(cfg.ConfigDir, store.BackupsDirName), resolved once.
func NewSystemHandlers(m *store.MaintenanceStore, backupDir string) *SystemHandlers

func (h *SystemHandlers) CreateBackup(ctx context.Context, in *struct{}) (*CreateBackupOutput, error)
```

Worked response, `201`:

```json
{"path":"/config/backups/dl-tool.db.20260901T094500.000000000Z.bak","size_bytes":4194304,"created_at":"2026-09-01T09:45:00Z"}
```

## Steps
1. Create `internal/store/maintenance.go` with `BackupInto`. Build the target name as
   `dl-tool.db.` + `time.Now().UTC().Format(backupTimestampFormat)` + `.bak` — the constant already
   exists in `internal/store/db.go` — and never reuse a name.
2. Create the temporary target inside `dir` with `os.CreateTemp(dir, "dl-tool.db.*.tmp")` — already
   `O_CREATE|O_EXCL` and mode `0600` —
   close it, then execute `VACUUM INTO ?` against it. Integrity-check the output, enforce `0600`,
   fsync it, `os.Rename` it to the final name, fsync the directory and `os.Stat` it for `SizeBytes`.
   On any error remove the temporary file before returning.
3. Guard the whole operation with a `sync.Mutex` held for its duration; a second concurrent call returns
   `ErrBackupRunning` immediately rather than blocking.
4. Implement `PruneBackups`: list the glob `dl-tool.db.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9]*Z.bak`
   in `dir` — timestamp-shaped, so the `pre-migration` and `replaced` families are excluded by
   construction — sort by name descending (the timestamp format sorts lexicographically) and remove
   everything past `keep`. Never touch a temporary `*.tmp` file younger than one hour.
5. Implement `PruneTaskEvents`, `PruneDoneJobs` and `PruneSearchJobs` with the exact windows and columns of
   doc 04 §7. `search_results` follows its `ON DELETE CASCADE`; write no separate delete for it.
6. Create `internal/api/system.go` with `SystemHandlers`, its constructor taking the
   `*store.MaintenanceStore` and the backup directory, and `CreateBackup` calling
   `BackupInto(ctx, backupDir)` then `PruneBackups(ctx, backupDir, 7)`.
7. Map `ErrBackupRunning` to `409` `/problems/conflict`
   and any other failure to `500` `/problems/internal`.
8. Edit `internal/jobs/cron.go` to add `Scheduler.WithMaintenance(m *store.MaintenanceStore, backupDir
   string)` — the `WithGovernor`/`WithWatcher` attach pattern — and, while a store is attached, three
   entries on T066's `Scheduler`: `0 3 * * *` running `BackupInto(backupDir)` then
   `PruneBackups(backupDir, 7)`, a `0 4 * * *` entry running the two retention prunes
   (`PruneTaskEvents`, `PruneDoneJobs`) — staggered an hour after the backup entry because
   robfig/cron dispatches each entry on its own goroutine and same-tick entries would race
   `VACUUM INTO` — and `30 * * * *` running `PruneSearchJobs`, offset off the top of the hour so it
   never shares a tick with the 03:00 or 04:00 entries. The call site is
   `cmd/dl-tool/main.go`'s scheduler chain —
   `NewScheduler(db, logger).WithGovernor(governor).WithWatcher(watcher).WithMaintenance(server.Maintenance, filepath.Join(cfg.ConfigDir, store.BackupsDirName))`
   — with `store.BackupsDirName` superseding the file-local `backupsDirName` const, the `store.Open`
   join included.
9. Edit `internal/api/server.go` to register the operation as `create-backup` on `POST /system/backup`,
   building the `*store.MaintenanceStore` in `NewServer`, handing it to `NewSystemHandlers` with
   `filepath.Join(cfg.ConfigDir, store.BackupsDirName)` and exporting it as `Server.Maintenance` — the
   `RuleCreator`/`WatchCreator` sharing rule of doc 14 §8.3 — so `cmd/dl-tool` attaches the same
   instance to the scheduler.
10. Create `internal/api/system_test.go`: a successful backup returns `201` and a file that opens and
    answers `PRAGMA integrity_check` with `ok`; a second concurrent call returns `409`; and a forced
    failure mid-statement leaves no file matching `dl-tool.db.*.bak`.

## Acceptance criteria
- [ ] The snapshot opens independently and `PRAGMA integrity_check` returns `ok`.
- [ ] Two backups started in the same second produce two different file names.
- [ ] A concurrent second call returns `409` `/problems/conflict` and writes no file.
- [ ] A failed statement leaves no file matching `dl-tool.db.*.bak` in the backup directory.
- [ ] Eight nightly runs leave exactly seven files, and `dl-tool.db.pre-migration-*.bak` and
  `dl-tool.db.replaced-*.bak` files in the same directory are never counted or pruned.
- [ ] `PruneTaskEvents` deletes rows older than 90 days and leaves a row exactly 89 days old.
- [ ] A produced backup is `0600`, and its temporary target was created with `O_CREATE|O_EXCL`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then `ok` lines for `github.com/L-K-M/dl-tool/internal/store`,
`.../internal/api` and `.../internal/jobs`, with `TestBackupIntoIsConsistent`,
`TestBackupNamesNeverCollide`, `TestConcurrentBackupIs409`, `TestFailedBackupLeavesNoFile`,
`TestPruneBackupsKeepsSeven` and `TestPruneTaskEventsRespectsWindow` all listed as passing. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement `GET /system/info`; T092 owns it in this same file.
- Do NOT implement `GET /system/logs`; T096 owns it.
- Do NOT implement `dl-tool restore --from`, `GET /settings/export` or `POST /settings/import`; T108 owns them.
- Do NOT copy `dl-tool.db` with the filesystem: `VACUUM INTO` is the only permitted mechanism.
- Do NOT prune `rule_matches`, `rule_seen_episodes`, `tasks` or any configuration table.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked — resolved

**Remedy 1 was applied.** The `## Files` table now lists `internal/store/maintenance_test.go` — the
home of the four store-level cases `## Verification` names — and `cmd/dl-tool/main.go`, the only
`NewScheduler` call site. `internal/jobs/cron.go` gains `Scheduler.WithMaintenance` beside
`WithGovernor`/`WithWatcher`, step 8 names the `cmd/dl-tool/main.go` call site that attaches the
store and `filepath.Join(cfg.ConfigDir, store.BackupsDirName)`, and step 9 has `NewServer` build and export
the one `*store.MaintenanceStore` as `Server.Maintenance`, so the `ErrBackupRunning` lock spans the
nightly entry and `POST /system/backup`. The contract receiver is `MaintenanceStore`, matching the
merged `TaskStore`/`SettingsStore` precedent. The original record is preserved below.

---

### The original 2026-09-23 record

The `dl-tool.db.<UTC>.bak` remedy below resolved the naming contradiction; the contract is now
self-consistent. Implementing it still cannot stay inside `## Files`: two files the work
requires are not listed.

### Gap 1 — `cmd/dl-tool/main.go`: the nightly entries cannot be armed

Step 8 puts three entries on T066's `Scheduler`: the `0 3 * * *` backup + `PruneBackups(7)` +
the two nightly prunes, and `@hourly` `PruneSearchJobs`. The nightly entry calls
`BackupInto(ctx, dir)` / `PruneBackups(ctx, dir, 7)` where `dir` is the backup directory —
`filepath.Join(cfg.ConfigDir, "backups")`, the same join `store.Open` receives in
`cmd/dl-tool/main.go` (the `backupsDirName` grep below) and step 6 hands the API handler
from `cfg.ConfigDir`.

`NewScheduler(db, log)` is called exactly once, in `cmd/dl-tool/main.go`'s OnStart, chained
with `WithGovernor`/`WithWatcher` — the attach pattern this task would extend. That file is
not in `## Files`, and every in-table route to the directory fails:

- `*sqlx.DB` exposes no DSN, and `filepath.Dir(cfg.DBPath)` is not the backup dir:
  `DLTOOL_DB_PATH` is validated independently of `DLTOOL_CONFIG_DIR`
  (`internal/config/config.go` — "the database may live elsewhere"), while backups are
  defined as `ConfigDir/backups` (doc 04 §6's `/config/backups/`).
- `store.Open`'s `backupDir` parameter is retained nowhere reachable; recording it would edit
  `internal/store/db.go` — also outside the table.
- No `settings` row carries a config path; the migrations seed only `max_active_total`,
  `max_active_per_engine` and `min_free_space`.
- Re-reading `DLTOOL_CONFIG_DIR` inside `internal/jobs` bypasses `config.Load`'s
  validation and normalisation and duplicates its default — a second parser for a setting
  the config package owns.
- Arming the entries with no directory — a `WithMaintenance` nothing calls — leaves the
  nightly job dead, the §8.3 "built and never wired" defect the acceptance criteria exist
  to forbid.
- The only in-table wiring — a second `Scheduler` owned by `api.NewServer` — is production
  architecture no document describes: it either double-registers `rss_poll` on a second cron
  runtime or needs a maintenance-only Start variant, and both exceed "add three entries to
  T066's `Scheduler`".

This is F087's defect class — an entry with no legal call site — and the documented remedy is
the same one T083's repaired table shows: a `cmd/dl-tool/main.go` row for the attach.

### Gap 2 — `internal/store/maintenance_test.go`: four Verification tests have no home

F607 already recorded this: Verification expects `TestBackupIntoIsConsistent`,
`TestBackupNamesNeverCollide`, `TestPruneBackupsKeepsSeven` and
`TestPruneTaskEventsRespectsWindow` under `github.com/L-K-M/dl-tool/internal/store`, but the
Files table's only test file is `internal/api/system_test.go`, whose stated purpose is the
step-10 API cases ("Success, conflict and partial-file cases"). The four store-level tests
would compile in package `api`, but housing them there widens the file beyond its listed
purpose — the narrower-scope recording the Files-table rule exists to prevent.

### Remedies — either unblocks the task

1. Add two rows to `## Files` — `internal/store/maintenance_test.go | create` for the four
   store-level cases, and `cmd/dl-tool/main.go | edit` to attach the maintenance store and
   `filepath.Join(cfg.ConfigDir, backupsDirName)` to the scheduler chain beside
   `WithGovernor`/`WithWatcher` — plus a step-8 clause naming that call site. The same
   `*store.MaintenanceStore` should go to the scheduler and `NewSystemHandlers` so the
   `ErrBackupRunning` lock spans the cron entry and `POST /system/backup`.
2. Or rule that the maintenance entries run on a dedicated `Scheduler` inside
   `internal/api/server.go` — a composition root doc 14 §8.3 names — and say so in step 8.
   The store test file is needed under either remedy.

Worth settling in the same repair: the contract's `(s *Store)` receiver. The aggregate
`store.Store` does not exist (F086/F601); the merged precedent substitutes the concrete
domain store — here `MaintenanceStore`, matching `TaskStore`/`SettingsStore` — which is what
an implementation will produce regardless. Naming it removes one more planned-vs-merged
drift.

Rerunnable evidence on this commit:

```bash
# The only Scheduler construction site — outside the Files table.
grep -rn "NewScheduler(" cmd/ internal/ --include="*.go" | grep -v _test

# The backup dir exists only as a ConfigDir join in the composition root.
grep -n "backupsDirName" cmd/dl-tool/main.go

# DBPath is independent of ConfigDir — Dir(db) is not the backup dir.
grep -n "envDBPath\|filepath.Dir(cfg.DBPath)" internal/config/config.go

# The Files table and the test file the Verification names but omits.
sed -n '/^## Files/,/^No other/p' docs/tasks/T091-database-backup-and-retention.md
```

### Deferral mechanics — same shape as the first record

- The picker takes the topmost eligible `todo` row and T091 heads the eligible set, so
  leaving it `todo` re-selects it on every iteration. The row is set to `deferred` — the
  status the picker skips — in this file's `**Status**` cell and both `00-task-index.md`
  rows (the T078/#250 precedent, and the first T091 deferral, #267). This chooses neither
  remedy; un-deferring flips the same three cells back to `todo`.
- The deferral stalls T092 — its `Depends on` names T091 — and through it T096, T117, T118,
  T119 and T121, exactly as the first record walked. Eligible with T091 parked: T108 (T080,
  T106, T107 all `done`), T120 (T053, T084, T106) and the M7 yt-dlp chain T087→T089→T090.
- Reactivation trigger: flip both index rows and the `**Status**` cell back to `todo` in the
  same change that lands the chosen remedy, so the deferral cannot outlive its cause.
- M6's exit checkpoint still cannot exit while this deferral stands; the gap stays visible
  through this record and the checkpoint.

## Blocked — resolved

**Remedy 1 was applied.** The file now uses the adjudicated `dl-tool.db.<UTC>.bak` form throughout:
the contract names `backupTimestampFormat` (`20060102T150405.000000000Z`, so the same-second
criterion holds), `PruneBackups` uses the timestamp-exact glob that excludes the `pre-migration` and
`replaced` families, the staging recipe follows doc 04 §6 (`O_CREATE|O_EXCL` temporary, integrity
check, fsync, rename, fsync dir), and the worked response and criteria name the `.bak` glob. The
original record is preserved below.

---

The backup file name is specified two different ways, and the choice decides the on-disk name, the
`PruneBackups` glob and the `path` the endpoint returns — a fact this task must act on, not a detail
it can pick. This file's interface contract, steps, worked response and acceptance criterion all say
`dl-tool-<UTC RFC3339 basic>.db`, pruned by the glob `dl-tool-*.db`. The documents that own the fact
say `dl-tool.db.<UTC>.bak`, and they record that the question was already adjudicated:

- [`docs/04-data-model.md` §6](../04-data-model.md#6-backup-and-restore) — the fact's home per
  [`docs/00-INDEX.md`](../00-INDEX.md) — shows
  `VACUUM INTO '/config/backups/dl-tool.db.20260901T120000Z.bak'` and scopes nightly retention to
  "the newest 7 files matching exactly `dl-tool.db.<UTC>.bak`".
- [`docs/05-api-contract.md` §13](../05-api-contract.md#13-system-endpoints) — the response shape's
  home — returns `.../dl-tool.db.20260901T094500Z.bak`, and its change log records the fix: "the
  `POST /system/backup` example path now uses the `dl-tool.db.<UTC>.bak` form owned by
  `04-data-model.md` §6".
- [`docs/17-operations-and-runbook.md`](../17-operations-and-runbook.md) §3.2 and §3.4 use the `.bak`
  form throughout, and its change log states the backup-naming open question was resolved to that
  form. Existing code agrees: `internal/store/db.go` writes
  `dl-tool.db.pre-migration-<from>-to-<to>.<UTC>.bak`.

So the question was asked and answered once already; the `.db` form here is the stale half of a
resolved contradiction. Implementing it anyway overrides doc 04's retention pattern and doc 05's
response contract and reverses that recorded resolution; implementing `.bak` instead overrides this
file's interface contract, step 1, step 4, the worked response and the `dl-tool-*.db` criterion.
Either diff contradicts a written requirement — the stop condition, not a judgment call.

The name is not cosmetic. Under `dl-tool.db.*.bak` a naive glob also matches
`dl-tool.db.pre-migration-*.bak` and the restore flow's `dl-tool.db.replaced-<UTC>.bak`, which doc 04
§6 says this job must never count or prune — "matching exactly `dl-tool.db.<UTC>.bak`" has to mean a
timestamp-shaped middle segment. Under `dl-tool-*.db` the exclusion is structural instead, which may
be why this file's form exists; but the docs were never re-adjudicated to it, and T108's restore and
doc 17 §3.4 still speak `.bak`.

A second, smaller tension the repair should settle at the same time: step 1's `20060102T150405Z` is
second-precision, so two backups in the same second collide on `O_EXCL`, yet the second acceptance
criterion requires distinct names. The existing `backupTimestampFormat` in `internal/store/db.go`
(`20060102T150405.000000000Z`) already solves this and matches the pre-migration family.

Remedies — either unblocks the task:

1. Restate this file in the adjudicated `dl-tool.db.<UTC>.bak` form: the contract comment, the prune
   glob (made timestamp-exact so `pre-migration` and `replaced` infixes stay excluded, as §6
   requires), step 1, step 4, the worked response and the criterion's glob; and give step 1
   sub-second precision (or name `backupTimestampFormat`) so criterion 2 can hold. Pin the glob to
   accept that format's fractional-second suffix while still excluding the infixes — e.g.
   `dl-tool.db.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9]*Z.bak`, which matches both
   `dl-tool.db.20260901T120000Z.bak` and `dl-tool.db.20260901T120000.000000000Z.bak` but neither
   `dl-tool.db.pre-migration-*.bak` nor `dl-tool.db.replaced-*.bak`.
2. Or rule that `dl-tool-<UTC>.db` is the intended new scheme and re-adjudicate the docs: doc 04 §6,
   doc 05 §13, doc 17 §3.2 and §3.4 and their change logs — noting the pre-migration and
   replaced-database families stay `.bak`, which `dl-tool-*.db` then excludes by construction.

The file that should answer it: this task file, after the owner picks 1 or 2.

Rerunnable evidence on this commit:

```bash
# The docs' adjudicated form and the recorded resolution.
git grep -n -E "dl-tool\.db" HEAD -- docs/04-data-model.md docs/05-api-contract.md \
  docs/17-operations-and-runbook.md
git grep -n -E "backup-naming|dl-tool\.db\.<UTC>\.bak" HEAD -- docs/05-api-contract.md \
  docs/17-operations-and-runbook.md

# This file's conflicting form.
grep -n "dl-tool-" docs/tasks/T091-database-backup-and-retention.md

# The existing backup family in code.
grep -n -E "backupTimestampFormat|pre-migration" internal/store/db.go
```

### 2026-09-23 — re-verified on eee12ef, row set to `deferred`

The blocker is unchanged on eee12ef, the head of `main` (the merge of #266). Re-running
the evidence block above prints the same three facts: doc 04 §6, doc 05 §13 and doc 17
§3.2/§3.4 name `dl-tool.db.<UTC>.bak` and record the 2026-09-01 resolution in their change
logs; this file's contract, steps, worked response and criterion still name
`dl-tool-<UTC RFC3339 basic>.db` / `dl-tool-*.db`; `internal/store/db.go` still writes the
`.bak` pre-migration family through `backupTimestampFormat`. Neither remedy above has been
picked.

Additions to the record:

- The picker takes the topmost eligible `todo` row, and T091 heads the eligible set, so
  leaving it `todo` re-selects it on every iteration and nothing below it can start. The
  row is set to `deferred`, the status the picker skips (the T078 precedent, #250), so the
  queue can proceed to T106. This chooses neither remedy: the owner still decides, and
  un-deferring is a one-word flip back to `todo`. FR-142 stays a `must`; the deferral parks
  the task rather than waiving the requirement.
- Unlike T078, this deferral stalls downstream work: T092's `Depends on` names T091, and a
  `deferred` entry is not `done`, so T092 — and through it T096, T117, T118, T119 and T121
  — stay unreachable until the row flips back. T106, T107, T110 and T111 are eligible now;
  T108 and T120 follow once T106/T107 land; the M7 rows that do not pass through T091/T092
  are unaffected.
- Reactivation trigger: flip both T091 rows in `00-task-index.md` — the `## M6`
  milestone-table row and the `## Roster` detail-table row — and this file's `**Status**`
  row back to `todo` in the same change that lands the chosen remedy (remedy 1 rewrites
  this file; remedy 2 re-adjudicates docs 04 §6, 05 §13 and 17 §3.2/§3.4), so the deferral
  cannot outlive its cause.
- M6's exit checkpoint names the settings sections, and T117–T119 need T092, which needs
  this task — so the milestone cannot exit while this deferral stands. The gap stays
  visible through this record and the checkpoint.
