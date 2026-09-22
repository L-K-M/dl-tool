# T083 — Load .torrent files dropped into a watch folder

| Field | Value |
|---|---|
| **ID** | T083 |
| **Milestone** | M6 |
| **Status** | done |
| **Depends on** | T020, T031, T066, T081 |
| **Blocks** | T107, T119 |
| **Parallel-safe** | no — it also edits the shared files `internal/jobs/cron.go`, `internal/store/settings.go`, `internal/api/server.go` and `cmd/dl-tool/main.go` |
| **Implements** | [FR-043](../02-requirements.md#fr-043-import-torrent-files-from-a-watch-folder), [FR-044](../02-requirements.md#fr-044-report-the-effective-destination) |
| **Decisions** | [ADR-0015](../decisions/0015-db-backed-in-process-job-queue.md), [ADR-0012](../decisions/0012-single-data-mount.md) |
| **Est. size** | 5 new files, ~540 LOC plus the composition-root wiring this repair assigns to `internal/api/server.go` and `cmd/dl-tool/main.go` |

## Goal
A `.torrent` file dropped into an enabled watch folder becomes a task in that folder's destination and
category, within one poll interval. Registration uses inotify and falls back to polling when inotify is
unavailable. With `delete_after_load` the source file is unlinked only after the
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
| `internal/store/settings.go` | modify | Add `WatchFolder`, `ListEnabledWatchFolders`, `GetWatchFolder`, `TouchWatchFolder`, `SeedWatchFolder` and the loaded-set pair. |
| `internal/jobs/cron.go` | modify | Add `Scheduler.WithWatcher` and start `Run` beside the cron entries. |
| `internal/api/watchcreator.go` | create | `watchTaskCreator`, the `jobs.TaskCreator` adapter over `TaskHandlers.CreateTasks`. |
| `internal/api/watchcreator_test.go` | create | The adapter's slug/detail → sentinel mapping exercised against the real create path. |
| `internal/api/server.go` | modify | Add the exported `Server.WatchCreator` field and build it in `NewServer`. |
| `cmd/dl-tool/main.go` | modify | Attach the watcher to the scheduler and seed `cfg.WatchDir`. |

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
type Watcher struct{ /* settings *store.SettingsStore; creator TaskCreator */ }

func NewWatcher(st *store.SettingsStore, creator TaskCreator) *Watcher

// ScanOnce scans exactly one watch folder and returns what it did. It is synchronous and
// idempotent: a file already loaded is skipped with SkipAlreadyLoaded, never loaded twice.
// A disabled folder still scans on demand — the scan is what the button does, not the schedule.
func (w *Watcher) ScanOnce(ctx context.Context, folderID string) (ScanResult, error)

// Run watches every enabled folder until ctx ends. It calls newOSWatcher for each folder; when
// registration fails it falls back to a time.Ticker at the folder's poll_interval_s, which is 10 by
// default (a value below 1 falls back to 10 as well — the API validates ≥1, the store does not).
// Both paths call ScanOnce and nothing else.
func (w *Watcher) Run(ctx context.Context) error

// newOSWatcher is the platform hook. The default is the polling implementation; the linux build
// replaces it in an init() with the inotify implementation, so no dependency outside the standard
// library is added. The Linux file uses syscall.InotifyInit1, syscall.InotifyAddWatch and
// syscall.IN_CLOSE_WRITE | syscall.IN_MOVED_TO.
var newOSWatcher = newPollWatcher

// TaskCreator is the T020 creation path, injected so the watcher never re-implements it.
// name is the file's base name — it feeds the rejection display and the task-name fallback,
// exactly as an UploadedFile part name does.
type TaskCreator interface {
	CreateFromTorrent(ctx context.Context, name string, blob []byte, dest, category string) (taskID string, err error)
}

// ErrTorrentDuplicate and ErrDestinationRejected are the classified failures the TaskCreator
// contract defines; ScanOnce maps them with errors.Is onto SkipDuplicate and SkipPathRejected.
// Any other error is a hand-off failure: the file stays in place and the scan records it in the
// folder's last_error through TouchWatchFolder.
var (
	ErrTorrentDuplicate    = errors.New("jobs: a task for this torrent already exists")
	ErrDestinationRejected = errors.New("jobs: destination resolves outside the data roots")
)

// WithWatcher attaches the loader; Start runs w.Run on the scheduler context beside the cron
// entries and joins it during the drain. The field is set before the cron goroutine can observe
// it — the same attach rule WithGovernor follows.
func (s *Scheduler) WithWatcher(w *Watcher) *Scheduler
```

```go
package api

// watchTaskCreator adapts TaskHandlers.CreateTasks to jobs.TaskCreator (file
// internal/api/watchcreator.go): the blob rides the same uploadedFilesKey context slot the
// multipart middleware fills, so normalisation, routing, the destination jail, dedup and the
// requested_destination echo stay the T020/T033 path unchanged.
type watchTaskCreator struct{ tasks *TaskHandlers }

// CreateFromTorrent first parses blob with uri.InspectTorrent and answers jobs.ErrTorrentDuplicate
// when c.tasks.FindByInfohash finds a live task — the same lookup duplicateRejection runs. It then
// calls CreateTasks with ctx carrying []UploadedFile{{Name: name, Bytes: blob}} and the body holding
// dest and category. A returned *huma.ErrorModel maps Type SlugPathRejected onto
// jobs.ErrDestinationRejected and a Detail prefixed by duplicateDetail onto jobs.ErrTorrentDuplicate
// (the commit-between-check-and-insert race); every other error passes through.
func (c watchTaskCreator) CreateFromTorrent(ctx context.Context, name string, blob []byte, dest, category string) (string, error)
```

`Server.WatchCreator jobs.TaskCreator` is the exported field `NewServer` fills beside
`RuleCreator` — `watchTaskCreator{tasks: tasks}` built at the same site — and `cmd/dl-tool` hands
it to the watcher, the same sharing rule `Server.RuleCreator` documents
(docs/14-conventions.md §8.3).

```go
package store

// WatchFolder is one row of watch_folders with the category name joined in from categories
// (docs/04-data-model.md section 3.6); Category is NULL when the folder has none or the category
// row was deleted (ON DELETE SET NULL). The struct lives in settings.go beside Category, Tag,
// Engine and NotificationChannel — the same placement T077 used.
type WatchFolder struct {
	ID              string  `db:"id"`
	Path            string  `db:"path"`
	Enabled         int     `db:"enabled"`
	Destination     string  `db:"destination"`
	Category        *string `db:"category"`
	DeleteAfterLoad int     `db:"delete_after_load"`
	PollIntervalS   int     `db:"poll_interval_s"`
	LastScanAt      *int64  `db:"last_scan_at"`
	LastError       *string `db:"last_error"`
	CreatedAt       int64   `db:"created_at"`
	UpdatedAt       int64   `db:"updated_at"`
}

// ListEnabledWatchFolders returns every row of watch_folders with enabled = 1.
func (s *SettingsStore) ListEnabledWatchFolders(ctx context.Context) ([]WatchFolder, error)

// GetWatchFolder returns one row by id, or ErrNotFound.
func (s *SettingsStore) GetWatchFolder(ctx context.Context, id string) (WatchFolder, error)

// TouchWatchFolder writes last_scan_at and last_error after a scan; an empty lastErr clears
// the column.
func (s *SettingsStore) TouchWatchFolder(ctx context.Context, id string, at int64, lastErr string) error

// SeedWatchFolder inserts one enabled row for path — the DLTOOL_WATCH_DIR seed — and reports
// whether it created the row. ON CONFLICT(path) DO NOTHING leaves an operator's row untouched
// across restarts.
func (s *SettingsStore) SeedWatchFolder(ctx context.Context, path, destination string) (created bool, err error)

// MaxWatchFolderLoaded bounds the per-folder loaded set; the oldest entries drop first, the
// same trim AppendExtractPassword applies. Eviction is graceful but visible: a still-present
// file whose infohash was trimmed re-surfaces as SkipDuplicate on the next sweep — task dedup,
// not the loaded set, keeps it from loading twice — and a SkipDuplicate file is never unlinked,
// even when delete_after_load is set.
const MaxWatchFolderLoaded = 4096

// WatchFolderLoaded reports whether the folder's loaded set already holds infohash. The set is
// the persistent record already_loaded is answered from: a per-folder settings key
// watch_folder_loaded_<id> holding a JSON array of infohashes, so the record survives a restart —
// the tasks row alone cannot separate already_loaded from torrent_duplicate.
func (s *SettingsStore) WatchFolderLoaded(ctx context.Context, folderID, infohash string) (bool, error)

// MarkWatchFolderLoaded adds infohash to the folder's loaded set; ScanOnce calls it only after
// the creator returned a task id.
func (s *SettingsStore) MarkWatchFolderLoaded(ctx context.Context, folderID, infohash string) error
```

The created task records both destinations, so a category or watch-folder default is visible rather than a
silent override: `destination` is what the server resolved and `requested_destination` carries what was asked
for when the two differ, per the Task object in
[`05-api-contract.md` §3](../05-api-contract.md#3-the-canonical-task-object)
([FR-044](../02-requirements.md#fr-044-report-the-effective-destination)).

## Steps
1. Add the `WatchFolder` row struct and the six watch-folder methods — `ListEnabledWatchFolders`,
   `GetWatchFolder`, `TouchWatchFolder`, `SeedWatchFolder`, `WatchFolderLoaded`,
   `MarkWatchFolderLoaded` — to `internal/store/settings.go` with explicit column lists, the category
   name joined in from `categories`.
2. Create `internal/jobs/watch.go` with the skip vocabulary, `ScanResult`, `Watcher`, `ScanOnce`, `Run`,
   `newOSWatcher`, the polling implementation and the `ErrTorrentDuplicate`/`ErrDestinationRejected`
   sentinels.
3. In `ScanOnce`, read each directory entry, skip anything not ending in `.torrent` with `SkipNotATorrent`,
   parse it with the T031 metainfo parser and skip an unparsable file with `SkipNotATorrent`. An entry
   that cannot be read at all is `SkipUnreadable`.
4. Consult `WatchFolderLoaded` on the parsed manifest's infohash — v1 when present, v2 otherwise, the
   identity `magnetFromManifest` prefers — and skip a known hash with `SkipAlreadyLoaded`. Then create
   the task through `TaskCreator` with the file's base name and the folder's `destination` and
   `category`; map `ErrDestinationRejected` to `SkipPathRejected` and `ErrTorrentDuplicate` to
   `SkipDuplicate`, and leave the file in place on any other error.
5. Unlink the source file only after the creator returned a task id, and only when `delete_after_load` is
   set; a failed hand-off always leaves the file in place.
6. Record each accepted file with `MarkWatchFolderLoaded` after the creator returned a task id, so a
   re-scan — in this process or after a restart — answers `SkipAlreadyLoaded`.
7. Create `internal/jobs/watch_inotify_linux.go` with `//go:build linux`, registering
   `syscall.IN_CLOSE_WRITE | syscall.IN_MOVED_TO` and installing itself into `newOSWatcher` from `init()`.
   Any registration error returns the polling watcher instead of failing. The inotify watcher still
   ticks at the folder's `poll_interval_s` between events: NFS and CIFS mounts accept the registration
   but never deliver remote writes, so the sweep is what keeps the one-interval bound there. The event
   path and the tick share one loop per folder (or a per-folder mutex), so `ScanOnce` never runs
   concurrently with itself — `MarkWatchFolderLoaded` is a read-modify-write on one settings key.
8. Create `internal/api/watchcreator.go` and edit `internal/api/server.go` to export
   `Server.WatchCreator`, filled in `NewServer` as `watchTaskCreator{tasks: tasks}` at the site that
   builds `RuleCreator`. Edit `internal/jobs/cron.go`: add `Scheduler.WithWatcher` and run
   `watcher.Run` on the scheduler context inside `Start` beside the cron entries, joining it during
   the drain. Edit `cmd/dl-tool/main.go`: build
   `jobs.NewWatcher(store.NewSettingsStore(db), server.WatchCreator)` and attach it at the existing
   `NewScheduler` construction chain, and when `cfg.WatchDir` is non-empty seed one enabled row —
   `SeedWatchFolder(ctx, cfg.WatchDir, root)` with `root` the
   `fsx.ResolveDestinationRoot(cfg.DataRoots, cfg.WatchDir)` result, the containing data root —
   before the watcher starts. `cfg.WatchDir` arrives already root-validated (`config.Load` warns and
   clears an out-of-roots value); a resolve or seed error is logged and skipped, never fatal.
9. Create `internal/jobs/watch_test.go`: drop a fixture `.torrent` into a temporary directory and assert a
   task appears within one poll interval; assert the file is removed with `delete_after_load` and kept
   without it; assert a `.part` file yields `not_a_torrent`; assert a second scan yields `already_loaded`;
   assert a creator answering `ErrTorrentDuplicate` yields `torrent_duplicate` and one answering
   `ErrDestinationRejected` yields `path_rejected`; assert a failing hand-off leaves the file; assert the
   fallback path runs when `newOSWatcher` errors.
10. Create `internal/api/watchcreator_test.go`: drive `watchTaskCreator` (or `Server.WatchCreator`) into
    the real `CreateTasks` responses — a live task carrying the blob's infohash answers
    `jobs.ErrTorrentDuplicate`, and a destination outside the roots answers
    `jobs.ErrDestinationRejected` — so the slug and `Detail`-prefix mapping is pinned against drift in
    the create endpoint's wording.
11. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [x] A dropped `.torrent` becomes a task within one poll interval, in the folder's destination.
- [x] `delete_after_load` unlinks the source only after the engine accepted the task.
- [x] A second scan skips the file with `already_loaded`; nothing is imported twice.
- [x] Every skip carries one of the five documented reasons and no other string.
- [x] The task's `destination` is the resolved path and `requested_destination` is non-null when they differ.
- [x] An inotify registration failure falls back to polling and the same files are still loaded.
- [x] Review check (not assertable from `internal/jobs/watch_test.go`) — `cmd/dl-tool/main.go` attaches
  `jobs.NewWatcher(store.NewSettingsStore(db), server.WatchCreator)` through `scheduler.WithWatcher`
  before `Start`, so an enabled folder is watched in production, and seeds `cfg.WatchDir` through
  `SeedWatchFolder` with the containing data root as `destination` when the variable is set
  (docs/14-conventions.md §8.3).

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/jobs/... ./internal/store/... ./internal/api/..." && echo WATCH_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/jobs`, `ok  github.com/L-K-M/dl-tool/internal/store` and
`ok  github.com/L-K-M/dl-tool/internal/api`, with `TestDroppedTorrentBecomesTask`,
`TestDeleteAfterLoadOnlyOnSuccess`, `TestSecondScanSkipsLoaded`, `TestNonTorrentSkipped`,
`TestRequestedDestinationRecorded` and `TestFallsBackToPolling` in jobs plus the adapter-mapping tests
of step 10 in api each reported as `--- PASS`. The final line of stdout is exactly `WATCH_OK`.
No `FAIL`.

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
`make lint && make test PKG="./internal/jobs/... ./internal/store/... ./internal/api/..." && echo WATCH_OK`:

```
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
go test -race -count=1 ./internal/jobs/... ./internal/store/... ./internal/api/...
ok  	github.com/L-K-M/dl-tool/internal/jobs	27.563s
ok  	github.com/L-K-M/dl-tool/internal/store	76.446s
ok  	github.com/L-K-M/dl-tool/internal/api	155.403s
WATCH_OK
```

The six named jobs tests and the adapter-mapping tests, individually:

```
--- PASS: TestDroppedTorrentBecomesTask (0.61s)
--- PASS: TestDeleteAfterLoadOnlyOnSuccess (0.38s)
--- PASS: TestSecondScanSkipsLoaded (0.33s)
--- PASS: TestNonTorrentSkipped (0.36s)
--- PASS: TestRequestedDestinationRecorded (0.35s)
--- PASS: TestFallsBackToPolling (0.34s)
ok  	github.com/L-K-M/dl-tool/internal/jobs	3.436s
--- PASS: TestWatchCreatorDuplicateMapsToSentinel (0.48s)
--- PASS: TestWatchCreatorPathRejectedMapsToSentinel (0.46s)
--- PASS: TestWatchCreatorRequestedDestinationEcho (0.39s)
--- PASS: TestWatchCreatorSameDestinationLeavesEchoNull (0.40s)
--- PASS: TestMapCreateError (0.00s)
ok  	github.com/L-K-M/dl-tool/internal/api	2.844s
```

`make ci` on the same tree: lint, vet, typecheck, `test` (Go + 279 vitest cases),
`compose-check` and `doclint` (2523 links, 0 errors) all green.

Scope:

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
cmd/dl-tool/main.go
internal/api/server.go
internal/api/watchcreator.go
internal/api/watchcreator_test.go
internal/jobs/cron.go
internal/jobs/watch.go
internal/jobs/watch_inotify_linux.go
internal/jobs/watch_test.go
internal/store/settings.go
```

## Blocked

The contract's `jobs.TaskCreator` (`CreateFromTorrent(ctx, blob, dest, category)`) has no implementer
and no delivery path inside the `## Files` table, so step 8's "start `Run` alongside the existing
entries" cannot run in production — the same defect class findings F342 records and F671 repaired for
T081. The only task-creation entry point is `(*api.TaskHandlers).CreateTasks`
(`internal/api/tasks.go:380`); the only adapter of that shape is the unexported `ruleTaskCreator`
(`internal/api/rules.go:144`), exported once as `Server.RuleCreator` — an `rss.TaskCreator` whose
`CreateForRule` takes a URI, never a `.torrent` blob. `internal/jobs` cannot build its own adapter: it
cannot import `internal/api` (`internal/api/search.go:25` already imports `internal/jobs` — an import
cycle), and re-implementing the create path is what the injected interface exists to prevent. Even the
re-implementation would fail: the destination jail needs `fsx.ResolveDestination(cfg.DataRoots, …)`
(doc 05 §15 checks `destination` against the roots, and this task's own out-of-scope rule makes an
outside folder `path_rejected`), the engine check needs `server.Engines`, and the `requested_destination`
echo (acceptance criterion 5) lives in `insertPlanned` — `cfg` and the registry are composition-root
state that `NewScheduler` never receives.

Nor can any in-scope file deliver a creator to the watcher. `NewWatcher(st, creator)` needs the
creator at construction; the only `Scheduler` construction site is `cmd/dl-tool/main.go:266`, a file
outside the `## Files` table, calling `jobs.NewScheduler(db, logger)` — a signature pinned by that
uneditable call. A `WithWatcher`-style attach on `Scheduler` would have no caller, which
docs/14-conventions.md §8.3 counts as not done. Every in-scope alternative is an improvisation the
plan never specified:

- **A nil or stub creator** lets `Run` start and every unit test pass (they inject fakes) while
  production scans folders and creates nothing — the "built and never wired" failure §8.3 exists to
  catch, hidden behind green tests.
- **A jobs-local creator** writing `tasks` rows through the store duplicates T020's routing,
  destination-jail, dedup, category and `requested_destination` logic, needs `cfg.DataRoots` and the
  engine registry `NewScheduler` cannot see, and is precisely what the contract's "injected so the
  watcher never re-implements it" forbids.

A second gap rides the same missing caller: step 8's `DLTOOL_WATCH_DIR` seed needs configuration the
Scheduler does not hold. `internal/config` already parses `DLTOOL_WATCH_DIR` into `cfg.WatchDir`
(`internal/config/config.go:188`) and clears it with `warningOutOfRoot` when it escapes every
`DLTOOL_DATA_ROOTS` entry (`internal/config/config.go:549`); `NewScheduler(db, logger)` receives
neither `cfg` nor the roots, so cron.go can neither reuse the validated value nor re-derive the
containment check without re-reading the environment — a second parser duplicating a fact doc 11
§2 gives to config. The seeded row's `destination` is also unspecified (doc 11 §2 names only the
`path`), and `watch_folders.destination` is `NOT NULL`.

Not itself blocking, for the repair's awareness: `store.WatchFolder` has no definition site (F261),
but `internal/store/settings.go` — in this table — already hosts the `Category`, `Tag`, `Engine` and
`NotificationChannel` row structs, so the struct can live beside the three new queries exactly as
T077 did.

Rerunnable evidence on this commit:

```bash
# No implementer of the contract's creator exists anywhere.
git grep -nE "CreateFromTorrent" HEAD -- '*.go'
# → exit 1: the signature is named only by this task file's contract.

# The only creator-shaped adapter and its one exported handle; it is
# URI-shaped (rss.TaskCreator), not blob-shaped.
git grep -nE "type ruleTaskCreator|RuleCreator" HEAD -- 'internal/api/*.go' | grep -v _test | sed 's|^[^:]*:||'
# → internal/api/rules.go:144 (adapter), internal/api/server.go:118 (rss.TaskCreator field).

# api already imports jobs, so jobs cannot reach TaskHandlers through api.
grep -rn "internal/jobs" internal/api/*.go | grep -v _test
# → internal/api/search.go:25

# The sole production Scheduler construction site; its signature is pinned.
git grep -nE "NewScheduler" HEAD -- '*.go' | grep -v _test | sed 's|^[^:]*:||'
# → cmd/dl-tool/main.go:266 (call), internal/jobs/cron.go:53 (definition NewScheduler(db, log)).

# DLTOOL_WATCH_DIR is already parsed and root-validated in config; the
# Scheduler sees neither cfg nor DataRoots.
grep -n "envWatchDir\|WatchDir" internal/config/config.go
# → config.go:50,128,188 (parsed into cfg.WatchDir); config.go:549-554 (out-of-root warn + clear).
```

Remedies for the owner — the choice changes the composition root or the contract, so it is not made
here (deciding files: this task's `## Files` table, docs/14-conventions.md §8.3):

1. Widen the `## Files` table with `cmd/dl-tool/main.go` and the api seam of the owner's choice —
   e.g. a new `internal/api/watchcreator.go` plus `internal/api/server.go` exporting
   `Server.WatchCreator jobs.TaskCreator`. The adapter can hand `CreateTasks` the blob through the
   multipart path (`context.WithValue(ctx, uploadedFilesKey{}, []UploadedFile{…})`, same package),
   so routing, dedup, the destination jail and the `requested_destination` echo come from T020/T033
   unchanged. main.go then attaches
   `scheduler.WithWatcher(jobs.NewWatcher(store.NewSettingsStore(db), server.WatchCreator))` — or the
   Scheduler construction moves below the server build — and seeds `cfg.WatchDir` (already
   root-validated) at the same site, with the repair also pinning the seeded row's `destination`.
   Same repair class the owner applied to T081 (F671).
2. Alternatively pin `NewScheduler(db, log, creator, watchDir)` (or a `WithWatcher` the contract
   names) and put `cmd/dl-tool/main.go` in the table to update the call site; the api adapter from
   remedy 1 is still required.
3. Alternatively amend the contract so `Run` is started by the job worker (`worker.Register`) with a
   creator delivered through a package the plan adds to the table — larger, and still a main.go edit.

Status stays `todo` pending plan repair — do not re-dispatch; the owner must pick remedy 1–3 or amend
the contract first.

**Repair applied 2026-09-22.** The ruling picked remedy 1, the smallest interpretation consistent
with the accepted ADRs and the merged code — the same repair class F671 applied to T081. The
`## Files` table now carries `internal/api/watchcreator.go`, `internal/api/server.go` and
`cmd/dl-tool/main.go`, and the contract pins every seam the blocked record found missing:
`watchTaskCreator` hands `CreateTasks` the blob through the `uploadedFilesKey` context slot, so
routing, the destination jail, dedup and the `requested_destination` echo stay the T020/T033 code
path; `Server.WatchCreator` exposes it the way `Server.RuleCreator` does; the classified failures
cross the package boundary as `jobs.ErrTorrentDuplicate` and `jobs.ErrDestinationRejected`, since
the create endpoint's all-refused answer flattens the rejected[] entry's slug into its detail text;
`Scheduler.WithWatcher` attaches the loader the way `WithGovernor` attaches the governor, leaving
the `NewScheduler(db, log)` signature untouched; and the `DLTOOL_WATCH_DIR` seed moved to the
composition root as `SeedWatchFolder(ctx, cfg.WatchDir, root)` with the destination pinned to the
`DLTOOL_DATA_ROOTS` entry containing the directory, resolved through `fsx.ResolveDestinationRoot` —
`watch_folders.destination` is `NOT NULL` and doc 11 §2 names only the path.

The repair also closes the sibling findings the blocked record cited: `WatchFolder` is pinned into
`internal/store/settings.go` beside `NotificationChannel` (F261, F086's T083 slice — the merged code
took concrete `*store.SettingsStore` everywhere, no aggregate); `watch_folders` carries no
`owner_id` after ADR-0019, so the seeded row needs no owner (F341); `tasks.requested_destination`
exists and `insertPlanned` writes it (F345); the persistent loaded set is a per-folder `settings`
key behind `WatchFolderLoaded`/`MarkWatchFolderLoaded`, which is why `already_loaded` survives a
restart (F343); and the inotify path keeps ticking at `poll_interval_s`, so NFS/CIFS mounts that
accept the registration but never deliver remote writes are still swept (F344). The creator
signature gained the file's base name so a watch-folder torrent gets the same display-name and
rejection text an upload would.

The index row stays `todo`; the next loop iteration implements the task.
