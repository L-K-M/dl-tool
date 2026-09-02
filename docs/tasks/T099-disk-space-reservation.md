# T099 — Reserve disk space and keep a free-space floor

| Field | Value |
|---|---|
| **ID** | T099 |
| **Milestone** | M1 |
| **Status** | todo |
| **Depends on** | T020, T024, T098 |
| **Blocks** | T047, T076 |
| **Parallel-safe** | no — it also edits the shared files `internal/engine/admission.go`, `internal/store/tasks.go` |
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
| `internal/store/tasks.go` | modify | Add `SumRemainingByDestination`. |

No other file may be modified.

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
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
