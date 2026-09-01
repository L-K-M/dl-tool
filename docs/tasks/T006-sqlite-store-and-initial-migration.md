# T006 — Open the SQLite store, generate ULIDs and apply the initial migration

| Field | Value |
|---|---|
| **ID** | T006 |
| **Milestone** | M0 |
| **Status** | todo |
| **Depends on** | T004, T005 |
| **Blocks** | T007, T008, T010, T012 |
| **Parallel-safe** | yes — touches only `internal/store/` |
| **Implements** | [NFR-026](../02-requirements.md#nfr-026-store-data-durably-in-one-sqlite-database) |
| **Decisions** | [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md), [ADR-0015](../decisions/0015-db-backed-in-process-job-queue.md) |
| **Est. size** | 3 new source files, 1 test file, ~600 LOC — of which ~330 is DDL transcribed verbatim from doc 04 §3. Doc 04 requires the whole schema to be one migration, so this cannot be split. |

## Goal
`store.Open` returns a `*sqlx.DB` in WAL mode with the four documented pragmas, refuses a database directory
on a network filesystem, takes a pre-migration backup, applies `00001_init.sql` through goose, and refuses to
start when the applied schema version is newer than the binary. `store.NewID` mints prefixed ULIDs.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/04-data-model.md` §1.1 DSN](../04-data-model.md#11-dsn-exact-string),
   [§1.2 pragmas](../04-data-model.md#12-the-four-pragmas),
   [§1.3 connection pool](../04-data-model.md#13-connection-pool).
2. [`docs/04-data-model.md` §3 Schema DDL](../04-data-model.md#3-schema-ddl) — every `CREATE TABLE`, in the
   given order, plus the seeded `settings` rows and the 168 `bandwidth_schedule` cells.
3. [`docs/04-data-model.md` §1.5 ID prefix allocation](../04-data-model.md#15-id-prefix-allocation).
4. [`docs/04-data-model.md` §5 Schema migration policy](../04-data-model.md#5-schema-migration-policy) and
   [§6 Backup and restore](../04-data-model.md#6-backup-and-restore).
5. [`docs/14-conventions.md` §2.4 SQL and sqlx](../14-conventions.md#24-sql-and-sqlx) and
   [§8.3 Add a migration](../14-conventions.md#83-add-a-migration).

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/store/db.go` | create | `Open`, DSN assembly, pragmas, pool, network-FS refusal, goose runner, version gate, pre-migration backup. |
| `internal/store/models.go` | create | ID prefix constants and `NewID`. |
| `internal/store/migrations/00001_init.sql` | create | The entire schema of doc 04 §3, `Up` and `Down`. |
| `internal/store/db_test.go` | create | Pragma, migration, version-gate, network-FS and ULID tests. |

No other file may be modified.

## Interface contract

```go
package store

//go:embed migrations/*.sql
var embedMigrations embed.FS

// ErrNotFound is returned when no row with the given id exists.
var ErrNotFound = errors.New("store: not found")

// Open builds the DSN from dbPath, applies the connection pool limits, backs the
// database up, migrates it and returns a ready handle. It refuses to run when the
// directory holding dbPath is on nfs, cifs, smb3 or a fuse filesystem, and when the
// applied schema version is newer than the newest embedded migration.
func Open(ctx context.Context, dbPath, backupDir string) (*sqlx.DB, error)

// SchemaVersion returns MAX(version_id) from goose's version table.
func SchemaVersion(ctx context.Context, db *sqlx.DB) (int64, error)
```

DSN, assembled from `dbPath` and otherwise byte-identical to doc 04 §1.1:

```
file:<dbPath>?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_txlock=immediate
```

```go
package store

// ID prefixes, one per table, from 04-data-model.md section 1.5.
const (
	PrefixUser = "usr_"; PrefixSession = "ses_"; PrefixAPIToken = "tok_"
	PrefixSetting = "set_"; PrefixEngine = "eng_"; PrefixCategory = "cat_"
	PrefixTag = "tag_"; PrefixTaskTracker = "ttr_"; PrefixTaskEvent = "evt_"
	PrefixIndexer = "idx_"; PrefixSearchJob = "sch_"; PrefixSearchResult = "res_"
	PrefixFeed = "fed_"; PrefixFeedItem = "itm_"; PrefixTask = "tsk_"
	PrefixNotificationChannel = "ntf_"; PrefixRule = "rul_"; PrefixRuleMatch = "mat_"
	PrefixJob = "job_"; PrefixBandwidthCell = "bws_"; PrefixUIPref = "uip_"
	PrefixWatchFolder = "wfd_"; PrefixTaskFile = "tfi_"
)

// NewID returns prefix followed by a 26-character Crockford base32 ULID,
// for example "tsk_01J9Z3K7QF8N4V2XW6P0RSTBCD".
func NewID(prefix string) string
```

`internal/store/migrations/00001_init.sql` frame:

```sql
-- +goose Up
-- (every CREATE TABLE, CREATE INDEX and seed INSERT of 04-data-model.md section 3, in that order)

-- +goose Down
-- (DROP TABLE for every table created above, in reverse order)
```

## Steps
1. Create `internal/store/models.go` with the prefix constants and `NewID`, using
   `github.com/oklog/ulid/v2` with `crypto/rand` as the entropy source. Do not add row structs yet.
2. Create `internal/store/migrations/00001_init.sql`. Transcribe doc 04 §3.1 through §3.6 in order, verbatim,
   including every `CHECK`, every index and the `priority IN (0,1,6,7)` constraint on `task_files`.
3. In the same file seed the four concurrency and reservation `settings` rows of doc 04 §3.2, and the 168
   `bandwidth_schedule` cells with `mode = 'default'`, using a recursive CTE over day 0–6 and hour 0–23.
4. Write the `-- +goose Down` section dropping every table created above, in reverse order.
5. Create `internal/store/db.go`. Register `modernc.org/sqlite` under the driver name `"sqlite"`, build the
   DSN exactly as above, and apply `SetMaxOpenConns(1)`, `SetMaxIdleConns(1)`, `SetConnMaxLifetime(0)`.
6. Before opening, read the filesystem type of `filepath.Dir(dbPath)` from `/proc/self/mountinfo`; if it is
   `nfs`, `cifs`, `smb3` or matches `fuse.*`, return an error naming the path and the type. There is no
   degraded fallback.
7. Run `VACUUM INTO '<backupDir>/dl-tool.db.pre-migration-<version>.bak.tmp'` and rename it into place, then
   abort the migration if the backup failed. Skip the backup when the database file does not yet exist.
8. Call `goose.SetBaseFS(embedMigrations)`, `goose.SetDialect("sqlite3")`, then compare `SchemaVersion` with
   the newest embedded migration; when the applied version is higher, return an error naming both numbers.
   Otherwise `goose.Up(db, "migrations")`, then run `PRAGMA integrity_check`.
9. Write `internal/store/db_test.go` covering: `journal_mode` reads back `wal` and `foreign_keys` reads back
   `1`; a fresh file migrates to version 1 and `bandwidth_schedule` holds 168 rows; `Down` then `Up` is
   clean; a database stamped with version 999 makes `Open` fail with a message naming both versions; a
   backup file exists after migrating an existing database; `NewID(PrefixTask)` returns 30 characters with
   the `tsk_` prefix and two calls never collide.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] Every table, index, `CHECK` and seed row of doc 04 §3 is present in `00001_init.sql`, none renamed.
- [ ] `TestPragmas` asserts `journal_mode=wal`, `busy_timeout=5000`, `synchronous=1` and `foreign_keys=1`.
- [ ] `TestSchemaNewerThanBinaryRefuses` asserts `Open` returns an error naming the applied and the embedded
      version.
- [ ] `TestNetworkFilesystemRefused` asserts the refusal path is reached for a `nfs` mount entry.
- [ ] `TestPreMigrationBackup` asserts the `.bak` file exists and no `.bak.tmp` file remains.
- [ ] `TestNewID` asserts the 26-character ULID body and 10 000 collision-free identifiers.
- [ ] No `SELECT *` appears anywhere in `internal/store`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make test PKG=./internal/store/... && echo STORE_OK
```
Expected: `ok  	github.com/L-K-M/dl-tool/internal/store` on its own line, no `FAIL`, and a final line of
exactly `STORE_OK`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT write row structs or query functions for `tasks`, `feeds`, `rules`, `indexers`, `events` or
  `settings`; T017, T065, T069, T054 and T092 own those files.
- Do NOT write `internal/store/users.go`; T008 owns it.
- Do NOT write `internal/store/jobs.go`; T012 owns it.
- Do NOT add a second migration file. Doc 04 §3 puts the whole v1 schema in `00001_init.sql`.
- Do NOT add any column or setting describing what to do with a transfer dl-tool did not create; there is
  exactly one rule and no option ([ADR-0017](../decisions/0017-exclusive-control-of-engines.md)).
- Do NOT implement `POST /system/backup` or the nightly backup cron; T091 owns them.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
