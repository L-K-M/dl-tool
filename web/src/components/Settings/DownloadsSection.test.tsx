import {
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
import { MemoryRouter, Route, Routes } from "react-router-dom";
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  expect,
  test,
} from "vitest";
import { initI18n } from "../../i18n";
import { SettingsScreen, useSettingsDirty } from "./SettingsScreen";

const server = setupServer();
let qc: QueryClient;
let settingsBody: Record<string, unknown>;
let rootRows: Record<string, unknown>[];
let watchRows: Record<string, unknown>[];
let categoryRows: Record<string, unknown>[];
let engineRows: Record<string, unknown>[];
let settingsPatches: Record<string, unknown>[];
let watchPosts: Record<string, unknown>[];
let watchPatches: { id: string; body: Record<string, unknown> }[];
let categoryPosts: Record<string, unknown>[];
let categoryPatches: { name: string; body: Record<string, unknown> }[];
let categoryDeletes: string[];
let watchDeletes: string[];

function freshQueryClient() {
  qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
}

function mount(section: string) {
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[`/settings/${section}`]}>
        <Routes>
          <Route path="/settings/:section" element={<SettingsScreen />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function watchFolder(id: string): Record<string, unknown> {
  return {
    id,
    path: `/data/watch-${id}`,
    enabled: true,
    destination: "/data/iso",
    category: "linux",
    delete_after_load: true,
    poll_interval_s: 10,
    last_scan_at: "2026-09-24T09:40:00Z",
    last_error: null,
    created_at: "2026-08-01T10:00:00Z",
    updated_at: "2026-09-24T09:40:00Z",
  };
}

function browseAnswer(path: string): Record<string, unknown> {
  return {
    path,
    parent: path === "/data" ? null : "/data",
    separator: "/",
    writable: true,
    free_bytes: 442381537280,
    total_bytes: 2000398934016,
    directories:
      path === "/data"
        ? [
            { name: "iso", path: "/data/iso", writable: true },
            { name: "watch", path: "/data/watch", writable: true },
          ]
        : [],
  };
}

/** Drives the nested FolderBrowserDialog: open it through the button with the
 *  given accessible name, highlight a directory row and confirm with Select. */
async function pickPath(browseName: string, dirName: string) {
  fireEvent.click(await screen.findByRole("button", { name: browseName }));
  const dialog = await screen.findByRole("dialog", {
    name: "Select destination",
  });
  fireEvent.click(await within(dialog).findByRole("option", { name: dirName }));
  fireEvent.click(within(dialog).getByRole("button", { name: "Select" }));
  await waitFor(() =>
    expect(
      screen.queryByRole("dialog", { name: "Select destination" }),
    ).toBeNull(),
  );
}

beforeAll(() => {
  initI18n();
  server.listen({ onUnhandledRequest: "error" });
});
beforeEach(() => {
  freshQueryClient();
  settingsBody = {
    default_destination: "/data",
    auto_extract: false,
    extract_passwords: "__redacted__",
    min_free_space: {},
  };
  rootRows = [{ path: "/data", writable: true, free_bytes: 1, total_bytes: 2 }];
  watchRows = [];
  categoryRows = [];
  engineRows = [];
  settingsPatches = [];
  watchPosts = [];
  watchPatches = [];
  categoryPosts = [];
  categoryPatches = [];
  categoryDeletes = [];
  watchDeletes = [];
  useSettingsDirty.setState({ report: null });
  server.use(
    http.get("*/api/v1/settings", () => HttpResponse.json(settingsBody)),
    http.get("*/api/v1/fs/roots", () => HttpResponse.json({ roots: rootRows })),
    http.get("*/api/v1/fs/browse", ({ request }) => {
      const path = new URL(request.url).searchParams.get("path") ?? "";
      return HttpResponse.json(browseAnswer(path));
    }),
    http.get("*/api/v1/fs/free-space", ({ request }) => {
      const path = new URL(request.url).searchParams.get("path") ?? "";
      return HttpResponse.json({
        path,
        free_bytes: 442381537280,
        total_bytes: 2000398934016,
      });
    }),
    http.get("*/api/v1/watch-folders", () =>
      HttpResponse.json({ watch_folders: watchRows }),
    ),
    http.get("*/api/v1/categories", () =>
      HttpResponse.json({ categories: categoryRows }),
    ),
    http.get("*/api/v1/engines", () =>
      HttpResponse.json({ engines: engineRows }),
    ),
    http.patch("*/api/v1/settings", async ({ request }) => {
      const patch = (await request.json()) as Record<string, unknown>;
      settingsPatches.push(patch);
      settingsBody = { ...settingsBody, ...patch };
      return HttpResponse.json(settingsBody);
    }),
    http.post("*/api/v1/watch-folders", async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      watchPosts.push(body);
      const row = { id: "wfd_new", ...body };
      watchRows = [...watchRows, row];
      return HttpResponse.json(row, { status: 201 });
    }),
    http.patch("*/api/v1/watch-folders/:id", async ({ request, params }) => {
      const body = (await request.json()) as Record<string, unknown>;
      const id = params.id as string;
      watchPatches.push({ id, body });
      watchRows = watchRows.map((row) =>
        row.id === id ? { ...row, ...body } : row,
      );
      return HttpResponse.json(watchRows.find((row) => row.id === id) ?? {});
    }),
    http.delete("*/api/v1/watch-folders/:id", ({ params }) => {
      watchDeletes.push(params.id as string);
      watchRows = watchRows.filter((row) => row.id !== params.id);
      return new HttpResponse(null, { status: 204 });
    }),
    http.post("*/api/v1/categories", async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      categoryPosts.push(body);
      const row = { ...body, task_count: 0 };
      categoryRows = [...categoryRows, row];
      return HttpResponse.json(row, { status: 201 });
    }),
    http.patch("*/api/v1/categories/:name", async ({ request, params }) => {
      const body = (await request.json()) as Record<string, unknown>;
      const name = params.name as string;
      categoryPatches.push({ name, body });
      categoryRows = categoryRows.map((row) =>
        row.name === name
          ? { ...row, name: body.new_name ?? row.name, ...body }
          : row,
      );
      return HttpResponse.json(
        categoryRows.find((row) => row.name === (body.new_name ?? name)) ?? {},
      );
    }),
    http.delete("*/api/v1/categories/:name", ({ params }) => {
      categoryDeletes.push(params.name as string);
      categoryRows = categoryRows.filter((row) => row.name !== params.name);
      return new HttpResponse(null, { status: 204 });
    }),
  );
});
afterEach(() => {
  cleanup();
  server.resetHandlers();
});
afterAll(() => server.close());

test("TestSavesOnlyChangedSettingKeys", async () => {
  mount("downloads");
  const destination = (await screen.findByLabelText(
    "Default destination",
  )) as HTMLInputElement;
  // The field is read-only text: FolderBrowserDialog is the only writer.
  expect(destination.readOnly).toBe(true);
  expect(destination.value).toBe("/data");

  await pickPath("Choose the default destination", "iso");
  expect(destination.value).toBe("/data/iso");

  fireEvent.click(screen.getByLabelText("Auto-extract archives"));
  fireEvent.click(await screen.findByRole("button", { name: "Save" }));
  await waitFor(() => expect(settingsPatches).toHaveLength(1));
  // Exactly the changed keys — no key outside DOWNLOAD_KEYS can appear.
  expect(settingsPatches).toEqual([
    { default_destination: "/data/iso", auto_extract: true },
  ]);
  await waitFor(() =>
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull(),
  );
});

test("TestPasswordListNeverEchoesRedaction", async () => {
  mount("downloads");
  // The stored list is never rendered — only the fixed sentence.
  await screen.findByText("A shared password list is stored.");
  expect(screen.queryByText(/__redacted__/)).toBeNull();
  expect(screen.queryByText(/hunter2/)).toBeNull();

  fireEvent.click(screen.getByRole("button", { name: "Replace list" }));
  const editor = (await screen.findByLabelText(
    "New shared password list",
  )) as HTMLTextAreaElement;
  fireEvent.change(editor, {
    target: { value: "hunter2\ncorrect horse battery\n" },
  });
  fireEvent.click(await screen.findByRole("button", { name: "Save" }));
  await waitFor(() => expect(settingsPatches).toHaveLength(1));
  // Replacing sends the full array, never the placeholder back.
  expect(settingsPatches[0]).toEqual({
    extract_passwords: ["hunter2", "correct horse battery"],
  });
  await waitFor(() =>
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull(),
  );

  // Clearing sends an empty array; leaving the control alone sends nothing.
  fireEvent.click(await screen.findByRole("button", { name: "Clear list" }));
  fireEvent.click(await screen.findByRole("button", { name: "Save" }));
  await waitFor(() => expect(settingsPatches).toHaveLength(2));
  expect(settingsPatches[1]).toEqual({ extract_passwords: [] });
  for (const patch of settingsPatches)
    expect(JSON.stringify(patch)).not.toContain("__redacted__");
});

test("TestMinFreeSpacePerRoot", async () => {
  rootRows = [
    { path: "/data", writable: true, free_bytes: 1, total_bytes: 2 },
    { path: "/srv/media", writable: true, free_bytes: 1, total_bytes: 2 },
  ];
  settingsBody = {
    ...settingsBody,
    min_free_space: { "/data": 1073741824 },
  };
  mount("downloads");
  // One input per GET /fs/roots entry; the absent root renders the doc 11
  // default of 2147483648.
  const data = (await screen.findByLabelText("/data")) as HTMLInputElement;
  const media = (await screen.findByLabelText(
    "/srv/media",
  )) as HTMLInputElement;
  expect(data.value).toBe("1073741824");
  expect(media.value).toBe("2147483648");

  fireEvent.change(media, { target: { value: "0" } });
  fireEvent.click(await screen.findByRole("button", { name: "Save" }));
  await waitFor(() => expect(settingsPatches).toHaveLength(1));
  // The key's value is a map: the whole object goes out, not the delta.
  expect(settingsPatches).toEqual([
    { min_free_space: { "/data": 1073741824, "/srv/media": 0 } },
  ]);
});

test("TestWatchFolderScanRendersReport", async () => {
  watchRows = [
    {
      ...watchFolder("wfd_01"),
      last_error: "watch loader lost inotify; polling",
    },
  ];
  server.use(
    http.post("*/api/v1/watch-folders/wfd_01/scan", () =>
      HttpResponse.json({
        scanned: 3,
        created: ["tsk_a", "tsk_b"],
        skipped: [{ file: "partial.torrent.part", reason: "not_a_torrent" }],
        elapsed_ms: 86,
      }),
    ),
  );
  mount("downloads");
  const row = (await screen.findByText("/data/watch-wfd_01")).closest("tr")!;
  // last_error renders as a warning cell on the row.
  await within(row).findByText("watch loader lost inotify; polling");

  fireEvent.click(within(row).getByRole("button", { name: "Scan now" }));
  await within(row).findByText("scanned 3, created 2");
  await within(row).findByText("partial.torrent.part — not_a_torrent");
});

test("TestCategoryTableCrud", async () => {
  categoryRows = [{ name: "linux", save_path: "/data/iso", task_count: 12 }];
  mount("downloads");
  const table = (await screen.findByText("linux")).closest("table")!;
  expect(
    within(table).getByRole("columnheader", { name: "Tasks" }),
  ).toBeTruthy();
  expect(within(table).getByText("12")).toBeTruthy();

  // Create: the save path comes only from the folder browser.
  fireEvent.click(screen.getByRole("button", { name: "Add category" }));
  const addDialog = await screen.findByRole("dialog", {
    name: "Add category",
  });
  const savePathInput = within(addDialog).getByLabelText(
    "Save path",
  ) as HTMLInputElement;
  expect(savePathInput.readOnly).toBe(true);
  fireEvent.change(within(addDialog).getByLabelText("Name"), {
    target: { value: "movies" },
  });
  await pickPath("Choose the category save path", "iso");
  expect(savePathInput.value).toBe("/data/iso");
  fireEvent.click(within(addDialog).getByRole("button", { name: "Save" }));
  await waitFor(() =>
    expect(categoryPosts).toEqual([{ name: "movies", save_path: "/data/iso" }]),
  );

  // Rename: only the changed member goes out.
  const linuxRow = (await screen.findByText("linux")).closest("tr")!;
  fireEvent.click(within(linuxRow).getByRole("button", { name: "Edit" }));
  const editDialog = await screen.findByRole("dialog", {
    name: "Edit category",
  });
  fireEvent.change(within(editDialog).getByLabelText("Name"), {
    target: { value: "linux-iso" },
  });
  fireEvent.click(within(editDialog).getByRole("button", { name: "Save" }));
  await waitFor(() =>
    expect(categoryPatches).toEqual([
      { name: "linux", body: { new_name: "linux-iso" } },
    ]),
  );

  // Delete goes through the confirm step; the note says the tasks' data is
  // left alone.
  const renamedRow = (await screen.findByText("linux-iso")).closest("tr")!;
  fireEvent.click(within(renamedRow).getByRole("button", { name: "Delete" }));
  const confirm = await screen.findByRole("dialog", {
    name: "Confirm deletion",
  });
  expect(confirm.textContent).toContain("keep their data");
  fireEvent.click(within(confirm).getByRole("button", { name: "Delete" }));
  await waitFor(() => expect(categoryDeletes).toEqual(["linux-iso"]));
});

test("TestBitTorrentSectionIsReadOnly", async () => {
  engineRows = [
    {
      id: "eng_qbittorrent",
      kind: "qbittorrent",
      name: "qBittorrent",
      enabled: true,
      url: "http://qbittorrent:8080",
      connected: false,
      version: "5.0.0",
      capabilities: ["bittorrent", "peers"],
      last_seen_at: null,
      last_error: "dial tcp: connection refused",
    },
  ];
  mount("bittorrent");
  await screen.findByText("dial tcp: connection refused");

  // Nine named rows — the Download Station BitTorrent controls.
  const table = screen.getByRole("table");
  const bodyRows = within(table)
    .getAllByRole("row")
    .filter((row) => row.closest("tbody") !== null);
  expect(bodyRows).toHaveLength(9);
  for (const label of [
    "DHT",
    "Peer exchange (PeX)",
    "Local peer discovery (LSD)",
    "Encryption",
    "Maximum peers",
    "Auto-append trackers",
    "Default share-ratio limit",
    "Default seeding-time limit",
    "Action when a limit is reached",
  ])
    expect(within(table).getByText(label)).toBeTruthy();

  // Zero form controls and no Save bar: the section is read-only by
  // construction and never publishes a dirty report.
  for (const role of ["checkbox", "spinbutton", "textbox", "combobox", "radio"])
    expect(screen.queryByRole(role)).toBeNull();
  expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Revert" })).toBeNull();
  expect(useSettingsDirty.getState().report).toBeNull();
  // The engine status is rendered read-only too: name, status, version.
  await screen.findByText("qBittorrent engine");
  expect(screen.getByText("Not connected")).toBeTruthy();
  expect(screen.getByText(/5\.0\.0/)).toBeTruthy();
});
