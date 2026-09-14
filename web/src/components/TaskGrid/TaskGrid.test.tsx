import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { HttpResponse, http } from "msw";
import { setupServer } from "msw/node";
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  expect,
  test,
  vi,
} from "vitest";
import {
  TaskGrid,
  DEFAULT_COLUMN_ORDER,
  STATUS_ORDINAL,
  invalidateTaskList,
} from "./TaskGrid";
import { useTasks, type Task } from "../../store/useTasks";
import { PREFS_KEY, defaultPrefs, useUiPrefs } from "../../store/useUiPrefs";
import { useShellUi } from "../Shell/Toolbar";
import { ColumnsMenu, moveGridColumn, useGridTable } from "./ColumnsMenu";
import { formatBytes } from "../../lib/format";

const task = (id: string, patch: Partial<Task> = {}): Task => ({
  id,
  name: id,
  engine: "aria2",
  source_kind: "http",
  source_uri: null,
  state: "queued",
  category: null,
  tags: [],
  destination: "/downloads/linux",
  requested_destination: null,
  content_path: null,
  infohash_v1: null,
  infohash_v2: null,
  error_code: null,
  error_message: null,
  total_bytes: 1024,
  completed_bytes: 512,
  uploaded_bytes: 0,
  progress: 0.5,
  download_rate: 1024,
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
const server = setupServer();
let qc: QueryClient;
let tasks: Task[];
let reportedTotal: number;
let requests: URL[];
let mobile = false;
let changeMedia: () => void;
const viewportHeight = 320;

beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
beforeEach(() => {
  useTasks.getState().reset();
  useShellUi.setState({
    nameFilter: "",
    debouncedFilter: "",
    pending: new Set(),
    filterInput: null,
  });
  useUiPrefs.setState(structuredClone(defaultPrefs));
  useGridTable.setState({ table: null });
  localStorage.clear();
  tasks = [task("one"), task("two"), task("three")];
  reportedTotal = tasks.length;
  requests = [];
  mobile = false;
  qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  const listeners = new Set<() => void>();
  vi.spyOn(window, "matchMedia").mockImplementation((query) => ({
    media: query,
    onchange: null,
    addEventListener: (
      _type: string,
      listener: EventListenerOrEventListenerObject,
    ) => listeners.add(listener as () => void),
    removeEventListener: (
      _type: string,
      listener: EventListenerOrEventListenerObject,
    ) => listeners.delete(listener as () => void),
    addListener: vi.fn(),
    removeListener: vi.fn(),
    dispatchEvent: () => true,
    get matches() {
      return mobile;
    },
  }));
  changeMedia = () => act(() => listeners.forEach((listener) => listener()));
  vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockReturnValue(
    viewportHeight,
  );
  vi.spyOn(HTMLElement.prototype, "offsetWidth", "get").mockReturnValue(1024);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(
    viewportHeight,
  );
  vi.spyOn(HTMLElement.prototype, "scrollHeight", "get").mockImplementation(
    function (this: HTMLElement) {
      return (
        Number.parseFloat(
          this.firstElementChild instanceof HTMLElement
            ? this.firstElementChild.style.height
            : "0",
        ) || viewportHeight
      );
    },
  );
  vi.spyOn(HTMLElement.prototype, "scrollTo").mockImplementation(function (
    this: HTMLElement,
    options: number | ScrollToOptions,
  ) {
    if (typeof options !== "object") return;
    this.scrollTop = options.top ?? 0;
    this.scrollLeft = options.left ?? 0;
    this.dispatchEvent(new Event("scroll"));
  });
  server.use(
    http.get("*/api/v1/tasks", ({ request }) => {
      const url = new URL(request.url);
      requests.push(url);
      const start = Number(url.searchParams.get("cursor") ?? 0);
      const limit = Number(url.searchParams.get("limit"));
      const end = start + limit;
      return HttpResponse.json({
        items: tasks.slice(start, end),
        total: start ? reportedTotal + 1 : reportedTotal,
        next_cursor: end < tasks.length ? String(end) : null,
      });
    }),
  );
});
afterEach(() => {
  cleanup();
  qc.clear();
  server.resetHandlers();
  vi.restoreAllMocks();
  localStorage.clear();
});
afterAll(() => server.close());

async function mount() {
  useTasks.getState().hydrate(tasks);
  const result = render(
    <QueryClientProvider client={qc}>
      <TaskGrid filter="all" />
    </QueryClientProvider>,
  );
  await screen.findByText(tasks[0].name, { exact: true });
  return result;
}
function row(id: string) {
  return document.querySelector<HTMLElement>(`[data-task-id="${id}"]`)!;
}
function selected() {
  return [...useTasks.getState().selection];
}
function order() {
  return [...document.querySelectorAll<HTMLElement>("[data-task-id]")].map(
    (node) => node.dataset.taskId,
  );
}
function columnIds() {
  return screen
    .getAllByRole("columnheader")
    .map((node) => node.getAttribute("data-column-id"));
}
/** Renders the toolbar's popover against the mounted grid's table. */
async function openColumnsMenu() {
  const table = useGridTable.getState().table;
  expect(table).not.toBeNull();
  render(<ColumnsMenu table={table!} />);
  fireEvent.click(screen.getByRole("button", { name: "Columns" }));
  await screen.findByRole("group", { name: "Columns" });
}

test("TestRendersDefaultColumns", async () => {
  await mount();
  const headers = screen.getAllByRole("columnheader");
  expect(headers).toHaveLength(15);
  expect(DEFAULT_COLUMN_ORDER).toEqual([
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
  ]);
  expect(headers.slice(1).map((node) => node.textContent)).toEqual([
    "#",
    "Name",
    "Size",
    "Progress",
    "Status",
    "Down",
    "Up",
    "ETA",
    "Seeds/Peers",
    "Ratio",
    "Uploaded",
    "Destination",
    "Added",
    "Completed",
  ]);
  expect(within(row("one")).getAllByRole("gridcell")[3].textContent).toBe(
    formatBytes(1024, "en"),
  );
  const progress = within(row("one")).getByRole("progressbar");
  expect(progress.getAttribute("aria-valuenow")).toBe("50");
  expect(progress.style.background).toBe("var(--progress-track)");
  expect(progress.getAttribute("aria-valuemin")).toBe("0");
  expect(progress.getAttribute("aria-valuemax")).toBe("100");
  expect(progress.getAttribute("aria-valuetext")).toBe(
    "50% — 512 byte of 1 kB",
  );
  expect(within(row("one")).getByTitle("/downloads/linux")).toBeTruthy();
});

test("TestStatusSortsByOrdinal", async () => {
  tasks = Object.keys(STATUS_ORDINAL)
    .reverse()
    .map((state) => task(state, { state }));
  reportedTotal = tasks.length;
  await mount();
  fireEvent.click(screen.getByRole("columnheader", { name: "Status" }));
  expect(order()).toEqual(Object.keys(STATUS_ORDINAL));
  expect(
    screen
      .getByRole("columnheader", { name: "Status" })
      .getAttribute("aria-sort"),
  ).toBe("ascending");
  fireEvent.click(screen.getByRole("columnheader", { name: "Status" }));
  expect(order()).toEqual(Object.keys(STATUS_ORDINAL).reverse());
});

test("TestShiftClickSelectsRange", async () => {
  await mount();
  fireEvent.click(row("one"));
  fireEvent.click(row("three"), { shiftKey: true });
  expect(selected()).toEqual(["one", "two", "three"]);
  expect(row("two").getAttribute("aria-selected")).toBe("true");
  expect(row("two").style.background).toContain("var(--accent)");
  fireEvent.click(row("two"), { ctrlKey: true });
  expect(selected()).toEqual(["one", "three"]);
  const header = screen.getByRole("checkbox", {
    name: "Select tasks",
  }) as HTMLInputElement;
  expect(header.indeterminate).toBe(true);
  fireEvent.click(header);
  expect(selected()).toEqual(["one", "two", "three"]);
  fireEvent.click(header);
  expect(selected()).toEqual([]);
  fireEvent.click(row("two"), { metaKey: true });
  expect(selected()).toEqual(["two"]);
});

test.each([false, true])(
  "TestRowCheckboxTogglesWithoutClearingOthers mobile=%s",
  async (isMobile) => {
    mobile = isMobile;
    await mount();
    const checkbox = (id: string) => within(row(id)).getByRole("checkbox");
    fireEvent.click(checkbox("one"));
    fireEvent.click(checkbox("two"));
    expect(selected()).toEqual(["one", "two"]);
    fireEvent.click(checkbox("one"));
    expect(selected()).toEqual(["two"]);
    fireEvent.click(checkbox("three"), { shiftKey: true });
    expect(selected()).toEqual(["one", "two", "three"]);
    expect(row("three").tabIndex).toBe(0);
  },
);

test("TestShrinkingPageKeepsVirtualIndicesInBounds", async () => {
  tasks = Array.from({ length: 100 }, (_, index) => task(`task-${index}`));
  reportedTotal = tasks.length;
  await mount();
  fireEvent.keyDown(screen.getByRole("grid"), { key: "End" });
  await waitFor(() => expect(row("task-99")).not.toBeNull());
  tasks = [task("replacement")];
  reportedTotal = 1;
  await act(() => invalidateTaskList(qc));
  expect(screen.getByRole("grid").getAttribute("aria-rowcount")).toBe("1");
  fireEvent.scroll(screen.getByRole("grid"), { target: { scrollTop: 0 } });
  await screen.findByText("replacement");
  expect(order()).toEqual(["replacement"]);
  tasks = [];
  reportedTotal = 0;
  await act(() => invalidateTaskList(qc));
  await waitFor(() =>
    expect(screen.getByRole("grid").getAttribute("aria-rowcount")).toBe("0"),
  );
  expect(order()).toEqual([]);
});

test("TestAriaRowcountIsTotalNotDomRows", async () => {
  tasks = Array.from({ length: 10000 }, (_, index) => task(`task-${index}`));
  reportedTotal = 10007;
  await mount();
  expect(useTasks.getState().tasks.size).toBe(10000);
  expect(screen.getByRole("grid").getAttribute("aria-rowcount")).toBe("10007");
  expect(screen.getAllByRole("row").length).toBeLessThan(40);
  expect(requests).toHaveLength(20);
  expect(
    requests.every(
      (url) =>
        url.searchParams.get("state") === "all" &&
        url.searchParams.get("limit") === "500",
    ),
  ).toBe(true);
  expect(requests[1].searchParams.get("cursor")).toBe("500");
  expect(screen.getByTestId("virtual-rows").style.height).toBe("320000px");
});

test("TestGridKeyboardNavigationAndSelection", async () => {
  tasks = Array.from({ length: 100 }, (_, index) => task(`task-${index}`));
  reportedTotal = tasks.length;
  await mount();
  const grid = screen.getByRole("grid");
  const key = async (value: string, options = {}) => {
    fireEvent.keyDown(
      document.activeElement?.closest('[role="grid"]')
        ? document.activeElement
        : grid,
      { key: value, ...options },
    );
    await act(async () => {});
  };
  const expectFocus = async (id: string) => {
    await waitFor(() =>
      expect(
        document.activeElement
          ?.closest("[data-task-id]")
          ?.getAttribute("data-task-id"),
      ).toBe(id),
    );
    expect(grid.querySelectorAll('[tabindex="0"]')).toHaveLength(1);
  };
  row("task-0").focus();
  await key("ArrowDown");
  await expectFocus("task-1");
  await key("ArrowUp");
  await expectFocus("task-0");
  await key("PageDown");
  await expectFocus("task-10");
  await key("PageUp");
  await expectFocus("task-0");
  await key("End");
  await expectFocus("task-99");
  await key("Home");
  await expectFocus("task-0");
  await key("End", { ctrlKey: true });
  await expectFocus("task-99");
  expect(document.activeElement?.getAttribute("aria-colindex")).toBe("15");
  await key("Home", { ctrlKey: true });
  await expectFocus("task-0");
  expect(document.activeElement?.getAttribute("aria-colindex")).toBe("1");
  await key(" ");
  expect(selected()).toEqual(["task-0"]);
  await key(" ");
  expect(selected()).toEqual([]);
  await key(" ", { shiftKey: true });
  await key(" ", { shiftKey: true });
  expect(selected()).toEqual(["task-0"]);
  await key("a", { ctrlKey: true });
  expect(selected()).toHaveLength(100);
  await key("Escape");
  expect(selected()).toEqual([]);
  await key("a", { metaKey: true });
  expect(selected()).toHaveLength(100);
  for (const tag of ["input", "textarea", "div"]) {
    const editable = document.createElement(tag);
    if (tag === "div") editable.contentEditable = "true";
    grid.appendChild(editable);
    const event = new KeyboardEvent("keydown", {
      key: "Escape",
      bubbles: true,
      cancelable: true,
    });
    act(() => {
      editable.dispatchEvent(event);
    });
    expect(event.defaultPrevented).toBe(false);
    expect(selected()).toHaveLength(100);
    editable.remove();
  }
  for (const [value, options] of [
    ["Enter", {}],
    ["F2", {}],
    ["Delete", {}],
    ["Delete", { shiftKey: true }],
    ["f", { ctrlKey: true }],
    ["f", { metaKey: true }],
    ["?", {}],
  ] as const) {
    const event = new KeyboardEvent("keydown", {
      key: value,
      bubbles: true,
      cancelable: true,
      ...options,
    });
    act(() => {
      grid.dispatchEvent(event);
    });
    expect(event.defaultPrevented).toBe(false);
  }
});

test("TestShellActionCallbacksDispatchOnce", async () => {
  const actions = {
    requestRemove: vi.fn(),
    focusFilter: vi.fn(),
    showShortcuts: vi.fn(),
  };
  useTasks.getState().hydrate(tasks);
  render(
    <QueryClientProvider client={qc}>
      <TaskGrid filter="all" actions={actions} />
    </QueryClientProvider>,
  );
  await screen.findByText("one", { exact: true });
  const grid = screen.getByRole("grid");
  // An empty selection never dispatches and never suppresses the key.
  const empty = new KeyboardEvent("keydown", {
    key: "Delete",
    bubbles: true,
    cancelable: true,
  });
  act(() => {
    grid.dispatchEvent(empty);
  });
  expect(empty.defaultPrevented).toBe(false);
  expect(actions.requestRemove).not.toHaveBeenCalled();
  act(() => useTasks.getState().setSelection(["one", "two"]));
  fireEvent.keyDown(grid, { key: "Delete" });
  expect(actions.requestRemove).toHaveBeenCalledTimes(1);
  expect(actions.requestRemove).toHaveBeenLastCalledWith(["one", "two"], false);
  // Held-key auto-repeat must not re-dispatch the removal flow.
  fireEvent.keyDown(grid, { key: "Delete", repeat: true });
  expect(actions.requestRemove).toHaveBeenCalledTimes(1);
  fireEvent.keyDown(grid, { key: "Delete", shiftKey: true });
  expect(actions.requestRemove).toHaveBeenCalledTimes(2);
  expect(actions.requestRemove).toHaveBeenLastCalledWith(["one", "two"], true);
  // Escape clears a live selection; with nothing selected it is a no-op and
  // leaves the key unsuppressed for any focused dialog.
  const cleared = new KeyboardEvent("keydown", {
    key: "Escape",
    bubbles: true,
    cancelable: true,
  });
  act(() => {
    grid.dispatchEvent(cleared);
  });
  expect(cleared.defaultPrevented).toBe(true);
  expect(useTasks.getState().selection.size).toBe(0);
  const unselected = new KeyboardEvent("keydown", {
    key: "Escape",
    bubbles: true,
    cancelable: true,
  });
  act(() => {
    grid.dispatchEvent(unselected);
  });
  expect(unselected.defaultPrevented).toBe(false);
  act(() => useTasks.getState().setSelection(["one", "two"]));
  // Pending rows announce and style the in-flight mutation.
  act(() => useShellUi.setState({ pending: new Set(["one"]) }));
  await waitFor(() =>
    expect(
      document.querySelector('[data-task-id="one"]')?.getAttribute("aria-busy"),
    ).toBe("true"),
  );
  expect(document.querySelector('[data-task-id="one"]')?.className).toContain(
    "task-pending",
  );
  act(() => useShellUi.setState({ pending: new Set() }));
  fireEvent.keyDown(grid, { key: "f", ctrlKey: true });
  fireEvent.keyDown(grid, { key: "f", metaKey: true });
  fireEvent.keyDown(grid, { key: "f" });
  expect(actions.focusFilter).toHaveBeenCalledTimes(2);
  fireEvent.keyDown(grid, { key: "?" });
  expect(actions.showShortcuts).toHaveBeenCalledTimes(1);
  // Editable targets never dispatch.
  const input = document.createElement("input");
  grid.appendChild(input);
  fireEvent.keyDown(input, { key: "Delete" });
  fireEvent.keyDown(input, { key: "f", ctrlKey: true });
  fireEvent.keyDown(input, { key: "?" });
  input.remove();
  expect(actions.requestRemove).toHaveBeenCalledTimes(2);
  expect(actions.focusFilter).toHaveBeenCalledTimes(2);
  expect(actions.showShortcuts).toHaveBeenCalledTimes(1);
  // A name filter that hides every row leaves the selection live; the shell
  // keys must still dispatch (doc 09 section 3.6 ownership rules).
  act(() => useShellUi.setState({ debouncedFilter: "zzz" }));
  await waitFor(() =>
    expect(document.querySelectorAll("[data-task-id]")).toHaveLength(0),
  );
  fireEvent.keyDown(grid, { key: "Delete" });
  expect(actions.requestRemove).toHaveBeenCalledTimes(3);
  expect(actions.requestRemove).toHaveBeenLastCalledWith(["one", "two"], false);
  fireEvent.keyDown(grid, { key: "f", ctrlKey: true });
  fireEvent.keyDown(grid, { key: "?" });
  expect(actions.focusFilter).toHaveBeenCalledTimes(3);
  expect(actions.showShortcuts).toHaveBeenCalledTimes(2);
  act(() => useShellUi.setState({ debouncedFilter: "" }));
});

test("TestDensityIsControlledNotCached", async () => {
  localStorage.setItem(
    "dl.ui.prefs.v1",
    JSON.stringify({ grid: { density: "compact" } }),
  );
  await mount();
  expect(row("one").style.height).toBe("32px");
});

test("TestRowHeightTracksLayoutAndDensity", async () => {
  tasks = Array.from({ length: 100 }, (_, index) => task(`task-${index}`));
  reportedTotal = tasks.length;
  const { rerender } = await mount();
  fireEvent.click(row("task-1"));
  for (const [isMobile, density, expected] of [
    [false, "comfortable", 32],
    [false, "compact", 26],
    [true, "compact", 160],
    [true, "comfortable", 160],
    [false, "comfortable", 32],
  ] as const) {
    mobile = isMobile;
    changeMedia();
    rerender(
      <QueryClientProvider client={qc}>
        <TaskGrid filter="all" density={density} />
      </QueryClientProvider>,
    );
    await waitFor(() =>
      expect(row("task-0").style.height).toBe(`${expected}px`),
    );
    expect(row("task-1").style.transform).toBe(`translateY(${expected}px)`);
    expect(screen.getByTestId("virtual-rows").style.height).toBe(
      `${100 * expected}px`,
    );
    expect(order().slice(0, 3)).toEqual(["task-0", "task-1", "task-2"]);
    expect(selected()).toEqual(["task-1"]);
  }
});

test.each([320, 375, 639])(
  "TestMobileContentBudgetAndTargets %s",
  async (width) => {
    mobile = true;
    vi.spyOn(HTMLElement.prototype, "offsetWidth", "get").mockReturnValue(
      width,
    );
    tasks = [
      task("a very long task name ".repeat(20).trim(), {
        state: "downloading",
        upload_rate: 2048,
        eta_seconds: 60,
      }),
    ];
    await mount();
    const content = screen.getByTestId("card-content");
    expect(content.style.gridTemplateRows).toBe("32px 16px 80px");
    expect(content.style.gap).toBe("8px");
    const cell = screen.getByRole("gridcell");
    expect(cell.style.padding).toBe("8px 0px");
    expect(cell.style.fontSize).toBe("14px");
    expect(cell.style.lineHeight).toBe("16px");
    const metadata = screen.getByTestId("card-metadata");
    expect(metadata.children).toHaveLength(5);
    expect(metadata.textContent).toContain("Downloading");
    expect(metadata.textContent).toContain("1 kB");
    expect(metadata.textContent).toContain("↓1 kB/s");
    expect(metadata.textContent).toContain("↑2 kB/s");
    expect(metadata.textContent).toContain("1m");
    for (const item of metadata.children) {
      expect((item as HTMLElement).style.whiteSpace).toBe("nowrap");
      expect((item as HTMLElement).style.display).toBe("flex");
    }
    const input = screen.getByRole("checkbox") as HTMLElement;
    expect(input.style.width).toBe("44px");
    expect(input.style.height).toBe("44px");
    expect(cell.style.minWidth).toBe("min-content");
    expect(content.firstElementChild?.getAttribute("title")).toBe(
      tasks[0].name,
    );
  },
);

test("TestMissingCellsAndErrorDetails", async () => {
  tasks = [
    task("empty", {
      total_bytes: null,
      progress: 0,
      download_rate: 0,
      destination: "",
      state: "error",
      error_code: "disk_full",
      error_message: "Not enough space",
    }),
  ];
  await mount();
  const cells = within(row("empty")).getAllByRole("gridcell");
  for (const index of [1, 3, 4, 6, 7, 9, 10, 11, 12, 14])
    expect(cells[index].textContent).toBe("—");
  expect(cells[8].textContent).toBe("∞");
  expect(cells[5].textContent).toContain("Error: disk_full");
  expect(within(cells[5]).getByTitle("Not enough space")).toBeTruthy();
  expect(cells[5].querySelector("svg")).not.toBeNull();
  act(() =>
    useTasks
      .getState()
      .hydrate([task("empty", { total_bytes: 0, eta_seconds: 0 })]),
  );
  expect(cells[3].textContent).toBe("—");
  expect(cells[8].textContent).toBe("—");
});

test("TestLiveCellsAndSortInvalidation", async () => {
  await mount();
  act(() => useTasks.getState().hydrate([task("one", { total_bytes: 4096 })]));
  expect(within(row("one")).getAllByRole("gridcell")[3].textContent).toBe(
    "4 kB",
  );
  fireEvent.click(screen.getByRole("columnheader", { name: "Name" }));
  act(() => useTasks.getState().hydrate([task("three", { name: "aaa" })]));
  expect(order()[0]).toBe("three");
  tasks.push(task("four"));
  reportedTotal++;
  await act(() => invalidateTaskList(qc));
  expect(screen.getByText("four")).toBeTruthy();
});

test("TestSecondarySortTracksLiveChanges", async () => {
  await mount();
  fireEvent.click(screen.getByRole("columnheader", { name: "Status" }));
  fireEvent.click(screen.getByRole("columnheader", { name: "Name" }), {
    shiftKey: true,
  });
  act(() => useTasks.getState().hydrate([task("one", { name: "zzz" })]));
  expect(order()).toEqual(["three", "two", "one"]);
});

test("TestSortReadsUnsortedLiveChanges", async () => {
  await mount();
  act(() => useTasks.getState().hydrate([task("one", { name: "zzz" })]));
  fireEvent.click(screen.getByRole("columnheader", { name: "Name" }));
  expect(order()).toEqual(["three", "two", "one"]);
});

test("TestTimestampSortUsesInstants", async () => {
  tasks = [
    task("later", { added_at: "2026-09-01T01:00:00Z" }),
    task("earlier", { added_at: "2026-09-01T02:00:00+02:00" }),
  ];
  await mount();
  const added = screen.getByRole("columnheader", { name: "Added" });
  // The persisted default sorts Added descending (doc 09 section 3.3).
  expect(added.getAttribute("aria-sort")).toBe("descending");
  expect(order()).toEqual(["later", "earlier"]);
  fireEvent.click(added);
  // The asc → desc → default cycle clears the sort back to server order.
  expect(order()).toEqual(["later", "earlier"]);
  expect(added.getAttribute("aria-sort")).toBe("none");
  fireEvent.click(added);
  expect(order()).toEqual(["earlier", "later"]);
  expect(added.getAttribute("aria-sort")).toBe("ascending");
});

test("TestGridOwnsHeaderAndRows", async () => {
  await mount();
  const grid = screen.getByRole("grid");
  const header = screen.getAllByRole("columnheader")[0].parentElement!;
  const ownership = grid.getAttribute("aria-owns")?.split(" ");
  expect(ownership).toEqual([header.id, screen.getByTestId("virtual-rows").id]);
  expect(header.id).not.toBe("");
});

test("TestPageFailureKeepsGridAndRetries", async () => {
  server.use(http.get("*/api/v1/tasks", () => HttpResponse.error()));
  render(
    <QueryClientProvider client={qc}>
      <TaskGrid filter="all" />
    </QueryClientProvider>,
  );
  await screen.findByRole("alert");
  expect(screen.getByRole("grid")).toBeTruthy();
  expect(useTasks.getState().tasks.size).toBe(0);
  server.use(
    http.get("*/api/v1/tasks", () =>
      HttpResponse.json({
        items: tasks,
        total: tasks.length,
        next_cursor: null,
      }),
    ),
  );
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await screen.findByText("one");
});

test("TestOnlyGridScrollsHorizontally", async () => {
  await mount();
  const scrollers = [...document.querySelectorAll<HTMLElement>("*")].filter(
    (node) => node.style.overflow === "auto",
  );
  expect(scrollers).toEqual([screen.getByRole("grid")]);
  const grid = screen.getByRole("grid");
  expect(grid.parentElement?.style.overflow).toBe("hidden");
  const cells = within(row("one")).getAllByRole("gridcell");
  expect(cells[0].style.position).toBe("sticky");
  expect(cells[0].style.left).toBe("0px");
  expect(cells[2].style.position).toBe("sticky");
  expect(cells[2].style.left).toBe("36px");
  fireEvent.scroll(grid, { target: { scrollLeft: 120 } });
  expect(
    screen.getAllByRole("columnheader")[0].parentElement?.style.transform,
  ).toBe("translateX(-120px)");
});

test("TestColumnPrefsSurviveRemount", async () => {
  const first = await mount();
  await openColumnsMenu();
  fireEvent.click(screen.getByRole("checkbox", { name: "Destination" }));
  fireEvent.click(screen.getByRole("button", { name: "Move Size down" }));
  const handle = document.querySelector('[data-resize-handle="size"]')!;
  fireEvent.mouseDown(handle, { clientX: 100 });
  fireEvent.mouseMove(document, { clientX: 150 });
  fireEvent.mouseUp(document);
  expect(useUiPrefs.getState().grid.sizing.size).toBe(140);
  const expected = [
    "select",
    "queuePos",
    "name",
    "progress",
    "size",
    "status",
    "dlSpeed",
    "ulSpeed",
    "eta",
    "peers",
    "ratio",
    "uploaded",
    "addedOn",
    "completedOn",
  ];
  expect(columnIds()).toEqual(expected);
  // The debounced write lands the whole document in localStorage.
  await waitFor(() =>
    expect(localStorage.getItem(PREFS_KEY)).toContain('"size":140'),
  );
  first.unmount();
  await mount();
  expect(columnIds()).toEqual(expected);
  expect(within(row("one")).getAllByRole("gridcell")).toHaveLength(14);
  expect(
    document
      .querySelector("section")!
      .style.getPropertyValue("--col-size-size"),
  ).toBe("140");
});

test("TestAriaSortAndMultiSortBadge", async () => {
  await mount();
  const added = screen.getByRole("columnheader", { name: "Added" });
  const status = screen.getByRole("columnheader", { name: "Status" });
  const name = screen.getByRole("columnheader", { name: "Name" });
  expect(added.getAttribute("aria-sort")).toBe("descending");
  expect(status.getAttribute("aria-sort")).toBe("none");
  fireEvent.click(status);
  expect(status.getAttribute("aria-sort")).toBe("ascending");
  expect(added.getAttribute("aria-sort")).toBe("none");
  // Single sort: no priority badge.
  expect(within(status).queryByText("1")).toBeNull();
  fireEvent.click(name, { shiftKey: true });
  expect(name.getAttribute("aria-sort")).toBe("ascending");
  expect(within(status).getByText("1")).toBeTruthy();
  expect(within(name).getByText("2")).toBeTruthy();
});

test("TestPinnedColumnsStayFixed", async () => {
  await mount();
  await openColumnsMenu();
  const up = (label: string) =>
    screen.getByRole("button", {
      name: `Move ${label} up`,
    }) as HTMLButtonElement;
  const down = (label: string) =>
    screen.getByRole("button", {
      name: `Move ${label} down`,
    }) as HTMLButtonElement;
  for (const label of ["Select tasks", "Name"]) {
    expect(up(label).disabled).toBe(true);
    expect(down(label).disabled).toBe(true);
  }
  // Pinned columns are never a drag source or a reorder target.
  const current = [...DEFAULT_COLUMN_ORDER];
  expect(moveGridColumn(current, "select", "size")).toEqual(current);
  expect(moveGridColumn(current, "size", "select")).toEqual(current);
  expect(moveGridColumn(current, "name", "size")).toEqual(current);
  const nameHeader = document.querySelector('[data-column-id="name"]')!;
  fireEvent.pointerDown(nameHeader, {
    pointerId: 1,
    clientX: 100,
    clientY: 5,
  });
  fireEvent.pointerMove(document, { pointerId: 1, clientX: 300, clientY: 5 });
  fireEvent.pointerUp(document, { pointerId: 1, clientX: 300, clientY: 5 });
  expect(columnIds()).toEqual([...DEFAULT_COLUMN_ORDER]);
  // A movable column swaps within the unpinned slots; the pinned positions stay.
  fireEvent.click(up("Size"));
  expect(columnIds().slice(0, 4)).toEqual([
    "select",
    "size",
    "name",
    "queuePos",
  ]);
});
