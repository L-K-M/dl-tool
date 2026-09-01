# Copilot instructions — dl-tool

See [`../AGENTS.md`](../AGENTS.md) for the full instructions. It is the canonical file.

The three things you most need to know:

1. The plan lives in `docs/`. Start at `docs/00-INDEX.md`; work is defined one task per file in
   `docs/tasks/`, picked via `docs/tasks/00-task-index.md`.
2. Only modify files listed in the current task file's `Files` table.
3. Verify with `make lint && make test` and paste the output into the task file's `Evidence` section.
