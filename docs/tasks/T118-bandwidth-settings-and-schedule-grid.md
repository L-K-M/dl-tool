# T118 — Build the Bandwidth settings section and the 24×7 schedule grid

| Field | Value |
|---|---|
| **ID** | T118 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T053, T079, T080, T092, T110 |
| **Blocks** | — |
| **Parallel-safe** | no — it also edits the shared files `web/src/components/Settings/SettingsScreen.tsx` and `web/src/locales/en/settings.json` |
| **Implements** | — (renders [FR-090](../02-requirements.md#fr-090-enforce-global-rate-limits-in-bytes-per-second), [FR-092](../02-requirements.md#fr-092-store-and-edit-a-247-schedule-grid) and [FR-097](../02-requirements.md#fr-097-evaluate-the-schedule-in-the-container-time-zone), covered by T079, T080 and T110) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md), [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| **Est. size** | 3 new files, ~420 LOC |

## Goal
`/settings/bandwidth` renders the global and alternative limits in bytes per second, the
*Immediately* / *Advanced schedule* radio, and the 24 × 7 painting grid of doc 09 §9.1 with its three
brushes, its four bulk buttons, its keyboard map, its pattern-plus-colour legend and the active time zone.
Saving writes the four limits with `PATCH /settings` and all 168 cells with `PUT /settings/schedule`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/09-web-ui-spec.md` §9.1 The 24×7 schedule grid](../09-web-ui-spec.md#91-the-247-schedule-grid) —
   the wireframe, the three paint states, the painting gestures, the keyboard map, the four buttons, the
   legend rule and the displayed time zone.
2. [`docs/09-web-ui-spec.md` §9 Settings screens](../09-web-ui-spec.md#9-settings-screens) — the Bandwidth
   row and the dirty `Save / Revert` bar.
3. [`docs/05-api-contract.md` §11.2 `GET /settings/schedule` and `PUT /settings/schedule`](../05-api-contract.md#112-get-settingsschedule-and-put-settingsschedule)
   — the 168-cell array, the read-only `timezone` and `active_mode`, and the `422`.
4. [`docs/05-api-contract.md` §11.1 `GET /settings` and `PATCH /settings`](../05-api-contract.md#111-get-settings-and-patch-settings)
   and [`docs/11-config-reference.md` §5](../11-config-reference.md#5-database-backed-settings) — the four
   rate-limit keys and `schedule_enabled`.
5. [`docs/06-download-engines.md` §10 Bandwidth precedence and fan-out](../06-download-engines.md#10-bandwidth-precedence-and-fan-out)
   — a `No Download` cell pauses, it never throttles; this is the hint text the section shows.
6. [`docs/09-web-ui-spec.md` §10.4 Accessibility](../09-web-ui-spec.md#104-accessibility) and
   [§10.1 Theme and tokens](../09-web-ui-spec.md#101-theme-and-tokens) — roving tabindex, and the
   `--error` / `--ok` / `--warn` tokens the three states use in both themes.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/Settings/ScheduleGrid.tsx` | create | The `<table>`, the painting gestures, the keyboard map and the pure cell helpers. |
| `web/src/components/Settings/BandwidthSection.tsx` | create | The four limits, the radio, the brush selector, the bulk buttons, the legend and the save handler. |
| `web/src/components/Settings/BandwidthSection.test.tsx` | create | The helpers, the gestures, the keyboard map and the two save bodies. |
| `web/src/components/Settings/SettingsScreen.tsx` | edit | Add `bandwidth` to `IMPLEMENTED` and render the section. |
| `web/src/locales/en/settings.json` | edit | The `settings.bandwidth.*` strings, including the three state names. |

No other file may be modified.

## Interface contract

```tsx
// web/src/components/Settings/ScheduleGrid.tsx

/** The wire encoding of doc 05 §11.2: 0 no download, 1 default speed, 2 alternative speed. */
export type Brush = 0 | 1 | 2;

/** Exactly 168 entries, index day * 24 + hour, day 0 = Monday, hour 0..23, in the schedule's
 *  timezone. Any other length is a bug the server rejects with 422. */
export type Cells = Brush[];

export const DAYS = ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'] as const;

export function idx(day: number, hour: number): number;      // day * 24 + hour

/** Every helper is pure and total: it returns a new 168-entry array and mutates nothing. */
export function paintRect(cells: Cells, from: number, to: number, brush: Brush): Cells;
export function paintDay(cells: Cells, day: number, brush: Brush): Cells;
export function paintHour(cells: Cells, hour: number, brush: Brush): Cells;

/** The four bulk buttons of doc 09 §9.1.
 *  fillAll              → every cell becomes brush.
 *  clearAll             → every cell becomes 1 (Default Speed), the value 00001_init.sql seeds.
 *  copyMondayToWeekdays → Tue..Fri become a copy of Mon; Sat and Sun are untouched.
 *  invert               → 0 becomes 1 and 1 becomes 0; 2 is left alone, so an alternative-speed
 *                         block survives an invert. */
export function fillAll(cells: Cells, brush: Brush): Cells;
export function clearAll(cells: Cells): Cells;
export function copyMondayToWeekdays(cells: Cells): Cells;
export function invert(cells: Cells): Cells;

/** A 24-column by 7-row table. Cells are 28 x 28 px, role="gridcell", labelled
 *  aria-label="Monday 14:00 — Default speed". Pointer: mousedown then drag paints; Shift+drag paints
 *  the rectangle from the anchor; a row header paints that day; a column header paints that hour
 *  across every day. Keyboard: exactly one cell holds tabindex="0", arrows move it, Space applies the
 *  brush, Shift+arrow extends the rectangle from the anchor and paints it. */
export function ScheduleGrid(props: {
  cells: Cells;
  brush: Brush;
  disabled: boolean;
  onChange: (next: Cells) => void;
}): JSX.Element;
```

```tsx
// web/src/components/Settings/BandwidthSection.tsx

/** The four rate-limit keys of GET /settings, doc 11 §5. Bytes per second everywhere; 0 = unlimited.
 *  No KB/s value exists on this screen, in either direction. */
export interface BandwidthSettings {
  download_rate_limit: number;
  upload_rate_limit: number;
  alt_download_rate_limit: number;
  alt_upload_rate_limit: number;
}

/** GET /settings/schedule, doc 05 §11.2. timezone and active_mode are read-only: they are rendered
 *  and echoed back unchanged, and the server ignores whatever a client sends. */
export interface ScheduleBody {
  enabled: boolean;
  cells: number[];
  timezone: string;
  active_mode: 'no_download' | 'default' | 'alternative';
}

/** The radio is a rendering of ScheduleBody.enabled and of nothing else. The wire has no third value.
 *  'immediately'       → enabled:false — the global pair is in force at all times and the grid is inert.
 *  'advanced_schedule' → enabled:true  — the cell in force selects the global or the alternative pair. */
export type ScheduleChoice = 'immediately' | 'advanced_schedule';

export function BandwidthSection(): JSX.Element;
```

The two bodies this section sends, verbatim:

```jsonc
PATCH /settings          {"download_rate_limit":1048576,"alt_download_rate_limit":5242880}
PUT   /settings/schedule {"enabled":true,"cells":[1,1,1,1,1,1,0,0,2,2,"…158 more…"],
                          "timezone":"Europe/Zurich","active_mode":"default"}
```

## Steps
1. Edit `web/src/locales/en/settings.json`: add `settings.bandwidth.*` with the four limit labels, the
   `0 means unlimited` hint, the two radio labels, the three state names — `No Download`, `Default Speed`,
   `Alternative Speed` — the four button labels, the legend and the day and hour headers.
2. Create `ScheduleGrid.tsx` with `idx`, `paintRect`, `paintDay`, `paintHour` and the four bulk helpers as
   pure functions before any JSX, then render the `<table>`: `role="grid"`, a `columnheader` per hour
   `00`–`23`, a `rowheader` per day, and 168 `gridcell`s carrying the doc 09 §9.1 `aria-label`. Give each
   state a fill **pattern** as well as a colour — diagonal hatch on `--error`, solid on `--ok`, dots on
   `--warn` — so the legend never depends on colour alone.
3. Implement the pointer gestures: `pointerdown` sets the anchor and paints, `pointerenter` while pressed
   paints, `Shift` held repaints the rectangle from the anchor on every move, `pointerup` commits one
   `onChange`. A drag emits one `onChange` per move and never a request.
4. Implement the keyboard map with a roving `tabindex`: arrows move the focused cell, `Space` applies the
   brush, `Shift`+arrow extends the rectangle from the anchor and paints it. `Tab` leaves the grid.
5. Honour `disabled`: the grid renders `aria-disabled="true"`, ignores every gesture and every key, and is
   visibly muted while the radio is on *Immediately*.
6. Create `BandwidthSection.tsx` reading `GET /settings` and `GET /settings/schedule`, seeding the form
   state once both resolve.
7. Render the four limits as number inputs in bytes per second with the `0 means unlimited` hint, the
   alternative pair under the heading `BT Alternative Speed Settings` directly above the grid, the
   *Immediately* / *Advanced schedule* radio, the three-way brush selector with `aria-pressed` on the
   selected brush, the four bulk buttons, the legend, and `Time zone: <timezone>` beside the grid.
8. State once, beside the legend, that a `No Download` cell pauses the tasks dl-tool started and never
   throttles them, linking [`06-download-engines.md` §10](../06-download-engines.md#10-bandwidth-precedence-and-fan-out).
9. On `Save`, send `PATCH /settings` with only the changed limit keys, then `PUT /settings/schedule` with
    all 168 cells and `enabled`; a `422` on either call leaves the form dirty and shows the server `detail`.
10. Edit `SettingsScreen.tsx` to add `'bandwidth'` to `IMPLEMENTED` and render `<BandwidthSection />`; do
    not reorder `SECTIONS`.
11. Create `BandwidthSection.test.tsx` with `msw`: the four helpers against a known grid; a drag from
    `idx(0,0)` to `idx(0,3)` paints four cells; `Shift`+drag paints a rectangle; a row header paints 24
    cells and a column header paints 7; `Space` paints the focused cell and only one cell has
    `tabindex="0"`; `invert` leaves every `2` untouched; `Save` sends exactly one `PATCH /settings` with the
    changed keys and one `PUT /settings/schedule` whose `cells` has length 168; *Immediately* makes the grid
    `aria-disabled` and still sends `enabled:false`.
12. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestBulkHelpersArePure` asserts all four helpers return a new 168-entry array and mutate nothing.
- [ ] `TestDragPaintsRange`, `TestShiftDragPaintsRectangle` and `TestHeaderPaintsDayAndHour` pass.
- [ ] `TestKeyboardMapPaintsAndRoves` asserts exactly one cell carries `tabindex="0"` at all times.
- [ ] `TestInvertLeavesAlternativeUntouched` passes.
- [ ] `TestSaveSendsBothBodies` asserts `cells.length === 168` and that no KB/s value is sent anywhere.
- [ ] The legend pairs every state with a pattern and a text label, and the displayed time zone is the
      `timezone` the server returned, never one computed in the browser.
- [ ] Every cell carries the doc 09 §9.1 `aria-label`, and the axe-core Settings run stays at zero serious
      or critical violations.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo BANDWIDTH_OK
```
Expected: Vitest lists `src/components/Settings/BandwidthSection.test.tsx (8 tests)` as passed with every
test named above, reports `0 failed`, and the final line of stdout is exactly `BANDWIDTH_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table and nothing else. Use `git status`, not `git diff`: the three
files this task creates are untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement any server behaviour. `GET`/`PUT /settings/schedule` is T080, the fan-out is T079, the
  once-a-minute evaluation is T081, and the precedence chain and the DST rules are T110.
- Do NOT render the active cell in the status bar; T044 owns the status bar and reads `active_mode`.
- Do NOT resolve the active cell in the browser. `active_mode` is read-only and comes from the server so a
  DST boundary is never computed twice.
- Do NOT build the per-task limit controls; T082 owns them and they live in the task detail pane.
- Do NOT build the Downloads, BitTorrent, Users, Notifications or Advanced sections, or per-root
  `min_free_space`; T119, T120 and T121 own those.
- Do NOT convert a limit to or from KB/s, and do NOT add a settings key: doc 11 §5 is the authoritative list.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
