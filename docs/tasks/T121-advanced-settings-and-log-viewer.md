# T121 — Build the Advanced settings section and the system log viewer

| Field | Value |
|---|---|
| **ID** | T121 |
| **Milestone** | M7 |
| **Status** | todo |
| **Depends on** | T053, T091, T092, T096, T108 |
| **Blocks** | — |
| **Parallel-safe** | no — it edits `SettingsScreen.tsx`, `App.tsx` and `settings.json`, shared with T116–T120 |
| **Implements** | — (renders [FR-140](../02-requirements.md#fr-140-read-and-update-settings-without-exposing-secrets) covered by T092, [FR-142](../02-requirements.md#fr-142-produce-consistent-backups) covered by T091, [FR-145](../02-requirements.md#fr-145-export-and-import-portable-settings) covered by T108, [FR-151](../02-requirements.md#fr-151-expose-system-logs-with-secrets-redacted) covered by T096) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md), [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md) |
| **Est. size** | 2 new files, ~400 LOC |

## Goal
`/settings/advanced` runs a backup, exports and imports the portable settings document dry-run first, resets
the settings keys to their documented defaults, and shows version and build info. `/logs` renders
`GET /system/logs` newest first with a level filter and cursor paging.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/09-web-ui-spec.md` §9 Settings screens](../09-web-ui-spec.md#9-settings-screens) — the **Advanced**
   row; and [§2.1 Routes](../09-web-ui-spec.md#21-routes) — the `/logs` route.
2. [`docs/05-api-contract.md` §13 System endpoints](../05-api-contract.md#13-system-endpoints) —
   `GET /system/info`, the `GET /system/logs` query and body, and `POST /system/backup`.
3. [`docs/05-api-contract.md` §11.5 `GET /settings/export` and `POST /settings/import`](../05-api-contract.md#115-get-settingsexport-and-post-settingsimport)
   — the document, the two flags and the dry-run report.
4. [`docs/11-config-reference.md` §5 Database-backed settings](../11-config-reference.md#5-database-backed-settings)
   — the key list and every default; and [§1 Precedence rule](../11-config-reference.md#1-precedence-rule) —
   why log level has no control here.
5. [`docs/09-web-ui-spec.md` §10.5 Empty states](../09-web-ui-spec.md#105-empty-states) — the logs copy.
6. [`docs/tasks/T053-settings-screens.md`](T053-settings-screens.md) — `SECTIONS`, `IMPLEMENTED` and the
   `Save / Revert` bar.
7. [`docs/tasks/T040-router-and-authentication-screens.md`](T040-router-and-authentication-screens.md) — the
   route table and the `Placeholder` this task replaces at `/logs`.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/Settings/AdvancedSection.tsx` | create | The Advanced form and the `LogViewer` screen; both read only `/system/*` and `/settings*`. |
| `web/src/components/Settings/AdvancedSection.test.tsx` | create | Backup, export, dry-run import, reset, read-only rows and log paging. |
| `web/src/components/Settings/SettingsScreen.tsx` | edit | Add `advanced` to `IMPLEMENTED` and render it. |
| `web/src/App.tsx` | edit | Route `/logs` to `LogViewer` instead of `Placeholder`. |
| `web/src/locales/en/settings.json` | edit | Advanced labels, the reset warning and the log-viewer strings. |

No other file may be modified. `LogViewer` shares the module because nothing else imports it and both
screens read the same endpoint group.

## Interface contract

```tsx
// web/src/components/Settings/AdvancedSection.tsx

/** The exact body "Reset to defaults" sends. Every key and every value comes from
 *  docs/11-config-reference.md §5. Three keys of that table are deliberately absent:
 *  default_destination (its default is the first DLTOOL_DATA_ROOTS entry, which is host-specific),
 *  min_free_space (a per-root map built from GET /fs/roots) and extract_passwords (a secret — a
 *  reset must not silently discard stored passwords). */
export const SETTINGS_DEFAULTS = {
  download_rate_limit: 0,
  upload_rate_limit: 0,
  alt_download_rate_limit: 5242880,
  alt_upload_rate_limit: 1048576,
  schedule_enabled: false,
  max_active_total: 5,
  max_active_per_engine: 3,
  process_order: 'by_date_created',
  rss_enabled: true,
  rss_interval_s: 1800,
  auto_extract: false,
  confirm_on_delete: true,
} as const;

/** GET /system/info, doc 05 §13, narrowed to what this section renders. */
export interface SystemInfo {
  version: string;
  commit: string;
  built_at: string;
  go_version: string;
  started_at: string;
  uptime_s: number;
  database: { path: string; size_bytes: number; schema_version: number };
}

/** POST /settings/import, doc 05 §11.5. dry_run defaults to true and a dry run is side-effect free. */
export interface ImportReport {
  dry_run: boolean;
  document_version: number;
  totals: { created: number; updated: number; skipped: number; rejected: number };
  collections: Record<string, { created: number; updated: number; skipped: number; rejected: number }>;
  rejected: Array<{ collection: string; key: string; type: string; detail: string }>;
}

export function AdvancedSection(): JSX.Element;

/** One record of GET /system/logs, doc 05 §13. Values are already redacted before storage,
 *  so the viewer renders attrs verbatim and offers no "reveal" affordance of any kind. */
export interface LogRecord {
  at: string;
  level: 'debug' | 'info' | 'warn' | 'error';
  msg: string;
  attrs: Record<string, unknown>;
}
export interface LogPage {
  items: LogRecord[];
  next_cursor: string | null;
  total: number;
}

/** The /logs route. Query: level (default "info"), since, limit, cursor. Newest first. */
export function LogViewer(): JSX.Element;
```

Advanced controls, each against a real endpoint:

| Control | Call | Rendered result |
|---|---|---|
| Back up now | `POST /system/backup` | `path`, `size_bytes`, `created_at` from the `201` body; a `409 /problems/conflict` renders `A backup is already running.` inline. |
| Export settings | `GET /settings/export` | The document saved as `dl-tool-settings.json` through a `Blob` object URL. |
| Import settings | `POST /settings/import` | Always `{"dry_run":true}` first; the report table; then a confirmed `{"dry_run":false,"on_conflict":"skip"\|"overwrite"}`. |
| Reset to defaults | `PATCH /settings` with `SETTINGS_DEFAULTS` | Confirm dialog naming what is untouched. |
| Version and build | `GET /system/info` | `version`, `commit`, `built_at`, `go_version`, `started_at`, `uptime_s`, `database`. |

Worked dry-run report the table renders:

```json
{"dry_run":true,"document_version":1,
 "totals":{"created":9,"updated":0,"skipped":2,"rejected":1},
 "collections":{"categories":{"created":1,"updated":0,"skipped":1,"rejected":0}},
 "rejected":[{"collection":"watch_folders","key":"/mnt/other/watch",
              "type":"/problems/path-rejected","detail":"outside every configured data root"}]}
```

**Not editable, rendered as static text naming the variable.** `DLTOOL_LOG_LEVEL`, `DLTOOL_LOG_FORMAT` and
`DLTOOL_DB_PATH` are `infrastructure` variables: the environment wins at boot and no API can change them
([`11-config-reference.md` §1](../11-config-reference.md#1-precedence-rule)). Retention windows are fixed and
are not settings keys. Each row reads `Set by <NAME>; restart the container to change it.` and carries no
input; the database path and size come from `GET /system/info`.

## Steps
1. Edit `web/src/locales/en/settings.json`: add an `advanced` subtree (button labels, the reset confirmation
   text, the three `Set by …` sentences, the report column headers) and a `logs` subtree (level names,
   column headers, `Load more`, and the empty state `Nothing logged yet.`).
2. Create `AdvancedSection.tsx` reading `GET /system/info` through the T014 `api` client with TanStack Query
   and rendering the version, build and database rows.
3. Add `Back up now` posting `POST /system/backup`, rendering the returned path and size, and handling
   `409 /problems/conflict` inline rather than as a toast.
4. Add `Export settings` fetching `GET /settings/export` and saving it as `dl-tool-settings.json`; state on
   screen that the document carries no password hash, API token or channel secret.
5. Add `Import settings`: a file input, a mandatory `dry_run: true` call, the totals and per-collection
   table, the `rejected` list with each `detail`, an `on_conflict` selector, and a confirm that repeats the
   call with `dry_run: false`. A `409` for a newer `document_version` renders as a named refusal.
6. Add `Reset to defaults`: a confirm dialog that names what it does not touch — users, categories,
   indexers, feeds, rules, watch folders, the schedule, `default_destination`, `min_free_space` and
   `extract_passwords` — then one `PATCH /settings` carrying exactly `SETTINGS_DEFAULTS`.
7. Render the three `infrastructure` rows as static text per the contract, with no input of any kind.
8. Add `LogViewer` in the same module: `GET /system/logs` with a level select (`debug`, `info`, `warn`,
   `error`, default `info`), an optional `since`, `limit=100`, a `Load more` that passes `next_cursor`, and
   a `Refresh`. Render `at` through `Intl`, `level` as a badge, `msg`, and `attrs` in an expandable row.
9. Render the empty result as `Nothing logged yet.` and a `403 /problems/forbidden` as
   `System logs are visible to administrators only.`, never as a blank screen.
10. Edit `SettingsScreen.tsx` to add `'advanced'` to `IMPLEMENTED` and render `AdvancedSection`, and edit
    `App.tsx` to route `/logs` to `LogViewer`; change nothing else in either file.
11. Create `AdvancedSection.test.tsx` covering the acceptance criteria below against stubbed endpoints.
12. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestBackupRendersPathAndConflict` asserts the `201` path and size are shown and that a `409` renders
      inline.
- [ ] `TestImportIsDryRunFirst` asserts the first `POST /settings/import` body carries `dry_run: true`, that
      no write call follows until the confirm, and that every `rejected[].detail` is rendered.
- [ ] `TestResetSendsExactlyDocumentedDefaults` asserts the `PATCH /settings` body deep-equals
      `SETTINGS_DEFAULTS` and contains no other key.
- [ ] `TestInfrastructureRowsHaveNoInput` asserts the log-level, log-format and database-path rows render no
      `input`, `select` or `textarea`.
- [ ] `TestLogViewerPagesWithCursor` asserts the second request carries the first response's `next_cursor`
      and that records render newest first.
- [ ] `TestLogViewerEmpty` asserts `Nothing logged yet.` for an empty page and the
      message for a `403`.
- [ ] The viewer renders `attrs` values verbatim and offers no control that would reveal a redacted value.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo ADVANCED_OK
```
Expected: Vitest lists `src/components/Settings/AdvancedSection.test.tsx` among the passed files, each of the
six tests named above appears with a `✓`, no file reports a failure, and the final line of stdout is exactly
`ADVANCED_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the five paths in the Files table and nothing else. Use `git status`, not `git diff`: two
of these files are new and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add a control for log level, log format, retention windows or the database path. They are
  `infrastructure` variables or fixed constants; the environment wins at boot
  ([`11-config-reference.md` §1](../11-config-reference.md#1-precedence-rule)).
- Do NOT add a restore control. `dl-tool restore --from <file>` is a CLI command that requires the server to
  be stopped; T108 owns it and it has no endpoint.
- Do NOT include `default_destination`, `min_free_space` or `extract_passwords` in the reset body, and do NOT
  have the reset touch any table other than `settings`.
- Do NOT add an un-redact, decode or "show raw secret" affordance to the log viewer; records are redacted
  before storage and the plaintext does not exist to show.
- Do NOT build the Downloads, BitTorrent, Users or Notifications sections; **T119** and **T120** own them.
- Do NOT change `web/src/components/Settings/SettingsScreen.tsx` beyond `IMPLEMENTED` and the new section
  branch, or `web/src/App.tsx` beyond the `/logs` element; T116–T120 edit the same files.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
