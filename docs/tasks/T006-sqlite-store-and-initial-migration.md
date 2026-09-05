# T006 — Open the SQLite store, generate ULIDs and apply the initial migration

| Field | Value |
|---|---|
| **ID** | T006 |
| **Milestone** | M0 |
| **Status** | done |
| **Depends on** | T004, T005 |
| **Blocks** | T007, T008, T010, T012, T017, T055, T065, T091 |
| **Parallel-safe** | yes — touches only `internal/store/` |
| **Implements** | [FR-142](../02-requirements.md#fr-142-produce-consistent-backups), [NFR-026](../02-requirements.md#nfr-026-store-data-durably-in-one-sqlite-database) |
| **Decisions** | [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md), [ADR-0015](../decisions/0015-db-backed-in-process-job-queue.md) |
| **Est. size** | 3 new source files, 1 test file, ~600 LOC — of which ~330 is DDL transcribed verbatim from doc 04 §3. Doc 04 requires the whole schema to be one migration, so this cannot be split. |

## Goal
`store.Open` returns a `*sqlx.DB` in WAL mode with the four documented pragmas, refuses a database directory
on a network filesystem, backs up an older nonzero schema before upgrading it, applies `00001_init.sql`
through goose, and refuses to start when the applied schema version is newer than the binary. `store.NewID`
mints prefixed ULIDs.

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
   [§8.4 Add a migration](../14-conventions.md#84-add-a-migration).
6. [`docs/11-config-reference.md` §8 Boot validation](../11-config-reference.md#8-boot-validation).

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

// Open builds the DSN from dbPath, applies the connection pool limits, conditionally
// backs up an older nonzero schema, migrates it and returns a ready handle. It refuses
// to run when the directory holding dbPath is on nfs, nfs4, cifs, smb3 or a fuse
// filesystem, and when the applied schema version is newer than the newest embedded migration.
func Open(ctx context.Context, dbPath, backupDir string) (*sqlx.DB, error)

// SchemaVersion returns MAX(version_id) among goose rows where is_applied=1. A
// missing version table or NULL maximum is version 0 only when no schema object except
// goose_db_version or a sqlite_ internal object exists; every other schema or query error is returned.
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
3. In the same file transcribe the `settings` seed rows and the recursive-CTE insert for all 168
   `bandwidth_schedule` cells from doc 04 §3.2 and §3.6 verbatim.
4. Write the `-- +goose Down` section dropping every table created above, in reverse order.
5. Create `internal/store/db.go`. Register `modernc.org/sqlite` under the driver name `"sqlite"`, build the
   DSN exactly as above, and apply `SetMaxOpenConns(1)`, `SetMaxIdleConns(1)`, `SetConnMaxLifetime(0)`.
6. Before opening, read the filesystem type of `filepath.Dir(dbPath)` from `/proc/self/mountinfo`; if it is
   `nfs`, `nfs4`, `cifs` or `smb3`, or begins with `fuse.`, return an error naming the path and the type. There is no
   degraded fallback.
7. Call `goose.SetBaseFS(embedMigrations)` and `goose.SetDialect("sqlite3")`, then read the applied and
   highest embedded versions before backup or migration. Per doc 04 §5, a missing version table or `NULL`
   applied maximum is version `0` only when no schema object except `goose_db_version` or a `sqlite_`
   internal object exists; refuse every other unrecognised schema and propagate every other query error.
   The refusal names the offending objects and says to move the foreign file aside. Refuse an applied
   version above the embedded version. Skip backup for version `0` and when the versions match. Only when
   `0 < applied < embedded`,
   take the crash-safe `VACUUM INTO` backup specified by doc 04 §5–6, binding the temporary path as a SQL
   parameter, then abort before goose if any backup step fails. Log the final path after its rename and
   directory fsync succeed. Put this operation in a private helper whose inputs include the from-version,
   to-version and UTC start time, so the test injects all three.
8. Run `goose.Up(db, "migrations")` when migration is needed. Run `PRAGMA integrity_check` after that step
   on every open, including when goose had nothing to apply. Read every result row and pass them to a private
   validator taking `dbPath`; return an `integrity_check_failed` error naming that path unless the result is
   exactly one row equal to `ok`.
9. Write `internal/store/db_test.go` covering: `journal_mode` reads back `wal` and `foreign_keys` reads back
   `1`; a fresh file migrates to version 1 and `bandwidth_schedule` holds 168 rows whose IDs combine the
   documented prefix with a 26-character ULID body and are pairwise distinct, as are the seeded `settings`
   IDs; `Down` then `Up` is clean; a
   database stamped with version 999 makes `Open` fail with a message naming both versions; databases with
   an unrelated table or a standalone view and no goose history are refused unchanged with errors naming
   those objects; neither a
   nonexistent database nor an existing empty version-0 database produces a backup; the private backup
   operation exercised directly for a synthetic version 1-to-2 transition at the injected UTC time
   `2026-09-01T12:00:00.123456789Z` produces the documented final file and no temporary file; and
   `NewID(PrefixTask)` returns 30 characters with the `tsk_` prefix and two calls never collide.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [x] Every table, index, `CHECK` and seed row of doc 04 §3 is present in `00001_init.sql`, none renamed.
- [x] `TestPragmas` asserts `journal_mode=wal`, `busy_timeout=5000`, `synchronous=1` and `foreign_keys=1`.
- [x] `TestSchemaNewerThanBinaryRefuses` asserts `Open` returns an error naming the applied and the embedded
      version.
- [x] `TestNetworkFilesystemRefused` asserts the refusal path is reached for a `nfs` mount entry.
- [x] `TestUnrecognisedDatabaseRefused` asserts databases containing an unrelated table or a standalone
      view and no goose history are refused without changing that object, and each error names it.
- [x] `TestZeroVersionSkipsBackup` asserts a nonexistent database, an empty file and a database containing
      only goose's initial applied version-0 row each leave no backup artifact.
- [x] `TestIntegrityCheckResult` asserts the private validator accepts one `ok` row and rejects zero rows,
      multiple rows or any non-`ok` row with an `integrity_check_failed` error naming the database path.
- [x] `TestPreMigrationBackup` exercises the private backup operation with from-version 1, to-version 2 and
      injected time `2026-09-01T12:00:00.123456789Z`; it asserts
      `dl-tool.db.pre-migration-1-to-2.20260901T120000.123456789Z.bak` exists and no temporary file remains.
      It repeats with `2026-09-01T12:00:00Z` and expects
      `dl-tool.db.pre-migration-1-to-2.20260901T120000.000000000Z.bak`, proving fixed-width padding. It must
      not add an artificial embedded migration. Captured logs name the exact final path after success.
- [x] `TestInitialSeedIDs` asserts every seeded ID has its documented prefix, a 26-character ULID body and
      no duplicate within its table; the seeded settings rows are exactly the keys and values documented in
      doc 04 §3.2.
- [x] `TestNewID` asserts the 26-character ULID body and 10 000 collision-free identifiers.
- [x] No `SELECT *` appears anywhere in `internal/store`.
- [x] The configuration directory is `0700` and `dl-tool.db` is `0600` after `Open`, with `UMASK=002` set.
- [x] The `-wal` and `-shm` sidecars are `0600` after a write transaction, inheriting the database's mode.
- [x] A symlink or non-regular file at `DLTOOL_DB_PATH` is a fatal boot error, not a silent follow.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make test PKG=./internal/store/... && echo STORE_OK
```
Expected: `ok  	github.com/L-K-M/dl-tool/internal/store` on its own line, no `FAIL`, and a final line of
exactly `STORE_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

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
```text
go test -race -count=1 ./internal/store/...
ok  	github.com/L-K-M/dl-tool/internal/store	7.057s
STORE_OK
```

```text
internal/store/db.go
internal/store/db_test.go
internal/store/migrations/00001_init.sql
internal/store/models.go
```

Audit regressions cover literal database/backup filenames containing URI punctuation and
rejection of corrupt backups at those paths.

```text
$ make test PKG=./internal/store/... && echo STORE_OK
go test -race -count=1 ./internal/store/...
ok  	github.com/L-K-M/dl-tool/internal/store	62.935s
STORE_OK
```

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
