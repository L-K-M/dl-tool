import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { StrictMode, type ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
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
import App from "../../App";
import { initI18n } from "../../i18n";
import { useTasks, type Task } from "../../store/useTasks";
import {
  RemoveTasksDialog,
  ShellActionsContext,
  Toolbar,
  useShellUi,
  type RemoveRequest,
  type ShellActions,
} from "./Toolbar";
import { Toaster } from "../ui/sonner";
import { Sidebar } from "./Sidebar";
import { StatusBar } from "./StatusBar";

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
const session = {
  user: {
    id: "usr_test",
    username: "operator",
    enabled: true,
    locale: "en",
    last_login_at: null,
    created_at: "2026-09-01T09:00:00Z",
  },
  csrf_token: "memory-only-csrf",
};
const server = setupServer();
let qc: QueryClient;
let tasks: Task[];
const viewportHeight = 320;

beforeAll(() => {
  initI18n();
  server.listen({ onUnhandledRequest: "error" });
});
beforeEach(() => {
  useTasks.getState().reset();
  useShellUi.setState({
    nameFilter: "",
    debouncedFilter: "",
    pending: new Set(),
    filterInput: null,
  });
  tasks = [task("alpha", { name: "Alpha" }), task("beta", { name: "Beta" })];
  qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  localStorage.clear();
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
    http.get("*/api/v1/tasks", () =>
      HttpResponse.json({
        items: tasks,
        total: tasks.length,
        next_cursor: null,
      }),
    ),
  );
});
afterEach(() => {
  cleanup();
  // TestShellKeyboardActions injects <base> and rewrites history; a failure
  // before its own cleanup must not leak either into later tests.
  document.querySelector("base")?.remove();
  window.history.replaceState(null, "", "/");
  qc.clear();
  server.resetHandlers();
  vi.restoreAllMocks();
  localStorage.clear();
});
afterAll(() => server.close());

function wrap(children: ReactNode, route = "/") {
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[route]}>{children}</MemoryRouter>
    </QueryClientProvider>,
  );
}
function shellActions(overrides: Partial<ShellActions> = {}): ShellActions {
  return {
    requestRemove: vi.fn(),
    focusFilter: vi.fn(),
    showShortcuts: vi.fn(),
    signOut: vi.fn(),
    userName: "operator",
    ...overrides,
  };
}
function mountToolbar(actions: ShellActions = shellActions()) {
  wrap(
    <ShellActionsContext.Provider value={actions}>
      <Toolbar />
    </ShellActionsContext.Provider>,
  );
  return actions;
}
function link(label: string): HTMLElement {
  const found = screen
    .getAllByRole("link")
    .find((node) => node.textContent?.startsWith(label));
  if (!found) throw new Error(`no sidebar link ${label}`);
  return found;
}
function linkCount(label: string): string | null | undefined {
  return link(label).querySelector(".count")?.textContent;
}
function selected(): string[] {
  return [...useTasks.getState().selection];
}

test("TestSidebarCountsComeFromStore", () => {
  useTasks
    .getState()
    .hydrate([
      task("a", { state: "downloading" }),
      task("b", { state: "downloading" }),
      task("c", { state: "completed", category: "linux", tags: ["iso"] }),
      task("d", { state: "error" }),
    ]);
  wrap(<Sidebar />);
  expect(linkCount("All")).toBe("4");
  expect(linkCount("Downloading")).toBe("2");
  expect(linkCount("Completed")).toBe("1");
  expect(linkCount("Active")).toBe("2");
  expect(linkCount("Inactive")).toBe("1");
  expect(linkCount("Stopped")).toBe("0");
  expect(linkCount("Error")).toBe("1");
  expect(linkCount("linux")).toBe("1");
  expect(linkCount("Uncategorised")).toBe("3");
  expect(linkCount("iso")).toBe("1");
  expect(linkCount("Untagged")).toBe("3");
});

test("TestZeroCountNodeStaysVisible", () => {
  useTasks.getState().hydrate([task("a", { state: "downloading" })]);
  wrap(<Sidebar />);
  expect(link("Stopped").style.opacity).toBe("0.45");
  expect(link("Error").style.opacity).toBe("0.45");
  expect(link("Downloading").style.opacity).toBe("");
});

test("TestActiveNodeHasAriaCurrent", () => {
  useTasks.getState().hydrate([]);
  wrap(<Sidebar />, "/tasks/downloading");
  expect(link("Downloading").getAttribute("aria-current")).toBe("page");
  expect(link("All").getAttribute("aria-current")).toBeNull();
  expect(link("Stopped").getAttribute("aria-current")).toBeNull();
});

test("TestToolbarDisabledWithoutSelection", () => {
  useTasks.getState().setConnection("live");
  mountToolbar();
  for (const name of ["Start", "Pause", "Remove", "Move"]) {
    const button = screen.getByRole("button", { name }) as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    expect(button.getAttribute("aria-disabled")).toBe("true");
    expect(button.getAttribute("title")).toBe("Select at least one task");
  }
  const add = screen.getByRole("button", { name: "Add" });
  expect((add as HTMLButtonElement).disabled).toBe(true);
  expect(add.getAttribute("title")).toBe("Coming with the add dialog");
  const columns = screen.getByRole("button", { name: "Columns" });
  expect((columns as HTMLButtonElement).disabled).toBe(true);
  const clear = screen.getByRole("button", { name: "Clear completed" });
  expect((clear as HTMLButtonElement).disabled).toBe(true);
  expect(clear.getAttribute("title")).toBe("No completed tasks");
  act(() => {
    useTasks.getState().setSelection(["missing"]);
    useTasks.getState().setConnection("offline");
  });
  expect(
    screen.getByRole("button", { name: "Pause" }).getAttribute("title"),
  ).toBe("Reconnecting…");
});

test("TestPausePostsActionsPayload", async () => {
  useTasks.getState().setConnection("live");
  tasks = [task("one", { state: "downloading", name: "One" })];
  useTasks.getState().hydrate(tasks);
  useTasks.getState().setSelection(["one"]);
  let body: unknown;
  let release = () => {};
  server.use(
    http.post("*/api/v1/tasks/actions", async ({ request }) => {
      body = await request.json();
      await new Promise<void>((resolve) => {
        release = resolve;
      });
      return HttpResponse.json({ results: [{ id: "one", ok: true }] });
    }),
  );
  mountToolbar();
  fireEvent.click(screen.getByRole("button", { name: "Pause" }));
  // The optimistic patch lands while the request is still in flight.
  await waitFor(() =>
    expect(useTasks.getState().tasks.get("one")?.state).toBe("paused"),
  );
  await waitFor(() => expect(body).toEqual({ ids: ["one"], action: "pause" }));
  await act(async () => release());
});

test.each([
  ["unticked", false],
  ["shiftDeletePreChecked", true],
])("TestRemoveDialogDeleteFilesUnticked %s", async (_label, preChecked) => {
  useTasks.getState().hydrate([task("one", { name: "One" })]);
  wrap(
    <RemoveTasksDialog
      request={{ ids: ["one"], deleteFiles: preChecked }}
      onClose={vi.fn()}
    />,
  );
  const dialog = await screen.findByRole("dialog");
  expect(dialog.textContent).toContain("One");
  const box = screen.getByRole("checkbox", {
    name: "Also delete downloaded files",
  });
  expect(box.getAttribute("aria-checked")).toBe(String(preChecked));
  expect(dialog.textContent?.includes("Shift+Delete")).toBe(preChecked);
});

test("TestStatusBarSegments", () => {
  const state = useTasks.getState();
  state.applySync({
    rid: 1,
    full_update: false,
    seq_gap: false,
    tasks: {},
    tasks_removed: [],
    stats: { speed_down: 13500000, speed_up: 1600000, active: 8, queued: 2 },
  });
  useTasks.getState().hydrate([task("a"), task("b")]);
  useTasks.getState().setConnection("live");
  wrap(<StatusBar />);
  const segments = [...screen.getByRole("status").parentElement!.children];
  expect(segments).toHaveLength(6);
  expect(segments[0].getAttribute("aria-live")).toBe("polite");
  expect(segments[0].textContent).toBe("● Connected");
  expect(segments[1].textContent).toBe("↓ 12.9 MB/s ↑ 1.5 MB/s");
  expect(screen.getByRole("status").parentElement!.className).toContain(
    "tabular-nums",
  );
  expect(segments[2].textContent).toBe("8 active / 2 total");
  expect(segments[3].textContent).toBe("Free: —");
  expect(segments[4].textContent).toBe("Sched: —");
  expect(segments[4].tagName).toBe("A");
  expect(segments[5].textContent).toBe("");
  act(() => useTasks.getState().setConnection("offline"));
  expect(segments[0].textContent).toBe("○ Offline");
});

test("TestShellKeyboardActions", async () => {
  const deletes: { id: unknown; deleteData: string | null }[] = [];
  server.use(
    http.get("*/api/v1/auth/me", () => HttpResponse.json(session)),
    http.post("*/api/v1/tasks/actions", () =>
      HttpResponse.json({ results: [] }),
    ),
    http.delete("*/api/v1/tasks/:id", ({ params, request }) => {
      deletes.push({
        id: params.id,
        deleteData: new URL(request.url).searchParams.get("delete_data"),
      });
      tasks = tasks.filter((item) => item.id !== params.id);
      return HttpResponse.json({
        removed: true,
        delete_data: true,
        files_unlinked: 1,
        bytes_unlinked: 1024,
        missing: 0,
      });
    }),
  );
  const base = document.createElement("base");
  base.href = "/";
  document.head.appendChild(base);
  window.history.replaceState(null, "", "/");
  render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
  const alpha = await screen.findByText("Alpha");
  const row = alpha.closest("[data-task-id]") as HTMLElement;
  const grid = screen.getByRole("grid");
  fireEvent.click(row);
  expect(selected()).toEqual(["alpha"]);

  // Delete opens the confirmation flow; nothing is sent before confirm.
  row.focus();
  fireEvent.keyDown(row, { key: "Delete" });
  let dialog = await screen.findByRole("dialog");
  expect(dialog.textContent).toContain("Alpha");
  expect(deletes).toHaveLength(0);
  expect(
    screen
      .getByRole("checkbox", { name: "Also delete downloaded files" })
      .getAttribute("aria-checked"),
  ).toBe("false");

  // Escape inside the dialog closes it without clearing the grid selection,
  // and focus returns to the row.
  fireEvent.keyDown(dialog, { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(deletes).toHaveLength(0);
  expect(selected()).toEqual(["alpha"]);
  await waitFor(() => expect(document.activeElement).toBe(row));

  // Shift+Delete pre-ticks the box and the body says so.
  fireEvent.keyDown(row, { key: "Delete", shiftKey: true });
  dialog = await screen.findByRole("dialog");
  expect(dialog.textContent).toContain("Shift+Delete");
  expect(
    screen
      .getByRole("checkbox", { name: "Also delete downloaded files" })
      .getAttribute("aria-checked"),
  ).toBe("true");
  fireEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
  await waitFor(() =>
    expect(deletes).toEqual([{ id: "alpha", deleteData: "true" }]),
  );
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(useTasks.getState().tasks.has("alpha")).toBe(false);
  expect(selected()).toEqual([]);

  // Empty-selection removal is a no-op. Re-query the row: virtual rows are
  // re-keyed when the list above them shrinks.
  let betaRow = screen
    .getByText("Beta")
    .closest("[data-task-id]") as HTMLElement;
  fireEvent.keyDown(betaRow, { key: "Delete" });
  await act(async () => {});
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(deletes).toHaveLength(1);

  // Ctrl/Cmd+F focuses the toolbar filter box, which filters the grid.
  fireEvent.click(betaRow);
  fireEvent.keyDown(betaRow, { key: "f", ctrlKey: true });
  const filter = screen.getByRole("textbox", {
    name: "Filter tasks by name",
  });
  expect(document.activeElement).toBe(filter);
  fireEvent.change(filter, { target: { value: "zzz" } });
  await waitFor(() =>
    expect(document.querySelectorAll("[data-task-id]")).toHaveLength(0),
  );
  fireEvent.change(filter, { target: { value: "" } });
  await waitFor(() =>
    expect(document.querySelectorAll("[data-task-id]")).toHaveLength(1),
  );

  // ? opens the cheat-sheet overlay; Escape closes it in isolation.
  betaRow = screen.getByText("Beta").closest("[data-task-id]") as HTMLElement;
  fireEvent.keyDown(betaRow, { key: "?" });
  const sheet = await screen.findByRole("dialog");
  expect(sheet.textContent).toContain("Keyboard shortcuts");
  fireEvent.keyDown(sheet, { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(selected()).toEqual(["beta"]);

  // The editable-target guard: grid keys never fire from form fields.
  filter.blur();
  const input = document.createElement("input");
  grid.appendChild(input);
  fireEvent.keyDown(input, { key: "Delete" });
  fireEvent.keyDown(input, { key: "f", ctrlKey: true });
  fireEvent.keyDown(input, { key: "?" });
  input.remove();
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(document.activeElement).not.toBe(filter);

  base.remove();
});

test("TestRemoveDialogRecoversFromTransportFailure", async () => {
  useTasks
    .getState()
    .hydrate([task("one", { name: "One" }), task("two", { name: "Two" })]);
  let calls = 0;
  server.use(
    http.delete("*/api/v1/tasks/:id", ({ params }) => {
      calls++;
      if (params.id === "two") return HttpResponse.error();
      return HttpResponse.json({
        removed: true,
        delete_data: false,
        files_unlinked: 0,
        bytes_unlinked: 0,
        missing: 0,
      });
    }),
  );
  const onClose = vi.fn();
  const tree = (request: RemoveRequest | null) => (
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <RemoveTasksDialog request={request} onClose={onClose} />
        <Toaster theme="system" />
      </MemoryRouter>
    </QueryClientProvider>
  );
  const view = render(tree({ ids: ["one", "two"], deleteFiles: false }));
  fireEvent.click(
    within(await screen.findByRole("dialog")).getByRole("button", {
      name: "Remove",
    }),
  );
  await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
  // The confirmed removal reconciles even though the next DELETE rejected.
  expect(useTasks.getState().tasks.has("one")).toBe(false);
  expect(useTasks.getState().tasks.has("two")).toBe(true);
  await screen.findByText("Removal failed: Network error");

  // The wedged busy flag was the bug: a reopened dialog must confirm again.
  view.rerender(tree({ ids: ["two"], deleteFiles: false }));
  const again = (await screen.findByRole("button", {
    name: "Remove",
  })) as HTMLButtonElement;
  expect(again.disabled).toBe(false);
  fireEvent.click(again);
  await waitFor(() => expect(onClose).toHaveBeenCalledTimes(2));
  expect(calls).toBe(3);
});

test("TestToolbarCollapsesToIconsBelow1100", () => {
  useTasks.getState().setConnection("live");
  mountToolbar();
  for (const name of [
    "Add",
    "Start",
    "Pause",
    "Remove",
    "Edit",
    "Move",
    "Clear completed",
    "Columns",
  ]) {
    const button = screen.getByRole("button", { name });
    // The aria-label keeps the accessible name when the visible label
    // drops below 1100 px (doc 09 section 2.5 icon-only mode).
    expect(button.getAttribute("aria-label")).toBe(name);
    expect(within(button).getByText(name).className).toContain(
      "max-[1100px]:hidden",
    );
  }
});
