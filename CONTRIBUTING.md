# Contributing to dl-tool

dl-tool is currently in the **planning** phase. The repository holds a complete implementation plan;
the code does not exist yet.

## If you are an AI coding agent

Read [`AGENTS.md`](AGENTS.md). It tells you where the plan lives, how to pick the next task, which
commands to run, and the seven hard rules. Do not start from this file.

## If you are a human

### Working on the implementation

1. Read [`docs/00-INDEX.md`](docs/00-INDEX.md) for the map and the reading order.
2. Pick the topmost `todo` row in [`docs/tasks/00-task-index.md`](docs/tasks/00-task-index.md) whose
   dependencies are all `done`.
3. Branch as `task/T0NN-slug`. One commit per task, message `T0NN: <imperative summary>`.
4. Only touch the files in that task's `Files` table.
5. Run the task's `## Verification` block, paste the real output into its `## Evidence` section, and set
   the row to `done` in the task index — in the same commit.

The repository-wide Definition of Done is in
[`docs/13-testing-and-verification.md`](docs/13-testing-and-verification.md). A change is not done until
every item on that list is true.

### Changing the plan itself

- A fact belongs to exactly one document. Change it there; everything else links.
- A *decision* changes only by adding a new ADR in [`docs/decisions/`](docs/decisions/) that supersedes
  the old one. Never edit an accepted ADR in place — set its status to `superseded` and link forward.
- Task IDs, requirement IDs and ADR numbers are **immutable**. A dropped task keeps its row with status
  `dropped`. Never renumber.
- Bump `Last reviewed` in the header of any document you edit, in the same commit.
- `make doclint` must pass. It checks for unresolved `[NEEDS CLARIFICATION]` markers, missing task-file
  sections, hedging language outside `docs/decisions/`, and broken relative links.

### Proposing a new feature

Open an issue describing the user-visible behaviour first. If it changes an interface, it needs a
requirement in [`docs/02-requirements.md`](docs/02-requirements.md) and a task before it needs code.

## Releasing

1. Every task in `docs/tasks/00-task-index.md` is `done` or `dropped`.
2. `make ci` is green on `main`.
3. `make test-integration` is green against real qBittorrent and aria2 containers.
4. `make e2e` is green, including the accessibility and installability gates.
5. The yt-dlp pin in `Dockerfile` is at most seven days old.
6. `docs/` carries no `[NEEDS CLARIFICATION]` marker: `make doclint` proves it.
7. Tag `v1.0.0` and push the tag; the `release` workflow publishes and signs both images.
8. Verify the published image before announcing it:

    cosign verify ghcr.io/l-k-m/dl-tool:1.0.0 \
      --certificate-identity-regexp '^https://github\.com/L-K-M/dl-tool/\.github/workflows/release\.yml@refs/tags/v' \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com

9. `docker buildx imagetools inspect ghcr.io/l-k-m/dl-tool:1.0.0` lists `linux/amd64` and `linux/arm64`.
10. A clean `docker compose up -d` from the published tag reaches the first-run wizard.

## Scope boundaries

Contributions that add a bundled indexer for a site whose catalogue is predominantly infringing will be
declined, as will anything that executes third-party definition code — no PHP, no Python plugins, no
scripting runtime. See [`ADR-0010`](docs/decisions/0010-never-execute-third-party-definitions.md).

## Licence of contributions

The project is currently under [The Unlicense](LICENSE); a move to Apache-2.0 is proposed in
[`ADR-0016`](docs/decisions/0016-relicense-to-apache-2.md) and not yet decided. Until that decision is
made, please do not submit substantial contributions — the inbound licence terms are the thing being
settled.
