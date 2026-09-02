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
| `internal/api/server.go` | modify | Register `list-engines` and `test-engine`. |

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
7. Restrict both operations to `role = "admin"`, returning `403` `/problems/forbidden` otherwise, and
   `404` `/problems/not-found` for an unknown engine id.
8. Register both operations in `internal/api/server.go` as `list-engines` and `test-engine`.
9. Create `internal/api/settings_test.go`: a stub aria2 returning `1.37.0` yields `connected:true` and that
   version; a stopped engine yields `ok:false` with the transport error and still `200`.
   receives `403`; and no response body contains the configured secret.

## Acceptance criteria
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
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
