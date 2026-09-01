@AGENTS.md

## Claude Code specifics

- Read the task file first, in full, before touching any code. It caps your reading budget on purpose.
- Use plan mode for any task whose `Est. size` is more than 3 files.
- Use a subagent for repo-wide investigation so the main context stays clean; do not explore files the
  task's `Context you need` section did not name.
- After implementing a task, use a subagent to review the diff against the task file. Tell it to flag
  only gaps that affect correctness or the stated acceptance criteria.
- Run `make lint && make test` before you claim a task is done, and paste the real output into the task
  file's `## Evidence` section. Do not assert success without it.
