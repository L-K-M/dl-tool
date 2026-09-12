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

## Pull request babysitting

When you push a branch and open a pull request in this repo (or the user points
you at one), subscribe to it with `subscribe_pr_activity` immediately — don't
wait to be asked. Also arm an hourly `send_later` self check-in (webhooks don't
deliver CI successes, new pushes, or merge-conflict transitions). Then handle
review feedback under this policy:

### Respond to every round on its merits
- Triage each comment into exactly one of: **apply** (real bug or improvement),
  **decline with recorded reasons** (commit message and chat), or **refute with
  evidence** (official docs, actual CI runs, the code itself) when a claim is
  factually wrong.
- Verify factual claims against primary sources before acting on them. Never
  apply a change just to appease a reviewer.
- Never flip-flop: if an earlier round declined something for stated reasons,
  don't apply it later unless genuinely new evidence appears. Keep a running
  list of what was declined and why.
- Post a PR comment only when an incorrect claim would otherwise mislead a
  merge decision (e.g. "this won't compile"); otherwise let commit messages and
  chat summaries carry the record.

### Declare steady-state and stop when any of these hold
- Two consecutive rounds yield no valid, actionable findings (only nits,
  restatements, or self-answered "✅ fine" items),
- the reviewer re-raises items already declined with reasons, or contradicts
  its own earlier feedback,
- everything remaining is out of scope for the PR (pre-existing behavior,
  product decisions) — collect those as follow-up suggestions instead.

At steady-state: post a short scorecard in chat (what was real, what was
refuted, what's deferred), state that the PR is merge-ready, call
`unsubscribe_pr_activity`, and delete any pending self check-in triggers for
that PR. Ignore further automated review rounds after that point.

**Exceptions:** comments from human reviewers are never subject to the
steady-state cutoff — always address them. And always unsubscribe when the PR
is merged or closed, or the user says stop.
