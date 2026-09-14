import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import ts from "typescript";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import {
  useTasks,
  selectTask,
  selectStats,
  selectFilterCounts,
  selectCategoryCounts,
  selectTagCounts,
  type Task,
  type SyncMessage,
  type TasksState,
} from "./useTasks";

const task = (id: string, patch: Partial<Task> = {}): Task => ({
  id,
  name: id,
  engine: "aria2",
  source_kind: "http",
  source_uri: null,
  state: "queued",
  category: null,
  tags: [],
  destination: "",
  requested_destination: null,
  content_path: null,
  infohash_v1: null,
  infohash_v2: null,
  error_code: null,
  error_message: null,
  total_bytes: null,
  completed_bytes: 0,
  uploaded_bytes: 0,
  progress: 0,
  download_rate: 0,
  upload_rate: 0,
  eta_seconds: null,
  ratio: 0,
  total_peers: 0,
  connected_seeders: 0,
  connected_leechers: 0,
  dl_limit: 0,
  ul_limit: 0,
  ratio_limit: null,
  seeding_time_limit: null,
  sequential: false,
  queue_position: null,
  unzip_progress: null,
  file_count: null,
  added_at: "2026-09-01T00:00:00Z",
  updated_at: "2026-09-01T00:00:00Z",
  started_at: null,
  completed_at: null,
  ...patch,
});
const message = (patch: Partial<SyncMessage> = {}): SyncMessage => ({
  rid: 1,
  full_update: false,
  seq_gap: false,
  tasks: {},
  tasks_removed: [],
  stats: { speed_down: 100, speed_up: 20, active: 1, queued: 2 },
  ...patch,
});
const state = () => useTasks.getState();

beforeEach(() => state().reset());
afterEach(() => {
  vi.restoreAllMocks();
  useTasks.setState(useTasks.getInitialState(), true);
});

test("TestApplySyncFullUpdateReplacesMap", () => {
  state().hydrate([task("old")]);
  state().applySync(message({ rid: 10 }));
  const snapshot = task("new");
  const msg = message({ full_update: true, rid: 0, tasks: { new: snapshot } });
  state().applySync(msg);
  expect(state().tasks).toEqual(new Map([["new", snapshot]]));
  expect(state().rid).toBe(0);
  expect(selectStats(state())).toBe(msg.stats);
  expect(selectTask("old")(state())).toBeUndefined();
  expect(selectTask("new")(state())).toBe(snapshot);
});

test("TestApplySyncDeltaMergesFields", () => {
  const original = task("a", {
    name: "original",
    total_bytes: 500,
    tags: ["old"],
  });
  state().hydrate([original]);
  const before = state().tasks;
  const inserted = task("b");
  const msg = message({
    rid: 2,
    tasks: {
      a: { progress: 0.5, total_bytes: null, tags: ["new"] },
      b: inserted,
    },
  });
  state().applySync(msg);
  expect(selectTask("a")(state())).toEqual({
    ...original,
    progress: 0.5,
    total_bytes: null,
    tags: ["new"],
  });
  expect(selectTask("b")(state())).toBe(inserted);
  expect(state().tasks).not.toBe(before);
  expect(before.get("a")).toBe(original);
  expect(original.total_bytes).toBe(500);
  expect(state().rid).toBe(2);
  expect(state().stats).toBe(msg.stats);
});

test("TestApplySyncRemovesTasksAndSelection", () => {
  for (const flags of [{}, { full_update: true }, { seq_gap: true }]) {
    state().hydrate([task("a"), task("b")]);
    state().setSelection(["a", "b", "missing"]);
    const before = state();
    const msg = message({
      ...flags,
      tasks: { a: task("a"), b: task("b") },
      tasks_removed: ["a", "missing"],
    });
    state().applySync(msg);
    expect([...state().tasks.keys()]).toEqual(["b"]);
    expect(state().selection).toEqual(new Set(["b"]));
    expect(before.tasks.has("a")).toBe(true);
    expect(before.selection).toEqual(new Set(["a", "b", "missing"]));
    expect(state().stats).toBe(msg.stats);
  }
});

test("TestSeqGapReplacesMap", () => {
  state().hydrate([task("old"), task("same", { category: "stale" })]);
  state().applySync(message({ rid: 100 }));
  const replacement = task("same");
  state().applySync(
    message({ rid: 1, seq_gap: true, tasks: { same: replacement } }),
  );
  expect(state().rid).toBe(1);
  expect(state().tasks).toEqual(new Map([["same", replacement]]));
});

test("TestUnchangedTaskKeepsIdentity", () => {
  state().hydrate([task("a"), task("b")]);
  const before = state();
  state().applySync(
    message({ tasks: { a: { progress: 0.5 } }, tasks_removed: null }),
  );
  expect(selectTask("b")(state())).toBe(selectTask("b")(before));
  expect(selectTask("a")(state())).not.toBe(selectTask("a")(before));
  expect(selectTask("a")(before)?.progress).toBe(0);
});

test("TestReconnectFullUpdateReplacesMap", () => {
  state().applySync(
    message({
      rid: 50,
      full_update: true,
      tasks: { a: task("a"), b: task("b") },
    }),
  );
  state().applySync(message({ rid: 51, tasks: { a: { progress: 0.5 } } }));
  state().setConnection("offline");
  state().setConnection("connecting");
  const replacement = task("c");
  state().applySync(
    message({ rid: 60, full_update: true, tasks: { c: replacement } }),
  );
  expect(state().tasks).toEqual(new Map([["c", replacement]]));
  expect(state().rid).toBe(60);
  expect(state().connection).toBe("connecting");
});

test("TestSidebarCountsCoverEveryFilterAndState", () => {
  const empty = {
    all: 0,
    downloading: 0,
    completed: 0,
    active: 0,
    inactive: 0,
    stopped: 0,
    error: 0,
  };
  expect(selectFilterCounts(state())).toEqual(empty);
  for (const [status, membership] of [
    ["downloading", { downloading: 1, active: 1 }],
    ["seeding", { active: 1 }],
    ["completed", { completed: 1 }],
    ["error", { error: 1, inactive: 1 }],
    ["queued", { inactive: 1 }],
    ["paused", { stopped: 1, inactive: 1 }],
    ["checking", {}],
    ["extracting", {}],
    ["moving", {}],
    ["removed", {}],
  ] as const) {
    state().applySync(
      message({
        full_update: true,
        tasks: { a: task("a", { state: status }) },
      }),
    );
    expect(selectFilterCounts(state())).toEqual({
      ...empty,
      all: 1,
      ...membership,
    });
  }
  state().hydrate([
    task("b", { state: "downloading" }),
    task("c", { state: "downloading" }),
  ]);
  expect(selectFilterCounts(state())).toEqual({
    ...empty,
    all: 3,
    downloading: 2,
    active: 2,
  });
});

test("TestCategoryAndTagCounts", () => {
  expect(selectCategoryCounts(state())).toEqual(new Map());
  expect(selectTagCounts(state())).toEqual(new Map());
  state().hydrate([
    task("a", { category: "linux", tags: ["iso", "iso", "linux"] }),
    task("b", { category: "linux", tags: ["iso"] }),
    task("c", { tags: null }),
    task("d"),
  ]);
  expect(selectCategoryCounts(state())).toEqual(
    new Map([
      ["linux", 2],
      [null, 2],
    ]),
  );
  expect(selectTagCounts(state())).toEqual(
    new Map([
      ["iso", 2],
      ["linux", 1],
    ]),
  );
  state().applySync(
    message({
      tasks: { a: { category: null, tags: [] } },
      tasks_removed: ["b"],
    }),
  );
  expect(selectCategoryCounts(state())).toEqual(new Map([[null, 3]]));
  expect(selectTagCounts(state())).toEqual(new Map());
});

test("TestConnectionChangesOnlyThroughSetter", () => {
  expect(state().connection).toBe("connecting");
  for (const connection of [
    "live",
    "polling",
    "offline",
    "connecting",
  ] as const) {
    state().setConnection(connection);
    state().applySync(message({ full_update: true }));
    state().applySync(message({ seq_gap: true }));
    state().applySync(message());
    state().hydrate([task("a")]);
    state().setSelection(["a"]);
    state().clearSelection();
    expect(state().connection).toBe(connection);
  }
  const setter = vi.spyOn(state(), "setConnection");
  state().reset();
  expect(setter).toHaveBeenCalledWith("connecting");
});

test("TestHydrateAndResetDelegateTaskWritesToApplySync", () => {
  state().applySync(
    message({ rid: 42, tasks: { existing: task("existing") } }),
  );
  state().setConnection("live");
  const selected = new Set(["existing"]);
  state().setSelection(selected);
  selected.clear();
  expect(state().selection).toEqual(new Set(["existing"]));
  const before = state();
  const reducer = vi.spyOn(state(), "applySync");
  state().hydrate([task("a")]);
  expect(reducer).toHaveBeenCalledOnce();
  expect([...state().tasks.keys()]).toEqual(["existing", "a"]);
  expect(state().rid).toBe(42);
  expect(state().stats).toBe(before.stats);
  expect(state().connection).toBe("live");
  expect(state().selection).toEqual(before.selection);
  state().clearSelection();
  expect(state().selection.size).toBe(0);
  state().setSelection(["a"]);
  state().reset();
  expect(reducer).toHaveBeenCalledTimes(2);
  expect(state()).toMatchObject({
    rid: 0,
    tasks: new Map(),
    selection: new Set(),
    connection: "connecting",
    stats: { speed_down: 0, speed_up: 0, active: 0, queued: 0 },
  });
});

test("TestStoreHasNoTransportImportsOrExtraTaskWriters", () => {
  for (const path of [
    "src/store/useTasks.ts",
    "src/store/useTasks.test.ts",
    "src/lib/format.ts",
    "src/lib/format.test.ts",
  ]) {
    const source = ts.createSourceFile(
      path,
      readFileSync(
        resolve(dirname(fileURLToPath(import.meta.url)), "../..", path),
        "utf8",
      ),
      ts.ScriptTarget.Latest,
      true,
    );
    for (const statement of source.statements) {
      if (!ts.isImportDeclaration(statement)) continue;
      expect(statement.getText()).not.toMatch(/\b(EventSource|fetch)\b/);
      if (!statement.moduleSpecifier.getText().includes("/api/")) continue;
      expect(statement.importClause?.isTypeOnly).toBe(true);
      expect(statement.moduleSpecifier.getText()).toContain("/api/schema");
    }
  }
  // Exposed non-reducer operations must leave the task map untouched without the reducer.
  const before = state().tasks;
  vi.spyOn(state(), "applySync").mockImplementation(() => {});
  const actions: ((s: TasksState) => void)[] = [
    (s) => s.hydrate([task("a")]),
    (s) => s.reset(),
    (s) => s.setSelection(["a"]),
    (s) => s.clearSelection(),
    (s) => s.setConnection("live"),
    selectFilterCounts,
    selectCategoryCounts,
    selectTagCounts,
    selectStats,
    selectTask("a"),
  ];
  for (const action of actions) {
    action(state());
    expect(state().tasks).toBe(before);
    expect(state().tasks.size).toBe(0);
  }
});
