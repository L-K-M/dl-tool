# T052 — Complete the i18n plumbing and the string-literal lint rule

| Field | Value |
|---|---|
| **ID** | T052 |
| **Milestone** | M3 |
| **Status** | todo |
| **Depends on** | T039, T044, T048, T049 |
| **Blocks** | T053 |
| **Parallel-safe** | no — extends T039's `i18n.ts` and `format.ts` |
| **Implements** | [NFR-008](../02-requirements.md#nfr-008-ship-translation-plumbing-with-english-only) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md) |
| **Est. size** | 2 new files, ~250 LOC |

## Goal
Every user-visible string in `web/src/` reaches the screen through `t()`, every number and date through
`Intl` bound to the active locale, and `make lint` fails on a bare literal. The complete `en` catalogue
covers the seven namespaces and an `errors` namespace mapping every problem type and task-event code.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/09-web-ui-spec.md` §10.2 i18n](../09-web-ui-spec.md#102-i18n) — namespaces, the plural rule, the
   `Intl` rule, the "no empty locale files" rule and logical CSS properties.
2. [`docs/05-api-contract.md` §1.3 Errors](../05-api-contract.md#13-errors--rfc-9457-applicationproblemjson)
   — the problem-type registry the `errors` namespace names.
3. [`docs/14-conventions.md` §4 The `task_events` code vocabulary](../14-conventions.md#4-the-task_events-code-vocabulary) — every
   code needing a translated sentence.
4. [`docs/tasks/T039-ui-stack-and-design-tokens.md`](T039-ui-stack-and-design-tokens.md) — `NAMESPACES`,
   `initI18n` and the existing `common` catalogue.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/i18n.ts` | edit | Register all seven namespaces, plural handling and the missing-key handler. |
| `web/src/locales/en/errors.json` | create | Problem types and `task_events` codes as sentences. |
| `web/src/i18n.test.ts` | create | Catalogue completeness and plural resolution. |
| `web/src/lib/format.ts` | edit | Bind the default locale of every formatter to the active i18next language. |
| `web/eslint.config.js` | edit | The bare-literal rule over `web/src/**/*.tsx`. |

No other file may be modified.

## Interface contract

```ts
// web/src/i18n.ts
export const NAMESPACES = ['common', 'grid', 'dialogs', 'settings', 'rss', 'search', 'errors'] as const;
export type Namespace = (typeof NAMESPACES)[number];

/** en only in v1. Adding a language must be a data change, never a code change. */
export const SUPPORTED_LOCALES = ['en'] as const;

/** Throws in test and development when a key is missing, so a gap fails a test instead of
 *  rendering the key to a user. In production it returns the key unchanged. */
export function initI18n(opts?: { throwOnMissing?: boolean }): typeof i18next;

/** The active language, used as the default locale argument of every formatter. */
export function activeLocale(): string;
```

```ts
// web/src/lib/format.ts — signature unchanged; only the default changes
export function formatBytes(bytes: number | null, locale: string = activeLocale()): string;
```

`web/src/locales/en/errors.json`, keyed by the slug of the problem type and by the event code:

```json
{
  "problem": {
    "path-rejected": "That folder is outside the allowed download roots.",
    "quota-exceeded": "This download would exceed your storage quota.",
    "engine-unavailable": "The download engine is not reachable right now.",
    "unsupported-scheme": "That kind of link is not supported."
  },
  "event": {
    "task.created_one": "{{count}} task created",
    "task.created_other": "{{count}} tasks created"
  }
}
```

The ESLint rule, using core ESLint only so no unpinned plugin is introduced:

```js
// web/eslint.config.js
rules: {
  'no-restricted-syntax': ['error',
    { selector: 'JSXText[value=/[A-Za-z]{2,}/]',
      message: 'User-visible text must go through t(); add the key to a locale catalogue.' },
    { selector: 'JSXAttribute[name.name=/^(title|placeholder|aria-label|alt)$/] > Literal',
      message: 'User-visible attributes must go through t().' },
  ],
}
```

## Steps
1. Edit `web/src/i18n.ts` to load all seven namespace catalogues, set `fallbackLng: 'en'`,
   `interpolation.escapeValue: false`, and a `parseMissingKeyHandler` that throws when
   `throwOnMissing` is set.
2. Use i18next suffix plurals (`key_one`, `key_other`) everywhere; never inline ICU syntax.
3. Create `web/src/locales/en/errors.json` covering every problem type in doc 05 §1.3 and every code in
   doc 14 §4, each as a complete sentence with no value interpolated into the key itself.
4. Edit `web/src/lib/format.ts` so each formatter's `locale` parameter defaults to `activeLocale()`,
   changing no call site and no output for `en`.
5. Edit `web/eslint.config.js` with the two selectors above, scoped to `src/**/*.tsx`, and fix every
   violation the rule reports by moving the string into the right namespace catalogue.
6. Create `web/src/i18n.test.ts`: the four catalogues in use at M3 — `common`, `grid`, `dialogs` and
   `errors` — exist under `src/locales/en/`, parse, and are listed in `NAMESPACES`; every catalogue file
   present on disk is a reserved namespace; no value is an empty string; `t('errors:problem.path-rejected')` returns the sentence, not the
   key; a plural key resolves for `count: 1` and `count: 2`; `initI18n({throwOnMissing:true})` throws for
   an unknown key.
7. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestM3CataloguesExist`, `TestNoEmptyCatalogueValues` and `TestPluralKeysResolve` pass.
- [ ] `TestMissingKeyThrowsInTests` passes.
- [ ] `make lint` reports zero `no-restricted-syntax` violations across `web/src/**/*.tsx`.
- [ ] No locale other than `en` exists, no namespace file is empty, and no catalogue exists for a name
      outside `NAMESPACES`.
- [ ] Every formatter still returns the doc 09 renderings with the locale forced to `en`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo I18N_OK
```
Expected: ESLint prints no error, Vitest reports `Test Files  12 passed (12)` including
`src/i18n.test.ts`, every test named above appears as passing, and the final line of stdout is exactly
`I18N_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add a second locale, a translation-management service, or an empty `de`/`fr` catalogue.
- Do NOT add `eslint-plugin-i18next` or any unpinned plugin; the core rule above is the enforcement.
- Do NOT create the `rss` or `search` catalogues with placeholder content; M5 and M4 create them with their
  screens, and `NAMESPACES` already reserves the names.
- Do NOT hand-roll a number, byte or date formatter to work around a locale problem.
- Do NOT change any formatter's output for `en`; the doc 09 examples are the contract.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
