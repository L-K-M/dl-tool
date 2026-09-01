# T117 — Build the RSS settings section

| Field | Value |
|---|---|
| **ID** | T117 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T053, T065, T066, T068, T092 |
| **Blocks** | — |
| **Parallel-safe** | no — it also edits the shared files `web/src/components/Settings/SettingsScreen.tsx` and `web/src/locales/en/settings.json` |
| **Implements** | — (renders [FR-070](../02-requirements.md#fr-070-manage-feeds-and-refresh-on-demand) and [FR-140](../02-requirements.md#fr-140-read-and-update-settings-without-exposing-secrets), covered by T065 and T092) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md), [ADR-0009](../decisions/0009-native-cross-protocol-rss-rules.md) |
| **Est. size** | 2 new files, ~260 LOC |

## Goal
`/settings/rss` renders the RSS row of doc 09 §9: enable fetching, the update interval, the maximum
articles kept per feed, the auto-downloader status and the smart-episode-filter patterns. Saving writes
`rss_enabled` and `rss_interval_s` through `PATCH /settings` and the article cap through `PATCH /feeds/{id}`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/09-web-ui-spec.md` §9 Settings screens](../09-web-ui-spec.md#9-settings-screens) — the RSS row's
   five controls and the dirty-bar rule.
2. [`docs/11-config-reference.md` §5 Database-backed settings](../11-config-reference.md#5-database-backed-settings)
   — `rss_enabled` and `rss_interval_s` are the only RSS keys, and the interval's minimum is `60` seconds.
3. [`docs/05-api-contract.md` §11.1 `GET /settings` and `PATCH /settings`](../05-api-contract.md#111-get-settings-and-patch-settings)
   — the flat body, the subset `PATCH` and the `422` for an out-of-range value.
4. [`docs/05-api-contract.md` §10.1 Feeds](../05-api-contract.md#101-feeds) — `item_cap` is a per-feed
   column and `PATCH /feeds/{id}` is the only way to change it.
5. [`docs/08-rss-automation.md` §6.4 Smart episode filter](../08-rss-automation.md#64-smart-episode-filter)
   — the four fixed patterns and the "rejects everything that is not TV-shaped" warning.
6. [`docs/tasks/T053-settings-screens.md`](T053-settings-screens.md) — `SECTIONS`, `IMPLEMENTED` and the
   `Save / Revert` bar this section plugs into.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/Settings/RssSection.tsx` | create | The five controls, the save handler and the feed fan-out. |
| `web/src/components/Settings/RssSection.test.tsx` | create | Save bodies, the minimum, the mixed cap and the read-only blocks. |
| `web/src/components/Settings/SettingsScreen.tsx` | edit | Add `rss` to `IMPLEMENTED` and render the section. |
| `web/src/locales/en/settings.json` | edit | The `settings.rss.*` strings. |

No other file may be modified.

## Interface contract

```tsx
// web/src/components/Settings/RssSection.tsx

/** The two RSS members of GET /settings, doc 11 §5. No other key on this screen exists. */
export interface RssSettings {
  rss_enabled: boolean;
  rss_interval_s: number;   // seconds, server minimum 60
}

/** The fields of GET /feeds this section reads, doc 05 §10.1. */
export interface FeedCap {
  id: string;
  title: string | null;
  url: string;
  item_cap: number;
}

/** The value shown by "Maximum articles kept per feed".
 *  null  → no feed exists yet, the input renders the server default 50 and is disabled;
 *  'mixed' → feeds disagree, the input renders empty with the placeholder "Mixed"; typing a
 *            number and saving applies it to every feed. */
export function commonItemCap(feeds: FeedCap[]): number | 'mixed' | null;

/** Minutes are the unit the user edits; seconds are the unit the API stores.
 *  Round-trips: secondsToMinutes(900) === 15, minutesToSeconds(15) === 900. The input's min is 1
 *  because the server rejects rss_interval_s below 60 with 422. */
export function secondsToMinutes(s: number): number;
export function minutesToSeconds(m: number): number;

export function RssSection(): JSX.Element;
```

The two bodies this section sends, verbatim:

```jsonc
PATCH /settings      {"rss_enabled":true,"rss_interval_s":900}
PATCH /feeds/{id}    {"item_cap":50}          // one call per feed, only when the cap changed
```

Control-to-carrier map — every row of doc 09 §9's RSS cell, and nothing else:

| Control | Carrier | Notes |
|---|---|---|
| Enable RSS fetching | `rss_enabled` | Off pauses polling; feeds and rules are untouched. |
| Update interval | `rss_interval_s` | Edited in minutes, sent in seconds, minimum 1 minute. |
| Maximum articles kept per feed | `item_cap` on every feed | One `PATCH /feeds/{id}` per feed, on save only. |
| Auto-downloader | read-only status + link to `/rss/rules` | `N of M rules enabled`, counted from `GET /rules`. |
| Smart-episode-filter patterns | read-only | The four patterns of doc 08 §6.4, verbatim, not editable in v1. |

## Steps
1. Edit `web/src/locales/en/settings.json`: add the `settings.rss.*` labels, the minute hint, the
   `Mixed` placeholder, the auto-downloader sentence and the smart-filter warning.
2. Create `RssSection.tsx` reading `GET /settings`, `GET /feeds` and `GET /rules` through the generated
   client; render nothing until all three resolve, then seed the form state once.
3. Render `Enable RSS fetching` as a checkbox over `rss_enabled`, and `Update interval` as a number input in
   minutes over `secondsToMinutes(rss_interval_s)` with `min={1}`.
4. Render `Maximum articles kept per feed` from `commonItemCap`, handling all three returns; state beside it
   that the cap is stored per feed and that saving applies the typed value to every feed.
5. Render the auto-downloader row as `N of M rules enabled` with a link to `/rss/rules`, plus one sentence
   saying rules are switched on and off individually there.
6. Render the four smart-episode-filter patterns from doc 08 §6.4 in a read-only `<pre>`, with the warning
   that turning the smart filter on rejects every title that is not TV-shaped, and a link to the rule editor
   where `episode.smart` is set per rule.
7. Mark the form dirty on any change so T053's `Save / Revert` bar appears and announces the count; `Revert`
   restores the seeded state and issues no request.
8. On `Save`, send one `PATCH /settings` with only the changed keys, then one `PATCH /feeds/{id}` per feed
   whose `item_cap` differs; a `422` on the settings call leaves the feed calls unsent.
9. Surface a `422` on the interval against the input itself using `errors[].location`, and a `403`
   `/problems/forbidden` as "only an administrator can change these settings" with the form left dirty.
10. Edit `SettingsScreen.tsx` to add `'rss'` to `IMPLEMENTED` and render `<RssSection />`; do not reorder
    `SECTIONS`.
11. Create `RssSection.test.tsx` with `msw`: an interval of 900 renders `15`; saving 30 sends
    `{"rss_interval_s":1800}` and no other key; two feeds with different caps render `Mixed` and saving `40`
    sends two `PATCH /feeds/{id}` calls; a settings `422` sends no feed call; the patterns block and the
    rules count render read-only, with no input.
12. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestIntervalRendersMinutesAndSavesSeconds` passes and asserts the body carries only changed keys.
- [ ] `TestMixedItemCapFansOutToEveryFeed` passes and asserts one call per feed.
- [ ] `TestSettingsValidationErrorSkipsFeedWrites` passes.
- [ ] `TestSmartFilterPatternsAreReadOnly` asserts the four patterns render and no editable control exists.
- [ ] The section renders exactly the five controls of doc 09 §9's RSS row, in that order.
- [ ] No control on this screen writes a settings key outside `rss_enabled` and `rss_interval_s`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo RSS_SETTINGS_OK
```
Expected: Vitest lists `src/components/Settings/RssSection.test.tsx (5 tests)` as passed with every test
named above, reports `0 failed`, and the final line of stdout is exactly `RSS_SETTINGS_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table and nothing else. Use `git status`, not `git diff`: the two
files this task creates are untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add a settings key. Doc 11 §5 is the authoritative list and it has no global auto-downloader
  switch; rules carry their own `enabled`, so this section links to `/rss/rules` instead of inventing one.
  If a global switch is wanted, doc 11 §5 must gain the key first.
- Do NOT implement `GET`/`PATCH /settings`; T092 owns them. Do NOT implement `/feeds` or `/rules`; T065 and
  T068 own them.
- Do NOT build the feeds screen or the rule editor; T072 and T073 own them. This section shows no feed
  table and no rule form.
- Do NOT make the smart-episode-filter patterns editable. They are compiled into `internal/rss/episode.go`
  by T069 and doc 08 §6.4 fixes them for v1.
- Do NOT reorder or rename a settings section; a Download Station user must find them where doc 09 §9 puts
  them.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
