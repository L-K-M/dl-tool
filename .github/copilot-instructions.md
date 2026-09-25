# Copilot instructions — dl-tool

[`AGENTS.md`](../AGENTS.md) is the canonical agent brief — read it first. This file is a short mirror
for tools that only load `.github/copilot-instructions.md`; where the two differ, `AGENTS.md` wins.

## Where the plan lives

- Start at [`docs/00-INDEX.md`](../docs/00-INDEX.md) — the map and the reading order.
- Work is defined one task per file in `docs/tasks/`; the task file is authoritative.
- Every *why* lives in `docs/decisions/` — do not re-argue an accepted ADR.

## Picking a task

Take the topmost row in `docs/tasks/00-task-index.md` whose `Status` is `todo` and whose every
`Depends on` entry is `done`; skip `deferred` rows. Open that task file and follow it — do not read
other task files. Branch as `task/T0NN-slug`; commit as `T0NN: <imperative summary>`; one task per PR.

## The seven hard rules

An excerpt of [`docs/14-conventions.md`](../docs/14-conventions.md) §7, not a second source of
truth — update both files in the same commit when §7 changes:

1. NEVER modify a file not listed in the current task file's `Files` table. If you believe you must,
   stop and write why under `## Blocked` in that task file.
2. NEVER skip, xfail or delete a test, weaken an assertion, or add `//nolint` / `_ = err` to make a
   check pass. Fix the cause.
3. NEVER add a dependency without an ADR in `docs/decisions/`, and never change a pinned version.
4. NEVER hardcode a path, port, URL or secret — each is a setting in `docs/11-config-reference.md`.
5. NEVER copy a fact between documents — each fact has exactly one home; link to it.
6. NEVER ship a bundled indexer for a piracy site, and never execute third-party definition code —
   no PHP, no Python plugins, no scripting runtime (ADR-0010).
7. ALWAYS finish a task by running its `## Verification` block, pasting the real output into the task
   file's `## Evidence` section, and flipping its index row to `done` — in the same commit.

Verify with `make lint && make test` before claiming a task is done.
