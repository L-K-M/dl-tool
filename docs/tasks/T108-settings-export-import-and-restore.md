# T108 — Export and import portable settings, and restore from the CLI

| Field | Value |
|---|---|
| **ID** | T108 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T080, T106, T107 |
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
	WatchFolders    []ExportWatch     `json:"watch_folders"`
	Schedule        ExportSchedule    `json:"schedule"`      // enabled + the 168 cells
}

// Excluded from every export, with no option to include them: sessions, the account row and therefore
// its password_hash, api_tokens, notification_channels.secret_enc, engines.secret_enc,
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
lands or none does. Paths are re-validated against the importing host's roots; a failing path is
`rejected`, never silently rewritten.

```go
package store

// RestoreFrom replaces the live database with the backup at src, following the staged procedure of
// docs/17-operations-and-runbook.md §3.4. The four gates run in this order and each is a named
// refusal, exit code 1:
//
//	restore_server_running  — flock(LOCK_EX|LOCK_NB) on the stable process lock fails
//	restore_source_rejected — src does not resolve to a regular file inside DLTOOL_CONFIG_DIR,
//	                          or names the live database, its lock or its sidecars
//	restore_schema_too_new  — MAX(version_id) in the backup exceeds the highest embedded migration;
//	                          an older schema is accepted and migrates forward at the next boot
//	restore_integrity_failed — PRAGMA integrity_check on the backup, opened read-only, is not "ok"
//
//	Any failure to open or read the backup as a database at all instead refuses as
//	restore_integrity_failed, whichever gate encounters it.
//
// Every gate completes before the command changes the live database. On success it copies src to a
// unique dl-tool.db.restore-<ULID>.tmp beside dbPath with O_EXCL, mode 0600 and fsync, then
// integrity-checks the staged copy; preserves the live database through VACUUM INTO to
// dl-tool.db.replaced-<UTC>.bak (integrity-checked, fsynced, renamed into place, directory fsynced);
// checkpoints with PRAGMA wal_checkpoint(TRUNCATE), closes every handle and removes the stale -wal
// and -shm sidecars; then atomically renames the staged file over dbPath and fsyncs the directory.
// A crash yields either the complete old file or the complete checked replacement. It returns the
// restored task count.
func RestoreFrom(ctx context.Context, dbPath, configDir, src string) (tasks int, err error)

var (
	ErrRestoreServerRunning  = errors.New("store: restore_server_running")
	ErrRestoreSourceRejected = errors.New("store: restore_source_rejected")
	ErrRestoreSchemaTooNew   = errors.New("store: restore_schema_too_new")
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
5. Re-validate every path against the importing host's roots, recording a failure in `rejected[]` with
   `/problems/path-rejected`.
6. Add `RestoreFrom` and the four sentinels to `internal/store/db.go`, running the gates in the
   documented order and following §3.4's staged procedure: the live database is changed only after
   all four gates pass, and any failure before the final atomic rename removes only the temporary
   stage.
7. Add the `restore --from <file>` subcommand to `cmd/dl-tool/main.go` through the existing `humacli`
   wiring, printing the named refusal and exiting `1` on any gate, and printing the restored task count on
   success. Map the `DLTOOL_CONFIG_DIR` path rejection to `restore_source_rejected`.
8. Create `internal/api/settings_export_test.go`: export from a populated instance and grep the document for
   a session id, a password hash, a token prefix, an indexer API key and an engine secret, asserting none
   appear; import into an empty instance and assert the seven collections match; assert a dry run writes
   nothing and reports the same counts as the commit; assert `on_conflict: skip` keeps the existing row and
   `overwrite` replaces it; assert a newer `document_version` is `409`; assert `RestoreFrom` refuses a
   locked database, a source outside `DLTOOL_CONFIG_DIR` (and the live database, lock or sidecars), a
   newer schema version and a corrupt file with the four named errors; that the live database is
   untouched in each case; that an older-schema backup restores and migrates forward on next open; and
   that a failure injected before the atomic rename leaves the original database intact.
9. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] An export contains no session id, password hash, API token, indexer API key or engine secret.
- [ ] Importing an export into an empty instance reproduces all seven collections.
- [ ] A dry run writes nothing and reports exactly what a commit would do.
- [ ] A committing import is transactional: a rejected row leaves the database unchanged.
- [ ] A `document_version` newer than the binary is `409` `/problems/conflict`.
- [ ] `restore --from` refuses with `restore_server_running` while a server holds the database.
- [ ] `restore --from` refuses with `restore_source_rejected` for a path outside `DLTOOL_CONFIG_DIR`
  or naming the live database, its lock or its sidecars, and the live database is untouched.
- [ ] `restore --from` refuses with `restore_schema_too_new` on a backup newer than the embedded
  migration maximum, printing both versions; an older-schema backup is accepted and migrates forward
  at the next boot.
- [ ] `restore --from` refuses with `restore_integrity_failed` on a corrupt file, and the live
  database is untouched.
- [ ] A failure injected before the final atomic rename leaves the original database intact, and a
  successful restore reports the source's task count.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/api/... ./internal/store/..." && echo BACKUP_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/api` and `ok  github.com/L-K-M/dl-tool/internal/store`,
with `TestExportExcludesEverySecret`, `TestExportImportRoundTrip`, `TestDryRunWritesNothing`,
`TestImportIsTransactional`, `TestNewerDocumentVersionConflict`, `TestRestoreRefusesRunningServer`,
`TestRestoreRejectsForeignSource`, `TestRestoreRefusesSchemaTooNew`, `TestRestoreRefusesCorrupt`,
`TestRestoreAcceptsOlderSchema` and `TestRestoreFailureLeavesOriginal` each reported as `--- PASS`. The final line of stdout is exactly
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
- Do NOT include the account row, sessions, password hashes, API tokens or any secret in an export, under any flag.
- Do NOT export `tasks` or any task-derived table; a portable settings document is not a database copy.
- Do NOT include the `ui_prefs` document in the export; doc 05 §11.4 keeps it out and
  [T129](T129-ui-prefs-document.md) owns it.
- Do NOT add `POST /system/backup` or the nightly `VACUUM INTO` job; T091 owns both.
- Do NOT restore while the server is running, or skip any of the four gates.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked — resolved

**Remedy 1 was applied.** The restore half now matches the adjudicated form of doc 17 §3.4, doc 04
§6 and FR-146: four gates (`restore_server_running`, `restore_source_rejected`,
`restore_schema_too_new` — older schemas accepted, forward-migrated at boot — and
`restore_integrity_failed`), the staged atomic replacement procedure that never leaves a window
without a valid `dl-tool.db`, the renamed sentinels, and updated steps, acceptance criteria and
Verification test names. The original record is preserved below.

---

The restore half of this file contradicts the documents that own restore behaviour on three facts
the task must act on — the schema gate's name and semantics, the gate set, and the replacement
procedure. This file's `RestoreFrom` contract predates the adjudication it conflicts with: it was
written in 7134d6e (2026-09-01) and is an ancestor of 7925568 "Make database recovery crash-safe"
(later the same day), which rewrote [`docs/17-operations-and-runbook.md` §3.4](../17-operations-and-runbook.md#34-dl-tool-restore---from-file),
[`docs/04-data-model.md` §6](../04-data-model.md#6-backup-and-restore) and
[FR-146](../02-requirements.md#fr-146-restore-a-backup-from-the-command-line) into the four-gate,
staged-atomic form. The 2026-09-02 consistency review (36f3af6) touched this file's import wording
but not the restore contract. So the restore half here is the stale side of an adjudicated change —
the same posture as T091's `dl-tool-<UTC>.db` name.

The three contradictions:

1. **Schema gate name and semantics.** This file specifies `restore_schema_mismatch` — "MAX(version_id)
   in the backup differs from the highest embedded migration" — in the contract comment, the sentinel
   `ErrRestoreSchemaMismatch`, the seventh acceptance criterion and the Verification block's
   `TestRestoreRefusesSchemaMismatch`. The documents say only a *newer* backup is refused, under a
   different name: doc 17 §3.4's gate table names `restore_schema_too_new` and states "Older is
   valid"; doc 04 §6 states the command "accepts schema versions at or below the embedded maximum;
   boot migrates an older restore forward"; FR-146 states "refuse a backup whose schema is newer than
   the binary ... An older schema is accepted and migrates forward at the next boot" and its Verify
   step requires restoring an older schema and observing the forward migration. Read literally, this
   file makes restoring an older nightly backup impossible — the exact flow FR-146, which this task
   claims to implement, requires.
2. **The gate set.** This file specifies three gates and repeats "the three gates" under Out of
   scope. Doc 17 §3.4 specifies four, adding `restore_source_rejected` — resolve to a regular file
   inside `DLTOOL_CONFIG_DIR`, rejecting the live database, lock and sidecars. This file requires the
   path check ("src must resolve inside DLTOOL_CONFIG_DIR", step 7 "Accept only a file inside
   DLTOOL_CONFIG_DIR") but names no refusal for it and omits the reject-live-database/lock/sidecar
   detail.
3. **The replacement procedure.** This file specifies "renames the current database to
   `dl-tool.db.replaced-<UTC>.bak`, copies the backup into place with mode 0600, removes any stale
   `-wal` and `-shm`". Between that rename and the copy completing, no valid `dl-tool.db` exists; a
   crash leaves a partial live file — the window 7925568 closed. Doc 17 §3.4 specifies a staged
   procedure instead: copy the source to `dl-tool.db.restore-<ULID>.tmp` with `O_EXCL`, mode `0600`
   and fsync, integrity-check the staged copy; preserve the live database through `VACUUM INTO` to
   `dl-tool.db.replaced-<UTC>.bak`; `PRAGMA wal_checkpoint(TRUNCATE)`, close handles, remove the
   sidecars; then atomically rename the staged file over `DLTOOL_DB_PATH` and fsync the directory —
   "a crash yields either the complete old file or the complete checked replacement". Doc 04 §6
   repeats that procedure, and doc 17's change log records the adjudication: "Made migration backup
   and database restore idempotent, lock-protected and crash-safe" (2026-09-01).

Every candidate diff contradicts a written requirement — the stop condition, not a judgment call:

- Implementing this file literally (refuse any differing schema version, three gates,
  rename-then-copy) violates FR-146's "older schema is accepted", doc 04 §6's "at or below the
  embedded maximum", doc 17 §3.4's gate names and staged procedure, and reopens the crash window the
  adjudication closed.
- Implementing the documents' form (four gates, `restore_schema_too_new`, staged rename) overrides
  this file's interface contract, the `ErrRestoreSchemaMismatch` sentinel, the seventh acceptance
  criterion and the Verification block's named tests.
- A hybrid (this file's refusal names over the documents' semantics) still overrides doc 17 §3.4's
  `restore_schema_too_new` name and the contract's "differs" language.

The export/import half is unaffected: `ExportDocument`, `ImportInput`, `ImportReport` and the
conflict keys match doc 05 §11.5. The task still cannot land partially — the Definition of Done ties
the code, the Evidence and both index flips to one commit, and this file's Verification requires
`TestRestoreRefusesSchemaMismatch` and `TestRestoreRefusesRunningServer` to PASS in that commit.

Remedies — either unblocks the task:

1. Restate this file's restore half in the adjudicated form: four gates with doc 17's refusal names
   (`restore_server_running`, `restore_source_rejected`, `restore_schema_too_new` refusing only a
   backup newer than the embedded maximum, `restore_integrity_failed`), the §3.4 staged procedure or
   an explicitly equivalent crash-safe sequence, and update the sentinels, steps 6–8, the acceptance
   criteria and the Verification block's test names to match.
2. Or rule that this file's contract is the intended spec and re-adjudicate the docs: doc 17 §3.4's
   gate table and five-step procedure plus its change log, doc 04 §6's "at or below the embedded
   maximum" sentence and procedure paragraph, and FR-146's "newer than the binary" / "older schema is
   accepted" / Verify wording — noting that this reverses the recorded crash-safety fix and removes
   the documented older-backup restore path.

The file that should answer it: this task file, after the owner picks 1 or 2.

Rerunnable evidence on this commit:

```bash
# This file's contract: three gates, mismatch-on-difference, rename-then-copy.
grep -n -E "restore_|RestoreFrom|three gates|differs" \
  docs/tasks/T108-settings-export-import-and-restore.md

# The adjudicated docs: four gates, too-new-only, staged atomic procedure.
grep -n -E "restore_schema_too_new|restore_source_rejected|Older is valid|embedded maximum" \
  docs/17-operations-and-runbook.md docs/04-data-model.md
grep -n -E "newer than the binary|[Oo]lder schema|forward migration" docs/02-requirements.md

# Ordering: this contract predates the crash-safe adjudication.
git merge-base --is-ancestor 7134d6e 7925568 && echo "task contract predates adjudication"
```

### 2026-09-23 — verified on cc90704, row set to `deferred`

The blocker is unchanged on cc90704, the head of `main` at evaluation time. Re-running the evidence
block above prints the same three facts: this file's contract still specifies three gates,
`restore_schema_mismatch` on any differing version and rename-then-copy; docs 17 §3.4 and 04 §6
still specify four gates, `restore_schema_too_new` (older valid) and the staged atomic procedure;
FR-146 still requires accepting an older schema and migrating it forward. Neither remedy above has
been picked.

Additions to the record:

- The picker takes the topmost eligible `todo` row, and T108 heads the eligible set, so leaving it
  `todo` re-selects it on every iteration. The row is set to `deferred`, the status the picker skips
  (the T091 precedent, #267), so the queue can proceed to T110 — whose `Depends on` is all `done` —
  and the other eligible rows. This chooses neither remedy: the owner still decides, and
  un-deferring is a one-word flip back to `todo` in each of the three status cells (see the
  reactivation trigger below). FR-145 and FR-146 stay `must`; the deferral parks
  the task rather than waiving the requirements.
- Downstream impact is nil beyond what already stands: the only dependent is T121 (`Depends on`:
  T053, T091, T092, T096, T108), and it is already unreachable behind T091's deferral through
  T092/T096. T110, T111 and T120 are eligible without T108.
- Reactivation trigger: flip both T108 rows in `00-task-index.md` — the `## M6` milestone-table row
  and the `## Roster` detail-table row — and this file's `**Status**` row back to `todo` in the same
  change that lands the chosen remedy (remedy 1 rewrites this file; remedy 2 re-adjudicates doc 17
  §3.4, doc 04 §6 and FR-146), so the deferral cannot outlive its cause.
- T108 sits in M6, whose exit checkpoint ("auto-extract, the watch folder and the 24×7 grid all work
  end to end") does not name restore — but the plan-wide Definition of Done requires every row
  `done` or `deferred`, so this record is where the gap stays visible until the owner rules.
