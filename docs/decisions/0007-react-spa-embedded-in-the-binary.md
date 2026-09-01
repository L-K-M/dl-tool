# 0007 - React SPA embedded in the Go binary

> **Status:** accepted
> **Date:** 2026-09-01
> **Deciders:** repository owner

## Context and Problem Statement

The repository owner's brief is *"a tool that basically does the same thing from the user's point of view
and that I can host wherever I want using docker compose"*. Both halves of that land on this decision: the
interface is the product claim, and the deployment must stay one compose file. So there are two questions —
which UI stack renders a Download-Station-grade screen, and how that screen is served.

The hard part is not the styling: it is a virtualised, column-resizable, column-reorderable table of up to
several thousand rows, multi-selectable, updating at 1 Hz from [ADR-0006](0006-sse-with-rid-deltas.md).

## Decision Drivers

- One container and one artefact. A second web server means a second image, a second port and CORS.
- The client's API types are generated from `api/openapi.json`
  ([ADR-0003](0003-chi-huma-code-first-openapi.md)), so the UI stack must be TypeScript, and the grid and
  virtualisation libraries the screen needs target React first.
- Deployment behind a reverse proxy at a sub-path must work without rebuilding the image, and the
  implementing agent is a weaker model, so corpus size is a real selection criterion.

## Considered Options

- **Option A** — React 19 + Vite + TypeScript SPA, `vite build` to `web/dist`, `//go:embed` into the binary,
  served by the Go process with an SPA fallback to `index.html`.
- **Option B** — Go `html/template` server rendering plus HTMX, no build step, no Node.
- **Option C** — SvelteKit or Vue SPA, embedded the same way.
- **Option D** — React SPA served by a separate nginx container.
- **Option E** — Server-side rendering with Next.js or Remix.

## Decision Outcome

Chosen option: **Option A**, because embedding `dist/` is what makes dl-tool one container, and because
React is the only candidate whose grid ecosystem (`@tanstack/react-table` v8, `@tanstack/react-virtual`)
already solves the screen's hard part. TypeScript is now GitHub's most-contributed language, so it is also
the largest corpus available to the implementing agent.

Two pins are part of the decision because "latest" is wrong here: `@tanstack/react-table@8.21.3`, **not**
v9, which went stable on 2026-08-04 and is outside the corpus; and `typescript@5.9.3`, not the 6.x or
7.x lines. The full list lives in [`../03-architecture.md`](../03-architecture.md).

### Consequences

- Good, because `docker compose up -d` yields one image serving the API and the UI on one port, with no
  CORS configuration.
- Good, because `//go:embed dist` fails to compile on an empty match, so no binary ships without its UI.
- Good, because a wrong endpoint path or field name in the SPA is a compile error, not a runtime 404.
- Bad, because any UI change requires a Go rebuild. The development loop therefore runs Vite's dev server
  separately and proxies to the Go process; `compose.dev.yaml` wires that.
- Bad, because `vite build` bakes its `base` into the bundle, so sub-path deployment (`DLTOOL_BASE_PATH`)
  has to be rewritten at serve time rather than at build time. The mechanism is specified in
  [`../10-deployment-and-compose.md`](../10-deployment-and-compose.md).
- Neutral, because the PWA rides on the same bundle — manifest, maskable icons, `standalone` display and a
  service worker whose only jobs are the install criterion and static-asset caching. Nothing works offline;
  [`../09-web-ui-spec.md`](../09-web-ui-spec.md) says so.

### Confirmation

```bash
make build && make test PKG=./internal/api/...
```

Expected: exit 0. `make build` runs the web build then `go build`, which fails outright if
`web/dist/index.html` is missing because `//go:embed` errors on an empty match. `TestStaticSPAFallback` in
`internal/api/static_test.go` asserts that an unknown UI route returns the embedded `index.html` with 200,
a hashed asset path returns the embedded asset, and an unknown `/api/v1/...` path returns
`application/problem+json` 404 rather than HTML.

## Pros and Cons of the Options

### Option A - React SPA embedded in the binary

- Good, because one binary is one artefact to build, sign, scan and release, and the API and the UI cannot
  drift in version — they are the same file.
- Bad, because the binary grows by the size of the bundle, and every UI-only fix is a full release.

### Option B - Go templates plus HTMX

- Good, because it removes Node from the build entirely and makes the whole project one language, which is
  genuinely the most maintainable option here.
- Bad, because a virtualised, resizable, reorderable multi-thousand-row grid with 1 Hz live updates is
  precisely the case HTMX is worst at; the JavaScript ends up hand-written and unstructured.

### Option C - SvelteKit or Vue

- Good, because both produce smaller bundles than React and Svelte's reactivity suits a 1 Hz stream well.
- Bad, because Svelte 5 runes are a 2024-and-later idiom that models still mix with Svelte 4 stores — which
  does not compile — and both ecosystems are thinner for dense data grids.

### Option D - separate nginx container serving the SPA

- Good, because the UI can be rebuilt and redeployed without touching the backend image.
- Bad, because it adds a container, a port and CORS to a product whose deployment story is the point.

### Option E - SSR with Next.js or Remix

- Good, because first paint is faster and the app is indexable.
- Bad, because a single-user authenticated download manager gains nothing from either, and it puts a Node
  runtime inside the runtime image, which [ADR-0011](0011-alpine-runtime-with-puid-pgid.md) keeps out.

## More Information

- Research: `architecture.md` §5 and §7, and `ui-ux.md` — summarised in
  [`../16-prior-art-and-research.md`](../16-prior-art-and-research.md).
- Depends on this decision: [`../09-web-ui-spec.md`](../09-web-ui-spec.md),
  [`../10-deployment-and-compose.md`](../10-deployment-and-compose.md).
