# T027 — List engines and test connectivity

| Field | Value |
|---|---|
| **ID** | T027 |
| **Milestone** | M1 |
| **Status** | todo |
| **Depends on** | T019, T020 |
| **Blocks** | T029, T053, T079, T080, T092, T101 |
| **Parallel-safe** | no — it also edits the shared file `internal/api/server.go` |
| **Implements** | [FR-143](../02-requirements.md#fr-143-list-engines-and-test-connectivity) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md), [ADR-0013](../decisions/0013-mandatory-built-in-authentication.md) |
| **Est. size** | 2 new files, ~260 LOC |

## Goal
`GET /api/v1/engines` reports every configured engine with its declared capabilities, connection state and
resolved version, and `POST /api/v1/engines/{id}/test` probes one engine on demand. A failed probe is still
`200`, with `ok:false` and the transport error in `error`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §11.3 `GET /engines` and `POST /engines/{id}/test`](../05-api-contract.md#113-get-engines-and-post-enginesidtest)
2. [`docs/04-data-model.md` §3.2 Configuration](../04-data-model.md#32-configuration)
3. [`docs/06-download-engines.md` §1 The Engine interface](../06-download-engines.md#1-the-engine-interface)
4. [`docs/11-config-reference.md` §6 Secrets](../11-config-reference.md#6-secrets)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/settings.go` | create | The `GET /engines` and `POST /engines/{id}/test` handlers. |
| `internal/api/settings_test.go` | create | Cases for a healthy engine and a stopped engine. |
| `internal/store/settings.go` | create | New file. `ListEngines` and `TouchEngine` over the `engines` table; every later task that adds a settings-table query extends this file. |
| `internal/api/server.go` | modify | Build the engine registry, construct and register the aria2 client, register `list-engines` and `test-engine`. |

No other file may be modified.

## Interface contract

```go
package api

// EngineDTO is one entry of GET /engines. Secrets are never present in any form.
type EngineDTO struct {
	ID           string   `json:"id"`            // eng_aria2 | eng_qbittorrent | eng_ytdlp
	Kind         string   `json:"kind"`          // aria2 | qbittorrent | ytdlp
	Name         string   `json:"name"`
	Enabled      bool     `json:"enabled"`
	URL          *string  `json:"url"`
	Connected    bool     `json:"connected"`
	Version      *string  `json:"version"`
	Capabilities []string `json:"capabilities"`
	LastSeenAt   *string  `json:"last_seen_at"`  // RFC 3339 UTC
	LastError    *string  `json:"last_error"`
}

type ListEnginesOutput struct {
	Body struct {
		Engines []EngineDTO `json:"engines"`
	}
}

// TestEngineOutput is 200 whether or not the probe succeeded.
type TestEngineOutput struct {
	Body struct {
		Ok        bool    `json:"ok"`
		Version   *string `json:"version"`
		ElapsedMS int64   `json:"elapsed_ms"`
		Error     *string `json:"error"`
	}
}

func (h *SettingsHandlers) ListEngines(ctx context.Context, in *struct{}) (*ListEnginesOutput, error)
func (h *SettingsHandlers) TestEngine(ctx context.Context, in *TestEngineInput) (*TestEngineOutput, error)

type TestEngineInput struct {
	ID string `path:"id"`
}
```

```go
package store

// ListEngines returns the engines rows with an explicit column list. secret_enc is never selected.
func (s *Store) ListEngines(ctx context.Context) ([]Engine, error)

// TouchEngine records the outcome of a probe: last_seen_at on success, last_error on failure, and
// version whenever the probe resolved one.
func (s *Store) TouchEngine(ctx context.Context, id string, version, lastErr *string, at int64) error
```

## Steps
1. Create `internal/store/settings.go` — no earlier task creates it — with `ListEngines` and `TouchEngine`; never put `secret_enc` in a
   `SELECT` list.
2. Create `internal/api/settings.go` with `SettingsHandlers`, its constructor taking the store and the
   engine registry, and the structs above.
3. Implement `ListEngines`: read the `engines` rows, look each `kind` up in the registry, and fill
   `Capabilities` from `Engine.Capabilities()` — the declared set, never a guess.
4. Report `Connected` from the last successful probe rather than by probing inside the list handler, so a
   dead engine cannot slow the page down.
5. Implement `TestEngine`: call `Engine.Health` with a short deadline, measure `elapsed_ms`, and return
   `200` with `ok:false` and the transport error in `error` when it fails.
6. Call `TouchEngine` after every probe so `last_seen_at`, `version` and `last_error` stay current.
7. Return `404` `/problems/not-found` for an unknown engine id.
8. In `internal/api/server.go`, build the one process-wide registry with `engine.NewRegistry()`; when
   `cfg.Aria2URL` is non-empty construct `aria2.New(aria2.Config{URL: cfg.Aria2URL,
   Secret: cfg.Aria2Secret.Reveal(), Timeout: 10 * time.Second}, hc)`, call `Connect` — on failure log
   `engine_unreachable` at `warn` and continue, never failing `NewServer` — and `Register` it. Pass that
   registry to `NewTaskHandlers` and `NewSettingsHandlers`, then register both operations as
   `list-engines` and `test-engine`. This is the composition-root call site required by
   [`14-conventions.md` §8.3](../14-conventions.md#83-wire-a-long-lived-component).
9. Create `internal/api/settings_test.go`: a stub aria2 returning `1.37.0` yields `connected:true` and that
   version; a stopped engine yields `ok:false` with the transport error and still `200`; and no response
   body contains the configured secret.

## Acceptance criteria
- [ ] `Registry.Get("aria2")` returns the client after `NewServer` with `DLTOOL_ARIA2_URL` set.
- [ ] `GET /engines` lists the aria2 entry with the capabilities its adapter declares.
- [ ] A healthy probe returns `{"ok":true}` with the version and a non-zero `elapsed_ms`.
- [ ] A stopped engine returns `200` with `ok:false` and the transport error in `error`.
- [ ] Neither response contains `DLTOOL_ARIA2_SECRET` in any form.
- [ ] An unknown engine id returns `404` `/problems/not-found`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/api/...
```
Expected: `make lint` prints nothing, then `ok  	github.com/L-K-M/dl-tool/internal/api` followed by its
elapsed time, with `TestListEngines` and `TestTestEngineFailureIs200` both
running. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement `GET /settings` or `PATCH /settings`; T092 owns them.
- Do NOT run or expose the boot conformance probe; T101 owns it.
- Do NOT return, echo or log an engine secret in any field.
- Do NOT add the engines page in the SPA; T053 owns the settings screens.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

`make lint && make test PKG=./internal/api/...` (the Verification block, run at the final tree):

```
$ make lint && make test PKG=./internal/api/...
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
go test -race -count=1 ./internal/api/...
ok  	github.com/L-K-M/dl-tool/internal/api	44.190s
```

`TestListEngines` and `TestTestEngineFailureIs200` both ran and passed (with
`TestTestEngineUnknownID` and `TestNewServerWiresConfiguredAria2`, the composition-root case):

```
$ go test ./internal/api/... -run 'TestListEngines|TestTestEngineFailureIs200|TestTestEngineUnknownID|TestNewServerWiresConfiguredAria2' -v
=== RUN   TestListEngines
--- PASS: TestListEngines (0.06s)
=== RUN   TestTestEngineFailureIs200
--- PASS: TestTestEngineFailureIs200 (0.03s)
=== RUN   TestTestEngineUnknownID
--- PASS: TestTestEngineUnknownID (0.03s)
=== RUN   TestNewServerWiresConfiguredAria2
--- PASS: TestNewServerWiresConfiguredAria2 (0.03s)
ok  	github.com/L-K-M/dl-tool/internal/api	0.163s
```

Scope check:

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
api/openapi.json
internal/api/server.go
internal/api/settings.go
internal/api/settings_test.go
internal/store/settings.go
web/src/api/schema.d.ts
```

Exactly the Files table plus the two artefacts docs/13 §7.1 adds implicitly for a task that registers Huma operations (`make gen`, committed).

`make test` over every package is **not** green — see Blocked.

## Blocked

The implementation is complete and its own Verification block passes, but
step 8's boot `Connect` inside `NewServer` breaks an M1 test that lives
outside this task's Files table, so `make test` cannot go green without an
out-of-scope edit:

- Step 8 and [docs/17-operations-and-runbook.md §1](../17-operations-and-runbook.md) mandate that `NewServer`, with `DLTOOL_ARIA2_URL` set, calls the engine's `Connect` and records the outcome in the `engines` row. The aria2 client sends a single-object JSON-RPC request (`aria2.getVersion`) for that probe.
- T026's `TestNewServerReconcilesBeforeServing` (`internal/engine/reconcile_test.go`, ~line 434) fakes the aria2 daemon with a handler that decodes every request body as a **batch array** and calls `t.Errorf` on anything else. The reconciler's sweep is batched, so the fixture passed until this task added the single-object boot probe.

Observed on this branch (green on `main` before the change):

```
$ go test ./internal/engine/ -run TestNewServerReconcilesBeforeServing
--- FAIL: TestNewServerReconcilesBeforeServing (0.44s)
    reconcile_test.go:457: decode rpc batch: json: cannot unmarshal object into Go value of type []struct { ID string "json:\"id\""; Method string "json:\"method\"" }
FAIL
```

The fixture pinned an implementation detail — "NewServer's only boot-time aria2 traffic is the reconciler's batched List" — that the plan's own T027 contradicts. The resolution is a ~10-line fix to that one test fixture (decode object-or-array and answer `aria2.getVersion` with a version string), which is outside this task's Files table, so it is not done here. Alternatives considered and rejected: dropping the boot `Connect` (contradicts step 8 and doc 17 §1), and moving the probe to `cmd/dl-tool/main.go` (also outside the Files table, and step 8 names `internal/api/server.go`).
