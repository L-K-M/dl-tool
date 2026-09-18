import { create } from "zustand";

import { api } from "../api/client";

const WRITE_DEBOUNCE_MS = 500;

/** The saved-search cap the document enforces at both the write path
 *  (SavedSearches refuses the 51st) and the sanitize boundary. */
export const MAX_SAVED_SEARCHES = 50;
export const MAX_SAVED_NAME_LENGTH = 64;

export interface SavedSearch {
  id: string; // crypto.randomUUID()
  name: string; // 1..64 characters, unique within the document
  query: string;
  indexerIds: string[];
  categories: number[];
  createdAt: string; // RFC 3339
  lastTotal: number; // total of the last run, for the "new since last view" badge
}

export interface SearchPrefs {
  indexerIds: string[];
  categories: number[];
  saved: SavedSearch[]; // at most 50; saving a 51st is refused with a toast
}

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
  search: SearchPrefs;
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
  search: { indexerIds: [], categories: [], saved: [] },
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
  /** Transient session bookkeeping, never serialized into the document:
   *  true once a GET response has been applied. Writes are gated on it —
   *  a PUT built from pre-hydrate defaults would erase stored members. */
  hydrated: boolean;
  /** Deep-merges a patch, then schedules one write 500 ms later. */
  patch: (p: Omit<Partial<UiPrefs>, "version">) => void;
  /** Called by the grid on gesture end; a write is never scheduled during a drag. */
  setDragging: (dragging: boolean) => void;
  resetGrid: () => void;
  /** Session end counterpart to hydrate: restores the defaults, cancels any
   *  armed write and drops every member — including unknown ones — so one
   *  account's document cannot bleed into the next session. */
  reset: () => void;
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
  if (!isObject(merged.search)) merged.search = clone(defaultPrefs.search);
  const search = merged.search as SearchPrefs;
  if (
    !Array.isArray(search.indexerIds) ||
    search.indexerIds.some((id) => typeof id !== "string")
  )
    search.indexerIds = [];
  else search.indexerIds = [...new Set(search.indexerIds)];
  if (
    !Array.isArray(search.categories) ||
    search.categories.some((id) => typeof id !== "number")
  )
    search.categories = [];
  else search.categories = [...new Set(search.categories)];
  // Consumers key saved entries off id — the finish effect writes lastTotal
  // through a map keyed on it — so a duplicated id keeps its first entry.
  const seenSavedIds = new Set<string>();
  search.saved = Array.isArray(search.saved)
    ? search.saved
        .filter(
          (entry): entry is SavedSearch =>
            isObject(entry) &&
            typeof entry.id === "string" &&
            typeof entry.name === "string" &&
            entry.name.trim().length >= 1 &&
            entry.name.length <= MAX_SAVED_NAME_LENGTH &&
            typeof entry.query === "string" &&
            Array.isArray(entry.indexerIds) &&
            entry.indexerIds.every((id) => typeof id === "string") &&
            Array.isArray(entry.categories) &&
            entry.categories.every((id) => typeof id === "number") &&
            typeof entry.createdAt === "string" &&
            typeof entry.lastTotal === "number",
        )
        .filter((entry) => {
          if (seenSavedIds.has(entry.id)) return false;
          seenSavedIds.add(entry.id);
          return true;
        })
        // The same corruption vector hits the per-entry selections, which a
        // re-run posts verbatim without passing back through this boundary.
        .map((entry) => ({
          ...entry,
          indexerIds: [...new Set(entry.indexerIds)],
          categories: [...new Set(entry.categories)],
        }))
        .slice(0, MAX_SAVED_SEARCHES)
    : [];
  return merged;
}

/** The PUT body: every state member that is not a function and not
 *  transient bookkeeping. Unknown members hydrated from the server or
 *  written through patch under a cast round-trip verbatim (doc 05 §11.4). */
function documentOf(state: UiPrefsState): Record<string, unknown> {
  const doc: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(state)) {
    if (key === "hydrated") continue;
    if (typeof value !== "function") doc[key] = value;
  }
  return doc;
}

const WRITE_RETRY_MS = 2_000;

let dragging = false;
let writeTimer: ReturnType<typeof setTimeout> | null = null;
let retryTimer: ReturnType<typeof setTimeout> | null = null;
let putInFlight = false;
// Top-level members patch or resetGrid touched since the last completed
// write; hydrate merges the server document beneath it, so a local edit made
// while the GET is in flight is never clobbered (doc 09 §3.3).
const dirty = new Set<string>();
// Completed PUTs, monotonic. A hydrate response that lands after a write it
// predates is discarded and re-issued once, so a stale server snapshot cannot
// revert a member the PUT already wrote.
let writeSerial = 0;
// Session generation, bumped by reset(). A GET or PUT that was already over
// the wire when the session ended is discarded on settle, so one account's
// document cannot merge, clear dirtiness, or arm a retry into the next.
let sessionGen = 0;

export const useUiPrefs = create<UiPrefsState>()((set, get) => {
  const scheduleWrite = (isRetry = false) => {
    if (dragging) return;
    if (writeTimer !== null) clearTimeout(writeTimer);
    writeTimer = setTimeout(() => {
      writeTimer = null;
      // A gesture can start after this write was armed; doc 09 §3.3 forbids
      // writing mid-gesture. The gesture end reschedules with the final state.
      if (dragging) return;
      if (!get().hydrated) {
        // A PUT built from pre-hydrate defaults would erase every stored
        // member, so the write re-attempts hydration instead; a successful
        // merge flushes the dirty members. Members stay dirty either way.
        void fetchAndMerge(true);
        return;
      }
      // PUTs are serialized: a write fired while one is still on the wire
      // would let the older body land after the newer. The members stay
      // dirty and the in-flight settle re-arms the flush below.
      if (putInFlight) return;
      // Snapshot the dirty set so a patch landing mid-request stays dirty
      // and is not cleared by this write's completion.
      const written = new Set(dirty);
      const gen = sessionGen;
      const body = documentOf(get());
      putInFlight = true;
      void api
        .PUT("/prefs", { body })
        .then(({ error }) => {
          // The session ended mid-flight: the next session owns
          // putInFlight now, so the stale settle touches nothing.
          if (gen !== sessionGen) return;
          putInFlight = false;
          if (error) {
            retryWrite(isRetry);
            return;
          }
          writeSerial += 1;
          for (const key of written) dirty.delete(key);
          // Members patched mid-flight are still dirty; flush them now so
          // a newer document is never held behind an older one.
          if (dirty.size > 0) scheduleWrite();
        })
        .catch(() => {
          if (gen !== sessionGen) return;
          putInFlight = false;
          retryWrite(isRetry);
        });
    }, WRITE_DEBOUNCE_MS);
  };

  const retryWrite = (isRetry: boolean) => {
    // A failed PUT leaves the in-memory document in place and its members
    // dirty. One delayed retry covers a transient blip; a retry that fails
    // does not arm another — the next patch's write still carries them.
    if (isRetry) return;
    retryTimer = setTimeout(() => {
      retryTimer = null;
      if (dirty.size > 0) scheduleWrite(true);
    }, WRITE_RETRY_MS);
  };

  const fetchAndMerge = async (retried: boolean): Promise<void> => {
    const gen = sessionGen;
    const stamped = writeSerial;
    let data: unknown;
    let failed = false;
    try {
      const response = await api.GET("/prefs");
      if (response.error) failed = true;
      else data = response.data;
    } catch {
      failed = true;
    }
    // The session ended while the GET was on the wire; the previous
    // account's snapshot must not merge into the fresh post-reset state.
    if (gen !== sessionGen) return;
    if (failed) {
      // The document's state is unknown, so writes stay gated — a PUT
      // built from defaults would erase every stored member. One
      // immediate retry bounds a transient failure; a later write
      // re-attempts hydration through scheduleWrite.
      if (!retried) await fetchAndMerge(true);
      return;
    }
    // A non-object body is not a preference document, and a document whose
    // version the SPA does not own is ignored whole (doc 09 §3.3): neither
    // is applied and writes stay gated, so a newer client's members are
    // never flattened. An absent version is the empty first-run document,
    // which is owned by definition.
    if (!isObject(data)) return;
    if (data.version !== undefined && data.version !== defaultPrefs.version)
      return;
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
    set({ ...applied, hydrated: true });
    // Patches made while the GET was in flight stay dirty; flush them now
    // that the document the server holds is reflected in state.
    if (dirty.size > 0) scheduleWrite();
  };

  const actions = {
    hydrate: () => fetchAndMerge(false),
    patch: (p: Omit<Partial<UiPrefs>, "version">) => {
      for (const key of Object.keys(p)) dirty.add(key);
      // Clone so the store never shares a mutable reference with the caller.
      set((state) => deepMerge(state, clone(p)));
      scheduleWrite();
    },
    setDragging: (next: boolean) => {
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
    reset: () => {
      if (writeTimer !== null) clearTimeout(writeTimer);
      if (retryTimer !== null) clearTimeout(retryTimer);
      writeTimer = retryTimer = null;
      putInFlight = false;
      dirty.clear();
      // A request already on the wire settles against the bumped
      // generation and is discarded rather than merged.
      sessionGen += 1;
      // Replace the whole state so the previous account's members —
      // unknown ones included — do not leak into the next session.
      set({ ...clone(defaultPrefs), hydrated: false, ...actions }, true);
    },
  };

  return { ...clone(defaultPrefs), hydrated: false, ...actions };
});
