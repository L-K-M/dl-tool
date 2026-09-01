# T083 — Load .torrent files dropped into a watch folder

| Field | Value |
|---|---|
| **ID** | T083 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T020, T031, T066, T081 |
| **Blocks** | T107, T119 |
| **Parallel-safe** | no — it also edits the shared files `internal/jobs/cron.go`, `internal/store/settings.go` |
| **Implements** | [FR-043](../02-requirements.md#fr-043-import-torrent-files-from-a-watch-folder), [FR-044](../02-requirements.md#fr-044-report-the-effective-destination) |
| **Decisions** | [ADR-0015](../decisions/0015-db-backed-in-process-job-queue.md), [ADR-0012](../decisions/0012-single-data-mount.md) |
| **Est. size** | 3 new files, ~360 LOC |

## Goal
A `.torrent` file dropped into an enabled watch folder becomes a task owned by that folder's owner, in that
folder's destination and category, within one poll interval. Registration uses inotify and falls back to
polling when inotify is unavailable. With `delete_after_load` the source file is unlinked only after the
engine accepted the task.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §15 Watch folders](../05-api-contract.md#15-watch-folders)
2. [`docs/04-data-model.md` §3.6 Jobs, schedule and preferences](../04-data-model.md#36-jobs-schedule-and-preferences)
3. [`docs/02-requirements.md` FR-043](../02-requirements.md#fr-043-import-torrent-files-from-a-watch-folder)
4. [`docs/02-requirements.md` FR-044](../02-requirements.md#fr-044-report-the-effective-destination)
5. [`docs/11-config-reference.md` §2 `DLTOOL_` variables (application)](../11-config-reference.md#2-dltool_-variables-application)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/jobs/watch.go` | create | `Watcher`, `ScanOnce`, the polling loop and the skip-reason vocabulary. |
| `internal/jobs/watch_inotify_linux.go` | create | The inotify registration that replaces the polling loop on Linux. |
| `internal/jobs/watch_test.go` | create | Scan, skip-reason, delete-after-load and fallback cases. |
| `internal/store/settings.go` | modify | Add `ListEnabledWatchFolders`, `GetWatchFolder`, `TouchWatchFolder`. |
| `internal/jobs/cron.go` | modify | Register the watch-folder entry and seed `DLTOOL_WATCH_DIR`. |

No other file may be modified.

## Interface contract

```go
package jobs

// SkipReason is the closed vocabulary of doc 05 §15.
type SkipReason string

const (
	SkipNotATorrent   SkipReason = "not_a_torrent"
	SkipAlreadyLoaded SkipReason = "already_loaded"
	SkipUnreadable    SkipReason = "unreadable"
	SkipDuplicate     SkipReason = "torrent_duplicate"
	SkipQuota         SkipReason = "quota_exceeded"
	SkipPathRejected  SkipReason = "path_rejected"
)

// ScanResult is the body of POST /watch-folders/{id}/scan.
type ScanResult struct {
	Scanned   int             `json:"scanned"`
	Created   []string        `json:"created"`
	Skipped   []SkippedFile   `json:"skipped"`
	ElapsedMS int64           `json:"elapsed_ms"`
}

type SkippedFile struct {
	File   string     `json:"file"`
	Reason SkipReason `json:"reason"`
}

// Watcher loads torrents from watch folders.
type Watcher struct{ /* store *store.Store; creator TaskCreator */ }

func NewWatcher(st *store.Store, creator TaskCreator) *Watcher

// ScanOnce scans exactly one watch folder and returns what it did. It is synchronous and
// idempotent: a file already loaded is skipped with SkipAlreadyLoaded, never loaded twice.
// A disabled folder still scans on demand — the scan is what the button does, not the schedule.
func (w *Watcher) ScanOnce(ctx context.Context, folderID string) (ScanResult, error)

// Run watches every enabled folder until ctx ends. It calls newOSWatcher for each folder; when
// registration fails it falls back to a time.Ticker at the folder's poll_interval_s, which is 10 by
// default. Both paths call ScanOnce and nothing else.
func (w *Watcher) Run(ctx context.Context) error

// newOSWatcher is the platform hook. The default is the polling implementation; the linux build
// replaces it in an init() with the inotify implementation, so no dependency outside the standard
// library is added. The Linux file uses syscall.InotifyInit1, syscall.InotifyAddWatch and
// syscall.IN_CLOSE_WRITE | syscall.IN_MOVED_TO.
var newOSWatcher = newPollWatcher

// TaskCreator is the T020 creation path, injected so the watcher never re-implements it.
type TaskCreator interface {
	CreateFromTorrent(ctx context.Context, ownerID string, blob []byte, dest, category string) (taskID string, err error)
}
```

```go
package store

// ListEnabledWatchFolders returns every row of watch_folders with enabled = 1.
func (s *SettingsStore) ListEnabledWatchFolders(ctx context.Context) ([]WatchFolder, error)

// GetWatchFolder returns one row by id, or ErrNotFound.
func (s *SettingsStore) GetWatchFolder(ctx context.Context, id string) (WatchFolder, error)

// TouchWatchFolder writes last_scan_at and last_error after a scan.
func (s *SettingsStore) TouchWatchFolder(ctx context.Context, id string, at int64, lastErr string) error
```

The created task records both destinations, so a category or watch-folder default is visible rather than a
silent override: `destination` is what the server resolved and `requested_destination` carries what was asked
for when the two differ, per the Task object in
[`05-api-contract.md` §3](../05-api-contract.md#3-the-canonical-task-object)
([FR-044](../02-requirements.md#fr-044-report-the-effective-destination)).

## Steps
1. Add the three watch-folder queries to `internal/store/settings.go` with explicit column lists.
2. Create `internal/jobs/watch.go` with the skip vocabulary, `ScanResult`, `Watcher`, `ScanOnce`, `Run`,
   `newOSWatcher` and the polling implementation.
3. In `ScanOnce`, read each directory entry, skip anything not ending in `.torrent` with `SkipNotATorrent`,
   parse it with the T031 metainfo parser and skip an unparsable file with `SkipNotATorrent`.
4. Create the task through `TaskCreator` with the folder's `owner_id`, `destination` and `category`; map a
   rejected destination to `SkipPathRejected`, a known infohash to `SkipDuplicate` and a quota refusal to
   `SkipQuota`.
5. Unlink the source file only after the creator returned a task id, and only when `delete_after_load` is
   set; a failed hand-off always leaves the file in place.
6. Record the loaded files so a re-scan skips them with `SkipAlreadyLoaded`, keyed on the torrent's
   infohash rather than the filename.
7. Create `internal/jobs/watch_inotify_linux.go` with `//go:build linux`, registering
   `syscall.IN_CLOSE_WRITE | syscall.IN_MOVED_TO` and installing itself into `newOSWatcher` from `init()`.
   Any registration error returns the polling watcher instead of failing.
8. Edit `internal/jobs/cron.go` to start `Run` alongside the existing entries and, when `DLTOOL_WATCH_DIR`
   is set and no row exists for it, seed one enabled `watch_folders` row pointing at it.
9. Create `internal/jobs/watch_test.go`: drop a fixture `.torrent` into a temporary directory and assert a
   task appears within one poll interval; assert the file is removed with `delete_after_load` and kept
   without it; assert a `.part` file yields `not_a_torrent`; assert a second scan yields `already_loaded`;
   assert a failing hand-off leaves the file; assert the fallback path runs when `newOSWatcher` errors.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] A dropped `.torrent` becomes a task within one poll interval, owned by the folder's owner.
- [ ] `delete_after_load` unlinks the source only after the engine accepted the task.
- [ ] A second scan skips the file with `already_loaded`; nothing is imported twice.
- [ ] Every skip carries one of the six documented reasons and no other string.
- [ ] The task's `destination` is the resolved path and `requested_destination` is non-null when they differ.
- [ ] An inotify registration failure falls back to polling and the same files are still loaded.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/jobs/... ./internal/store/..." && echo WATCH_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/jobs` and `ok  github.com/L-K-M/dl-tool/internal/store`,
with `TestDroppedTorrentBecomesTask`, `TestDeleteAfterLoadOnlyOnSuccess`, `TestSecondScanSkipsLoaded`,
`TestNonTorrentSkipped`, `TestRequestedDestinationRecorded` and `TestFallsBackToPolling` each reported as
`--- PASS`. The final line of stdout is exactly `WATCH_OK`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add the `/watch-folders` CRUD endpoints or `POST /watch-folders/{id}/scan`; T107 owns them and
  calls `ScanOnce` from this task.
- Do NOT add `fsnotify` or any other dependency; inotify comes from the standard library `syscall`
  package and the polling loop is the portable path.
- Do NOT import a `.txt` URL list here; the add-task dialog owns that path and T020 owns the endpoint.
- Do NOT read, migrate or import another download manager's state; there is no migration subsystem.
- Do NOT scan a directory outside `DLTOOL_DATA_ROOTS`; a folder that resolves outside is `path_rejected`.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
