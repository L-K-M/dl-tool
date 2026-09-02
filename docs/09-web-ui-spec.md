# 09 — Web UI Specification

> **Status:** draft
> **Last reviewed:** 2026-09-01
> **Audience:** implementing agent
> **Read this before:** T039–T053, T103, T104, and any task touching `web/`

## Purpose
Define every screen dl-tool ships: the shell, the task grid and its columns, every dialog and its fields, the
detail-pane tabs, the search, RSS and settings screens, and the cross-cutting rules for theming, i18n,
accessibility and error presentation. It does not define HTTP payloads, database columns or environment
variables.

## Scope of this document
- In scope: frontend dependency pins, routes, layout geometry, the sidebar tree, toolbar and status bar, grid
  columns and their persistence, keyboard and pointer interaction, all dialogs and their exact field labels,
  detail tabs, search/RSS/settings screens, the 24×7 schedule grid, dark-mode tokens, i18n plumbing, the
  mobile breakpoint, accessibility, empty states, toasts, optimistic updates, truncation, and the reconnect
  banner.
- Out of scope (lives instead in): request and response shapes, status codes →
  [`05-api-contract.md`](05-api-contract.md); table columns, enums and DDL →
  [`04-data-model.md`](04-data-model.md); the RSS rule schema and matching algorithm →
  [`08-rss-automation.md`](08-rss-automation.md); indexer definitions →
  [`07-search-and-indexers.md`](07-search-and-indexers.md); file-priority semantics per engine →
  [`06-download-engines.md`](06-download-engines.md); env vars → [`11-config-reference.md`](11-config-reference.md);
  test harness and Definition of Done → [`13-testing-and-verification.md`](13-testing-and-verification.md).

---

## 1. Frontend stack

Every package below is pinned to the exact version string. Do not upgrade, do not substitute.

| Concern | Package | Version |
|---|---|---|
| Framework | `react`, `react-dom` | `19.2.8` |
| Build | `vite` | `8.2.2` |
| Vite React plugin | `@vitejs/plugin-react` | `6.1.1` |
| Language | `typescript` | `5.9.3` |
| Styling | `tailwindcss`, `@tailwindcss/vite` | `4.3.3` |
| Components | shadcn/ui CLI (Radix default) | `4.19.1` |
| Data grid | `@tanstack/react-table` | `8.21.3` |
| Row virtualisation | `@tanstack/react-virtual` | `3.14.10` |
| Server cache | `@tanstack/react-query` | `5.102.8` |
| Client state | `zustand` | `5.0.15` |
| Routing | `react-router-dom` | `7.18.3` |
| i18n | `i18next`, `react-i18next` | `26.4.1`, `17.0.13` |
| Icons | `lucide-react` | `1.38.0` |
| Class utilities | `tailwind-merge`, `clsx`, `class-variance-authority` | `3.6.0`, `2.1.1`, `0.7.1` |
| Typed client | `openapi-typescript`, `openapi-fetch` | `7.13.0`, `0.17.0` |
| Unit tests | `vitest`, `@testing-library/react` | `4.1.11`, `16.3.3` |
| API mocking | `msw` | `2.15.0` |
| E2E | `@playwright/test` | `1.62.1` |
| Lint and format | `eslint`, `typescript-eslint`, `prettier` | `10.9.1`, `8.69.0`, `3.9.6` |

Two pins are deliberate downgrades and must not be "fixed":

- **`@tanstack/react-table` is `8.21.3` — v8, NOT v9.** v9.0.0 shipped 2026-08-04, four weeks before this
  plan; its declared-feature-module API appears in no tutorial an implementing model has seen.
- **`typescript` is `5.9.3` — NOT 6.x or 7.x.** TypeScript 7 is the Go-native rewrite that turns 6.0's
  deprecations into hard errors and defaults to `strict` and `esnext`.

Dependencies the research recommends but the brief does not pin — add them with the version `go get`/`npm
install` resolves at implementation time and record it in `web/package.json`:
`@dnd-kit/core` + `@dnd-kit/sortable` (column reorder) and `react-resizable-panels` (sidebar and detail-pane
splitters). <!-- pin at implementation time -->

Deliberately **not** dependencies, each with a named replacement: `next-themes` → shadcn/ui's Vite
dark-mode provider (a `class="dark"` toggle on `<html>` persisted in `localStorage`, defaulting to
`prefers-color-scheme`); `react-dropzone` → native `dragover`/`drop` handlers plus a hidden
`<input type="file" multiple>`; `react-hotkeys-hook` → one `keydown` listener with the guard in §3.6;
`parse-torrent` → `POST /api/v1/tasks/inspect` parses server-side; `d3-shape` → an inline
`<svg><polyline>`; `zod` → types generated from `api/openapi.json`. `cmdk` and `vaul` are out of v1 scope;
mobile sheets use shadcn/ui's `drawer`.

---

## 2. Application shell

### 2.1 Routes

| Path | Screen |
|---|---|
| `/` | Task grid, filter `all` |
| `/tasks/:filter` | Task grid; `:filter` ∈ `all` `downloading` `completed` `active` `inactive` `stopped` `error` |
| `/tasks/category/:name` | Task grid filtered to one category |
| `/tasks/tag/:name` | Task grid filtered to one tag |
| `/search` | Search screen |
| `/rss/feeds` | RSS feeds and items |
| `/rss/rules` | RSS rule editor |
| `/settings/:section` | Settings; `:section` ∈ `general` `connection` `bandwidth` `bittorrent` `downloads` `rss` `indexers` `users` `notifications` `advanced` |
| `/logs` | System log viewer |
| `/setup` | First-run wizard; reachable only while no user exists |
| `/login` | Sign-in |

### 2.2 Global layout

```
┌───────────────────────────────────────────────────────────────────────────────────────────────┐
│ ☰  dl-tool     [ + Add ]  [▶ Start] [⏸ Pause] [🗑 Remove] [✎ Edit] │ [🔍 filter…] │ 🌙 ⚙ 👤 │  ← Top toolbar (48px)
├──────────────┬────────────────────────────────────────────────────────────────────────────────┤
│ DOWNLOAD     │  Name                     Size   Progress          Status   ↓      ↑    ETA   … │
│  All      42 │ ┌────────────────────────────────────────────────────────────────────────────┐ │
│  Downloading 6│ │▸ ubuntu-26.04-desktop… 5.2 GB [████████░░ 78%]  Downloading 12.4M 340k 6m │ │
│  Completed 30│ │  Big.Buck.Bunny.2160p… 14 GB  [██████████100%]  Seeding      —    1.2M  ∞  │ │
│  Active    8 │ │  archive-2019.tar.zst  912 MB [██░░░░░░░░ 19%]  Error        —     —    —  │ │
│  Inactive  4 │ │  …                                                                          │ │
│  Stopped   2 │ │  (virtualised rows, 32 px each, 10 000+ rows)                              │ │
│  Error     2 │ └────────────────────────────────────────────────────────────────────────────┘ │
│              ├────────────────────────────────────────────────────────────────────────────────┤ ← drag handle
│ SEARCH       │ ubuntu-26.04-desktop-amd64.iso                                            [ ✕ ] │
│  Results     │ ┌ General │ Transfer │ Trackers │ Peers │ Files │ Log ──────────────────────┐  │
│  Saved       │ │ Destination /data/iso             Added   2026-08-30 14:02              │  │
│              │ │ Size        5.2 GB (4.1 GB done)  User    alice                          │  │
│ RSS          │ │ Source      magnet:?xt=urn:btih:… Health  ●●●●○  seeds 42 / peers 118    │  │
│  Feeds     5 │ └────────────────────────────────────────────────────────────────────────────┘ │
│  Rules     3 │                                                                                │
│              │                                                                                │
│ ─────────    │                                                                                │
│ Settings     │                                                                                │
│ Logs         │                                                                                │
├──────────────┴────────────────────────────────────────────────────────────────────────────────┤
│ ● Connected  ↓ 12.9 MB/s  ↑ 1.6 MB/s   8 active / 42 total   Free: 412 GB of 2 TB   ⏱ Sched: ON│ ← Status bar (28px)
└───────────────────────────────────────────────────────────────────────────────────────────────┘
```

### 2.3 Region sizing

| Region | Size | Behaviour |
|---|---|---|
| Top toolbar | 48 px fixed | Sticky. Left: primary actions. Centre-right: filter box. Right: theme toggle, settings, user menu. |
| Sidebar | 220 px default, resizable 180–360 px, collapsible to a 56 px icon rail | Width and collapsed state persisted (§3.3). |
| Grid | `flex-1` | Virtualised. Horizontal scroll lives inside the grid only; the page body never scrolls horizontally. |
| Detail pane | 0 when nothing is selected, 260 px default, drag-resizable from 160 px to 70 % of the viewport | Collapses on empty selection and on `Esc`. Height persisted. |
| Status bar | 28 px fixed | Always visible, including on mobile. |

### 2.4 Sidebar tree

Node labels and count semantics are fixed. Counts render as a muted right-aligned pill, `Label (n)`.

```
DOWNLOAD
  All            (n)    all tasks whose state is not `removed`          → /
  Downloading    (n)    state = downloading                             → /tasks/downloading
  Completed      (n)    state = completed                               → /tasks/completed
  Active         (n)    state ∈ {downloading, seeding}                  → /tasks/active
  Inactive       (n)    state ∈ {error, queued, paused}                 → /tasks/inactive
  Stopped        (n)    state = paused                                  → /tasks/stopped
  Error          (n)    state = error                    (new vs DS)    → /tasks/error
CATEGORIES  (collapsible; one node per category + an "Uncategorised" pseudo-node, each with a count)
TAGS        (collapsible; one node per tag + an "Untagged" pseudo-node, each with a count)
SEARCH
  Search Results
  Saved Searches (n)
RSS
  Feeds          (n unread items)
  Rules          (n enabled rules)
─────
Settings
Logs
```

- Counts come from the SSE `sync` payload, never from a separate request per node.
- Do **not** auto-hide zero-count nodes under `DOWNLOAD`; dim them to 45 % opacity instead. Category and tag
  nodes may hide at zero, controlled by a Settings toggle that is off by default.
- Each group is a `<nav>` with an `<h2 class="sr-only">` label; nodes are `<a>`; the active node carries
  `aria-current="page"`.
- The seven `DOWNLOAD` filters are resolved server-side by `GET /api/v1/tasks?state=` — see
  [`05-api-contract.md`](05-api-contract.md). The client never recomputes them from a full task list.

### 2.5 Toolbar

Button order is fixed. Icons plus text at ≥ 1100 px, icon-only below.

1. `+ Add` — primary split button: *Add URLs…* / *Add .torrent file…* / *Add from clipboard*.
2. separator
3. `Start` · `Pause` · `Remove ▾` (menu: *Remove task* / *Remove task and files*) · `Edit`
4. separator
5. `Move ▾` (*Top* / *Up* / *Down* / *Bottom*) · `Clear completed`
6. separator
7. filter box (client-side substring match on the name column, 250 ms debounce, clear `✕`)
8. `Columns ▾` · theme toggle · `Settings` · user menu

Every selection-dependent button gets `disabled` **and** `aria-disabled="true"` when the selection is empty,
with a tooltip naming the reason. When the event stream is down, mutating buttons are disabled with the
tooltip `Reconnecting…` (§10.8) rather than silently failing.

### 2.6 Status bar

Segments, left to right:

| # | Segment | Content | Notes |
|---|---|---|---|
| 1 | Connection | `● Connected` / `◐ Reconnecting…` / `○ Offline` | `role="status"`, `aria-live="polite"`. |
| 2 | Rates | `↓ 12.9 MB/s   ↑ 1.6 MB/s` | Tabular-numeral font so the width never jitters. |
| 3 | Counts | `8 active / 42 total` | |
| 4 | Free space | `Free: 412 GB of 2 TB` | For the destination of the currently selected task, else the default destination. |
| 5 | Schedule | `Sched: OFF` / `Default` / `Alt` / `No-DL` | Click navigates to `/settings/bandwidth`. |
| 6 | Alt-speed indicator | turtle icon | Shown only while `schedule_enabled` is true; indicates whether the active cell applies the alternative pair, and click navigates to `/settings/bandwidth`. There is no manual override toggle: the grid is the only source of alternative speed, and no settings key backs one. |

---

## 3. Task grid

### 3.1 Default columns

`Source` names the field of the Task object in [`05-api-contract.md`](05-api-contract.md) §3.

| # | id | Header | Width | Align | Visible | Sort type | Renderer | Source |
|---|---|---|---|---|---|---|---|---|
| 1 | `select` | (checkbox) | 36 | centre | ✅ pinned left | — | tri-state header checkbox | — |
| 2 | `queuePos` | `#` | 44 | right | ✅ | numeric | integer, `—` when null | `queue_position` |
| 3 | `name` | Name | 320 | left | ✅ pinned left | `Intl.Collator` string | source-kind icon + single-line ellipsis + tooltip | `name`, `source_kind` |
| 4 | `size` | Size | 90 | right | ✅ | numeric bytes | `Intl.NumberFormat` bytes, `—` when null | `total_bytes` |
| 5 | `progress` | Progress | 130 | left | ✅ | numeric 0–1 | inline bar with `78.4%` inside | `progress` |
| 6 | `status` | Status | 110 | left | ✅ | enum order (§3.2) | coloured dot + icon + label | `state`, `error_code` |
| 7 | `dlSpeed` | Down | 90 | right | ✅ | numeric | `12.4 MB/s`, `—` at 0 | `download_rate` |
| 8 | `ulSpeed` | Up | 90 | right | ✅ | numeric | `12.4 MB/s`, `—` at 0 | `upload_rate` |
| 9 | `eta` | ETA | 80 | right | ✅ | numeric seconds | `6m 12s`, `∞` when null | `eta_seconds` |
| 10 | `peers` | Seeds/Peers | 100 | right | ✅ | numeric (seeders) | `42 / 58`, tooltip `118 known peers`, `—` when `total_peers = 0` | `connected_seeders`, `connected_leechers`, `total_peers` |
| 11 | `ratio` | Ratio | 70 | right | ✅ | numeric | 2 decimal places, `∞` above 9999 | `ratio` |
| 12 | `uploaded` | Uploaded | 90 | right | ✅ | numeric | bytes | `uploaded_bytes` |
| 13 | `destination` | Destination | 200 | left | ✅ | string | middle-ellipsis path + tooltip | `destination` |
| 14 | `addedOn` | Added | 140 | left | ✅ | numeric timestamp | relative under 7 days, absolute after; tooltip is the RFC 3339 string | `added_at` |
| 15 | `completedOn` | Completed | 140 | left | ✅ | numeric timestamp | as `addedOn`, `—` when null | `completed_at` |

Hidden by default, available from `Columns ▾`, each backed by an existing Task field (id → source):
`downloaded` → `completed_bytes` · `remaining` → `total_bytes - completed_bytes` · `category` · `tags`
(chips) · `dlLimit` → `dl_limit` · `ulLimit` → `ul_limit` · `ratioLimit` → `ratio_limit` · `user` →
`owner_username` · `sourceUrl` → `source_uri` (middle-ellipsis + copy button) · `sourceKind` →
`source_kind` · `engine` · `fileCount` → `file_count` · `sequential` (✓ or blank) · `errorCode` →
`error_code` with `error_message` in the tooltip · `startedAt` → `started_at` · `updatedAt` →
`updated_at` · `infoHashV1` → `infohash_v1` · `infoHashV2` → `infohash_v2`. All render `—` when the
source is null or zero.

Deferred to v2 because no v1 field carries them — do not add a column that renders a placeholder:
`wantedSize`, `tracker`, `availability`, `timeActive`, `lastActivity`, `createdOn`, `private`, `pieces`,
`reannounceIn`.

### 3.2 Status renderer

`status` sorts by this ordinal, not alphabetically. Icons are `lucide-react` names.

| Ordinal | `state` | Label | Dot token | Icon |
|---|---|---|---|---|
| 0 | `downloading` | Downloading | `--accent` | `arrow-down` |
| 1 | `seeding` | Seeding | `--ok` | `arrow-up` |
| 2 | `checking` | Checking | `--warn` | `search-check` |
| 3 | `extracting` | Extracting | `--warn` | `file-archive` |
| 4 | `moving` | Moving | `--warn` | `folder-input` |
| 5 | `queued` | Queued | `--fg-muted` | `clock` |
| 6 | `paused` | Paused | `--fg-muted` | `pause` |
| 7 | `completed` | Completed | `--ok` | `check` |
| 8 | `error` | Error | `--error` | `triangle-alert` |
| 9 | `removed` | Removed | `--fg-muted` | `trash-2` |

Every status carries an icon as well as a colour; colour is never the only signal (WCAG 1.4.1). The `error`
row shows `error_code` in the cell and `error_message` in the tooltip.

### 3.3 Persistence

One document, persisted server-side per user through `GET`/`PUT /api/v1/prefs`. Write on a 500 ms debounce; never write during an active drag or
resize gesture. Render the built-in defaults immediately and patch them once `GET /api/v1/prefs` resolves,
accepting a brief default→saved flash in exchange for dropping the client-side cache.

```json
{
  "version": 1,
  "grid": {
    "order":   ["select","queuePos","name","size","progress","status","dlSpeed","ulSpeed","eta","peers","ratio","uploaded","destination","addedOn","completedOn"],
    "visibility": { "category": false, "tags": false, "user": false },
    "sizing":  { "name": 420, "destination": 240 },
    "sorting": [{ "id": "addedOn", "desc": true }],
    "density": "comfortable"
  },
  "sidebarWidth": 220,
  "sidebarCollapsed": false,
  "detailHeight": 260,
  "detailTab": "general",
  "theme": "system",
  "lastDestination": "/data/iso"
}
```

`grid.order`, `grid.visibility`, `grid.sizing` and `grid.sorting` map 1:1 onto TanStack Table v8 state and
are wired straight through. The v8 API shapes, verbatim:

```ts
export type VisibilityState = Record<string, boolean>
export type VisibilityTableState = { columnVisibility: VisibilityState }
onColumnVisibilityChange?: OnChangeFn<VisibilityState>
setColumnVisibility: (updater: Updater<VisibilityState>) => void
resetColumnVisibility: (defaultState?: boolean) => void
column.getIsVisible: () => boolean
column.toggleVisibility: (value?: boolean) => void
```

Server-side persistence uses `GET /api/v1/prefs` and `PUT /api/v1/prefs` →
[`05-api-contract.md`](05-api-contract.md). `localStorage` is not used for the grid layout.

### 3.4 Sorting, resizing, reordering, visibility

- **Sort**: click a header to cycle ascending → descending → default. `Shift+click` appends a secondary key.
  The header shows ▲/▼ plus a `1`/`2` badge for multi-sort and sets `aria-sort` to `ascending`,
  `descending` or `none`.
- **Resize**: a 5 px grab zone on the right edge of each header cell, `cursor: col-resize`, live preview
  line. Double-click auto-fits to content. Use `columnResizeMode: 'onChange'` with the CSS-variable
  technique from TanStack's "Column Resizing Performant" example so 10 000 rows do not re-render per frame.
- **Reorder**: drag the header cell with `@dnd-kit/sortable` restricted to the horizontal axis. `select` and
  `name` are pinned and non-draggable. The `Columns ▾` popover also offers keyboard *Move up* / *Move down*
  buttons, because drag alone is not accessible.
- **Show/hide**: `Columns ▾` popover with a search box, a checkbox per column, and *Reset to defaults*. The
  same list is available from the header context menu.
- **Density**: `comfortable` = 32 px rows, `compact` = 26 px rows. Set in Settings → General.

### 3.5 Selection

- Click selects one row; `Ctrl/Cmd+click` toggles; `Shift+click` selects the range from the anchor;
  `Ctrl/Cmd+A` selects everything in the current filter; `Esc` clears.
- Selected rows carry `aria-selected="true"`; the grid carries `aria-multiselectable="true"`.
- A selection chip appears in the toolbar: `3 selected · 12.4 GB` with an `✕` that clears the selection.
- Double-click toggles start/pause. Settings → General exposes the action for downloading tasks and for
  completed tasks separately: *Start/stop* / *Open detail* / *No action*.

### 3.6 Keyboard

| Keys | Action |
|---|---|
| `↑` `↓` | Move the focused row |
| `Home` `End` | First / last row |
| `PageUp` `PageDown` | ± one viewport of rows |
| `Ctrl+Home` `Ctrl+End` | First / last cell of the grid |
| `Space` | Toggle selection of the focused row |
| `Shift+Space` | Select the focused row |
| `Ctrl/Cmd+A` | Select all in the current filter |
| `Enter` or `F2` | Open the detail pane on the focused row |
| `Delete` | Remove selected — confirmation dialog |
| `Shift+Delete` | Remove selected with data — confirmation dialog, delete-files box pre-checked |
| `Ctrl/Cmd+F` | Focus the toolbar filter box |
| `Esc` | Clear selection, or close the focused dialog |
| `?` | Shortcut cheat-sheet overlay |

Every handler must bail out when the event target is `INPUT`, `TEXTAREA` or `isContentEditable` — this is
qBittorrent's own guard, and its absence is why its `Escape` handling misfires inside text fields.

### 3.7 Context menu

Built from shadcn/ui's `context-menu` (Radix) so it is keyboard- and touch-reachable. Order, with `—`
marking a separator: Start · Force start · Pause — Remove ▸ (*Remove task* / *Remove task and files*) ·
Move ▸ (*Top* / *Up* / *Down* / *Bottom*) — Set destination… · Rename… · Category ▸ · Tags ▸ — Limit
download rate… · Limit upload rate… · Share-ratio limit… · Sequential download (✓) — Force recheck ·
Force reannounce — Copy ▸ (*Name* / *Source URL* / *Magnet link* / *Info hash* / *Content path*) —
Open detail pane.

BitTorrent-only entries (*Force reannounce*, *Share-ratio limit…*, *Copy magnet link*, *Copy info hash*) are
hidden, not disabled, for non-BitTorrent tasks.

### 3.8 Progress bar

A full-width track with a filled portion. Downloading = `--accent`; seeding = `--ok`; paused = `--fg-muted`
with a static stripe; error = `--error`; checking, extracting and moving = animated indeterminate stripes.
The percentage sits inside the bar, centred, with `mix-blend-mode: difference` so it reads over both halves.
Attributes: `role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow="78"
aria-valuetext="78% — 4.1 GB of 5.2 GB"`.

### 3.9 Virtualisation

- `@tanstack/react-virtual`'s `useVirtualizer({ count, getScrollElement, estimateSize: () => 32, overscan: 10 })`.
- Rows are **fixed height** — 32 px comfortable, 26 px compact. No auto-measurement.
- Column virtualisation is not used; at ≤ 40 columns it adds bugs and buys nothing.
- Exactly one element has `overflow: auto`. The header is a separate sticky row translated in sync with
  `transform: translateX(-scrollLeft)`.
- SSE deltas are merged into a `Map<string, Task>` inside a `zustand` store. Never rebuild the array
  identity on each tick; derive the row array only when the key set or a rendered field changes, so only
  changed rows re-render.
- Budget: ≤ 8 ms of scripting per 1 Hz update tick with 10 000 rows. Measured by the performance test in
  [`13-testing-and-verification.md`](13-testing-and-verification.md).

---

## 4. Add-task dialog

```
┌─ Create Download Task ─────────────────────────────────────────────── ✕ ─┐
│                                                                          │
│  Destination  [ /data/iso                                  ] [ Select ]  │
│               412 GB free of 2 TB                                        │
│                                                                          │
│  Enter URL                                                               │
│  ┌────────────────────────────────────────────────────────────────────┐  │
│  │ magnet:?xt=urn:btih:8f9c…                                          │  │
│  │ https://example.org/ubuntu-26.04.iso                               │  │
│  │ ftp://files.example.org/pub/                                       │  │
│  │                                                                    │  │
│  └────────────────────────────────────────────────────────────────────┘  │
│  One URL per line (max 50). Supported: http://, https://, ftp://,        │
│  ftps://, sftp://, magnet:, and 40-char hex / 32-char base32 info-hashes.│
│                                                                          │
│  … or drop .torrent / .txt files here, or click to browse                │
│  ┌ ⇩ ────────────────────────────────────────────────────────────────┐  │
│  │  ubuntu-26.04.torrent   (28 KB)                              ✕    │  │
│  └────────────────────────────────────────────────────────────────────┘  │
│                                                                          │
│  ☐ Authentication required (for ftp:// only)                             │
│      Username [                ]   Password [                ]           │
│                                                                          │
│  ☐ Show dialog to select files for download                              │
│                                                                          │
│  ▸ More options                                                          │
│      Category [ none ▾ ]   Tags [ + add tag ]                            │
│      ☐ Add paused        ☐ Sequential download                           │
│      Download limit [  0 ] B/s    Upload limit [  0 ] B/s                │
│      ☐ Save in subfolder named after the task                            │
│      Extraction password [                ]                              │
│                                                                          │
│                                        [ Cancel ]   [ Create ]           │
└──────────────────────────────────────────────────────────────────────────┘
```

| Control | Type | Rules |
|---|---|---|
| **Destination** | read-only text + `Select` button | Defaults to the user's default destination, or the last used one when Settings → General has *Remember last destination* on. The free-space line under it refreshes from `GET /api/v1/fs/free-space?path=` on every change. |
| **Enter URL** | textarea, 6 rows, monospace, `spellcheck="false" autocapitalize="off" autocorrect="off"` | One URI per line. A gutter badge per line: ✓ recognised scheme, ⚠ unrecognised, ✕ duplicate of an existing task. Counter `12 / 50` beneath. |
| dropzone | native drag-and-drop + hidden file input | Accepts `.torrent` and `.txt`. A `.txt` is read client-side and its lines appended to the textarea. `.nzb` is rejected with the message `NZB is not supported in v1`. |
| **Authentication required (for ftp:// only)** | checkbox | Label verbatim from Download Station. Unchecked by default; reveals Username and Password (with a reveal toggle). The whole block is disabled and greyed with the tooltip `Only applies to ftp:// URLs` when no `ftp://` line is present. |
| **Show dialog to select files for download** | checkbox | Label verbatim from Download Station. When ticked, `Create` becomes `Next →` and submits to §5 instead of creating. Disabled with a tooltip when no `.torrent` file and no magnet is present. |
| Category | combobox | Existing categories plus inline create. |
| Tags | multi-select with free entry | |
| Add paused | checkbox | Maps to `paused` in `POST /api/v1/tasks`. |
| Sequential download | checkbox | Maps to `sequential`. |
| Download / Upload limit | integer, **bytes per second**, `0` = unlimited | Never KB/s — see [`04-data-model.md`](04-data-model.md) for the unit rule. |
| Save in subfolder named after the task | checkbox | Tooltip carries Download Station's full sentence: *"The subfolder will be named as the same list name displayed here."* |
| Extraction password | password | Passed as `extract_password`; never echoed back by any response. |

Behaviour:

1. The 50-line cap is a **soft warning with a live count**, never a silent truncation. Over 50 lines the
   dialog shows `62 / 50 — the first 50 will be created` and offers *Split into batches*.
2. Dropping files or text onto the **main window**, not just the dialog, opens this dialog pre-filled.
   A dropped *text* payload is split on `\n`, trimmed, and a line is kept only when it satisfies
   qBittorrent's own predicate:

   ```js
   lowercaseStr.startsWith("http:") || lowercaseStr.startsWith("https:") || lowercaseStr.startsWith("magnet:")
   || ((str.length === 40) && !(/[^0-9A-F]/i.test(str)))   // v1 hex-encoded SHA-1 info-hash
   || ((str.length === 32) && !(/[^2-7A-Z]/i.test(str)))   // v1 Base32 encoded SHA-1 info-hash
   ```

   Both regexes are case-insensitive, so lowercase hex and lowercase base32 pass. A dropped directory aborts
   the whole drop with a toast.
3. **Clipboard detection**: on window `focus`, if `navigator.clipboard.readText()` resolves and the text
   satisfies the predicate above and differs from the last handled value, show a non-modal toast
   `Magnet link on clipboard — [Add task] [Dismiss]`. Never auto-open a modal from the clipboard. Store the
   last handled value in `sessionStorage`. A `paste` onto the grid opens the dialog pre-filled.
4. `Enter` inside the textarea inserts a newline; `Ctrl/Cmd+Enter` submits. On submit the dialog closes at
   once and optimistic rows appear in `queued`; a failure rolls them back and raises an error toast naming
   the offending URI.

### 4.1 Server-side folder browser

Opened by every `Select` button in the product. A nested dialog with its own focus trap.

```
┌─ Select destination ────────────────────────────────── ✕ ─┐
│  /data  ›  iso  ›  archive                                │
│  ┌──────────────────────────────────────────────────────┐ │
│  │ ⬑  ..                                                │ │
│  │ 📁 incoming                                     🔒   │ │
│  │ 📁 released                                          │ │
│  │ 📁 tmp                                               │ │
│  └──────────────────────────────────────────────────────┘ │
│  Path [ /data/iso/archive                              ]  │
│  412 GB free of 2 TB          [ New folder ]              │
│                          [ Cancel ]   [ Select ]          │
└───────────────────────────────────────────────────────────┘
```

- Data comes from `GET /api/v1/fs/browse?path=`, `POST /api/v1/fs/mkdir` and `GET /api/v1/fs/roots`; shapes
  live in [`05-api-contract.md`](05-api-contract.md) §7. Directories only — the endpoint never returns files.
- `403 /problems/path-rejected` renders as `That folder is outside the allowed download roots.` and keeps the
  previous listing on screen. `404` renders as `Folder not found or unreadable.`
- A directory with `writable: false` shows a 🔒 badge and cannot be chosen; `Select` is disabled with a
  tooltip.
- Keyboard: `↑` `↓` move, `Enter` or `→` descend, `Backspace` or `←` ascend, `Esc` cancels.
- The manual `Path` field accepts a typed absolute path and validates it on blur through the same endpoint.
- Non-admin users see only the subtree of their default destination; the roots list is already filtered
  server-side, so the client shows whatever it is given without a second rule.

---

## 5. File-selection dialog

```
┌─ Select files to download — ubuntu-26.04-desktop-amd64.iso ─────────── ✕ ─┐
│  Destination [ /data/iso ] [Select]   ☐ Save in subfolder                 │
│  ┌ Filter files…                    ┐  [All] [None] [Invert] [Expand all] │
│  ├───────────────────────────────────────────────────────────────────────┤│
│  │ ☑ ▾ 📁 ubuntu-26.04                          5.2 GB   Normal  ▾       ││
│  │ ☑   ├─ 📄 ubuntu-26.04-amd64.iso             5.1 GB   High    ▾       ││
│  │ ▣   ├─ ▾ 📁 extras                          104 MB   —       ▾       ││
│  │ ☑   │    ├─ 📄 SHA256SUMS                    1 KB    Normal  ▾       ││
│  │ ☐   │    └─ 📄 sample.mkv                   104 MB   Skip    ▾       ││
│  │ ☑   └─ 📄 README.txt                         2 KB    Maximum ▾       ││
│  ├───────────────────────────────────────────────────────────────────────┤│
│  │  Selected 4 of 5 files · 5.10 GB of 5.2 GB wanted · 412 GB free       ││
│  └───────────────────────────────────────────────────────────────────────┘│
│  ◂ Torrent 1 of 3 ▸                       [ Back ]  [ Cancel ]  [ Create ]│
└───────────────────────────────────────────────────────────────────────────┘
```

- The tree is built from the manifest returned by `POST /api/v1/tasks/inspect`. Folders collapse.
  `role="tree"`; items are `role="treeitem"` with `aria-expanded`, `aria-level`, `aria-selected` and
  `aria-checked="true|false|mixed"`.
- **Tri-state checkbox**: ☑ when every descendant is checked, ☐ when none are, ▣ when mixed. Toggling a
  folder sets every descendant. Implement with a real `<input type="checkbox">` whose `.indeterminate` is
  assigned imperatively, or shadcn/ui's `checkbox` with `checked="indeterminate"` — CSS cannot express it.
- **Priority** per row is a select with exactly four options, in this order and with these integers:
  `Skip` = 0 · `Normal` = 1 · `High` = 6 · `Maximum` = 7. There is no "Low". Setting a folder's priority
  applies it to every descendant. `Skip` unchecks the row and unchecking a row sets `Skip`; the checkbox and
  the priority are one concept and must never disagree. Engine-side meaning is in
  [`06-download-engines.md`](06-download-engines.md).
- **Size** per row; a folder shows the sum of its descendants.
- **All / None / Invert** act on the *currently filtered* set and relabel themselves `Invert (filtered)`
  while a filter is active.
- **Filter box** matches a substring, plus the shortcuts `ext:mkv` and `>100MB`. Debounce 250 ms, clear `✕`.
- **Running total** in the footer, `aria-live="polite"`, recomputed on every toggle:
  `Selected N of M files · X of Y wanted · Z free`. Render the free-space figure in `--error` when the wanted
  size exceeds free space, and disable `Create`.
- **Multi-item paging**: when the add dialog carried several torrents, each gets its own page;
  `◂ Torrent i of n ▸` navigates. `Create` is enabled on any page and applies defaults to unvisited pages.
- For a magnet the dialog opens in a `Fetching metadata…` state with a `Cancel` that instead creates the
  task paused.
- Submitting sends `select_files[]` inside `POST /api/v1/tasks`; changing files on an existing task uses
  `PATCH /api/v1/tasks/{id}/files`.

---

## 6. Detail pane

A resizable bottom pane. Tabs are left-aligned, `role="tablist"` with roving tabindex; each panel is
`role="tabpanel"` with `aria-labelledby`. The selected tab is persisted per user. With more than one row
selected, show an aggregate summary (`3 tasks · 12.4 GB · ↓ 4.1 MB/s`) and no tabs.

| Tab | Fields |
|---|---|
| **General** | Name with an inline rename pencil · Destination with *Open folder* and *Change…* · Requested destination, shown only when it differs from the effective one · Size (total / done / remaining) · Added by (user) · Source URI with a copy button · Task type (`source_kind`) · Engine · Info hash v1 · Info hash v2 · Category · Tags · Added at · Started at · Completed at · **Health**, a five-dot indicator derived from connected seeders and peers, with an explanatory tooltip |
| **Transfer** | Progress bar and percentage · Downloaded · Uploaded · Share ratio · Down speed / Up speed · Down limit / Up limit · ETA · Connected seeders / leechers / known peers · Queue position · Sequential (yes/no) · Extraction progress while `state = extracting` · a 60-second inline SVG sparkline of ↓ and ↑ |
| **Trackers** | Grid: URL · Tier · Status · Seeds · Peers · Leeches · Times downloaded · Message · Next announce. Toolbar: **Add trackers…** (multi-line textarea, one per line), **Remove**, **Copy URL**, **Force reannounce**. DHT, PeX and LSD appear as pseudo-rows and cannot be removed. BitTorrent tasks only. |
| **Peers** | Grid: Country/Region · IP:Port · Client · Connection · Flags with a legend popover · Progress · Down speed · Up speed · Downloaded · Uploaded · Relevance. Toolbar: **Copy IP:port**. BitTorrent tasks only. |
| **Files** | The §5 tree, live: adds Progress and Remaining columns. Priority changes apply immediately and optimistically through `PATCH /api/v1/tasks/{id}/files`. Read-only for single-file HTTP/FTP tasks. |
| **Log** | Reverse-chronological events for this task from `GET /api/v1/tasks/{id}/events`: created, started, metadata received, checking, error with the raw message, moved, extracted, completed, deleted. Each row: absolute timestamp with a relative tooltip · level badge · code · message. A **Copy all** button. |

Tabs that do not apply to the selected task are hidden, not disabled. When nothing is selected the pane
collapses to a centred muted line, `Select a task to see its details`, plus the three most used shortcuts.

---

## 7. Search screen

```
┌ Search ───────────────────────────────────────────────────────────────────┐
│ [ ubuntu 26.04              ] [ Indexers: 3 of 4 ▾ ] [ Category: All ▾ ]  │
│                                        [ Search ]  [ Stop ]  [ Save… ]    │
├───────────────────────────────────────────────────────────────────────────┤
│ ● internet-archive 42  ● arch-linux 6  ◐ academic-torrents…  ✕ my-torznab │ ← per-indexer strip
├───────────────────────────────────────────────────────────────────────────┤
│ ☐ Name                                  Size   ↑Seeds ↓Leech  Age Indexer │
│ ☐ ubuntu-26.04-desktop-amd64.iso       5.2 GB    412     37    2d  IA   ⬇ │
│ ☑ archlinux-2026.09.01-x86_64.iso      1.1 GB    198     12    4h  AL   ⬇ │
│ …                                                                         │
├───────────────────────────────────────────────────────────────────────────┤
│ 2 selected · 7.3 GB      [ Download selected ▾ ]  (▾ = Download to…)      │
└───────────────────────────────────────────────────────────────────────────┘
```

- **Indexer multi-select**: a popover checkbox list with *All* / *None*; each row shows the indexer name, a
  health dot and its last error in a tooltip. Disabled indexers are greyed with a link to
  `/settings/indexers`. The selection is persisted in the prefs document and pruned against
  `GET /api/v1/indexers` on load, so an id the server has hidden (a key-bearing indexer,
  [`05-api-contract.md`](05-api-contract.md) §9.1) or deleted never reaches `POST /search`: the list and
  the effective selection are whatever the server returns, and the client adds no rule of its own.
- **Category filter** is populated from `GET /api/v1/indexers/categories`; the client never hard-codes a
  category tree.
- **Results grid** reuses the §3 grid component. Columns: checkbox · Name · Size · Seeders · Leechers ·
  Age (relative published-on) · Indexer · a per-row `⬇` action. Default sort: Seeders descending.
- **Per-indexer status strip** is `aria-live="polite"`. States: queued `○`, searching `◐`, done `● N`,
  error `✕` with the message on hover and on click. **Results render as each indexer answers** — never block
  the grid on the slowest one.
- **Per-row `⬇`** adds the result with the current defaults in one click; the row flashes and the button
  becomes a `✓` linking to the created task. **Download selected ▾** offers exactly two items, matching
  Download Station: *Download immediately* (default destination) and *Download to…* (opens the add dialog
  pre-filled).
  Both paths submit only `search_result_ids`; the browser never receives, reconstructs or reposts a provider
  URL or magnet.
- **Saved searches**: `Save…` captures name, query, indexers and category; saved searches appear under
  Sidebar → Search → Saved Searches with a badge for results new since the last view.
- **Zero states** are three distinct screens: *No results* (every indexer answered with zero), *All indexers
  failed* (list the errors, offer `Retry`), *No indexers enabled* (call to action to `/settings/indexers`).
- Search state survives navigation away and back within the session.

---

## 8. RSS screens

### 8.1 Feeds and items

```
┌ RSS ──────────────────────────────────────────────────────────────────────┐
│ ┌ Feeds ──────────┐┌ Items ────────────────────────────────────────────┐  │
│ │ [+ Feed]        ││ ☐  Title                          Feed    Age  Rule│  │
│ │ ● Arch        9 ││ ●  archlinux-2026.09.01-x86_64…   Arch    1h  [ISO]│  │
│ │ ● Academic    3 ││ ○  archlinux-2026.08.01-x86_64…   Arch    1d  [ISO]│  │
│ │ ⚠ BrokenFeed  ! ││ ○  Some.Dataset.2026                Acad  2d   —  │  │
│ └─────────────────┘│ ┌ Preview ────────────────────────────────────────┐│  │
│  [Update] [Update all]│ Title · pubDate · size · category · torrent URL ││  │
│                    │ │ [ Download ] [ Mark read ] [ Open link ]        ││  │
│                    │ └─────────────────────────────────────────────────┘│  │
└───────────────────────────────────────────────────────────────────────────┘
```

- **Feed list**: a flat list of feeds, each with an unread count; there are no feed folders, because the
  API and schema carry no folder concept. Feed states: OK `●`, loading `◐`, error `⚠`
  with the HTTP status or parse error in the tooltip. Context menu: Update · Rename… · Edit URL… ·
  Mark all read · Copy feed URL · Remove.
- **Add feed** dialog fields: URL · Name · `☐ Automatically download all items`. Ticking the checkbox also
  creates an enabled rule named `auto:<feed_id>`, scoped to that feed's URL, whose `any_of` is empty — every
  item passes — reusing the rules engine as the only auto-download path ([`05-api-contract.md`](05-api-contract.md)
  §10.1). The result is an ordinary rule: it is edited and deleted like any other, and editing the feed's
  URL afterwards does not rewrite it, because rules scope feeds by URL.
  Mark all read · Copy feed URL · Remove.
- **Feed and rule management is admin-only** ([`05-api-contract.md`](05-api-contract.md) §2); a
  non-admin sees this screen read-only, with Update/Refresh actions but no create, edit or remove.
- **Add feed** dialog fields: URL · Name · `☐ Automatically download all items`. Ticking the checkbox also
  creates an enabled rule named `auto:<feed_id>`, scoped to that feed's URL, whose `any_of` is empty — every
  item passes — reusing the rules engine as the only auto-download path ([`05-api-contract.md`](05-api-contract.md)
  §10.1). The result is an ordinary rule: it is edited and deleted like any other, and editing the feed's
  URL afterwards does not rewrite it, because rules scope feeds by URL.
- **Item list** columns: checkbox · Title (unread bold with a filled dot, read muted) · Feed · Age ·
  matched-rule chips. Hovering a chip explains why the rule matched. Multi-select plus *Download selected*,
  *Mark read*, *Mark all read*, and a filter box.
- Selecting an item renders the preview card with its title, publication date, size, category and the
  extracted download URI.

### 8.2 Rule editor

```
┌ RSS Rules ────────────────────────────────────────────────────────────────────────────┐
│ Rules            │ Rule: "Linux ISOs"                       │ Matches                  │
│ [+] [⧉] [🗑]      │                                          │  ● matches 7 of the last │
│ ☑ Linux ISOs     │ ☑ Enabled                                │    50 items              │
│ ☑ Datasets       │ Use regular expressions      ☐           │ ─────────────────────────│
│ ☐ Paused rule    │ Must contain     [ x86_64 iso          ] │ ✓ archlinux-2026.09.01…  │
│                  │ Must not contain [ bootstrap|netboot   ] │ ✓ archlinux-2026.08.01…  │
│                  │ Episode filter   [ 1x2;5;8-15;30-      ] │ ✓ archlinux-2026.07.01…  │
│                  │ ☑ Use smart episode filter               │ ✗ archlinux-bootstrap…   │
│                  │ ─────────────────────────────────────    │   ↳ excluded by "bootstr"│
│                  │ Apply to feeds  [☑ Arch ☑ Academic ☐ …]  │ ✗ Some.Dataset.2026      │
│                  │ Destination     [ /data/iso    ][Select] │   ↳ no match on "x86_64" │
│                  │ Category        [ linux ▾ ]  Tags [ + ]  │ ─────────────────────────│
│                  │ Add stopped     [ Use global ▾ ]         │ Test a title             │
│                  │ Ignore subsequent matches for [0] days   │ [ archlinux-2026.10.01…] │
│                  │ Last match: 3 days ago                   │ [ Test ]  → ✓ MATCH      │
│                  │                          [ Save ]        │   contains "x86_64 iso"  │
└───────────────────────────────────────────────────────────────────────────────────────┘
```

Field labels, left to right, top to bottom: **Enabled** · **Use regular expressions** · **Must contain** ·
**Must not contain** · **Episode filter** · **Use smart episode filter** · **Apply to feeds** ·
**Destination** · **Category** · **Tags** · **Add stopped** · **Ignore subsequent matches for … days** ·
**Last match**.

- The rule document schema and the matching algorithm are owned by
  [`08-rss-automation.md`](08-rss-automation.md); this screen only edits them.
- **Live preview**: every keystroke, on a 250 ms debounce, posts the in-progress rule to
  `POST /api/v1/rules/test` and re-renders the right column. The headline reads exactly
  `matches N of the last 50 items`.
- **Non-matching items are listed too**, greyed with a `✗` and the reason code rendered as a sentence
  underneath. This is the single largest improvement over both Download Station and qBittorrent, which show
  only positives.
- The **test panel** stays docked. Paste a title, press `Test`, and the answer names the clause that decided
  it.
- A regex error renders inline under the field (`Invalid regex: unterminated group at 12`) and the preview
  freezes on the last valid result rather than clearing.
- A `(?)` popover beside the mode toggle carries qBittorrent's help text verbatim:
  *"Regex mode: use Perl-compatible regular expressions"*; *"Wildcard mode: you can use `?` to match any
  single character, `*` to match zero or more of any characters. Whitespaces count as AND operators (all
  words, any order). `|` is used as OR operator. If word order is important use `*` instead of whitespace."*
- **Run rule against existing items** calls `POST /api/v1/rules/{id}/run`, fixing Download Station's
  documented limitation that its Download Filter "only works for newly-added feeds".
- **Import / export rules** as JSON, from the rules-list toolbar.
- The rule's `Destination` uses the §4.1 folder browser.

---

## 9. Settings screens

A left settings-nav and a right scrolling form with sticky section headers. A sticky `Save / Revert` bar
appears only when the form is dirty and announces `3 unsaved changes` through `aria-live="polite"`.

| Section | Contents |
|---|---|
| **General** | UI language · Theme (System / Light / Dark) · Density (Comfortable / Compact) · Date format (Browser default / ISO 8601) · Confirm on delete · Alternating row colours · Action on double-click, set separately for downloading and completed tasks · Default sidebar filter on startup · Remember last destination · Process order (**By date created** / **By user (one task at a time)**) |
| **Connection** | Engine endpoints, read-only when supplied by the environment · `max_active_total`, `max_active_per_engine`, `max_active_per_user` |
| **Bandwidth** | Global download and upload limits in bytes per second, `0` = unlimited · Alternative download and upload limits · radio *Immediately* / *Advanced schedule* · the 24×7 grid (§9.1) |
| **BitTorrent** | Default share-ratio limit, seeding-time limit and the action when reached |
| **Downloads** | Default destination (folder browser) · Watch folders, each with a path and *Delete loaded .torrent files* · Auto-extract archives plus a shared password list · Category → path mapping table · Per-root `min_free_space` |
| **RSS** | Enable RSS fetching · Update interval · Maximum articles kept per feed (the per-feed `item_cap`, edited feed by feed) |
| **Indexers** | Table: Name · Type · URL · Categories · Enabled · Priority · Last test result. Actions: Add · Edit · Test · Test all · Reorder · Import. Imported definitions arrive **disabled** and show their provenance. |
| **Users & Auth** | Users table (username, role, quota, default destination, enabled) · Add / Edit / Delete · Change password · API tokens with a create-once reveal |
| **Notifications** | A per-event × per-channel checkbox matrix. Channels: Webhook, ntfy, Gotify, Apprise. Every channel has **Send test**, which shows the **raw upstream status line and body**. |
| **Advanced** | Log level (read-only; `DLTOOL_LOG_LEVEL` is environment-only) · Settings export and import · Version and build info |

Every control above is backed by a settings key in [`11-config-reference.md`](11-config-reference.md) §5 or
by a CRUD endpoint in [`05-api-contract.md`](05-api-contract.md). Controls that have no backing are cut,
not deferred with a placeholder: proxy settings (the SSRF client is a direct dialer by design), global and
per-task connection counts, DHT/PeX/LSD/encryption and per-task peer ceilings (engine-side preferences
dl-tool does not fan out), auto-append trackers, an incomplete folder and a content-layout default, a
global auto-downloader switch and smart-episode patterns (the rule engine owns both behaviours), log
retention, database vacuum and a reset-to-defaults action (no endpoints), and session lifetime
(`DLTOOL_SESSION_TTL` is infrastructure, environment-only).

### 9.1 The 24×7 schedule grid

```
 BT Alternative Speed Settings   Download [ 1048576 ] B/s   Upload [ 262144 ] B/s   (0 means unlimited)

 Brush:  ( ) No Download   (•) Default Speed   ( ) Alternative Speed        Time zone: Europe/Zurich

        00 01 02 03 04 05 06 07 08 09 10 11 12 13 14 15 16 17 18 19 20 21 22 23
  Mon   ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ░░ ░░ ██ ██ ██ ██ ██ ██ ██ ██ ██ ░░ ░░ ░░ ▓▓ ▓▓ ▓▓ ▓▓
  Tue   ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ░░ ░░ ██ ██ ██ ██ ██ ██ ██ ██ ██ ░░ ░░ ░░ ▓▓ ▓▓ ▓▓ ▓▓
  Wed   …
  Sun   ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓ ▓▓

  Legend  ██ ░░ hatched = No Download (red)   ▓▓ = Default Speed (green)   ░░ = Alternative Speed (amber)
  [ Fill all with brush ]  [ Clear ]  [ Copy Monday to weekdays ]  [ Invert ]
```

- A `<table>` of 24 columns (hours `00`–`23`) × 7 rows (Mon–Sun). Each cell is 28 × 28 px,
  `role="gridcell"`, `aria-label="Monday 14:00 — Default speed"`.
- Exactly **three paint states**, with Download Station's exact labels: **No Download** (red, plus diagonal
  hatching) · **Default Speed** (green, solid) · **Alternative Speed** (amber, dotted). The brush selector
  sits above the grid; the selected brush carries `aria-pressed="true"`.
- Painting: mousedown and drag paints; clicking a row header paints that whole day; clicking a column header
  paints that hour across every day; `Shift+drag` paints a rectangle.
- Keyboard: arrows move the focused cell, `Space` applies the current brush, `Shift`+arrows extends a
  rectangle.
- Buttons: `Fill all with <brush>` · `Clear` · `Copy Monday to weekdays` · `Invert`.
- The legend pairs each colour with a fill pattern and a text label, so the meaning never depends on colour
  alone.
- The alternative-speed inputs sit directly above the grid under the heading **BT Alternative Speed
  Settings** with the hint `0 means unlimited`.
- State is a 168-element array of `0 | 1 | 2`, index `day * 24 + hour`, Monday = day 0, read and written
  with `GET`/`PUT /api/v1/settings/schedule`.
- The active time zone is displayed beside the grid because cells are evaluated in the container's `TZ`.
- The status bar's schedule segment renders the currently active cell.

---

## 10. Cross-cutting rules

### 10.1 Theme and tokens

Three states — **System** (default), **Light**, **Dark** — implemented by toggling `class="dark"` on
`<html>` and persisting the choice in the prefs document. Define every colour once as a semantic custom
property on `:root`, and override the same names under `.dark`. Never define a colour only inside a media
query.

```css
:root {
  --bg: …;            --bg-elevated: …;   --fg: …;         --fg-muted: …;
  --border: …;        --accent: …;        --accent-fg: …;
  --ok: …;            --warn: …;          --error: …;
  --progress-track: …; --progress-fill: …; --focus-ring: …;
}
.dark { /* the same names, dark values */ }
```

Rules: status colours keep at least 3:1 contrast against their background in both themes; every status is
also carried by an icon or a shape; spacing follows an 8 px scale; icons come from `lucide-react` only.

### 10.2 i18n

- `i18next` + `react-i18next`, JSON resources at `web/src/locales/<lang>/<namespace>.json`.
- Namespaces: `common`, `grid`, `dialogs`, `settings`, `rss`, `search`, `errors`.
- **v1 ships `en` only, complete.** The plumbing, the namespaces and the `t()` call sites all exist so that
  adding a language is a data change. Do not scaffold empty locale files.
- Plurals use i18next suffix keys (`key_one` / `key_other`), never inline ICU.
- Every byte count, rate, duration and date goes through `Intl.NumberFormat`,
  `Intl.DateTimeFormat` or `Intl.RelativeTimeFormat` with the active locale. No hand-rolled formatters.
- No string concatenation in code; every user-visible string passes through `t()`. This is enforced by an
  ESLint rule.
- Use logical CSS properties (`margin-inline-start`, `inset-inline-end`) throughout so a future `dir="rtl"`
  locale needs no layout rewrite.

### 10.3 Responsive and mobile

Breakpoints: `< 640 px` mobile · `640–1023 px` tablet · `≥ 1024 px` desktop.

| Breakpoint | Shell | Task list |
|---|---|---|
| Desktop | Sidebar, grid, bottom detail pane | The §3 virtualised table |
| Tablet | Sidebar collapses to the icon rail; the detail pane moves to the right side | The table, roughly seven columns |
| Mobile | Sidebar becomes a slide-over drawer behind ☰; the detail pane becomes a full-screen sheet; the toolbar collapses to `+ Add` and `⋮`; the status bar keeps only the connection dot and the two rates | A **virtualised card list** — the same `useVirtualizer`, a different row renderer |

Mobile card: line 1 the name clamped to two lines, line 2 the progress bar, line 3
`Status · Size · ↓rate · ↑rate · ETA`, and a `⋮` button opening a bottom sheet carrying the §3.7 context
menu. Long-press enters selection mode with a top action bar. Tap targets are at least 44 × 44 px, and no
action is hover-only — every hover affordance also exists in the `⋮` menu.

A web app manifest, maskable icons, `display: standalone` and a `theme-color` matching the dark background
ship in v1. The service worker's only jobs are meeting the install criterion and caching static assets.
**Nothing works offline; the install prompt copy says so.**

### 10.4 Accessibility

**IMPORTANT** The virtualised grid must set `aria-rowcount` to the **total** number of rows, not the number
currently in the DOM. Getting this wrong makes the grid unusable with a screen reader and is the single most
likely accessibility defect in this codebase.

- Grid roles: `role="grid"` on the scroll container with `aria-rowcount`, `aria-colcount` and
  `aria-multiselectable="true"`; each row `role="row"` with a 1-based `aria-rowindex` (the header row is 1);
  each cell `role="gridcell"` with `aria-colindex`; header cells `role="columnheader"` with `aria-sort`.
- **Roving tabindex**: exactly one row or cell inside the grid has `tabindex="0"`, everything else `-1`.
  `Tab` moves out of the grid; arrows move within it.
- The active sidebar node carries `aria-current="page"`.
- Dialogs use shadcn/ui's `dialog` (Radix), which supplies the focus trap, `Esc`, `aria-modal` and focus
  restoration. Never hand-roll a modal.
- Every icon-only button has an `aria-label`; every truncated element has a native `title`.
- Live regions: the status bar is `aria-live="polite"`; success and info toasts are `role="status"`; error
  toasts and the reconnect banner are `role="alert"`.
- Honour `prefers-reduced-motion`: disable the indeterminate stripe animation, sheet slide-ins and row
  transitions.
- Visible `:focus-visible` ring — 2 px, `--focus-ring`, 2 px offset — that survives both themes.
- Target WCAG 2.2 AA, verified by the axe-core run in
  [`13-testing-and-verification.md`](13-testing-and-verification.md).

### 10.5 Empty states

Every list gets an icon, one sentence and one primary action.

| Screen | Copy | Action |
|---|---|---|
| Tasks, none ever | `No downloads yet — paste a link or drop a .torrent file to start.` | `Add task` |
| A filter with no rows | `No tasks are downloading right now.` | `Show all` |
| Search before a query | `Pick your indexers and search.` | — |
| Search, zero hits | `No results for "ubuntu 26.04" across 3 indexers.` | `Try fewer filters` |
| Search, every indexer failed | The per-indexer error list | `Retry` |
| RSS, no feeds | `Add an RSS feed to auto-download new releases.` | `Add feed` |
| RSS, no rules | A worked example rule | `Create rule` |
| Logs empty | `Nothing logged yet.` | — |

### 10.6 Toasts and optimistic updates

- Toasts render bottom-right, bottom-centre on mobile, at most 3 stacked. Success auto-dismisses after 5 s;
  errors never auto-dismiss. Every error toast carries the server's `detail` string, a `Retry` when the
  operation is retriable, and a `Details` disclosure showing the request id.
- **Optimistic**: pause, resume, priority change, category and tag assignment, file selection, queue move,
  rule enable/disable, mark-read. Pattern with TanStack Query: `onMutate` snapshots and patches the cache,
  `onError` rolls back and toasts, `onSettled` invalidates. The row shows a 1 px shimmer while in flight.
- **Never optimistic**: creating a task from a magnet, remove-with-data, force recheck, and anything
  otherwise irreversible.
- Destructive confirmation dialogs name the affected tasks and carry an explicit
  `☐ Also delete downloaded files` checkbox that is **never** pre-checked, except after `Shift+Delete`,
  which pre-checks it and says so in the dialog body.

### 10.7 Truncation

- Task and file names: single line, `overflow: hidden; text-overflow: ellipsis; white-space: nowrap`.
- Paths: middle ellipsis computed in JS (`/data/…/Season 05`) so the leaf stays visible.
- Mobile card names: `-webkit-line-clamp: 2`.
- Status, size, rate and ETA are **never truncated** — size those columns to their widest value.

Every truncated element gets a native `title` and a tooltip that opens after 400 ms on hover or focus.

### 10.8 Reconnect banner

The live transport is `GET /api/v1/events` (SSE), with `GET /api/v1/sync?rid=` as the identical-payload
polling fallback — see [`05-api-contract.md`](05-api-contract.md) §6.

1. When nothing has arrived for two heartbeat intervals — no `sync` event, no heartbeat comment and no
   successful polling response (the server emits a heartbeat every 15 s while idle, so about 30 s of
   silence) — the connection dot turns amber and the grid dims to 70 % opacity.
2. After 5 s of the transport being known down — an SSE `error` event or a closed connection — or once
   silence passes the amber threshold, a full-width banner slides in beneath the toolbar, `role="alert"`:
   `⚠ Lost connection to the server. Reconnecting in 3s…  [Retry now]`. The ladder is monotonic:
   amber can never fire after the banner.
3. Reconnect backoff: 1, 2, 4, 8, 15, 30 s, then every 30 s.
4. After three consecutive SSE failures, fall back to polling `GET /api/v1/sync?rid=` every 2 s and say so
   in the banner; keep attempting SSE in the background.
5. On reconnect, **refetch the full state** with `rid=0` rather than replaying deltas, show a brief
   `Reconnected` success toast, and clear the banner.
6. While disconnected, mutating controls are disabled with the tooltip `Reconnecting…`, and any queued
   optimistic mutation is discarded with a toast rather than silently retried.
7. A `401` on the stream is a **different** banner: `Your session expired. [Sign in]`.

---

## 11. Complaints → requirements

The justification record for every choice above. Both tables are evidence, not decisions to reopen.

### 11.1 Download Station

| Complaint | Requirement for dl-tool |
|---|---|
| BT search silently returns nothing when plugins break; Synology's own KB blames "Malfunction of BT search tools / Local network restrictions / Denial of access to Download Station by search engines" | Per-indexer status strip with explicit error text; partial results stream in as they arrive; a `Test` button per indexer in Settings; an empty grid is never presented as "no results" (§7) |
| Everything lands in one destination folder; no categories or labels | First-class categories and tags, per-category save paths, per-task destination at add time, category assignment from RSS rules and search (§2.4, §4, §8.2) |
| RSS Download Filter "only works for newly-added feeds" (Synology KB, verbatim) | Rules apply to new items and offer `Run rule against existing items`; the live preview always shows what would match today (§8.2) |
| Filters are match/no-match text only, tested through a separate modal | Regex and wildcard modes, episode filter, smart episode filter, a live `matches N of the last 50 items` panel with a reason for every item, and a docked test field (§8.2) |
| Hard caps: 50 URLs per add, 50 files per upload, 2048 tasks for admins and 256 for users | No task cap; the 50-line add cap is a soft warning with a live counter and a *Split into batches* action, never a silent truncation (§4) |
| Errors are hidden inside "Inactive Downloads" — the DS sidebar has no Error node | A dedicated **Error** sidebar node with a count, an `errorCode` column, and a per-task **Log** tab (§2.4, §3.1, §6) |
| No transfer, pieces or availability detail; General only | A separate **Transfer** tab with rates, limits, peer counts and a sparkline (§6) |
| Drag-and-drop documented as needing "Flash Player 9 or later" and "specific browsers only: Chrome, Firefox 4 or onwards" | Native HTML5 multi-file input plus a dropzone; no plugin; works in every modern browser including mobile (§4) |
| Alternative speeds apply to "BT tasks only" | The bandwidth schedule applies to every protocol; per-engine fan-out is specified in [`06-download-engines.md`](06-download-engines.md) |
| No dark mode; dated look | System / Light / Dark driven by a semantic token set (§10.1) |
| Post-update regressions: watched-folder tasks landed in the default destination while the UI showed the requested path | The General tab shows the **effective** destination the server resolved, and surfaces the requested one only when the two differ (§6) |
| No sequential download or first/last-piece options | Sequential download is exposed at add time, in the context menu and in Settings defaults (§4, §3.7) |
| Search results give no age or indexer clarity | Sortable **Age** and **Indexer** columns, default sort Seeders descending (§7) |

### 11.2 qBittorrent WebUI

| Complaint | Requirement for dl-tool |
|---|---|
| "WebUI should be mobile friendly" (#9143), "Add Mobile Friendly WebUI" (#8887), #17504 closed as not planned | Mobile is a first-class breakpoint with a card list and bottom sheets, shipped in v1 (§10.3) |
| Issue #9796, open since 2018-10-31: missing speed graphs, a torrent options dialog, tracker editing in the context menu, watched-folder settings, hidden zero and infinity values, better speed-limit dialogs, RSS rule import/export, notifications | Ship all of them: the Transfer sparkline; one unified per-task edit dialog; tracker add/remove in the context menu and the Trackers tab; watch folders in Settings → Downloads; `0` renders as `—` and unknown ETA as `∞`; numeric byte-per-second limit fields; RSS rule import/export as JSON; the notification matrix in §9 |
| Columns in the trackers and content tables cannot be hidden, resized or reordered | **Every** grid — tasks, files, trackers, peers, search results, RSS items — uses the same column-management component with show/hide, resize, reorder and persistence (§3.3, §3.4) |
| Tracker status is not available as a sortable column (#22121) | The Trackers tab has a sortable Status column (§6) |
| "cluttered, dated design isn't just an aesthetic issue — it affects usability" | One design system: semantic tokens, an 8 px spacing scale, one font stack, `lucide-react` icons only (§10.1) |
| Auto-hiding zero-count status filters makes the sidebar shift | Zero-count Download filters stay in place, dimmed to 45 %; auto-hide is opt-in and applies only to categories and tags (§2.4) |
| The add-link dialog is a bare textarea with no destination, no category and no free-space readout | The Download-Station-style dialog in §4: destination, `Select`, free space, category, tags and per-task limits on one screen, with file selection behind one checkbox |

---

## Decisions referenced

| ADR | Decision |
|---|---|
| [ADR-0006](decisions/0006-sse-with-rid-deltas.md) | Server-sent events with rid deltas for live updates — the grid's only data source |
| [ADR-0007](decisions/0007-react-spa-embedded-in-the-binary.md) | React SPA embedded in the Go binary — one container, one artifact |
| [ADR-0009](decisions/0009-native-cross-protocol-rss-rules.md) | A native cross-protocol RSS rule engine — the rule editor edits dl-tool's own schema |
| [ADR-0013](decisions/0013-mandatory-built-in-authentication.md) | Mandatory built-in authentication — hence `/setup` and `/login` |

## Open questions

- The Torznab category tree behind the search Category filter is `[INFERRED]` in the UI research; the
  client populates it from `GET /api/v1/indexers/categories` and hard-codes nothing, so a different tree
  needs no UI change.
- Download Station also enables "Show dialog to select files for download" for supported file-hosting URLs.
  dl-tool ships no file-hosting support, so §4 enables it for `.torrent` files and magnets only.
- Download Station's irreversible "End incomplete or erroneous download tasks" action maps to the
  `force_complete` action in `POST /api/v1/tasks/actions`, which no toolbar or context menu currently
  surfaces. [NEEDS CLARIFICATION: decide whether v1 exposes `force_complete` in the UI.]
- The research marks the Download Station destination field's read-only behaviour `[INFERRED]`, not
  `[READ]`. §4 keeps it read-only regardless, because free text bypasses the folder browser's jail feedback.

## Change log
| Date | Change |
|---|---|
| 2026-09-01 | Initial version |
| 2026-09-01 | Consistency review: corrected the ADR-0006, ADR-0007 and ADR-0009 links to the canonical filenames; dropped the two stale open questions about `/prefs` and `infohash_v1`/`infohash_v2`, both of which `05-api-contract.md` now specifies, and removed the `localStorage` fallback for the grid layout. |
| 2026-09-01 | Privilege review: the RSS screens are read-only for non-admins (feed and rule writes are admin-only), and the search indexer list notes that key-bearing indexers are absent for non-admins by server rule. |
| 2026-09-01 | The add-feed dialog's *Automatically download all items* checkbox is now wired: `auto_download` on `POST /feeds` creates the `auto:<feed_id>` rule (`05-api-contract.md` §10.1). |
| 2026-09-01 | Security review: made search downloads submit opaque result ids only. |
