import { create } from "zustand";
import type { paths } from "../api/schema";

export type Task =
  paths["/tasks/{id}"]["get"]["responses"][200]["content"]["application/json"];
export type SyncMessage =
  paths["/sync"]["get"]["responses"][200]["content"]["application/json"];
export type Stats = SyncMessage["stats"];
export type SidebarFilter =
  | "all"
  | "downloading"
  | "completed"
  | "active"
  | "inactive"
  | "stopped"
  | "error";

export interface TasksState {
  rid: number;
  tasks: ReadonlyMap<string, Task>;
  stats: Stats;
  selection: ReadonlySet<string>;
  /** Owned by the transport; data updates never change connectivity. */
  connection: "connecting" | "live" | "polling" | "offline";
  /** Ids the last applySync touched, including ids a snapshot dropped by absence. */
  changedIds: ReadonlySet<string>;
  /** Pre-update task per changed id (undefined for adds). Delta merges mutate
   *  the shared map in place — a 10k-entry clone per tick misses the scripting
   *  budget — so diff subscribers must read the old value here, not from the
   *  previous state. */
  changedFrom: ReadonlyMap<string, Task | undefined>;
  /** Sidebar aggregates maintained incrementally by applySync; per-tick deltas
   *  keep the previous reference when no counted field moved. */
  filterCounts: Record<SidebarFilter, number>;
  categoryCounts: ReadonlyMap<string | null, number>;
  tagCounts: ReadonlyMap<string, number>;
  uncategorisedCount: number;
  untaggedCount: number;
  applySync: (msg: SyncMessage) => void;
  hydrate: (tasks: Task[]) => void;
  setSelection: (ids: Iterable<string>) => void;
  clearSelection: () => void;
  setConnection: (connection: TasksState["connection"]) => void;
  reset: () => void;
}

const emptyStats = (): Stats => ({
  speed_down: 0,
  speed_up: 0,
  active: 0,
  queued: 0,
});

const emptyFilterCounts = (): Record<SidebarFilter, number> => ({
  all: 0,
  downloading: 0,
  completed: 0,
  active: 0,
  inactive: 0,
  stopped: 0,
  error: 0,
});

/** Buckets a task state feeds beyond "all"; unlisted states count nowhere else. */
const FILTER_BUCKETS: Partial<Record<Task["state"], readonly SidebarFilter[]>> =
  {
    downloading: ["downloading", "active"],
    seeding: ["active"],
    completed: ["completed"],
    paused: ["stopped", "inactive"],
    queued: ["inactive"],
    error: ["error", "inactive"],
  };

const bumpCount = (
  counts: Map<string | null, number>,
  key: string | null,
  delta: number,
) => {
  const value = (counts.get(key) ?? 0) + delta;
  if (value > 0) counts.set(key, value);
  else counts.delete(key);
};

interface CountSnapshot {
  filterCounts: Record<SidebarFilter, number>;
  categoryCounts: ReadonlyMap<string | null, number>;
  tagCounts: ReadonlyMap<string, number>;
  uncategorisedCount: number;
  untaggedCount: number;
}

/** Rebuilds every aggregate; snapshots are rare, so a full scan is fine. */
const recount = (tasks: ReadonlyMap<string, Task>): CountSnapshot => {
  const snapshot: CountSnapshot = {
    filterCounts: emptyFilterCounts(),
    categoryCounts: new Map(),
    tagCounts: new Map(),
    uncategorisedCount: 0,
    untaggedCount: 0,
  };
  const categoryCounts = snapshot.categoryCounts as Map<string | null, number>;
  const tagCounts = snapshot.tagCounts as Map<string, number>;
  for (const task of tasks.values()) {
    snapshot.filterCounts.all++;
    for (const bucket of FILTER_BUCKETS[task.state] ?? [])
      snapshot.filterCounts[bucket]++;
    bumpCount(categoryCounts, task.category, 1);
    if (!task.category) snapshot.uncategorisedCount++;
    let tagged = false;
    for (const tag of new Set(task.tags ?? [])) {
      bumpCount(tagCounts, tag, 1);
      tagged = true;
    }
    if (!tagged) snapshot.untaggedCount++;
  }
  return snapshot;
};

/** Applies only the changed ids to the previous aggregates, cloning each
 *  aggregate lazily so untouched ones keep their reference for subscribers. */
const adjustCounts = (
  state: TasksState,
  tasks: ReadonlyMap<string, Task>,
  changedIds: ReadonlySet<string>,
  changedFrom: ReadonlyMap<string, Task | undefined>,
): CountSnapshot => {
  let filtersDraft: Record<SidebarFilter, number> | null = null;
  let categoriesDraft: Map<string | null, number> | null = null;
  let tagsDraft: Map<string, number> | null = null;
  let uncategorisedCount = state.uncategorisedCount;
  let untaggedCount = state.untaggedCount;

  const moveFilters = (task: Task | undefined, delta: number) => {
    const buckets = task ? FILTER_BUCKETS[task.state] : undefined;
    if (!buckets?.length) return;
    filtersDraft ??= { ...state.filterCounts };
    for (const bucket of buckets) filtersDraft[bucket] += delta;
  };
  const moveCategory = (task: Task | undefined, delta: number) => {
    if (!task) return;
    categoriesDraft ??= new Map(state.categoryCounts);
    bumpCount(categoriesDraft, task.category, delta);
    if (!task.category) uncategorisedCount += delta;
  };
  const moveTags = (task: Task | undefined, delta: number) => {
    if (!task) return;
    if (!task.tags?.length) {
      untaggedCount += delta;
      return;
    }
    tagsDraft ??= new Map(state.tagCounts);
    for (const tag of new Set(task.tags)) bumpCount(tagsDraft, tag, delta);
  };

  for (const id of changedIds) {
    const previous = changedFrom.get(id);
    const next = tasks.get(id);
    if (previous === next) continue;
    if (previous?.state !== next?.state) {
      moveFilters(previous, -1);
      moveFilters(next, 1);
    }
    if (previous?.category !== next?.category) {
      moveCategory(previous, -1);
      moveCategory(next, 1);
    }
    if (previous?.tags !== next?.tags) {
      moveTags(previous, -1);
      moveTags(next, 1);
    }
  }

  const filterCounts =
    filtersDraft || state.filterCounts.all !== tasks.size
      ? { ...(filtersDraft ?? state.filterCounts), all: tasks.size }
      : state.filterCounts;
  return {
    filterCounts,
    categoryCounts: categoriesDraft ?? state.categoryCounts,
    tagCounts: tagsDraft ?? state.tagCounts,
    uncategorisedCount,
    untaggedCount,
  };
};

export const useTasks = create<TasksState>((set, get) => ({
  rid: 0,
  tasks: new Map(),
  stats: emptyStats(),
  selection: new Set(),
  connection: "connecting",
  changedIds: new Set(),
  changedFrom: new Map(),
  filterCounts: emptyFilterCounts(),
  categoryCounts: new Map(),
  tagCounts: new Map(),
  uncategorisedCount: 0,
  untaggedCount: 0,
  applySync: (msg) =>
    set((state) => {
      const replace = msg.full_update || msg.seq_gap;
      const source = state.tasks;
      const tasks = replace
        ? new Map<string, Task>()
        : (source as Map<string, Task>);
      const changedIds = new Set<string>();
      const changedFrom = new Map<string, Task | undefined>();
      for (const [id, value] of Object.entries(msg.tasks)) {
        // The generated map is unknown-valued; the wire contract supplies Task patches.
        const patch = value as Task;
        const previous = source.get(id);
        changedFrom.set(id, previous);
        tasks.set(id, previous ? { ...previous, ...patch } : patch);
        changedIds.add(id);
      }

      let selection = state.selection;
      const removal = (msg.tasks_removed?.length ?? 0) > 0 || replace;
      if (removal) {
        const pruned = new Set(state.selection);
        for (const id of msg.tasks_removed ?? []) {
          if (!changedFrom.has(id)) changedFrom.set(id, source.get(id));
          tasks.delete(id);
          pruned.delete(id);
          changedIds.add(id);
        }
        // Snapshots express missed removals by absence, not tasks_removed.
        if (replace) {
          for (const id of pruned) {
            if (!tasks.has(id)) pruned.delete(id);
          }
          // Any previously held id may vanish by absence, so a snapshot
          // invalidates every row, not only the ids it carries.
          for (const id of source.keys()) {
            if (!changedFrom.has(id)) changedFrom.set(id, source.get(id));
            changedIds.add(id);
          }
        }
        if (pruned.size !== state.selection.size) selection = pruned;
      }
      const counts = replace
        ? recount(tasks)
        : adjustCounts(state, tasks, changedIds, changedFrom);
      return {
        tasks,
        selection,
        stats: msg.stats,
        rid: msg.rid,
        changedIds,
        changedFrom,
        ...counts,
      };
    }),
  hydrate: (tasks) => {
    const state = get();
    state.applySync({
      rid: state.rid,
      full_update: false,
      seq_gap: false,
      tasks: Object.fromEntries(tasks.map((task) => [task.id, task])),
      tasks_removed: [],
      stats: state.stats,
    });
  },
  setSelection: (ids) => set({ selection: new Set(ids) }),
  clearSelection: () => set({ selection: new Set() }),
  setConnection: (connection) => set({ connection }),
  reset: () => {
    // Keep one task writer, including page hydration and logout cleanup.
    const state = get();
    state.applySync({
      rid: 0,
      full_update: true,
      seq_gap: false,
      tasks: {},
      tasks_removed: [],
      stats: emptyStats(),
    });
    state.clearSelection();
    state.setConnection("connecting");
  },
}));

export const selectTask =
  (id: string) =>
  (state: TasksState): Task | undefined =>
    state.tasks.get(id);
export const selectStats = (state: TasksState): Stats => state.stats;

export const selectFilterCounts = (
  state: TasksState,
): Record<SidebarFilter, number> => state.filterCounts;

export const selectCategoryCounts = (
  state: TasksState,
): ReadonlyMap<string | null, number> => state.categoryCounts;

export const selectTagCounts = (
  state: TasksState,
): ReadonlyMap<string, number> => state.tagCounts;
