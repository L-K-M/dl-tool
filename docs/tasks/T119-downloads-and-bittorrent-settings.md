# T119 — Build the Downloads and BitTorrent settings sections

| Field | Value |
|---|---|
| **ID** | T119 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T050, T053, T074, T083, T092, T107 |
| **Blocks** | — |
| **Parallel-safe** | no — it edits `SettingsScreen.tsx` and `settings.json`, shared with T116–T118, T120 and T121 |
| **Implements** | — (renders [FR-030](../02-requirements.md#fr-030-manage-categories-with-a-save-path) covered by T050, [FR-046](../02-requirements.md#fr-046-manage-watch-folders-and-scan-one-on-demand) covered by T107, [FR-047](../02-requirements.md#fr-047-reserve-committed-but-unwritten-bytes-and-keep-a-free-space-floor) covered by T099, [FR-100](../02-requirements.md#fr-100-auto-extract-the-supported-archive-formats) covered by T074, [FR-140](../02-requirements.md#fr-140-read-and-update-settings-without-exposing-secrets) covered by T092) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md), [ADR-0012](../decisions/0012-single-data-mount.md), [ADR-0017](../decisions/0017-exclusive-control-of-engines.md) |
| **Est. size** | 3 new files, ~400 LOC |

## Goal
`/settings/downloads` edits the four download settings keys that exist, manages watch folders and the
category→path table, and picks paths through the folder browser. `/settings/bittorrent` renders Download
Station's BitTorrent row read-only, naming for each control where it is actually set.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/09-web-ui-spec.md` §9 Settings screens](../09-web-ui-spec.md#9-settings-screens) — the **Downloads**
   and **BitTorrent** rows, and the dirty-bar rule.
2. [`docs/11-config-reference.md` §5 Database-backed settings](../11-config-reference.md#5-database-backed-settings)
   — the complete list of keys that are editable at all.
3. [`docs/11-config-reference.md` §1 Precedence rule](../11-config-reference.md#1-precedence-rule) — why an
   `infrastructure` variable has no control on any screen.
4. [`docs/05-api-contract.md` §11.1 `GET /settings` and `PATCH /settings`](../05-api-contract.md#111-get-settings-and-patch-settings)
   — the flat body, `"__redacted__"` and the `422` for an unknown key.
5. [`docs/05-api-contract.md` §15 Watch folders](../05-api-contract.md#15-watch-folders) and
   [§8.1 Categories](../05-api-contract.md#81-categories) — both tables and the scan report.
6. [`docs/tasks/T047-mkdir-free-space-and-folder-browser.md`](T047-mkdir-free-space-and-folder-browser.md) —
   `FolderBrowserDialog`, which every path field on this screen uses.
7. [`docs/tasks/T053-settings-screens.md`](T053-settings-screens.md) — `SECTIONS`, `IMPLEMENTED` and the
   `Save / Revert` bar this screen plugs into.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/Settings/DownloadsSection.tsx` | create | The Downloads form, watch-folder table and category table. |
| `web/src/components/Settings/BitTorrentSection.tsx` | create | The read-only BitTorrent row and its engine status. |
| `web/src/components/Settings/DownloadsSection.test.tsx` | create | Both sections: patching, redaction, tables, scan and read-only. |
| `web/src/components/Settings/SettingsScreen.tsx` | edit | Add `downloads` and `bittorrent` to `IMPLEMENTED` and render them. |
| `web/src/locales/en/settings.json` | edit | Labels, hints and column headers for both sections. |

No other file may be modified.

## Interface contract

```tsx
// web/src/components/Settings/DownloadsSection.tsx

/** The only settings keys this screen writes. Every one appears in
 *  docs/11-config-reference.md §5; PATCH /settings answers 422 for anything else. */
export const DOWNLOAD_KEYS = [
  'default_destination',
  'auto_extract',
  'extract_passwords',
  'min_free_space',
] as const;
export type DownloadKey = (typeof DOWNLOAD_KEYS)[number];

/** One row of GET /watch-folders, doc 05 §15. */
export interface WatchFolderRow {
  id: string;
  path: string;
  enabled: boolean;
  owner_username: string;
  destination: string;
  category: string | null;
  delete_after_load: boolean;
  poll_interval_s: number;
  last_scan_at: string | null;
  last_error: string | null;
}

/** One row of GET /categories, doc 05 §8.1. */
export interface CategoryRow {
  name: string;
  save_path: string;
  task_count: number;
}

export function DownloadsSection(): JSX.Element;
```

The dirty form collects a partial body and sends exactly the changed keys:

```json
PATCH /api/v1/settings
{"default_destination":"/data/iso",
 "auto_extract":true,
 "extract_passwords":["hunter2","correct horse battery"],
 "min_free_space":{"/data":2147483648}}
```

`GET /settings` returns `extract_passwords` as the literal string `"__redacted__"`, never the entries, so the
editor renders `A shared password list is stored.` and offers `Replace list` and `Clear list`. Replacing sends
the full array; leaving it alone sends nothing for that key. Never send `"__redacted__"` back as a value.

Controls, each against its doc 11 §5 key or its own endpoint:

| Control | Backing | Notes |
|---|---|---|
| Default destination | `default_destination` | Opens `FolderBrowserDialog`; the field is read-only text. |
| Auto-extract archives | `auto_extract` | Off by default. |
| Shared password list | `extract_passwords` | Write-only, per the redaction rule above. |
| Minimum free space per root | `min_free_space` | One number input per root from `GET /fs/roots`, in bytes. |
| Watch folders | `/watch-folders` CRUD + `POST /watch-folders/{id}/scan` | Columns: path, destination, category, owner, `Delete loaded .torrent files`, enabled, last scan, last error. |
| Category → path | `/categories` CRUD | Columns: name, save path, task count. |

```tsx
// web/src/components/Settings/BitTorrentSection.tsx

/** Read-only by construction. docs/11-config-reference.md §5 defines no settings key for any
 *  BitTorrent protocol control, and PATCH /settings answers 422 for an unknown key, so this
 *  section names where each control lives instead of rendering a form whose Save cannot work. */
export interface BitTorrentRow {
  /** i18next key under settings.bittorrent.rows */
  labelKey: string;
  /** 'engine' = a qBittorrent daemon preference; 'task' = per task, set in the add-task dialog
   *  and PATCH /tasks/{id}. */
  livesIn: 'engine' | 'task';
}

export const BITTORRENT_ROWS: readonly BitTorrentRow[] = [
  { labelKey: 'dht', livesIn: 'engine' },
  { labelKey: 'pex', livesIn: 'engine' },
  { labelKey: 'lsd', livesIn: 'engine' },
  { labelKey: 'encryption', livesIn: 'engine' },
  { labelKey: 'maxPeers', livesIn: 'engine' },
  { labelKey: 'appendTrackers', livesIn: 'engine' },
  { labelKey: 'shareRatioLimit', livesIn: 'task' },
  { labelKey: 'seedingTimeLimit', livesIn: 'task' },
  { labelKey: 'limitReachedAction', livesIn: 'task' },
];

export function BitTorrentSection(): JSX.Element;
```

The section also renders the `qbittorrent` row of `GET /engines` — `connected`, `version` and `last_error` —
because `last_error` is where T101 records a boot conformance failure, and states once that dl-tool assumes
exclusive control of its engines, linking
[ADR-0017](../decisions/0017-exclusive-control-of-engines.md). It renders no Save button and never becomes
dirty.

## Steps
1. Edit `web/src/locales/en/settings.json`: add a `downloads` subtree (control labels, the two table column
   headers, the password-list copy, the byte-unit hint) and a `bittorrent` subtree with one entry per
   `BITTORRENT_ROWS.labelKey` plus the two `livesIn` sentences.
2. Create `DownloadsSection.tsx` reading `GET /settings`, `GET /fs/roots`, `GET /watch-folders` and
   `GET /categories` through the T014 `api` client with TanStack Query.
3. Build the four settings controls of the table above, keeping a dirty patch object keyed by
   `DOWNLOAD_KEYS` and sending only changed keys on Save; on Revert, refetch `GET /settings`.
4. Wire both path fields — default destination and each watch folder's path and destination — to
   `FolderBrowserDialog`, and never let a user type a path that was not returned by the browser.
5. Render one `min_free_space` input per entry of `GET /fs/roots`, in bytes, defaulting a missing root to
   `2147483648`, and send the whole object because the key's value is a map.
6. Build the watch-folder table with Add, Edit, Delete and a per-row `Scan now` posting
   `POST /watch-folders/{id}/scan` and rendering `scanned`, `created.length` and each `skipped` entry with
   its `reason`; render `last_error` as a warning cell.
7. Build the category table with Add, Edit and Delete over `/categories`, showing `task_count` read-only and
   stating that deleting a category leaves its tasks' data alone.
8. Surface a `403 /problems/forbidden` from any write as the section-level note that these writes are
   and a `403 /problems/path-rejected` as a field error on the path that caused it.
9. Create `BitTorrentSection.tsx` per the contract: the read-only row list, the `qbittorrent` engine status
   from `GET /engines`, the ADR-0017 sentence, and no form controls.
10. Edit `SettingsScreen.tsx` to add `'downloads'` and `'bittorrent'` to `IMPLEMENTED` and render the two
    components; change nothing else in that file.
11. Create `DownloadsSection.test.tsx` covering the acceptance criteria below against stubbed endpoints.
12. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestSavesOnlyChangedSettingKeys` asserts the `PATCH /settings` body holds exactly the changed keys and
      no key outside `DOWNLOAD_KEYS`.
- [ ] `TestPasswordListNeverEchoesRedaction` asserts the stored list is never rendered and that no request
      body ever carries `"__redacted__"` as a value.
- [ ] `TestMinFreeSpacePerRoot` renders one input per `GET /fs/roots` entry and sends the whole map.
- [ ] `TestWatchFolderScanRendersReport` asserts `scanned`, the created count and each `skipped.reason`.
- [ ] `TestCategoryTableCrud` asserts a create, a rename and a delete against `/categories`.
- [ ] `TestBitTorrentSectionIsReadOnly` asserts nine rows, zero form controls, no Save bar, and that the
      `qbittorrent` `last_error` string is rendered.
- [ ] Every path on the screen is chosen through `FolderBrowserDialog`, never typed free-hand.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo DOWNLOADS_OK
```
Expected: Vitest lists `src/components/Settings/DownloadsSection.test.tsx` among the passed files, each of
the six tests named above appears with a `✓`, no file reports a failure, and the final line of stdout is
exactly `DOWNLOADS_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the five paths in the Files table and nothing else. Use `git status`, not `git diff`: three
of these files are new and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add a settings key. `PATCH /settings` answers `422` for a key absent from
  [`11-config-reference.md` §5](../11-config-reference.md#5-database-backed-settings); T092 owns that list.
- Do NOT build an "incomplete folder" or a "content layout default" control. Neither is a settings key:
  ADR-0012 has exactly one `/data` mount, and `content_layout` is per task and per RSS rule action.
- Do NOT proxy qBittorrent preferences. dl-tool writes only the conformance keys T101 owns; DHT, PeX, LSD,
  encryption and max-peers stay in that daemon's own UI.
- Do NOT touch the Bandwidth section or the 24×7 grid; **T118** owns them.
- Do NOT touch the Users, Notifications or Advanced sections; **T120** and **T121** own them.
- Do NOT change `web/src/components/Settings/SettingsScreen.tsx` beyond `IMPLEMENTED` and the two new
  section branches; T116, T117, T118, T120 and T121 all edit that same file.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
