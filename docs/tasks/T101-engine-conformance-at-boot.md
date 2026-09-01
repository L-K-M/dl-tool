# T101 — Assert and force engine conformance at boot

| Field | Value |
|---|---|
| **ID** | T101 |
| **Milestone** | M2 |
| **Status** | todo |
| **Depends on** | T019, T027, T028, T029 |
| **Blocks** | — |
| **Parallel-safe** | no — extends `internal/engine/engine.go` and `internal/api/settings.go` |
| **Implements** | [FR-147](../02-requirements.md#fr-147-assert-engine-conformance-at-boot) |
| **Decisions** | [ADR-0017](../decisions/0017-exclusive-control-of-engines.md), [ADR-0009](../decisions/0009-native-cross-protocol-rss-rules.md), [ADR-0010](../decisions/0010-never-execute-third-party-definitions.md) |
| **Est. size** | 3 new files, ~390 LOC |

## Goal
On `Connect`, each adapter asserts that the engine's own competing automation is off, forces it off where
the API allows, and reports every check by key name. A conformance failure is a visible warning, never a
crash and never an exit.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §9 Engine conformance at boot](../06-download-engines.md#9-engine-conformance-at-boot)
2. [`docs/06-download-engines.md` §9.1 qBittorrent](../06-download-engines.md#91-qbittorrent)
3. [`docs/06-download-engines.md` §9.2 aria2](../06-download-engines.md#92-aria2)
4. [`docs/06-download-engines.md` §9.4 Why the engine queues are raised, not used](../06-download-engines.md#94-why-the-engine-queues-are-raised-not-used)
5. [`docs/05-api-contract.md` §11.3 `GET /engines` and `POST /engines/{id}/test`](../05-api-contract.md#113-get-engines-and-post-enginesidtest)
6. [`docs/04-data-model.md` §3.2 Configuration](../04-data-model.md#32-configuration)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/qbittorrent/conform.go` | create | The preference reads, the forced writes and the plugin check. |
| `internal/engine/qbittorrent/conform_test.go` | create | One case per check, plus the never-crash case. |
| `internal/engine/aria2/conform.go` | create | The `getGlobalOption` checks and the concurrency raise. |
| `internal/engine/engine.go` | modify | Add `ConformanceCheck` beside the existing types. |
| `internal/api/settings.go` | modify | Run the probe and record its outcome in `engines.last_error`. |

No other file may be modified.

## Interface contract

```go
package engine

// ConformanceCheck is one boot assertion about an engine's own settings. Key is the engine's own
// preference or option name, so a warning names something the operator can find in that engine's UI.
type ConformanceCheck struct {
	Key      string // e.g. "auto_tmm_enabled", "max-concurrent-downloads"
	Want     string
	Got      string
	Forced   bool   // dl-tool changed it to Want
	Warn     bool   // dl-tool cannot change it and the operator must
	Severity string // "ok" | "forced" | "warn"
}
```

```go
package qbittorrent

// Conform reads GET /api/v2/app/preferences, forces the three competing-automation keys off with
// POST /api/v2/app/setPreferences, raises the queueing limits out of dl-tool's way, and checks that no
// search plugin is installed. It never returns a fatal error for a failed check: an unreachable daemon
// returns engine.ErrUnavailable, everything else comes back as ConformanceCheck rows.
//
// setPreferences takes a single "json" form field carrying ONLY the keys being changed.
func (c *Client) Conform(ctx context.Context, maxActiveTotal int) ([]engine.ConformanceCheck, error)
```

```go
package aria2

// Conform reads aria2.getGlobalOption and raises max-concurrent-downloads to at least maxActiveTotal
// with aria2.changeGlobalOption. dir and save-session are warn-only, because both are daemon flags set
// in compose.yaml.
func (c *Client) Conform(ctx context.Context, maxActiveTotal int) ([]engine.ConformanceCheck, error)
```

qBittorrent checks, exactly these:

| Key | Required | Action on mismatch |
|---|---|---|
| `rss_processing_enabled` | `false` | Force with `setPreferences`; `Severity = "forced"`. |
| `scheduler_enabled` | `false` | Force with `setPreferences`; `Severity = "forced"`. |
| `auto_tmm_enabled` | `false` | Force with `setPreferences`; `Severity = "forced"`. |
| the queueing limit keys | at or above `max_active_total`, or queueing disabled | Force with `setPreferences`; `Severity = "forced"`. |
| search plugins | `GET /api/v2/search/plugins` returns an empty list | Warn only; `Severity = "warn"`. dl-tool never uninstalls a plugin. |

<!-- UNVERIFIED: the exact qBittorrent preference key names for the queueing limits were not read verbatim
     from release-5.2.3. Read GET /api/v2/app/preferences on a live daemon, identify the queueing keys,
     paste the relevant excerpt under `## Evidence`, and only then name them in conform.go. Do not guess
     them, and do not force a key you have not observed. -->

aria2 checks, exactly these:

| Option | Required | Action on mismatch |
|---|---|---|
| `max-concurrent-downloads` | at or above `max_active_total` | Raise with `aria2.changeGlobalOption`; `Severity = "forced"`. |
| `dir` | inside a configured data root | Warn only — it is a daemon flag. |
| `save-session` | non-empty | Warn only — it is a daemon flag. |

## Steps
1. Edit `internal/engine/engine.go` to add `ConformanceCheck` exactly as above. Do not add a method to the
   `Engine` interface and do not change any existing type.
2. Read `max_active_total` from the `settings` table and pass it in; never hardcode a ceiling.
3. Create `internal/engine/qbittorrent/conform.go`. Read `GET app/preferences` once and decode it into
   `map[string]any` so an unknown key is neither lost nor required.
4. Build one `setPreferences` call carrying only the keys that need changing, as a single `json` form
   field; issue no request when every key is already correct.
5. Observe the queueing key names as the UNVERIFIED note directs, then include them in the same call,
   raising them above `max_active_total` rather than relying on them.
6. Call `GET search/plugins` and emit a `warn` row naming the installed plugin count when the list is not
   empty; never call any other `search/*` endpoint and never uninstall anything.
7. Create `internal/engine/aria2/conform.go` with the three checks, raising
   `max-concurrent-downloads` through `aria2.changeGlobalOption` with the value encoded as a string.
8. Edit `internal/api/settings.go` to run `Conform` for every registered engine that implements it, at
   `NewServer` time and again on `POST /engines/{id}/test`, and to write a single-line summary of the
   non-`ok` rows into `engines.last_error` through `TouchEngine`, so `GET /engines` surfaces it.
9. Log every non-`ok` row once at warn with the engine name, the key, the wanted value and the observed
   value. A failed probe must not stop `NewServer` from returning and must not exit the process.
10. Create `internal/engine/qbittorrent/conform_test.go` with an `httptest` server covering: all keys
    already correct issuing no `setPreferences` call; `auto_tmm_enabled` true being forced false; a
    non-empty plugin list producing exactly one `warn` row and no write; a `500` from `app/preferences`
    returning rows with `Severity = "warn"` and no error that stops boot; and an unreachable daemon
    returning `engine.ErrUnavailable`.

## Acceptance criteria
- [ ] `rss_processing_enabled`, `scheduler_enabled` and `auto_tmm_enabled` are all false after `Conform`
      against a daemon that had them true.
- [ ] `setPreferences` carries only the changed keys, and is not called at all when nothing changed.
- [ ] An installed search plugin produces one `warn` row and no uninstall attempt.
- [ ] The queueing keys written are the ones observed in `app/preferences`, recorded under Evidence.
- [ ] A conformance failure never exits the process and never fails `NewServer`.
- [ ] `GET /engines` shows the failure summary in `last_error` for the affected engine.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then `ok` for
`github.com/L-K-M/dl-tool/internal/engine/qbittorrent`, `.../internal/engine/aria2` and
`.../internal/api`, with `TestConformNoWriteWhenClean`, `TestConformForcesAutoTMMOff`,
`TestConformWarnsOnSearchPlugin`, `TestConformNeverFailsBoot` and `TestConformRaisesAria2Concurrency`
all `PASS`. No `FAIL`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT add a `conformance` field to `EngineDTO` or a "fix it" endpoint; the response shape is owned by
  [`05-api-contract.md` §11.3](../05-api-contract.md#113-get-engines-and-post-enginesidtest) and does not
  define one. The settings screen that surfaces this is T053.
- Do NOT uninstall a qBittorrent search plugin or call any other `search/*` or `rss/*` endpoint.
- Do NOT enforce `max_active_total` here; T098 owns admission control and this task only moves the
  engine's own ceiling out of its way.
- Do NOT exit, panic or refuse to serve because a check failed.
- Do NOT add a foreign-task policy column or setting; there is one rule and it has no options.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
