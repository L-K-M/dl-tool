# T116 — Build the Indexers settings section

| Field | Value |
|---|---|
| **ID** | T116 |
| **Milestone** | M4 |
| **Status** | todo |
| **Depends on** | T053, T055, T058 |
| **Blocks** | — |
| **Parallel-safe** | no — it also edits the shared files `web/src/components/Settings/SettingsScreen.tsx` and `web/src/locales/en/settings.json` |
| **Implements** | — (renders [FR-052](../02-requirements.md#fr-052-ship-four-legitimate-engines-and-no-piracy-indexers), [FR-053](../02-requirements.md#fr-053-import-legacy-definitions-by-static-analysis-only) and [FR-056](../02-requirements.md#fr-056-test-an-indexer-on-demand), covered by T057, T059 and T058) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md), [ADR-0008](../decisions/0008-torznab-first-declarative-yaml-second.md), [ADR-0010](../decisions/0010-never-execute-third-party-definitions.md) |
| **Est. size** | 3 new files, ~400 LOC |

## Goal
`/settings/indexers` renders the indexer table of doc 09 §9 — Name, Type, URL, Categories, Enabled,
Priority, Last test — with Add, Edit, Test, Test all, Move up/down and Import. An imported indexer arrives
disabled and shows its provenance, and no API key is ever displayed.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/09-web-ui-spec.md` §9 Settings screens](../09-web-ui-spec.md#9-settings-screens) — the Indexers
   row: the seven columns, the six actions and the disabled-with-provenance rule.
2. [`docs/05-api-contract.md` §9.1 Indexer CRUD](../05-api-contract.md#91-indexer-crud) — the indexer
   object, the accepted request fields, the status codes, the test result and the two import bodies.
3. [`docs/tasks/T055-indexer-store-and-provider-wizard.md`](T055-indexer-store-and-provider-wizard.md) —
   `IndexerDTO`, the `ORDER BY priority, name` listing and the JSON import branch.
4. [`docs/tasks/T053-settings-screens.md`](T053-settings-screens.md) — `SECTIONS`, `IMPLEMENTED`, the
   section nav and the dirty `Save / Revert` bar this section plugs into.
5. [`docs/09-web-ui-spec.md` §10.6 Toasts and optimistic updates](../09-web-ui-spec.md#106-toasts-and-optimistic-updates)
   — enabling a row is optimistic; an import is not.
6. [`docs/14-conventions.md` §5 Frontend conventions](../14-conventions.md#5-frontend-conventions) — the
   generated client is the only transport and every string goes through `t()`.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/Settings/IndexersSection.tsx` | create | The table, the toolbar, Test / Test all and the reorder buttons. |
| `web/src/components/Settings/IndexerDialog.tsx` | create | The Add, Edit and Import forms in one Radix dialog. |
| `web/src/components/Settings/IndexersSection.test.tsx` | create | Listing, test result, reorder, import and secret-redaction cases. |
| `web/src/components/Settings/SettingsScreen.tsx` | edit | Add `indexers` to `IMPLEMENTED` and render the section. |
| `web/src/locales/en/settings.json` | edit | The `settings.indexers.*` strings. |

No other file may be modified.

## Interface contract

```tsx
// web/src/components/Settings/IndexersSection.tsx

/** One row of GET /indexers, doc 05 §9.1. `api_key_set` is a boolean because the key itself is
 *  never returned; there is no field on this screen that could render it. */
export interface IndexerRow {
  id: string;
  name: string;
  kind: 'torznab' | 'newznab' | 'dlsearch';
  enabled: boolean;
  url: string | null;
  api_key_set: boolean;
  definition_id: string | null;
  definition_source: string | null;   // bundled | user | imported
  provenance: string | null;
  legal_tier: string;                 // legitimate | user-supplied
  priority: number;
  seeders_unknown: boolean;
  categories: { id: number; name: string }[];
  last_test_at: string | null;        // RFC 3339 UTC
  last_error: string | null;
}

/** POST /indexers/{id}/test, doc 05 §9.1. A reachable-but-broken indexer is a 200 with ok:false. */
export interface IndexerTestResult {
  ok: boolean;
  elapsed_ms: number;
  categories_found: number;
  server: string;
  error: string | null;
}

export function IndexersSection(): JSX.Element;

/** Rows render in the server's order — ORDER BY priority, name, lower value first.
 *  Move up swaps the two rows' priority values with one PATCH /indexers/{id} each and changes
 *  nothing else. It is a no-op on the first row, and on any row whose neighbour shares its priority. */
export function swapPriority(rows: IndexerRow[], id: string, dir: -1 | 1):
  { id: string; priority: number }[];
```

```tsx
// web/src/components/Settings/IndexerDialog.tsx
export type IndexerDialogState =
  | { mode: 'add' }
  | { mode: 'edit'; indexer: IndexerRow }
  | { mode: 'import' };

/** onClose(true) means the caller must refetch GET /indexers. */
export function IndexerDialog(props: {
  state: IndexerDialogState | null;
  onClose: (changed: boolean) => void;
}): JSX.Element | null;
```

The three bodies this section sends, verbatim:

```jsonc
POST  /indexers        {"name":"Prowlarr","kind":"torznab","url":"https://prowlarr.example/1/api",
                        "api_key":"…","enabled":true,"priority":50}
PATCH /indexers/{id}   {"enabled":false}            // also {"priority":40} for a reorder
POST  /indexers/import {"torznab_url":"https://prowlarr.example","api_key":"…"}
```

`POST /indexers/import` also accepts `multipart/form-data` with one `file` part
(`.dlsearch.yaml`, `.dlm`, `.py`). Its `201` body carries `indexer` and `warnings[]`; render every warning.

## Steps
1. Edit `web/src/locales/en/settings.json`: add the `settings.indexers.*` keys — the seven column headers,
   the six action labels, the dialog fields and the empty state.
2. Create `IndexersSection.tsx` fetching `GET /indexers` through the generated client and rendering one
   `<table>` in the returned order, with `Name`, `Type` (`kind`), `URL`, `Categories` (the count, with the
   names in a `title`), `Enabled`, `Priority` and `Last test`.
3. Render `Last test` as `last_test_at` through `formatWhen`, plus `last_error` as a warning line under the
   name when it is non-null. A row with `last_test_at: null` renders `Never tested`.
4. Render `provenance` as a muted line under the name of any row whose `definition_source` is not
   `bundled`, and state once above the table that imported definitions arrive disabled and are converted by
   static analysis only, linking [ADR-0010](../decisions/0010-never-execute-third-party-definitions.md).
5. Wire the `Enabled` checkbox to `PATCH /indexers/{id}` with `{"enabled":…}`, optimistically per doc 09
   §10.6: patch the cache in `onMutate`, roll back and toast in `onError`, invalidate in `onSettled`.
6. Wire `Test` to `POST /indexers/{id}/test` and render the returned `ok`, `elapsed_ms`,
   `categories_found`, `server` and `error` in the row. `Test all` runs the same call over every row
   sequentially and reports a per-row outcome; `ok:false` is a result, never an error toast.
7. Implement `swapPriority` and the `Move up` / `Move down` buttons over it; each click issues at most two
   `PATCH /indexers/{id}` calls and then refetches.
8. Create `IndexerDialog.tsx` on shadcn/ui's `dialog`: `add` and `edit` show Name, Type, URL, API key
   (`type="password"`, empty on edit and sent only when the user typed a new one), Enabled and Priority;
   `import` shows the file picker and the `torznab_url` + `api_key` pair, and renders `warnings[]` on `201`.
9. Map the documented failures to the toast text: `409` `/problems/conflict` to "an indexer for that
   definition already exists", `422` to the field named in `errors[].location`, and `403`
   `/problems/ssrf-blocked` to the blocked URL.
10. Edit `SettingsScreen.tsx` to add `'indexers'` to `IMPLEMENTED` and render `<IndexersSection />`; do not
    reorder `SECTIONS`.
11. Create `IndexersSection.test.tsx` with `msw` handlers: four bundled rows render in server order; a
    `Test` returning `ok:false` renders the upstream error and raises no error toast; `Test all` issues one
    call per row; `Move up` issues exactly two `PATCH` calls with swapped `priority` values; an import
    returning `enabled:false` renders the provenance line and the warnings; and no rendered text ever
    contains an API key.
12. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestRowsRenderInServerOrder`, `TestProbeFailureRendersAsData` and `TestTestAllRunsEveryRow` pass.
- [ ] `TestMoveUpSwapsPriorities` asserts exactly two `PATCH` requests and the swapped values.
- [ ] `TestImportedIndexerRendersDisabledWithProvenance` passes and asserts the warnings list.
- [ ] `TestApiKeyNeverRendered` asserts no DOM text node contains the stubbed key, in any of the three
      dialog modes.
- [ ] The section renders the seven columns of doc 09 §9 under those names, in that order.
- [ ] Enabling a row is optimistic and rolls back on a `403`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo INDEXERS_OK
```
Expected: Vitest lists `src/components/Settings/IndexersSection.test.tsx (6 tests)` as passed with every
test named above, reports `0 failed`, and the final line of stdout is exactly `INDEXERS_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table and nothing else. Use `git status`, not `git diff`: the three
files this task creates are untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement any server endpoint. `/indexers` CRUD and the JSON import branch are T055; the probe is
  T058; the `.dlm` and `.py` file branches are T059 and T060.
- Do NOT render the category tree editor. `GET /indexers/categories` belongs to the search screen's category
  filter, which T063 owns; this section shows only the categories the indexer reported.
- Do NOT touch the search screen, the RSS section, or any other member of `SECTIONS`; T117, T118, T119,
  T120 and T121 own the remaining sections.
- Do NOT display, log or place an API key in a URL, a `title`, a toast or a test payload — doc 05 §9.1
  returns `api_key_set`, never the key.
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
