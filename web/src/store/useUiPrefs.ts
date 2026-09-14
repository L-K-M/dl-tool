import { create } from "zustand";

export const PREFS_KEY = "dl.ui.prefs.v1";
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
  /** Deep-merges a patch, then schedules one write 500 ms later. */
  patch: (p: Partial<UiPrefs>) => void;
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

/** The stored document, or null when absent or unparseable. */
function readStored(): Record<string, unknown> | null {
  try {
    const value: unknown = JSON.parse(
      localStorage.getItem(PREFS_KEY) ?? "null",
    );
    return isObject(value) ? value : null;
  } catch {
    return null;
  }
}

function loadInitial(): UiPrefs {
  const stored = readStored();
  if (!stored || stored.version !== 1) return clone(defaultPrefs);
  const merged = deepMerge(clone(defaultPrefs), stored);
  // Members with the wrong shape fall back to their default individually.
  if (!isObject(merged.grid)) merged.grid = clone(defaultPrefs.grid);
  const grid = merged.grid;
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
  return merged;
}

/** The document members, without the store's own functions. */
function documentOf(state: UiPrefs): UiPrefs {
  return {
    version: state.version,
    grid: state.grid,
    sidebarWidth: state.sidebarWidth,
    sidebarCollapsed: state.sidebarCollapsed,
    detailHeight: state.detailHeight,
    detailTab: state.detailTab,
    theme: state.theme,
    lastDestination: state.lastDestination,
  };
}

let dragging = false;
let writeTimer: ReturnType<typeof setTimeout> | null = null;

export const useUiPrefs = create<UiPrefsState>()((set, get) => {
  const scheduleWrite = () => {
    if (dragging) return;
    if (writeTimer !== null) clearTimeout(writeTimer);
    writeTimer = setTimeout(() => {
      writeTimer = null;
      // A gesture can start after this write was armed; doc 09 §3.3 forbids
      // writing mid-gesture. The gesture end reschedules with the final state.
      if (dragging) return;
      try {
        // Re-read at write time so members written by other owners since load
        // and unknown members survive verbatim. This store is the sole owner
        // of `grid`, so its known members replace storage (a resetGrid must
        // not resurrect stale sizing keys); nested members it does not define
        // survive. lib/theme.ts owns `theme`: a stored value wins.
        const stored = readStored() ?? {};
        const doc = documentOf(get());
        const merged: Record<string, unknown> = { ...stored, ...doc };
        merged.grid = {
          ...(isObject(stored.grid) ? stored.grid : {}),
          ...doc.grid,
        };
        const storedTheme = stored.theme;
        if (
          storedTheme === "system" ||
          storedTheme === "light" ||
          storedTheme === "dark"
        )
          merged.theme = storedTheme;
        localStorage.setItem(PREFS_KEY, JSON.stringify(merged));
      } catch {
        // Storage can be full or unavailable; the in-memory document still works.
      }
    }, WRITE_DEBOUNCE_MS);
  };
  return {
    ...loadInitial(),
    patch: (p) => {
      set((state) => deepMerge(state, p));
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
      // Replace, not deep-merge: a sizing key removed by the reset must not
      // survive as a stale member of the merged map.
      set({ grid: clone(defaultPrefs.grid) });
      scheduleWrite();
    },
  };
});
