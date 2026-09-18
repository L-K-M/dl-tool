# T129 — Serve the UI preference document and persist web prefs through it

| Field | Value |
|---|---|
| **ID** | T129 |
| **Milestone** | M4 |
| **Status** | todo |
| **Depends on** | T007, T008, T014, T040, T044, T045, T053 |
| **Blocks** | T064 |
| **Parallel-safe** | no — edits the shared files `internal/api/server.go`, `internal/store/settings.go`, `web/src/App.tsx` and `web/src/store/useUiPrefs.ts` |
| **Implements** | [FR-144](../02-requirements.md#fr-144-persist-server-side-ui-preferences) |
| **Decisions** | [ADR-0003](../decisions/0003-chi-huma-code-first-openapi.md), [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md) |
| **Est. size** | 2 new files, 14 modified, ~800 LOC |

## Goal
`GET`/`PUT /prefs` serve the one server-side UI preference document — the pair was T107's but T064's
merged `## Blocked` record showed M4 cannot close without it, so it moved forward into this task. The
web client then stops persisting the document in `localStorage`: `useUiPrefs` hydrates from
`GET /prefs` once the session authenticates and writes the whole document through `PUT /prefs` on the
existing 500 ms debounce. T064's saved searches ride the document as [`05-api-contract.md`
§11.4](../05-api-contract.md#114-get-prefs-and-put-prefs) specifies; nothing here is search-specific.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §11.4 `GET /prefs` and `PUT /prefs`](../05-api-contract.md#114-get-prefs-and-put-prefs)
   — the wire contract: whole document, unknown members verbatim, 64 KiB cap.
2. [`docs/09-web-ui-spec.md` §3.3 Persistence](../09-web-ui-spec.md#33-persistence) — render the built-in
   defaults immediately and patch once `GET` resolves; write on a 500 ms debounce and never mid-gesture;
   `localStorage` is not used.
3. [`docs/04-data-model.md` §3.6](../04-data-model.md#36-jobs-schedule-and-preferences) — the `ui_prefs`
   DDL (one row per top-level member, keyed `(user_id, key)`) — and [§8](../04-data-model.md#8-table-reachability)
   for the table's reachability entry.
4. [`docs/tasks/T045-column-management-and-ui-prefs.md`](T045-column-management-and-ui-prefs.md) — the
   store being migrated: `useUiPrefs` persists to `localStorage` only because no endpoint existed; the
   `theme.ts` and `GeneralSection.tsx` direct writers the switch must absorb.
5. [`docs/13-testing-and-verification.md` §7.1](../13-testing-and-verification.md) — the generated pair
   `api/openapi.json` and `web/src/api/schema.d.ts` rides the Files table implicitly for the two new
   operations; run `make gen` and commit both.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/prefs.go` | create | `GET`/`PUT /prefs` handlers — the document pair only; the tag and watch-folder operations stay with T107. |
| `internal/api/prefs_test.go` | create | `humatest` pins: round trip, unknown member verbatim, 64 KiB cap, non-object rejection. |
| `internal/store/settings.go` | modify | `Prefs` and `PutPrefs` on `SettingsStore`. |
| `internal/api/server.go` | modify | Register the two operations. |
| `web/src/store/useUiPrefs.ts` | modify | `hydrate` from `GET /prefs`; the debounced writer PUTs the whole document; `localStorage` and `PREFS_KEY` removed. |
| `web/src/store/useUiPrefs.test.ts` | modify | Re-pin load, write, merge and gesture rules against the stubbed client instead of `localStorage`. |
| `web/src/lib/theme.ts` | modify | `readStoredTheme`/`storeTheme` read and write the `theme` member through the store, not a private `localStorage` row. |
| `web/src/lib/theme.test.ts` | modify | Re-pin theme persistence on the document transport. |
| `web/src/components/Settings/GeneralSection.tsx` | modify | `writePrefsDocument` goes away; unknown members reach the document through `patch` alone. |
| `web/src/components/Settings/SettingsScreen.test.tsx` | modify | Re-pin the persistence assertions on the PUT body, not `localStorage`. |
| `web/src/components/TaskGrid/TaskGrid.test.tsx` | modify | Seed the prefs state through the store and assert the debounced PUT body. |
| `web/src/components/Shell/Toolbar.tsx` | modify | Read `theme` reactively from the store so the hydrated value lands without a remount. |
| `web/src/components/Shell/Shell.test.tsx` | modify | Stub the prefs verbs so drag-driven `patch()` writes stay handled under the strict MSW suite. |
| `web/src/components/DetailPane/DetailPane.test.tsx` | modify | Stub the prefs verbs so gesture-driven `patch()` writes stay handled under the strict MSW suite. |
| `web/src/App.tsx` | modify | Call `useUiPrefs.getState().hydrate()` when the session reports `authenticated`. |
| `web/src/App.test.tsx` | modify | Stub `GET /prefs`; the suite runs MSW with `onUnhandledRequest: "error"`. |

No other file may be modified. `api/openapi.json` and `web/src/api/schema.d.ts` are implicit per
doc 13 §7.1; `make gen` produces both.

## Interface contract

```go
package api

// PrefsBody is one preference document. The server stores unknown members verbatim and returns
// them unchanged, so the SPA can add a preference without a server change. version is an integer
// the SPA owns; the server never inspects it. There is no PATCH: PUT replaces wholesale.
type PrefsBody map[string]any

type PrefsOutput struct{ Body PrefsBody }
type PutPrefsInput struct{ Body PrefsBody }

// PrefsHandlers serves the ui_prefs document; built in server.go over the shared SettingsStore
// and registered through a Register(hapi huma.API) method, the sibling-handler pattern
// CategoryHandlers already uses.
type PrefsHandlers struct{ Store *store.SettingsStore }

func (h *PrefsHandlers) Get(ctx context.Context, in *struct{}) (*PrefsOutput, error)
func (h *PrefsHandlers) Put(ctx context.Context, in *PutPrefsInput) (*PrefsOutput, error)
```

```go
package store

// Prefs returns the account's ui_prefs rows assembled into one document, or an empty document
// when the account has never stored one — the SPA owns the defaults (doc 09 §3.3).
func (s *SettingsStore) Prefs(ctx context.Context, userID string) (map[string]any, error)

// PutPrefs replaces the document wholesale in one transaction: the account's existing rows are
// deleted and one row per top-level member is inserted, each value_json holding the member's JSON.
func (s *SettingsStore) PutPrefs(ctx context.Context, userID string, doc map[string]any) error
```

Both verbs return `200` with the stored document and take the caller's `userID` from
`IdentityFrom(ctx).User.ID`. Statuses: `401` · `413` `/problems/payload-too-large` above 64 KiB ·
`422` `/problems/validation-failed` when the body's top level is not a JSON object. Huma hands the
handler a decoded `PrefsBody`, so both rejections happen in a middleware the `PUT` operation carries
(`huma.Middlewares`, the mechanism `acceptSubmissionForm` already uses): it bounds the raw body to
64 KiB — `http.MaxBytesReader` is the sibling precedent in `submission.go` — and requires the top
level to decode as a JSON object before re-attaching it for huma.

```ts
// web/src/store/useUiPrefs.ts
export interface UiPrefsState extends UiPrefs {
  /** GET /prefs once the session is authenticated; declared members are shape-checked as
   *  loadInitial does today, unknown members land in state verbatim, and a failed or absent
   *  document leaves the built-in defaults in place (doc 09 §3.3's accepted flash). */
  hydrate: () => Promise<void>;
  patch: (p: Omit<Partial<UiPrefs>, "version">) => void;   // unchanged
  setDragging: (dragging: boolean) => void;               // unchanged
  resetGrid: () => void;                                  // unchanged
}
```

The PUT body is every state member that is not a function and not transient bookkeeping
(`dragging`, hydration or pending-write flags) — so members hydrated from the server and members
written through `patch` under a cast (the General section's extras today, `search` under T064)
round-trip verbatim with no dedicated write path. A failed PUT leaves the in-memory document in
place, matching the tolerance the `localStorage` writer already had. `App.tsx` calls `hydrate()`
when `SessionState` reaches `authenticated`; before it resolves the store renders `defaultPrefs`.
The store tracks which members `patch` or `resetGrid` has touched since the last write completed —
a dirty-member set the debounced PUT clears. `hydrate` merges the server document beneath that set:
every server member lands, including over a member still holding its `defaultPrefs` value, while a
member with a pending local edit keeps the local value, so no write is silently lost.

## Steps
1. Add `Prefs` and `PutPrefs` to `internal/store/settings.go`: `Prefs` selects `key, value_json` for
   the user and unmarshals each row into one `map[string]any`; `PutPrefs` deletes the user's rows
   and inserts one row per top-level member in the same `sqlx.Tx`, each with a fresh `uip_` id.
   Explicit column lists, errors wrapped with `%w`.
2. Create `internal/api/prefs.go`: `Get` resolves `IdentityFrom(ctx)` and returns the document;
   `Put` stores through `PutPrefs` and returns `200` with the stored document. Huma decodes and
   validates the body before `Put` runs, so the `PUT` operation carries a middleware
   (`huma.Middlewares`, the `acceptSubmissionForm` precedent) that bounds the raw body to 64 KiB —
   `413 /problems/payload-too-large` above it — and rejects a body whose top level is not a JSON
   object with `422 /problems/validation-failed`, before re-attaching it for huma to decode.
3. Register both operations in `internal/api/server.go` — `PrefsHandlers` follows the sibling
   `Register(hapi huma.API)` pattern `CategoryHandlers` uses, and the server wiring is the same
   one-line call — then run `make gen`; the regenerated `api/openapi.json` and
   `web/src/api/schema.d.ts` carry only the `/prefs` pair.
4. Migrate `web/src/store/useUiPrefs.ts`: add `hydrate`, point the debounced writer at
   `api.PUT("/prefs", …)` with the CSRF header, and delete `PREFS_KEY`, `readStored` and every
   `localStorage` access. The merge rules in `loadInitial` keep their shape checks, now applied to
   the GET response.
5. Reroute `web/src/lib/theme.ts`: `readStoredTheme` reads `useUiPrefs.getState().theme` and
   `storeTheme` calls `patch({ theme })`. `main.tsx`'s pre-paint call keeps working — it returns
   `system` until hydration lands. The import is one-way: `theme.ts` touches the store lazily
   inside function bodies and `useUiPrefs` never imports `theme.ts`, so the pre-paint path
   initializes cleanly under any module order.
6. Edit `web/src/components/Settings/GeneralSection.tsx`: drop `writePrefsDocument` and the
   `PREFS_KEY` import; unknown members go through `patch` only, since the serializer now emits every
   non-function state member.
7. Edit `web/src/App.tsx` to call `hydrate()` when the session reports `authenticated`, and
   `web/src/components/Shell/Toolbar.tsx` to read `theme` from the store so the hydrated value lands.
8. Re-pin the test files on the new transport: seed prefs state with `useUiPrefs.setState` or a
   stubbed `GET /prefs`, and assert the debounced `PUT /prefs` body where a case asserted the
   `localStorage` write. `App.test.tsx` stubs the hydration `GET`; `Shell.test.tsx` and
   `DetailPane.test.tsx` stub the verbs wherever a driven `patch()` would otherwise hit
   `onUnhandledRequest: "error"`.
9. Create `internal/api/prefs_test.go`: `TestPrefsRoundTripPerUser` PUTs a document and GETs it back
   equal under `go-cmp`; `TestPrefsUnknownMemberPreserved` stores a member the server does not model
   and reads it back verbatim; `TestPrefsTooLarge` posts 64 KiB + 1 and sees `413`;
   `TestPrefsRejectsNonObject` posts a JSON array and sees `422`.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestPrefsRoundTripPerUser` asserts a document PUT and then GET returns identical members.
- [ ] `TestPrefsUnknownMemberPreserved` asserts a member the server does not model survives a
      round trip verbatim — the rule `search` and the General extras rely on.
- [ ] `TestPrefsTooLarge` asserts a body above 64 KiB is `413 /problems/payload-too-large` and
      `TestPrefsRejectsNonObject` asserts a non-object body is `422 /problems/validation-failed`.
- [ ] A `useUiPrefs.test.ts` case asserts `hydrate()` lands a server-only member in state and a
      `patch()` write PUTs the whole document — every non-function, non-transient member, unknown
      ones included.
- [ ] A `useUiPrefs.test.ts` case stubs `GET /prefs` with a known member holding a value that
      differs from `defaultPrefs`, calls `hydrate()` with no `patch()` pending and asserts state
      ends with the server value — the merge applies the whole document, not only unknown keys.
- [ ] A `useUiPrefs.test.ts` case stubs `GET /prefs` to resolve after a delay, patches a member
      before it resolves, advances past the debounce and asserts the PUT body carries the local
      value — a local edit is never clobbered by an in-flight hydrate.
- [ ] A `useUiPrefs.test.ts` case asserts the write still waits out the 500 ms debounce and never
      fires during a drag, now against the PUT rather than `localStorage`.
- [ ] `storeTheme("dark")` reaches the server document: the re-pinned `theme.test.ts` asserts the
      `theme` member in the PUT body, and `Toolbar` shows the hydrated choice without a remount.
- [ ] No `localStorage` reference remains in `web/src/store/useUiPrefs.ts`, `web/src/lib/theme.ts`
      or `web/src/components/Settings/GeneralSection.tsx`; Evidence includes the empty `grep` that
      proves it.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && make test PKG="./internal/api/... ./internal/store/..." && make e2e && echo PREFS_DOC_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/api` and `ok  github.com/L-K-M/dl-tool/internal/store`
with `TestPrefsRoundTripPerUser`, `TestPrefsUnknownMemberPreserved`, `TestPrefsTooLarge` and
`TestPrefsRejectsNonObject` each reported as `--- PASS`; Vitest reports the re-pinned suites passing;
the Playwright suite is green against the binary that now serves `/prefs`; the final line of stdout is
exactly `PREFS_DOC_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, plus the generated `api/openapi.json` and
`web/src/api/schema.d.ts`, and nothing else. Use `git status`, not `git diff`: a file this task
creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add a `PATCH /prefs`; the document is replaced wholesale and that is deliberate
  (doc 05 §11.4).
- Do NOT implement `PATCH`/`DELETE /tags/{name}` or any `/watch-folders` operation; T107 keeps them.
- Do NOT validate or normalise unknown members; store and return them verbatim.
- Do NOT keep a `localStorage` copy as a cache or a fallback; the server document is the only store
  and the brief default→saved flash is the documented trade (doc 09 §3.3).
- Do NOT move the search screen's indexer and category selection off `sessionStorage`, and do NOT add
  the `search` member; T064 owns both.
- Do NOT change the members or defaults the document declares; the shape is doc 09 §3.3's.
- Do NOT write during an active drag or resize gesture; the rule survives the transport switch.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
Created by the T064 blocker repair (the prefs slice of T107 moved forward per the repair options in
T064's `## Blocked`). Not blocked itself.
