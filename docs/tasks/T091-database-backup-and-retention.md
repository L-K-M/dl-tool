# T091 — Back up the database on demand and prune on a schedule

| Field | Value |
|---|---|
| **ID** | T091 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T006, T012, T066 |
| **Blocks** | T092, T121 |
| **Parallel-safe** | no — it edits `internal/jobs/cron.go` and `internal/api/server.go` |
| **Implements** | [FR-142](../02-requirements.md#fr-142-produce-consistent-backups), [NFR-026](../02-requirements.md#nfr-026-store-data-durably-in-one-sqlite-database) |
| **Decisions** | [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md), [ADR-0015](../decisions/0015-db-backed-in-process-job-queue.md) |
| **Est. size** | 3 new files, ~380 LOC |

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
| `internal/store/maintenance.go` | create | `BackupInto`, `PruneBackups` and the five retention deletes. |
| `internal/api/system.go` | create | The `POST /system/backup` handler; later system routes join this file. |
| `internal/api/system_test.go` | create | Success, conflict, forbidden and partial-file cases. |
| `internal/jobs/cron.go` | edit | Add the nightly backup and retention entries and the hourly search prune. |
| `internal/api/server.go` | edit | Register `create-backup`. |

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

// BackupInto writes a consistent snapshot into dir using SQLite's VACUUM INTO.
//
// It generates the name dl-tool-<UTC RFC3339 basic>.db, writes to "<name>.partial" inside dir so an
// interrupted statement never produces a file that looks like a good backup, then renames it into
// place. It returns ErrBackupRunning when another backup holds the in-process lock.
func (s *Store) BackupInto(ctx context.Context, dir string) (BackupResult, error)

// ErrBackupRunning maps to 409 /problems/conflict.
var ErrBackupRunning = errors.New("store: a backup is already running")

// PruneBackups deletes all but the newest keep files matching dl-tool-*.db in dir.
func (s *Store) PruneBackups(ctx context.Context, dir string, keep int) (deleted int, err error)

// Retention windows from docs/04-data-model.md §7. now is injected so the tests are deterministic.
func (s *Store) PruneTaskEvents(ctx context.Context, now time.Time) (int64, error)   // at < now-90d
func (s *Store) PruneDoneJobs(ctx context.Context, now time.Time) (int64, error)     // state='done', older than 7d
func (s *Store) PruneSearchJobs(ctx context.Context, now time.Time) (int64, error)   // created_at < now-24h
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

func (h *SystemHandlers) CreateBackup(ctx context.Context, in *struct{}) (*CreateBackupOutput, error)
```

Worked response, admin only, `201`:

```json
{"path":"/config/backups/dl-tool-20260901T094500Z.db","size_bytes":4194304,"created_at":"2026-09-01T09:45:00Z"}
```

## Steps
1. Create `internal/store/maintenance.go` with `BackupInto`. Build the target name from
   `time.Now().UTC().Format("20060102T150405Z")` and never reuse a name.
2. Execute `VACUUM INTO ?` against the `.partial` path, then `os.Rename` it to the final name and `os.Stat`
   it for `SizeBytes`. On any error remove the `.partial` file before returning.
3. Guard the whole operation with a `sync.Mutex` held for its duration; a second concurrent call returns
   `ErrBackupRunning` immediately rather than blocking.
4. Implement `PruneBackups`: list `dl-tool-*.db` in `dir`, sort by name descending — the timestamp format
   sorts lexicographically — and remove everything past `keep`. Never touch a `.partial` file younger than
   one hour.
5. Implement `PruneTaskEvents`, `PruneDoneJobs` and `PruneSearchJobs` with the exact windows and columns of
   doc 04 §7. `search_results` follows its `ON DELETE CASCADE`; write no separate delete for it.
6. Create `internal/api/system.go` with `SystemHandlers`, its constructor taking the store and the config
   directory, and `CreateBackup` calling `BackupInto(ctx, cfg.ConfigDir+"/backups")` then `PruneBackups(…, 7)`.
7. Map `ErrBackupRunning` to `409` `/problems/conflict`, a non-admin caller to `403` `/problems/forbidden`
   and any other failure to `500` `/problems/internal`.
8. Edit `internal/jobs/cron.go` to add three entries to T066's `Scheduler`: `0 3 * * *` running the backup
   then `PruneBackups(7)` then the two nightly prunes, and `@hourly` running `PruneSearchJobs`.
9. Edit `internal/api/server.go` to register the operation as `create-backup` on `POST /system/backup`.
10. Create `internal/api/system_test.go`: a successful backup returns `201` and a file that opens and
    answers `PRAGMA integrity_check` with `ok`; a second concurrent call returns `409`; a non-admin gets
    `403`; and a forced failure mid-statement leaves no file matching `dl-tool-*.db`.

## Acceptance criteria
- [ ] The snapshot opens independently and `PRAGMA integrity_check` returns `ok`.
- [ ] Two backups started in the same second produce two different file names.
- [ ] A concurrent second call returns `409` `/problems/conflict` and writes no file.
- [ ] A failed statement leaves no file matching `dl-tool-*.db` in the backup directory.
- [ ] Eight nightly runs leave exactly seven files.
- [ ] `PruneTaskEvents` deletes rows older than 90 days and leaves a row exactly 89 days old.
- [ ] A non-admin caller receives `403` `/problems/forbidden`.

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

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
