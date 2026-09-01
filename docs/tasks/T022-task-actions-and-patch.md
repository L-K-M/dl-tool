# T022 — Update a task and apply bulk lifecycle actions

| Field | Value |
|---|---|
| **ID** | T022 |
| **Milestone** | M1 |
| **Status** | todo |
| **Depends on** | T019, T020, T021 |
| **Blocks** | T023, T025 |
| **Parallel-safe** | no — extends the shared `internal/api/tasks.go` |
| **Implements** | [FR-014](../02-requirements.md#fr-014-apply-lifecycle-and-queue-actions-to-a-selection) |
| **Decisions** | [ADR-0003](../decisions/0003-chi-huma-code-first-openapi.md), [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| **Est. size** | 2 new files, ~330 LOC |

## Goal
`POST /api/v1/tasks/actions` applies one of the nine actions to up to 500 ids and reports a per-id outcome,
so one bad id never fails the batch. `PATCH /api/v1/tasks/{id}` updates the display name, category, tags,
per-task limits, share limits and the sequential flag.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §5.7 `POST /tasks/actions`](../05-api-contract.md#57-post-tasksactions)
2. [`docs/05-api-contract.md` §5.5 `PATCH /tasks/{id}`](../05-api-contract.md#55-patch-tasksid)
3. [`docs/03-architecture.md` §8.1 Task state machine](../03-architecture.md#81-task-state-machine)
4. [`docs/06-download-engines.md` §4.3 Methods dl-tool calls](../06-download-engines.md#43-methods-dl-tool-calls)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/tasks_actions.go` | create | The `POST /tasks/actions` and `PATCH /tasks/{id}` handlers. |
| `internal/api/tasks_actions_test.go` | create | `humatest` cases for per-id outcomes and for each action. |
| `internal/store/tasks.go` | modify | Add `Update` for the patchable columns. |
| `internal/api/server.go` | modify | Register `task-actions` and `patch-task`. |

No other file may be modified.

## Interface contract

```go
package api

// ActionsInput is the body of POST /tasks/actions.
type ActionsInput struct {
	Body struct {
		IDs        []string `json:"ids"        minItems:"1" maxItems:"500"`
		Action     string   `json:"action"     enum:"pause,resume,remove,recheck,force_complete,queue_top,queue_up,queue_down,queue_bottom"`
		DeleteData bool     `json:"delete_data,omitempty"`
	}
}

// ActionResult is one per-id outcome. Type and Detail are set only when Ok is false and carry a slug
// from the registry in docs/05-api-contract.md §1.3.
type ActionResult struct {
	ID     string `json:"id"`
	Ok     bool   `json:"ok"`
	Type   string `json:"type,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// ActionsOutput is 200 whenever the batch was accepted, whatever the per-id outcomes.
type ActionsOutput struct {
	Body struct {
		Results []ActionResult `json:"results"`
	}
}

func (h *TaskHandlers) Actions(ctx context.Context, in *ActionsInput) (*ActionsOutput, error)

// PatchTaskInput carries only the patchable fields. An omitted field is untouched; an explicit null
// clears a nullable one.
type PatchTaskInput struct {
	ID   string `path:"id"`
	Body struct {
		Name             *string  `json:"name,omitempty"`
		Category         *string  `json:"category,omitempty"`
		Tags             []string `json:"tags,omitempty"`
		DLLimit          *int64   `json:"dl_limit,omitempty"`
		ULLimit          *int64   `json:"ul_limit,omitempty"`
		RatioLimit       *float64 `json:"ratio_limit,omitempty"`
		SeedingTimeLimit *int64   `json:"seeding_time_limit,omitempty"`
		Sequential       *bool    `json:"sequential,omitempty"`
	}
}

func (h *TaskHandlers) PatchTask(ctx context.Context, in *PatchTaskInput) (*TaskOutput, error)
```

Action to engine call and state transition:

| Action | Engine call | Transition |
|---|---|---|
| `pause` | `Engine.Pause` | any → `paused` |
| `resume` | `Engine.Resume` | `paused` → `queued`, released by T098's admission pass |
| `remove` | `Engine.Remove` | any → `removed` |
| `recheck` | `engine.ErrNotSupported` for aria2 | `downloading` → `checking` when supported |
| `force_complete` | `Engine.Remove` with `deleteData` false | any → `completed` |
| `queue_top`, `queue_up`, `queue_down`, `queue_bottom` | none — dl-tool owns the queue | rewrite `tasks.queue_position` only |

## Steps
1. Add `Update` to `internal/store/tasks.go` as one `UPDATE` with an explicit column list covering the
   patchable columns and `updated_at`.
2. Create `internal/api/tasks_actions.go` with the structs above.
3. In `Actions`, validate `ids` and `action` first; an empty list, more than 500 ids or an unknown action
   is `422` `/problems/validation-failed` for the whole request.
4. Load every id in one query, and record `{"ok":false,"type":"/problems/not-found"}` for an id that does
   not exist or belongs to another user when the caller is not an admin.
5. Per id, call the engine, then apply the transition through `store.Transition` with the task-event code
   `task.paused`, `task.resumed`, `task.removed`, `task.rechecking` or `task.force_completed`.
6. Map `engine.ErrUnavailable` to a per-id `/problems/engine-unavailable` and
   `engine.ErrNotSupported` to a per-id `/problems/validation-failed`; leave the task's state unchanged in
   both cases.
7. Implement the four queue actions by rewriting `tasks.queue_position` inside one transaction and calling
   no engine at all.
8. Implement `PatchTask`: reject an unknown category with `422`, apply `dl_limit` and `ul_limit` to a
   running task through `Engine.SetRateLimits` without restarting it, and return the full Task object.
9. Register both operations in `internal/api/server.go`.
10. Create `internal/api/tasks_actions_test.go` with the FR-014 case — pause three ids, one of which does
    not exist, asserting two `ok:true` and one `not-found` — plus one case per action and a `422` case for
    a 501-id batch.

## Acceptance criteria
- [ ] A three-id pause batch with one unknown id returns `200` with two successes and one `not-found`.
- [ ] An unknown `action` returns `422` `/problems/validation-failed` and calls no engine.
- [ ] `recheck` against an aria2 task returns a per-id failure and leaves the state unchanged.
- [ ] The four queue actions change `queue_position` only and issue no engine call.
- [ ] `PATCH` with `dl_limit` calls `SetRateLimits` once and returns the updated Task object.
- [ ] A batch of 501 ids returns `422`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/api/...
```
Expected: `make lint` prints nothing, then `ok  	github.com/L-K-M/dl-tool/internal/api` followed by its
elapsed time, with `TestActionsPerIDOutcomes`, `TestActionsRejectsUnknownAction`,
`TestQueueActionsTouchNoEngine` and `TestPatchTaskAppliesRateLimit` all running. No `FAIL`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT implement `delete_data` unlinking here; T023 owns the six-step delete and T111 owns its full
  semantics.
- Do NOT move data when `destination` changes; T076 owns the cross-filesystem move.
- Do NOT implement share limits against an engine; T036 owns them.
- Do NOT implement `process_order`; T085 owns queue ordering by owner.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
