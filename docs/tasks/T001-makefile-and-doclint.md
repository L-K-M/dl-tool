# T001 — Create the Makefile and the doc-lint script

| Field | Value |
|---|---|
| **ID** | T001 |
| **Milestone** | M0 |
| **Status** | todo |
| **Depends on** | — |
| **Blocks** | T002, T003, T004 |
| **Parallel-safe** | no — it creates the repository root files every later task calls |
| **Implements** | — (foundation; the Definition of Done in [`13-testing-and-verification.md`](../13-testing-and-verification.md#3-definition-of-done) depends on it) |
| **Decisions** | [ADR-0002](../decisions/0002-go-for-the-backend.md) |
| **Est. size** | 3 new files, ~120 LOC |

## Goal
`make doclint` runs and exits 0 against the finished plan documents. Every Makefile target named in
[`13-testing-and-verification.md`](../13-testing-and-verification.md#2-makefile) exists, so every later task
can call it. No Go and no JavaScript source is created here.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/13-testing-and-verification.md` §2 Makefile](../13-testing-and-verification.md#2-makefile) — the
   verbatim Makefile body and the per-target exit behaviour.
2. [`docs/13-testing-and-verification.md` §8 scripts/doclint.sh](../13-testing-and-verification.md#8-scriptsdoclintsh)
   — the verbatim script.
3. [`docs/14-conventions.md` §1 Repository layout](../14-conventions.md#1-repository-layout) — what may exist
   at the repository root.

## Files
| Path | Action | Purpose |
|---|---|---|
| `Makefile` | create | Every target in doc 13 §2, verbatim, plus the two pinned tool versions in the head. |
| `scripts/doclint.sh` | create | The plan-document checker from doc 13 §8, verbatim, `chmod 0755`. |
| `.gitignore` | create | Keep build output, node modules, the database and `.env` out of the index. |

No other file may be modified.

## Interface contract

Makefile head — the two pinned tool versions and the full target list:

```makefile
SHELL   := /bin/bash
GO      ?= go
PKG     ?= ./...
IMAGE   ?= ghcr.io/l-k-m/dl-tool
VERSION ?= dev
GOLANGCI_LINT_VERSION ?= PINME   # pin at implementation time; make setup fails until it is set
LYCHEE_VERSION        ?= PINME   # pin at implementation time; make setup fails until it is set

.PHONY: setup gen lint vet typecheck test test-go test-web test-integration e2e \
        build docker-build compose-check doclint ci
```

`setup` gains one line beyond the body in doc 13 §2, because `scripts/doclint.sh` fails when `lychee` is
absent (doc 13 `## Open questions`):

```makefile
	cargo install --locked lychee --version $(LYCHEE_VERSION)
```

`.gitignore`:

```gitignore
/bin/
/config/
/web/node_modules/
/web/dist/
/web/test-results/
/web/playwright-report/
.env
*.db
*.db-wal
*.db-shm
```

`scripts/doclint.sh` exit contract: `0` = no violation; `1` = at least one violation, each printed to stderr
as `doclint: <reason>`.

## Steps
1. Create `Makefile` with the head above, then the target bodies copied verbatim from doc 13 §2. Change
   nothing about `test`, `test-go`, `test-web`, `test-integration`, `e2e`, `build`, `docker-build`,
   `compose-check`, `doclint` or `ci`.
2. Add the `cargo install` line to `setup`, after the `npx playwright install` line.
3. Create `scripts/doclint.sh` with the script from doc 13 §8, byte for byte, and `chmod 0755` it.
4. Create `.gitignore` with the block above.
5. Install `lychee` locally at a version you pin into `LYCHEE_VERSION`, replacing `PINME`.
6. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `make doclint` exits 0 and prints nothing.
- [ ] `make -n lint`, `make -n test`, `make -n build` and `make -n ci` each expand without a "No rule to make
      target" error.
- [ ] `GOLANGCI_LINT_VERSION` and `LYCHEE_VERSION` both hold a real version string, not `PINME`.
- [ ] `scripts/doclint.sh` is executable and every check from doc 13 §8 is present, none removed.
- [ ] No Go file, no file under `web/`, and no workflow file was created.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make doclint && echo DOCLINT_OK
```
Expected: no `doclint:` line on stderr, and the final line of stdout is exactly `DOCLINT_OK`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT create `scripts/gen.sh` or any file under `.github/`; T002 owns those.
- Do NOT create `go.mod`, `cmd/`, `internal/` or `web/`; T003 and T004 own those. `make lint`, `make vet` and
  `make typecheck` therefore cannot pass yet, and that is expected until T004.
- Do NOT edit any file under `docs/`; the plan documents are finished.
- Do NOT weaken a `doclint` check to make it pass. A failing check means a document is wrong, and documents
  are out of scope here — stop and write it under `## Blocked`.
- Do NOT add `compose.yaml`, `Dockerfile` or `.dockerignore`; T093 owns those.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
