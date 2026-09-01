# T036 — Apply share limits, sequential download and the location mutators

| Field | Value |
|---|---|
| **ID** | T036 |
| **Milestone** | M2 |
| **Status** | todo |
| **Depends on** | T022, T029 |
| **Blocks** | T038 |
| **Parallel-safe** | no — it also edits the shared file `internal/api/tasks_actions.go` |
| **Implements** | [FR-019](../02-requirements.md#fr-019-offer-sequential-download-and-share-limits) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| **Est. size** | 2 new files, ~330 LOC |

## Goal
`PATCH /api/v1/tasks/{id}` carrying `ratio_limit`, `seeding_time_limit`, `sequential`, `destination` or
`category` reaches qBittorrent through the matching adapter method, and seeding stops when **either**
share limit is reached.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §5.5 `PATCH /tasks/{id}`](../05-api-contract.md#55-patch-tasksid)
2. [`docs/06-download-engines.md` §5.7 Files, priorities, trackers, peers, lifecycle](../06-download-engines.md#57-files-priorities-trackers-peers-lifecycle)
3. [`docs/06-download-engines.md` §1 The Engine interface](../06-download-engines.md#1-the-engine-interface)
4. [`docs/04-data-model.md` §3.3 Tasks](../04-data-model.md#33-tasks)
5. [`docs/14-conventions.md` §2.2 Error model](../14-conventions.md#22-error-model)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/qbittorrent/mutate.go` | create | `SetShareLimits`, `SetLocation`, `SetCategory`, `Rename`, tags and sequential. |
| `internal/engine/qbittorrent/mutate_test.go` | create | Form-field cases for each call and the unlimited sentinels. |
| `internal/api/tasks_actions.go` | modify | Route the patched share, sequential and location fields to the engine. |
| `internal/api/tasks_actions_test.go` | modify | Cases for each patched field reaching the engine exactly once. |

No other file may be modified.

## Interface contract

```go
package qbittorrent

// SetShareLimits posts torrents/setShareLimits with hashes, ratioLimit, seedingTimeLimit and
// inactiveSeedingTimeLimit. dl-tool sends both limits on every call, so qBittorrent stops seeding on
// whichever fires first — deliberately OR, unlike Download Station's AND.
//
// A nil ratio or nil seedMinutes means "use the global default" and is sent as the daemon's
// use-the-global sentinel; an explicit 0 means "stop as soon as the check runs". dl-tool does not expose
// inactiveSeedingTimeLimit, so it is always sent as the same use-the-global sentinel.
func (c *Client) SetShareLimits(ctx context.Context, id string, ratio *float64, seedMinutes *int64) error

// SetLocation posts torrents/setLocation with hashes and location. The path is already resolved and
// jailed by internal/fsx; this method never joins or cleans a path itself.
func (c *Client) SetLocation(ctx context.Context, id, path string) error

// SetCategory posts torrents/setCategory with hashes and category. An empty category clears it.
func (c *Client) SetCategory(ctx context.Context, id, category string) error

// Rename posts torrents/rename with hash and name. It renames the torrent, never a file on disk.
func (c *Client) Rename(ctx context.Context, id, name string) error

// SetTags replaces the task's tags: one torrents/removeTags for the tags to drop and one
// torrents/addTags for the tags to add, both with comma-separated values.
func (c *Client) SetTags(ctx context.Context, id string, tags []string) error

// SetSequential posts torrents/toggleSequentialDownload when the requested value differs from the
// seq_dl field the cache holds. The endpoint toggles, so it must never be called unconditionally.
func (c *Client) SetSequential(ctx context.Context, id string, sequential bool) error
```

Sentinel encoding:

| dl-tool value | `ratioLimit` | `seedingTimeLimit` |
|---|---|---|
| `nil` — use the global default | the use-the-global sentinel | the use-the-global sentinel |
| `0` — stop as soon as the check runs | `0` | `0` |
| a positive value | the value | the value in **minutes** |

<!-- UNVERIFIED: the numeric value qBittorrent uses for "use the global limit" on ratioLimit and
     seedingTimeLimit was not read verbatim from release-5.2.3. Read it back from
     GET /api/v2/torrents/info (`max_ratio`, `max_seeding_time`) on a torrent that has never had a share
     limit set, record the observed value under `## Evidence`, and only then write it into one named
     constant in mutate.go. Do not guess it. -->

`tasks.seeding_time_limit` is stored in **seconds** ([`04-data-model.md`](../04-data-model.md)) and
qBittorrent takes **minutes**; the adapter is the only place that divides, and it rounds up.

## Steps
1. Create `internal/engine/qbittorrent/mutate.go` with the six methods above, each splitting the
   `"qbittorrent:"` prefix off `id` and posting a `url.Values` form through `Client.do`.
2. Determine the use-the-global sentinel as described above, put it in one named constant, and convert
   seconds to minutes with `(seconds + 59) / 60`.
3. Implement `SetTags` as a diff against the tags the maindata cache holds, so an unchanged tag set
   issues no request.
4. Implement `SetSequential` as a read-then-toggle against the cached `seq_dl`, and document in one
   comment that the endpoint is a toggle, not a setter.
5. Return `engine.ErrNotFound` for a hash the daemon does not know and `engine.ErrUnavailable` on a
   transport failure, wrapped per [`14-conventions.md` §2.2](../14-conventions.md#22-error-model).
6. Edit `internal/api/tasks_actions.go` so `PATCH /tasks/{id}` calls, in this order and only for the
   fields present in the body: `SetCategory`, `SetTags`, `SetSequential`, `SetShareLimits`, then
   `SetLocation`; each engine failure aborts the patch with `503` and leaves the row unchanged.
7. Keep the destination change transactional at the API level: write `tasks.destination` only after
   `SetLocation` succeeded, and set the state to `moving` exactly as T022 already does.
8. Return `422` `/problems/validation-failed` for a negative `ratio_limit` or `seeding_time_limit`, and
   for `sequential` on an engine that does not declare `sequential`.
9. Create `internal/engine/qbittorrent/mutate_test.go` asserting the exact form fields of each call, the
   use-the-global sentinels, the seconds-to-minutes rounding, the no-op tag diff and the toggle guard.
10. Extend `internal/api/tasks_actions_test.go` with one case per patched field asserting the engine was
    called exactly once, and one case asserting an engine failure leaves the stored row untouched.

## Acceptance criteria
- [ ] `SetShareLimits` always sends `ratioLimit` and `seedingTimeLimit` together, so either can stop
      seeding.
- [ ] `nil` becomes the observed use-the-global sentinel and `0` stays `0` on both limits.
- [ ] `seeding_time_limit` of 90 seconds is sent as `2` minutes, not `1`.
- [ ] `SetSequential` issues no request when the cached `seq_dl` already matches.
- [ ] `SetTags` with an unchanged set issues no request.
- [ ] A failing engine call leaves `tasks` unchanged and returns `503` `/problems/engine-unavailable`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then `ok` for
`github.com/L-K-M/dl-tool/internal/engine/qbittorrent` and `github.com/L-K-M/dl-tool/internal/api`, with
`TestShareLimitsSendsBoth`, `TestNilLimitsSendGlobalSentinel`, `TestSeedTimeRoundsUpToMinutes`,
`TestSequentialToggleGuard` and `TestPatchRollsBackOnEngineFailure` all `PASS`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement `SetRateLimits`; T037 owns per-task and global bandwidth.
- Do NOT move data across filesystems; T076 owns the EXDEV-safe move, and `SetLocation` only tells the
  engine where the content now belongs.
- Do NOT call `torrents/setAutoManagement` or enable Automatic Torrent Management for any reason.
- Do NOT rename a file inside the torrent; `torrents/renameFile` is not in v1.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
