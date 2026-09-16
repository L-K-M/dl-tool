# T103 — Ship the web app manifest, maskable icons and the service worker

| Field | Value |
|---|---|
| **ID** | T103 |
| **Milestone** | M3 |
| **Status** | done |
| **Depends on** | T039, T040, T043 |
| **Blocks** | — |
| **Parallel-safe** | no — it also edits the shared files `web/index.html`, `web/src/main.tsx` |
| **Implements** | [NFR-029](../02-requirements.md#nfr-029-ship-an-installable-progressive-web-app) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md) |
| **Est. size** | 5 new files, ~150 LOC. Two of the five are binary icon assets, not code. |

## Goal
dl-tool is installable: a manifest with maskable icons, `display: standalone` and a `theme-color` matching
the dark background, plus a service worker that caches the content-hashed assets and nothing else. Nothing
works offline, and the install copy says so.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/09-web-ui-spec.md` §10.3 Responsive and mobile](../09-web-ui-spec.md#103-responsive-and-mobile) —
   the final paragraph: manifest, maskable icons, `standalone`, `theme-color`, and the "nothing works
   offline" rule.
2. [`docs/13-testing-and-verification.md` §6.2 Accessibility and PWA gates](../13-testing-and-verification.md#62-accessibility-and-pwa-gates)
   — the five facts the installability check asserts.
3. [`docs/10-deployment-and-compose.md` §7.3 Base-path requirements](../10-deployment-and-compose.md#73-base-path-requirements)
   — every URL is relative, because the app may be served under a sub-path.
4. [`docs/tasks/T043-playwright-harness-and-grid-performance.md`](T043-playwright-harness-and-grid-performance.md)
   — the harness this spec runs in.
5. [`docs/tasks/T039-ui-stack-and-design-tokens.md`](T039-ui-stack-and-design-tokens.md) — the dark `--bg`
   value the `theme-color` must equal.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/public/manifest.webmanifest` | create | The web app manifest. |
| `web/public/icons/icon-192.png` | create | 192×192 icon, `purpose: "any maskable"`. |
| `web/public/icons/icon-512.png` | create | 512×512 icon, `purpose: "any maskable"`. |
| `web/public/sw.js` | create | Static-asset cache; no offline behaviour. |
| `web/e2e/pwa.spec.ts` | create | The installability assertions of doc 13 §6.2. |
| `web/index.html` | edit | The manifest link and the `theme-color` meta. |
| `web/src/main.tsx` | edit | Register the service worker against `document.baseURI`. |

No other file may be modified.

## Interface contract

```json
{
  "name": "dl-tool",
  "short_name": "dl-tool",
  "description": "Self-hosted download manager. Requires a connection; nothing works offline.",
  "start_url": "./",
  "scope": "./",
  "display": "standalone",
  "background_color": "#0f1115",
  "theme_color": "#0f1115",
  "icons": [
    { "src": "icons/icon-192.png", "sizes": "192x192", "type": "image/png", "purpose": "any maskable" },
    { "src": "icons/icon-512.png", "sizes": "512x512", "type": "image/png", "purpose": "any maskable" }
  ]
}
```

`web/index.html` additions, both relative so a sub-path deployment resolves them against `<base>`:

```html
<link rel="manifest" href="manifest.webmanifest">
<meta name="theme-color" content="#0f1115">
```

```ts
// web/src/main.tsx
if ('serviceWorker' in navigator) {
  window.addEventListener('load', () => {
    void navigator.serviceWorker.register(new URL('sw.js', document.baseURI), {
      scope: new URL('./', document.baseURI).pathname,
    });
  });
}
```

```js
// web/public/sw.js — the only two jobs are the install criterion and caching static assets.
const CACHE = 'dl-tool-assets-v1';
self.addEventListener('install', (e) => { self.skipWaiting(); });
self.addEventListener('activate', (e) => {
  e.waitUntil(caches.keys().then((k) => Promise.all(k.filter((n) => n !== CACHE).map(caches.delete, caches)))
    .then(() => self.clients.claim()));
});
self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  const isAsset = e.request.method === 'GET' && url.origin === self.location.origin
    && url.pathname.includes('/assets/');
  if (!isAsset) return;                       // API, SSE and index.html always go to the network
  e.respondWith(caches.open(CACHE).then(async (c) => {
    const hit = await c.match(e.request);
    if (hit) return hit;
    const res = await fetch(e.request);
    if (res.ok) c.put(e.request, res.clone());
    return res;
  }));
});
```

## Steps
1. Create the two PNG icons on the dark background `#0f1115`, each with the glyph inside the maskable safe
   zone, that is the central 80 % circle, so no platform mask clips it.
2. Create `web/public/manifest.webmanifest` exactly as above. Every `src` is relative; never write a leading
   slash.
3. Edit `web/index.html` with the manifest link and the `theme-color` meta, both relative.
4. Create `web/public/sw.js` exactly as above. It must never intercept `/api/`, `/events` or `index.html`,
   and it must never serve an offline fallback page.
5. Edit `web/src/main.tsx` with the registration above, guarded by the `serviceWorker` feature check.
6. Create `web/e2e/pwa.spec.ts` asserting the five facts of doc 13 §6.2: the manifest is fetchable and
   parses; `display` is `standalone`; `theme_color` equals the dark `--bg`; both icons return `200` with
   `content-type: image/png` and carry `purpose` containing `maskable`; and
   `navigator.serviceWorker.getRegistration()` resolves to a registration after load.
7. Assert in the same spec that a request to `/api/v1/tasks` is not served from the cache while the service
   worker is active.
8. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [x] `manifest is installable`, `icons are maskable` and `service worker registers` pass in Chromium.
- [x] `api requests bypass the cache` passes.
- [x] Every URL in the manifest and in `index.html` is relative, so the app installs under a sub-path.
- [x] No offline page, no cached API response, and no background sync exists.
- [x] The install description states that a connection is required.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make e2e && echo PWA_OK
```
Expected: the list reporter shows the four `pwa.spec.ts` tests passing alongside T043's specs, and the
final line of stdout is exactly `PWA_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add `vite-plugin-pwa`, `workbox`, `lighthouse` or `@lhci/cli`; doc 09 §1 pins none of them, and
  the assertions above are the installability check.
- Do NOT cache an API response, an SSE stream or `index.html`.
- Do NOT add an offline page, a background sync or a push subscription.
- Do NOT add a custom install prompt beyond the browser's own.
- Do NOT change the theme tokens; T039 owns them and the `theme-color` copies the dark `--bg`.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

Verified 2026-09-16 on branch `task/T103-progressive-web-app` at `905af4b`
(the post-review head; the review round anchored the asset predicate to the
worker scope, made `cache.put` awaited, caught registration rejection and
added the manifest `id` member).
`npx playwright install --with-deps chromium` needs root and fails in this
environment, so `make e2e` ran with the cached Chromium per the environment
note T043 recorded: `LD_LIBRARY_PATH` covering
`$HOME/.local/share/goodebics-validation/browser/{lib,usr/lib}/x86_64-linux-gnu`
and `$HOME/opt/mesa-root/usr/lib/x86_64-linux-gnu`, and `PATH` including
`$HOME/.local/go/bin` and `/tmp/dltool-recovery-tools/docker`.

```text
$ make e2e && echo PWA_OK
cd web && npx playwright test
[WebServer] (!) Some chunks are larger than 500 kB after minification. Consider:
[WebServer] - Using dynamic import() to code-split the application
[WebServer] - Use build.rolldownOptions.output.codeSplitting to improve chunking: https://rolldown.rs/reference/OutputOptions.codeSplitting
[WebServer] - Adjust chunk size limit for this warning via build.chunkSizeWarningLimit.

Running 7 tests using 2 workers

  ✓  2 [chromium] › e2e/pwa.spec.ts:30:1 › manifest is installable (366ms)
  ✓  1 [chromium] › e2e/setup.spec.ts:26:1 › first run creates the admin (736ms)
  ✓  4 [chromium] › e2e/setup.spec.ts:41:1 › a second setup attempt is rejected (17ms)
  ✓  3 [chromium] › e2e/pwa.spec.ts:60:1 › icons are maskable (377ms)
  ✓  6 [chromium] › e2e/pwa.spec.ts:96:1 › service worker registers (278ms)
  ✓  7 [chromium] › e2e/pwa.spec.ts:114:1 › api requests bypass the cache (428ms)
grid perf: 30 changed rows per tick, 10 measured ticks
grid perf deltas (ms): 5.316, 6.035, 4.498, 6.142, 6.788, 6.497, 6.810, 6.514, 6.004, 6.079
grid perf p95: 6.810 ms (budget 8 ms)
  ✓  5 [chromium] › e2e/setup.spec.ts:53:1 › the grid stays inside the scripting budget (18.2s)

  7 passed (33.7s)
PWA_OK
```

`pwa.spec.ts` never creates the admin account (the spec files run
concurrently on separate workers, so creating one could race
`setup.spec.ts`'s first-run redirect assertion); all four assertions run
signed-out against the anonymously served SPA. The API-bypass test waits for
`navigator.serviceWorker.controller` so the fetch really passes through the
worker's handler, and also proves an `/api/v1/assets/…` path is not
intercepted.

Scope check. The task's Files-table files are committed, so `git status` is
clean apart from `internal/api/dist/index.html` — a side effect of the
`e2e:server` build copy, restored with `git checkout` before this run's diff
was taken:

```text
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
$ git diff --name-only origin/main...HEAD | sort
web/e2e/pwa.spec.ts
web/index.html
web/public/icons/icon-192.png
web/public/icons/icon-512.png
web/public/manifest.webmanifest
web/public/sw.js
web/src/main.tsx
```

Both PNGs were verified against their IHDR headers: `icon-192.png` is
192×192 and `icon-512.png` is 512×512. The glyph sits inside the maskable
safe zone: the accent disc radius is 0.38·S < 0.40·S, so no platform mask
clips it.

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
