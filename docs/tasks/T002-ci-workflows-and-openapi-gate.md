# T002 — Add the CI workflows and the OpenAPI generation script

| Field | Value |
|---|---|
| **ID** | T002 |
| **Milestone** | M0 |
| **Status** | done |
| **Depends on** | T001 |
| **Blocks** | T014, T097 |
| **Parallel-safe** | yes — touches only `.github/workflows/` and `scripts/gen.sh` |
| **Implements** | [NFR-027](../02-requirements.md#nfr-027-keep-the-generated-api-contract-in-step-with-the-code) |
| **Decisions** | [ADR-0003](../decisions/0003-chi-huma-code-first-openapi.md) |
| **Est. size** | 3 new files, ~160 LOC |

## Goal
Every push and pull request runs `lint`, `test`, `gen-drift`, `integration`, `compose` and `doclint` as
separate jobs, and `scripts/gen.sh` is the only producer of `api/openapi.json` and
`web/src/api/schema.d.ts`. A handler changed without regeneration fails `gen-drift`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/13-testing-and-verification.md` §7 CI](../13-testing-and-verification.md#7-ci) — the job table, the
   pinned action versions and the drift-gate steps.
2. [`docs/13-testing-and-verification.md` §2 Makefile](../13-testing-and-verification.md#2-makefile) — the
   targets each job calls.
3. [`docs/14-conventions.md` §6 Git conventions](../14-conventions.md#6-git-conventions) — generated
   artefacts are committed and never hand-edited.

## Files
| Path | Action | Purpose |
|---|---|---|
| `.github/workflows/ci.yml` | create | The five blocking jobs: `lint`, `test`, `gen-drift`, `integration`, `compose`. |
| `.github/workflows/docs-lint.yml` | create | The `doclint` job. |
| `scripts/gen.sh` | create | Regenerate `api/openapi.json` and `web/src/api/schema.d.ts`. |

No other file may be modified.

## Interface contract

`scripts/gen.sh`, verbatim from doc 13 §7.1, `chmod 0755`:

```bash
#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
go run ./cmd/dl-tool openapi > api/openapi.json
cd web && npx openapi-typescript ../api/openapi.json -o src/api/schema.d.ts
```

The drift gate, as two steps of the `gen-drift` job:

```yaml
      - name: Regenerate the API contract
        run: make gen
      - name: Fail if generated files drifted
        run: git diff --exit-code -- api/openapi.json web/src/api/schema.d.ts
```

Job-to-target mapping — the job names are load-bearing and are referenced by later tasks:

| Workflow | Job id | Command |
|---|---|---|
| `ci.yml` | `lint` | `make lint`, `make vet`, `make typecheck` |
| `ci.yml` | `test` | `make test` |
| `ci.yml` | `gen-drift` | `make gen`, then the `git diff --exit-code` step above |
| `ci.yml` | `integration` | `make test-integration`, behind the guard below |
| `ci.yml` | `compose` | `make compose-check`, `make docker-build`, behind the guard below |
| `docs-lint.yml` | `doclint` | `cargo install --locked lychee --version <pin>`, then `make doclint` |

The `compose` and `integration` jobs have no input files when this task runs — `Dockerfile`,
`compose.yaml` and `compose.dev.yaml` arrive later in M0 with T124 and T125, and the first adapter under
test is T028, in M2. Both jobs therefore start with a guard step, which becomes a no-op as soon as the
files land. This is a guard, not `continue-on-error`: once the file exists the job fails on a real failure.

```yaml
      - id: probe
        run: |
          test -f compose.yaml && echo "ready=yes" >> "$GITHUB_OUTPUT" || echo "ready=no" >> "$GITHUB_OUTPUT"
      - if: steps.probe.outputs.ready == 'yes'
        run: make compose-check && make docker-build
```

The `integration` job uses the same shape with `test -d internal/engine/enginetest`.

Action versions, read from the official READMEs on 2026-09-01 and fixed by doc 13 §7:
`actions/checkout@v4`, `docker/setup-qemu-action@v4`, `docker/setup-buildx-action@v4`,
`docker/login-action@v4`, `docker/build-push-action@v7`, `docker/metadata-action@v6`.
`actions/setup-go` and `actions/setup-node` are pinned at implementation time.

## Steps
1. Create `scripts/gen.sh` with the body above and `chmod 0755` it.
2. Create `.github/workflows/ci.yml` triggered `on: [push, pull_request]`, with one job per row of the table
   above, each running `make setup` before its targets.
3. In the `lint`, `test` and `gen-drift` jobs, check out with `actions/checkout@v4`, then set up Go 1.26 and
   Node 24 with `actions/setup-go` and `actions/setup-node` at versions you pin now.
4. In `gen-drift`, add the two steps from the Interface contract in that order, and no others.
5. Create `.github/workflows/docs-lint.yml` with a single `doclint` job that checks out, installs `lychee`
   with `cargo install --locked lychee --version <the LYCHEE_VERSION T001 pinned>`, then runs `make doclint`.
   It must **not** run `make setup`: `go.mod` and `web/package-lock.json` do not exist yet, and `doclint`
   needs neither. `scripts/doclint.sh` reports a missing `lychee` as a violation, so without this step the
   job is red from its first run.
6. Add the guard step from the Interface contract to the `compose` and `integration` jobs, and to no other
   job.
7. Mark every job blocking; do not add `continue-on-error` and do not add a `release.yml` file.
8. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `.github/workflows/ci.yml` defines exactly the job ids `lint`, `test`, `gen-drift`, `integration` and
      `compose`.
- [ ] `gen-drift` runs `make gen` and then `git diff --exit-code -- api/openapi.json web/src/api/schema.d.ts`.
- [ ] `.github/workflows/docs-lint.yml` defines exactly one job, `doclint`, running `make doclint`.
- [ ] `scripts/gen.sh` is executable, sets `set -euo pipefail`, and writes only the two generated paths.
- [ ] No job carries `continue-on-error`, and no action reference is unpinned.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make doclint && bash -n scripts/gen.sh && echo CI_FILES_OK
```
Expected: no `doclint:` line on stderr, no output from `bash -n`, and the final line of stdout is exactly
`CI_FILES_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT run `make gen`. `cmd/dl-tool` does not exist yet, so the `gen-drift` job is first exercised by T007,
  which commits `api/openapi.json`, and by T014, which commits `web/src/api/schema.d.ts`.
- Do NOT create `.github/workflows/release.yml`; T097 owns the release pipeline, its SBOM and its signing.
- Do NOT create `.github/copilot-instructions.md`; T096 owns the contributor documentation.
- Do NOT hand-write `api/openapi.json` or `web/src/api/schema.d.ts`.
- Do NOT change any Makefile target to make a job pass; the Makefile is owned by T001 and doc 13 §2.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
```text
$ make doclint && bash -n scripts/gen.sh && echo CI_FILES_OK
./scripts/doclint.sh
🔍 2309 Total (in 204ms) 🔗 541 Unique ✅ 2303 OK 🚫 0 Errors 👻 6 Excluded

CI_FILES_OK
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
.github/workflows/ci.yml
.github/workflows/docs-lint.yml
scripts/gen.sh
```

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
