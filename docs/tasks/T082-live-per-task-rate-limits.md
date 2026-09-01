# T082 — Apply a per-task rate limit to a running task

| Field | Value |
|---|---|
| **ID** | T082 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T022, T037, T079 |
| **Blocks** | T110 |
| **Parallel-safe** | no — extends `internal/engine/bandwidth.go` and `internal/api/tasks_actions.go` |
| **Implements** | [FR-094](../02-requirements.md#fr-094-apply-per-task-limits-to-already-running-tasks) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| **Est. size** | 0 new files, ~180 LOC |

## Goal
`PATCH /tasks/{id}` with `dl_limit` or `ul_limit` reaches the engine immediately, including while the task
is `downloading`, and the transfer does not restart. This is the capability Download Station lacks, so the
test exists to prove dl-tool has it.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §10.1 The fan-out call per engine](../06-download-engines.md#101-the-fan-out-call-per-engine)
2. [`docs/05-api-contract.md` §5.5 `PATCH /tasks/{id}`](../05-api-contract.md#55-patch-tasksid)
3. [`docs/04-data-model.md` §3.3 Tasks](../04-data-model.md#33-tasks)
4. [`docs/02-requirements.md` FR-094](../02-requirements.md#fr-094-apply-per-task-limits-to-already-running-tasks)
5. [`docs/tasks/T079-global-bandwidth-governor.md`](T079-global-bandwidth-governor.md)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/bandwidth.go` | modify | Add `ApplyTask` and the per-task read-back. |
| `internal/engine/bandwidth_test.go` | modify | Per-task fan-out, unlimited and no-restart cases. |
| `internal/api/tasks_actions.go` | modify | Call `ApplyTask` from the `PATCH` handler after the row is written. |
| `internal/api/tasks_actions_test.go` | modify | `humatest` cases for a running task and an unreachable engine. |

No other file may be modified.

## Interface contract

```go
package engine

// ApplyTask pushes a per-task limit to the engine that owns the task, in bytes per second. A nil
// direction is left unchanged at the engine; 0 means unlimited.
//
// engineTaskID is the engine-namespaced id, e.g. "aria2:2089b05ecca3d829". The call must not
// restart, re-add or re-check the transfer:
//   - aria2:       aria2.changeOption([secret], gid, {"max-download-limit","max-upload-limit"}) —
//                  both keys are in the safe list that does not restart the transfer
//   - qBittorrent: POST /api/v2/torrents/setDownloadLimit and .../setUploadLimit, form fields
//                  hashes and limit
//   - yt-dlp:      recorded for the next spawn; a running process is never re-limited, so this
//                  returns ErrNotSupported and the caller records the value only
func (g *Governor) ApplyTask(ctx context.Context, engineTaskID string, down, up *int64) error
```

```go
package api

// PatchTaskInput already carries the two limit fields (T022). After the row is updated the handler
// calls the governor and reports an engine failure per the table below; the stored value is kept
// either way, so a later boot reconciliation re-pushes it.
type PatchTaskBody struct {
	DownloadLimit *int64 `json:"dl_limit,omitempty" minimum:"0" doc:"bytes per second, 0 = unlimited"`
	UploadLimit   *int64 `json:"ul_limit,omitempty" minimum:"0" doc:"bytes per second, 0 = unlimited"`
	// … the other patchable fields of doc 05 §5.5 are unchanged
}
```

| Outcome | Response |
|---|---|
| Engine accepted | `200` with the updated task object, `dl_limit` and `ul_limit` as sent. |
| Engine returned `ErrNotSupported` (yt-dlp) | `200`; the value is stored and applies at the next spawn. |
| Engine unreachable | `503` `/problems/engine-unavailable`; the row keeps the new value. |

## Steps
1. Add `ApplyTask` to `internal/engine/bandwidth.go`, resolving the engine from the namespace prefix of
   `engineTaskID` and passing the bare engine reference to `Engine.SetRateLimits`.
2. Treat `ErrNotSupported` as success at the API layer: store the value, report `200`, and let the next
   spawn pick it up.
3. Edit `internal/api/tasks_actions.go` so the `PATCH /tasks/{id}` handler calls `ApplyTask` after the row
   is written, never before, and maps `engine.ErrUnavailable` to `503` `/problems/engine-unavailable`.
4. Keep the stored `dl_limit` and `ul_limit` on an engine failure so boot reconciliation re-pushes them.
5. Never pause, resume, re-add or re-check the task as part of applying a limit.
6. Extend `internal/engine/bandwidth_test.go` with a fake engine: assert the per-task call carries the bare
   engine reference and not the namespaced id; assert `0` is pushed as unlimited; assert a `nil` direction
   is omitted from the call; assert no lifecycle method is invoked during `ApplyTask`.
7. Extend `internal/api/tasks_actions_test.go`: assert a `PATCH` on a task in state `downloading` returns
   `200`, that the fake engine recorded the new limit, and that the task's state and `completed_bytes` are
   unchanged; assert an unreachable engine yields `503` with the row still carrying the new value.
8. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] A limit set on a `downloading` task reaches the engine without a restart, re-add or recheck.
- [ ] The task's state and `completed_bytes` are unchanged by the call.
- [ ] `0` is applied as unlimited; a `nil` direction is left untouched at the engine.
- [ ] yt-dlp's `ErrNotSupported` yields `200` with the value stored for the next spawn.
- [ ] An unreachable engine yields `503` `/problems/engine-unavailable` and the row keeps the new value.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/engine/... ./internal/api/..." && echo TASK_LIMIT_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/engine` and `ok  github.com/L-K-M/dl-tool/internal/api`,
with `TestApplyTaskUsesBareEngineRef`, `TestApplyTaskZeroIsUnlimited`, `TestApplyTaskNoLifecycleCalls`,
`TestPatchLimitOnRunningTask` and `TestPatchLimitEngineUnavailable` each reported as `--- PASS`. The final
line of stdout is exactly `TASK_LIMIT_OK`. No `FAIL`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT resolve the `min()` chain between the schedule cell, the global limit and this one; T110 owns it.
- Do NOT change the global fan-out or `ApplyGlobal`; T079 owns them.
- Do NOT add a per-task limit column, field or endpoint; `tasks.dl_limit` and `tasks.ul_limit` already exist
  and `PATCH /tasks/{id}` already accepts them.
- Do NOT restart, re-add or re-check a transfer to make a limit take effect.
- Do NOT use aria2 options outside the safe list; changing an unsafe option restarts the transfer.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
