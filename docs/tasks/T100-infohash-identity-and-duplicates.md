# T100 — Record both infohashes and reject duplicate torrents

| Field | Value |
|---|---|
| **ID** | T100 |
| **Milestone** | M2 |
| **Status** | todo |
| **Depends on** | T017, T020, T029, T030, T031 |
| **Blocks** | — |
| **Parallel-safe** | no — extends `internal/api/tasks.go` and `internal/store/tasks.go` |
| **Implements** | [FR-022](../02-requirements.md#fr-022-record-both-bittorrent-infohash-forms), [FR-023](../02-requirements.md#fr-023-reject-a-duplicate-torrent-by-either-infohash) |
| **Decisions** | [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md), [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| **Est. size** | 2 new files, ~330 LOC |

## Goal
Every BitTorrent task stores `infohash_v1` as 40 lowercase hex characters and `infohash_v2` as 64, and a
submission whose **either** hash already belongs to a live task is rejected with `torrent_duplicate`
instead of creating a second row.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/04-data-model.md` §3.3 Tasks](../04-data-model.md#33-tasks)
2. [`docs/06-download-engines.md` §3.5 BitTorrent v2 (BEP 52) identity](../06-download-engines.md#35-bittorrent-v2-bep-52-identity)
3. [`docs/06-download-engines.md` §3.3 Magnet URIs](../06-download-engines.md#33-magnet-uris-magnetgo)
4. [`docs/05-api-contract.md` §5.2 `POST /tasks`](../05-api-contract.md#52-post-tasks)
5. [`docs/05-api-contract.md` §1.3 Errors](../05-api-contract.md#13-errors--rfc-9457-applicationproblemjson)
6. [`docs/04-data-model.md` §4.2 `tasks.error_code`](../04-data-model.md#42-taskserror_code)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/store/tasks_infohash.go` | create | `FindByInfohash`, `SetInfohashes`, `ErrDuplicateInfohash`. |
| `internal/store/tasks_infohash_test.go` | create | Normalisation, partial-unique-index and hybrid cases. |
| `internal/api/tasks.go` | modify | The pre-insert duplicate check and the `rejected[]` entry. |
| `internal/api/tasks_test.go` | modify | Cases for each duplicate form and for the late resolution. |
| `internal/engine/qbittorrent/sync.go` | modify | Write both hashes back when metadata resolves. |

No other file may be modified.

## Interface contract

```go
package store

// ErrDuplicateInfohash is returned when either hash already belongs to a task whose state is not
// 'removed'. The API maps it to a rejected[] entry of type /problems/conflict and records the
// tasks.error_code value "torrent_duplicate".
var ErrDuplicateInfohash = errors.New("store: torrent already present")

// NormaliseInfohash lowercases and validates one hash. It accepts 40 hex (v1), 32 base32 characters
// (v1, decoded to 40 hex), 64 hex (v2) and the 68-character multihash form 1220<64 hex> (v2, stripped
// to its 64 hex digits). It returns "" and no error for an empty input.
func NormaliseInfohash(s string) (string, error)

// FindByInfohash returns the live task matching either hash. Empty arguments never match. A hybrid
// torrent submitted by its v1 magnet and later by its v2 magnet resolves to the same row, which is why
// the two columns are always queried together and never one at a time.
func (s *Store) FindByInfohash(ctx context.Context, v1, v2 string) (Task, error) // ErrNotFound when absent

// SetInfohashes fills both columns once metadata resolves. It is idempotent, refuses to overwrite a
// non-empty column with a different value, and returns ErrDuplicateInfohash when the resolved hash
// collides with another live task.
func (s *Store) SetInfohashes(ctx context.Context, taskID, v1, v2 string) error
```

Storage rules, exactly these:

| Input | `infohash_v1` | `infohash_v2` |
|---|---|---|
| `xt=urn:btih:<40 hex>` | lowercased verbatim | `NULL` |
| `xt=urn:btih:<32 base32>` | base32-decoded to 20 bytes, hex-encoded | `NULL` |
| `xt=urn:btmh:1220<64 hex>` | `NULL` | the 64 hex digits after `1220` |
| hybrid magnet carrying both `xt` values | set | set |
| `.torrent`, v1 | from `uri.InspectTorrent` | `NULL` |
| `.torrent`, hybrid | set | set |
| `.torrent`, v2-only | `NULL` | set |

An empty string is stored as `NULL`, never as `''`, because `idx_tasks_infohash_v1` and
`idx_tasks_infohash_v2` are partial unique indices over the non-null rows.

## Steps
1. Create `internal/store/tasks_infohash.go` with `NormaliseInfohash`, `FindByInfohash`, `SetInfohashes`
   and `ErrDuplicateInfohash`, using explicit column lists and `?` placeholders.
2. Implement `FindByInfohash` as one query with `(infohash_v1 = ? AND ? != '') OR (infohash_v2 = ? AND ? != '')`
   and `state != 'removed'`, so an empty argument can never match.
3. Implement `SetInfohashes` inside one `sqlx.Tx`: re-check with `FindByInfohash`, refuse a conflicting
   overwrite, then update both columns and `updated_at`.
4. Edit `internal/api/tasks.go` to resolve both hashes before insert — from `uri.ParseMagnet` for a
   magnet, from `uri.InspectTorrent` for a blob, and from a bare 40-hex or 64-hex submission — and to
   call `FindByInfohash` first.
5. On a match, add a `rejected[]` entry with `type: "/problems/conflict"` and a detail naming the existing
   task id, and create no row. Do not invent a `/problems/torrent-duplicate` slug: the registry in 05 §1.3
   is closed.
6. Write `torrent_duplicate` into `tasks.error_code` only where an existing task is being marked, never on
   the rejected submission, which has no row.
7. Edit `internal/engine/qbittorrent/sync.go` so the delta path calls back into `SetInfohashes` when a
   torrent's `infohash_v1` or `infohash_v2` becomes non-empty, which is when a magnet's metadata arrives;
   take the values from those keys, never from `hash`.
8. Handle the late-collision case: when `SetInfohashes` returns `ErrDuplicateInfohash` after metadata
   resolves, pause the task with `error_code = "torrent_duplicate"`, write a `task_events` row, and never
   delete it or its data.
9. Confirm the open question of [`06` §3.5](../06-download-engines.md#35-bittorrent-v2-bep-52-identity)
   against a v2-only fixture: whether qBittorrent's `hash` is the 40-hex truncation of `infohash_v2`.
   Record the observed value under `## Evidence`. `engine_ref` stores `hash` verbatim either way.
10. Create `internal/store/tasks_infohash_test.go` covering: a base32 magnet and its hex form resolving to
    the same row; a hybrid torrent added by v1 then by v2 producing one row and one duplicate rejection;
    a v2-only torrent leaving `infohash_v1` NULL; two tasks with NULL hashes coexisting under the partial
    unique indices; and an uppercase input being stored lowercase.
11. Extend `internal/api/tasks_test.go` with the duplicate rejection body and with the late-resolution
    pause, asserting the row still exists and `completed_bytes` is unchanged.

## Acceptance criteria
- [ ] A magnet in 32-character base32 and the same magnet in 40-hex resolve to one task.
- [ ] A hybrid torrent added by its v1 magnet and then by its v2 magnet yields exactly one row.
- [ ] Both columns are lowercase hex of exactly 40 and 64 characters, or NULL.
- [ ] Two tasks with no infohash coexist; the partial unique indices do not collide on NULL.
- [ ] Deduplication never queries `engine_ref`.
- [ ] A duplicate discovered after metadata resolves pauses the task and deletes nothing.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then `ok` for `github.com/L-K-M/dl-tool/internal/store`,
`.../internal/api` and `.../internal/engine/qbittorrent`, with `TestBase32AndHexAreOneTask`,
`TestHybridDedupBothDirections`, `TestV2OnlyLeavesV1Null`, `TestNullHashesCoexist` and
`TestLateDuplicatePausesTask` all `PASS`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add a migration; `infohash_v1`, `infohash_v2` and both partial unique indices are already in
  `00001_init.sql` from T006.
- Do NOT deduplicate non-BitTorrent tasks by URL; two HTTP tasks for one URL are legal.
- Do NOT widen `feed_items.info_hash` or `rule_matches.info_hash` here; T065 owns the RSS tables.
- Do NOT invent a problem slug. Use `/problems/conflict` from the registry in 05 §1.3.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
