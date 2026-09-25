# T131 — Wait for the persisted send in the notification fanout test

| Field | Value |
|---|---|
| **ID** | T131 |
| **Milestone** | M6 |
| **Status** | done |
| **Depends on** | T077 |
| **Blocks** | — |
| **Parallel-safe** | yes — it edits only a test file |
| **Implements** | T077's test contract — the fanout test waits on the postcondition it asserts |
| **Decisions** | [ADR-0015](../decisions/0015-db-backed-in-process-job-queue.md) |
| **Est. size** | ~40 LOC, test-only |

## Goal

`TestChainFanoutDeliversEvent` flakes in CI because it waits on the wrong postcondition. The
recording stub appends the request under its mutex **before** writing the response, so
`stub.count() == 1` becomes true while the notifier goroutine is still inside `client.Do` — with the
response, the body read, `Notifier.touch` and the `TouchNotificationChannel` UPDATE all outstanding
(`internal/jobs/handlers_notify_test.go:106-124`, `internal/jobs/handlers_notify.go:259-297`). The
test then asserts `LastSendAt != nil` immediately (`handlers_notify_test.go:497-499`), and whether
the row write landed is pure scheduling. Give the stub a held-response variant — the handler records,
then parks on a channel the test releases — so "the request arrived" is provably earlier than "the
send was persisted", and wait on the persisted row (`LastSendAt != nil`) instead of the arrival
count.

## Context you need

1. [`internal/jobs/handlers_notify_test.go`](../../internal/jobs/handlers_notify_test.go) — the
   `recordingStub` fixture and `TestChainFanoutDeliversEvent`.
2. [`internal/jobs/handlers_notify.go`](../../internal/jobs/handlers_notify.go) `Notifier.Send` — the
   full ordering between response arrival and `touch`/`TouchNotificationChannel`.
3. [`internal/jobs/worker_test.go`](../../internal/jobs/worker_test.go) — `waitFor`/`waitTick`/`waitTimeout`
   (5 s budget); `secure.NewClient` gives the notifier a 10 s timeout, so a test-released hold never
   trips it.

## Files

| Path | Action | Purpose |
|---|---|---|
| `internal/jobs/handlers_notify_test.go` | modify | Add a held-response constructor for `recordingStub` (handler parks on a channel after recording, before responding); in `TestChainFanoutDeliversEvent`, wait `stub.count() == 1`, assert the recorded request as today, release the hold, then wait `LastSendAt != nil` before the row assertions. |
| `docs/tasks/T131-wait-persisted-notify-send.md` | modify | Set Status to `done` and paste the verification output under Evidence (step 4). |
| `docs/tasks/00-task-index.md` | modify | Flip both of this task's status cells (the M6 row and the roster row) to `done` (step 4); touch no other row except the identifier-order note's M6 enumeration. |

No other file may be modified.

## Steps

1. Add `newHeldRecordingStub(t, status, body, hold <-chan struct{})` — same handler as
   `newRecordingStub`, but it parks on `hold` after recording and before writing the response; keep
   `newRecordingStub` as a nil-hold wrapper so existing call sites are untouched. The channel is a
   constructor parameter so it exists before `httptest.NewServer` spawns the handler goroutine — no
   racy cross-goroutine field write.
2. In `TestChainFanoutDeliversEvent` switch to the held stub and **observe the deterministic red
   first**: keep the existing `waitFor(stub.count() == 1)` then immediate row assertions while the
   hold remains unreleased. The notifier cannot have persisted anything — `LastSendAt` is nil by
   construction — so the test fails every run, proving the ordering defect rather than a scheduling
   coincidence.
3. Apply the fix: assert the recorded request (unchanged — it is fully populated under the mutex),
   `close(hold)`, then `waitFor` on `getChannel(t, db, id).LastSendAt != nil` before the final row
   assertions. `touch` writes the row only after a completed send attempt, so the new predicate is a
   strictly-later postcondition that implies delivery. Do not weaken any existing assertion.
4. After Verification passes, paste its output under Evidence, set this file's Status to `done`,
   and flip both status cells in [`00-task-index.md`](00-task-index.md). Commit them with the work.

## Acceptance criteria

- [x] With the response hold still armed, the count-based wait followed by the row assertions fails
      deterministically — observed once, recorded in Evidence.
- [x] The fixed test waits on `LastSendAt != nil` and passes under `-race` with the hold released
      only after the arrival assertions.
- [x] Every pre-existing assertion (method, event payload, `LastSendAt` non-nil, `LastError` nil) is
      unchanged; no sleep replaces the channel-based hold.

## Verification

```bash
make lint && make vet && make test PKG=./internal/jobs/...
```

Expected: lint and vet clean; `ok` for `internal/jobs`, no `FAIL`, no `DATA RACE`.

## Out of scope — do NOT

- Do NOT change `Notifier`, `TouchNotificationChannel`, or any production code — the defect is the
  test's postcondition, not the notifier.
- Do NOT weaken or delete an assertion, skip the test, or add a retry loop.
- Do NOT fix `TestCloseStopsPollGoroutine` (T030) or any other suspected flake here.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under
  "Blocked".

## Forbidden shortcuts

- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add a lint-suppression directive or assign an error to `_`; fix the cause.
- Do NOT use `time.Sleep` to order goroutines — the hold channel is the deterministic mechanism.

## Evidence

Step 2, deterministic red — with the response hold still armed and the original assertion order,
`LastSendAt` is nil by construction every run:

```
--- FAIL: TestChainFanoutDeliversEvent (0.50s)
    handlers_notify_test.go:513:
        Error:       Expected value not to be nil.
        Test:        TestChainFanoutDeliversEvent
FAIL	github.com/L-K-M/dl-tool/internal/jobs	0.548s
```

Step 3 onward, the task's Verification block (`make lint && make vet && make test
PKG=./internal/jobs/...`):

```
test -z "$(gofmt -l cmd internal)"          # clean
golangci-lint run ./...
0 issues.
cd web && npm run lint                       # clean
cd web && npx prettier --check .             # clean
go vet ./...                                 # clean
go test -race -count=1 ./internal/jobs/...
ok  	github.com/L-K-M/dl-tool/internal/jobs	40.352s
```

Parent verification after syncing main:

```text
Temporary forced abort before response release:
  before: panic: test timed out after 3s (held server handler)
  after: expected forced assertion failure exits in 0.054s; no timeout
$ go test -race ./internal/jobs -run '^TestChainFanoutDeliversEvent$' -count=10 -timeout=30s
ok  github.com/L-K-M/dl-tool/internal/jobs  5.111s
```

The response is now released by a function defer before test cleanup drains
workers and closes the server. The temporary abort probe was removed; every
original assertion remains.

The required block passed again after the cleanup fix:

```text
$ make lint
0 issues; eslint clean; Prettier clean
$ make vet
exit 0
$ make test PKG=./internal/jobs/...
go test -race -count=1 ./internal/jobs/...
ok  	github.com/L-K-M/dl-tool/internal/jobs	39.296s
```
