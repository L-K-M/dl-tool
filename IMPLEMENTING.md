# Implementing dl-tool

This repository contains a plan, not an implementation. This file is the standing
brief for the agent that builds it. Point an agent at this file and it has
everything it needs; `AGENTS.md` and [`docs/00-INDEX.md`](docs/00-INDEX.md) remain
authoritative on the plan itself and this file does not restate them.

It lives at the repository root, alongside [`PLAN-REVIEW.md`](PLAN-REVIEW.md), so
`scripts/doclint.sh` does not scan it and the plan's own gate stays green.

---

## The loop

1. Open [`docs/tasks/00-task-index.md`](docs/tasks/00-task-index.md). Take the
   topmost row whose `Status` is `todo` and whose every `Depends on` entry is
   `done`. Skip `deferred` rows.
2. Read that task file in full, before touching any code. Read the documents its
   `## Context you need` section names, and nothing else. The reading budget is
   deliberate — do not explore the repo to "get oriented".
3. Implement it on a branch named `task/T0NN-slug`. Touch only the files in its
   `## Files` table. Two standing exceptions are defined in
   [`docs/13-testing-and-verification.md`](docs/13-testing-and-verification.md)
   §7.1: `api/openapi.json` and `web/src/api/schema.d.ts` are implicitly part of the `Files` table of any
   task that registers, removes or changes a Huma operation or one of its request/response structs (run
   `make gen` and commit both), and `go.mod`/`go.sum` are implicitly
   part of the `Files` table of any task that first imports an already-pinned dependency (commit exactly
   what `go mod tidy` produces).
4. Run the task's `## Verification` block. Paste the real output into its
   `## Evidence` section. Never write Evidence you did not observe.
5. Flip the task's row to `done` in the index, in the same commit as the work.
   Commit message `T0NN: <imperative summary>`, one commit per task.
6. Open a pull request for that one task, address the review, and get it
   **merged** before starting the next one.

Step 6 is not optional bookkeeping. Rule 9 of the Definition of Done puts the
flip of the index row to `done` inside the task's own commit, so until that
commit is on `main` the row still reads `todo` there. A session that stops at
"PR opened" leaves the next session — which starts from `main` — picking the
same task again. The loop only advances when the PR merges.

M0 is blocking: nothing outside it starts until every M0 row is `done`.

## Pull requests: one task, one PR

Every PR here is reviewed by the `GLM 5.3 PR Review` workflow, and its runtime
scales with the diff. An 84-file PR took over 90 minutes; a single task's diff
takes a few. Keep them small:

- **One task per PR. Never batch tasks.** Even two small adjacent tasks go in two
  PRs. The `Est. size` field in each task file tells you roughly what the diff
  should be — a handful of files and a few hundred lines. If your diff is much
  bigger than that field predicts, you have gone outside the task.
- **Include the plan edits.** The pasted `## Evidence` and the flipped index row
  belong in the same PR as the code. They are small, and the Definition of Done
  requires them together.
- **Nothing else.** No drive-by formatting, no unrelated renames, no "while I was
  here" fixes. If you spot something outside the task, note it in the PR
  description and leave it.
- **Do not open several PRs at once.** Concurrent runs of the review workflow
  cancel each other. One open PR at a time: wait for the review, address it,
  merge, then start the next task.
- **PR description**: what the task was, what you built, a summary of the
  Verification output, and anything you wrote under `## Blocked`. Link the task
  file.

If a task is genuinely too large to review in one PR, that is a defect in the
task, not a reason to split it across PRs — the Definition of Done ties the code,
the Evidence and the index row to one commit. Stop and say so instead.

## When to stop and ask

Write your reason under `## Blocked` in the task file and stop, rather than
improvising, when any of these happen:

- The task cannot be done without editing a file outside its `Files` table. That
  is a planning error and it needs to be seen. Do not silently widen the scope.
- Two documents contradict each other on a fact you must act on.
- A decision in [`docs/decisions/`](docs/decisions/) looks wrong. Do not re-argue
  it inline.
- An acceptance criterion cannot pass without weakening it.

Never satisfy a check by skipping or deleting a test, weakening an assertion, or
adding `//nolint` or `_ = err`. Never add a dependency or change a pinned version
without an ADR. Never hardcode a path, port, URL or secret — every one of them is
a setting in [`docs/11-config-reference.md`](docs/11-config-reference.md).

## Things this plan gets wrong in a specific way

The consistency review of 2026-09-02 fixed a great deal, but the failure modes it
found will recur. Watch for them:

- **Components that are built and never wired.**
  [`docs/14-conventions.md` §8.3](docs/14-conventions.md#83-wire-a-long-lived-component)
  is the rule: if you write a constructor, start a goroutine or register a job
  handler, you must also add the call site at the composition root
  (`cmd/dl-tool/main.go` or `internal/api/server.go`) and an acceptance criterion
  that observes it through that root. A `NewX` with no caller is not done.
- **Claims about external tools.** The plan's research is good but not perfect.
  Where a task asserts what aria2, qBittorrent, yt-dlp, 7-Zip or testcontainers
  does, verify it against the pinned version before building on it. If reality
  differs, stop and say so with the command output — do not code around it.
- **`UNVERIFIED` and `[NEEDS CLARIFICATION]` markers.** These are real open
  questions. If one blocks your task, that is a `## Blocked`, not a guess.

## Definition of done

The plan is done when every task row is `done` or `deferred`, every milestone
exit checkpoint in [`docs/00-INDEX.md`](docs/00-INDEX.md) passes, and `make ci`
is green.

Two items are deliberately unresolved and need an ADR from the repository owner
before their tasks can run. Surface them when you reach them rather than deciding
yourself:

- **T088** (`deferred`): routing a URL to the yt-dlp lane. The mechanism the plan
  assumed does not exist — see
  [`docs/06-download-engines.md` §7.2](docs/06-download-engines.md#72-routing-check).
  Until it is decided, `MediaMatcher` stays nil and such URLs route to aria2.
- **`ffmpeg` in the runtime image**, and whether Alpine's `7zip` carries the RAR
  codec — both open questions in
  [`docs/06-download-engines.md`](docs/06-download-engines.md#open-questions).

Report after each milestone: what you built, what the exit checkpoint printed, and
anything you wrote under `## Blocked`.

## A note on sessions

Do not attempt all 119 tasks in one session. Context degrades and the Evidence
discipline is the first thing to go. The plan is built for one task per session:
start fresh from `main`, pick the next row, and stop once that task's PR is
merged — not once it is opened, for the reason under the loop above.

If you are not permitted to merge, say so and stop there rather than starting the
next task on top of an unmerged branch. Stacking tasks on one branch breaks the
one-task-one-PR rule and makes the review useless.
