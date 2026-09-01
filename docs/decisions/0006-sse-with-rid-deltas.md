# 0006 - Server-sent events with rid deltas for live updates

> **Status:** accepted
> **Date:** 2026-09-01
> **Deciders:** repository owner

## Context and Problem Statement

The Download Station screen a user expects is a dense table that moves: speeds, progress, ETA and peer
counts change every second across every visible row, and a task list can run to thousands of rows. Re-sending
the whole list once a second is the naive answer and it is wrong at that size. The transport also has to
survive the reverse proxies users actually run in front of a NAS, because a live screen that dies behind
Caddy or nginx reads as a broken product.

## Decision Drivers

- Every client-to-server action — pause, resume, remove, add, reprioritise — is already a REST call. Nothing
  needs a back-channel.
- The client must have exactly one reducer. Reconnection, heartbeat and backoff written by hand are three
  places for a weaker model to be subtly wrong.
- The payload must be small enough that a 1 Hz update over a 500-row grid is cheap.
- A fallback for environments that break streaming has to be free, not a second code path.

## Considered Options

- **Option A** — SSE at `GET /api/v1/events` carrying qBittorrent-style `rid` deltas, with an identical
  envelope at `GET /api/v1/sync?rid=N` as the polling fallback.
- **Option B** — WebSocket (`coder/websocket`) carrying the same deltas.
- **Option C** — Plain periodic polling of `GET /api/v1/tasks`, full list each time.
- **Option D** — SSE carrying one event per change, the client refetching the affected task (aria2's model).

## Decision Outcome

Chosen option: **Option A**, because `Last-Event-ID` **is** the `rid`. The WHATWG SSE reconnection contract
and qBittorrent's delta protocol compose with zero client bookkeeping: the browser reconnects on its own,
resends the last id it saw, and honours a server-sent `retry:`. Huma's `huma/v2/sse` registers the stream in
the OpenAPI document with a discriminated event-to-struct map, so the reducer's TypeScript type is generated
by [ADR-0003](0003-chi-huma-code-first-openapi.md) rather than hand-written.

Fixed by this decision; the payload itself is specified in [`../05-api-contract.md`](../05-api-contract.md):

- Exactly one event type, `sync`, so the client has one reducer. The server pushes at most once per second.
- An in-memory ring buffer of the last **300** deltas keyed by `rid`, about five minutes at 1 Hz. A
  reconnect inside the buffer is served the coalesced diff; one outside it gets `full_update: true` and
  `seq_gap: true`.
- `Retry: 3000` on the first message and an SSE comment heartbeat every **15 s** to defeat proxy idle timeouts.
- `GET /api/v1/sync?rid=N` returns the identical envelope from the same delta builder. The client falls back
  to 2 s polling after three consecutive stream failures.

### Consequences

- Good, because reconnection is the browser's job; the server keeps no per-stream session state beyond the ring.
- Good, because SSE is a plain HTTP response with no `Upgrade` dance: nginx needs `proxy_buffering off;`
  where WebSocket needs that plus `Upgrade`/`Connection` headers and a 24 h `proxy_read_timeout`.
- Bad, because SSE is unidirectional and, over HTTP/1.1, holds one of the browser's six per-origin
  connections; several open tabs can starve the API. HTTP/2 behind a reverse proxy removes it, and
  [`../10-deployment-and-compose.md`](../10-deployment-and-compose.md) says so.
- Bad, because the delta carries partially populated task objects, which JSON Schema expresses poorly; `05`
  documents the merge rule by hand and a golden-file test keeps it true.
- Neutral, because the hub is transport-agnostic: a WebSocket transport could be added behind it without
  touching the delta builder.

### Confirmation

```bash
make test PKG=./internal/sync/... && make test PKG=./internal/api/...
```

Expected: exit 0. `TestRingReplayFromLastEventID` asserts that a reconnect with an in-buffer id replays a
coalesced diff and that an out-of-buffer id yields `full_update` with `seq_gap`.
`TestSyncEndpointMatchesSSEEnvelope` asserts `GET /sync?rid=N` and the stream produce a byte-identical
payload from the same golden file. The user-visible behaviour is covered by `web/e2e/live-updates.spec.ts`,
which fails if the grid needs a reload to show progress.

## Pros and Cons of the Options

### Option A - SSE with rid deltas

- Good, because the reconnection semantics are specified by WHATWG and implemented by the browser.
- Good, because the same envelope serves both the stream and the poll, so there is one reducer and one test.
- Bad, because a proxy that buffers responses silently delays every update, and the symptom looks like a
  hung application rather than a configuration error.

### Option B - WebSocket

- Good, because it is bidirectional and immune to the per-origin connection limit.
- Bad, because it buys a back-channel nothing needs, and reconnection, heartbeat and backoff all become
  hand-written client code.
- Bad, because it needs strictly more reverse-proxy configuration than SSE, in exactly the deployments least
  under our control.

### Option C - full-list polling

- Good, because it is the simplest thing that works and needs no server state at all.
- Bad, because a 500-row grid at 1 Hz re-serialises the whole list sixty times a minute, and every poll
  re-renders rows that did not change.

### Option D - notification events plus refetch

- Good, because the server-side event is trivial to emit at the point where state changes.
- Bad, because it is aria2's model and aria2 demonstrates the cost: the notification carries only a GID, so
  the client must call `aria2.tellStatus` anyway, doubling the round trips. aria2 also pushes notifications
  over WebSocket only, never over HTTP.

## More Information

- Research: `architecture.md` §4.1 to §4.3 and its fact-check — summarised in
  [`../16-prior-art-and-research.md`](../16-prior-art-and-research.md).
- Depends on this decision: [`../05-api-contract.md`](../05-api-contract.md),
  [`../03-architecture.md`](../03-architecture.md), [`../09-web-ui-spec.md`](../09-web-ui-spec.md).
