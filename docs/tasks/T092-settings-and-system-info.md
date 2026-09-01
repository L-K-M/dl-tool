# T092 — Serve settings and system info without leaking secrets

| Field | Value |
|---|---|
| **ID** | T092 |
| **Milestone** | M7 |
| **Status** | todo |
| **Depends on** | T005, T027, T091 |
| **Blocks** | T096 |
| **Parallel-safe** | no — it edits `internal/api/server.go` |
| **Implements** | [FR-140](../02-requirements.md#fr-140-read-and-update-settings-without-exposing-secrets), [FR-141](../02-requirements.md#fr-141-resolve-settings-from-environment-then-database) |
| **Decisions** | [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md), [ADR-0013](../decisions/0013-mandatory-built-in-authentication.md) |
| **Est. size** | 0 new files, ~390 LOC |

## Goal
`GET /api/v1/settings` returns every user-changeable key as one flat object with secrets rendered as
`"__redacted__"`, `PATCH /api/v1/settings` accepts any subset, and `GET /api/v1/system/info` reports the
running build, database, engines, task counts, schedule, limits and job counts.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/11-config-reference.md` §5 Database-backed settings](../11-config-reference.md#5-database-backed-settings) — the authoritative sixteen keys, their types and defaults.
2. [`docs/11-config-reference.md` §1 Precedence rule](../11-config-reference.md#1-precedence-rule) — environment wins for infrastructure, the database wins for preferences.
3. [`docs/11-config-reference.md` §6 Secrets](../11-config-reference.md#6-secrets) — the `"__redacted__"` placeholder and the no-op `PATCH` rule.
4. [`docs/05-api-contract.md` §11.1 `GET /settings` and `PATCH /settings`](../05-api-contract.md#111-get-settings-and-patch-settings) — the worked body and the status codes.
5. [`docs/05-api-contract.md` §13 System endpoints](../05-api-contract.md#13-system-endpoints) — the `GET /system/info` shape.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/store/settings.go` | edit | `GetSettings` and `PutSettings` over the `settings` key/value table. |
| `internal/api/settings.go` | edit | Add the `GET /settings` and `PATCH /settings` handlers to T027's file. |
| `internal/api/settings_test.go` | edit | Redaction, no-op patch, unknown key and range cases. |
| `internal/api/system.go` | edit | Add `GET /system/info` to T091's file. |
| `internal/api/server.go` | edit | Register `get-settings`, `patch-settings` and `get-system-info`. |

No other file may be modified.

## Interface contract

```go
package store

// Settings is the flat, typed view of the settings table. Every field maps to one key in
// docs/11-config-reference.md §5. ExtractPasswords is the only secret.
type Settings struct {
	DownloadRateLimit   int64             `json:"download_rate_limit"`
	UploadRateLimit     int64             `json:"upload_rate_limit"`
	AltDownloadRate     int64             `json:"alt_download_rate_limit"`
	AltUploadRate       int64             `json:"alt_upload_rate_limit"`
	ScheduleEnabled     bool              `json:"schedule_enabled"`
	DefaultDestination  string            `json:"default_destination"`
	MinFreeSpace        map[string]int64  `json:"min_free_space"`
	MaxActiveTotal      int               `json:"max_active_total"`
	MaxActivePerEngine  int               `json:"max_active_per_engine"`
	MaxActivePerUser    int               `json:"max_active_per_user"`
	ProcessOrder        string            `json:"process_order"` // by_date_created | by_user_round_robin
	RSSEnabled          bool              `json:"rss_enabled"`
	RSSIntervalS        int               `json:"rss_interval_s"`
	AutoExtract         bool              `json:"auto_extract"`
	ExtractPasswords    []secure.Secret   `json:"-"` // never marshalled; the API emits "__redacted__"
	ConfirmOnDelete     bool              `json:"confirm_on_delete"`
}

// GetSettings reads every row, applies the documented default for a missing key and returns the
// typed struct. It never returns a partially populated value.
func (s *Store) GetSettings(ctx context.Context) (Settings, error)

// PutSettings writes only the keys present in patch, in one transaction. A key whose value is the
// literal "__redacted__" is skipped. It returns store.ErrUnknownSettingKey for an unknown key and
// store.ErrSettingOutOfRange for a value outside its documented domain.
func (s *Store) PutSettings(ctx context.Context, patch map[string]json.RawMessage) (Settings, error)

var (
	ErrUnknownSettingKey  = errors.New("store: unknown setting key")
	ErrSettingOutOfRange  = errors.New("store: setting value out of range")
)
```

```go
package api

// RedactedPlaceholder is the only form a secret takes on the wire.
const RedactedPlaceholder = "__redacted__"

type GetSettingsOutput struct{ Body map[string]any }

type PatchSettingsInput struct {
	Body map[string]json.RawMessage
}

func (h *SettingsHandlers) GetSettings(ctx context.Context, in *struct{}) (*GetSettingsOutput, error)
func (h *SettingsHandlers) PatchSettings(ctx context.Context, in *PatchSettingsInput) (*GetSettingsOutput, error)

type SystemInfoOutput struct {
	Body struct {
		Version   string         `json:"version"`
		Commit    string         `json:"commit"`
		BuiltAt   string         `json:"built_at"`
		GoVersion string         `json:"go_version"`
		StartedAt string         `json:"started_at"`
		UptimeS   int64          `json:"uptime_s"`
		Database  DatabaseInfo   `json:"database"`
		Engines   []EngineBrief  `json:"engines"`
		Tasks     TaskCounts     `json:"tasks"`
		Schedule  ScheduleBrief  `json:"schedule"`
		Limits    LimitsBrief    `json:"limits"`
		Jobs      JobCounts      `json:"jobs"`
	}
}

func (h *SystemHandlers) GetSystemInfo(ctx context.Context, in *struct{}) (*SystemInfoOutput, error)
```

Worked `GET /settings` body, secrets already replaced:

```json
{"download_rate_limit":0,"upload_rate_limit":0,
 "alt_download_rate_limit":5242880,"alt_upload_rate_limit":1048576,
 "schedule_enabled":true,"default_destination":"/data",
 "rss_enabled":true,"rss_interval_s":900,
 "auto_extract":true,"extract_passwords":"__redacted__",
 "process_order":"by_date_created","confirm_on_delete":true}
```

## Steps
1. Edit `internal/store/settings.go` to add `Settings`, `GetSettings` and `PutSettings` with the sixteen keys
   of doc 11 §5 and their documented defaults. Read and write `value_json` as JSON, never as a bare string.
2. Validate in `PutSettings`: `rss_interval_s` at least `60`; every rate limit and `max_active_*` at least
   `0`; `process_order` one of the two enum values; `default_destination` non-empty. Return
   `ErrSettingOutOfRange` otherwise.
3. Skip any key whose submitted value is exactly `"__redacted__"`, so a client that round-trips
   `GET /settings` into `PATCH /settings` cannot erase `extract_passwords`.
4. Edit `internal/api/settings.go` to add `GetSettings` and `PatchSettings` on the existing
   `SettingsHandlers`. Serialise `extract_passwords` as the literal `"__redacted__"` string, never as an
   array, and never as the empty string.
5. Map `ErrUnknownSettingKey` and `ErrSettingOutOfRange` to `422`, and a non-admin `PATCH` to `403`
   `/problems/forbidden`. `GET /settings` is available to any authenticated user.
6. Edit `internal/api/system.go` to add `GetSystemInfo` on `SystemHandlers`, filling `version`, `commit` and
   `built_at` from the `main` package ldflags variables, `database` from the DB path, file size and
   `goose_db_version`, `engines` from the registry and the `engines` rows, `tasks` from one grouped count
   query, `schedule` from the settings and the container `TZ`, `limits` from the three `max_active_*` keys
   and `jobs` from one grouped count over `jobs`.
7. Restrict `GET /system/info` to admins and return `403` `/problems/forbidden` otherwise.
8. Edit `internal/api/server.go` to register `get-settings`, `patch-settings` and `get-system-info`.
9. Edit `internal/api/settings_test.go` with: a `GET` asserting `extract_passwords` is exactly
   `"__redacted__"` and that no other field contains a configured secret; a `PATCH` carrying
   `"extract_passwords":"__redacted__"` leaving the stored value unchanged; an unknown key returning `422`;
   `rss_interval_s` of `30` returning `422`; a non-admin `PATCH` returning `403`; and a `GET /system/info`
   whose serialised body contains neither `DLTOOL_ARIA2_SECRET` nor `DLTOOL_QBITTORRENT_PASSWORD`.

## Acceptance criteria
- [ ] `GET /settings` emits `extract_passwords` as exactly `"__redacted__"`.
- [ ] `PATCH` with `"__redacted__"` leaves the stored secret byte-identical.
- [ ] An unknown key and an out-of-range value each return `422`.
- [ ] A non-admin can `GET` settings and receives `403` `/problems/forbidden` on `PATCH`.
- [ ] `GET /system/info` returns all eleven top-level fields of doc 05 §13.
- [ ] No response body from either endpoint contains a configured engine secret in any form.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then `ok` lines for `github.com/L-K-M/dl-tool/internal/store` and
`.../internal/api`, with `TestSettingsRedactsExtractPasswords`, `TestPatchRedactedIsNoOp`,
`TestPatchUnknownKeyIs422`, `TestPatchOutOfRangeIs422`, `TestPatchRequiresAdmin` and
`TestSystemInfoCarriesNoSecret` all listed as passing. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement `GET /settings/schedule` or `PUT /settings/schedule`; T080 owns the 168-cell grid.
- Do NOT implement `GET /settings/export` or `POST /settings/import`; T108 owns them.
- Do NOT implement `GET /system/logs`; T096 owns it.
- Do NOT build the settings screens; T053 owns the SPA.
- Do NOT return a secret in any field, in any shape, at any log level.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
