# T028 — Add the shared engine contract test suite

| Field | Value |
|---|---|
| **ID** | T028 |
| **Milestone** | M2 |
| **Status** | done |
| **Depends on** | T016, T019 |
| **Blocks** | T038, T090, T101 |
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
| `internal/engine/aria2/client.go` | modify | *Widened mid-task, see [`## Blocked`](#blocked):* aria2 serves every JSON-RPC fault as HTTP 400 with the fault object in the body; `post` now decodes a 400 instead of reporting `ErrUnavailable`, so the fault→error mapping of §4.7 actually runs. |
| `internal/engine/enginetest/contract_speedlimits_test.go` | create | *Added by the 2026-09-08 readback repair, see [Evidence](#evidence):* regression coverage for the exact-setting obligation — a daemon holding three quarters of the request fails, and so does an engine with no readback. |

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

// DownloadLimitReadback is implemented by the engine a call site returns: SpeedLimitRoundTrips
// reads the daemon's configured limit back through it and asserts the exact requested value,
// per task and globally — the obligation "the daemon reports 1048576" made testable.
type DownloadLimitReadback interface {
	DaemonDownloadLimit(ctx context.Context, id string) (int64, error)
}
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
9. Reach the `Fixture` server from inside the container by binding `httptest` to `0.0.0.0`, passing
   `testcontainers.WithHostPortAccess(port)`, and rewriting the fixture URL's host to
   `testcontainers.HostInternal` (`"host.testcontainers.internal"`, `port_forwarding.go`) — that option is
   what makes the name resolve. Do **not** use `DaemonHost`: in the pinned v0.44.0 it is a method on
   `*DockerProvider`, not a package function, and it names the Docker daemon rather than the test process.
   A URL of `127.0.0.1:<port>` is the container's own loopback and the download hangs until the subtest
   deadline.
10. Have the container `Cmd` pass `--enable-rpc`, `--rpc-listen-all`, `--dir=/downloads` and `--rpc-secret`
    matching the `Config.Secret` the test constructs, and call `enginetest.RunContract(t, newAria2)`.

## Acceptance criteria
- [x] `go test ./internal/engine/...` with no build tag compiles and runs without starting a container.
- [x] `make test-integration` runs all five subtests against a real aria2 container.
- [x] `UnsupportedCapabilityReturnsErrNotSupported` fails if an adapter silently succeeds on an undeclared
      capability, proved by a temporary local edit that is reverted before committing.
- [x] No subtest contacts a host outside the container network and the local `httptest` fixture.
- [x] Both files carry `//go:build integration` on their first line.

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

### Exact-setting readback repair (recorded 2026-09-08)

History first. PR #101 merged the original task at c4b098d on 2026-09-08T04:29:10Z. Its
`go.mod` change — promoting testify and testcontainers-go to direct requires — was **hand-edited, not
produced by `go mod tidy`** (a full tidy at the time also dropped 86 lines of pre-pinned indirect
requires for later tasks). `docs/13` §7.1 requires committing exactly what tidy produces. That is a
procedural breach and it stands as one: the current `go mod tidy -diff` at this repair's commit is
empty (verified below), which resolves the drift, not the breach.

PR #102 (60195fb, merged 2026-09-08T04:58:37Z) added the elapsed-time ceiling to reject a
daemon that applies a lower cap. An independent audit on 2026-09-08 proved it insufficient: against
native aria2 1.37.0 asked for 1048576 B/s while the daemon was actually configured 786432 B/s
(three quarters), both `SpeedLimitRoundTrips` phases still passed — 8 MiB at 786432 B/s takes
~10.7 s, inside the [5.6 s, 14.4 s] window; only a cap below ~0.56× of the request trips the
ceiling. The exact-setting obligation above ("the daemon reports 1048576 for that task") was never
verified, and both repairs merged without renewed Astra verification. That rejection is what this
repair resolves.

What changed:

- `SpeedLimitRoundTrips` now requires a `DownloadLimitReadback` from the call site's engine and
  asserts the daemon's configured limit equals the request — per task and globally, read back while
  the task is still parked, before any throttled byte moves.
- The aria2 call site wraps its client in `readbackClient`, which queries `aria2.getOption` /
  `aria2.getGlobalOption` itself; it rides the client's transport but decodes the answer on its
  own, so the suite learns what the daemon is configured with, never what the adapter was asked to
  set.
- `internal/engine/enginetest/contract_speedlimits_test.go` pins the obligation with fakes that
  need no daemon: an exact fake passes (and the suite's two consultations are recorded, per task
  then global), while a three-quarters daemon and an engine without a readback each fail the
  suite — asserted by re-executing the test binary against them, because a failure the suite
  correctly records would also fail this test's own tree.
- `TestAria2DaemonLimitReadback` (CI) pins the real readback to daemon truth: adapter-set 1048576
  reads back exactly, and a wrong 786432 injected straight into the daemon through
  `changeOption`/`changeGlobalOption` — bypassing the adapter — reads back as the injected value,
  so the readback cannot be satisfied by echoing a request.

Regression proof, observed locally in dependency order (this machine has no Docker; containers run
in the CI `integration` job). The fakes against the **unrepaired** suite — all three cases red,
which is the original defect reproduced:

```
$ go test -tags=integration -count=1 -v -run 'TestSpeedLimitsReadBackTheDaemonLimit' ./internal/engine/enginetest/
=== RUN   TestSpeedLimitsReadBackTheDaemonLimit/exact_readback_passes_and_is_consulted
        Error:  Not equal:
        Messages: the suite must read both limits back from the daemon
=== RUN   TestSpeedLimitsReadBackTheDaemonLimit/the_suite_rejects_the_fraction_engine
        Error:  An error is expected but got nil.
        Messages: SpeedLimitRoundTrips must fail the fraction engine, but it passed. Output:
=== RUN   TestSpeedLimitsReadBackTheDaemonLimit/the_suite_rejects_the_no-readback_engine
        Error:  An error is expected but got nil.
        Messages: SpeedLimitRoundTrips must fail the no-readback engine, but it passed. Output:
--- FAIL: TestSpeedLimitsReadBackTheDaemonLimit (53.74s)
FAIL	github.com/L-K-M/dl-tool/internal/engine/enginetest
```

The same fakes against the repaired suite — green, both dishonest engines rejected by
re-executed processes:

```
$ go test -tags=integration -count=1 -v -run 'TestSpeedLimitsReadBackTheDaemonLimit' ./internal/engine/enginetest/
=== RUN   TestSpeedLimitsReadBackTheDaemonLimit/exact_readback_passes_and_is_consulted
=== RUN   TestSpeedLimitsReadBackTheDaemonLimit/the_suite_rejects_the_fraction_engine
=== RUN   TestSpeedLimitsReadBackTheDaemonLimit/the_suite_rejects_the_no-readback_engine
--- PASS: TestSpeedLimitsReadBackTheDaemonLimit (16.16s)
    --- PASS: .../exact_readback_passes_and_is_consulted (16.05s)
    --- PASS: .../the_suite_rejects_the_fraction_engine (0.09s)
    --- PASS: .../the_suite_rejects_the_no-readback_engine (0.03s)
ok  	github.com/L-K-M/dl-tool/internal/engine/enginetest
```

Mutation check — the readback assertions stripped from the suite locally (restored before the
commit; `git diff` afterwards shows only this repair's intended change), both reject cases fail:

```
$ go test -tags=integration -count=1 -run 'TestSpeedLimitsReadBackTheDaemonLimit' ./internal/engine/enginetest/
    Error:  An error is expected but got nil.
    Messages: SpeedLimitRoundTrips must fail the fraction engine, but it passed. Output: …
    Error:  An error is expected but got nil.
    Messages: SpeedLimitRoundTrips must fail the no-readback engine, but it passed. Output: …
--- FAIL: TestSpeedLimitsReadBackTheDaemonLimit (53.73s)
```

Review hardening (GLM 5.3 round 1, verified after adoption): the child-process assertion pins
exit code 1 — a genuine test failure — so a crash or timeout in the child cannot pass vacuously.
Proved with a temporary panicking engine target (reverted before the commit):

```
$ go test -tags=integration -count=1 -run '.../the_suite_rejects_the_panic' ./internal/engine/enginetest/
    Error:  Not equal:
    Messages: the suite must reject the panic engine with a test failure (exit 1), not a crash or timeout. Output: …
--- FAIL: TestSpeedLimitsReadBackTheDaemonLimit (0.10s)
```

The fake's `Get` also reports a task that was never resumed as `paused` instead of a completed
transfer, and the consulted-readback assertions name count and order separately. After these
fixes the fakes are green again (`--- PASS: TestSpeedLimitsReadBackTheDaemonLimit (16.15s)`),
`golangci-lint` reports `0 issues` with and without the integration tag, `go vet` and `make
doclint` are clean, and `go test -race ./internal/engine/...` passes.

Full local checks on the repaired tree: `gofmt -l` empty; `go vet ./...` and
`go vet -tags=integration ./internal/...` clean; `golangci-lint run ./...` and
`golangci-lint run --build-tags integration ./internal/engine/...` — `0 issues`; `make lint`,
`make typecheck`, `make doclint` clean; `go mod tidy -diff` empty; race tests green:

```
$ go test -race -count=1 ./internal/...
ok  	github.com/L-K-M/dl-tool/internal/api	49.497s
ok  	github.com/L-K-M/dl-tool/internal/config	1.152s
ok  	github.com/L-K-M/dl-tool/internal/engine	21.061s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.202s
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	4.530s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.024s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.500s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.167s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.117s
ok  	github.com/L-K-M/dl-tool/internal/store	66.629s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.363s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.032s
```

The Files table gained one row for `contract_speedlimits_test.go` in this repair — the same
widening precedent as `client.go` below, recorded here because the regression coverage the
rejection demanded has no home otherwise.

Against **real aria2** — the audit's own scenario, replayed on the repaired code. A native aria2
1.37.0 (the audit's binary) replaced the container through a temporary overlay harness (the
audit's `audit_native_test.go`, given a `DaemonDownloadLimit` that queries `getOption` /
`getGlobalOption` directly; file overlaid into `internal/engine/aria2` as
`zz_audit_native_repair_test.go` — an overlay leaves the worktree untouched, so nothing of it is
committed; overlay `/tmp/native-repair-overlay.json`). The wrapper applies a fraction of every
requested limit and the unchanged suite decides — requested 1048576 while the daemon is actually
configured:

```
$ DLTOOL_AUDIT_ARIA2_BINARY=<native aria2 1.37.0> go test -mod=readonly -tags=integration -count=1 -v \
    -overlay=/tmp/native-repair-overlay.json \
    -run 'TestAuditNativeRateContract/(exact|three_quarters)/SpeedLimitRoundTrips$' ./internal/engine/aria2
=== RUN   TestAuditNativeRateContract/exact/SpeedLimitRoundTrips
    requested=1048576 daemon_max-download-limit=1048576
    requested=1048576 daemon_max-overall-download-limit=1048576
=== RUN   TestAuditNativeRateContract/three_quarters/SpeedLimitRoundTrips
    requested=1048576 daemon_max-download-limit=786432
        Error:  Not equal: expected: 1048576, actual: 786432
        Messages: the daemon is configured with 786432 B/s download limit for task aria2:42b6e2678e3aa589, not the requested 1048576 B/s
--- FAIL: TestAuditNativeRateContract (17.69s)
    --- PASS: TestAuditNativeRateContract/exact (17.63s)
    --- FAIL: TestAuditNativeRateContract/three_quarters (0.06s)
```

Exact cap passes; the audit's three-quarters daemon — which passed the pre-repair suite — is now
rejected at the readback before a single throttled byte moves.

`make test-integration` on this PR's CI `integration` job (GitHub Actions, ubuntu-latest; this dev
machine has no Docker, as before). Container lifecycle noise elided, nothing else changed:

```
$ make test-integration
go test -tags=integration -count=1 -timeout=20m ./internal/engine/...
ok  	github.com/L-K-M/dl-tool/internal/engine	8.953s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	96.771s
ok  	github.com/L-K-M/dl-tool/internal/engine/enginetest	16.075s
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	3.374s
```

`ok` with no `--- FAIL` and no `--- SKIP`: the runner is not verbose, but this job printed
`--- FAIL:` subtest lines on every genuinely failing run of the original task (see `## Blocked` and
the flake rounds below), so failures surface at this verbosity. The aria2 package grew from ~62 s
to ~97 s — `TestAria2Contract` now also performs the two readback assertions, and
`TestAria2DaemonLimitReadback` adds its own container round; `enginetest`'s 16.075 s are the
committed fake subtests.

#### Original task runs (historical — describe commit 91e6d24 and PR #101's CI, superseded above where the repair speaks)

`make test-integration` needs a Docker daemon; this dev machine has none (no
CLI, no socket), so the command ran on this branch's CI `integration` job
(GitHub Actions, ubuntu-latest, commit `91e6d24`; the only later commit
on the branch touches this Evidence text and nothing else) — the job the
docs/13 §4-gated workflow starts precisely because `internal/engine/enginetest`
now exists. Output verbatim (container lifecycle noise elided; nothing else
changed):

```
go test -tags=integration -count=1 -timeout=20m ./internal/engine/...
ok  	github.com/L-K-M/dl-tool/internal/engine	2.011s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	62.013s
?   	github.com/L-K-M/dl-tool/internal/engine/enginetest	[no test files]
```

`ok` with no `--- FAIL` and no `--- SKIP`: the runner is not verbose, but
this job printed `--- FAIL:` subtest lines on every genuinely failing run
(see `## Blocked` and the flake rounds below), so failures do surface at
this verbosity — their absence on the green run means all five subtests
passed. The ~62 s package time matches five subtests each starting a
container and a throttled ~8 s transfer.

Two intermediate CI runs flaked on `SpeedLimitRoundTrips` — aria2 reported
1452256 (1.385×) and then 2994097 (2.855×) B/s under the 1048576 B/s cap
while the transfer itself honoured it (~10 s per 8 MiB phase). The windowed
`downloadSpeed` is bursty by construction, so the instantaneous rate
ceiling was replaced by the average-rate bound (`elapsed ≥ 0.7 ×
bytes/cap`); the recorded flake numbers live in the comment on
`assertThrottled`.

Acceptance criterion 1, locally with no build tag and no Docker:

```
$ go test ./internal/engine/...
ok  	github.com/L-K-M/dl-tool/internal/engine	1.657s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	(cached)
```

Acceptance criterion 3, proved with a temporary in-repo fake engine
(`internal/engine/enginetest/fake_proof_test.go`, deleted before the commit;
the fake declares aria2's set and `Rename` returns nil — a silent success on
an undeclared capability):

```
$ go test -tags=integration -count=1 -run 'TestFakeProof/UnsupportedCapability' ./internal/engine/enginetest/
--- FAIL: TestFakeProof/UnsupportedCapabilityReturnsErrNotSupported (0.06s)
        Error:       Expected error with "engine: capability not supported" in chain but got nil.
        Messages:    rename is not declared, so its method must refuse
FAIL
```

With the same fake returning `engine.ErrNotSupported` (control), the subtest
passes:

```
$ go test -tags=integration -count=1 -v -run 'TestFakeProof/UnsupportedCapability' ./internal/engine/enginetest/
=== RUN   TestFakeProof/UnsupportedCapabilityReturnsErrNotSupported
--- PASS: TestFakeProof/UnsupportedCapabilityReturnsErrNotSupported (0.05s)
PASS
```

Scope, as the Verification block prescribes:

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
deploy/aria2/Dockerfile
go.mod
internal/engine/aria2/client.go
internal/engine/aria2/contract_test.go
internal/engine/enginetest/contract.go
```

Exactly the Files table (including the widened `client.go` row) plus the
`go.mod` promotion of the two already-pinned imports allowed by
`docs/13-testing-and-verification.md` §7.1.

## Blocked

*Resolved — see the end of this section — but recorded because it forced a
Files-table widening before the out-of-table edit was made.*

The suite is complete and three of five subtests pass against a real
container, but `UnknownIDReturnsErrNotFound` and the `Get`-after-`Remove`
step of the lifecycle subtest cannot pass without editing a file outside
this task's Files table:

- aria2 answers **every JSON-RPC fault with HTTP 400**, not 200 —
  `HttpServerBodyCommand::sendJsonRpcResponse` maps fault code 1 to 400
  (aria2 release-1.37.0 source, `src/HttpServerBodyCommand.cc`).
- The T019 client's `post` treated any non-200 as
  `engine.ErrUnavailable` and discarded the body, so `rpcReply.result` —
  the fault-message→`ErrNotFound` mapping this task's obligations mandate —
  was unreachable for single calls over HTTP. Batch replies are always 200,
  which is why T019's own tests never caught it.

Observed on this branch's first CI run (GitHub Actions `integration` job,
real aria2 1.37.0 container from `deploy/aria2/Dockerfile`):

```
--- FAIL: TestAria2Contract (65.55s)
    --- FAIL: TestAria2Contract/AddURL/Progress/Pause/Resume/Remove (15.12s)
        Error:       Target error should be in err chain:
                     expected: "engine: task not found"
                     in chain: "aria2: rpc status 400: engine: daemon unreachable or session refused"
        Messages:    Get after Remove must report ErrNotFound
    --- FAIL: TestAria2Contract/UnknownIDReturnsErrNotFound (8.01s)
        Error:       Target error should be in err chain:
                     expected: "engine: task not found"
                     in chain: "aria2: rpc status 400: engine: daemon unreachable or session refused"
        Messages:    Get on a fabricated id
FAIL	github.com/L-K-M/dl-tool/internal/engine/aria2	67.370s
```

The alternatives were rejected: weakening the subtests to accept
`ErrUnavailable` contradicts both the obligations table and
`engine.Engine`'s contract, and no suite-side trick can change what the
adapter returns. The fix is four lines in `internal/engine/aria2/client.go`
(`post` decodes a 400 body like a 200; any other status, or a non-JSON body,
stays `ErrUnavailable`), so the Files table above was widened by that one
row — the same resolution T027 used for its fixture collision.

**Resolution (2026-09-08):** widened, fixed, and the full
`make test-integration` is green on the widened tree; see `## Evidence`.
