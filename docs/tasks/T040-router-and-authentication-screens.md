# T040 — Mount the router, the providers and the authentication screens

| Field | Value |
|---|---|
| **ID** | T040 |
| **Milestone** | M3 |
| **Status** | todo |
| **Depends on** | T009, T013, T014, T039 |
| **Blocks** | T042, T043, T072, T103 |
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
| `web/src/main.test.ts` | edit | Mock boot requests with MSW and assert the mounted app without network access. |
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
7. Edit `web/src/main.tsx` to render `<App />`. Update `web/src/main.test.ts` to mount the real app
   with MSW (`GET /auth/me` → `200`), reject unhandled requests, await the authenticated layout and
   clean up handlers and mounts.
   Preserve its root-rendering coverage; follow [doc 13 §1](../13-testing-and-verification.md#1-test-pyramid).
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
- [ ] `web/src/main.test.ts` verifies the real app mount with mocked boot requests and no network access.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web &&
  (cd web && npx vitest run --reporter=verbose) && echo AUTH_UI_OK
```
Expected: both Vitest runs report `Test Files  4 passed (4)` including `src/App.test.tsx`, the verbose
run shows every test named above passing, and the final line of stdout is exactly `AUTH_UI_OK`.

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
- Do NOT implement the account, password-change or API-token screens; M6 owns them.
- Do NOT read `location.pathname` to guess the base path; use `basePath()` from T014.
- Do NOT add a "remember me" control, a second auth mode or an anonymous mode; ADR-0013 forbids it.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
### Withdrawn implementation

The following output records the partial attempt at `06b29e58a11dc8a2c78bf828841c4709eb325b14`,
not task completion. PR #143 withdraws that implementation in an appended commit for a plan-only
repair; its code and failing tests remain in PR history. Both index rows remain `todo`.

Installed dependencies with
`npm ci --prefix web` without changing pins. The task verification command exited 2:

```text
$ make lint && make typecheck && make test-web && echo AUTH_UI_OK
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
cd web && npx tsc --noEmit -p tsconfig.json
cd web && npx vitest run
```

Failure excerpts from that run:

```text
Error: connect ECONNREFUSED 127.0.0.1:3000
    at TCPConnectWrap.afterConnect [as oncomplete] (node:net:1638:16) {
  errno: -111,
  code: 'ECONNREFUSED',
  syscall: 'connect',
  address: '127.0.0.1',
  port: 3000
}
```

```text
 FAIL  src/App.test.tsx > TestLoginStoresCsrfToken
TestingLibraryElementError: Unable to find role="heading" and name "Search"
```

```text
Expected: "/dl-tool/"
Received: "/dl-tool"
```

```text
 Test Files  1 failed | 3 passed (4)
      Tests  7 failed | 63 passed (70)
   Start at  21:28:13
   Duration  3.01s (transform 762ms, setup 0ms, import 2.34s, tests 3.10s, environment 977ms)

make: *** [Makefile:44: test-web] Error 1
```

The verbose diagnostic run identified all four test files:
`src/main.test.ts`, `src/api/client.test.ts`, `src/lib/theme.test.ts`,
`src/App.test.tsx`. No tests were removed or skipped.

`AUTH_UI_OK` was not printed. `make ci` was not run; no merge requested.

### Plan repair verification

Reproduced the withdrawn head after `npm ci --prefix web`: `make test-web` exited 2 with
`7 failed | 63 passed (70)` across four files and the same connection refusal.
This repair restores all frontend files to the main baseline, including its existing tests;
it does not pass T040 by removing coverage. The partial code and tests remain at the SHA above.

The first repair `make ci` run reached compose-check but failed because `docker` was absent from
`PATH`. Reused the existing Docker CLI (`28.3.3`) and Compose plugin (`v5.5.1`); no repository
pin changed. The complete rerun exited 0:

```text
$ PATH=/tmp/t039-tools:$PATH make ci
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
go vet ./...
cd web && npx tsc --noEmit -p tsconfig.json
go test -race -count=1 ./...
?   	github.com/L-K-M/dl-tool/cmd/dl-tool	[no test files]
ok  	github.com/L-K-M/dl-tool/internal/api	91.614s
ok  	github.com/L-K-M/dl-tool/internal/config	1.119s
ok  	github.com/L-K-M/dl-tool/internal/engine	21.423s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.194s
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	8.943s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.020s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.557s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.177s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.216s
ok  	github.com/L-K-M/dl-tool/internal/store	71.645s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.399s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.064s
?   	github.com/L-K-M/dl-tool/web/node_modules/flatted/golang/pkg/flatted	[no test files]
cd web && npx vitest run

 RUN  v4.1.11 /home/paseo/.paseo/worktrees/0a6udotz/recovery-dltool-165-1789335048/web


 Test Files  3 passed (3)
      Tests  22 passed (22)
   Start at  21:37:38
   Duration  2.00s (transform 377ms, setup 0ms, import 1.35s, tests 797ms, environment 764ms)

docker compose -f compose.yaml config -q
docker compose -f compose.yaml -f compose.dev.yaml config -q
./scripts/doclint.sh
🔍 2422 Total (in 241ms) 🔗 572 Unique ✅ 2396 OK 🚫 0 Errors 👻 26 Excluded
```

The verbose command above also exited 0 on the baseline: three files, 22 tests. Four files are
expected only when T040 is implemented. A plan check confirmed the authorized entrypoint-test path,
the expected count (three existing files plus `App.test.tsx`), both `todo` rows, and a docs-only diff:

```text
T040_PLAN_REPAIR_OK: entrypoint scope, four-file expectation, both todo rows, docs-only diff
```

The review fix puts verbose output inside the same command chain, before the success sentinel.
Executed that exact chain on the unchanged baseline; exit 0, final stdout excerpt:

```text
 Test Files  3 passed (3)
      Tests  22 passed (22)
   Start at  21:48:15
   Duration  2.05s (transform 423ms, setup 0ms, import 1.44s, tests 868ms, environment 686ms)

AUTH_UI_OK
```

This validates command ordering, not T040 acceptance or its future four-file suite.

### Retry notes

The withdrawn attempt's `TestLoginStoresCsrfToken` exposed a login redirect race:
the session update let the authenticated login gate override `next` with `/`. Six further failures
asserted `/dl-tool/`, while router navigation produced `/dl-tool`. Preserve regression coverage when
resuming; validate navigation against [doc 09 §2.1](../09-web-ui-spec.md#21-routes) and the required
`basename={basePath()}`. The repair does not claim these implementation failures are fixed.
Run all task verification, record fresh Evidence and complete review before marking T040 done.

## Blocked
None. This plan-only repair resolves the scope and count blockers; T040 remains unimplemented.
