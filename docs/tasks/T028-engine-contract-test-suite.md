# T028 — Add the shared engine contract test suite

| Field | Value |
|---|---|
| **ID** | T028 |
| **Milestone** | M2 |
| **Status** | todo |
| **Depends on** | T016, T019 |
| **Blocks** | T038, T101 |
| **Parallel-safe** | yes — adds `internal/engine/enginetest/` and one aria2 test file |
| **Implements** | infrastructure for [FR-011](../02-requirements.md#fr-011-maintain-the-canonical-task-state-machine) and [FR-014](../02-requirements.md#fr-014-apply-lifecycle-and-queue-actions-to-a-selection) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| **Est. size** | 3 new files, ~335 LOC |

## Goal
`enginetest.RunContract` exercises the whole `engine.Engine` interface against a real daemon started by
testcontainers, and aria2 is its first call site. An adapter that does not pass it is not done.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §11 The shared contract test suite](../06-download-engines.md#11-the-shared-contract-test-suite)
2. [`docs/06-download-engines.md` §1 The Engine interface](../06-download-engines.md#1-the-engine-interface)
3. [`docs/13-testing-and-verification.md` §4 Adapter contract tests](../13-testing-and-verification.md#4-adapter-contract-tests)
4. [`docs/13-testing-and-verification.md` §1 Test pyramid](../13-testing-and-verification.md#1-test-pyramid)
5. [`docs/14-conventions.md` §2.3 Signatures](../14-conventions.md#23-signatures)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/enginetest/contract.go` | create | `RunContract` and its five subtests. |
| `internal/engine/aria2/contract_test.go` | create | The aria2 call site plus its testcontainers fixture. |
| `deploy/aria2/Dockerfile` | create | Two lines: `FROM alpine:3.22` and `RUN apk add --no-cache aria2`. Nothing else — T115 turns it into the published image. |

No other file may be modified.

## Interface contract

```go
//go:build integration

package enginetest

// RunContract asserts that an Engine implementation honours the interface in
// docs/06-download-engines.md against a real daemon. newEngine must return a connected Engine bound
// to a throwaway container and register its own t.Cleanup.
func RunContract(t *testing.T, newEngine func(t *testing.T) engine.Engine) {
	t.Run("AddURL/Progress/Pause/Resume/Remove", func(t *testing.T) { /* ... */ })
	t.Run("ListReturnsStableIDs", func(t *testing.T) { /* ... */ })
	t.Run("UnknownIDReturnsErrNotFound", func(t *testing.T) { /* ... */ })
	t.Run("SpeedLimitRoundTrips", func(t *testing.T) { /* ... */ })
	t.Run("UnsupportedCapabilityReturnsErrNotSupported", func(t *testing.T) { /* ... */ })
}

// Fixture serves the bytes an AddURL subtest downloads, so no test ever reaches a third-party host.
// It returns the URL of a deterministic 8 MiB body and stops with the test.
func Fixture(t *testing.T) (url string, sha256hex string)

// Has reports whether e declares c. Subtests use it to skip a capability the adapter does not have,
// and to assert ErrNotSupported for every capability it does not declare.
func Has(e engine.Engine, c engine.Capability) bool
```

Subtest obligations, exactly these:

| Subtest | Asserts |
|---|---|
| `AddURL/Progress/Pause/Resume/Remove` | `Add` returns a non-empty engine-namespaced id; `Get` reaches `downloading` and `CompletedBytes` grows; `Pause` reaches `paused`; `Resume` leaves `paused`; `Remove(id, true)` then `Get` returns `engine.ErrNotFound`. |
| `ListReturnsStableIDs` | The id from `Add` appears in `List` and is byte-identical across three consecutive calls. |
| `UnknownIDReturnsErrNotFound` | `Get`, `Files`, `Pause`, `Resume` and `Remove` on a fabricated id return `engine.ErrNotFound`. |
| `SpeedLimitRoundTrips` | `SetRateLimits(ctx, id, &1048576, nil)` succeeds and the daemon reports 1048576 for that task; `SetRateLimits(ctx, "", …)` sets the global limit. |
| `UnsupportedCapabilityReturnsErrNotSupported` | For every `Capability` the adapter does **not** declare, the matching method returns `engine.ErrNotSupported` **and mutates nothing** — re-read state before and after. |

## Steps
1. Create `internal/engine/enginetest/contract.go` with `//go:build integration` on its first line, so
   `make test` stays green with no Docker.
2. Implement `Fixture` with `httptest.NewServer` serving a deterministic 8 MiB body from a fixed seed, and
   return its URL together with the SHA-256 of the body.
3. Implement `Has` over `Engine.Capabilities()`.
4. Implement `RunContract` with exactly the five subtests named above, in that order, each opening its own
   `context.WithTimeout` of 120 s and calling `newEngine(t)` itself so subtests never share a daemon.
5. Poll for state changes with a 250 ms ticker and a deadline, never `time.Sleep` alone; on timeout fail
   with the last `engine.TaskInfo` rendered in the message.
6. Map each undeclared capability to its method in a table: `CapPerFileSelect`→`SetFiles`,
   `CapPerFilePriority`→`SetFiles` with a non-nil `priorities` map, `CapSetLocation`→`SetLocation`,
   `CapRename`→`Rename`, `CapCategories`→`SetCategory`, `CapShareLimits`→`SetShareLimits`.
7. Create `deploy/aria2/Dockerfile` with exactly `FROM alpine:3.22` and `RUN apk add --no-cache aria2`.
   [`13-testing-and-verification.md` §4](../13-testing-and-verification.md#4-adapter-contract-tests) makes the
   contract test build its container from this path, and no earlier task creates it. T115 later adds the
   entrypoint, the flags and the publish matrix; do not add them here.
8. Create `internal/engine/aria2/contract_test.go` with `//go:build integration`, building the container
   with `testcontainers.FromDockerfile` over `deploy/aria2/Dockerfile` and an explicit
   `wait.ForListeningPort("6800/tcp")`; the default wait deadline is 60 s.
9. Reach the `Fixture` server from inside the container by binding `httptest` to `0.0.0.0`, resolving the
   host with `testcontainers.DaemonHost(ctx)` and rewriting the fixture URL's host to it, plus
   `testcontainers.WithHostPortAccess(port)`. A URL of `127.0.0.1:<port>` is the container's own loopback and
   the download will hang until the subtest deadline.
10. Have the container `Cmd` pass `--enable-rpc`, `--rpc-listen-all`, `--dir=/downloads` and `--rpc-secret`
    matching the `Config.Secret` the test constructs, and call `enginetest.RunContract(t, newAria2)`.

## Acceptance criteria
- [ ] `go test ./internal/engine/...` with no build tag compiles and runs without starting a container.
- [ ] `make test-integration` runs all five subtests against a real aria2 container.
- [ ] `UnsupportedCapabilityReturnsErrNotSupported` fails if an adapter silently succeeds on an undeclared
      capability, proved by a temporary local edit that is reverted before committing.
- [ ] No subtest contacts a host outside the container network and the local `httptest` fixture.
- [ ] Both files carry `//go:build integration` on their first line.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make test-integration
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/engine/aria2` with
`--- PASS: TestAria2Contract/AddURL/Progress/Pause/Resume/Remove`,
`/ListReturnsStableIDs`, `/UnknownIDReturnsErrNotFound`, `/SpeedLimitRoundTrips` and
`/UnsupportedCapabilityReturnsErrNotSupported` all `PASS`. No `FAIL`, no `SKIP` at the top level.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add `internal/engine/qbittorrent/contract_test.go`; T038 adds that call site with the adapter.
- Do NOT add `deploy/aria2/entrypoint.sh`, the PUID/PGID drop or the publish matrix; T115 owns them.
- Do NOT add a `StateNormalisationCoversEveryEngineState` subtest here. It needs no container, so it lives
  beside each adapter's mapping table — T018 for aria2, T029 for qBittorrent.
- Do NOT assert the boot conformance probe or engine ownership; T101 and T030 own them.
- Do NOT download from a public tracker, a distribution mirror or any third-party host.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
