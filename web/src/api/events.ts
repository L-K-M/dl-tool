import { useCallback, useEffect, useRef } from "react";
import { useQueryClient, type QueryClient } from "@tanstack/react-query";
import i18next from "i18next";
import { toast } from "sonner";
import { create } from "zustand";

import { invalidateTaskList } from "../components/TaskGrid/TaskGrid";
import {
  useTasks,
  type SyncMessage,
  type Task,
  type TasksState,
} from "../store/useTasks";
import { api, eventsUrl } from "./client";

/** Reconnect delays in seconds, doc 09 section 10.8 rule 3; the last value repeats. */
export const BACKOFF_SECONDS = [1, 2, 4, 8, 15, 30] as const;

/** Consecutive stream failures after which the client polls instead, doc 05 section 6.1. */
export const POLL_AFTER_FAILURES = 3;

/** Polling period in milliseconds. Doc 05 section 6.1 and doc 09 section 10.8 rule 4 both say 2 s. */
export const POLL_INTERVAL_MS = 2000;

/** Silence after which the connection reads offline: two 15 s heartbeats, doc 09 section 10.8 rule 1. */
export const OFFLINE_AFTER_MS = 30_000;

/** Doc 09 section 10.8 rule 2: the banner waits five seconds so a blip never flashes it. */
const BANNER_AFTER_MS = 5_000;
const TICK_MS = 1_000;
const unauthorized = 401;

type Connection = TasksState["connection"];

export interface Transport {
  /** Opens the stream and keeps it open until stop(). Safe to call once per session. */
  start(): void;
  stop(): void;
  /** Forces an immediate reconnect attempt, used by the banner's Retry now button. */
  retryNow(): void;
}

interface TransportUiState {
  /** Doc 09 section 10.8 rule 7: a 401 swaps the reconnect banner for the session banner. */
  unauthenticated: boolean;
  /** Rule 2: the transport has been down five seconds, or silence passed the amber threshold. */
  banner: boolean;
  /** Seconds until the next scheduled stream retry; null when none is pending. */
  nextRetryIn: number | null;
}

/** What the banner sees of the transport; createTransport is the only writer. */
export const useTransportUi = create<TransportUiState>(() => ({
  unauthenticated: false,
  banner: false,
  nextRetryIn: null,
}));

/** One transport per app. onSync receives every payload, whether it arrived by SSE or by polling. */
export function createTransport(opts: {
  onSync: (msg: SyncMessage) => void;
  onConnection: (c: Connection) => void;
  onUnauthenticated: () => void;
  queryClient: QueryClient;
}): Transport {
  let source: EventSource | null = null;
  let started = false;
  let unauthenticated = false;
  let failures = 0;
  // The rid is kept only for the polling URL: on the stream itself EventSource
  // resends Last-Event-ID, which doc 05 section 6.1 defines as the rid.
  let lastRid = 0;
  let lastActivity = 0;
  let downAt: number | null = null;
  let needsRecovery = false;
  let connection: Connection = "connecting";
  let pollTimer: ReturnType<typeof setInterval> | null = null;
  let probing = false;
  let retryTimer: ReturnType<typeof setTimeout> | null = null;
  let retryAt: number | null = null;
  let tickTimer: ReturnType<typeof setInterval> | null = null;

  function setConnection(next: Connection): void {
    if (connection === next) return;
    connection = next;
    opts.onConnection(next);
  }

  function deliver(msg: SyncMessage): void {
    // A delta older than the last applied rid would regress fields — stream
    // deltas from a reopened connection can arrive after the rid=0 snapshot.
    if (!msg.full_update && !msg.seq_gap && msg.rid <= lastRid) return;
    lastRid = msg.rid;
    opts.onSync(msg);
  }

  type FetchResult = "ok" | "unauthenticated" | "failed";

  async function fetchSync(rid: number): Promise<FetchResult> {
    try {
      const { data, response } = await api.GET("/sync", {
        params: { query: { rid } },
      });
      if (response.status === unauthorized) return "unauthenticated";
      if (!data) return "failed";
      lastActivity = Date.now();
      deliver(data);
      return "ok";
    } catch {
      return "failed";
    }
  }

  /** EventSource cannot see a 401, so every outage is probed through /sync. */
  async function probe(): Promise<void> {
    // A hung request must not pile up under the 2 s poller and starve the
    // socket a reconnect needs.
    if (probing) return;
    probing = true;
    try {
      const result = await fetchSync(lastRid);
      if (result === "unauthenticated") {
        authLost();
        return;
      }
      // Data through the fallback lifts an amber read, but only the stream
      // itself can end the outage.
      if (result === "ok" && connection === "offline") {
        setConnection(pollTimer !== null ? "polling" : "connecting");
      }
    } finally {
      probing = false;
    }
  }

  /** Rule 5: a reconnect refetches the world rather than replaying deltas. */
  async function refetchAll(): Promise<void> {
    const result = await fetchSync(0);
    if (result === "unauthenticated") {
      authLost();
      return;
    }
    // The full snapshot travels through onSync, where its full_update flag
    // already invalidates the task list; no second invalidation is needed.
    if (result === "ok") {
      toast.success(i18next.t("shell.reconnected"));
    }
  }

  function clearRetry(): void {
    if (retryTimer !== null) clearTimeout(retryTimer);
    retryTimer = null;
    retryAt = null;
    useTransportUi.setState({ nextRetryIn: null });
  }

  function startPoller(): void {
    if (pollTimer !== null) return;
    pollTimer = setInterval(() => void probe(), POLL_INTERVAL_MS);
  }

  function stopPoller(): void {
    if (pollTimer !== null) clearInterval(pollTimer);
    pollTimer = null;
  }

  function streamError(): void {
    // The ladder owns reconnect timing; EventSource's own retry would bypass it.
    source?.close();
    source = null;
    failures += 1;
    downAt ??= Date.now();
    needsRecovery = true;
    if (failures >= POLL_AFTER_FAILURES) {
      startPoller();
      setConnection("polling");
    } else {
      setConnection("connecting");
    }
    scheduleRetry();
    void probe();
  }

  function scheduleRetry(): void {
    clearRetry();
    const delay =
      BACKOFF_SECONDS[Math.min(failures - 1, BACKOFF_SECONDS.length - 1)] *
      TICK_MS;
    retryAt = Date.now() + delay;
    retryTimer = setTimeout(connect, delay);
    useTransportUi.setState({ nextRetryIn: Math.ceil(delay / TICK_MS) });
  }

  function connect(): void {
    if (!started || unauthenticated) return;
    clearRetry();
    source?.close();
    source = null;
    let stream: EventSource;
    try {
      stream = new EventSource(eventsUrl(), { withCredentials: true });
    } catch {
      // A missing or refused EventSource takes the same failure path as a
      // dropped stream; the ladder and the polling fallback still apply.
      streamError();
      return;
    }
    source = stream;
    stream.addEventListener("sync", (event) => {
      try {
        deliver(JSON.parse((event as MessageEvent).data) as SyncMessage);
      } catch {
        // A malformed event cannot be applied; it still proves liveness.
      }
      alive();
    });
    // Doc 05 section 6.1: hb is the client-observable keep-alive. The comment
    // line that precedes it is invisible to EventSource, so it cannot count.
    stream.addEventListener("hb", () => alive());
    stream.addEventListener("error", () => {
      if (source === stream) streamError();
    });
  }

  function alive(): void {
    lastActivity = Date.now();
    if (!needsRecovery) {
      setConnection("live");
      return;
    }
    needsRecovery = false;
    failures = 0;
    downAt = null;
    clearRetry();
    stopPoller();
    useTransportUi.setState({ banner: false, nextRetryIn: null });
    setConnection("live");
    void refetchAll();
  }

  function authLost(): void {
    if (unauthenticated) return;
    unauthenticated = true;
    source?.close();
    source = null;
    stopPoller();
    clearRetry();
    if (tickTimer !== null) {
      clearInterval(tickTimer);
      tickTimer = null;
    }
    useTransportUi.setState({ banner: false, nextRetryIn: null });
    opts.onUnauthenticated();
  }

  function tick(): void {
    if (unauthenticated) return;
    const t = Date.now();
    if (retryAt !== null) {
      useTransportUi.setState({
        nextRetryIn: Math.max(0, Math.ceil((retryAt - t) / TICK_MS)),
      });
    }
    const silent = t - lastActivity >= OFFLINE_AFTER_MS;
    // Rule 2's ladder is monotonic: amber never fires after the banner, so a
    // stream that errored into the banner does not regress to offline later.
    if (
      silent &&
      !useTransportUi.getState().banner &&
      connection !== "offline"
    ) {
      needsRecovery = true;
      setConnection("offline");
      // A silently dead stream (NAT timeout, sleep/wake, hung server) never
      // fires an error event, so force a fresh connection; if it fails, the
      // ladder and the polling fallback take over from there.
      connect();
    }
    if (silent || (downAt !== null && t - downAt >= BANNER_AFTER_MS)) {
      useTransportUi.setState({ banner: true });
    }
  }

  return {
    start() {
      if (started) return;
      started = true;
      unauthenticated = false;
      failures = 0;
      lastRid = 0;
      downAt = null;
      needsRecovery = false;
      lastActivity = Date.now();
      connection = "connecting";
      opts.onConnection("connecting");
      useTransportUi.setState({
        unauthenticated: false,
        banner: false,
        nextRetryIn: null,
      });
      connect();
      tickTimer = setInterval(tick, TICK_MS);
    },
    stop() {
      started = false;
      source?.close();
      source = null;
      stopPoller();
      clearRetry();
      if (tickTimer !== null) {
        clearInterval(tickTimer);
        tickTimer = null;
      }
      useTransportUi.setState({ banner: false, nextRetryIn: null });
    },
    retryNow() {
      connect();
    },
  };
}

/** Mounts the transport for the lifetime of the authenticated shell. */
export function useEventStream(): {
  retryNow: () => void;
  nextRetryIn: number | null;
} {
  const queryClient = useQueryClient();
  const ref = useRef<Transport | null>(null);
  ref.current ??= createTransport({
    queryClient,
    onConnection: (c) => useTasks.getState().setConnection(c),
    onUnauthenticated: () => useTransportUi.setState({ unauthenticated: true }),
    onSync: (msg) => {
      const known = useTasks.getState().tasks;
      // State changes, inserts and removals rewrite the grid's server-side
      // row set, so the list query refetches (doc 09 section 10.8's sync row);
      // pure field patches are the reducer's business alone.
      const structural =
        msg.full_update ||
        msg.seq_gap ||
        (msg.tasks_removed?.length ?? 0) > 0 ||
        Object.entries(msg.tasks).some(
          ([id, patch]) =>
            !known.has(id) || (patch as Partial<Task>).state !== undefined,
        );
      useTasks.getState().applySync(msg);
      if (structural) void invalidateTaskList(queryClient);
    },
  });
  useEffect(() => {
    const transport = ref.current;
    transport?.start();
    return () => transport?.stop();
  }, []);
  const retryNow = useCallback(() => ref.current?.retryNow(), []);
  const nextRetryIn = useTransportUi((s) => s.nextRetryIn);
  return { retryNow, nextRetryIn };
}
