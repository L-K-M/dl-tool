# T030 — Track qBittorrent state through `sync/maindata` rid deltas

| Field | Value |
|---|---|
| **ID** | T030 |
| **Milestone** | M2 |
| **Status** | todo |
| **Depends on** | T026, T029 |
| **Blocks** | T032, T035, T037, T038, T100 |
| **Parallel-safe** | no — extends `internal/engine/qbittorrent/client.go` |
| **Implements** | the qBittorrent half of [FR-148](../02-requirements.md#fr-148-ignore-engine-tasks-dl-tool-did-not-create); infrastructure for [FR-016](../02-requirements.md#fr-016-stream-task-changes-as-rid-deltas-over-sse) |
| **Decisions** | [ADR-0006](../decisions/0006-sse-with-rid-deltas.md), [ADR-0017](../decisions/0017-exclusive-control-of-engines.md) |
| **Est. size** | 3 new files, ~380 LOC |

## Goal
The adapter holds a merged torrent cache fed by `GET /api/v2/sync/maindata?rid=N` polled every second, and
serves `List`, `Get` and `Events` from it. Failed polls, the five-minute recovery interval and ownership
resets force a full snapshot; a silent ownership drop does not. Ownership is decided only by hash: rejected
torrent fields never enter the served cache, while rejected hashes are retained privately so later ownership
can be detected.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §5.4 `sync/maindata` — the delta protocol](../06-download-engines.md#54-syncmaindata--the-delta-protocol)
2. [`docs/06-download-engines.md` §5.5 `torrents/info`](../06-download-engines.md#55-torrentsinfo)
3. [`docs/06-download-engines.md` §8 Engine ownership](../06-download-engines.md#8-engine-ownership)
4. [`docs/06-download-engines.md` §1 The Engine interface](../06-download-engines.md#1-the-engine-interface)
5. [`docs/13-testing-and-verification.md` §5 Golden-file fixtures](../13-testing-and-verification.md#5-golden-file-fixtures)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/qbittorrent/sync.go` | create | The rid cache, the merge, `List`, `Get`, `Events`. |
| `internal/engine/qbittorrent/sync_test.go` | create | Merge, removal, rid-reset, `OwnedRefs` and reconciler-wiring cases. |
| `internal/engine/qbittorrent/testdata/qb_maindata_full_5.2.3.json` | create | One captured `full_update` response. |
| `internal/engine/qbittorrent/client.go` | modify | Start and stop the poll goroutine from `Connect`/`Close`. |
| `internal/engine/reconcile.go` | modify | Add `Reconciler.OwnedRefs` and install its ownership source on qBittorrent. |

No other file may be modified.

## Interface contract

```go
package qbittorrent

const qbtFullSyncInterval = 5 * time.Minute

// maindata is the GET /api/v2/sync/maindata?rid=N envelope. Torrents values are partial objects, so
// they are held as raw JSON and merged key by key — never decoded into torrentJSON before merging.
type maindata struct {
	Rid             int                        `json:"rid"`
	FullUpdate      bool                       `json:"full_update"`
	Torrents        map[string]json.RawMessage `json:"torrents"`
	TorrentsRemoved []string                   `json:"torrents_removed"`
	ServerState     json.RawMessage            `json:"server_state"`
}

type mergeDisposition uint8

const (
	mergeRejected mergeDisposition = iota // Fail closed unless merge proves the response is publishable.
	mergeApplied
)

// cache is the merged view. fields[hash] holds one owned torrent object. rejected holds foreign hash
// identifiers; pendingFull holds identifiers that became owned and await complete fields. Neither set
// retains torrent fields.
type cache struct {
	rid             int
	fields          map[string]map[string]any
	rejected        map[string]struct{}
	pendingFull     map[string]struct{}
	owned           map[string]struct{} // merge reads this snapshot; nil owns nothing
	ownershipSource func() map[string]struct{}

	// Recovery state shares the cache mutex so a response cannot consume a reset.
	forceFull      bool
	lastFullAt     time.Time
	ownershipEpoch uint64
}

// merge applies one response. On FullUpdate it rebuilds fields and rejected and clears pendingFull;
// otherwise it deep-merges each accepted per-hash object and then applies TorrentsRemoved to all three.
// It returns mergeRejected when pendingFull still contains an accepted hash; the caller then accepts no
// payload, rid or events. Changed and removed contain sorted visible hashes only for mergeApplied.
func (c *cache) merge(m maindata) (disposition mergeDisposition, changed, removed []string)

// SetOwnershipFilter installs the snapshot source deciding which qBittorrent hashes belong to dl-tool;
// it is supplied by the Reconciler of T026. The source is called and its result copied before the cache
// mutex is taken, so store I/O never blocks cache readers. Installation synchronously drops newly
// rejected torrent fields without events and forces a full resync. A nil source owns nothing.
func (c *Client) SetOwnershipFilter(snapshot func() map[string]struct{})

func (c *Client) List(ctx context.Context) ([]engine.TaskInfo, error)
func (c *Client) Get(ctx context.Context, id string) (engine.TaskInfo, error) // ErrNotFound when absent

// Events polls sync/maindata every second and emits one TaskEvent per changed hash: EventRemoved with a
// nil Info for a removed hash, otherwise the kind implied by the new state. The channel closes when ctx
// is cancelled or Close is called.
func (c *Client) Events(ctx context.Context) (<-chan engine.TaskEvent, error)
```

Poll loop rules, exactly these:

| Rule | Behaviour |
|---|---|
| Interval | 1 s, from a `time.Ticker` owned by the goroutine `Connect` starts. |
| First call | `rid=0`, which the server answers with `full_update: true`. |
| Subsequent calls | The last accepted response `rid`, held in adapter memory only, unless the force-full flag is set. |
| `full_update: true` | Rebuild `fields` and `rejected` solely from the response and clear `pendingFull`; report only new or genuinely different accepted hashes as changed; directly publish an accepted pending hash because the response supplies complete fields; emit `EventRemoved` for each previously visible hash the current ownership snapshot accepts but the response omits; silently drop hashes it rejects; clear the force-full flag and restart the full-sync interval. |
| `full_update` absent or false | Re-evaluate every reported hash against the current ownership snapshot. Under [06 §5.4's first-seen guarantee](../06-download-engines.md#54-syncmaindata--the-delta-protocol), insert a new accepted hash or deep-merge an accepted visible hash. Silently drop a now-rejected visible hash and retain only its identifier. Move a now-rejected `pendingFull` hash back to `rejected` without a reset. If a `rejected` hash transitions to accepted, move it to `pendingFull`, increment `ownershipEpoch` once for the pass and set the force-full flag. While an accepted hash remains in `pendingFull`, return `mergeRejected`: apply no response payload, including `torrents_removed`, and accept no rid or events. Do not increment the epoch again, and leave the flag set. Otherwise apply `torrents_removed` to all three collections and return `mergeApplied`. |
| Poll failure | On timeout, transport failure, non-2xx response or decode failure, log at warn, keep the last accepted cache and rid, and set the shared force-full flag; every later poll sends `rid=0` until an accepted `full_update: true` clears it. |
| Periodic full sync | Five minutes after the last accepted full update, using `qbtFullSyncInterval`, set the shared force-full flag so requests use `rid=0` until an accepted full update clears it. |
| Ownership resync | Follow the rules below. |
| Relation to dl-tool's SSE rid | None. This `rid` is the engine's own and never leaves the adapter ([ADR-0006](../decisions/0006-sse-with-rid-deltas.md)). |

Every poll-failure class resets immediately because the outcome is ambiguous: qBittorrent may have generated
an unaccepted response, and a threshold can preserve fields lost by the pinned 5.2.3 bug described in 06 §5.4.

### Ownership resync rules

- Each pass snapshots the source and epoch under the cache mutex, invokes the source and copies its result
  without that mutex, then reacquires it. If the pass epoch changed, discard the fetched set. A pre-request
  pass skips its request for that tick; a response pass also discards its response. The next tick retries.
  A response pass compares the request's captured epoch before the source call and after reacquiring the
  mutex. Only matching passes store the set in `owned` before scanning `fields`, `rejected` and `pendingFull`.
  A full-response pass stores `owned` but skips the rejected-to-pending edge because its merge has complete
  fields.
- The source refreshes its memoized hash set with at most one `ListNonTerminalByEngine` read under the named
  `ownershipCheckBudget` when its one-second TTL expires. A failed read logs at warn and returns the last good
  set, or an empty set before the first success. The initial empty result deliberately fails closed because
  ownership cannot yet be proven; it may temporarily empty `List` but cannot expose foreign fields.
  Membership checks under the cache mutex perform no I/O. A response-time pass may refresh after a slow
  request.
- A visible hash rejected by a recheck moves to `rejected` without an event, a force-full change or an epoch
  increment. If a pending hash becomes rejected, move it back to `rejected` under the same rule.
- Installing or replacing the source is always one reset: move newly accepted rejected hashes to
  `pendingFull`, and move newly rejected visible or pending hashes to `rejected`. The other reset is the
  edge-triggered `rejected` → `pendingFull` transition found before a request or while applying a partial
  response. Increment `ownershipEpoch` once per resetting pass and set the force-full flag. A hash already in
  `pendingFull` never triggers another reset while it remains accepted, and leaves that flag set.
  `pendingFull` is non-empty only while the force-full flag is set.
- An accepted full response publishes accepted pending hashes directly, rebuilds `rejected` from the response
  and clears `pendingFull` and the force-full flag. An omitted pending hash is pruned; otherwise a later
  partial reporting it would be rejected with no remaining full-resync trigger.
- A request captures `ownershipEpoch`. Any response whose captured epoch no longer matches is stale, including
  `full_update: true`; it publishes nothing and cannot clear the flag or restart the full-sync interval.

## Steps
1. Create `internal/engine/qbittorrent/sync.go` with `maindata`, `cache` and one mutex guarding every cache
   field, including the accepted rid, force-full flag, last-full timestamp and ownership epoch. A request
   captures the epoch with its selected rid, then checks the epoch under that mutex before publishing.
2. Implement `cache.merge`: on `FullUpdate`, rebuild `fields` and `rejected` only from hashes the response
   reports, clear `pendingFull`, and compare accepted entries with the old cache so only new or genuinely
   different hashes are changed. On a partial, return `mergeRejected` without processing its payload while
   `pendingFull` contains an accepted hash. Otherwise decode each reported accepted hash into
   `map[string]any`, insert it when absent and copy its keys over an existing entry, then apply
   `TorrentsRemoved` to `fields`, `rejected` and `pendingFull`. Return `mergeApplied` with sorted changed and
   removed visible hashes; the poll loop accepts the response rid and emits events only for that disposition.
3. Apply the hash-only ownership snapshot inside every merge. Keep identifiers in `rejected` or `pendingFull`,
   never their fields. A visible hash rejected by a delta-time recheck disappears without an event. On a
   `rejected` → `pendingFull` transition during a partial, increment `ownershipEpoch` once for the pass, set
   the force-full flag and return `mergeRejected`, applying none of that response, removals included. An
   accepted hash already pending rejects another partial without another increment and leaves the flag set.
   A pending hash that becomes rejected returns to `rejected` without a reset. A full response promotes an
   accepted pending hash directly.
4. Implement `SetOwnershipFilter` with a snapshot source and default nil to an empty set, so a caller that
   forgets to install it sees no foreign transfers. Invoke the source and copy its result before taking the
   cache mutex. Under the mutex, install the source and snapshot, synchronously move rejected-to-accepted
   hashes to `pendingFull`, move newly rejected visible or pending hashes to `rejected` without events,
   increment `ownershipEpoch` once and set the force-full flag. Before each pre-request or
   response-time pass, snapshot the source and epoch under the mutex, release it, invoke and copy the source,
   then reacquire the mutex. On an epoch change, skip a pre-request pass for that tick or discard a response;
   otherwise store the set in `owned` and scan every hash in `fields`, `rejected` and `pendingFull` before a
   partial merge. For a full response, store `owned` and let the merge classify its complete objects directly.
   A recheck-driven visible-to-rejected change is a silent drop, not a reset. Then edit
   `internal/engine/reconcile.go` to add `OwnedRefs(engineName string) func() map[string]struct{}`, backed by
   one `ListNonTerminalByEngine` call under the named `ownershipCheckBudget`. It memoizes the returned hash
   set for the one-second `ownershipListingTTL`, never mutates a returned set and keeps the last good set after a store failure.
   Before its first success it returns empty, which fails closed: visible hashes become rejected without
   events and return only after store recovery triggers one full resync. One pass therefore performs at most
   one store query, outside the cache mutex. Have `NewReconciler` call
   `SetOwnershipFilter(r.OwnedRefs("qbittorrent"))` on every registered engine that exposes the method, so
   production uses the installed source and not default deny.
5. Implement `List` and `Get` by re-marshalling one `fields` entry into `torrentJSON` and calling
   `toTaskInfo` from T029; `Get` splits the `"qbittorrent:"` prefix and returns `engine.ErrNotFound` for a
   hash the cache does not hold.
6. Implement `Events` as a buffered channel fed by the poll goroutine, emitting `EventRemoved` with a nil
   `Info` for removed hashes and otherwise `EventProgress`, `EventPaused`, `EventCompleted` or
   `EventError` from the new state; drop an event rather than block when the channel is full, and log it.
   Events are lossy hints: `List` is authoritative, including after a silent ownership rejection.
7. Edit `internal/engine/qbittorrent/client.go` so `Connect` starts the poll goroutine with a context
   derived from the client's own, and `Close` cancels it and waits for it to exit.
8. Capture `internal/engine/qbittorrent/testdata/qb_maindata_full_5.2.3.json` from a real 5.2.3 daemon with
   `curl -s "$QBT/api/v2/sync/maindata?rid=0"`, redact absolute paths and any token, and record the exact
   capture command and its date under `## Evidence` in this file.
9. Create `sync_test.go` covering: the captured full update populating the cache; the literal partial
   `{"rid":15,"torrents":{"8c2127…":{"state":"pausedUP"}}}` from 06 §5.4 changing only `state`;
   `torrents_removed` deleting a hash and emitting `EventRemoved`; a partial inserting a new accepted hash
   and emitting its change event; an unchanged full snapshot emitting nothing; a forced full snapshot
   emitting `EventRemoved` for a visible hash the current ownership snapshot accepts but the response omits;
   a full response pruning rejected and pending identifiers it omits and directly publishing an accepted
   pending hash; a foreign hash never appearing in `List`, `Get` or the event channel; source installation
   immediately dropping a newly rejected visible hash without an event; a delta-time ownership recheck doing
   the same; a pre-request recheck dropping a newly rejected hash without setting the force-full flag or
   incrementing the epoch; one recheck across many hashes making one store listing without blocking
   concurrent `List`, `Get` or event publication; a mid-request source replacement making the old fetched
   set and response epoch-stale, including when the response is a full update, without publishing cache
   state, withheld identifiers, rid or events, clearing the force-full flag or restarting the interval; a
   rejected-to-pending transition incrementing the epoch only once across later passes and a rejected partial
   applying no torrent removal, rid advance or event; a pending-to-rejected transition allowing the partial
   without a reset; an initial ownership-store failure serving an empty queue until one full
   resync after recovery; each poll-failure class leaving the previous cache intact while setting the shared
   force-full flag until a full response succeeds; the periodic case, with `lastFullAt`
   seeded more than `qbtFullSyncInterval` in the past, setting that flag and forcing the next request to use
   `rid=0` without a real five-minute wait; a rejected hash becoming visible only after its forced full
   resync; the engine `rid` never appearing in serialized `List`, `Get` or `TaskEvent` output; and lifecycle
   cases named `TestConcurrentStopWaitsForPollExit` and `TestCloseStopsPollGoroutine`.

## Acceptance criteria
- [ ] A partial delta inserts a new accepted hash and merges an existing one without clearing any field the
  delta omitted.
- [ ] `full_update: true` rebuilds the visible cache and rejected set from the response, clears `pendingFull`,
  directly publishes an accepted pending hash, reports no unchanged hash as changed, and emits
  `EventRemoved` for each previously visible hash the current ownership snapshot accepts but the response
  omits. An omitted pending identifier is pruned.
- [ ] A withheld hash has no retained torrent fields and appears in no `List`, no `Get` and no `TaskEvent`;
  its adapter-local identifier is used only to recheck hash ownership.
- [ ] Installing or replacing the snapshot source immediately removes newly rejected visible hashes without
  events and is an ownership reset. Every delta rechecks its reported hashes and does the same removal. A
  visible-to-rejected change found by a recheck, unlike source installation, does not set the force-full flag
  or increment the ownership epoch.
- [ ] One ownership recheck over any number of hashes makes at most one `ListNonTerminalByEngine` call, and
  store I/O never runs under the cache mutex. Before the first successful call, ownership fails closed; the
  first recovered snapshot forces one full resync.
- [ ] `TestForeignHashIsInvisible` exercises a snapshot source installed by `NewReconciler`, not the default.
- [ ] The engine `rid` is never sent to any dl-tool client and never stored in the database.
- [ ] A failed poll leaves the last accepted cache and rid unchanged and sets the shared force-full flag, so
  every later request sends `rid=0` until an accepted full response succeeds.
- [ ] Five minutes after the last accepted full update, the shared force-full flag is set and the next request
  sends `rid=0`.
- [ ] Only installing or replacing the ownership source, or the edge-triggered `rejected` → `pendingFull`
  transition during a pre-request or partial-response pass, is an ownership reset. It forces `rid=0` and
  increments `ownershipEpoch` once per pass; an already-pending accepted hash cannot retrigger it. A partial
  while an accepted pending hash remains does not advance rid, apply cache or removals, or emit events. A
  pending hash that becomes rejected is silently demoted and does not reject the partial. An in-flight
  response whose captured epoch no longer matches, including a full response, cannot publish state or events,
  consume that reset or restart the full-sync interval. An accepted full response directly publishes an
  accepted pending hash.
- [ ] `Close` returns only after the poll goroutine has exited; `go test -race` is clean.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/engine/...
```
Expected: `make lint` prints nothing, then
`ok  github.com/L-K-M/dl-tool/internal/engine` and
`ok  github.com/L-K-M/dl-tool/internal/engine/qbittorrent` with `TestMergeFullUpdate`,
`TestMergePartialKeepsUntouchedFields`, `TestMergePartialAddsNewAcceptedHash`,
`TestTorrentsRemovedEmitsEventRemoved`, `TestUnchangedFullSnapshotEmitsNothing`,
`TestFullSnapshotEmitsEventRemovedForDroppedHash`, `TestFullSnapshotPrunesStaleRejectedHashes`,
`TestFullSnapshotPrunesStalePendingHashes`, `TestFullSnapshotPublishesPendingHash`,
`TestForeignHashIsInvisible`, `TestOwnershipFilterImmediatelyDropsRejectedHash`,
`TestDeltaRevokingOwnershipDropsHash`, `TestOwnershipPrecheckDropsWithoutReset`,
`TestOwnershipRecheckUsesOneStoreListing`, `TestOwnershipRefreshDoesNotBlockCacheReads`,
`TestOwnedRefsFailsClosedBeforeFirstSuccess`, `TestOwnershipTransitionResetsOnce`,
`TestRejectedPartialAppliesNothing`, `TestPendingHashRevocationAllowsPartial`,
`TestRejectedHashBecomesVisibleAfterOwnershipRefresh`,
`TestOwnershipResetRejectsStaleResponse`, `TestEngineRidStaysInsideCache`,
`TestPollFailureKeepsCacheAndForcesFullUpdate`, `TestPeriodicFullSync`,
`TestConcurrentStopWaitsForPollExit` and `TestCloseStopsPollGoroutine` all `PASS`.
`TestOwnershipResetRejectsStaleResponse` covers stale delta and full responses; `TestPeriodicFullSync` seeds
`lastFullAt` instead of waiting five minutes. No `FAIL`, no data-race report.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, sorted, and nothing else. Use `git status`, not `git diff`: a
file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add an adopt mode, an import path or any setting about transfers dl-tool did not create; there is
  one rule and it has no options.
- Do NOT publish these events to the SSE hub; T025 owns `internal/sync` and T026 owns the reconciler that
  bridges the two.
- Do NOT poll `torrents/info` on a timer; `sync/maindata` is the only polling loop this adapter runs.
- Do NOT add `Files`, trackers or peers to the cache; T032, T034 and T035 fetch those on demand.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

Fresh verification output is required after implementing the corrected recovery rules.

Fixture capture — 2026-09-08, from a live qBittorrent **5.2.3** daemon (no
Docker on the capturing machine; `qbittorrent-nox --version` reported
`qBittorrent v5.2.3`, the static official-source build
`x86_64-qbittorrent-nox` of userdocs/qbittorrent-nox-static
`release-5.2.3_v2.0.14`). The WebUI listened on 127.0.0.1:8080 with the
loopback subnet whitelisted; two Ubuntu 24.04.3 iso torrents were mid-download:

```sh
QBT=http://127.0.0.1:8080
curl -s "$QBT/api/v2/torrents/add" -F "torrents=@ubuntu-24.04.3-desktop-amd64.iso.torrent"
curl -s "$QBT/api/v2/torrents/add" -F "torrents=@ubuntu-24.04.3-live-server-amd64.iso.torrent"
curl -s "$QBT/api/v2/sync/maindata?rid=0" > testdata/qb_maindata_full_5.2.3.json
```

Redaction per docs/13-testing-and-verification.md §5: the local save-path
prefix was replaced with `/data`, and the capture machine's public IP in
`server_state.last_external_address_v4` with `198.51.100.7` (TEST-NET-2).
Everything else is byte-for-byte what the daemon emitted, including the
top-level `trackers` object §5.4 does not document — the envelope ignores
unknown keys, and the fixture proves it.

Behaviour verified against the live daemon beyond the fixture: within one
session (`SID` cookie) `rid` advances per served response, `full_update` is
**absent** — not false — on a partial, and a no-change poll is a bare
`{"rid":2}`. Sessionless requests (no cookie) always answer `full_update:
true`, which is why the adapter's poll rides the login session of T029.

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
