# T127 — Keep an operator pause authoritative over a disk-full auto-resume

| Field | Value |
|---|---|
| **ID** | T127 |
| **Milestone** | M1 |
| **Status** | todo |
| **Depends on** | T022, T099 |
| **Blocks** | — |
| **Parallel-safe** | yes — code edits stay in the action layer's and the store's files; the only other edit is this task's own row in `00-task-index.md` |
| **Implements** | [FR-048](../02-requirements.md#fr-048-never-destroy-partial-data-when-a-filesystem-fills) |
| **Decisions** | — |
| **Est. size** | 0 new files, ~80 LOC |

## Goal
An operator pause on a task the disk-space guard parked (paused + `error_code = disk_full`) takes
ownership of the row: the hold stamp is cleared, so the admission pass no longer selects the task
and cannot silently un-pause what the operator parked. Today the operator pause keeps the stamp
in both directions — landing on an already-paused row is an idempotent no-op that keeps it, and
on a queued row carrying a `disk_full` hold the stamp survives the transition — and the pass
resumes the task the moment space returns: the reverse hole of the one `PauseDiskFull` already
refuses. The clear runs after every successful pause, which covers both.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/02-requirements.md` FR-048](../02-requirements.md#fr-048-never-destroy-partial-data-when-a-filesystem-fills)
2. [`docs/05-api-contract.md` §5.7](../05-api-contract.md) — the task action table
3. [`docs/tasks/T099-disk-space-reservation.md`](T099-disk-space-reservation.md) — the pass's
   candidate selection (`paused` + `disk_full`) and `PauseDiskFull`'s documented operator-pause
   stance

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/store/tasks.go` | modify | A guarded clear that wipes a hold stamp (`disk_full`, `concurrency_limit`) only off a paused row — the operator-pause counterpart of `ClearHoldCode`'s refusal. |
| `internal/store/tasks_test.go` | modify | The clear's cases: paused+hold clears, paused+other code keeps, any active state keeps, missing id is `ErrNotFound`. |
| `internal/api/tasks_actions.go` | modify | After a successful pause — fresh or the idempotent already-paused branch — clear the hold stamp so the pass stops claiming the row. |
| `internal/api/tasks_actions_test.go` | modify | Operator pause on a guard-parked task keeps it parked across an admission pass that has space and a slot. |
| `docs/tasks/00-task-index.md` | modify | Flip this task's own status row to `done` (step 4); touch no other row. |

No other file may be modified.

## Steps
1. Add the store's guarded clear: one row write matching `state = 'paused'` and
   `error_code IN ('disk_full', 'concurrency_limit')`, clearing the stamp's columns —
   `error_code` and `error_message` — to NULL, never `state`. A row in any
   other state, or carrying any other code, is left untouched and reports success; a missing id is
   `ErrNotFound`. Do not touch `ClearHoldCode` — its paused-row refusal is the release path's
   protection and stays.
2. In the pause action path, once the row is paused (including the idempotent already-paused
   success), call the clear, and log the takeover at info (hold stamp cleared by operator pause)
   so the action is auditable without adding a `task_events` row. The clear must also beat an
   in-flight pass, not just the next tick: verify the pass *atomically claims* the candidate
   after selection and *before* it calls `Engine.Resume` — one guarded row write conditioned on
   `state = 'paused' AND error_code = 'disk_full'`, with `Engine.Resume` gated on the claim
   having taken the row. The claim must be recoverable — claim by transitioning the row, or by
   clearing the stamp and restoring `paused` + `disk_full` when `Engine.Resume` fails; a bare
   stamp-clear on the claim path drops the row out of the candidate query forever. A read-only
   re-check still
   races the clear in the window between the check and the resume call, and a guard that only
   conditions the row update leaves the engine downloading against the operator's pause. If no
   such claim exists, STOP — and first register the atomic claim as its own deferral task that
   owns the admission pass's files (the T099→T126 pattern), re-pointing this row's
   **Depends on** at it: an unowned STOP leaves the exact race this task exists to fix with no
   owner, and criterion 3 uncheckable until that task lands.
   An operator resume needs no counterpart: a queued row's hold stamp is re-evaluated by the
   pass every tick. (`concurrency_limit` is the queued branch's other hold stamp; the candidate
   query never selects paused rows carrying it, so after a pause it is inert — the clear wipes
   it for message accuracy, which is why it is in the allow-list.)
3. Assert end to end: park a task via `Admitter.PauseDiskFull`, pause it through the operator
   action, run one admission pass with admitting space and a free slot, and assert the task is
   still paused and the engine saw no `Resume`.
4. Flip this row to `done` in [`00-task-index.md`](00-task-index.md) in the same commit as the work.

## Acceptance criteria
- [ ] An operator pause on a paused + `disk_full` row (the idempotent branch) clears the stamp
  and writes no pause event; a fresh pause on an active task still writes exactly one.
- [ ] The admission pass does not select the row afterwards: state stays `paused` with space and
  a slot available, and the engine sees no `Resume`.
- [ ] The clear also beats an in-flight pass, not just the next tick: the pass atomically
  claims the candidate (one guarded write conditioned on `state = 'paused' AND
  error_code = 'disk_full'`) after selection, and `Engine.Resume` is gated on the claim having
  taken the row, so a clear landing mid-pass aborts the engine call and the row write together.
- [ ] An operator pause on an active task is unchanged: state, stamp and event land as before.
- [ ] A paused row carrying a non-hold code (e.g. an operator message) keeps it.
- [ ] An operator pause on a queued row carrying a hold stamp clears the stamp with the pause,
  so the parked row is never the pass's candidate — the stamp-survives-the-transition direction
  of the same hole.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then `ok` lines for every `internal/` package. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | cut -c4- | sort
```
Expected: exactly the four code paths in the Files table, and nothing else; the
`docs/tasks/00-task-index.md` edit is hidden by the `:(exclude)docs` pathspec.

## Out of scope — do NOT
- Do NOT change the admission pass's selection or `PauseDiskFull`; T099 owns them and they are
  done.
- Do NOT change the engine-side pause calls of the action; the transfer of a parked task is
  already engine-paused.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
