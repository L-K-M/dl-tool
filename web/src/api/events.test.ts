import { act, cleanup, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { StrictMode, createElement, type ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { toast } from "sonner";
import {
  afterEach,
  beforeEach,
  expect,
  test,
  vi,
  type MockInstance,
} from "vitest";

import { api } from "./client";
import {
  BACKOFF_SECONDS,
  createTransport,
  OFFLINE_AFTER_MS,
  POLL_AFTER_FAILURES,
  POLL_INTERVAL_MS,
  useEventStream,
  useTransportUi,
} from "./events";
import { ReconnectBanner } from "../components/Shell/ReconnectBanner";
import { initI18n } from "../i18n";
import { useTasks, type SyncMessage } from "../store/useTasks";

// happy-dom has no EventSource; the stub records every instance and lets each
// test drive the named events doc 05 section 6.1 defines.
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  static open(): FakeEventSource[] {
    return FakeEventSource.instances.filter((i) => !i.closed);
  }
  readonly url: string;
  readonly withCredentials: boolean;
  closed = false;
  private listeners = new Map<string, Set<EventListener>>();

  constructor(url: string | URL, init?: EventSourceInit) {
    this.url = String(url);
    this.withCredentials = init?.withCredentials ?? false;
    FakeEventSource.instances.push(this);
  }
  addEventListener(type: string, listener: EventListener): void {
    const set = this.listeners.get(type) ?? new Set();
    set.add(listener);
    this.listeners.set(type, set);
  }
  removeEventListener(type: string, listener: EventListener): void {
    this.listeners.get(type)?.delete(listener);
  }
  close(): void {
    this.closed = true;
  }
  emit(type: string, data?: unknown): void {
    const event =
      data === undefined
        ? new Event(type)
        : new MessageEvent(type, { data: JSON.stringify(data) });
    for (const listener of this.listeners.get(type) ?? []) listener(event);
  }
}

type SyncResult = {
  data?: SyncMessage;
  error?: unknown;
  response: Response;
};
type GetStub = MockInstance<
  (
    path: string,
    init?: {
      params?: { query?: { rid?: number } };
      signal?: AbortSignal;
    },
  ) => Promise<SyncResult>
>;

const ok = (msg: SyncMessage): SyncResult => ({
  data: msg,
  response: new Response(null, { status: 200 }),
});
const unauthorized = (): SyncResult => ({
  error: { type: "/problems/unauthenticated" },
  response: new Response(null, { status: 401 }),
});
const failed = (): SyncResult => ({
  error: { type: "/problems/unavailable" },
  response: new Response(null, { status: 503 }),
});

const delta = (patch: Partial<SyncMessage> = {}): SyncMessage => ({
  rid: 1,
  full_update: false,
  seq_gap: false,
  tasks: {},
  tasks_removed: [],
  stats: { speed_down: 0, speed_up: 0, active: 0, queued: 0 },
  ...patch,
});

let qc: QueryClient;
let getSync: GetStub;

const ridOf = (index: number) =>
  getSync.mock.calls.at(index)?.[1]?.params?.query?.rid;

function makeTransport() {
  const syncs: SyncMessage[] = [];
  const connections: string[] = [];
  const unauth = vi.fn();
  const transport = createTransport({
    onSync: (msg) => {
      syncs.push(msg);
      useTasks.getState().applySync(msg);
    },
    onConnection: (c) => {
      connections.push(c);
      useTasks.getState().setConnection(c);
    },
    onUnauthenticated: unauth,
    queryClient: qc,
  });
  return { transport, syncs, connections, unauth };
}

async function tick(ms: number): Promise<void> {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

function Harness({ banner = false }: { banner?: boolean }) {
  const { retryNow } = useEventStream();
  return banner ? createElement(ReconnectBanner, { retryNow }) : null;
}

function wrap(node: ReactNode) {
  return createElement(
    QueryClientProvider,
    { client: qc },
    createElement(MemoryRouter, null, node),
  );
}

function mount(node: ReactNode) {
  return render(wrap(node));
}

beforeEach(() => {
  initI18n();
  vi.useFakeTimers();
  FakeEventSource.instances = [];
  vi.stubGlobal("EventSource", FakeEventSource);
  useTasks.getState().reset();
  useTransportUi.setState({
    unauthenticated: false,
    banner: false,
    nextRetryIn: null,
  });
  qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  getSync = vi.spyOn(api, "GET") as unknown as GetStub;
  getSync.mockResolvedValue(ok(delta()));
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  qc.clear();
});

test("TestSyncEventReachesReducer", () => {
  const { transport } = makeTransport();
  transport.start();
  expect(FakeEventSource.instances).toHaveLength(1);
  const stream = FakeEventSource.instances[0];
  expect(stream.withCredentials).toBe(true);
  act(() =>
    stream.emit(
      "sync",
      delta({ rid: 7, tasks: { t1: { id: "t1", state: "downloading" } } }),
    ),
  );
  expect(useTasks.getState().tasks.get("t1")?.state).toBe("downloading");
  expect(useTasks.getState().rid).toBe(7);
  expect(useTasks.getState().connection).toBe("live");
  transport.stop();
});

test("TestBackoffLadder", async () => {
  getSync.mockResolvedValue(failed());
  const { transport } = makeTransport();
  transport.start();
  // Doc 09 section 10.8 rule 3: 1, 2, 4, 8, 15, 30 s, then every 30 s.
  const expected = [...BACKOFF_SECONDS, BACKOFF_SECONDS.at(-1)!];
  expect(expected).toEqual([1, 2, 4, 8, 15, 30, 30]);
  for (const delay of expected) {
    const stream = FakeEventSource.instances.at(-1)!;
    const count = FakeEventSource.instances.length;
    act(() => stream.emit("error"));
    await tick(delay * 1000 - 1);
    expect(FakeEventSource.instances).toHaveLength(count);
    await tick(1);
    expect(FakeEventSource.instances).toHaveLength(count + 1);
    // A failed stream must be closed before its replacement opens, or a real
    // browser would keep it retrying and double-apply every sync event.
    expect(
      FakeEventSource.instances.slice(0, count).every((i) => i.closed),
    ).toBe(true);
  }
  transport.stop();
});

test("TestPollingAfterThreeFailures", async () => {
  const { transport, connections } = makeTransport();
  transport.start();
  act(() => FakeEventSource.instances[0].emit("sync", delta({ rid: 7 })));
  // A closed stream cannot fail again, so each failure waits for the next
  // attempt the ladder opens: +1 s, then +2 s.
  for (const retry of [0, 1000, 2000]) {
    await tick(retry);
    act(() => FakeEventSource.instances.at(-1)!.emit("error"));
  }
  await tick(0);
  expect(connections.at(-1)).toBe("polling");
  // Each failure already probed /sync once with the last rid.
  expect(getSync.mock.calls).toHaveLength(POLL_AFTER_FAILURES);
  for (const call of getSync.mock.calls) {
    expect(call[0]).toBe("/sync");
    expect(call[1]?.params?.query?.rid).toBe(7);
  }

  // The poller fires every POLL_INTERVAL_MS; the SSE ladder keeps running.
  await tick(POLL_INTERVAL_MS);
  expect(getSync.mock.calls).toHaveLength(POLL_AFTER_FAILURES + 1);
  expect(ridOf(-1)).toBe(7);
  await tick(POLL_INTERVAL_MS);
  expect(getSync.mock.calls).toHaveLength(POLL_AFTER_FAILURES + 2);
  // The fourth rung opens a new stream while the poller is active.
  expect(FakeEventSource.instances.length).toBeGreaterThan(POLL_AFTER_FAILURES);
  transport.stop();
});

test("TestHungProbeReleasesTheGuard", async () => {
  // A /sync request that never responds must not stall the fallback: each
  // reconnect aborts the hung probe and releases the in-flight guard, and
  // the deadline still covers a probe that outlives its rung.
  getSync.mockImplementation(
    (_path, init) =>
      new Promise<SyncResult>((_resolve, reject) => {
        init?.signal?.addEventListener("abort", () =>
          reject(new DOMException("Aborted", "AbortError")),
        );
      }),
  );
  const { transport } = makeTransport();
  transport.start();
  act(() => FakeEventSource.instances[0].emit("error"));
  await tick(0);
  expect(getSync).toHaveBeenCalledTimes(1);
  // The 1 s rung's reconnect aborts the hung probe, so the next rung probes
  // immediately instead of waiting out the deadline.
  await tick(1000);
  act(() => FakeEventSource.instances.at(-1)!.emit("error"));
  await tick(0);
  expect(getSync).toHaveBeenCalledTimes(2);
  await tick(2000);
  act(() => FakeEventSource.instances.at(-1)!.emit("error"));
  await tick(0);
  expect(useTasks.getState().connection).toBe("polling");
  expect(getSync).toHaveBeenCalledTimes(3);
  // Polling keeps probing: the 4 s rung's reconnect aborts the latest hung
  // probe — the poll tick sharing that millisecond still sees the latched
  // guard — so the following tick probes again.
  await tick(POLL_INTERVAL_MS * 3);
  expect(getSync.mock.calls.length).toBeGreaterThan(3);
  transport.stop();
});

test("TestRecoveryRefetchesWithRidZero", async () => {
  const reconnected = vi.spyOn(toast, "success");
  const { transport } = makeTransport();
  transport.start();
  act(() => FakeEventSource.instances[0].emit("sync", delta({ rid: 7 })));
  act(() => FakeEventSource.instances[0].emit("error"));
  await tick(0);
  expect(ridOf(-1)).toBe(7); // the outage probe, not the recovery
  await tick(1000); // first rung of the ladder opens a new stream
  expect(FakeEventSource.instances).toHaveLength(2);

  getSync.mockResolvedValue(
    ok(
      delta({
        rid: 20,
        full_update: true,
        tasks: { t9: { id: "t9", state: "queued" } },
      }),
    ),
  );
  act(() => FakeEventSource.instances[1].emit("sync", delta({ rid: 8 })));
  await tick(0);
  expect(ridOf(-1)).toBe(0); // doc 09 section 10.8 rule 5: refetch, no replay
  await tick(0);
  expect(useTasks.getState().connection).toBe("live");
  expect(useTasks.getState().tasks.has("t9")).toBe(true);
  expect(reconnected).toHaveBeenCalledWith("Reconnected");
  expect(useTransportUi.getState().banner).toBe(false);
  transport.stop();
});

test("TestUnauthenticatedRendersSessionBanner", async () => {
  getSync.mockResolvedValue(unauthorized());
  mount(createElement(Harness, { banner: true }));
  expect(FakeEventSource.instances).toHaveLength(1);
  act(() => FakeEventSource.instances[0].emit("error"));
  await tick(0);
  const alert = screen.getByRole("alert");
  expect(alert.textContent).toContain("Your session expired.");
  expect(within(alert).getByRole("link", { name: "Sign in" })).toBeTruthy();
  // Rule 7: no retry ladder runs behind the session banner.
  await tick(120_000);
  expect(FakeEventSource.instances).toHaveLength(1);
  expect(FakeEventSource.open()).toHaveLength(0);
  expect(useTransportUi.getState().nextRetryIn).toBeNull();
  expect(useTransportUi.getState().banner).toBe(false);
});

test("TestSingleEventSourcePerSession", () => {
  function Stream() {
    useEventStream();
    return null;
  }
  const tree = createElement(StrictMode, null, createElement(Stream));
  const view = mount(tree);
  expect(FakeEventSource.open()).toHaveLength(1);
  // A re-render — which is all a client-side route change is under the same
  // layout — must not open a second stream.
  view.rerender(wrap(tree));
  expect(FakeEventSource.open()).toHaveLength(1);
});

test("TestSilenceMarksOfflineAndRaisesBanner", async () => {
  const { transport, connections } = makeTransport();
  transport.start();
  await tick(OFFLINE_AFTER_MS - 1);
  expect(connections.at(-1)).toBe("connecting");
  expect(useTransportUi.getState().banner).toBe(false);
  await tick(1);
  expect(connections.at(-1)).toBe("offline");
  expect(useTransportUi.getState().banner).toBe(true);
  transport.stop();
});

test("TestSilenceForcesAReconnect", async () => {
  const { transport } = makeTransport();
  transport.start();
  expect(FakeEventSource.instances).toHaveLength(1);
  // A half-open stream never fires an error event, so the silence detector
  // must close it and open a replacement; if that fails, the ladder runs.
  await tick(OFFLINE_AFTER_MS);
  expect(useTasks.getState().connection).toBe("offline");
  expect(FakeEventSource.instances).toHaveLength(2);
  expect(FakeEventSource.instances[0].closed).toBe(true);
  expect(FakeEventSource.instances[1].closed).toBe(false);
  transport.stop();
});

test("TestAmberNeverFiresAfterTheBanner", async () => {
  const { transport, connections } = makeTransport();
  transport.start();
  act(() => FakeEventSource.instances[0].emit("error"));
  // The banner takes five seconds of known-down.
  await tick(4_999);
  expect(useTransportUi.getState().banner).toBe(false);
  await tick(1);
  expect(useTransportUi.getState().banner).toBe(true);
  // The first rung already fired: no retry is pending, so the countdown is
  // cleared rather than frozen on a stale number.
  expect(useTransportUi.getState().nextRetryIn).toBeNull();
  // Rule 2's ladder is monotonic: silence past the threshold must not regress
  // an already-bannered outage to amber.
  await tick(OFFLINE_AFTER_MS);
  expect(connections).not.toContain("offline");
  transport.stop();
});

test("TestStructuralSyncInvalidatesTaskList", () => {
  const inv = vi.spyOn(qc, "invalidateQueries");
  mount(createElement(Harness));
  const stream = FakeEventSource.instances[0];
  act(() =>
    stream.emit(
      "sync",
      delta({ rid: 1, tasks: { a: { id: "a", progress: 0.5 } } }),
    ),
  );
  expect(inv).toHaveBeenCalledTimes(1); // insert
  act(() =>
    stream.emit("sync", delta({ rid: 2, tasks: { a: { progress: 0.6 } } })),
  );
  expect(inv).toHaveBeenCalledTimes(1); // a pure field patch does not refetch
  act(() =>
    stream.emit("sync", delta({ rid: 3, tasks: { a: { state: "paused" } } })),
  );
  expect(inv).toHaveBeenCalledTimes(2); // state change
  act(() => stream.emit("sync", delta({ rid: 4, tasks_removed: ["a"] })));
  expect(inv).toHaveBeenCalledTimes(3); // removal
  act(() => stream.emit("sync", delta({ rid: 5, tasks: { b: { id: "b" } } })));
  expect(inv).toHaveBeenCalledTimes(4); // insert
  // Category and tag are server-side list filters too: a patch to either
  // rewrites a filtered grid's membership, so the list refetches.
  act(() =>
    stream.emit("sync", delta({ rid: 6, tasks: { b: { category: "iso" } } })),
  );
  expect(inv).toHaveBeenCalledTimes(5);
  act(() =>
    stream.emit("sync", delta({ rid: 7, tasks: { b: { tags: ["x"] } } })),
  );
  expect(inv).toHaveBeenCalledTimes(6);
  act(() =>
    stream.emit("sync", delta({ rid: 8, tasks: { b: { progress: 0.9 } } })),
  );
  expect(inv).toHaveBeenCalledTimes(6); // a pure field patch does not refetch
});

test("TestStopDropsInFlightResponses", async () => {
  let release: ((result: SyncResult) => void) | undefined;
  let signal: AbortSignal | undefined;
  getSync.mockImplementation(
    (_path, init) =>
      new Promise<SyncResult>((resolvePromise) => {
        signal = init?.signal;
        release = resolvePromise;
      }),
  );
  const { transport, syncs, unauth } = makeTransport();
  transport.start();
  act(() => FakeEventSource.instances[0].emit("error"));
  await tick(0);
  expect(getSync).toHaveBeenCalledTimes(1);
  transport.stop();
  // stop() aborts the request, and the late answer must not reach the store
  // or the banners even though its rid looks fresh.
  expect(signal?.aborted).toBe(true);
  release?.(ok(delta({ rid: 5, tasks: { late: { id: "late" } } })));
  await tick(0);
  expect(syncs).toHaveLength(0);
  expect(useTasks.getState().tasks.has("late")).toBe(false);
  expect(unauth).not.toHaveBeenCalled();
});

test("TestLateResponsesCannotRegressTheStore", async () => {
  const releases: ((result: SyncResult) => void)[] = [];
  getSync.mockImplementation(
    () =>
      new Promise<SyncResult>((resolvePromise) =>
        releases.push(resolvePromise),
      ),
  );
  const { transport, syncs } = makeTransport();
  transport.start();
  const first = FakeEventSource.instances[0];
  act(() => first.emit("sync", delta({ rid: 5 })));
  act(() => first.emit("error"));
  await tick(0);
  expect(ridOf(0)).toBe(5); // the outage probe, still in flight
  await tick(1000); // the first rung opens a fresh stream and a new epoch
  const second = FakeEventSource.instances[1];
  act(() => second.emit("sync", delta({ rid: 10 })));
  await tick(0);
  expect(ridOf(1)).toBe(0); // the recovery refetch, also still in flight
  // Generated before rid 10 went out: the slower snapshot must lose.
  releases[1](
    ok(delta({ rid: 9, full_update: true, tasks: { old: { id: "old" } } })),
  );
  // Issued under the pre-reconnect epoch: even a newer rid must not land.
  releases[0](ok(delta({ rid: 11, tasks: { stale: { id: "stale" } } })));
  await tick(0);
  expect(syncs.map((msg) => msg.rid)).toEqual([5, 10]);
  expect(useTasks.getState().tasks.has("old")).toBe(false);
  expect(useTasks.getState().tasks.has("stale")).toBe(false);
  expect(useTasks.getState().connection).toBe("live");
  transport.stop();
});

test("TestRestartedServerRebaselinesOnANewEpoch", async () => {
  getSync.mockResolvedValue(failed());
  const { transport, syncs } = makeTransport();
  transport.start();
  const first = FakeEventSource.instances[0];
  act(() => first.emit("sync", delta({ rid: 100, tasks: { a: { id: "a" } } })));
  act(() => first.emit("error"));
  await tick(1000); // the first rung opens a fresh connection and epoch
  const second = FakeEventSource.instances[1];
  // The restarted process cannot honour Last-Event-ID: it opens with a full
  // snapshot at the bottom of its restarted rid sequence (doc 05 §6.1).
  act(() =>
    second.emit(
      "sync",
      delta({
        rid: 2,
        full_update: true,
        seq_gap: true,
        tasks: { b: { id: "b" } },
      }),
    ),
  );
  expect(syncs.map((msg) => msg.rid)).toEqual([100, 2]);
  expect(useTasks.getState().tasks.has("b")).toBe(true);
  expect(useTasks.getState().tasks.has("a")).toBe(false);
  act(() => second.emit("sync", delta({ rid: 3, tasks: { c: { id: "c" } } })));
  expect(useTasks.getState().tasks.has("c")).toBe(true);
  // A replayed pre-restart delta must not apply over the new baseline.
  act(() =>
    second.emit("sync", delta({ rid: 2, tasks: { ghost: { id: "ghost" } } })),
  );
  expect(useTasks.getState().tasks.has("ghost")).toBe(false);
  transport.stop();
});

test("TestPollingFallbackDetectsARestart", async () => {
  const { transport, syncs } = makeTransport();
  transport.start();
  act(() => FakeEventSource.instances[0].emit("sync", delta({ rid: 100 })));
  for (const retry of [0, 1000, 2000]) {
    await tick(retry);
    act(() => FakeEventSource.instances.at(-1)!.emit("error"));
  }
  await tick(0);
  expect(useTasks.getState().connection).toBe("polling");
  // The server restarted between polls: it cannot honour rid=100 and answers
  // a full snapshot at the bottom of its restarted sequence. A response rid
  // at or below the requested rid proves the restart — a live process never
  // rewinds its counter.
  getSync.mockResolvedValueOnce(
    ok(
      delta({
        rid: 1,
        full_update: true,
        seq_gap: true,
        tasks: { fresh: { id: "fresh" } },
      }),
    ),
  );
  await tick(POLL_INTERVAL_MS);
  expect(syncs.map((msg) => msg.rid)).toEqual([100, 1]);
  expect(useTasks.getState().tasks.has("fresh")).toBe(true);
  // The next poll continues from the new baseline.
  await tick(POLL_INTERVAL_MS);
  expect(ridOf(-1)).toBe(1);
  transport.stop();
});

test("TestTransportOwnsTheStreamAndSyncEndpoint", () => {
  const srcRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
  const allowed = new Set([
    join(srcRoot, "api", "events.ts"),
    join(srcRoot, "api", "events.test.ts"),
  ]);
  const walk = (dir: string): string[] =>
    readdirSync(dir).flatMap((name) => {
      const path = join(dir, name);
      if (statSync(path).isDirectory()) return walk(path);
      return /\.(ts|tsx)$/.test(name) ? [path] : [];
    });
  for (const path of walk(srcRoot)) {
    if (allowed.has(path)) continue;
    const text = readFileSync(path, "utf8");
    expect(text, path).not.toMatch(/\bnew\s+(?:window\.)?EventSource/);
    expect(text, path).not.toMatch(/\.GET\(\s*["'`]\/sync/);
  }
});
