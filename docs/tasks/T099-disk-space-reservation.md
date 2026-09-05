# T099 — Reserve disk space and keep a free-space floor

| Field | Value |
|---|---|
| **ID** | T099 |
| **Milestone** | M1 |
| **Status** | done |
| **Depends on** | T020, T024, T098 |
| **Blocks** | T047, T076 |
| **Parallel-safe** | no — it also edits the shared files `internal/engine/admission.go`, `internal/engine/admission_test.go`, `internal/store/tasks.go`, `internal/api/server.go` |
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
(`internal/api/server.go`), and the admission cases named in this task's own Steps need to live in the pass's test file.

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
Expected: exactly the paths in the Files table as a set, and nothing else. Pre-commit, use
`git status`, not `git diff`: a file this task creates is untracked, and `git diff --name-only`
never lists an untracked file. Once everything is committed, `git status` is empty by design —
use `git diff --name-only origin/main...HEAD -- . ':(exclude)docs' | sort` instead, the command
the Evidence paste below runs. That equivalence holds only while this branch carries no other
task's code; on a shared branch, diff from the parent of this task's first commit.

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

`make lint && make test PKG=./internal/...` — the unattributed pre-round-9 paste was removed in
round 10 (its commit could not be pinned); the newest round's labeled run below covers the final
state — every earlier paste predates later code changes and is retained as history only.

The four named tests, run verbosely (after review round 9 — history only; predates the shared-pool, vanished-task and unknown-total cases, which no verbose paste below covers):

```
$ go test ./internal/fsx/ ./internal/engine/ -run 'TestAdmitsAccountsForCommittedBytes|TestDefaultFloorIsTwoGiB|TestENOSPCPausesAndKeepsData|TestThirdTaskStaysQueued' -count=1 -v | grep -E '^(=== RUN|--- (PASS|FAIL)|PASS|FAIL|ok)'
=== RUN   TestAdmitsAccountsForCommittedBytes
--- PASS: TestAdmitsAccountsForCommittedBytes (0.00s)
=== RUN   TestDefaultFloorIsTwoGiB
--- PASS: TestDefaultFloorIsTwoGiB (0.00s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/fsx	0.005s
=== RUN   TestThirdTaskStaysQueued
--- PASS: TestThirdTaskStaysQueued (0.11s)
=== RUN   TestENOSPCPausesAndKeepsData
--- PASS: TestENOSPCPausesAndKeepsData (0.06s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/engine	0.186s
```

`make lint && make test PKG=./internal/...` after review round 9 — lint clean, every internal
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
ok  	github.com/L-K-M/dl-tool/internal/api	48.693s
ok  	github.com/L-K-M/dl-tool/internal/config	1.236s
ok  	github.com/L-K-M/dl-tool/internal/engine	15.047s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.182s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.029s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.827s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.189s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.180s
ok  	github.com/L-K-M/dl-tool/internal/store	66.785s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.382s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.045s
```

Scope check — exactly the amended Files table as a set, nothing else (the pasted command sorts).
Everything is committed, so the branch diff names the touched paths:

```
$ git diff --name-only origin/main...HEAD -- . ':(exclude)docs' | sort
internal/api/server.go
internal/engine/admission.go
internal/engine/admission_test.go
internal/fsx/space.go
internal/fsx/space_test.go
internal/store/tasks.go
```

### Review round 18 (commit 0941ada → this one)

Four majors — three verified-false premises, one 6th re-raise — plus a broad applied batch.

**Majors**

- **Operator pause overridden between selection and release (6th raise)** — declined, the
  standing T127 disposition: the mid-pass interlock is T127's specified step 2 (re-check the
  stamp before `Engine.Resume`, STOP if absent), the register names both directions, and no
  production path parks a row until T126 routes the first report. Implementing the interlock
  here would gut the task whose Files table exists to carry it.
- **markReleased rests on an invisible ClearHoldCode guard (6th raise of the guard)** —
  verified for the sixth time: `queryClearTaskErrorCodeUnlessPaused` writes
  `state <> 'paused'` and only the two hold codes, and
  `TestClearHoldCodeNeverWipesAPausedStamp` drives the exact interleaving through the real
  store.
- **SetErrorCodeIfState's clear bypasses the expected-state guard** — premise false: the
  `if errorCode == ""` branch only nils the message; the clear runs through the same
  transaction, the same `state = ?` predicate and the same pair guard as the stamp — one
  query, one path, and the decline-on-mismatch applies to it identically.
- **queryTaskErrorCode's widened projection may break other scanners** — verified: the query
  has exactly one caller (the `SetErrorCodeIfState` transaction read), whose anonymous struct
  scans all three columns.

**Applied (minors + infos)**

- The Evidence preamble is round-proof ("the newest round's labeled run"), the round-13 SQL
  quote includes the `MAX(..., 0)` clamp the shipped query carries, and the round-5
  Parallel-cell claim carries the same retro-annotation round 8 established. The doclint
  pastes of rounds 15–17 now name the grep command that produced them; round 18 pastes a bare
  full run below.
- `parseNonNegativeSettingInt` decodes through `encoding/json`, so the accepted grammar is
  exactly a bare JSON integer — `"4"`, `+4`, `04`, `null` all fail — matching the documented
  contract instead of `strconv`'s wider one.
- The queued-row engine stop logs its ENOSPC cause (the stop is that branch's whole visible
  reaction); fsx errors name the requested destination beside the climbed ancestor
  (`statfs /srv (nearest existing ancestor of /srv/media/newroot)`), so an operator sees the
  path they configured; `Reservation.Admits`' doc no longer overstates the unknown-total pass
  (a floor still holds it).
- `TestENOSPCPausesAndKeepsData` re-asserts the engine pause count after both the holding and
  the resume passes; the vanish tolerance is pinned on the fresh path too
  (`TestPauseDiskFullToleratesAVanishedActiveTask` — the stop is attempted once from the
  snapshot, the vanished landing reports nil, no partial state survives); `FreeSpace` shares
  the fail-closed through-file climb and now says so in the suite.
- The live-statfs margins were widened to 128 MiB on both sides — `TestThirdTaskStaysQueued`
  scales its seeds (400/200/20 MiB) so the shared head-room leaves ≥128 MiB of hold-side and
  release-side tolerance, and `TestTwoDestinationsShareOnePool` widens both floors.

**Declined with evidence**

- **T099 roster Parallel cell flipped `yes`→`no`"silently"** — deliberate and recorded: round
  6's triage landed it ("the roster's T099 Parallel cell reads `no` at last") once the task
  gained shared-file edits; the task file's own Parallel-safe cell lists them.
- **Risk row (9th raise) / T126-T127 links (9th raise)** — the wiring is in the diff and green;
  both files are in this PR.
- **Collapse the per-round lint pastes** — every paste is pinned to the tree it tested;
  replacing older ones with "identical to above" summaries would blur exactly the attribution
  the last three rounds policed. Length is the honest cost.
- **Typo'd `min_free_space` key rejected/warned (4th raise)** — "stored but ignored" is the
  plan's semantics (docs/11 §5); the write-side validation belongs to the settings endpoint.
- **Fail-open blast radius capped per filesystem** — the tradeoff is documented deliberate
  (a queue must not wedge on a filesystem answer); FR-048's pause backstops the burst, and a
  per-fs budget is new semantics for a plan-level decision.
- **`rootOf` TrimRight vs Clean on `//data`** — unreachable: the loader builds `Policy.Roots`
  from `filepath.Clean`ed roots (`admissionPolicyLoader`'s `canonicalRoots`), so the spellings
  the two normalizations disagree on never reach `rootOf`.
- **Two ways to produce ErrTransitionConflict** — verified: `errTransitionConflict` wraps the
  sentinel with `%w` (store helper), so both paths satisfy `errors.Is`.
- **doclint evidence for rounds 7–11** — same disposition as round 16: those pastes are
  historical trees; the current gate is pasted below, bare and full.
- **Unknown-size reservation (5th raise) / fsx non-Linux stub (4th raise) / hold-code
  literals via constants (3rd raise)** — standing declines, unchanged.

`make lint && make test PKG=./internal/...` after review round 18 — lint clean, every internal
package `ok`, no `FAIL`; `make doclint`, bare and full:

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
ok  	github.com/L-K-M/dl-tool/internal/api	47.537s
ok  	github.com/L-K-M/dl-tool/internal/config	1.124s
ok  	github.com/L-K-M/dl-tool/internal/engine	15.827s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.155s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.031s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.635s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.179s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.114s
ok  	github.com/L-K-M/dl-tool/internal/store	69.247s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.368s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.045s
```

```
$ make doclint
./scripts/doclint.sh
🔍 2360 Total (in 219ms) 🔗 552 Unique ✅ 2346 OK 🚫 0 Errors 👻 14 Excluded
```

### Review round 17 (commit d6e7e81 → this one)

Three majors — one verify-ask answered, two standing declines — plus a broad applied batch.

**Majors**

- **Space-gate failure policy stated both fail-open and fail-closed** — both comments were true
  and the implementations confirmed it: `holds` fails open for queued candidates (`!ok` → not
  held), `holdsParked` fails closed (`!ok` → held one more tick, logged at debug). The gate
  comment now states the split explicitly so the two sentences cannot be read as contradicting.
- **Admission pass resumes operator-paused tasks (5th raise)** — declined, the standing T127
  disposition: the paused branch's own comment names the attribution limit and T127's takeover,
  the register row names both directions, and the TODO the finding asks for already exists as
  that comment. No production path parks a row until T126.
- **Disk-space tests on live statfs; inject a space source** — declined, standing: the fourth
  `NewAdmitter` argument is the logger, not a space source — there is no injection seam, and
  adding one is a plan-level design change to fsx's static surface. The margins were hardened
  where free (rounds 14–16) and the fatal-on-undersized-tmpfs stance is the documented
  no-skip-rule tradeoff, declined five times.

**Applied (minors + infos)**

- The Parallel-safe cell lists `internal/engine/admission_test.go` among the shared files it
  edits; the round-13 parenthetical is a plain sentence (no dangling brackets); the Evidence
  preamble names which runs cover the final state; round 13's intro records that round 14's
  full-PR pass picked up the seven failed chunks.
- A statfs failure is cached per filesystem per pass (`spaceGate.unreadable`), matching the
  read-once-per-pass contract `spaces` documents — a broken mount no longer costs one failing
  statfs per candidate per tick.
- The repeat ENOSPC report logs its cause in the already-parked branch too, same message as
  the fresh path; every failing destination after the first identification warn is visible at
  debug (`filesystemOf`).
- `PauseWithCode` checks `FromStates` before `transitionLegal`: a row raced out of the
  allow-list is `ErrTransitionConflict` whatever it raced to, and `ErrIllegalTransition` fires
  only when the caller allowed a state the machine refuses to pause from.
- `FreeSpace` rejects a negative `f_frsize` (signed field, FUSE-controlled answer) instead of
  handing back negative byte counts that bypass the overflow guard.
- The loader comment no longer claims "never a wedged queue": a persistently malformed row
  fails every tick's load and admits nothing until fixed — deliberate fail-closed, now said so.
- `SelectQueuedCandidates`' interface comment states that the paused rows are meant to be
  guard pauses only and the operator pause must clear the stamp (T127).
- `admitEngine.Remove` records its calls, and `TestENOSPCPausesAndKeepsData` pins zero removes
  beside its byte-for-byte file check — the "unlinks nothing" guarantee is now observable.
  The same test pins still-exactly-one pause event after the resume pass.
- `TestUnknownTotalReservesNothing` re-sums the commitment after the seeding transition and
  fails on the store math directly; the vanished-task test seeds a handle and pins exactly the
  one snapshot-driven engine stop; the three wrapped-store tests share one `admitterOver`
  helper.

**Declined with evidence**

- **T126/T127 links (8th raise) / risk row (8th raise)** — both files are in this PR; the
  wiring is in the diff and green.
- **Round-10 evidence lacks doclint** — doclint is pasted from round 14 onward, including this
  round's block below; pasting a HEAD run into the round-10 block would misattribute the tree.
- **`Policy.Floor` accessor / `Policy.clean` normalization (2nd raise each)** — the only
  sanctioned reader is `fsx.Floor`'s comma-ok lookup and the loader canonicalises keys (rounds
  8, 11, 12); a second owner for either invariant is the drift the findings warn about.
- **Slot-blocked parked tasks at Warn (2nd raise)** — round 12 set it to Debug for churn
  discipline; the comment now also names the orphaning constraint that forbids re-stamping.
- **~86k events/day needs a task row** — a backoff ladder or cycle counter is a plan-level
  decision awaiting the owner (the T088 class); inventing a task row here would decide it.
  T126's bullet names the magnitude, the served-event-log signal and the pending decision.
- **Operator-pause stopgap (outside diff)** — the stopgap lives in the action layer, outside
  this task's Files table; T127 is it.
- **`CountActive` must exclude paused** — verified: the query counts exactly the four active
  states, paused not among them.
- **Non-Unix build tags** — runtime and CI are Linux-only per the plan; the `fsx` build tag
  states the dependency.
- **`SetErrorCodeIfState`'s clear bypasses the guard** — false premise: the clear runs through
  the same transaction, the same state predicate and the same pair guard as the set; there is
  one query and one path.

`make lint && make test PKG=./internal/...` after review round 17 — lint clean, every internal
package `ok`, no `FAIL`; `make doclint` 0 errors:

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
ok  	github.com/L-K-M/dl-tool/internal/api	46.418s
ok  	github.com/L-K-M/dl-tool/internal/config	1.131s
ok  	github.com/L-K-M/dl-tool/internal/engine	15.405s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.149s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.029s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.457s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.171s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.122s
ok  	github.com/L-K-M/dl-tool/internal/store	68.911s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.375s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.037s
```

```
$ make doclint 2>&1 | grep Total
🔍 2360 Total (in 207ms) 🔗 552 Unique ✅ 2346 OK 🚫 0 Errors 👻 14 Excluded
```

### Review round 16 (commit 48508f0 → this one)

One evidence misstatement of my own, one ordering fix, two spec hardenings; the standing races
stayed declined with the register already naming both directions.

**Majors**

- **Round-15 evidence header claimed a run the pasted pattern cannot produce** — real, my error:
  the header said the state-allow-list ran with ten subtests and six PASS lines, but the pattern
  omitted it. Fixed the honest way: the command was re-run with the widened pattern and the
  genuine output pasted (eight top-level tests, ten subtests), with a note naming the
  replacement. No fabricated lines.
- **Routed `PauseDiskFull` will attempt an engine pause on a download that already errored** —
  premise half-wrong (`pauseEngineSide` is best-effort by design: a failure is a warn and the
  store pause lands regardless, so the routed call cannot fall back to error adoption), but the
  production-only rejection is real and the fake engine cannot catch it. Applied to T126: step 2
  names the rejection, and a new acceptance criterion requires a fake-engine `Pause` failure
  with the row still landing `paused` + the stamp.
- **Accepted 1 Hz resume→pause loop writes ~86k events/day with no owner** — applied to T126's
  out-of-scope bullet: the magnitude is named, the detection signal is named (each landing is a
  served `task.paused` event — T024's log), and backoff or a cycle counter is recorded as a
  plan-level decision. A deferral-register row was not added: the register needs a carrier task
  and none exists — this is an accepted cost, not deferred work.
- **Admission loop has no shutdown path (3rd raise)** — declined, the round-12 stance: the
  process-lifetime context is the design, shared with the hub and the reconciler; the nil-db
  guard keeps test and openapi construction loop-free; a server lifecycle and drain semantics
  are plan-level.
- **Pass auto-resumes operator-paused tasks carrying a stale stamp (4th raise)** — declined,
  the T127 disposition unchanged: the register names both directions, T127's clear runs after
  every successful pause, and no parked row exists in production until T126 routes the first
  report.
- **Already-parked branch skips the engine stop when the re-stamp fails** — premise half-wrong
  (the round-14 compare-and-set declines silently on a state move — nil, not an error — so the
  resume race described still reaches `pauseEngineSide`), but a genuine store failure would
  abort before the engine stop. Applied the reorder: `pauseEngineSide` runs first, like the
  fresh path, so a store failure never leaves the transfer writing.

**Applied (minors + infos)**

- Round 6's duplicate-key "applied" claim is annotated in place — false when written; the guard
  landed in round 8 — matching the file's own convention for corrected claims.
- T127's step 2 and criterion 3 require the stamp re-check *before* `Engine.Resume`, so a clear
  landing mid-pass aborts the engine call and the row write together; the `concurrency_limit`
  allow-list entry is justified in place (the queued branch's other hold stamp; never selected
  once paused; cleared for message accuracy).
- T126's self-qualified `engine.ErrorCodeDiskFull` is unqualified (the constant lives in the
  same package the reconciler edits).
- `NewServer`'s comment states the boot sweep completes synchronously before admission starts.
- `parseNonNegativeSettingInt` documents the bare-number grammar the future writer must share.
- `PauseDiskFull`'s opening `Get` tolerates a vanished task, matching the branches below.
- The dead duplicate error check after `tx.Rebind` (a round-15 leftover) is gone.
- `SetErrorCodeIfState` rejects an empty `expectedState` up front — the twin of
  `PauseWithCode`'s empty-`FromStates` tripwire.
- `PauseWithCode`'s `FromStates` miss now answers `ErrTransitionConflict`, the documented
  raced-write signal, leaving `ErrIllegalTransition` for the genuinely illegal source.
- `ErrDiskFull` no longer says "no space left on device" twice (`fsx: disk full: …`).
- `existingAncestor` fails closed when the nearest existing ancestor is a regular file — a
  destination beneath it can never be created, so it gets an error, never a space promise;
  the pinned through-file test flipped to expect the error.
- `TestThirdTaskStaysQueued` hoists one `headRoom` both passes share; the double-pause
  assertion checks both handles; the parked-order test seeds `CompletedBytes` to match its
  partial-data narration; the parked slot-wait comment names the orphaning constraint that
  keeps the message from being re-stamped.

**Declined with evidence**

- **Risk-row removal (7th re-ask)** — the wiring is in this diff, named in the Files table,
  green in CI.
- **Reject or warn on floor keys matching no root (3rd raise)** — "stored but ignored" is the
  plan's semantics (docs/11 §5, docs/04 §3.2); a 1 Hz warn repeats forever on a benign value;
  write-side validation belongs to the settings endpoint (T092).
- **Unknown-size reservation (3rd raise)** — FR-047's NULL-total rule is the pinned contract.
- **MinFree spelling normalization inside the gate** — the loader canonicalises keys and errors
  on conflicting duplicates (rounds 8 and 12) and the invariant is documented at `rootOf`;
  a second normalization in the gate would give the invariant two owners.
- **Queued-report re-release churn** — closed by verification: the queued branch runs the space
  gate before release, and on a full disk the floor holds the row queued, so the cycle
  converges instead of repeating.
- **`SelectQueuedCandidates` rename (4th raise)** — the store's test file is outside this
  task's Files table; the method's doc comment carries the queued-or-paused contract.
- **T126/T127 links resolve** — both files are in this PR (verified each raise since round 10).
- **`floorLeaving` skip (5th raise)** — the no-skip rule stands.
- **Hold-code literals via `fmt.Sprintf` at var-init** — declined: the literals are pinned to
  the Go constants by the admission tests through the real store, and the comment beside the
  SQL keeps the pairing visible where the query lives.
- **Candidate-struct doc stale after COALESCE** — verified current: round 10 rewrote it to
  "reads `COALESCE(completed_bytes, 0)`".
- **`release` must clear hold stamps** — verified: `markReleased` transitions then calls
  `ClearHoldCode`; pinned by `TestThirdTaskStaysQueued`'s cleared-on-release assertion.

`make lint && make test PKG=./internal/...` after review round 16 — lint clean (the pasted
output), every internal package `ok`, no `FAIL`; `make doclint` 0 errors:

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
ok  	github.com/L-K-M/dl-tool/internal/api	47.747s
ok  	github.com/L-K-M/dl-tool/internal/config	1.232s
ok  	github.com/L-K-M/dl-tool/internal/engine	15.930s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.193s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.035s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.908s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.204s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.206s
ok  	github.com/L-K-M/dl-tool/internal/store	66.663s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.377s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.022s
```

```
$ make doclint 2>&1 | grep Total
🔍 2360 Total (in 204ms) 🔗 552 Unique ✅ 2346 OK 🚫 0 Errors 👻 14 Excluded
```

### Review round 15 (commit 1651c67 → this one)

Two majors re-raising the T127-carried race, declined with the register widened; the rest below.

**Majors**

- **Starting the admission loop ships the trigger for the T127 race** — declined, the standing
  disposition of rounds 10–12: conventions §8.3 forbids an unwired constructor, the deferral
  register assigns T099 the `Admitter.Run` wiring, and landing T127 inside this PR would need
  `internal/api/tasks_actions.go`, outside T099's Files table (one task, one PR). No production
  path parks a row until T126 routes the first engine disk-full report — `PauseDiskFull`'s only
  callers are tests — so the exposure opens at T126's merge, not this one, and T127 is unblocked
  the moment this PR merges. The provenance marker was declined in round 10 (schema change); the
  `NewReconciler` hoist note is T126's own file edit. The register row now names both directions
  of the stamp-keeping hole and T127's Goal and criteria name the queued-stamp variant round 15
  identified (the round-14 compare-and-set closed only the stamp-after-pause direction; the
  pause-after-mint direction — a queued row carrying a hold — is T127's clear).
- **Operator pause on a queued disk_full-held task is silently auto-resumed** — the same hole,
  same disposition: the queued+stamped row paused by an operator becomes `paused + disk_full`,
  and the pass would resume it. T127's clear runs after every successful pause — fresh, queued
  or the idempotent already-paused branch — so its mechanism already covers this direction; the
  spec now says so (Goal and the new criterion), instead of leaving the variant unnamed.

**Applied (minors + infos)**

- `PauseWithCode`'s error is checked before `tx.Rebind` consumes the query — the round-14 edit
  had read the result of a possibly-failed `sqlx.In`.
- `PauseDiskFull`'s fresh-pause path tolerates a vanished task (`ErrNotFound` → nil), the same
  vanish-tolerance the already-parked branch documents two cases above.
- A disk-full report on a queued row — the release window, where the pass has submitted or
  resumed the transfer engine-side before the row's transition lands — now stops the engine
  transfer even though the row-level pause is refused; pinned in the state-allow-list's queued
  subtest, which also pins every other refused state recording no pause.
- `TestPauseDiskFullStateAllowList` asserts the engine-side stop for every allowed active state;
  `TestPauseDiskFullFailureLeavesTheRowUntouched` asserts the retry stopped the transfer (the
  failed attempt's stop plus the retry's).
- `TestThirdTaskStaysQueued` re-reads free space before its second pass, so the release-side
  margin is measured from now, not from before the first pass.
- `FilesystemID`'s doc states the real stability contract (one process run — anonymous device
  numbers are remounted) and the real btrfs direction: two subvolumes' pools each count the same
  shared free bytes and can jointly over-admit until ENOSPC, not "under-promise consolidation".
- The ENOTDIR climb documents that a file-in-path destination can never be created and callers
  must reject it before treating the answer as an admission.
- `TestSpaceRejectsRelativePaths` pins the round-14 relative-path guard.
- The round-14 evidence pastes `make doclint` instead of asserting it; round 13's heading is
  pinned to `9c18a18`; rounds 6, 7 and 9's tallies match their enumerated findings (2 majors +
  11 minors, 10, 4); the scope-check Expected carries the single-task-branch caveat.

**Declined with evidence**

- **T127 should depend on T126** — the edge would force the fix to land after the trigger that
  makes the race live, widening the exposure it purports to close; T127's acceptance seeds the
  parked row by calling `Admitter.PauseDiskFull` directly (step 3), so it is implementable and
  not vacuous without T126, and leaving it unblocked lets it land before T126 ever ships the
  trigger.
- **Warn on `min_free_space` keys no configured root matches** — `docs/11` §5 and `docs/04` §3.2
  make "stored but ignored" the plan's semantics (rounds 6–7); a per-tick warn repeats at 1 Hz
  forever for a benign stored value, and the durable validation belongs to the settings write
  path (T092, M6).
- **Reserve bytes for unknown-size tasks** — FR-047's NULL-total rule is the task's pinned
  interface contract (`remainingBytes` returns 0, re-checked when metadata resolves); a
  placeholder allowance is an owner-level design decision (round-11 decline, unchanged).
- **`floorLeaving` skip (3rd re-raise)** — the no-skip rule stands.
- **Ticker risk-row removal (6th re-ask)** — the wiring is in this diff and green; no restoration.
- **Fail-open budget on persistently unreadable mounts** — the fail-open stance is documented
  deliberate (a queue must not wedge on a filesystem answer); an escalation ladder is
  plan-level.
- **`_txlock=immediate`** — already set: the store DSN carries
  `_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&...&_txlock=immediate`
  (`internal/store/db.go`, `databaseDSNFormat`), so every transaction takes the write lock up
  front and the deferred-upgrade window cannot occur.
- **`ClearHoldCode` paused-row guard (4th re-ask)** — the guard and its test are in this diff.
- **Release of parked candidates space-gated + `paused→downloading` legal** — both verified by
  the suite: `TestENOSPCPausesAndKeepsData` holds the parked row below the floor and releases it
  through the real store once room returns; the transition's legality is pinned by the same
  assertions.

`make lint && make test PKG=./internal/...` after review round 15 — lint clean (the pasted
output), every internal package `ok`, no `FAIL`; `make doclint` 0 errors:

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
ok  	github.com/L-K-M/dl-tool/internal/api	44.984s
ok  	github.com/L-K-M/dl-tool/internal/config	1.246s
ok  	github.com/L-K-M/dl-tool/internal/engine	14.676s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.190s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.036s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.279s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.180s
ok  	github.com/L-K-M/dl-tool/internal/secure	3.995s
ok  	github.com/L-K-M/dl-tool/internal/store	63.979s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.365s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.044s
```

```
$ make doclint 2>&1 | grep Total
🔍 2360 Total (in 211ms) 🔗 552 Unique ✅ 2346 OK 🚫 0 Errors 👻 14 Excluded
```

The named tests plus the round-14 and round-15 regressions, run verbosely after the round-15
fixes — the round-15 edit first pasted a narrower pattern whose header overclaimed this run;
round 16 replaced both with this widened, genuine run (eight top-level tests, the state-allow-
list's ten subtests among them):

```
$ go test ./internal/fsx/ ./internal/engine/ -run 'TestAdmitsAccountsForCommittedBytes|TestDefaultFloorIsTwoGiB|TestENOSPCPausesAndKeepsData|TestThirdTaskStaysQueued|TestHoldStampDeclinesOnAnOperatorPauseMidPass|TestPauseDiskFullStateAllowList|TestPauseDiskFullFailureLeavesTheRowUntouched|TestSpaceRejectsRelativePaths' -count=1 -v | grep -E '^(=== RUN   Test[A-Z]|--- (PASS|FAIL): Test[A-Z]|PASS|FAIL|ok)'
=== RUN   TestAdmitsAccountsForCommittedBytes
--- PASS: TestAdmitsAccountsForCommittedBytes (0.00s)
=== RUN   TestDefaultFloorIsTwoGiB
--- PASS: TestDefaultFloorIsTwoGiB (0.00s)
=== RUN   TestSpaceRejectsRelativePaths
--- PASS: TestSpaceRejectsRelativePaths (0.00s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/fsx	0.006s
=== RUN   TestThirdTaskStaysQueued
--- PASS: TestThirdTaskStaysQueued (0.11s)
=== RUN   TestENOSPCPausesAndKeepsData
--- PASS: TestENOSPCPausesAndKeepsData (0.07s)
=== RUN   TestPauseDiskFullFailureLeavesTheRowUntouched
--- PASS: TestPauseDiskFullFailureLeavesTheRowUntouched (0.04s)
=== RUN   TestHoldStampDeclinesOnAnOperatorPauseMidPass
--- PASS: TestHoldStampDeclinesOnAnOperatorPauseMidPass (0.06s)
=== RUN   TestPauseDiskFullStateAllowList
=== RUN   TestPauseDiskFullStateAllowList/checking
=== RUN   TestPauseDiskFullStateAllowList/completed
=== RUN   TestPauseDiskFullStateAllowList/downloading
=== RUN   TestPauseDiskFullStateAllowList/error
=== RUN   TestPauseDiskFullStateAllowList/extracting
=== RUN   TestPauseDiskFullStateAllowList/moving
=== RUN   TestPauseDiskFullStateAllowList/paused
=== RUN   TestPauseDiskFullStateAllowList/queued
=== RUN   TestPauseDiskFullStateAllowList/removed
=== RUN   TestPauseDiskFullStateAllowList/seeding
--- PASS: TestPauseDiskFullStateAllowList (0.59s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/engine	0.892s
```

### Review round 14 (commit 9c18a18 → this one)

One real bug, two majors declined on verified premises, minors and infos applied as below.

**Majors**

- **Hold stamp can mint an auto-resume token on a task the operator just paused** — real, applied.
  `stampHeld` wrote through `SetErrorCode`, which guards the (code, message) pair but not the
  state, so a queued candidate the operator paused between `SelectQueuedCandidates` and the
  stamp became `paused + disk_full` — exactly the membership token the candidate query resumes
  guard-parked tasks by. The store method is now `SetErrorCodeIfState(ctx, id, expectedState, …)`:
  a compare-and-set whose declined write reports success (a 1 Hz pass racing an operator action
  must neither warn per tick nor retry). `stampHeld` passes the candidate snapshot's state, so
  both hold codes route through the guard, and `PauseDiskFull`'s already-parked refresh passes
  `paused` for the same reason. `TestHoldStampDeclinesOnAnOperatorPauseMidPass` pins it — an
  `operatorPauseStore` wrapper lands the operator pause at selection time, the stamp declines,
  and the next pass with room and a free slot releases nothing; the test was verified to fail
  against the unguarded write.
- **Admission loop halts permanently on a single `Run` error** — premise false: `Admitter.Run`
  returns only `ctx.Err()` on cancellation; a failing `load` and a failing `Pass` are logged and
  retried inside the loop on the next tick, so no retryable cause escapes and the restart
  wrapper would supervise a loop that never exits while the process lives. The `/healthz`
  liveness surface stays the round-12 decline: a server-lifetime context and a degraded-health
  signal are plan-level additions shared with the hub and the reconciler.
- **T126's Parallel-safe cell contradicts its Files table** — applied: the cell names the
  one-line `internal/api/server.go` constructor edit the round-12 Files-table amendment added.

**Applied (minors + infos)**

- Round-13 preamble fixed: the two completed chunks produced the five triaged findings; the
  sentence now counts chunks, not findings.
- Round-13's lint half is pasted, not asserted — re-run at 9c18a18, verbatim in that round's
  block below.
- The scope-check Expected names the post-commit state: once everything is committed,
  `git status` is empty by design and the branch diff is the command.
- T126's Verification runs `make test PKG=./internal/...` — its Files table edits
  `internal/api/server.go`, which `./internal/engine/...` neither compiles nor tests.
- T126's out-of-scope bullet names the accepted cost of the resume→pause cycle: one
  `task_events` row per pause landing, at the pass rate.
- T127's third acceptance criterion carries its SQL predicate in one code span — the split
  backtick pair rendered garbled.
- `NewServer` builds the SPA handler before the background loops start, so a construction
  failure returns without leaking a reconciler or admitter loop nobody can stop; the mount
  stays last. The shutdown-context half remains the round-12 decline.
- `diskFullPauseSources` is a function returning a fresh slice: the shared state list can no
  longer be mutated from under the read-side switch and the atomic write.
- `PauseWithCode` rebinds the `sqlx.In` result (`tx.Rebind`) — the canonical pairing; SQLite's
  `?` bindvar makes it an identity today.
- `TestENOSPCPausesAndKeepsData`'s full-disk floor is 1 GiB above the free answer: a hold-side
  margin costs nothing and no realistic concurrent free on the shared temp filesystem
  outpaces it. `TestThirdTaskStaysQueued`'s `shortfall` stays 50 MiB: the same policy is reused
  for the release pass, where `free - floor - remaining` is `committed - shortfall` — a
  GiB-scale shortfall would hold the third task forever and break the test's second half.
- `existingAncestor` rejects a relative path up front instead of pooling it against the
  working directory's mount; the fixed-point-guard sentence is gone with the input it described.
- `namespacedHandle`'s comment no longer claims the API actions share the helper — they render
  the same shape at their own call site (`internal/api/tasks_actions.go`, outside this task's
  Files table); routing them through one home belongs to the task that owns that file (T127).

**Declined with evidence**

- **T099 Files table / done flip (5th re-ask)** — verified again: the Files table lists
  `internal/api/server.go` with the `load` closure and `Admitter.Run` wiring, the diff carries
  it, CI is green. Nothing to restore.
- **T126/T127 task docs missing** — both files exist in this PR (added in round 10) with the
  dependency lists their index rows carry.
- **Stale slot-wait message** — a second fixed sentence can alternate with `diskFullMessage`
  per tick while space flaps around the floor (`holdsParked` re-stamps on the holding tick, the
  slot-blocked tick would write the other sentence), reintroducing exactly the per-tick write
  churn the fixed-sentence invariant on `holdMessage` exists to prevent. The real reason is the
  Debug log beside it.
- **`floorLeaving` skip** — the repo's no-skip rule stands: an undersized temp filesystem is an
  environment failure that must fail loudly, not a silent `Skipf`.
- **`FreeSpace` clamping an informational `TotalBytes`** — round 13 made an overflowing statfs
  answer an error on purpose: never a wrapped or clamped guess. `Space.TotalBytes` has no
  decision-bearing consumer; the fail-closed trade-off is the comment's documented intent.
- **Same-tick over-commit "comment-only"** — the invariant is pinned by
  `TestPassCommitsReleasedBytesInMemory` through the real pass (round 12); the comment states
  what that test enforces.
- **`space_test.go` build tag gating portable tests** — the package itself is
  `//go:build linux` (`space.go`), so an untagged test file would not compile elsewhere at all;
  runtime and CI are Linux-only per docs/10.
- **`ClearHoldCode` paused-row guard (3rd re-ask)** — the guard is in this PR's store diff
  (`queryClearTaskErrorCodeUnlessPaused`, `state <> 'paused'`, hold codes only) and
  `TestClearHoldCodeNeverWipesAPausedStamp` covers the pause-vs-clear race through the real
  store; the illustrative diff's `hold_code` columns do not exist.
- **Extract/move worker checkpoint** — no extraction or move pipeline exists in M1 (T074 and
  T076 are `todo`); the task that builds a writer owns its checkpointing.
- **`Floor` max-wins overriding an explicit 0** — unreachable through the loader: it
  canonicalises keys with `filepath.Clean` and errors on conflicting duplicates that clean to
  the same root (round 8). Max-wins remains the documented fail-safe for hand-edited rows only.

`make lint && make test PKG=./internal/...` after review round 14 — lint clean (the pasted
output), every internal package `ok`, no `FAIL`; `make doclint`:

```
$ make doclint
./scripts/doclint.sh
🔍 2360 Total (in 208ms) 🔗 552 Unique ✅ 2346 OK 🚫 0 Errors 👻 14 Excluded
```

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
ok  	github.com/L-K-M/dl-tool/internal/api	49.824s
ok  	github.com/L-K-M/dl-tool/internal/config	1.285s
ok  	github.com/L-K-M/dl-tool/internal/engine	16.161s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.237s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.027s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.787s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.198s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.189s
ok  	github.com/L-K-M/dl-tool/internal/store	66.361s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.377s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.038s
```

The named tests plus the round-14 regression, run verbosely after the round-14 fixes:

```
$ go test ./internal/fsx/ ./internal/engine/ -run 'TestAdmitsAccountsForCommittedBytes|TestDefaultFloorIsTwoGiB|TestENOSPCPausesAndKeepsData|TestThirdTaskStaysQueued|TestHoldStampDeclinesOnAnOperatorPauseMidPass' -count=1 -v | grep -E '^(=== RUN|--- (PASS|FAIL)|PASS|FAIL|ok)'
=== RUN   TestAdmitsAccountsForCommittedBytes
--- PASS: TestAdmitsAccountsForCommittedBytes (0.00s)
=== RUN   TestDefaultFloorIsTwoGiB
--- PASS: TestDefaultFloorIsTwoGiB (0.00s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/fsx	0.005s
=== RUN   TestThirdTaskStaysQueued
--- PASS: TestThirdTaskStaysQueued (0.15s)
=== RUN   TestENOSPCPausesAndKeepsData
--- PASS: TestENOSPCPausesAndKeepsData (0.06s)
=== RUN   TestHoldStampDeclinesOnAnOperatorPauseMidPass
--- PASS: TestHoldStampDeclinesOnAnOperatorPauseMidPass (0.04s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/engine	0.271s
```

### Review round 13 (commit 7fe909e → 9c18a18)

The review run was incomplete — seven of nine chunks failed provider-side — so this triage covers
the findings its two completed chunks produced; the rest of the diff is unreviewed this round.
The regions those seven chunks would have covered were picked up by the round-14 pass, which
reviewed the full PR.

- **Major — NULL `completed_bytes` zeroing a known-size reservation** — the premise is half-wrong
  (the column is `NOT NULL DEFAULT 0` in migration 00001, so the window cannot occur), but the
  same belt the candidate scan got in round 10 now covers the sum too:
  `MAX(COALESCE(total_bytes, 0) - COALESCE(completed_bytes, 0), 0)`. Strictly more conservative —
  a NULL total still contributes 0 and completed-past-total still clamps at 0 — and immune to
  schema drift either way. The requested NULL-insert test cannot exist: the schema rejects it.
- Applied (minors): `FreeSpace` detects the int64 wrap instead of trusting a comment — the old
  "wraps negative, fails closed" claim was wrong (Go wraps modulo 2^64 and can land positive),
  and an overflowing statfs answer is now an error; `ClearHoldCode`'s guarded write and its
  existence probe are one transaction, so a row that moves or vanishes between them cannot
  misattribute the outcome.
- Declined: the `SelectQueuedCandidates` rename (cosmetic, and the store's test file is outside
  this task's Files table; the limit semantics the finding raises are already documented on the
  method — the pass, the only caller, asks for every candidate); the fsx non-Linux CI note (CI
  runs the suite on Linux — the evidence pastes show `ok .../internal/fsx` — and the build tag
  states the statfs dependency deliberately).

`make lint && make test PKG=./internal/...` after review round 13 — lint clean, every internal
package `ok`, no `FAIL`:

```
$ make lint && make test PKG=./internal/...
go test -race -count=1 ./internal/...
ok  	github.com/L-K-M/dl-tool/internal/api	46.978s
ok  	github.com/L-K-M/dl-tool/internal/config	1.126s
ok  	github.com/L-K-M/dl-tool/internal/engine	14.563s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.163s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.037s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.465s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.176s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.033s
ok  	github.com/L-K-M/dl-tool/internal/store	64.342s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.366s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.035s
```

`make lint` was re-run at commit 9c18a18 — the tree these fixes shipped in — and pasted in full:

```
$ make lint
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
```

The four named tests, run verbosely after the round-13 fixes:

```
$ go test ./internal/fsx/ ./internal/engine/ -run 'TestAdmitsAccountsForCommittedBytes|TestDefaultFloorIsTwoGiB|TestENOSPCPausesAndKeepsData|TestThirdTaskStaysQueued' -count=1 -v | grep -E '^(=== RUN|--- (PASS|FAIL)|PASS|FAIL|ok)'
=== RUN   TestAdmitsAccountsForCommittedBytes
--- PASS: TestAdmitsAccountsForCommittedBytes (0.00s)
=== RUN   TestDefaultFloorIsTwoGiB
--- PASS: TestDefaultFloorIsTwoGiB (0.00s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/fsx	0.005s
=== RUN   TestThirdTaskStaysQueued
--- PASS: TestThirdTaskStaysQueued (0.11s)
=== RUN   TestENOSPCPausesAndKeepsData
--- PASS: TestENOSPCPausesAndKeepsData (0.07s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/engine	0.202s
```

### Review round 12 (commit 6bad003 → 7fe909e)

Eight majors, fourteen minors, five infos, one outside-diff note; triage and outcome:

- **Major — the removed M1-exit gap row (re-asked a third time)** — declined, verified:
  `internal/api/server.go` constructs `engine.NewAdmitter` and starts `admitter.Run` beside the
  reconciler with the `admissionPolicyLoader` closure over the settings rows; T099's Files table
  carries `internal/api/server.go` for exactly that. The reviewer's own prompt ends in "if
  present, confirm no change is needed" — it is present.
- **Major — T126 assumes the reconciler can hold the Admitter** — the import-cycle premise is
  false (`admission.go` is `package engine`; the reconciler and the admitter are one package,
  and `engine_test` drives a real Admitter in `admission_test.go` today), but the wiring gap is
  real: `NewReconciler` has no `Admitter` field. T126's step 2 now says so, passes the engine's
  error instead of `nil`, and its Files table gained `internal/api/server.go` (the constructor
  call site).
- **Major — T127's clear does not interlock with an in-flight pass** — valid task-spec gap:
  T127's step 2 now requires verifying the pass's resume of a parked candidate re-checks the
  stamp after selection (and says STOP if it does not), with a matching acceptance criterion.
- **Major — admission halt leaves a healthy-looking server** — declined beyond the round-10 log
  line: every long-lived loop in `server.go` (hub, reconciler, admitter) runs on the process
  lifetime `context.Background()` by the file's own documented pattern — there is no server
  lifetime context to wire to, and a degraded-health surface or restart policy is a plan-level
  addition, not this task's.
- **Major — auto-resume of operator-paused stamped rows (re-asked)** — declined: the stamp
  attribution and its limit are documented at the branch, T127 carries the takeover, and the
  suggested interim message-text guard would create a second, subtler attribution channel
  without fixing the reported case (a guard-parked row's message is `diskFullMessage` either
  way).
- **Major — `markReleased`'s paused guard not visible in the chunk (re-asked)** — declined: the
  guard is in this PR's own store diff (`queryClearTaskErrorCodeUnlessPaused`, `state <>
  'paused'`), with `TestClearHoldCodeNeverWipesAPausedStamp` covering the race through the real
  store.
- **Major — `clearStaleStamp` wiping a paused row's stamp (re-asked)** — declined: the same SQL
  guard makes the clear a deterministic no-op on paused rows; the test pins it.
- **Major — the pool omits same-tick releases** — verification ask: the pass does accumulate
  (`gate.commit` spends each release's remaining bytes in memory;
  `TestPassCommitsReleasedBytesInMemory` pins it), and the invariant is now documented on
  `querySumRemainingByDestination` itself.
- Applied (minors + infos): `fsx.Floor` picks the strictest of duplicate-cleaning keys
  deterministically (with order-swapped tests); the slot-wait log is back at Debug (a saturated
  queue would otherwise log it every tick per task — the reviewer's churn point beats round 11's
  reasoning); the duplicated engine-pause block is one `pauseEngineSide` helper;
  `filesystemOf` caches the failure (empty sentinel) as well as the id; `PauseWithCode` rejects
  an empty `EventCode`; the loader skips an empty root before `filepath.Clean` can mint `"."`;
  `TestENOSPCPausesAndKeepsData`'s event counting is one closure; `TestThirdTaskStaysQueued`
  pins the engine-side `Add` once the hold lifts; the `mib` comment names cross-package TMPDIR
  traffic as the margins' rationale; T127 step 2 logs the takeover at info; round 10's heading
  is pinned to `2fde1fa`; round 11's itemization now labels the seventh major explicitly; the
  index's T126 row drops the stale "Blocked section" pointer.
- Declined: `strconv.Atoi` for the integer settings (both readers — the loader and
  `loadConcurrencySnapshot` — use it identically, and no write path exists yet to produce a
  JSON-quoted form; switching one reader would create the asymmetry the finding fears); the
  `-short`-gated skip in `floorLeaving` (nothing runs `-short`, and the fatal-on-undersized-tmpfs
  stance is documented deliberate); per-key settings degradation (fail-closed is the loader's
  documented design; changing it is plan-level); the docs/05 note on slot-blocked `disk_full`
  (docs/05 is outside this task's Files table; the code comment already records the tradeoff);
  the engine-paused/row-active split window (the reconciler's `writeBack` adopts the engine's
  reported state, which is the resync the finding asks to confirm; the error also propagates to
  the caller); the SQL placeholder style note (the store layer's sqlx/SQLite `?` convention is
  uniform).

`make lint && make test PKG=./internal/...` after review round 12 — lint clean, every internal
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
ok  	github.com/L-K-M/dl-tool/internal/api	45.203s
ok  	github.com/L-K-M/dl-tool/internal/config	1.119s
ok  	github.com/L-K-M/dl-tool/internal/engine	14.070s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.142s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.031s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.040s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.168s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.195s
ok  	github.com/L-K-M/dl-tool/internal/store	63.715s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.368s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.045s
```

The four named tests, run verbosely after the round-12 fixes:

```
$ go test ./internal/fsx/ ./internal/engine/ -run 'TestAdmitsAccountsForCommittedBytes|TestDefaultFloorIsTwoGiB|TestENOSPCPausesAndKeepsData|TestThirdTaskStaysQueued' -count=1 -v | grep -E '^(=== RUN|--- (PASS|FAIL)|PASS|FAIL|ok)'
=== RUN   TestAdmitsAccountsForCommittedBytes
--- PASS: TestAdmitsAccountsForCommittedBytes (0.00s)
=== RUN   TestDefaultFloorIsTwoGiB
--- PASS: TestDefaultFloorIsTwoGiB (0.00s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/fsx	0.007s
=== RUN   TestThirdTaskStaysQueued
--- PASS: TestThirdTaskStaysQueued (0.07s)
=== RUN   TestENOSPCPausesAndKeepsData
--- PASS: TestENOSPCPausesAndKeepsData (0.03s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/engine	0.112s
```

### Review round 11 (commit 2fde1fa → 6bad003)

Seven majors, eleven minors, six infos, seven outside-diff notes; triage and outcome:

- **Major — the removed M1-exit gap row (re-asked)** — declined again, verified:
  `Admitter.Run` has its composition-root call site in `internal/api/server.go` (`NewAdmitter` +
  the goroutine beside the reconciler, `admissionPolicyLoader` over the settings rows). The row
  would only be restored if the wiring were missing; it is not.
- **Major — T127's "both columns to NULL"** — valid doc precision: step 1 now names `error_code`
  and `error_message` and states `state` is never written.
- **Major — `MinFree` zero-value lookup** — the only production read is `fsx.Floor`'s comma-ok
  lookup; the `Policy.MinFree` comment now states the two-value rule so no future reader writes
  the single-value form.
- **Major — paused candidates vs operator pauses (re-asked)** — declined: the candidate query
  filters paused rows by `error_code = 'disk_full'` in SQL, pinned by
  `TestOperatorPausedTaskIsNotACandidate`, and the stamp-clearing takeover is T127's, recorded in
  this same changeset.
- **Major — ENOSPC on an unwatched volume ping-pongs** — a real limitation of the routed report,
  but the routing itself is T126's unshipped scope: T126's Out-of-scope now records that the pass
  re-examines parked tasks every tick and that changing it is plan-level.
- **Major — release cleanup can wipe a real failure's code** — valid and fixed at the store:
  `queryClearTaskErrorCodeUnlessPaused` now matches only the two admission hold codes, so a row
  that moved to `error` carrying its own code between the release and the clear keeps it. The
  literals follow the file's existing SQL pinning pattern; `TestClearHoldCodeNeverWipesAPausedStamp`
  gained the failed-state case.
- **Major — `markReleased`'s guard living store-side (re-asked)** — declined: the guard exists
  since round 3 in `queryClearTaskErrorCodeUnlessPaused` (`state <> 'paused'`), a declined clear
  returns nil, and `TestClearHoldCodeNeverWipesAPausedStamp` covers the pause-vs-clear race
  through the real store.
- Applied (minors + infos): T127's Depends columns name T022 (the action layer's owner); the
  round-10 named-test run is pasted, not asserted; the scope-check expectation reads "as a set";
  `Roots` documents separator-bounded ownership; "admit past a reservation" wording; the
  slot-wait log moved to Info (it is the one line explaining a recovered task still parked); a
  per-pass destination→filesystem-id cache ends the repeated ancestor climbs; the parked stamp
  refresh tolerates a vanished task (`vanishedStampStore` test); `fsx.Floor` cleans stored keys
  as well as the lookup root, with tests; `FreeSpace` documents the 2^63-byte assumption; the
  `mib` comment warns the disk-space tests off `t.Parallel()`; `AdmissionStore`'s `Get` and
  `ClearHoldCode` contracts are documented.
- Declined: `floorLeaving` staying fatal (documented deliberate — an undersized temp filesystem
  is an environment failure, and a skip would weaken the check); the unknown-size placeholder
  reservation (FR-047's unknown-size rule is the plan's stated semantics; a placeholder is an
  owner-level design decision); renaming `SelectQueuedCandidates` (cosmetic, and the store's test
  file is outside this task's Files table — the method's doc comment carries the contract); the
  `releaseFailed` stranding re-ask (the store guard exists since round 3 and a
  paused row's stamp is untouchable by the clear — covered by
  `TestClearHoldCodeNeverWipesAPausedStamp`); the still-full-disk churn concern (`holdsParked`
  fails closed on an unreadable or unadmitting filesystem, so a parked task is never resumed into
  a full disk); the `diskFullPauseSources` drift concern (the read side has consumed the slice
  via `slices.Contains` since round 10; the SQL counter's literal is pinned by the store's
  counting tests); live numbers in hold messages (both sentences are fixed constants —
  `holdMessage`, `diskFullMessage` — exactly so the guarded re-stamp stays silent); the fsx
  `!linux` companion (runtime and CI are Linux-only by plan; no importer is platform-neutral in
  practice).

`make lint && make test PKG=./internal/...` after review round 11 — lint clean, every internal
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
ok  	github.com/L-K-M/dl-tool/internal/api	48.268s
ok  	github.com/L-K-M/dl-tool/internal/config	1.248s
ok  	github.com/L-K-M/dl-tool/internal/engine	15.646s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.228s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.033s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.915s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.197s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.221s
ok  	github.com/L-K-M/dl-tool/internal/store	66.133s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.369s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.039s
```

The four named tests, run verbosely after the round-11 fixes:

```
$ go test ./internal/fsx/ ./internal/engine/ -run 'TestAdmitsAccountsForCommittedBytes|TestDefaultFloorIsTwoGiB|TestENOSPCPausesAndKeepsData|TestThirdTaskStaysQueued' -count=1 -v | grep -E '^(=== RUN|--- (PASS|FAIL)|PASS|FAIL|ok)'
=== RUN   TestAdmitsAccountsForCommittedBytes
--- PASS: TestAdmitsAccountsForCommittedBytes (0.00s)
=== RUN   TestDefaultFloorIsTwoGiB
--- PASS: TestDefaultFloorIsTwoGiB (0.00s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/fsx	0.005s
=== RUN   TestThirdTaskStaysQueued
--- PASS: TestThirdTaskStaysQueued (0.13s)
=== RUN   TestENOSPCPausesAndKeepsData
--- PASS: TestENOSPCPausesAndKeepsData (0.06s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/engine	0.208s
```

### Review round 10 (commit 03ca05b → 2fde1fa)

Two majors, fourteen minors, two outside-diff notes; triage and outcome:

- **Major — the `done` flip must be backed by composition-root wiring** — declined on the facts:
  `internal/api/server.go` constructs `engine.NewAdmitter` and starts `admitter.Run` beside the
  reconciler with `admissionPolicyLoader(db, cfg.DataRoots)`, the closure over the three settings
  rows including `min_free_space`; the Files table lists `internal/api/server.go` with exactly
  that purpose, and CI runs the acceptance tests green.
- **Major — auto-resume conflates guard-parked with operator-paused `disk_full` rows** — real,
  and out of this task's reach: the attribution lives in `error_code` alone, and the operator
  pause path (`internal/api/tasks_actions.go`) is outside the Files table. Carved out as
  [T127](T127-operator-pause-of-a-parked-task.md) — the same disposition round 5 gave the FR-048
  routing gap — with the register and roster rows; the pass's paused branch now names the limit.
- Applied: `PauseDiskFull`'s already-parked case retries the engine-side pause (the first
  attempt's failure was warn-only) with a fail-once engine test; the state allow-list reads
  `slices.Contains(diskFullPauseSources, ...)` so the read side and the atomic write cannot
  drift; the loader deduplicates roots that clean to the same spelling; the candidate scan reads
  `COALESCE(completed_bytes, 0)` so a NULL row can no longer fail the whole pass;
  `TestPassCommitsReleasedBytesInMemory` widens its margin to the file-standard 50 MiB;
  `TestPauseDiskFullFailureLeavesTheRowUntouched` now pins that no pause event survives the
  failed landing; `TestThirdTaskStaysQueued` derives the commitment from the seeded totals; the
  dead nil-map guard and the understated admission-stopped log line follow the info findings.
- Docs: round 6's heading names 34f7e71 (round 9's "→ this one" stays — the one-commit-per-task
  amend convention makes the final hash unknowable at write time); the unattributed first
  evidence paste is removed rather than guessed at; the Blocked section records both hand-offs;
  T126's step 2 makes the `PauseWithCode` shortcut conditional on proven equivalence.
- Declined: the empty-root catch-all (the config layer rejects empty `DLTOOL_DATA_ROOTS` entries
  fatally and the loader cleans every spelling, so `rootOf` only ever maps a literal `/` to `/`);
  verbatim `MinFree` keys (the loader canonicalises keys with `filepath.Clean` and errors on
  conflicting duplicates — round 8 — so the gate's lookup form is the loader's output form);
  `known` unused in `TestUnknownTotalReservesNothing` (it drives the second pass's
  `Transition`); the per-filesystem `MaxFloor` (the interface contract pins `MinFreeBytes` as
  "this root's min_free_space": the floor governs each admission into its own root, and pooling
  covers the filesystem facts — free and committed bytes — not the per-root policy); the
  `markReleased` reorder (`ClearHoldCode` has been guarded on `state <> 'paused'` since round 3
  and declines with nil — the interleaving the note describes is already impossible).

`make lint && make test PKG=./internal/...` after review round 10 — lint clean, every internal
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
ok  	github.com/L-K-M/dl-tool/internal/api	52.026s
ok  	github.com/L-K-M/dl-tool/internal/config	1.149s
ok  	github.com/L-K-M/dl-tool/internal/engine	16.498s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.213s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.034s
ok  	github.com/L-K-M/dl-tool/internal/jobs	5.117s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.247s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.534s
ok  	github.com/L-K-M/dl-tool/internal/store	75.102s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.376s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.036s
```

The four named tests, run verbosely after the round-10 fixes:

```
$ go test ./internal/fsx/ ./internal/engine/ -run 'TestAdmitsAccountsForCommittedBytes|TestDefaultFloorIsTwoGiB|TestENOSPCPausesAndKeepsData|TestThirdTaskStaysQueued' -count=1 -v | grep -E '^(=== RUN|--- (PASS|FAIL)|PASS|FAIL|ok)'
=== RUN   TestAdmitsAccountsForCommittedBytes
--- PASS: TestAdmitsAccountsForCommittedBytes (0.00s)
=== RUN   TestDefaultFloorIsTwoGiB
--- PASS: TestDefaultFloorIsTwoGiB (0.00s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/fsx	0.003s
=== RUN   TestThirdTaskStaysQueued
--- PASS: TestThirdTaskStaysQueued (0.07s)
=== RUN   TestENOSPCPausesAndKeepsData
--- PASS: TestENOSPCPausesAndKeepsData (0.03s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/engine	0.110s
```

### Review round 9 (commit ffbd804 → 03ca05b)

Four findings, all minor; triage and outcome — all applied:

- The pooling warn routes through `warnOnce` (keyed `unidentified` like the per-candidate reads),
  so one unidentifiable mount costs one warn per pass, not one per destination.
- `PauseWithCode` captures one `now` for the row's stamp and its `task_events` row, so the pair
  reads as the same instant.
- The candidate struct documents `completed_bytes`'s `NOT NULL`: a NULL row fails the whole scan,
  not just its own task.
- T126's scope check parses porcelain paths with `cut -c4-`, which survives a path with spaces
  where `awk '{print $NF}'` does not. (T099's own scope check above stays as executed — its
  Evidence output is pasted against that exact command.)

`make lint && make test PKG=./internal/...` after the fixes: lint 0 issues, all 11 internal
packages `ok`, no `FAIL`; the four named tests all pass verbosely above.

### Review round 8 (commit 316f1c0 → ffbd804)

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

Ten findings; triage and outcome:

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

### Review round 6 (commit 331ad0c → 34f7e71)

Four blockers (one real, one hallucinated, two about the new task file), two majors, eleven
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
  resolving nondeterministically (claim false when written — the guard landed in round 8); the
  int settings decode through JSON like the floor map; the SQL literal's pairing with
  `engine.ErrorCodeDiskFull` is documented; the full-disk pass in `TestENOSPCPausesAndKeepsData`
  asserts no redundant pause event.
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
- Applied minors: the roster's T099 Parallel cell reads `no` (claim false when written — the
  cell edit was the third mismatch that aborted the round-5 batch and only landed in round 6);
  a parked
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

None. Two out-of-task observations, not blocks, each handed off:

- Owned by [T126](T126-disk-full-pause-routing.md): an aria2-reported disk-full (errorCode 9)
  reaches the reconciler's `writeBack`, which moves the row to `error`; routing that report into
  `PauseDiskFull` needs `internal/engine/reconcile.go`, outside this task's Files table. The
  mechanism (`PauseDiskFull`, and the aria2 mapping already emitting `disk_full`) is in place for
  that call site.
- Owned by [T127](T127-operator-pause-of-a-parked-task.md): an operator pause landing on a row
  the guard already parked is an idempotent no-op that keeps the `disk_full` stamp, and the pass
  would resume what the operator parked. Clearing the stamp on an operator pause needs
  `internal/api/tasks_actions.go`, outside this task's Files table (round-10 review of PR #92).
