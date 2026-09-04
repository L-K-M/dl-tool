# T011 — Build the SSE hub and the rid ring buffer

| Field | Value |
|---|---|
| **ID** | T011 |
| **Milestone** | M0 |
| **Status** | done |
| **Depends on** | T004 |
| **Blocks** | T025, T051 |
| **Parallel-safe** | yes — touches only `internal/sync/` |
| **Implements** | — (the mechanism behind [FR-016](../02-requirements.md#fr-016-stream-task-changes-as-rid-deltas-over-sse) and [FR-017](../02-requirements.md#fr-017-serve-the-identical-delta-payload-by-polling), both verified by T025) |
| **Decisions** | [ADR-0006](../decisions/0006-sse-with-rid-deltas.md) |
| **Est. size** | 2 new source files, 1 test file, ~280 LOC |

## Goal
`sync.Hub` accepts a delta, assigns it the next monotonic `rid`, keeps the last 300 in a ring, and fans it
out to every subscriber at most once per second. A subscriber that presents a `rid` inside the ring receives
the coalesced diff from that point; one that presents an absent, unparseable or too-old `rid` receives one
message with `full_update: true` and `seq_gap: true`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §6.1 `GET /events`](../05-api-contract.md#61-get-events) — the exact `sync`
   payload fields and the seven server rules.
2. [`docs/03-architecture.md` §8.6 SSE ring buffer](../03-architecture.md#86-sse-ring-buffer) — the ring
   depth, push rate, heartbeat and reconnect key.
3. [`docs/14-conventions.md` §2.3 Signatures](../14-conventions.md#23-signatures) — goroutine ownership.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/sync/ring.go` | create | The fixed-size rid ring and its coalescing read. |
| `internal/sync/hub.go` | create | Subscriber registry, the 1 Hz publish tick and fan-out. |
| `internal/sync/ring_test.go` | create | Ring, coalescing, gap and fan-out tests. |

No other file may be modified.

## Interface contract

```go
package sync

// Delta is one `sync` event. It marshals to exactly the JSON of
// 05-api-contract.md section 6.1; GET /sync returns the identical bytes.
type Delta struct {
	RID               int64                      `json:"rid"`
	FullUpdate        bool                       `json:"full_update"`
	Tasks             map[string]json.RawMessage `json:"tasks"`
	TasksRemoved      []string                   `json:"tasks_removed"`
	Categories        map[string]json.RawMessage `json:"categories,omitempty"`
	CategoriesRemoved []string                   `json:"categories_removed,omitempty"`
	Stats             Stats                      `json:"stats"`
	SeqGap            bool                       `json:"seq_gap"`
}

type Stats struct {
	SpeedDown int64 `json:"speed_down"` // bytes per second
	SpeedUp   int64 `json:"speed_up"`   // bytes per second
	Active    int   `json:"active"`
	Queued    int   `json:"queued"`
}

// RingDepth is 300 deltas, about five minutes at the 1 Hz push rate.
const RingDepth = 300

// Ring stores the last RingDepth deltas keyed by rid.
type Ring struct{ /* mu, buf [RingDepth]Delta, newest int64 */ }

func NewRing() *Ring

// Append stores d under d.RID, evicting the oldest entry.
func (r *Ring) Append(d Delta)

// Since returns one Delta coalescing every entry after rid. ok is false when rid is
// outside the ring, and the caller must then send a full update with SeqGap true.
func (r *Ring) Since(rid int64) (d Delta, ok bool)
```

```go
package sync

// Hub owns the rid counter, the ring and every subscriber.
type Hub struct{ /* ring *Ring, subs map[int64]chan Delta, rid atomic.Int64 */ }

func NewHub() *Hub

// Publish assigns the next rid, stores the delta and fans it out. It drops nothing
// silently: a subscriber whose buffer is full is closed and must reconnect.
func (h *Hub) Publish(d Delta) int64

// Subscribe returns a channel seeded with the caller's starting delta. lastEventID
// is the Last-Event-ID header value; an empty or unparseable value, or one older
// than the ring, seeds a full update with SeqGap true. cancel removes the subscriber.
func (h *Hub) Subscribe(ctx context.Context, lastEventID string, snapshot func() Delta) (<-chan Delta, func())

// Snapshot returns the current rid, so GET /sync can answer with the same envelope.
func (h *Hub) Snapshot(rid int64, full func() Delta) Delta

// Clients reports the current subscriber count for the dltool_sse_clients gauge.
func (h *Hub) Clients() int
```

## Steps
1. Create `internal/sync/ring.go` with `Delta`, `Stats`, `Ring`, `Append` and `Since`. `Since` merges the
   `Tasks` maps newest-wins, unions `TasksRemoved`, and takes `Stats` from the newest entry.
2. A task id present in both `Tasks` and `TasksRemoved` during coalescing resolves to removed; drop it from
   `Tasks`.
3. Create `internal/sync/hub.go`. `Publish` increments the rid with `atomic.Int64`, sets `d.RID`, appends to
   the ring and sends to every subscriber channel with a non-blocking send.
4. Buffer each subscriber channel at 8. On a full buffer, close the channel and delete the subscriber; the
   client reconnects and takes the `seq_gap` path.
5. `Subscribe` parses `lastEventID` as an integer. On success and a ring hit, seed with `Since`. Otherwise
   seed with `snapshot()`, forcing `FullUpdate` and `SeqGap` to true.
6. Rate-limit `Publish` to at most one delivery per second per hub, coalescing anything produced inside the
   window into the next send. `rid` starts at 0 on every process start.
7. Write `internal/sync/ring_test.go` covering: 301 appends leave the oldest rid outside the ring;
   `Since(rid)` coalesces two updates of the same task into one newest-wins entry; `Since` on an evicted rid
   returns `ok == false`; a subscriber with `Last-Event-ID: 5` gets the coalesced diff; one with no header
   gets `full_update` and `seq_gap` true; a slow subscriber is closed rather than blocking `Publish`;
   `Clients` counts up and back down; and `json.Marshal` of a `Delta` produces exactly the field names of
   doc 05 §6.1.
8. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `RingDepth` is 300 and no other depth appears in the code.
- [ ] `TestSinceCoalesces` proves two deltas for one task become one newest-wins entry.
- [ ] `TestEvictedRIDForcesFullUpdate` asserts `FullUpdate` and `SeqGap` are both true.
- [ ] `TestSlowSubscriberIsClosed` asserts `Publish` never blocks.
- [ ] `TestDeltaJSONFieldNames` asserts the marshalled keys are exactly `rid`, `full_update`, `tasks`,
      `tasks_removed`, `stats`, `seq_gap`.
- [ ] `make test PKG=./internal/sync/...` passes with `-race`, which the target already sets.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make test PKG=./internal/sync/... && echo SYNC_OK
```
Expected: `ok  	github.com/L-K-M/dl-tool/internal/sync` on its own line, no `FAIL` and no `DATA RACE`, and a
final line of exactly `SYNC_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT create `internal/api/sse.go` or register `GET /events` and `GET /sync`; T025 owns both endpoints,
  the `retry: 3000` line and the 15-second heartbeat comment.
- Do NOT create `internal/sync/delta.go` or compute a diff from `tasks` rows; T025 owns snapshot diffing.
- Do NOT poll an engine or read the database here; the hub takes deltas from its caller.
- Do NOT wire the `dltool_sse_clients` gauge; T025 connects `Clients` to the metric T010 registered.
- Do NOT implement the client-side reducer or the polling fallback; T051 owns them.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

`make test PKG=./internal/sync/... && echo SYNC_OK`:

```
go test -race -count=1 ./internal/sync/...
ok  	github.com/L-K-M/dl-tool/internal/sync	2.037s
SYNC_OK
```

(Latest run, after all PR-review fixes: cursor-before-snapshot in
`Hub.Snapshot`, re-add-cancels-tombstone and removal-drops-stale-patch in
`coalesceInto`, full-update-replaces-coalesced-state, nil-collection
normalisation, and the new `TestSinceReaddBeatsRemoval`,
`TestFullUpdateReplacesCoalescedState` — including its pre-snapshot tombstone
seeding — and the category side of `TestSinceRemovalBeatsUpdate`.)

`gofmt -l internal/sync` printed nothing; `go vet ./internal/sync/...` and
`golangci-lint run ./internal/sync/...` reported 0 issues.

Scope check (`git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort`):

```
internal/sync/hub.go
internal/sync/ring.go
internal/sync/ring_test.go
```

Audit regressions cover partial-field merging in replay and fan-out, explicit nulls,
large integers and immutable retained patches.

```text
$ make test PKG=./internal/sync/... && echo SYNC_OK
go test -race -count=1 ./internal/sync/...
ok  	github.com/L-K-M/dl-tool/internal/sync	4.377s
SYNC_OK
```

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
