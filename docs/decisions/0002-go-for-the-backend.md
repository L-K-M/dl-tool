# 0002 - Go for the backend

> **Status:** accepted
> **Date:** 2026-09-01
> **Deciders:** repository owner

## Context and Problem Statement

After [ADR-0001](0001-control-plane-over-existing-engines.md) the backend is a set of HTTP clients, a
subprocess supervisor, a state machine, a SQLite store and an SSE fan-out. There is no in-process
libtorrent requirement and no in-process yt-dlp requirement — yt-dlp is invoked as `yt-dlp -J <url>` and its
stdout is parsed. Every candidate language can do that work, so the tie-breaker is not capability but
**which language a weaker model gets right**, and the plan must justify that honestly rather than by taste.

## Decision Drivers

- The implementing agent is a weaker model that runs `go build ./...` and `go vet ./...` after every edit.
  The question is not "is the first draft correct" but "**is an incorrect draft detected**".
- Every code path in a download manager is flaky I/O — network, disk, subprocess. Error paths must be
  written down, not defaulted.
- One static binary with an embedded SPA is what makes dl-tool a single container
  ([ADR-0007](0007-react-spa-embedded-in-the-binary.md), [ADR-0011](0011-alpine-runtime-image-with-puid-pgid-privilege-drop.md)).
- Dependency resolution must be hermetic and reproducible in CI without wheels, C extensions or peer-dep
  resolution.

## Considered Options

- **Option A** — Go 1.26 + chi v5 + Huma v2, `CGO_ENABLED=0`.
- **Option B** — Python 3.13 + FastAPI + SQLAlchemy 2.0 + APScheduler.
- **Option C** — TypeScript on Node, one language across backend and frontend.
- **Option D** — Rust + axum.

## Decision Outcome

Chosen option: **Go 1.26**, because the compiler converts the mistakes this project's author will actually
make into build failures before they ship, and because `CGO_ENABLED=0 go build` yields the single static
binary the packaging decisions depend on.

The honest counter-evidence points the other way and must be stated. Multi-language pass@1 benchmarking
favours Python:

> "Python achieves the highest mean Pass@1 of 0.482, with Java and C++ close behind at about 0.44.
> C#, Ruby, PHP, Go, Rust, Kotlin and JavaScript/TypeScript form a middle tier with means near 0.33–0.39"
> — Multi-LCB, *Extending LiveCodeBench to Multiple Programming Languages*, arXiv 2606.20517

That benchmark measures **single-shot function synthesis with no compiler in the loop**. dl-tool is not
written that way, and putting a compiler in the loop inverts the ranking:

| Failure mode a weak model actually commits | Go | Python / FastAPI | TypeScript / Node |
|---|---|---|---|
| Wrong field name on a struct or dict | **compile error** | `KeyError`/`AttributeError` at request time | compile error if typed, silent if `any` |
| Unhandled error path | `declared and not used` → **compile error** | silent | silent |
| Wrong argument order or arity | **compile error** | runtime `TypeError` | compile error |
| Dependency drift | `go.mod` + `go.sum`, hermetic | `requirements.txt` + wheels + C extensions | lockfile + peer deps |
| Concurrency | goroutine plus channel, one idiom | asyncio vs threads vs processes, and mixing them | one, but a missing `await` is silent |

Pin `go 1.26` in `go.mod` and build with `golang:1.26-alpine`. Not 1.27 — it was 13 days old at plan time
and sits outside the training corpus. Huma states *"Note that Go 1.25 or newer is required"*, which 1.26
satisfies. The runner-up is Option B and the fallback is coherent: FastAPI's pydantic request and response
models are the same declarative shape as Huma's input and output structs, so the API design transfers 1:1.

### Consequences

- Good, because every error path is textual and reviewable, which is what a network-and-disk product needs.
- Good, because HTTP server and client, JSON, SSE, `os/exec`, `context` and `log/slog` are all stdlib, so
  there are fewer third-party method names to hallucinate.
- Bad, because Go is verbose; the plan treats that verbosity as a feature rather than apologising for it.
- Neutral, because backend and frontend share no runtime — the only shared artefact is the generated
  OpenAPI type set ([ADR-0003](0003-chi-huma-code-first-openapi.md)).

### Confirmation

The build gate is the decision. A pure-Go dependency set is what keeps `CGO_ENABLED=0` viable:

```bash
make lint && make vet && make build && file bin/dl-tool | grep -q 'statically linked'
```

Expected: exit 0, and `bin/dl-tool` reported as a statically linked ELF executable. `make build` sets
`CGO_ENABLED=0` explicitly, so any dependency that needs cgo fails the target rather than silently
producing a dynamically linked binary. CI runs the same target — see
[`../13-testing-and-verification.md`](../13-testing-and-verification.md).

## Pros and Cons of the Options

### Option A - Go 1.26

- Good, because unused variables and unused imports are hard compile errors, which catches the two most
  common half-finished edits.
- Good, because one static binary means the runtime image carries no language runtime at all.
- Bad, because generics and error wrapping are the two places a weak model still writes noise.

### Option B - Python 3.13 + FastAPI

- Good, because it wins the cited single-shot synthesis benchmark outright, and FastAPI's declarative
  models are the closest analogue to Huma's.
- Bad, because the image is `python:3.13-slim` at roughly 120 MB before dependencies, against a roughly
  2 MB Alpine base.
- Bad, because there is no compile-time gate: a wrong field name surfaces as a 500 in production.

### Option C - TypeScript on Node

- Good, because "one language everywhere" is real, and the type system is strong when it is used.
- Bad, because the sharing benefit is captured anyway — the frontend types are generated from the OpenAPI
  document, not shared from a backend module.
- Bad, because a missing `await` is silent and the runtime must ship in the image.

### Option D - Rust + axum

- Good, because it has the strongest compile-time gate of any candidate and no runtime.
- Bad, because borrow-checker plus async lifetimes is the one place a weaker model enters an unrecoverable
  edit loop; any async HTTP handler produces a stream of `'static` lifetime errors.

## More Information

- Research: `architecture.md` §1 and its fact-check — summarised in
  [`../16-prior-art-and-research.md`](../16-prior-art-and-research.md).
- Exact pinned versions live in [`../03-architecture.md`](../03-architecture.md); build and image details in
  [`../10-deployment-and-compose.md`](../10-deployment-and-compose.md).
