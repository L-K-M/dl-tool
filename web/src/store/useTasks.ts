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

export const useTasks = create<TasksState>((set, get) => ({
  rid: 0,
  tasks: new Map(),
  stats: emptyStats(),
  selection: new Set(),
  connection: "connecting",
  applySync: (msg) =>
    set((state) => {
      const replace = msg.full_update || msg.seq_gap;
      const tasks = replace ? new Map<string, Task>() : new Map(state.tasks);
      for (const [id, value] of Object.entries(msg.tasks)) {
        // The generated map is unknown-valued; the wire contract supplies Task patches.
        const patch = value as Task;
        const previous = tasks.get(id);
        tasks.set(id, previous ? { ...previous, ...patch } : patch);
      }

      const selection = new Set(state.selection);
      for (const id of msg.tasks_removed ?? []) {
        tasks.delete(id);
        selection.delete(id);
      }
      // Snapshots express missed removals by absence, not tasks_removed.
      if (replace) {
        for (const id of selection) {
          if (!tasks.has(id)) selection.delete(id);
        }
      }
      return { tasks, selection, stats: msg.stats, rid: msg.rid };
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

export function selectFilterCounts(
  state: TasksState,
): Record<SidebarFilter, number> {
  const counts = {
    all: 0,
    downloading: 0,
    completed: 0,
    active: 0,
    inactive: 0,
    stopped: 0,
    error: 0,
  };
  for (const task of state.tasks.values()) {
    counts.all++;
    switch (task.state) {
      case "downloading":
        counts.downloading++;
        counts.active++;
        break;
      case "seeding":
        counts.active++;
        break;
      case "completed":
        counts.completed++;
        break;
      case "paused":
        counts.stopped++;
        counts.inactive++;
        break;
      case "queued":
        counts.inactive++;
        break;
      case "error":
        counts.error++;
        counts.inactive++;
        break;
    }
  }
  return counts;
}

export function selectCategoryCounts(
  state: TasksState,
): Map<string | null, number> {
  const counts = new Map<string | null, number>();
  for (const task of state.tasks.values()) {
    counts.set(task.category, (counts.get(task.category) ?? 0) + 1);
  }
  return counts;
}

export function selectTagCounts(state: TasksState): Map<string, number> {
  const counts = new Map<string, number>();
  for (const task of state.tasks.values()) {
    for (const tag of new Set(task.tags ?? [])) {
      counts.set(tag, (counts.get(tag) ?? 0) + 1);
    }
  }
  return counts;
}
