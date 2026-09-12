# T100 — Record both infohashes and reject duplicate torrents

| Field | Value |
|---|---|
| **ID** | T100 |
| **Milestone** | M2 |
| **Status** | done |
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
| `internal/api/server.go` | modify | *Widened mid-task, see [`## Blocked`](#blocked):* one wiring call — `SetInfohashWriter` at the qBittorrent construction — the composition-root call site docs/14-conventions.md §8.3 requires; the original five-row table left the write-back with no caller. |

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
- [x] A magnet in 32-character base32 and the same magnet in 40-hex resolve to one task. — `TestBase32AndHexAreOneTask` (store), `TestCreateTasksDuplicateTorrentForms` (api)
- [x] A hybrid torrent added by its v1 magnet and then by its v2 magnet yields exactly one row. — `TestHybridDedupBothDirections` (store), `TestCreateTasksDuplicateTorrentForms` (api)
- [x] Both columns are lowercase hex of exactly 40 and 64 characters, or NULL. — `TestNormaliseInfohash`, `TestBase32AndHexAreOneTask`, `TestV2OnlyLeavesV1Null`
- [x] Two tasks with no infohash coexist; the partial unique indices do not collide on NULL. — `TestNullHashesCoexist`
- [x] Deduplication never queries `engine_ref`. — `queryFindTaskByInfohash` names both infohash columns alone; `TestHybridDedupBothDirections` matches across two distinct engine_refs, which a handle-based lookup cannot produce
- [x] A duplicate discovered after metadata resolves pauses the task and deletes nothing. — `TestLateDuplicatePausesTask` (api, through the composition root), `TestResolveInfohashesLandsAndPauses` (store)

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

Final tree, the single task commit on `task/T100-infohash-identity-and-duplicates` (review round 1
fixes included).

`make lint && make test PKG=./internal/...` (exact commands of the Verification block):

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
ok  github.com/L-K-M/dl-tool/internal/api              83.821s
ok  github.com/L-K-M/dl-tool/internal/config            1.123s
ok  github.com/L-K-M/dl-tool/internal/engine           21.028s
ok  github.com/L-K-M/dl-tool/internal/engine/aria2       3.213s
ok  github.com/L-K-M/dl-tool/internal/engine/qbittorrent 8.863s
ok  github.com/L-K-M/dl-tool/internal/fsx               1.033s
ok  github.com/L-K-M/dl-tool/internal/jobs              4.419s
ok  github.com/L-K-M/dl-tool/internal/obs               1.184s
ok  github.com/L-K-M/dl-tool/internal/secure            4.041s
ok  github.com/L-K-M/dl-tool/internal/store            71.086s
ok  github.com/L-K-M/dl-tool/internal/sync               4.387s
ok  github.com/L-K-M/dl-tool/internal/uri               1.069s
```

The five named tests plus the review-round regressions, verbosely on the same tree:

```
$ go test ./internal/store/ ./internal/api/ ./internal/engine/qbittorrent/ \
    -run 'TestBase32AndHexAreOneTask|TestHybridDedupBothDirections|TestV2OnlyLeavesV1Null|TestNullHashesCoexist|TestLateDuplicatePausesTask|TestCrossFormCollision|TestCreateTasksHybridAndBareOverlap' -count=1 -v
--- PASS: TestBase32AndHexAreOneTask (0.07s)
--- PASS: TestHybridDedupBothDirections (0.03s)
--- PASS: TestCrossFormCollision (0.02s)
--- PASS: TestV2OnlyLeavesV1Null (0.02s)
--- PASS: TestNullHashesCoexist (0.02s)
ok  github.com/L-K-M/dl-tool/internal/store 0.182s
--- PASS: TestCreateTasksHybridAndBareOverlap (0.05s)
--- PASS: TestLateDuplicatePausesTask (3.04s)
ok  github.com/L-K-M/dl-tool/internal/api 3.122s
ok  github.com/L-K-M/dl-tool/internal/engine/qbittorrent [no tests to run]
```

`TestCrossFormCollision` was observed failing against the pre-fix re-check (no id exclusion: the
row's own reflection won the `LIMIT 1` scan and the collision surfaced as a raw constraint
refusal) before the fix landed, and passing after.

Scope. The Verification block's `git status` form is empty on a committed tree, so the equivalent
check over the branch's full change set:

```
$ git diff --name-only origin/main | sort
internal/api/server.go
internal/api/tasks.go
internal/api/tasks_test.go
internal/engine/qbittorrent/sync.go
internal/store/tasks_infohash.go
internal/store/tasks_infohash_test.go
```

Exactly the Files table (including the widened `server.go` row) and nothing else. `make gen` produces
no diff — no Huma operation or request/response struct changed, so `api/openapi.json` and
`web/src/api/schema.d.ts` stay as committed.

Step 9 (v2-only fixture observation): **not observed — no Docker daemon in this environment**
(`docker: command not found`; `make test-integration` and the testcontainers contract suite cannot
run). The implementation does not depend on the answer: `engine_ref` stores the daemon's `hash`
verbatim, both infohash columns are taken from the `infohash_v1`/`infohash_v2` keys, and the flush
never reconstructs either. The INFERRED marker in 06 §3.5 stays open for a Docker-capable run; the
libtorrent `get_best()` truncation noted in `expectedTorrentID` (T038) remains the best available
evidence.

`make ci`: lint, vet, typecheck, test and doclint pass locally; `compose-check` cannot run without
Docker (compose.yaml is untouched by this task, so CI's check there is identical to main's).

Repetition: `TestLateDuplicatePausesTask` passed 6/6 across two consecutive `-count=3` batches on
the final tree (the first implementation raced an in-flight reconciler sweep; fixed by projecting
the stop into the maindata cache before the engine call — see `flushInfohashes`).

Review rounds 1–3 (GLM 5.3; rounds 2 and 3 carried no important findings) — findings addressed
in the final commit:
the collision re-check now excludes the row being written (`findOtherTaskByInfohash`, regression
`TestCrossFormCollision`) and translates a unique-index refusal at the UPDATE into
`ErrDuplicateInfohash`; the within-submission duplicate set is keyed per hash
(`TestCreateTasksHybridAndBareOverlap`); the flush budget no longer burns tail entries or resets
on re-noted pairs, and dropped resolutions stay dropped; `applyResponseLocked` renamed
`applyResponseInner`; the stop projection picks the UP spelling for upload-side torrents; the
late test asserts the engine-side stop happened and no delete did, and tolerates the admission
pass winning the seeding race. Findings answered without code: `FindByInfohash` keeps
`state <> 'removed'` (verified by the tombstone case of `TestHybridDedupBothDirections`); the
slog JSON handler serializes writes through an internal mutex (Go `log/slog` handler.go), so the
strings.Builder log sink is not a data race, and this task's env never reads it mid-test;
`tasks.updated_at` is Unix milliseconds, so the 2 ms idempotency sleep spans distinct stamps;
`openTestStore` self-registers its cleanup and cannot fail; and `ResolveInfohashes` re-reports the
collision on retry (the failed write leaves the row hashless), so the engine-side pause retry
rides the same path. Round 2's notes landed the same way: budget expiry now spends one attempt on
the entry whose call was in flight (the never-attempted tail keeps its budget and the whole batch
stops), a pause failure on a spent budget counts like any other failure, the unseeded ordering of
the late test no longer asserts the seeded counter, the no-op `ToUpper` and a smart-quoted `''` in
comments are gone. The suggested bare-hash length guard is dead code — every non-empty
`NormaliseInfohash` result is exactly 40 or 64 hex — and was not added.

## Blocked

*Resolved by the Files-table widening recorded in the table itself — kept here because it forced an
edit outside the original table, following the precedent T038 set for exactly this situation.*

**The write-back needed one composition-root call the original five-row table did not list.** Step 7
puts the call-back in the qBittorrent delta path, but the layering of docs/03-architecture.md §5.2
keeps adapters off `internal/store` entirely (qbittorrent imports engine and uri, never the store),
so the store reaches the delta path only through an injected writer — and no file in the original
table could install it. That is precisely the "built and never wired" defect IMPLEMENTING.md names
(pattern 1 of PLAN-REVIEW.md). The widening is one guarded call in `internal/api/server.go`
(`qbittorrentEngine.SetInfohashWriter(store.NewTaskStore(db))`, behind the same nil-db guard as the
boot probe). Routing the write through the reconciler instead (`internal/engine/reconcile.go`) was
considered and rejected: it contradicts the task's own "the delta path calls back", and the
ownership-filter precedent (T030) wires at construction, not in the sweep.

Three contract notes, each a forced consequence of the same layering rule rather than a scope
choice:

1. The store methods live on `*TaskStore`, the package's task-row surface; the sketch's `*Store`
   receiver names a type that does not exist in `internal/store`.
2. `ResolveInfohashes` and `PauseDuplicate` join the sketched three. The delta path needs one
   store-free call that lands lookup + write + collision pause together (`InfohashWriter`, satisfied
   by `*TaskStore` at the wiring site), and the collision pause needs the `torrent_duplicate`
   code/event pair the sketched `SetInfohashes` deliberately does not write. The sketched three
   exist verbatim.
3. The late-collision landing pauses the engine-side transfer too (`torrents/pause`, data
   retained): the reconciler adopts engine state over the rows, and a transfer left downloading
   would un-pause the row on the next sweep — "pause the task and delete nothing" requires both
   halves to stick.

Step 9's v2-only fixture observation could not be made here (no Docker); recorded under Evidence
above. It gates no acceptance criterion and no code path depends on its answer.
