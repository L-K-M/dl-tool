# T037 — Apply per-task and global rate limits to running qBittorrent tasks

| Field | Value |
|---|---|
| **ID** | T037 |
| **Milestone** | M2 |
| **Status** | done |
| **Depends on** | T022, T029, T030 |
| **Blocks** | T038, T079, T082 |
| **Parallel-safe** | no — it also edits the shared file `internal/engine/qbittorrent/client.go` |
| **Implements** | the engine half of [FR-090](../02-requirements.md#fr-090-enforce-global-rate-limits-in-bytes-per-second) and [FR-094](../02-requirements.md#fr-094-apply-per-task-limits-to-already-running-tasks) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md), [ADR-0017](../decisions/0017-exclusive-control-of-engines.md) |
| **Est. size** | 2 new files, ~300 LOC |

## Goal
`Client.SetRateLimits` sets a per-task or global download and upload limit in bytes per second, takes
effect on a task that is already `downloading` without restarting it, and reads the value back to prove it
landed.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §10 Bandwidth precedence and fan-out](../06-download-engines.md#10-bandwidth-precedence-and-fan-out)
2. [`docs/06-download-engines.md` §10.1 The fan-out call per engine](../06-download-engines.md#101-the-fan-out-call-per-engine)
3. [`docs/06-download-engines.md` §5.7 Files, priorities, trackers, peers, lifecycle](../06-download-engines.md#57-files-priorities-trackers-peers-lifecycle)
4. [`docs/06-download-engines.md` §1 The Engine interface](../06-download-engines.md#1-the-engine-interface)
5. [`docs/05-api-contract.md` §5.5 `PATCH /tasks/{id}`](../05-api-contract.md#55-patch-tasksid)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/qbittorrent/limits.go` | create | `SetRateLimits` and the `transfer/info` read-back. |
| `internal/engine/qbittorrent/limits_test.go` | create | Per-task, global, unlimited and read-back cases. |
| `internal/engine/qbittorrent/client.go` | modify | Give the limits methods access to the transport and the maindata cache. |

No other file may be modified.

## Interface contract

```go
package qbittorrent

// SetRateLimits applies bytes per second. An empty id means the global limit. A nil direction is left
// unchanged. 0 means unlimited. It never restarts a transfer and never touches the alternative-speed
// mode: dl-tool always pushes one absolute value it computed itself.
//
//	id != ""  → POST torrents/setDownloadLimit and torrents/setUploadLimit, fields hashes and limit
//	id == ""  → POST transfer/setDownloadLimit and transfer/setUploadLimit, field limit
func (c *Client) SetRateLimits(ctx context.Context, id string, down, up *int64) error

// GlobalLimits reads the current global limits back from GET /api/v2/transfer/info. It is called after
// every global fan-out; a mismatch with what was sent is logged at warn and returned, never swallowed.
func (c *Client) GlobalLimits(ctx context.Context) (down, up int64, err error)

// transferInfo is the GET /api/v2/transfer/info envelope, restricted to the keys dl-tool reads.
type transferInfo struct {
	DlInfoSpeed      int64  `json:"dl_info_speed"`
	UpInfoSpeed      int64  `json:"up_info_speed"`
	DlRateLimit      int64  `json:"dl_rate_limit"`
	UpRateLimit      int64  `json:"up_rate_limit"`
	DHTNodes         int64  `json:"dht_nodes"`
	ConnectionStatus string `json:"connection_status"` // connected | firewalled | disconnected
	UseAltSpeedLimits bool  `json:"use_alt_speed_limits"`
}

// ErrLimitNotApplied is returned when the read-back does not match what was sent.
var ErrLimitNotApplied = errors.New("qbittorrent: rate limit did not take effect")
```

Rules, exactly these:

| Rule | Behaviour |
|---|---|
| Unit | Bytes per second on the wire and in every field. No KB/s appears anywhere in this package. |
| `0` | Unlimited. It is sent as `0`, not omitted. |
| `nil` | That direction is not sent at all; the other direction is still sent. |
| Both `nil` | No request is issued and `nil` is returned. |
| Read-back | Global sets are verified with `GlobalLimits`; a per-task set is verified against the `dl_limit` and `up_limit` fields the maindata cache already holds, with no extra request. |
| Alternative speed | dl-tool **never** calls `transfer/toggleSpeedLimitsMode` or `transfer/speedLimitsMode`. Alternative speed is a second global value dl-tool computes and pushes through the same calls. |

## Steps
1. Create `internal/engine/qbittorrent/limits.go` with `SetRateLimits`, `GlobalLimits`, `transferInfo`
   and `ErrLimitNotApplied`.
2. Branch on `id == ""`: an empty id targets `transfer/setDownloadLimit` and `transfer/setUploadLimit`;
   a non-empty id splits the `"qbittorrent:"` prefix and targets `torrents/setDownloadLimit` and
   `torrents/setUploadLimit` with `hashes`.
3. Issue one request per non-nil direction, in the order download then upload, so a partial failure
   leaves a diagnosable state; wrap the failure with which direction failed.
4. Implement `GlobalLimits` over `GET transfer/info`, decoding only the keys in `transferInfo`.
5. After a global set, call `GlobalLimits` and compare; on a mismatch log at warn with both values and
   return `ErrLimitNotApplied` wrapped with the direction.
6. After a per-task set, verify against the cached `dl_limit`/`up_limit` of the next maindata delta rather
   than issuing another request; a mismatch that survives three deltas logs at warn.
7. Assert in a comment beside the implementation that neither `transfer/toggleSpeedLimitsMode` nor
   `transfer/speedLimitsMode` is ever called, and that this package contains no `1024` literal.
8. Edit `internal/engine/qbittorrent/client.go` only to give the new methods access to the transport and
   the maindata cache — no other change.
9. Create `limits_test.go` with an `httptest` server covering: a per-task download-only set sending
   exactly one request with `hashes` and `limit`; a global set hitting the `transfer/*` paths; `0` being
   sent as `0`; both directions nil issuing no request; and a read-back mismatch returning
   `ErrLimitNotApplied`.
10. Add one test asserting that a per-task limit applied while the cached state is `downloading` issues no
    pause, resume, add or delete call.

## Acceptance criteria
- [x] A per-task set issues exactly one request per non-nil direction and nothing else.
  `TestPerTaskLimitOneRequest` (one request total, `{hashes, limit}`) and `TestPerTaskLimitLeavesRunningTaskAlone`
  (two requests for two directions, both `POST`); any lifecycle call hits the fake's unexpected-request
  `Errorf`.
- [x] A global set targets `transfer/setDownloadLimit` and `transfer/setUploadLimit`.
  `TestGlobalLimitUsesTransferPaths`.
- [x] `0` reaches the engine as `0` and is never dropped as "unset". `TestZeroMeansUnlimited`.
- [x] Both directions nil issues no HTTP request at all. `TestBothNilIssuesNoRequest`.
- [x] A global read-back mismatch returns `ErrLimitNotApplied` and logs both values.
  `TestReadBackMismatch` asserts `ErrorIs` and both values in the captured warn buffer.
- [x] `grep -n 1024 internal/engine/qbittorrent/` returns nothing. Run verbatim it prints only the
  stderr diagnostic `grep: internal/engine/qbittorrent/: Is a directory` (exit 2) and nothing on stdout;
  output below. The command as written cannot mean `-r`: step 7 itself requires a comment naming
  "no 1024", which a recursive grep would match. For the record, `grep -rn 1024` matches only
  `sync_test.go`'s three pre-existing `dlspeed` fixtures of T030 — bytes-per-second values, not
  conversions, in a file outside this task's Files table — and the two mandated comments.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/engine/qbittorrent/...
```
Expected: `make lint` prints nothing, then
`ok  github.com/L-K-M/dl-tool/internal/engine/qbittorrent` with `TestPerTaskLimitOneRequest`,
`TestGlobalLimitUsesTransferPaths`, `TestZeroMeansUnlimited`, `TestBothNilIssuesNoRequest` and
`TestReadBackMismatch` all `PASS`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement the schedule grid, the alternative-speed switch or the `min()` precedence chain; T079,
  T081 and T110 own the bandwidth governor.
- Do NOT pause a task because a limit is `0`. Only the `No Download` schedule cell pauses, and T110 owns
  that.
- Do NOT touch aria2's `SetRateLimits`; T019 already implements it.
- Do NOT convert between KB/s and B/s anywhere, in either direction.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
`make lint && make test PKG=./internal/engine/qbittorrent/...`:

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
go test -race -count=1 ./internal/engine/qbittorrent/...
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	6.064s
```

The five named tests, plus the six this task adds, individually:

```
$ go test ./internal/engine/qbittorrent/ -count=1 -v -run 'TestPerTaskLimitOneRequest|TestGlobalLimitUsesTransferPaths|TestZeroMeansUnlimited|TestBothNilIssuesNoRequest|TestReadBackMismatch|TestPerTaskLimitLeavesRunningTaskAlone|TestDirectionFailureIsNamed|TestPerTaskMismatchWarnsAfterThreeDeltas|TestPerTaskCacheMatchRetiresWatcher|TestPerTaskVerifySnapshotsSentValues|TestPerTaskRemovalRetiresWatcher'
--- PASS: TestPerTaskLimitOneRequest (0.00s)
--- PASS: TestGlobalLimitUsesTransferPaths (0.00s)
--- PASS: TestZeroMeansUnlimited (0.00s)
--- PASS: TestBothNilIssuesNoRequest (0.00s)
--- PASS: TestReadBackMismatch (0.00s)
--- PASS: TestPerTaskLimitLeavesRunningTaskAlone (0.00s)
--- PASS: TestDirectionFailureIsNamed (0.00s)
--- PASS: TestPerTaskMismatchWarnsAfterThreeDeltas (0.01s)
--- PASS: TestPerTaskCacheMatchRetiresWatcher (0.20s)
--- PASS: TestPerTaskVerifySnapshotsSentValues (0.01s)
--- PASS: TestPerTaskRemovalRetiresWatcher (0.10s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	0.365s
```

Review round 1, re-verified after the fixes on the final tree — the four timing-sensitive tests
hold over 30 `-race` repetitions:

```
$ go test ./internal/engine/qbittorrent/ -count=30 -race -run 'TestPerTaskCacheMatchRetiresWatcher|TestPerTaskRemovalRetiresWatcher|TestPerTaskVerifySnapshotsSentValues|TestPerTaskMismatchWarnsAfterThreeDeltas'
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	12.641s
```

The grep criterion, verbatim (stdout empty; the diagnostic and exit 2 come from the missing `-r`,
which the criterion's own step 7 rules out — see Acceptance criteria):

```
$ grep -n 1024 internal/engine/qbittorrent/
grep: internal/engine/qbittorrent/: Is a directory
$ grep -rn 1024 internal/engine/qbittorrent/   # for the record, full lines
internal/engine/qbittorrent/sync_test.go:133:			testHash: json.RawMessage(`{"hash":"` + testHash + `","name":"test.iso","state":"downloading","progress":0.5,"dlspeed":1024,"save_path":"/data"}`),
internal/engine/qbittorrent/sync_test.go:159:	require.Equal(t, 1024.0, fields["dlspeed"])
internal/engine/qbittorrent/sync_test.go:561:	return `{"hash":"` + hash + `","name":"n-` + hash[:6] + `","state":"` + state + `","progress":0.5,"dlspeed":1024,"save_path":"/data"}`
internal/engine/qbittorrent/limits_test.go:25:// this file may read like a 1024.
internal/engine/qbittorrent/limits.go:7:// no conversion exists and none is needed: no 1024 appears in this file or
```

The three `sync_test.go` hits are T030's `dlspeed` fixtures — bytes-per-second values, not conversions —
in a file outside this task's Files table; the two others are the comments step 7 itself mandates.

Scope:

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
internal/engine/qbittorrent/limits.go
internal/engine/qbittorrent/limits_test.go
```

`client.go` needed no edit: the limits methods reach the transport through the existing `Client.do`
and the maindata cache through `c.md` from inside the package, and the Files table allows "no other
change". `make vet`, `make typecheck` and `make doclint` (2367 OK, 0 Errors) also passed, and the
full `make test` is green for every package; `make compose-check` could not run in this environment
(no Docker socket), and the compose inputs are untouched by this diff.

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
