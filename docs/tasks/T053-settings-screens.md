# T053 — Build the settings screen shell, General and Connection

| Field | Value |
|---|---|
| **ID** | T053 |
| **Milestone** | M3 |
| **Status** | todo |
| **Depends on** | T027, T045, T047, T050, T052 |
| **Blocks** | T104 |
| **Parallel-safe** | no — it also edits the shared file `web/src/App.tsx` |
| **Implements** | — (renders [FR-143](../02-requirements.md#fr-143-list-engines-and-test-connectivity), covered by T027, and the client half of [FR-144](../02-requirements.md#fr-144-persist-server-side-ui-preferences-per-user), covered by T107) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md), [ADR-0017](../decisions/0017-exclusive-control-of-engines.md) |
| **Est. size** | 4 new files, ~400 LOC. The shell cannot compile without at least the two sections it routes to. |

## Goal
`/settings/:section` renders Download Station's settings layout: a left section nav, a scrolling form with
sticky headers and a `Save / Revert` bar that appears only when dirty. General edits the preference
document; Connection lists the engines with a per-engine `Test` reporting the raw result.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/09-web-ui-spec.md` §9 Settings screens](../09-web-ui-spec.md#9-settings-screens) — the section
   table and the dirty-bar rule.
2. [`docs/09-web-ui-spec.md` §2.1 Routes](../09-web-ui-spec.md#21-routes) — the nine `:section` values.
3. [`docs/05-api-contract.md` §11.3 `GET /engines` and `POST /engines/{id}/test`](../05-api-contract.md#113-get-engines-and-post-enginesidtest)
   — the engine object, the capability list and the always-`200` test result.
4. [`docs/tasks/T045-column-management-and-ui-prefs.md`](T045-column-management-and-ui-prefs.md) — the
   preference document General edits.
5. [`docs/09-web-ui-spec.md` §10.4 Accessibility](../09-web-ui-spec.md#104-accessibility) — the live region
   announcing the unsaved-change count.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/Settings/SettingsScreen.tsx` | create | Section nav, routing, sticky headers and the dirty bar. |
| `web/src/components/Settings/GeneralSection.tsx` | create | The preference-document form. |
| `web/src/components/Settings/ConnectionSection.tsx` | create | The engines table and `Test`. |
| `web/src/components/Settings/SettingsScreen.test.tsx` | create | Routing, dirty bar, prefs writes and the test button. |
| `web/src/locales/en/settings.json` | create | Section names, labels and hints. |
| `web/src/App.tsx` | edit | Route `/settings/:section` to the screen. |

No other file may be modified.

## Interface contract

```tsx
// web/src/components/Settings/SettingsScreen.tsx
export const SECTIONS = ['general','connection','bandwidth','bittorrent','downloads',
                         'rss','indexers','users','notifications','advanced'] as const;
export type Section = (typeof SECTIONS)[number];

/** Sections whose endpoints exist at M3. Every other section renders the one-line note
 *  "This section arrives with <milestone>." and is never a broken form. */
export const IMPLEMENTED: Section[] = ['general', 'connection'];

export function SettingsScreen(): JSX.Element;
```

```tsx
// web/src/components/Settings/GeneralSection.tsx
/** Every control writes through useUiPrefs.patch; nothing here calls the settings API. */
export function GeneralSection(): JSX.Element;
```

Controls, exactly the General row of doc 09 §9 minus the members no M3 endpoint backs:

| Control | Prefs member | Values |
|---|---|---|
| Theme | `theme` | `System` \| `Light` \| `Dark` |
| Density | `grid.density` | `Comfortable` \| `Compact` |
| Default sidebar filter on startup | `startupFilter` | the seven `DOWNLOAD` filters |
| Remember last destination | `rememberLastDestination` | boolean |
| Confirm on delete | `confirmOnDelete` | boolean |
| Action on double-click | `doubleClickDownloading`, `doubleClickCompleted` | `Start/stop` \| `Open detail` \| `No action` |

Unknown members are stored verbatim by `PUT /prefs` (doc 05 §11.4), so these four new members need no
server change.

```tsx
// web/src/components/Settings/ConnectionSection.tsx
/** Read-only engine rows plus one Test button each. Endpoint values supplied by the environment are
 *  rendered disabled with the reason, never as an editable field. */
export function ConnectionSection(): JSX.Element;
```

The test result renders verbatim: `ok`, `version`, `elapsed_ms` and, when present, the transport `error`
string. A failed probe is a `200` with `ok:false` and must not raise an error toast.

## Steps
1. Create `web/src/locales/en/settings.json` with the ten section names, the General labels and the
   Connection column headers.
2. Create `SettingsScreen.tsx`: a left nav over `SECTIONS` with `aria-current="page"`, a scrolling panel
   with sticky section headers, and a `Save / Revert` bar that appears only when the form is dirty and
   announces `N unsaved changes` through `aria-live="polite"`.
3. Render an unimplemented section as the single note above; never render a form whose Save cannot work.
4. Create `GeneralSection.tsx` writing every control through `useUiPrefs.patch`, and applying the theme
   immediately with T039's `applyTheme`.
5. Create `ConnectionSection.tsx` listing `GET /engines` rows: name, kind, url, connected dot, version,
   capabilities, `last_seen_at`, `last_error`, and a `Test` button posting `POST /engines/{id}/test`.
6. Render each engine's `last_error` as a warning row beside its name — that field is where T101 records
   a boot conformance failure — and state once, beside the table, that dl-tool assumes exclusive control of
   its engines and ignores transfers it did not create, linking
   [ADR-0017](../decisions/0017-exclusive-control-of-engines.md).
7. Never render a secret: the engine object carries none, and no field in this screen is a password input.
8. Edit `web/src/App.tsx` to route `/settings/:section` to `SettingsScreen`, redirecting an unknown section
   to `/settings/general`.
9. Create `SettingsScreen.test.tsx`: an unknown section redirects; `general` renders the six controls;
   changing Density writes `grid.density` and the dirty bar appears then clears on Save; `connection`
   renders two engine rows from a stubbed `GET /engines`; a `Test` returning `ok:false` renders the error
   string and raises no error toast; `bandwidth` renders the note, not a form.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestUnknownSectionRedirects` and `TestGeneralWritesPrefs` pass.
- [ ] `TestDirtyBarAppearsAndAnnounces` passes with the `aria-live` count text.
- [ ] `TestEngineTestFailureRendersError` passes and asserts no error toast.
- [ ] `TestConformanceWarningRendersFromLastError` passes.
- [ ] Every section in `SECTIONS` is reachable and none renders a non-functional form.
- [ ] No control in this screen calls `GET /settings` or `PATCH /settings`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo SETTINGS_OK
```
Expected: Vitest reports `Test Files  13 passed (13)` including
`src/components/Settings/SettingsScreen.test.tsx`, every test named above appears as passing, and the final
line of stdout is exactly `SETTINGS_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT call `GET /settings` or `PATCH /settings`; T092 owns those endpoints and the sections that need
  them arrive with their own milestone.
- Do NOT build the 24×7 schedule grid or the bandwidth section; M6 owns
  [FR-092](../02-requirements.md#fr-092-store-and-edit-a-247-schedule-grid).
- Do NOT build the Indexers, RSS, Users, Notifications or Advanced forms; M4, M5, M6 and M7 own them.
- Do NOT render an engine password, secret or token field, even disabled.
- Do NOT reorder or rename a section; a Download Station user must find them where doc 09 §9 puts them.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
