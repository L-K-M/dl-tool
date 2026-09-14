import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { PREFS_KEY } from "./useUiPrefs";
import { DEFAULT_COLUMN_ORDER } from "../components/TaskGrid/ColumnsMenu";

async function freshStore() {
  vi.resetModules();
  return await import("./useUiPrefs");
}

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  localStorage.clear();
});

test("TestDefaultsWhenStorageEmpty", async () => {
  const { useUiPrefs, defaultPrefs } = await freshStore();
  const state = useUiPrefs.getState();
  for (const key of Object.keys(defaultPrefs) as (keyof typeof defaultPrefs)[])
    expect(state[key]).toEqual(defaultPrefs[key]);
  expect(state.grid.order).toEqual([...DEFAULT_COLUMN_ORDER]);
});

test("TestCorruptStorageFallsBack", async () => {
  localStorage.setItem(PREFS_KEY, "{not json");
  let mod = await freshStore();
  expect(mod.useUiPrefs.getState().grid).toEqual(mod.defaultPrefs.grid);
  localStorage.setItem(
    PREFS_KEY,
    JSON.stringify({ version: 2, grid: { order: [] } }),
  );
  mod = await freshStore();
  expect(mod.useUiPrefs.getState().grid).toEqual(mod.defaultPrefs.grid);
  // A wrong-shaped member inside a v1 document falls back individually.
  localStorage.setItem(
    PREFS_KEY,
    JSON.stringify({ version: 1, grid: { order: "broken" } }),
  );
  mod = await freshStore();
  expect(mod.useUiPrefs.getState().grid.order).toEqual(
    mod.defaultPrefs.grid.order,
  );
});

test("TestPatchPreservesUnknownMembers", async () => {
  localStorage.setItem(
    PREFS_KEY,
    JSON.stringify({
      version: 1,
      grid: { order: ["size", "name"], future: { nested: true } },
      savedSearches: { keep: [1, 2] },
    }),
  );
  vi.useFakeTimers();
  const { useUiPrefs } = await freshStore();
  const state = useUiPrefs.getState();
  // Deep merge: patching one grid member leaves the rest intact.
  state.patch({
    grid: { ...state.grid, sizing: { name: 500 } },
    sidebarWidth: 300,
  });
  vi.advanceTimersByTime(600);
  const written = JSON.parse(localStorage.getItem(PREFS_KEY)!);
  expect(written.savedSearches).toEqual({ keep: [1, 2] });
  expect(written.grid.future).toEqual({ nested: true });
  expect(written.grid.order).toEqual(["size", "name"]);
  // Deep merge keeps the stored member's untouched keys.
  expect(written.grid.sizing).toEqual({ name: 500, destination: 240 });
  expect(written.sidebarWidth).toBe(300);
});

test("TestNoWriteDuringDrag", async () => {
  vi.useFakeTimers();
  const { useUiPrefs } = await freshStore();
  const spy = vi.spyOn(localStorage, "setItem");
  const state = useUiPrefs.getState();
  state.setDragging(true);
  state.patch({ sidebarWidth: 280 });
  vi.advanceTimersByTime(1000);
  expect(spy).not.toHaveBeenCalled();
  state.setDragging(false);
  vi.advanceTimersByTime(600);
  expect(spy).toHaveBeenCalledTimes(1);
  expect(JSON.parse(String(spy.mock.calls[0][1])).sidebarWidth).toBe(280);
});

test("TestSingleDebouncedWrite", async () => {
  vi.useFakeTimers();
  const { useUiPrefs } = await freshStore();
  const spy = vi.spyOn(localStorage, "setItem");
  const { patch } = useUiPrefs.getState();
  patch({ sidebarWidth: 260 });
  patch({ sidebarWidth: 270 });
  patch({ detailHeight: 100 });
  vi.advanceTimersByTime(499);
  expect(spy).not.toHaveBeenCalled();
  vi.advanceTimersByTime(1);
  expect(spy).toHaveBeenCalledTimes(1);
  expect(spy.mock.calls[0][0]).toBe(PREFS_KEY);
  const written = JSON.parse(String(spy.mock.calls[0][1]));
  expect(written.sidebarWidth).toBe(270);
  expect(written.detailHeight).toBe(100);
});

test("TestPendingWriteDoesNotFireDuringDrag", async () => {
  vi.useFakeTimers();
  const { useUiPrefs } = await freshStore();
  const spy = vi.spyOn(localStorage, "setItem");
  const state = useUiPrefs.getState();
  // A write armed before the gesture starts must not land mid-gesture.
  state.patch({ sidebarWidth: 280 });
  vi.advanceTimersByTime(100);
  state.setDragging(true);
  vi.advanceTimersByTime(1000);
  expect(spy).not.toHaveBeenCalled();
  state.setDragging(false);
  vi.advanceTimersByTime(600);
  expect(spy).toHaveBeenCalledTimes(1);
  expect(JSON.parse(String(spy.mock.calls[0][1])).sidebarWidth).toBe(280);
});

test("TestStoredThemeSurvivesPatchWrite", async () => {
  localStorage.setItem(PREFS_KEY, JSON.stringify({ version: 1 }));
  vi.useFakeTimers();
  const { useUiPrefs } = await freshStore();
  useUiPrefs.getState().patch({ sidebarWidth: 280 });
  // lib/theme.ts writes theme straight to storage without going through patch.
  localStorage.setItem(
    PREFS_KEY,
    JSON.stringify({ version: 1, theme: "dark" }),
  );
  vi.advanceTimersByTime(600);
  const written = JSON.parse(localStorage.getItem(PREFS_KEY)!);
  expect(written.theme).toBe("dark");
  expect(written.sidebarWidth).toBe(280);
});

test("TestResetGridRestoresColumnOrder", async () => {
  const { useUiPrefs, defaultPrefs } = await freshStore();
  const state = useUiPrefs.getState();
  state.patch({
    grid: {
      ...state.grid,
      order: [...DEFAULT_COLUMN_ORDER].reverse(),
      sorting: [],
    },
  });
  useUiPrefs.getState().resetGrid();
  expect(useUiPrefs.getState().grid.order).toEqual([...DEFAULT_COLUMN_ORDER]);
  expect(useUiPrefs.getState().grid).toEqual(defaultPrefs.grid);
});
