# T133 — Avoid duplicate CI runs on PR updates

| Field | Value |
|---|---|
| Status | done |
| Milestone | M7 |
| Depends on | T002 |
| Parallel-safe | no |
| Est. size | 2 workflow edits and verification docs |

## Goal

Run CI and doclint once per PR update while retaining main and tag checks.
The September 25 audit observed duplicate jobs from branch-push and PR events.

## Context you need

- [Testing and verification §7](../13-testing-and-verification.md#7-ci).
- [GitHub event filters](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#onpushbranchestagsbranches-ignoretags-ignore).

## Files

| File | Purpose |
|---|---|
| `.github/workflows/ci.yml` | Narrow push triggers; preserve all jobs. |
| `.github/workflows/docs-lint.yml` | Apply the same event policy. |
| `docs/13-testing-and-verification.md` | Own the event policy. |
| `docs/tasks/T133-ci-event-dedup.md` | Record verification and disposition. |
| `docs/tasks/00-task-index.md` | Track both task rows. |

## Interface contract

Only event selection changes. Job names, commands, dependencies, required
checks and tool versions retain their existing behavior.

## Steps

1. Restrict push triggers to main and all tags; retain all pull requests.
2. Update the canonical CI policy and link the doclint gate to it.
3. Validate workflow syntax and verify the job bodies are unchanged.
4. Complete PR review and confirm the PR runs one copy of each affected job.

## Acceptance criteria

- [x] A PR update produces one CI run and one doclint run.
- [x] Main pushes and all tag pushes still trigger both workflows.
- [x] All existing job bodies and checks remain unchanged.

## Verification

```bash
make doclint
git diff --check
```

Also run an available actionlint locally and inspect GitHub's PR run events.
Local lint alone does not prove the hosted event dispatch.

## Evidence

Local checks on 2026-09-25:

```text
$ actionlint -shellcheck="" -pyflakes="" .github/workflows/ci.yml .github/workflows/docs-lint.yml
exit 0 (no diagnostics), actionlint v1.7.12
$ compare each jobs: section against the parent revision
.github/workflows/ci.yml: job bodies unchanged
.github/workflows/docs-lint.yml: job bodies unchanged
$ make doclint
2540 Total; 583 Unique; 2511 OK; 0 Errors; 29 Excluded
$ git diff --check && git diff --cached --check
exit 0 (no diagnostics)
```

Event filters retain unfiltered PR events and explicitly include main and
all tags. Hosted dispatch will be checked in this task's PR before merge;
no synthetic tag or release was created for validation.

## Out of scope

Task verification, review, release and pin-bump workflows; job setup, caching,
path filters, cancellation policy, tool upgrades and test changes.
