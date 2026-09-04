# T022 — Update a task and apply bulk lifecycle actions

| Field | Value |
|---|---|
| **ID** | T022 |
| **Milestone** | M1 |
| **Status** | done |
| **Depends on** | T019, T020, T021 |
| **Blocks** | T023, T025, T036, T037, T044, T082 |
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
| `resume` | none — T098's admission pass calls `Engine.Resume` | `paused` → `queued` |
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
   not exist.
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
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement `delete_data` unlinking here; T023 owns the six-step delete and T111 owns its full
  semantics.
- Do NOT move data when `destination` changes; T076 owns the cross-filesystem move.
- Do NOT implement share limits against an engine; T036 owns them.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

`make lint && make test PKG=./internal/api/...`:

```
$ make lint
golangci-lint run ./...
0 issues.
cd web && npm run lint
> eslint .
cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
$ make test PKG=./internal/api/...
go test -race -count=1 ./internal/api/...
ok  	github.com/L-K-M/dl-tool/internal/api	34.214s
```

`go test -v -run 'TestActionsPerIDOutcomes|TestActionsRejectsUnknownAction|TestQueueActionsTouchNoEngine|TestPatchTaskAppliesRateLimit' ./internal/api/`:

```
=== RUN   TestActionsPerIDOutcomes
--- PASS: TestActionsPerIDOutcomes (0.06s)
=== RUN   TestActionsRejectsUnknownAction
--- PASS: TestActionsRejectsUnknownAction (0.02s)
=== RUN   TestQueueActionsTouchNoEngine
=== RUN   TestQueueActionsTouchNoEngine/queue_top_moves_the_task_to_the_front
=== RUN   TestQueueActionsTouchNoEngine/queue_bottom_moves_the_task_to_the_end
=== RUN   TestQueueActionsTouchNoEngine/queue_up_advances_one_slot
=== RUN   TestQueueActionsTouchNoEngine/queue_down_retards_one_slot
=== RUN   TestQueueActionsTouchNoEngine/a_contiguous_batch_moves_as_a_block
=== RUN   TestQueueActionsTouchNoEngine/a_task_outside_the_queue_fails_per-id
--- PASS: TestQueueActionsTouchNoEngine (0.18s)
=== RUN   TestPatchTaskAppliesRateLimit
--- PASS: TestPatchTaskAppliesRateLimit (0.02s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/api	0.305s
```

Scope check:

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
api/openapi.json
internal/api/server.go
internal/api/tasks_actions.go
internal/api/tasks_actions_test.go
internal/store/tasks.go
web/src/api/schema.d.ts
```

Exactly the Files table plus the two standing `make gen` outputs
(docs/13-testing-and-verification.md §7.1). `make doclint` reports 0 errors.
The full `go test ./...` is green across every package.

## Blocked

Nothing blocking. Three notes:

- `store.Task` carries no `dl_limit`/`ul_limit`/`ratio_limit`/`seeding_time_limit`
  fields (their read path belongs to later tasks, per the comment in
  `internal/store/models.go`), so `PATCH` reads the four columns back from the
  row when it renders the updated Task object; every other render keeps the DDL
  defaults until those tasks add the fields. `models.go` is outside this task's
  Files table, so the columns were not added to the struct here.
- `recheck` rides an optional `recheckable` interface asserted in the handler:
  the base `Engine` interface has no Recheck method, and no M1 engine implements
  it, so the action is a per-id `/problems/validation-failed` everywhere today,
  exactly as the task's action table says. qBittorrent's `torrents/recheck`
  (doc 06 §9) can implement the method when its adapter lands.
- `delete_data` is accepted on the wire but not acted on: the six-step unlink of
  doc 05 §5.6 is T023's, per this task's "Out of scope — do NOT".
