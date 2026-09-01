# T013 — Embed the built SPA and serve it under the base path

| Field | Value |
|---|---|
| **ID** | T013 |
| **Milestone** | M0 |
| **Status** | todo |
| **Depends on** | T003, T007 |
| **Blocks** | T014, T040, T095, T124 |
| **Parallel-safe** | no — it edits `internal/api/server.go` |
| **Implements** | — (the mechanism behind [NFR-006](../02-requirements.md#nfr-006-work-when-hosted-under-a-sub-path), verified end to end by T095) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md), [ADR-0012](../decisions/0012-single-data-mount.md) |
| **Est. size** | 1 new source file, 1 test file, 1 placeholder asset, 2 edits, ~200 LOC |

## Goal
The Go binary serves the SPA from `//go:embed`ed bytes, rewrites `index.html` at serve time to carry
`<base href="{base}/">` and `window.__DLTOOL_BASE__`, and falls back to `index.html` for any unknown path
**inside** the base. A path outside the base is 404, so a misconfigured proxy fails loudly.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/10-deployment-and-compose.md` §7.3 Base-path requirements](../10-deployment-and-compose.md#73-base-path-requirements)
   — the eight rules; this task implements 3, 4, 8 and the serve half of 2.
2. [`docs/10-deployment-and-compose.md` §5 `Dockerfile`](../10-deployment-and-compose.md#5-dockerfile) — the
   build stage that copies `/web/dist` to `./internal/api/dist`, which is why the embed path is that one.
3. [`docs/05-api-contract.md` §1.1 Base path and media types](../05-api-contract.md#11-base-path-and-media-types).

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/static.go` | create | The embedded filesystem, the SPA handler and the `index.html` rewrite. |
| `internal/api/dist/index.html` | create | Placeholder so `//go:embed` resolves before a web build. |
| `internal/api/static_test.go` | create | Rewrite, fallback, 404-outside-base and cache-header tests. |
| `internal/api/server.go` | edit | Mount the SPA handler last, under the base sub-router. |
| `.gitignore` | edit | Ignore built assets under `internal/api/dist/`, keep the placeholder. |

No other file may be modified.

## Interface contract

```go
package api

//go:embed all:dist
var distFS embed.FS

// SPAHandler serves the embedded SPA. basePath is "" for the web root, or a path
// with a leading and no trailing slash. Unknown paths inside the base return
// index.html; the caller mounts this last so API routes win.
func SPAHandler(basePath string) (http.Handler, error)
```

The rewrite applied to `index.html` on every request, per doc 10 §7.3 rule 4:

```html
<base href="{base}/">
<script>window.__DLTOOL_BASE__ = "{base}";</script>
```

`{base}` is `cfg.BasePath`, or the empty string at the web root, in which case `<base href="/">` is emitted.
The rewrite happens at serve time. The base path is never baked in at build time; Vite already builds with
`base: './'` (T003).

Caching, so a redeploy is picked up without a hard refresh:

| Path | `Cache-Control` |
|---|---|
| `index.html` | `no-cache` |
| `assets/*` (content-hashed by Vite) | `public, max-age=31536000, immutable` |
| everything else | `no-cache` |

`internal/api/dist/index.html` placeholder:

```html
<!doctype html><html lang="en"><head><meta charset="utf-8"><title>dl-tool</title></head>
<body><div id="root"></div></body></html>
```

## Steps
1. Create `internal/api/dist/index.html` with the placeholder above. The Docker build stage overwrites the
   whole directory with the real Vite output, so this file only keeps `go build` working locally.
2. Edit `.gitignore` to add `internal/api/dist/*` followed by `!internal/api/dist/index.html`, so a local
   `npm run build` copied into place is never committed.
3. Create `internal/api/static.go` with `//go:embed all:dist` and `SPAHandler`. Use `fs.Sub(distFS, "dist")`
   so URLs do not carry the `dist/` segment.
4. Read `index.html` once at construction, apply the rewrite for the given `basePath`, and keep the result in
   memory; do not re-read the embedded bytes per request.
5. Serve a request whose path maps to an existing embedded file directly, with the `Cache-Control` value from
   the table and a `Content-Type` from `mime.TypeByExtension`.
6. For a path with no matching file, return the rewritten `index.html` with status 200 — the SPA router
   resolves it. Do this only for `GET` and `HEAD`; any other method is 405.
7. Edit `internal/api/server.go` to mount the handler on the base sub-router with `base.Handle("/*", …)`,
   after `/api/v1`, `/healthz` and `/readyz`, so no API route is shadowed. Register nothing outside the base:
   a request to `/anything` while `BasePath=/dl-tool` must be 404.
8. Write `internal/api/static_test.go` covering: `GET /` returns 200 with `<base href="/">`; with
   `BasePath=/dl-tool`, `GET /dl-tool/` carries `<base href="/dl-tool/">` and
   `window.__DLTOOL_BASE__ = "/dl-tool"`; `GET /dl-tool/tasks/anything` returns `index.html`;
   `GET /tasks` returns 404; `GET /dl-tool/api/v1/system/info` still reaches the API; an `assets/` file
   carries the immutable `Cache-Control` and `index.html` carries `no-cache`; and `POST /dl-tool/` is 405.
9. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestBaseHrefInjected` passes for both an empty base and `/dl-tool`.
- [ ] `TestSPAFallbackInsideBase` returns `index.html` for an unknown path under the base.
- [ ] `TestOutsideBaseIs404` asserts 404, not a redirect and not the SPA.
- [ ] `TestAPIRouteNotShadowed` asserts `/api/v1/system/info` still returns JSON.
- [ ] `TestCacheHeaders` asserts both rows of the caching table.
- [ ] `make build` succeeds on a clean checkout with no `web/dist` present.
- [ ] `index.html` contains no absolute `/assets/…` URL.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make build && make test PKG=./internal/api/... && echo SPA_OK
```
Expected: `bin/dl-tool` is written, `ok  	github.com/L-K-M/dl-tool/internal/api` appears with no `FAIL`, and
the final line of stdout is exactly `SPA_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT build any screen, route or component in `web/src`; T039 through T053 own the interface.
- Do NOT add the web app manifest, icons or a service worker; T103 owns the progressive web app.
- Do NOT add security headers or the `Host` allowlist; T095 owns both.
- Do NOT bake `DLTOOL_BASE_PATH` into the Vite build or set Vite's `base` to anything but `'./'`.
- Do NOT commit a real `dist/` build output; only the placeholder `index.html` is committed.
- Do NOT load a font, a script or a stylesheet from a third-party origin; T039 verifies that rule.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
