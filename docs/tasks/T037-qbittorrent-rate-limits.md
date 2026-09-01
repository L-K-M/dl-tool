# T037 — Apply per-task and global rate limits to running qBittorrent tasks

| Field | Value |
|---|---|
| **ID** | T037 |
| **Milestone** | M2 |
| **Status** | todo |
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
- [ ] A per-task set issues exactly one request per non-nil direction and nothing else.
- [ ] A global set targets `transfer/setDownloadLimit` and `transfer/setUploadLimit`.
- [ ] `0` reaches the engine as `0` and is never dropped as "unset".
- [ ] Both directions nil issues no HTTP request at all.
- [ ] A global read-back mismatch returns `ErrLimitNotApplied` and logs both values.
- [ ] `grep -n 1024 internal/engine/qbittorrent/` returns nothing.

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
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
