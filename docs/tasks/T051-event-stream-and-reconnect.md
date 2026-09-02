# T051 — Connect the event stream, the reconnect ladder and the polling fallback

| Field | Value |
|---|---|
| **ID** | T051 |
| **Milestone** | M3 |
| **Status** | todo |
| **Depends on** | T011, T025, T041, T044 |
| **Blocks** | T104 |
| **Parallel-safe** | no — it also edits the shared files `web/src/App.tsx`, `web/src/locales/en/common.json` |
| **Implements** | [NFR-002](../02-requirements.md#nfr-002-recover-cleanly-from-a-dropped-event-stream) |
| **Decisions** | [ADR-0006](../decisions/0006-sse-with-rid-deltas.md) |
| **Est. size** | 3 new files, ~300 LOC |

## Goal
The SPA holds one `EventSource` on `GET /api/v1/events`, feeds every `sync` event into the T041 reducer,
and survives a dropped stream: amber dot, banner, the documented backoff ladder, a polling fallback and a
full refetch on recovery. No component subscribes to the stream itself.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §6.1 `GET /events`](../05-api-contract.md#61-get-events) — the envelope, the
   `Last-Event-ID` rule, the heartbeat comment and the client rules at the end.
2. [`docs/05-api-contract.md` §6.2 `GET /sync`](../05-api-contract.md#62-get-sync) — the identical payload
   and the `rid` query.
3. [`docs/09-web-ui-spec.md` §10.8 Reconnect banner](../09-web-ui-spec.md#108-reconnect-banner) — the seven
   numbered rules, including the separate `401` banner.
4. [`docs/tasks/T041-task-store-and-formatters.md`](T041-task-store-and-formatters.md) — `applySync`,
   `connection` and `setConnection`.
5. [`docs/tasks/T014-typed-api-client.md`](T014-typed-api-client.md) — `eventsUrl()` and `apiUrl()`.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/api/events.ts` | create | The stream, the ladder, the polling fallback and the invalidation hook. |
| `web/src/api/events.test.ts` | create | Ladder, fallback, refetch and 401 behaviour. |
| `web/src/components/Shell/ReconnectBanner.tsx` | create | The two banners of doc 09 §10.8. |
| `web/src/App.tsx` | edit | Start the transport once, inside the authenticated layout. |
| `web/src/locales/en/common.json` | edit | Banner and connection strings. |

No other file may be modified.

## Interface contract

```ts
// web/src/api/events.ts
/** Reconnect delays in seconds, doc 09 section 10.8 rule 3; the last value repeats. */
export const BACKOFF_SECONDS = [1, 2, 4, 8, 15, 30] as const;

/** Consecutive stream failures after which the client polls instead, doc 05 section 6.1. */
export const POLL_AFTER_FAILURES = 3;

/** Polling period in milliseconds. Doc 05 section 6.1 and doc 09 section 10.8 rule 4 both say 2 s. */
export const POLL_INTERVAL_MS = 2000;

/** Silence after which the connection reads offline: two 15 s heartbeats, doc 09 section 10.8 rule 1. */
export const OFFLINE_AFTER_MS = 30_000;

export interface Transport {
  /** Opens the stream and keeps it open until stop(). Safe to call once per session. */
  start(): void;
  stop(): void;
  /** Forces an immediate reconnect attempt, used by the banner's Retry now button. */
  retryNow(): void;
}

/** One transport per app. onSync receives every payload, whether it arrived by SSE or by polling. */
export function createTransport(opts: {
  onSync: (msg: import('../store/useTasks').SyncMessage) => void;
  onConnection: (c: 'connecting' | 'live' | 'polling' | 'offline') => void;
  onUnauthenticated: () => void;
  queryClient: import('@tanstack/react-query').QueryClient;
}): Transport;

/** Mounts the transport for the lifetime of the authenticated shell. */
export function useEventStream(): { retryNow: () => void; nextRetryIn: number | null };
```

Behaviour, from doc 05 §6.1 and doc 09 §10.8:

| Trigger | Effect |
|---|---|
| `sync` event | `applySync(payload)`; on any `state` change, insert or removal also call `invalidateTaskList` (T042). |
| no `sync` event, no `hb` event and no poll response for `OFFLINE_AFTER_MS` | `connection = 'offline'`, dot amber, grid dimmed to 70 %. |
| 5 s disconnected | Render the banner, `role="alert"`, with the countdown and `Retry now`. |
| reconnect attempt *n* | Wait `BACKOFF_SECONDS[min(n-1, 5)]` seconds. |
| 3 consecutive failures | `connection = 'polling'`; `GET /sync?rid=<last>` every `POLL_INTERVAL_MS`, while still retrying SSE in the background. |
| recovery | Refetch with `rid=0`, that is a full update, show a `Reconnected` toast and clear the banner. |
| `401` before the stream opens | The separate banner `Your session expired. [Sign in]`; no retry ladder. |

`EventSource` resends `Last-Event-ID` itself, and doc 05 §6.1 states that header **is** the rid, so the
client keeps no separate stream bookkeeping; it stores the last rid only for the polling URL.

## Steps
1. Create `web/src/api/events.ts` with the constants, `createTransport` and `useEventStream`.
2. Open `new EventSource(eventsUrl(), { withCredentials: true })` and listen for the named events `sync` and `hb`;
   parse `event.data` and hand it to `onSync`. Never listen for `message`.
3. Track liveness only from what the browser can observe — a `sync` payload, the named `hb` event of doc 05
   §6.1, or a successful poll response. `EventSource` discards comment lines, so `: hb` is invisible to the
   client. More than `OFFLINE_AFTER_MS` without any of the three means offline.
4. Implement the ladder exactly as `BACKOFF_SECONDS` gives it, resetting the attempt counter on the first
   successful message, and expose the remaining seconds so the banner can count down.
5. After `POLL_AFTER_FAILURES` failures start the poller on `GET /sync?rid=<last rid>` through the T014
   client, keep the same reducer, and stop it the moment SSE succeeds again.
6. On recovery request `rid=0` once, so the store is replaced rather than merged, then toast `Reconnected`.
7. On `401` call `onUnauthenticated`, which renders the session-expired banner and stops the ladder.
8. While `connection` is not `live`, disable mutating controls and discard any queued optimistic mutation
   with a toast rather than retrying it silently.
9. Create `ReconnectBanner.tsx` with both banners and mount it beneath the toolbar; edit `App.tsx` to call
   `useEventStream()` once inside the authenticated layout.
10. Create `web/src/api/events.test.ts` with a stubbed `EventSource`: a `sync` event reaches `applySync`;
    the ladder is `1, 2, 4, 8, 15, 30, 30`; the third failure starts polling at 2 000 ms; the recovery
    request carries `rid=0`; a `401` renders the session banner and schedules no retry.
11. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestSyncEventReachesReducer`, `TestBackoffLadder` and `TestPollingAfterThreeFailures` pass.
- [ ] `TestRecoveryRefetchesWithRidZero` passes.
- [ ] `TestUnauthenticatedRendersSessionBanner` passes and asserts no retry is scheduled.
- [ ] Exactly one `EventSource` exists per session; a route change does not open a second.
- [ ] No component other than this module constructs an `EventSource` or calls `GET /sync`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo TRANSPORT_OK
```
Expected: Vitest reports `Test Files  11 passed (11)` including `src/api/events.test.ts`, every test named
above appears as passing, and the final line of stdout is exactly `TRANSPORT_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT change the reducer; T041 owns `applySync` and this task only feeds it.
- Do NOT add a WebSocket, long-poll or third transport; ADR-0006 settles this.
- Do NOT replay deltas after a reconnect; refetch with `rid=0` instead.
- Do NOT store the rid in `localStorage`; it is per connection and resets when the server restarts.
- Do NOT silently retry a failed mutation while disconnected.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
