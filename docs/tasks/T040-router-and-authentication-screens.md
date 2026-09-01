# T040 — Mount the router, the providers and the authentication screens

| Field | Value |
|---|---|
| **ID** | T040 |
| **Milestone** | M3 |
| **Status** | todo |
| **Depends on** | T009, T013, T014, T039 |
| **Blocks** | T042, T043, T044, T048, T049, T051, T052, T053, T103 |
| **Parallel-safe** | no — it also edits the shared files `web/src/locales/en/common.json`, `web/src/main.tsx` |
| **Implements** | — (renders [FR-115](../02-requirements.md#fr-115-complete-a-first-run-setup-using-a-one-time-token) and [FR-116](../02-requirements.md#fr-116-authenticate-with-a-session-cookie-or-a-bearer-token), both covered by T009; carries [NFR-012](../02-requirements.md#nfr-012-protect-against-csrf-with-a-synchroniser-token)'s token client-side) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md), [ADR-0013](../decisions/0013-mandatory-built-in-authentication.md) |
| **Est. size** | 4 new files, ~340 LOC. `App.tsx` cannot compile against routes whose screens do not exist, so the router and the two screens land together. |

## Goal
Opening the SPA calls `GET /auth/me` once and lands on the setup wizard, the login form or the app shell.
A successful setup or login stores the CSRF token in memory, and every authenticated route renders inside
one layout with the three regions of doc 09 §2.2.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §4.1 `POST /auth/setup`](../05-api-contract.md#41-post-authsetup) and
   [§4.2](../05-api-contract.md#42-post-authlogin-post-authlogout-get-authme) — bodies, the
   `{"user":…,"csrf_token":…}` envelope and every status code.
2. [`docs/09-web-ui-spec.md` §2.1 Routes](../09-web-ui-spec.md#21-routes) — the complete route table.
3. [`docs/09-web-ui-spec.md` §2.3 Region sizing](../09-web-ui-spec.md#23-region-sizing) — the three regions.
4. [`docs/14-conventions.md` §5 Frontend conventions](../14-conventions.md#5-frontend-conventions).
5. [`docs/tasks/T014-typed-api-client.md`](T014-typed-api-client.md) — `api`, `basePath()`, `setCsrfToken()`.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/App.tsx` | create | Providers, the route table, the auth gate and the layout regions. |
| `web/src/components/Auth/LoginScreen.tsx` | create | The `/login` form. |
| `web/src/components/Auth/SetupScreen.tsx` | create | The `/setup` first-run wizard. |
| `web/src/App.test.tsx` | create | Boot routing, CSRF capture and redirect validation. |
| `web/src/main.tsx` | edit | Render `<App />` instead of T003's placeholder. |
| `web/src/locales/en/common.json` | edit | Auth strings. |

No other file may be modified.

## Interface contract

```tsx
// web/src/App.tsx
export type SessionState =
  | { status: 'loading' }
  | { status: 'setup-required' }
  | { status: 'anonymous' }
  | { status: 'authenticated'; user: User; csrfToken: string };

/** Calls GET /auth/me once on mount. 401 with type "/problems/setup-required" ⇒ 'setup-required',
 *  any other 401 ⇒ 'anonymous', 200 ⇒ 'authenticated' and setCsrfToken(csrf_token). */
export function useSession(): SessionState;

/** Renders children only when authenticated; otherwise <Navigate> to /setup or /login. */
export function RequireAuth(props: { children: React.ReactNode }): JSX.Element;

/** A ?next= value is used only when it starts with a single "/" and not with "//" (NFR-024). */
export function safeNext(raw: string | null): string;

export default function App(): JSX.Element;
```

Routes, exactly doc 09 §2.1, all inside a `BrowserRouter` whose `basename` is `basePath()`:

```tsx
<Route path="/setup" element={<SetupScreen />} />
<Route path="/login" element={<LoginScreen />} />
<Route element={<RequireAuth><AppLayout /></RequireAuth>}>
  <Route path="/" element={<TasksRoute filter="all" />} />
  <Route path="/tasks/:filter" element={<TasksRoute />} />
  <Route path="/tasks/category/:name" element={<TasksRoute />} />
  <Route path="/tasks/tag/:name" element={<TasksRoute />} />
  <Route path="/search" element={<Placeholder screen="search" />} />
  <Route path="/rss/feeds" element={<Placeholder screen="rss-feeds" />} />
  <Route path="/rss/rules" element={<Placeholder screen="rss-rules" />} />
  <Route path="/settings/:section" element={<Placeholder screen="settings" />} />
  <Route path="/logs" element={<Placeholder screen="logs" />} />
</Route>
```

`AppLayout` renders three regions and one `<Outlet />`: a 48 px header slot, a 220 px sidebar slot and a
28 px status-bar slot, each an empty landmark until T042 and T044 fill them.

```tsx
// web/src/components/Auth/SetupScreen.tsx
export function SetupScreen(): JSX.Element;   // fields: setup token, username, password (min 12), locale
// web/src/components/Auth/LoginScreen.tsx
export function LoginScreen(): JSX.Element;   // fields: username, password
```

Providers, outermost first: `QueryClientProvider` (`@tanstack/react-query`) → `I18nextProvider`
(`initI18n()` from T039) → `BrowserRouter`.

## Steps
1. Create `web/src/App.tsx` with the provider tree, `useSession`, `RequireAuth`, `safeNext` and the route
   table above. Every navigation decision comes from `GET /auth/me`; never from a cookie read.
2. Store the `csrf_token` from setup, login and `/auth/me` with `setCsrfToken` (T014). Never write it to
   `localStorage`, a cookie or the URL.
3. Create `SetupScreen.tsx`: token, username, password, confirm password, locale. Disable submit below 12
   password characters and show the requirement before submission, not after. On `201` navigate to `/`.
   On `409 /problems/setup-already-complete` navigate to `/login` with an explanatory toast.
4. Create `LoginScreen.tsx`: username, password, submit. On `401` show the server `detail` verbatim, which
   is identical for a wrong password and an unknown user. On `429` show the `Retry-After` seconds.
5. After login navigate to `safeNext(searchParams.get('next'))`, defaulting to `/`.
6. Edit `web/src/locales/en/common.json` with the auth strings; every visible string goes through `t()`.
7. Edit `web/src/main.tsx` to render `<App />`.
8. Create `web/src/App.test.tsx` with `msw` handlers: `/auth/me` → `401 /problems/setup-required` renders
   the wizard; `401 /problems/unauthenticated` renders the login form; `200` renders the layout;
   a successful login calls `setCsrfToken`; `safeNext('//evil.example')` and `safeNext('https://x')` both
   return `/`.
9. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestBootRoutesToSetupWizard`, `TestBootRoutesToLogin` and `TestBootRendersLayout` pass.
- [ ] `TestLoginStoresCsrfToken` passes and no test finds the token in `localStorage`.
- [ ] `TestSafeNextRejectsAbsoluteAndProtocolRelative` passes for `//evil.example`, `https://x` and `\\x`.
- [ ] Every route in doc 09 §2.1 resolves; an unknown path inside the base renders the layout, not a blank.
- [ ] `BrowserRouter` receives `basename={basePath()}`, so the SPA works under `DLTOOL_BASE_PATH`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo AUTH_UI_OK
```
Expected: Vitest reports `Test Files  3 passed (3)` including `src/App.test.tsx`, every test named above
appears as passing, and the final line of stdout is exactly `AUTH_UI_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT build the sidebar, toolbar or status bar; T042 and T044 own them, and this task leaves the three
  region slots empty.
- Do NOT open an `EventSource` or fetch `/tasks`; T041 and T051 own live data.
- Do NOT implement `/users`, password change or API-token screens; M6 owns them.
- Do NOT read `location.pathname` to guess the base path; use `basePath()` from T014.
- Do NOT add a "remember me" control, a second auth mode or an anonymous mode; ADR-0013 forbids it.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
