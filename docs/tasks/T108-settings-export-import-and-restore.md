# T108 — Export and import portable settings, and restore from the CLI

| Field | Value |
|---|---|
| **ID** | T108 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T080, T086, T106, T107 |
| **Blocks** | T121 |
| **Parallel-safe** | no — it also edits the shared files `cmd/dl-tool/main.go`, `internal/api/server.go`, `internal/store/db.go` |
| **Implements** | [FR-145](../02-requirements.md#fr-145-export-and-import-portable-settings), [FR-146](../02-requirements.md#fr-146-restore-a-backup-from-the-command-line) |
| **Decisions** | [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md), [ADR-0015](../decisions/0015-db-backed-in-process-job-queue.md) |
| **Est. size** | 2 new files, ~390 LOC |

## Goal
`GET /settings/export` produces a versioned document of the seven collections with no secret in it;
`POST /settings/import` applies one, dry-run first and transactionally. `dl-tool restore --from <file>`
replaces the database in place, refusing while a server holds it and refusing a foreign schema version.
This is dl-tool's own backup and restore — there is no import from any other product.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §11.5 `GET /settings/export` and `POST /settings/import`](../05-api-contract.md#115-get-settingsexport-and-post-settingsimport)
2. [`docs/17-operations-and-runbook.md` §3 Backup and restore](../17-operations-and-runbook.md#3-backup-and-restore)
3. [`docs/04-data-model.md` §6 Backup and restore](../04-data-model.md#6-backup-and-restore)
4. [`docs/11-config-reference.md` §5 Database-backed settings](../11-config-reference.md#5-database-backed-settings)
5. [`docs/02-requirements.md` FR-145](../02-requirements.md#fr-145-export-and-import-portable-settings)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/settings_export.go` | create | The export builder, the import applier and the dry-run report. |
| `internal/api/settings_export_test.go` | create | Exclusion, round-trip, dry-run, conflict and version cases. |
| `internal/store/db.go` | modify | Add `RestoreFrom` with the lock, schema and integrity gates. |
| `cmd/dl-tool/main.go` | modify | Add the `restore --from <file>` subcommand. |
| `internal/api/server.go` | modify | Register `export-settings` and `import-settings`. |

No other file may be modified.

## Interface contract

```go
package api

// ExportDocument is the portable settings document of doc 05 §11.5. Exactly these members, in this
// order, and no others.
type ExportDocument struct {
	DocumentVersion int               `json:"document_version"` // currently 1
	ExportedAt      time.Time         `json:"exported_at"`
	SchemaVersion   int64             `json:"schema_version"`
	Settings        map[string]any    `json:"settings"`      // secrets OMITTED, not "__redacted__"
	Categories      []ExportCategory  `json:"categories"`    // name, save_path
	Indexers        []ExportIndexer   `json:"indexers"`      // never api_key
	Feeds           []ExportFeed      `json:"feeds"`
	Rules           []ExportRule      `json:"rules"`
	WatchFolders    []ExportWatch     `json:"watch_folders"` // owner as a username
	Schedule        ExportSchedule    `json:"schedule"`      // enabled + the 168 cells
}

// Excluded from every export, with no option to include them: sessions, users and therefore every
// password_hash, api_tokens, notification_channels.secret_enc, engines.secret_enc,
// indexers.api_key, tasks and every task-derived table. An export is safe to attach to a bug report.

type ImportInput struct {
	Body struct {
		Document   ExportDocument `json:"document" required:"true"`
		DryRun     *bool          `json:"dry_run,omitempty"`     // default true
		OnConflict string         `json:"on_conflict,omitempty"` // "skip" (default) | "overwrite"
	}
}

// ImportReport is what both a dry run and a committing call return. A dry run is side-effect free
// and returns exactly the report a committing call would produce.
type ImportReport struct {
	DryRun          bool                    `json:"dry_run"`
	DocumentVersion int                     `json:"document_version"`
	Totals          Counts                  `json:"totals"`
	Collections     map[string]Counts       `json:"collections"`
	Rejected        []RejectedRow           `json:"rejected"`
}

type Counts struct{ Created, Updated, Skipped, Rejected int }

type RejectedRow struct {
	Collection string `json:"collection"`
	Key        string `json:"key"`
	Type       string `json:"type"`   // an RFC 9457 problem type, e.g. "/problems/path-rejected"
	Detail     string `json:"detail"`
}
```

Conflict matching is by `categories.name`, `indexers.definition_id` (else `name`), `feeds.url`,
`rules.name` and `watch_folders.path`. A committing import is one transaction: either every accepted row
lands or none does. Paths are re-validated against the importing host's roots and the caller's jail; a
failing path is `rejected`, never silently rewritten.

```go
package store

// RestoreFrom replaces the live database with the backup at src. src must resolve inside
// DLTOOL_CONFIG_DIR. The three gates run in this order and each is a named refusal, exit code 1:
//
//	restore_server_running  — an exclusive lock on the target database is already held
//	restore_schema_mismatch — MAX(version_id) in the backup differs from the highest embedded migration
//	restore_integrity_failed — PRAGMA integrity_check on the backup, opened read-only, is not "ok"
//
// On success it renames the current database to dl-tool.db.replaced-<UTC>.bak, copies the backup
// into place with mode 0600, removes any stale -wal and -shm, and returns the restored task count.
func RestoreFrom(ctx context.Context, dbPath, configDir, src string) (tasks int, err error)

var (
	ErrRestoreServerRunning  = errors.New("store: restore_server_running")
	ErrRestoreSchemaMismatch = errors.New("store: restore_schema_mismatch")
	ErrRestoreIntegrity      = errors.New("store: restore_integrity_failed")
)
```

## Steps
1. Create `internal/api/settings_export.go` with `ExportDocument` and the builder, reading the seven
   collections and omitting every excluded field at the query level, not by post-filtering.
2. Emit `schema_version` from the goose migration version and `document_version` as `1`.
3. Implement the importer: validate `document_version`, returning `409` `/problems/conflict` when it is
   newer than the binary understands; build the report; and write nothing when `dry_run` is true.
4. Apply a committing import inside one `sqlx.Tx`, honouring `on_conflict` per the matching keys above.
5. Re-validate every path against the importing host's roots and the caller's jail, recording a failure in
   `rejected[]` with `/problems/path-rejected`.
6. Add `RestoreFrom` and the three sentinels to `internal/store/db.go`, running the gates in the documented
   order and touching the live database only after all three pass.
7. Add the `restore --from <file>` subcommand to `cmd/dl-tool/main.go` through the existing `humacli`
   wiring, printing the named refusal and exiting `1` on any gate, and printing the restored task count on
   success. Accept only a file inside `DLTOOL_CONFIG_DIR`.
8. Create `internal/api/settings_export_test.go`: export from a populated instance and grep the document for
   a session id, a password hash, a token prefix, an indexer API key and an engine secret, asserting none
   appear; import into an empty instance and assert the seven collections match; assert a dry run writes
   nothing and reports the same counts as the commit; assert `on_conflict: skip` keeps the existing row and
   `overwrite` replaces it; assert a newer `document_version` is `409`; assert `RestoreFrom` refuses a
   locked database, a foreign schema version and a corrupt file with the three named errors, and that the
   live database is untouched in each case.
9. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] An export contains no session id, password hash, API token, indexer API key or engine secret.
- [ ] Importing an export into an empty instance reproduces all seven collections.
- [ ] A dry run writes nothing and reports exactly what a commit would do.
- [ ] A committing import is transactional: a rejected row leaves the database unchanged.
- [ ] A `document_version` newer than the binary is `409` `/problems/conflict`.
- [ ] `restore --from` refuses with `restore_server_running` while a server holds the database.
- [ ] `restore --from` refuses with `restore_schema_mismatch` on a foreign schema version.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/api/... ./internal/store/..." && echo BACKUP_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/api` and `ok  github.com/L-K-M/dl-tool/internal/store`,
with `TestExportExcludesEverySecret`, `TestExportImportRoundTrip`, `TestDryRunWritesNothing`,
`TestImportIsTransactional`, `TestNewerDocumentVersionConflict`, `TestRestoreRefusesRunningServer` and
`TestRestoreRefusesSchemaMismatch` each reported as `--- PASS`. The final line of stdout is exactly
`BACKUP_OK`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT import from Download Station, qBittorrent or any other product. There is no migration subsystem,
  no adopt-in-place and no `rules.json` importer. This task is dl-tool's own backup and restore, nothing else.
- Do NOT include users, sessions, password hashes, API tokens or any secret in an export, under any flag.
- Do NOT export `tasks` or any task-derived table; a portable settings document is not a database copy.
- Do NOT add `POST /system/backup` or the nightly `VACUUM INTO` job; T091 owns both.
- Do NOT restore while the server is running, or skip any of the three gates.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
