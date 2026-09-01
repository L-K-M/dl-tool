# 0003 - chi + Huma with code-first OpenAPI

> **Status:** accepted
> **Date:** 2026-09-01
> **Deciders:** repository owner

## Context and Problem Statement

dl-tool exposes roughly forty endpoints under `/api/v1`, and its own SPA is the first consumer of them:
`web/src/api/schema.d.ts` is generated from `api/openapi.json` by `openapi-typescript`, and every call goes
through `openapi-fetch`. If the OpenAPI document and the Go handlers can disagree, the SPA compiles against
a contract the server does not implement and the failure surfaces as a runtime 404 or a silently missing
field. The question is which artefact is authoritative — the spec or the handler — and what enforces it.

## Decision Drivers

- The frontend types are a build output of the spec, so the spec must never be stale by construction, not
  by discipline.
- Errors must have one machine-readable shape across every endpoint; the UI renders them generically.
- Auth, CSRF, the embedded SPA and `/healthz` sit **outside** the described API and need a real router with
  a middleware chain and sub-routers.
- The implementing agent is a weaker model. Keeping two hand-written artefacts in sync is exactly the task
  it fails at silently.

## Considered Options

- **Option A** — chi v5 + Huma v2, code-first: OpenAPI 3.1 generated from the handler input/output structs.
- **Option B** — Spec-first: a hand-written `api/openapi.yaml` plus `oapi-codegen/v2` generated server stubs.
- **Option C** — Bare chi + `net/http` with a hand-written, hand-maintained `api/openapi.json`.
- **Option D** — GraphQL (`gqlgen`) or tRPC instead of REST.

## Decision Outcome

Chosen option: **Option A, chi v5 + Huma v2**, because the spec is a build output — it is impossible for
the documented contract to disagree with the handler, because it *is* the handler. Huma is a declarative
layer over chi (`api := humachi.New(router, huma.DefaultConfig("dl-tool", "1.0.0"))`), so chi's `r.Group`,
`r.Route` and middleware chain stay available for everything the OpenAPI document does not describe.

What that buys, all of it load-bearing here: OpenAPI **3.1** plus JSON Schema at `/openapi.json` and
`/openapi.yaml`; input validation from struct tags (`maxLength`, `minimum`, `enum`, `required`); RFC 9457
`application/problem+json` errors by default; per-operation request size limits; `humatest` for handler
tests; and `huma/v2/sse`, which registers the event stream of [ADR-0006](0006-sse-with-rid-deltas.md) *in
the spec* with a discriminated event→struct map.

### Consequences

- Good, because contract drift is caught by a `git diff --exit-code` in CI rather than by a user.
- Good, because validation is written once as struct tags, and the error model is decided for us: one
  RFC 9457 shape, documented in [`../05-api-contract.md`](../05-api-contract.md).
- Bad, because anything Huma's schema generation cannot express — chiefly the partially populated task
  objects inside the sync delta — must be documented by hand in `05` and kept true by a golden-file test.
- Bad, because there are two registration paths: Huma operations for the API, plain chi handlers for the
  embedded SPA, `/healthz`, `/readyz` and `/metrics`. A weaker model must not register API endpoints on
  chi directly; [`../14-conventions.md`](../14-conventions.md) states the rule.
- Neutral, because Huma sits on chi rather than replacing it, so abandoning Huma later leaves working chi
  handlers rather than a rewrite.

### Confirmation

The drift gate is the decision. `make gen` regenerates the spec from the running handlers and the
TypeScript types from the spec; CI runs the same target and fails on any diff:

```bash
make gen && git diff --exit-code -- api/openapi.json web/src/api/schema.d.ts
```

Expected: exit 0 and no output. The job is `openapi-drift` in `.github/workflows/ci.yml` — see
[`../13-testing-and-verification.md`](../13-testing-and-verification.md). Handler-level behaviour is
covered by `humatest` tests under `internal/api/`.

## Pros and Cons of the Options

### Option A - chi v5 + Huma v2, code-first

- Good, because the spec cannot drift, and the generated client types make a wrong endpoint path a
  TypeScript compile error in the SPA.
- Good, because chi's middleware (`Recoverer`, `RequestID`, `Compress`) is what the non-API routes need.
- Bad, because Huma owns response writing for its operations, so streaming and static-file routes live
  outside it and the split has to be remembered.

### Option B - spec-first with oapi-codegen

- Good, because the contract can be reviewed and agreed before any Go exists, and non-Go consumers can
  generate clients from the same file.
- Bad, because the model must keep a hand-written YAML and hand-written handlers in sync, which is the
  failure this ADR exists to prevent.

### Option C - bare chi with a hand-written spec

- Good, because it has the fewest dependencies and the most direct control over every response.
- Bad, because validation, error shaping and documentation are then written once per endpoint, forty times,
  by a model that will not be consistent across all forty.

### Option D - GraphQL or tRPC

- Good, because a single flexible query endpoint suits a dense grid that wants different field sets per view.
- Bad, because tRPC's contract is a TypeScript type, which cannot be handed to a `curl` user and requires
  TypeScript on both sides — incompatible with [ADR-0002](0002-go-for-the-backend.md).
- Bad, because a resolver graph plus `gqlgen`'s codegen is more moving parts than forty endpoints justify.

## More Information

- Research: `architecture.md` §1.4 and §6, and its fact-check — summarised in
  [`../16-prior-art-and-research.md`](../16-prior-art-and-research.md).
- Pinned versions: [`../03-architecture.md`](../03-architecture.md).
- Depends on this decision: [`../05-api-contract.md`](../05-api-contract.md),
  [`../09-web-ui-spec.md`](../09-web-ui-spec.md), [`../14-conventions.md`](../14-conventions.md).
