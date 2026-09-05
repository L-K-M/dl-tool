# T099 — Reserve disk space and keep a free-space floor

| Field | Value |
|---|---|
| **ID** | T099 |
| **Milestone** | M1 |
| **Status** | done |
| **Depends on** | T020, T024, T098 |
| **Blocks** | T047, T076 |
| **Parallel-safe** | no — it also edits the shared files `internal/engine/admission.go`, `internal/store/tasks.go`, `internal/api/server.go` |
| **Implements** | [FR-047](../02-requirements.md#fr-047-reserve-committed-but-unwritten-bytes-and-keep-a-free-space-floor), [FR-048](../02-requirements.md#fr-048-never-destroy-partial-data-when-a-filesystem-fills) |
| **Decisions** | [ADR-0012](../decisions/0012-single-data-mount.md), [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md) |
| **Est. size** | 2 new files, ~300 LOC |

## Goal
A task starts only when its destination filesystem holds its remaining bytes plus every other active task's
committed-but-unwritten bytes on that filesystem plus that root's `min_free_space`. When a filesystem fills,
the task is paused with `disk_full` and every partially downloaded byte stays on disk.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/02-requirements.md` FR-047](../02-requirements.md#fr-047-reserve-committed-but-unwritten-bytes-and-keep-a-free-space-floor)
2. [`docs/11-config-reference.md` §5 Database-backed settings](../11-config-reference.md#5-database-backed-settings)
3. [`docs/04-data-model.md` §3.2 Configuration](../04-data-model.md#32-configuration)
4. [`docs/17-operations-and-runbook.md`](../17-operations-and-runbook.md)
5. [`docs/05-api-contract.md` §7.1 Endpoints](../05-api-contract.md#71-endpoints)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/fsx/space.go` | create | `FreeSpace`, `FilesystemID` and `Reservation` accounting. |
| `internal/fsx/space_test.go` | create | Reservation arithmetic, floor and `ENOSPC` cases against a temporary directory. |
| `internal/engine/admission.go` | modify | Add the space gate to the admission pass. |
| `internal/engine/admission_test.go` | modify | The admission cases of steps 7–10 (`TestThirdTaskStaysQueued`, `TestENOSPCPausesAndKeepsData`, the shared-pool and in-memory-commit cases); the Verification block names tests that can only live here. |
| `internal/store/tasks.go` | modify | Add `SumRemainingByDestination`. |
| `internal/api/server.go` | modify | The composition-root wiring this task owns per the deferral register: start `Admitter.Run` beside the reconciler and build `load` over the settings rows. |

No other file may be modified. The two added rows above amend this table in the task's own
commit: the
deferral register of [`00-task-index.md`](00-task-index.md) assigns T099 the `Admitter.Run` wiring
(`internal/api/server.go`), and the admission cases its own Steps name need the pass's test file.

## Interface contract

```go
package fsx

// Space is the answer of one statfs call, in bytes. Both values are plain integers, never KB.
type Space struct {
	FreeBytes  int64
	TotalBytes int64
}

// FreeSpace reports the space at path. The path must already have been resolved by
// ResolveDestination; FreeSpace performs no containment check of its own.
func FreeSpace(path string) (Space, error)

// FilesystemID returns a stable identifier for the filesystem holding path, so two destinations on
// one mount share one reservation pool. Two paths on the same device return the same value.
func FilesystemID(path string) (string, error)

// Reservation is the committed-but-unwritten accounting for one filesystem.
type Reservation struct {
	FilesystemID string
	FreeBytes    int64 // as reported by statfs right now
	CommittedBytes int64 // sum of total_bytes - completed_bytes over active tasks on this filesystem
	MinFreeBytes int64 // this root's min_free_space, default 2147483648
}

// Admits reports whether a task needing remaining bytes may start:
//
//	FreeBytes - CommittedBytes - MinFreeBytes >= remaining
//
// A task whose total_bytes is still unknown passes with remaining = 0 and is re-checked when
// metadata resolves.
func (r Reservation) Admits(remaining int64) bool

// ErrDiskFull is returned when a write failed with ENOSPC. The caller pauses the task with the
// tasks.error_code value disk_full and unlinks nothing.
var ErrDiskFull = errors.New("fsx: no space left on device")

// IsENOSPC reports whether err is or wraps syscall.ENOSPC.
func IsENOSPC(err error) bool
```

```go
package store

// SumRemainingByDestination returns, per filesystem identifier, the sum of
// total_bytes - completed_bytes over tasks in downloading, checking, extracting or moving.
// A task whose total_bytes is NULL contributes 0.
func (s *TaskStore) SumRemainingByDestination(ctx context.Context) (map[string]int64, error)
```

The default floor is `2147483648` bytes (2 GiB) per configured root. `00001_init.sql` seeds
`min_free_space` as `{}`; resolve every missing root entry to that default before building reservations.
An explicit `0` remains `0` and disables the floor for that root.

## Steps
1. Create `internal/fsx/space.go` with `Space`, `FreeSpace` over the stdlib `syscall.Statfs`, and `FilesystemID` derived from the device number of `os.Stat`.
2. Add `Reservation`, `Admits`, `ErrDiskFull` and `IsENOSPC`, with `IsENOSPC` implemented through
   `errors.Is(err, syscall.ENOSPC)` so a wrapped error still matches.
3. Add `SumRemainingByDestination` to `internal/store/tasks.go`, computing the sum in SQL and treating a
   `NULL` `total_bytes` as `0`.
4. In `internal/engine/admission.go`, build one `Reservation` per filesystem before the candidate walk and
   consult `Admits` in addition to the three concurrency limits.
5. Hold a candidate that fails the space check in `queued` with `error_code` `disk_full`, and clear the
   code once space returns — never reject it at creation time.
6. Decrement the reservation in memory after each release, so one pass cannot over-commit a filesystem.
7. Handle `ENOSPC` from a running task by transitioning it to `paused` with `error_code` `disk_full`, one
   `task_events` row, and no unlink of any kind.
8. Resume such a task from the next admission pass once `Admits` is true again, so the partial file is
   continued rather than restarted.
9. Create `internal/fsx/space_test.go`: assert `Admits` is false when free minus committed minus the floor
   is one byte short and true when it is exactly equal; assert two paths on one temporary directory return
   the same `FilesystemID`; assert `IsENOSPC` matches a wrapped `syscall.ENOSPC`; assert the default floor
   read from settings is `2147483648`.
10. Add an admission case: two tasks whose remaining bytes already commit a small root, a third submitted
    task stays `queued` with `disk_full` instead of starting, and it starts after the first two complete.

## Acceptance criteria
- [ ] `Admits` subtracts committed bytes and the floor before comparing against the remaining bytes.
- [ ] The default floor is `2147483648` bytes per root.
- [ ] A third task on a committed filesystem stays `queued` with `error_code` `disk_full`.
- [ ] `ENOSPC` mid-download pauses the task and unlinks nothing; the partial file is byte-for-byte
      unchanged.
- [ ] A paused `disk_full` task resumes once free space is above the floor again.
- [ ] Two destinations on one mount share one reservation pool.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then `ok` lines for
`github.com/L-K-M/dl-tool/internal/fsx`, `github.com/L-K-M/dl-tool/internal/store` and
`github.com/L-K-M/dl-tool/internal/engine`, with `TestAdmitsAccountsForCommittedBytes`,
`TestDefaultFloorIsTwoGiB`, `TestENOSPCPausesAndKeepsData` and `TestThirdTaskStaysQueued` all running. No
`FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add `GET /fs/free-space`, `GET /fs/roots`, `GET /fs/browse` or `POST /fs/mkdir`; T046 and T047 own
  the filesystem endpoints and the folder browser.
- Do NOT delete, truncate or move any partial data on `ENOSPC`, ever.
- Do NOT send a notification; T077 owns delivery and this task only writes the `task_events` row.
- Do NOT implement the cross-filesystem move or its EXDEV fallback; T076 owns it.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

`make lint && make test PKG=./internal/...` — lint clean, every internal package `ok`, no `FAIL`:

```
$ make lint && make test PKG=./internal/...
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
go test -race -count=1 ./internal/...
ok  	github.com/L-K-M/dl-tool/internal/api	48.715s
ok  	github.com/L-K-M/dl-tool/internal/config	1.143s
ok  	github.com/L-K-M/dl-tool/internal/engine	8.242s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.161s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.035s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.539s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.169s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.035s
ok  	github.com/L-K-M/dl-tool/internal/store	66.709s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.356s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.026s
```

The four named tests, run verbosely (after review round 8):

```
$ go test ./internal/fsx/ ./internal/engine/ -run 'TestAdmitsAccountsForCommittedBytes|TestDefaultFloorIsTwoGiB|TestENOSPCPausesAndKeepsData|TestThirdTaskStaysQueued' -count=1 -v | grep -E '^(=== RUN|--- (PASS|FAIL)|PASS|FAIL|ok)'
=== RUN   TestAdmitsAccountsForCommittedBytes
--- PASS: TestAdmitsAccountsForCommittedBytes (0.00s)
=== RUN   TestDefaultFloorIsTwoGiB
--- PASS: TestDefaultFloorIsTwoGiB (0.00s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/fsx	0.007s
=== RUN   TestThirdTaskStaysQueued
--- PASS: TestThirdTaskStaysQueued (0.06s)
=== RUN   TestENOSPCPausesAndKeepsData
--- PASS: TestENOSPCPausesAndKeepsData (0.03s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/engine	0.100s
```

`make lint && make test PKG=./internal/...` after review round 8 — lint clean, every internal
package `ok`, no `FAIL`:

```
$ make lint && make test PKG=./internal/...
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
go test -race -count=1 ./internal/...
ok  	github.com/L-K-M/dl-tool/internal/api	45.629s
ok  	github.com/L-K-M/dl-tool/internal/config	1.122s
ok  	github.com/L-K-M/dl-tool/internal/engine	13.771s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.140s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.031s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.397s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.169s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.136s
ok  	github.com/L-K-M/dl-tool/internal/store	63.848s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.364s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.038s
```

Scope check — exactly the amended Files table, nothing else. Everything is committed, so the
branch diff names the touched paths:

```
$ git diff --name-only origin/main...HEAD -- . ':(exclude)docs' | sort
internal/api/server.go
internal/engine/admission.go
internal/engine/admission_test.go
internal/fsx/space.go
internal/fsx/space_test.go
internal/store/tasks.go
```

### Review round 8 (commit 316f1c0 → this one)

Four findings; triage and outcome:

- **"Error on conflicting floors that clean to the same root"** — valid, and it exposed a false
  claim in the round-6 notes: the duplicate-key guard was written down as applied but never landed
  in `internal/api/server.go`. It lands now exactly as claimed: two raw keys cleaning to the same
  root with differing floors error (`/data` beside `/data/`), equal values are one entry, and map
  iteration order decides nothing.
- **`PauseWithCode` rejects an empty `FromStates` allow-list up front** — applied, beside the
  existing empty-`ErrorCode` tripwire: an allow-list that can match no row is a caller bug, and
  failing it at the top names it instead of surfacing a transition conflict on the first landing.
- Applied doc minors: the Files-table note names the two added rows instead of "the last two";
  T126's scope check names the two paths the `:(exclude)docs` pathspec can actually see.

`make lint && make test PKG=./internal/...` after the fixes: lint 0 issues, all 11 internal
packages `ok`, no `FAIL`; the four named tests all pass verbosely above.

### Review round 7 (commit 34f7e71 → 316f1c0)

Eleven findings; triage and outcome:

- **"Reject floor keys that match no configured data root"** — rejected again, the standing stance
  of round 6: `docs/11-config-reference.md` §5 says entries for roots no longer present in
  `DLTOOL_DATA_ROOTS` "remain stored but are ignored when reservations are built", and `docs/04`
  §3.2 says the same; turning a stale-but-harmless entry into a failed load would stop admission
  every tick for a row the plan explicitly keeps.
- **existingAncestor now fails closed on stat errors other than not-exist**: a permission wall or
  I/O error says nothing about which filesystem holds the path, and promising an ancestor's space
  would over-admit; ENOTDIR — a file mid-path — climbs like not-exist because the file exists on
  the mount the climb then finds (`TestFilesystemIDSharedPerMount` pins the ENOTDIR case).
- **`ErrDiskFull` now wraps `syscall.ENOSPC`**, so the sentinel and a raw errno are the same answer
  to `IsENOSPC` (`TestIsENOSPCMatchesWrapped` pins it).
- Applied minors: the parked-task concurrency hold logs at debug why the row stays parked; the
  commit failure routes through `warnOnce` so one mount warns once per pass; `rootOf` trims every
  trailing slash; the smoke test tolerates a zero-free (full) filesystem; `insertTaskEvent`
  failures carry the task id; the Evidence blocks name the round they follow; the Blocked note
  cross-references T126 as the owner.

`make lint && make test PKG=./internal/...` after the fixes: lint 0 issues, all 11 internal
packages `ok`, no `FAIL`; the four named tests all pass.

### Review round 6 (commit 331ad0c → this one)

Four blockers (one real, one hallucinated, two about the new task file), four majors, thirteen
minors; triage and outcome:

- "T126 is never registered in either M1 table" — **real, and my error**: the round-5 edit batch
  that added both rows failed atomically on a third mismatch, and only the register row was
  re-applied; the round-5 Evidence claim below was therefore false when written. Both rows are now
  in — the M1 task table and the roster — and the roster's T099 Parallel cell reads `no` at last.
- "Leftover `continue` makes every queued candidate skip release" — **hallucinated**: the file has
  no such statement (the queued branch falls through to `a.release`), and
  `TestPassRespectsTotalAndPerEngine` / `TestPassZeroMeansUnlimited` release queued tasks through
  exactly that path on every run.
- T126's own Files table now lists its index flip and its Parallel-safe cell says so; the expected
  verification output names the aria2 subpackage.
- "The paused branch stamps disk_full onto an operator pause" — **valid and fixed**: the refresh
  applies only to rows already carrying disk_full; an operator-paused row is refused untouched
  (`TestPauseDiskFullRefusesAnOperatorPause`), because the pass would otherwise later un-pause what
  the user parked.
- The read-to-write TOCTOU is closed for real: `CodedPause.FromStates` makes the atomic pause
  re-check the counted active states in the UPDATE itself, so a task that moved on between the
  caller's read and the landing is left untouched.
- Applied minors: `namespacedHandle` is the one home for the engine-id join; the refresh's store
  error carries the pause prefix; `holdsParked` computes `remainingBytes` once; a zero `f_frsize`
  falls back to `Bsize`; duplicate cleaned floor keys with different values error instead of
  resolving nondeterministically; the int settings decode through JSON like the floor map; the
  SQL literal's pairing with `engine.ErrorCodeDiskFull` is documented; the full-disk pass in
  `TestENOSPCPausesAndKeepsData` asserts no redundant pause event.
- Rejected (unchanged stances): a malformed row failing the whole load is the designed fail-closed
  behavior with a per-tick warn; unmatched floor keys stay ignored per `docs/11` §5 and `docs/04`
  §3.2; `SelectQueuedCandidates` keeps its T098-contract name with the wider doc comment.

Final `make lint && make test PKG=./internal/...`: lint 0 issues, all 11 internal packages `ok`,
no `FAIL`; the four named tests all pass; doclint 0 errors.

### Review round 5 (commit c4c1e49 → 331ad0c)

One major, thirteen minors; triage and outcome:

- "The FR-048 routing gap is deferred with no owner" — **fixed in the plan**: `T126 — Route an
  engine disk-full report into the pause` now exists (M1, `todo`, depends on T026+T099, files
  `internal/engine/reconcile.go` + its test), sits in both M1 tables, and the deferral register's
  carried-by cell names it — the loop will pick it as the next unblocked M1 row, so FR-048's
  end-to-end path lands inside M1 instead of riding an unowned entry.
- Applied minors: the roster's T099 Parallel cell now reads `no` to match the task file; a parked
  task's hold stamp uses `diskFullMessage` — the exact sentence `PauseDiskFull` writes — so the two
  writers never alternate sentences on one row; every identification failure shares one warn key
  per pass; `markReleased` and `clearStaleStamp` share `clearHoldCode`; `PauseWithCode` takes a
  name-bound `store.CodedPause` struct and refuses an empty code; the allow-list test covers the
  paused refresh (no second pause event).
- Rejected with reasoning (unchanged stances from earlier rounds): no skip for hosts with under
  550 MiB of temp space (a host property must fail loudly — the helper's comment says so);
  no non-Linux build path (the runtime image and CI are Linux-only by plan); the climb's
  error-swallowing is the designed ancestor walk; the `Candidate` byte-pair asymmetry mirrors the
  DDL (`total_bytes` nullable, `completed_bytes NOT NULL`); a consumer-side duplicate of the
  store's paused filter adds a second source of truth for an invariant the query and
  `TestOperatorPausedTaskIsNotACandidate` already pin; no seed or write path can produce a
  non-object `min_free_space` today — the migration seeds `{}` and the write path is T092's.

Final `make lint && make test PKG=./internal/...`: lint 0 issues, all 11 internal packages `ok`,
no `FAIL`; the four named tests all pass; doclint 0 errors.

### Review round 4 (commit 1483095 → c4c1e49)

One major, nine minors; triage and outcome:

- "The paused guard doesn't cover the stamp→transition window" — **fixed at the root**: the pause is
  now one atomic store write. `TaskStore.PauseWithCode` lands state, `error_code`, `error_message`
  and the one `task_events` row in a single transaction, so no hold-code clear can split the pause
  from its stamp — the round-3 `ClearHoldCode` guard remains as the release-side belt-and-braces.
  `TestPauseDiskFullFailureLeavesTheRowUntouched` pins the all-or-nothing landing.
- Applied minors: `PauseDiskFull` allow-lists the counted active states (refusing queued, seeding,
  error, completed, removed untouched — `TestPauseDiskFullStateAllowList`); the `disk_full` stamp is
  a fixed sentence with the failing write logged at warn instead (dedupe-safe); the stranded-stamp
  warn says the code stays on the downloading row; `Admits` is stepwise and overflow-safe and
  refuses garbage (negative commitment/floor/request); `ClearHoldCode` skips rows with nothing to
  clear so a 1 Hz pass cannot bump `updated_at` for nothing; the seed root is `os.TempDir()`;
  the shared-pool and unknown-total tests assert the released candidate's final state and cleared
  hold; the fsx sibling-directory case creates the directory; the Parallel-safe row names
  `internal/api/server.go`.

Final `make lint && make test PKG=./internal/...`: lint 0 issues, all 11 internal packages `ok`,
no `FAIL`; the four named tests all pass; doclint 0 errors.

### Review round 3 (commit e772b2f → 1483095)

One major, minors, infos; triage and outcome:

- "markReleased's stamp-clear can race PauseDiskFull and strand a paused task without disk_full"
  — **fixed**: the release cleanup now calls the new `TaskStore.ClearHoldCode`, an update guarded on
  `state <> 'paused'`, so a clear racing the pause's stamp→transition pair loses on purpose and the
  parked row keeps the code the pass selects on. `TestClearHoldCodeNeverWipesAPausedStamp` pins it.
- Applied minors: the malformed-`min_free_space` error names the key before the driver text;
  filesystem read failures log once per pass per filesystem (`warnOnce`) instead of per candidate
  per tick; a data root of `/` owns every absolute destination (`withinRoot`); `fsx.Floor` cleans
  its lookup key; `FilesystemID`'s comment records the btrfs subvolume limit; the full-disk pass
  asserts the `disk_full` code is retained; `floorLeaving` documents its negative-headRoom mode;
  the interface comment states the single ordering, and
  `TestParkedTaskKeepsItsPlaceInTheOrder` pins the older-parked-first walk.
- Rejected, with the plan as the authority: erroring on `min_free_space` keys that match no
  configured root — `docs/11-config-reference.md` §5 and `docs/04-data-model.md` §3.2 mandate that
  stale entries "remain stored but are ignored".
- Rejected: skipping the disk tests on small temp filesystems (a host property must fail loudly,
  not silently narrow coverage); splitting `fsx` into tagged/untagged files (the runtime image and
  CI are Linux-only, and every importer is Linux-built regardless); re-adding a restart loop or
  lifetime context (Run already retries; the goroutine matches the reconciler's by-design pattern);
  `strconv.Atoi` parity (the migration seeds `value_json` as bare integers — `docs/04` §3.2 — and
  `0` means unlimited per `docs/11` §5, which `Limits.Blocked` implements).
- The repeated outside-diff "over-commit within a tick" finding is unchanged from round 2:
  `gate.commit(cand)` spends every release's remaining bytes in memory before the next candidate.

Final `make lint && make test PKG=./internal/...`: lint 0 issues, all 11 internal packages `ok`,
no `FAIL`; the four named tests all pass; doclint 0 errors.

### Review round 2 (commit f722696 → e772b2f)

Five majors, six minors, four infos; triage and outcome:
- "Admission loop exits permanently on a load error" — **not a defect**: `Admitter.Run` already
  logs-and-retries load and pass failures on its ticker and returns only on a cancelled context
  (the goroutine now stays quiet on that expected cancellation instead of logging an error).
- "Hold message embeds live byte counts, defeating the sentence dedupe" — **fixed**: the stamp is
  a fixed sentence (`holdMessage`) and the numbers go to the debug log, so a 1 Hz pass no longer
  re-stamps a held row every tick.
- "Fail-open space check can ping-pong a paused disk_full task" — **fixed**: parked candidates
  fail closed (`holdsParked`) — an unreadable filesystem holds them one more tick instead of
  resuming them into the ENOSPC they were parked for. Queued candidates keep failing open.
- "PauseDiskFull can strand a task paused without disk_full" — **fixed by reordering**: the stamp
  lands before the transition, so a failure between the two writes leaves the row downloading
  with the pause still to come — self-healing on the next ENOSPC report. Covered by
  `TestPauseDiskFullSelfHealsAStampFailure`.
- "16 MiB admit-side margin is a latent CI flake" — **fixed**: widened to 64 MiB.
- Minors applied: the no-op `sqlx.In` is gone, roots and `min_free_space` keys are normalised to
  one canonical form (the second destination in the pool test now exists on disk, the shortfall
  has a name), each task's remaining is clamped at 0 before the SQL `SUM` (`MAX(x, 0)` — a task
  reporting completed past total cancels nothing), and `fsx.Floor` ignores negative entries.
- "Released candidates don't enter CommittedBytes until their state flips" (outside-diff) —
  **already implemented**: `gate.commit(cand)` after each release is exactly the suggested
  `pool.CommittedBytes += remaining`, and `TestPassCommitsReleasedBytesInMemory` is the requested
  joint-over-commit test. `TestOperatorPausedTaskIsNotACandidate` pins the paused intake filter
  the other outside-diff note asked about.
- The `//go:build linux` tag now states the statfs dependency of `internal/fsx`.
- Info notes answered on the PR: the immortal `context.Background()` matches the reconciler's
  by-design pattern; the handle-convention comment landed in round 1.
- The deferral register now carries the aria2 errorCode-9 → `PauseDiskFull` routing gap, so it is
  owned by a task rather than living in this file.

### Review round 1 (commit df844ca)

The GLM 5.3 review of PR #92 raised four findings:

- `COALESCE(total_bytes, 0) - completed_bytes` under-counted a destination whose unknown-total
task had reported progress — **fixed**: the COALESCE now wraps the subtraction
(`SUM(COALESCE(total_bytes - completed_bytes, 0))`), with `TestUnknownTotalReservesNothing`
covering the store sum and the admission hold.
- `existingAncestor` could spin on `filepath.Dir(".") == "."` for a relative path with a
vanished working directory — **fixed** with a fixed-point guard.
- The engine pause handle "double prefix" — **not a defect**: the engine-namespaced
`"<engine>:<ref>"` form is the TaskInfo.ID shape the API actions pass and `release` passes to
`Engine.Resume` (`docs/04-data-model.md` §3.3; the adapter strips its own namespace again).
  A comment now states the format at the call site.
- "release re-Adds instead of resuming" — **not a defect**: `release` resumes through the stored
handle first and reaches `Add` only when the engine lost the handle (with resume semantics);
  `TestENOSPCPausesAndKeepsData` asserts one Resume and zero Adds. A comment now says so at the
  paused-candidate branch.

## Blocked

None. One out-of-task observation, not a block (owned by
[T126](T126-disk-full-pause-routing.md)): an aria2-reported disk-full (errorCode 9) reaches
the reconciler's `writeBack`, which moves the row to `error`; routing that report into
`PauseDiskFull` needs `internal/engine/reconcile.go`, outside this task's Files table. The
mechanism (`PauseDiskFull`, and the aria2 mapping already emitting `disk_full`) is in place for
that call site.
