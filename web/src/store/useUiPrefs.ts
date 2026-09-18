import { create } from "zustand";

import { api } from "../api/client";

const WRITE_DEBOUNCE_MS = 500;

export interface UiPrefs {
  version: 1;
  grid: {
    order: string[];
    visibility: Record<string, boolean>;
    sizing: Record<string, number>;
    sorting: { id: string; desc: boolean }[];
    density: "comfortable" | "compact";
  };
  sidebarWidth: number;
  sidebarCollapsed: boolean;
  detailHeight: number;
  detailTab: string;
  theme: "system" | "light" | "dark";
  lastDestination: string | null;
}

/** The document shape of doc 09 section 3.3, verbatim. */
export const defaultPrefs: UiPrefs = {
  version: 1,
  grid: {
    order: [
      "select",
      "queuePos",
      "name",
      "size",
      "progress",
      "status",
      "dlSpeed",
      "ulSpeed",
      "eta",
      "peers",
      "ratio",
      "uploaded",
      "destination",
      "addedOn",
      "completedOn",
    ],
    visibility: { category: false, tags: false, user: false },
    sizing: { name: 420, destination: 240 },
    sorting: [{ id: "addedOn", desc: true }],
    density: "comfortable",
  },
  sidebarWidth: 220,
  sidebarCollapsed: false,
  detailHeight: 260,
  detailTab: "general",
  theme: "system",
  lastDestination: "/data/iso",
};

export interface UiPrefsState extends UiPrefs {
  // The document is open (doc 05 §11.4): members the typed surface does not
  // declare land here verbatim on hydrate and ride the PUT body unchanged.
  [member: string]: unknown;
  /** GET /prefs once the session is authenticated; declared members are
   *  shape-checked as loadInitial did, unknown members land in state
   *  verbatim, and a failed or absent document leaves the built-in defaults
   *  in place (doc 09 §3.3's accepted flash). */
  hydrate: () => Promise<void>;
  /** Deep-merges a patch, then schedules one write 500 ms later. */
  patch: (p: Omit<Partial<UiPrefs>, "version">) => void;
  /** Called by the grid on gesture end; a write is never scheduled during a drag. */
  setDragging: (dragging: boolean) => void;
  resetGrid: () => void;
}

const isObject = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const clone = <T>(value: T): T => JSON.parse(JSON.stringify(value)) as T;

/** Plain objects merge key by key; arrays and scalars replace. */
function deepMerge<T>(base: T, patch: unknown): T {
  if (!isObject(base) || !isObject(patch)) return base;
  const merged: Record<string, unknown> = { ...base };
  for (const [key, value] of Object.entries(patch)) {
    const current = merged[key];
    merged[key] =
      isObject(current) && isObject(value) ? deepMerge(current, value) : value;
  }
  return merged as T;
}

/** The known members of a stored document, shape-checked against the
 *  defaults member by member; unknown members pass through verbatim. A
 *  document whose version the SPA does not own is ignored whole, the rule
 *  loadInitial applied to the persisted copy. */
function sanitizeDoc(doc: Record<string, unknown>): Record<string, unknown> {
  // JSON round-trip: the typed defaults become the open record the document
  // is stored as.
  const base = JSON.parse(JSON.stringify(defaultPrefs)) as Record<
    string,
    unknown
  >;
  if (doc.version !== defaultPrefs.version) return base;
  const merged = deepMerge(base, doc);
  // Members with the wrong shape fall back to their default individually.
  if (!isObject(merged.grid)) merged.grid = clone(defaultPrefs.grid);
  const grid = merged.grid as UiPrefs["grid"];
  if (
    !Array.isArray(grid.order) ||
    grid.order.some((id) => typeof id !== "string")
  )
    grid.order = [...defaultPrefs.grid.order];
  if (!isObject(grid.visibility))
    grid.visibility = { ...defaultPrefs.grid.visibility };
  if (!isObject(grid.sizing)) grid.sizing = { ...defaultPrefs.grid.sizing };
  if (
    !Array.isArray(grid.sorting) ||
    grid.sorting.some(
      (entry) => !isObject(entry) || typeof entry.id !== "string",
    )
  )
    grid.sorting = clone(defaultPrefs.grid.sorting);
  if (grid.density !== "compact" && grid.density !== "comfortable")
    grid.density = "comfortable";
  if (merged.theme !== "light" && merged.theme !== "dark")
    merged.theme = "system";
  if (typeof merged.sidebarWidth !== "number")
    merged.sidebarWidth = defaultPrefs.sidebarWidth;
  if (typeof merged.sidebarCollapsed !== "boolean")
    merged.sidebarCollapsed = defaultPrefs.sidebarCollapsed;
  if (typeof merged.detailHeight !== "number")
    merged.detailHeight = defaultPrefs.detailHeight;
  if (typeof merged.detailTab !== "string")
    merged.detailTab = defaultPrefs.detailTab;
  if (
    merged.lastDestination !== null &&
    typeof merged.lastDestination !== "string"
  )
    merged.lastDestination = defaultPrefs.lastDestination;
  return merged;
}

/** The PUT body: every state member that is not a function. Transient
 *  bookkeeping never enters the state object, so nothing else is excluded —
 *  unknown members hydrated from the server or written through patch under a
 *  cast round-trip verbatim (doc 05 §11.4). */
function documentOf(state: UiPrefsState): Record<string, unknown> {
  const doc: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(state)) {
    if (typeof value !== "function") doc[key] = value;
  }
  return doc;
}

let dragging = false;
let writeTimer: ReturnType<typeof setTimeout> | null = null;
// Top-level members patch or resetGrid touched since the last completed
// write; hydrate merges the server document beneath it, so a local edit made
// while the GET is in flight is never clobbered (doc 09 §3.3).
const dirty = new Set<string>();
// Completed PUTs, monotonic. A hydrate response that lands after a write it
// predates is discarded and re-issued once, so a stale server snapshot cannot
// revert a member the PUT already wrote.
let writeSerial = 0;

export const useUiPrefs = create<UiPrefsState>()((set, get) => {
  const scheduleWrite = () => {
    if (dragging) return;
    if (writeTimer !== null) clearTimeout(writeTimer);
    writeTimer = setTimeout(() => {
      writeTimer = null;
      // A gesture can start after this write was armed; doc 09 §3.3 forbids
      // writing mid-gesture. The gesture end reschedules with the final state.
      if (dragging) return;
      // Snapshot the dirty set so a patch landing mid-request stays dirty
      // and is not cleared by this write's completion.
      const written = new Set(dirty);
      const body = documentOf(get());
      void api
        .PUT("/prefs", { body })
        .then(({ error }) => {
          if (error) return;
          writeSerial += 1;
          for (const key of written) dirty.delete(key);
        })
        .catch(() => {
          // A failed PUT leaves the in-memory document in place and its
          // members dirty; the next patch's write carries them.
        });
    }, WRITE_DEBOUNCE_MS);
  };

  const fetchAndMerge = async (retried: boolean): Promise<void> => {
    const stamped = writeSerial;
    let data: unknown;
    try {
      ({ data } = await api.GET("/prefs"));
    } catch {
      // A failed or absent document leaves the built-in defaults in place.
      return;
    }
    if (!isObject(data)) return;
    if (writeSerial !== stamped) {
      // The snapshot can predate a completed PUT; discard it and re-issue
      // the GET once. A second race leaves the local document — it is what
      // the last PUT wrote, so it is already the server's truth.
      if (!retried) await fetchAndMerge(true);
      return;
    }
    const merged = sanitizeDoc(data);
    const applied: Record<string, unknown> = {};
    for (const [key, value] of Object.entries(merged)) {
      if (!dirty.has(key)) applied[key] = value;
    }
    set(applied);
  };

  return {
    ...clone(defaultPrefs),
    hydrate: () => fetchAndMerge(false),
    patch: (p) => {
      for (const key of Object.keys(p)) dirty.add(key);
      // Clone so the store never shares a mutable reference with the caller.
      set((state) => deepMerge(state, clone(p)));
      scheduleWrite();
    },
    setDragging: (next) => {
      if (next === dragging) return;
      dragging = next;
      if (next) {
        // A write armed before the gesture must not fire mid-gesture.
        if (writeTimer !== null) {
          clearTimeout(writeTimer);
          writeTimer = null;
        }
      } else {
        // A drag's patches were suppressed; gesture end flushes the state.
        scheduleWrite();
      }
    },
    resetGrid: () => {
      dirty.add("grid");
      // Replace, not deep-merge: a sizing key removed by the reset must not
      // survive as a stale member of the merged map.
      set({ grid: clone(defaultPrefs.grid) });
      scheduleWrite();
    },
  };
});
