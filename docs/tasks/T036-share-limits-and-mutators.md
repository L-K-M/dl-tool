# T036 — Apply share limits, sequential download and the location mutators

| Field | Value |
|---|---|
| **ID** | T036 |
| **Milestone** | M2 |
| **Status** | done |
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
- [x] `SetShareLimits` always sends `ratioLimit` and `seedingTimeLimit` together, so either can stop
      seeding.
- [x] `nil` becomes the observed use-the-global sentinel and `0` stays `0` on both limits.
- [x] `seeding_time_limit` of 90 seconds is sent as `2` minutes, not `1`.
- [x] `SetSequential` issues no request when the cached `seq_dl` already matches.
- [x] `SetTags` with an unchanged set issues no request.
- [x] A failing engine call leaves `tasks` unchanged and returns `503` `/problems/engine-unavailable`.

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

`make lint && make test PKG=./internal/...`:

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
ok  	github.com/L-K-M/dl-tool/internal/api	82.425s
ok  	github.com/L-K-M/dl-tool/internal/config	1.223s
ok  	github.com/L-K-M/dl-tool/internal/engine	23.084s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.225s
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	5.364s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.033s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.795s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.184s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.217s
ok  	github.com/L-K-M/dl-tool/internal/store	70.725s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.381s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.059s
```

The five named tests, re-run individually on the final tree:

```
--- PASS: TestShareLimitsSendsBoth (0.00s)
--- PASS: TestNilLimitsSendGlobalSentinel (0.00s)
--- PASS: TestSeedTimeRoundsUpToMinutes (0.00s)
--- PASS: TestSequentialToggleGuard (0.00s)
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	0.026s
--- PASS: TestPatchRollsBackOnEngineFailure (0.37s)
ok  	github.com/L-K-M/dl-tool/internal/api	0.385s
```

The same run after the review round, on the final commit:

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
ok  	github.com/L-K-M/dl-tool/internal/api	85.585s
ok  	github.com/L-K-M/dl-tool/internal/config	1.242s
ok  	github.com/L-K-M/dl-tool/internal/engine	23.166s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.290s
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	5.393s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.029s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.871s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.197s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.381s
ok  	github.com/L-K-M/dl-tool/internal/store	70.980s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.380s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.067s
```

Scope:

```
api/openapi.json
internal/api/tasks_actions.go
internal/api/tasks_actions_test.go
internal/engine/qbittorrent/mutate.go
internal/engine/qbittorrent/mutate_test.go
web/src/api/schema.d.ts
```

`api/openapi.json` and `web/src/api/schema.d.ts` are the standing §7.1 exception: `PatchTaskBody`
gained the `destination` field, and both were produced by `make gen`. `make vet`, `make typecheck`,
`make doclint` (2367 OK, 0 Errors) and a re-run `make gen` with no diff also passed; `make compose-check`
could not run in this environment (no Docker socket), and the compose inputs are untouched by this diff.

The use-the-global sentinel (the UNVERIFIED block above). Docker is unavailable in this environment, so
the reading comes from the repository's own live capture — `internal/engine/qbittorrent/testdata/
qb_maindata_full_5.2.3.json`, recorded from a real release-5.2.3 daemon in T030 — cross-checked against
the pinned tag's sources:

- A torrent that never had a share limit set reports the per-torrent fields `"ratio_limit": -2`,
  `"seeding_time_limit": -2`, `"inactive_seeding_time_limit": -2`, and `"share_limit_action":
  "Default"`. `shareLimitUseGlobal = "-2"` and `shareLimitActionDefault = "Default"` in mutate.go.
- The `max_ratio` / `max_seeding_time` the block suggests reading are the *resolved effective* limits,
  not the sentinel: in that capture they read `-1` because the daemon's globals were unlimited, and
  `-1` is the daemon's *no-limit-at-all* value (`NO_RATIO_LIMIT`). Sending `-1` for a nil dl-tool limit
  would have meant "never stop" instead of "use the global default".
- Verbatim from the `release-5.2.3` tag, `src/base/bittorrent/sharelimits.h`:
  `inline const qreal DEFAULT_RATIO_LIMIT = -2;`, `inline const int DEFAULT_SEEDING_TIME_LIMIT = -2;`.
  `src/webui/api/torrentscontroller.cpp` `setShareLimitsAction` `requireParams` lists all of `hashes`,
  `ratioLimit`, `seedingTimeLimit`, `inactiveSeedingTimeLimit`, `shareLimitAction` — hence the constant
  `shareLimitAction` dl-tool always sends.

Two notes on the interface contract, resolved toward the steps, criteria and Verification block (which
agree with each other; only the contract block's parameter *name* differs):

- The qbittorrent `SetShareLimits` parameter is named `seedSeconds`, not `seedMinutes`: the steps put
  the seconds-to-minutes conversion in mutate.go ("the adapter is the only place that divides") and
  `TestSeedTimeRoundsUpToMinutes` must pass in this package, so the method receives dl-tool's stored
  seconds and converts once, via `(seconds + 59) / 60`. The signature still satisfies
  `engine.Engine.SetShareLimits(ctx, id, *float64, *int64)`; the API layer passes
  `tasks.seeding_time_limit` (seconds) straight through.
- `SetTags` and `SetSequential` read the maindata cache (as their steps require) and answer
  `engine.ErrNotFound` for a hash the cache does not hold. The five hashes-carried mutations the daemon
  silently skips for unknown hashes (`applyToTorrents` in release-5.2.3) cannot be reported by it at all,
  so `notFoundOr` maps the 404 the two hash-addressed endpoints (`rename`) do answer.

### Review round 1 (GLM 5.3, commit 7a7cbd4)

Addressed:

- `SetSequential` now writes the applied value back into the maindata cache after a successful toggle
  (`rememberCacheField`), so a retry after a lost reply — or a repeat of the same request inside one poll
  interval — cannot flip the torrent to the opposite of what was asked. Pinned by the extended
  `TestSequentialToggleGuard`.
- `SetTags` writes the applied set back on full success only, so consecutive patches inside one poll
  interval diff against what the daemon now holds instead of silently unioning; a failed add side leaves
  the cache stale so a retry converges. Pinned by the new `TestSetTagsWriteback`.
- The destination pre-gate: an admitted task whose state cannot enter moving is refused with 422 before
  the first engine call, so `SetLocation` is never issued for a request destined to fail. The moving-entry
  set lives beside the gate as `movingEntryStates`, named against the store's transition table (the store
  exports no legality probe and `internal/store/tasks.go` is outside this task's Files table).
  `applyDestination`'s illegal-transition mapping stays as the compare-and-set backstop. Pinned by the
  "a state that cannot enter moving is 422 before the engine call" subtest.
- The `tagMutator`/`sequentialEngine` narrowing refusals are hoisted before the first engine call, so a
  body mixing a supported field with an unsupported one cannot leave the engine half-mutated behind a
  422.
- `SetLocation`'s engine-side guard now matches the row-write guard (`body.Destination != nil`), not the
  resolved string.
- `TestPatchRollsBackOnEngineFailure` now covers all five mutators (the relocation case also pins the
  state, the destination column and the empty event log); the one-sided share subtest pins the nil half
  of the first call; `TestPatchDestination` gained the non-normalized in-root path (stored cleaned) and
  the pre-gate subtests; `TestActionStandInCapsMatchAdapters` fails if the stand-in capability lists
  drift from the real adapters.

Declined, with reasons:

- Schema `minimum: 0` on the four limit fields: the negative-limit 422 of doc 05 §5.5 is already
  enforced and tested at the handler (`buildTaskPatch` field errors), before any engine call; moving the
  check into huma's schema validation would change the documented problem shape
  (`/problems/validation-failed` with `errors[]`) for no behavioural gain, and the fields predate this
  task (T022).
- The remaining `api/openapi.json` schema findings (tags/urls maxItems, rid required, blob
  contentEncoding, untyped Delta maps, SSE id format, trailing newline, `elapsed_ms` minimum, and the
  FileSelection duplication): all are pre-existing properties of operations and structs owned by other
  tasks, reach the spec only through their source structs, and `api/openapi.json` is generated — editing
  it by hand is forbidden and touching the owning source files is outside this task's Files table.
- The doc 05 §5.5 sentence about engine-side partial application after a 503: `docs/05-api-contract.md`
  is outside the Files table; the clarification lives in `applyPatchMutators`' doc comment instead.
- The T037 index mismatch claim: per-task rate limits are wired since T022
  (`TestPatchTaskAppliesRateLimit` pins `SetRateLimits` on a running task); T037 owns the qBittorrent
  adapter's own rate-limit calls, not this endpoint.

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
