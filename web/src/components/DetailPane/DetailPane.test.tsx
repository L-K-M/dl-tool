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
import { DetailPane, visibleTabs } from "./DetailPane";
import { buildTree, FileTree, type FileChange } from "../FileTree/FileTree";
import { TaskGrid } from "../TaskGrid/TaskGrid";
import { useTasks, type Task } from "../../store/useTasks";
import { defaultPrefs, useUiPrefs } from "../../store/useUiPrefs";
import { useShellUi } from "../Shell/Toolbar";
import { useGridTable } from "../TaskGrid/ColumnsMenu";

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
let files: unknown[];
let events: unknown[];
let patchBody: unknown;

type FileInput = Parameters<typeof buildTree>[0][number];

const file = (
  index: number,
  path: string,
  patch: Partial<FileInput> = {},
): FileInput => ({
  index,
  path,
  size_bytes: 1024,
  selected: true,
  priority: "normal",
  ...patch,
});

beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
beforeEach(() => {
  initStores();
  vi.spyOn(window, "matchMedia").mockImplementation((query) => ({
    media: query,
    onchange: null,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    addListener: vi.fn(),
    removeListener: vi.fn(),
    dispatchEvent: () => true,
    matches: false,
  }));
  vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockReturnValue(320);
  vi.spyOn(HTMLElement.prototype, "offsetWidth", "get").mockReturnValue(1024);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(320);
  vi.spyOn(HTMLElement.prototype, "scrollTo").mockImplementation(function (
    this: HTMLElement,
    options: number | ScrollToOptions,
  ) {
    if (typeof options !== "object") return;
    this.scrollTop = options.top ?? 0;
    this.scrollLeft = options.left ?? 0;
    this.dispatchEvent(new Event("scroll"));
  });
  tasks = [task("one"), task("two"), task("three")];
  files = [file(0, "a.iso"), file(1, "extras/b.txt")];
  events = [];
  patchBody = undefined;
  qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  server.use(
    http.get("*/api/v1/tasks", () =>
      HttpResponse.json({
        items: tasks,
        total: tasks.length,
        next_cursor: null,
      }),
    ),
    http.get("*/api/v1/tasks/:id/files", () => HttpResponse.json({ files })),
    http.patch("*/api/v1/tasks/:id/files", async ({ request }) => {
      patchBody = await request.json();
      return HttpResponse.json({ files });
    }),
    http.get("*/api/v1/tasks/:id/events", () =>
      HttpResponse.json({
        items: events,
        next_cursor: null,
        total: events.length,
      }),
    ),
    http.get("*/api/v1/tasks/:id/trackers", () =>
      HttpResponse.json({ trackers: [] }),
    ),
    http.get("*/api/v1/tasks/:id/peers", () =>
      HttpResponse.json({ peers: [] }),
    ),
    http.get("*/api/v1/fs/free-space", () =>
      HttpResponse.json({
        path: "/downloads",
        free_bytes: 1 << 30,
        total_bytes: 1 << 40,
      }),
    ),
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

function initStores() {
  useTasks.getState().reset();
  useShellUi.setState({
    nameFilter: "",
    debouncedFilter: "",
    pending: new Set(),
    filterInput: null,
  });
  useUiPrefs.setState(structuredClone(defaultPrefs));
  useGridTable.setState({ table: null });
}

function mountPane(patch: Partial<Task> = {}) {
  const selected = task("picked", patch);
  useTasks.getState().hydrate([...tasks, selected]);
  useTasks.getState().setSelection(["picked"]);
  return render(
    <QueryClientProvider client={qc}>
      <DetailPane />
    </QueryClientProvider>,
  );
}

function pane() {
  return screen.getByRole("region", { name: "Task details" });
}

test("TestVisibleTabsHidesBitTorrentTabs", async () => {
  expect(visibleTabs(task("x", { source_kind: "http" }))).toEqual([
    "general",
    "transfer",
    "files",
    "log",
  ]);
  for (const kind of ["magnet", "torrent"])
    expect(visibleTabs(task("x", { source_kind: kind }))).toEqual([
      "general",
      "transfer",
      "trackers",
      "peers",
      "files",
      "log",
    ]);
  await mountPane();
  const tabs = within(pane())
    .getAllByRole("tab")
    .map((tab) => tab.textContent);
  expect(tabs).toEqual(["General", "Transfer", "Files", "Log"]);
  act(() => {
    useTasks.getState().hydrate([task("picked", { source_kind: "torrent" })]);
  });
  await waitFor(() =>
    expect(
      within(pane())
        .getAllByRole("tab")
        .map((tab) => tab.textContent),
    ).toEqual(["General", "Transfer", "Trackers", "Peers", "Files", "Log"]),
  );
});

test("TestRequestedDestinationOnlyWhenDifferent", async () => {
  await mountPane({
    destination: "/downloads/linux",
    requested_destination: "/downloads/linux",
  });
  expect(within(pane()).queryByText("Requested destination")).toBeNull();
  act(() => {
    useTasks
      .getState()
      .hydrate([task("picked", { requested_destination: "/data/other" })]);
  });
  expect(within(pane()).getByText("Requested destination")).toBeTruthy();
  expect(within(pane()).getByText("/data/other")).toBeTruthy();
});

test("TestFolderCheckboxSetsDescendants", () => {
  const nodes = buildTree([
    file(0, "ubuntu/a.iso"),
    file(1, "ubuntu/extras/b.txt", { priority: "high" }),
    file(2, "readme.txt"),
  ]);
  const changes: FileChange[][] = [];
  const { rerender } = render(
    <FileTree nodes={nodes} onChange={(change) => changes.push(change)} />,
  );
  const folder = screen.getByRole("treeitem", { name: /ubuntu/ });
  expect(folder.getAttribute("aria-checked")).toBe("true");
  expect(folder.getAttribute("aria-expanded")).toBe("true");
  expect(folder.getAttribute("aria-level")).toBe("1");
  fireEvent.click(screen.getByRole("checkbox", { name: "Select ubuntu" }));
  expect(changes).toEqual([
    [
      { index: 0, selected: false },
      { index: 1, selected: false },
    ],
  ]);
  const next = buildTree([
    file(0, "ubuntu/a.iso", { selected: false, priority: "skip" }),
    file(1, "ubuntu/extras/b.txt", { priority: "high" }),
    file(2, "readme.txt"),
  ]);
  rerender(<FileTree nodes={next} onChange={() => undefined} />);
  const mixed = screen.getByRole("treeitem", { name: /ubuntu/ });
  expect(mixed.getAttribute("aria-checked")).toBe("mixed");
  expect(
    (
      screen.getByRole("checkbox", {
        name: "Select ubuntu",
      }) as HTMLInputElement
    ).indeterminate,
  ).toBe(true);
});

test("TestSkipAndUnselectAreOneConcept", () => {
  const changes: FileChange[][] = [];
  const renderTree = (selected: boolean, priority: FileInput["priority"]) =>
    render(
      <FileTree
        nodes={buildTree([file(0, "a.iso", { selected, priority })])}
        onChange={(change) => changes.push(change)}
      />,
    );
  // An unchecked row always shows Skip in its select — they cannot disagree.
  const first = renderTree(false, "high");
  const select = screen.getByRole("combobox") as HTMLSelectElement;
  expect(select.value).toBe("skip");
  // Selecting Skip emits the priority change; the checkbox follows it.
  fireEvent.change(select, { target: { value: "skip" } });
  expect(changes.at(-1)).toEqual([{ index: 0, priority: "skip" }]);
  first.unmount();
  const second = renderTree(true, "normal");
  fireEvent.click(screen.getByRole("checkbox", { name: "Select a.iso" }));
  expect(changes.at(-1)).toEqual([{ index: 0, selected: false }]);
  second.unmount();
  // Re-checking a skipped row selects it; the server maps bare select to Normal.
  renderTree(false, "skip");
  fireEvent.click(screen.getByRole("checkbox", { name: "Select a.iso" }));
  expect(changes.at(-1)).toEqual([{ index: 0, selected: true }]);
});

test("TestPrioritySelectHasExactlyFourOptions", () => {
  render(
    <FileTree
      nodes={buildTree([file(0, "a.iso")])}
      onChange={() => undefined}
    />,
  );
  const select = screen.getByRole("combobox") as HTMLSelectElement;
  expect([...select.options].map((option) => option.value)).toEqual([
    "skip",
    "normal",
    "high",
    "maximum",
  ]);
  expect([...select.options].map((option) => option.textContent)).toEqual([
    "Skip",
    "Normal",
    "High",
    "Maximum",
  ]);
  // The qBittorrent integers never reach the UI.
  for (const option of select.options)
    expect(option.value).not.toMatch(/^[0-9]+$/);
});

test("TestPriorityPatchBody", async () => {
  useUiPrefs.setState({ detailTab: "files" });
  files = [file(0, "a.iso")];
  await mountPane({ source_kind: "torrent", engine: "qbittorrent" });
  const select = await within(pane()).findByRole("combobox", {
    name: "Priority for a.iso",
  });
  fireEvent.change(select, { target: { value: "high" } });
  await waitFor(() =>
    expect(patchBody).toEqual({ files: [{ index: 0, priority: "high" }] }),
  );
});

test("TestLogTabNewestFirstWithCodeColumn", async () => {
  useUiPrefs.setState({ detailTab: "log" });
  events = [
    {
      id: "evt_2",
      at: "2026-09-01T09:41:52Z",
      level: "info",
      code: "engine.accepted",
      message: "qbittorrent accepted the torrent",
      detail: null,
    },
    {
      id: "evt_1",
      at: "2026-09-01T09:41:50Z",
      level: "error",
      code: "task.created",
      message: "task created by alice",
      detail: null,
    },
  ];
  await mountPane();
  await within(pane()).findByText("engine.accepted");
  expect(
    within(pane())
      .getAllByRole("columnheader")
      .map((header) => header.textContent),
  ).toEqual(["Time", "Level", "Code", "Message"]);
  const rows = within(pane()).getAllByRole("row").slice(1);
  // Newest first, with the code rendered as its own column.
  expect(within(rows[0]).getByText("engine.accepted")).toBeTruthy();
  expect(within(rows[0]).getByText("info")).toBeTruthy();
  expect(within(rows[1]).getByText("task.created")).toBeTruthy();
  expect(within(rows[1]).getByText("task created by alice")).toBeTruthy();
  expect(within(rows[1]).getByText("error")).toBeTruthy();
  expect(rows[0].querySelector("td")!.getAttribute("title")).not.toBeNull();
});

test("TestAggregateLineAndCollapsedStates", async () => {
  await mountPane();
  act(() => useTasks.getState().setSelection(["one", "two"]));
  const line = pane();
  expect(line.textContent).toContain("2 tasks");
  expect(line.textContent).toContain("↓");
  expect(within(line).queryByRole("tab")).toBeNull();
  act(() => useTasks.getState().clearSelection());
  expect(pane().textContent).toContain("Select a task to see its details");
});

test("TestKeyboardOpensFocusedTask", async () => {
  useTasks.getState().hydrate(tasks);
  render(
    <QueryClientProvider client={qc}>
      <TaskGrid
        filter="all"
        actions={{
          openDetail: (id) => useTasks.getState().setSelection([id]),
        }}
      />
      <DetailPane />
    </QueryClientProvider>,
  );
  await screen.findByText("one", { exact: true });
  const grid = screen.getByRole("grid");
  // Focus and selection diverge: "one" stays selected while "two" is focused.
  fireEvent.click(document.querySelector('[data-task-id="one"]')!);
  fireEvent.keyDown(grid, { key: "ArrowDown" });
  await waitFor(() =>
    expect(
      document.activeElement
        ?.closest("[data-task-id]")
        ?.getAttribute("data-task-id"),
    ).toBe("two"),
  );
  expect([...useTasks.getState().selection]).toEqual(["one"]);
  expect(within(pane()).getByDisplayValue("one")).toBeTruthy();
  // Enter opens the focused row, not the stale selection.
  fireEvent.keyDown(grid, { key: "Enter" });
  await waitFor(() =>
    expect(within(pane()).getByDisplayValue("two")).toBeTruthy(),
  );
  expect([...useTasks.getState().selection]).toEqual(["two"]);
  // F2 does the same for a third row while "one" is selected again.
  act(() => useTasks.getState().setSelection(["one"]));
  fireEvent.keyDown(grid, { key: "ArrowDown" });
  await waitFor(() =>
    expect(
      document.activeElement
        ?.closest("[data-task-id]")
        ?.getAttribute("data-task-id"),
    ).toBe("three"),
  );
  expect([...useTasks.getState().selection]).toEqual(["one"]);
  fireEvent.keyDown(grid, { key: "F2" });
  await waitFor(() =>
    expect(within(pane()).getByDisplayValue("three")).toBeTruthy(),
  );
  expect([...useTasks.getState().selection]).toEqual(["three"]);
  // Editable targets never dispatch.
  const input = document.createElement("input");
  grid.appendChild(input);
  for (const key of ["Enter", "F2"]) fireEvent.keyDown(input, { key });
  input.remove();
  await act(async () => {});
  expect([...useTasks.getState().selection]).toEqual(["three"]);
});
