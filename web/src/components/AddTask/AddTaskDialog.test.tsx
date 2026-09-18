import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
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

import { initI18n } from "../../i18n";
import { useTasks, type Task } from "../../store/useTasks";
import {
  AddTaskDialog,
  classifyLine,
  isDroppableText,
  type SearchResultOutcome,
} from "./AddTaskDialog";
import { Toaster } from "../ui/sonner";

const task = (id: string, patch: Partial<Task> = {}): Task => ({
  id,
  name: id,
  engine: "aria2",
  source_kind: "http",
  source_uri: null,
  state: "queued",
  category: null,
  tags: [],
  destination: "/data",
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

const server = setupServer();
let qc: QueryClient;

beforeAll(() => {
  initI18n();
  server.listen({ onUnhandledRequest: "error" });
});
beforeEach(() => {
  useTasks.getState().reset();
  qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  sessionStorage.clear();
  server.use(
    http.get("*/api/v1/categories", () =>
      HttpResponse.json({ categories: [] }),
    ),
    http.get("*/api/v1/tags", () => HttpResponse.json({ tags: [] })),
    http.get("*/api/v1/fs/roots", () =>
      HttpResponse.json({
        roots: [
          {
            path: "/data",
            writable: true,
            free_bytes: 1073741824,
            total_bytes: 2147483648,
          },
        ],
      }),
    ),
    http.get("*/api/v1/fs/free-space", () =>
      HttpResponse.json({
        path: "/data",
        free_bytes: 1073741824,
        total_bytes: 2147483648,
      }),
    ),
  );
});
afterEach(() => {
  cleanup();
  qc.clear();
  server.resetHandlers();
  sessionStorage.clear();
});
afterAll(() => server.close());

function mount(
  props: Partial<{
    initialUris: string[];
    initialSearchResultIds: string[];
    onSearchResultOutcome: SearchResultOutcome;
  }> = {},
) {
  const onOpenChange = vi.fn();
  render(
    <QueryClientProvider client={qc}>
      <AddTaskDialog open onOpenChange={onOpenChange} {...props} />
      <Toaster theme="system" />
    </QueryClientProvider>,
  );
  return { onOpenChange };
}

function urisBox(): HTMLTextAreaElement {
  return screen.getByRole("textbox", { name: "Enter URL" });
}

function dropzone(): HTMLElement {
  return screen.getByRole("button", { name: /drop \.torrent/ });
}

function drop(node: HTMLElement, dataTransfer: object) {
  fireEvent.drop(node, { dataTransfer });
}

test("TestClassifyLineBadges", () => {
  const hash = "8f9c3a2b1d4e5f60718293a4b5c6d7e8f9a0b1c2";
  const known = new Set([hash, "https://example.org/seen.iso"]);
  // A magnet recognised through its xt topic, and a duplicate on that hash.
  expect(classifyLine(`magnet:?xt=urn:btih:${hash}`, known)).toBe("duplicate");
  expect(
    classifyLine(
      "magnet:?xt=urn:btih:aaaabbbbccccdddd000011112222333344445555",
      known,
    ),
  ).toBe("ok");
  // Bare 40-char hex infohash: recognised, and duplicated by hash.
  expect(classifyLine(hash, known)).toBe("duplicate");
  expect(classifyLine("aaaabbbbccccdddd000011112222333344445555", known)).toBe(
    "ok",
  );
  // A 32-char base32 infohash is recognised.
  expect(classifyLine("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", known)).toBe("ok");
  // Schemes the dialog supports.
  for (const uri of [
    "https://example.org/a.iso",
    "http://example.org/a.iso",
    "ftp://files.example.org/pub/",
    "ftps://files.example.org/pub/",
    "sftp://files.example.org/pub/",
  ])
    expect(classifyLine(uri, new Set())).toBe("ok");
  // A duplicate of a queued task's normalised source URI.
  expect(classifyLine("https://example.org/seen.iso", known)).toBe("duplicate");
  // Rubbish and unsupported schemes warn.
  expect(classifyLine("rubbish", known)).toBe("unknown");
  expect(classifyLine("ed2k://|file|x|", known)).toBe("unknown");
  expect(classifyLine("", known)).toBe("unknown");
});

test("TestIsDroppableTextPredicate", () => {
  expect(isDroppableText("http://example.org/a")).toBe(true);
  expect(isDroppableText("https://example.org/a")).toBe(true);
  expect(isDroppableText("MAGNET:?xt=urn:btih:abc")).toBe(true);
  expect(isDroppableText("aaaabbbbccccdddd000011112222333344445555")).toBe(
    true,
  );
  expect(
    isDroppableText("abcdefghijklmnopqrstuvwxyz234567".toUpperCase()),
  ).toBe(true);
  // qBittorrent's predicate covers neither ftp nor arbitrary text.
  expect(isDroppableText("ftp://example.org/a")).toBe(false);
  expect(isDroppableText("rubbish")).toBe(false);
  expect(isDroppableText("ed2k://|file|x|")).toBe(false);
});

test("TestTxtDropAppendsLines", async () => {
  mount();
  await screen.findByRole("dialog");
  const list = new File(
    [
      "# a comment line\n",
      "https://example.org/one.iso\n",
      "\n",
      "https://example.org/two.iso\n",
    ],
    "batch.txt",
    { type: "text/plain" },
  );
  drop(dropzone(), {
    files: [list],
    items: [{ kind: "file", webkitGetAsEntry: () => null }],
    types: ["Files"],
  });
  await waitFor(() =>
    expect(urisBox().value).toBe(
      "https://example.org/one.iso\nhttps://example.org/two.iso",
    ),
  );
  // A second .txt appends to the existing lines.
  const more = new File(
    ["magnet:?xt=urn:btih:aaaabbbbccccdddd00001111222233334444"],
    "more.txt",
    {
      type: "text/plain",
    },
  );
  drop(dropzone(), {
    files: [more],
    items: [{ kind: "file", webkitGetAsEntry: () => null }],
    types: ["Files"],
  });
  await waitFor(() =>
    expect(urisBox().value).toBe(
      "https://example.org/one.iso\nhttps://example.org/two.iso\nmagnet:?xt=urn:btih:aaaabbbbccccdddd00001111222233334444",
    ),
  );
});

test("TestNzbDropRefused", async () => {
  mount();
  await screen.findByRole("dialog");
  const nzb = new File(["<nzb/>"], "release.nzb", {
    type: "application/x-nzb",
  });
  drop(dropzone(), {
    files: [nzb],
    items: [{ kind: "file", webkitGetAsEntry: () => null }],
    types: ["Files"],
  });
  expect(await screen.findByText("NZB is not supported in v1")).toBeTruthy();
  // The file neither lands in the draft nor appends to the textarea.
  expect(urisBox().value).toBe("");
  expect(screen.queryByText("release.nzb")).toBeNull();
});

test("TestJsonSubmissionBody", async () => {
  useTasks.getState().setConnection("live");
  const created = [
    task("task-one", { name: "one" }),
    task("task-two", { name: "two" }),
  ];
  let body: Record<string, unknown> | null = null;
  const patches: { id: string; body: unknown }[] = [];
  server.use(
    http.get("*/api/v1/categories", () =>
      HttpResponse.json({
        categories: [{ name: "linux", save_path: "/data/iso", task_count: 0 }],
      }),
    ),
    http.post("*/api/v1/tasks", async ({ request }) => {
      body = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ created, rejected: [] }, { status: 201 });
    }),
    http.patch("*/api/v1/tasks/:id", async ({ request, params }) => {
      patches.push({
        id: params.id as string,
        body: await request.json(),
      });
      return HttpResponse.json(created[0]);
    }),
  );
  const { onOpenChange } = mount();
  const dialog = await screen.findByRole("dialog");
  fireEvent.change(urisBox(), {
    target: {
      value: "https://example.org/one.iso\nhttps://example.org/two.iso",
    },
  });
  fireEvent.click(screen.getByRole("button", { name: "More options" }));
  // Category through the combobox.
  const combobox = screen.getByRole("combobox");
  fireEvent.keyDown(combobox, { key: "ArrowDown" });
  fireEvent.click(await screen.findByRole("option", { name: "linux" }));
  // Tags through free entry.
  const tagInput = screen.getByRole("textbox", { name: "Tags" });
  fireEvent.change(tagInput, { target: { value: "iso" } });
  fireEvent.keyDown(tagInput, { key: "Enter" });
  fireEvent.click(screen.getByRole("checkbox", { name: "Add paused" }));
  fireEvent.click(
    screen.getByRole("checkbox", { name: "Sequential download" }),
  );
  fireEvent.click(
    screen.getByRole("checkbox", {
      name: "Save in subfolder named after the task",
    }),
  );
  fireEvent.change(screen.getByRole("spinbutton", { name: "Download limit" }), {
    target: { value: "1024" },
  });
  fireEvent.change(screen.getByRole("spinbutton", { name: "Upload limit" }), {
    target: { value: "2048" },
  });
  expect(dialog.textContent).not.toContain("KB/s");
  fireEvent.click(screen.getByRole("button", { name: "Create" }));

  await waitFor(() => expect(body).not.toBeNull());
  expect(body).toEqual({
    uris: ["https://example.org/one.iso", "https://example.org/two.iso"],
    paused: true,
    sequential: true,
    create_subfolder: true,
    category: "linux",
    tags: ["iso"],
  });
  // Non-zero limits travel as PATCH calls on each created task, never in the
  // create body (doc 05 §5.2/§5.5).
  await waitFor(() => expect(patches).toHaveLength(2));
  expect(patches).toEqual([
    { id: "task-one", body: { dl_limit: 1024, ul_limit: 2048 } },
    { id: "task-two", body: { dl_limit: 1024, ul_limit: 2048 } },
  ]);
  // A clean submission closes the dialog.
  await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
});

test("TestMultipartSubmissionForTorrent", async () => {
  useTasks.getState().setConnection("live");
  let contentType: string | null = null;
  let payload: Record<string, unknown> | null = null;
  let fileParts: FormDataEntryValue[] = [];
  server.use(
    http.post("*/api/v1/tasks", async ({ request }) => {
      contentType = request.headers.get("content-type");
      const form = await request.formData();
      payload = JSON.parse(form.get("payload") as string) as Record<
        string,
        unknown
      >;
      fileParts = form.getAll("file");
      return HttpResponse.json(
        { created: [task("torrent-one")], rejected: [] },
        { status: 201 },
      );
    }),
  );
  mount();
  await screen.findByRole("dialog");
  const torrent = new File(["d8:announce0:e"], "ubuntu.torrent", {
    type: "application/x-bittorrent",
  });
  drop(dropzone(), {
    files: [torrent],
    items: [{ kind: "file", webkitGetAsEntry: () => null }],
    types: ["Files"],
  });
  await screen.findByText("ubuntu.torrent");
  fireEvent.click(screen.getByRole("button", { name: "Create" }));

  await waitFor(() => expect(payload).not.toBeNull());
  expect(contentType).toContain("multipart/form-data");
  expect(payload).toEqual({
    uris: null,
    paused: false,
    sequential: false,
    create_subfolder: false,
    tags: [],
  });
  expect(fileParts).toHaveLength(1);
  expect((fileParts[0] as File).name).toBe("ubuntu.torrent");
});

test("TestFiftyLineSoftWarning", async () => {
  mount();
  await screen.findByRole("dialog");
  const lines = Array.from(
    { length: 62 },
    (_, index) => `https://example.org/file-${index}.iso`,
  );
  fireEvent.change(urisBox(), { target: { value: lines.join("\n") } });
  await screen.findByText("62 / 50 — the first 50 will be created");
  expect(
    screen.getByRole("button", { name: "Split into batches" }),
  ).toBeTruthy();
  // Nothing is truncated: all 62 lines stay in the textarea.
  expect(urisBox().value.split("\n")).toHaveLength(62);
});

test("TestCtrlEnterSubmits", async () => {
  useTasks.getState().setConnection("live");
  let posts = 0;
  server.use(
    http.post("*/api/v1/tasks", () => {
      posts += 1;
      return HttpResponse.json(
        { created: [task("ctrl-enter")], rejected: [] },
        { status: 201 },
      );
    }),
  );
  mount();
  await screen.findByRole("dialog");
  fireEvent.change(urisBox(), {
    target: { value: "https://example.org/one.iso" },
  });
  // Enter alone inserts a newline and submits nothing.
  fireEvent.keyDown(urisBox(), { key: "Enter" });
  await act(async () => {});
  expect(posts).toBe(0);
  fireEvent.keyDown(urisBox(), { key: "Enter", ctrlKey: true });
  await waitFor(() => expect(posts).toBe(1));
});

test("TestVerbatimLabels", async () => {
  mount();
  const dialog = await screen.findByRole("dialog");
  expect(dialog.textContent).toContain(
    "Authentication required (for ftp:// only)",
  );
  expect(dialog.textContent).toContain(
    "Show dialog to select files for download",
  );
  fireEvent.click(screen.getByRole("button", { name: "More options" }));
  expect(dialog.textContent).toContain(
    "Save in subfolder named after the task",
  );
  expect(
    screen.getByTitle(
      "The subfolder will be named as the same list name displayed here.",
    ),
  ).toBeTruthy();
  expect(dialog.textContent).not.toContain("KB/s");
});

test("TestRejectedEntriesToast", async () => {
  useTasks.getState().setConnection("live");
  server.use(
    http.post("*/api/v1/tasks", () =>
      HttpResponse.json(
        {
          created: [task("kept")],
          rejected: [
            {
              uri: "https://bad.example.org/x",
              detail: "scheme not allowed",
              type: "/problems/unsupported-scheme",
            },
          ],
        },
        { status: 201 },
      ),
    ),
  );
  mount();
  await screen.findByRole("dialog");
  fireEvent.change(urisBox(), {
    target: {
      value: "https://good.example.org/a\nhttps://bad.example.org/x",
    },
  });
  fireEvent.click(screen.getByRole("button", { name: "Create" }));
  // Partial success: the refused URI surfaces with the server's detail.
  expect(
    await screen.findByText(/bad\.example\.org\/x — scheme not allowed/),
  ).toBeTruthy();
});

test("TestSelectFilesStepPostsSelection", async () => {
  useTasks.getState().setConnection("live");
  const magnet = "magnet:?xt=urn:btih:aaaabbbbccccdddd000011112222333344445555";
  let body: Record<string, unknown> | null = null;
  server.use(
    http.post("*/api/v1/tasks/inspect", () =>
      HttpResponse.json({
        manifests: [
          {
            source_uri: magnet,
            kind: "torrent",
            name: "ubuntu",
            total_size: 3000,
            file_count: 2,
            metadata_pending: false,
            infohash_v1: "aaaabbbbccccdddd000011112222333344445555",
            infohash_v2: null,
            files: [
              { index: 0, path: "ubuntu/one.iso", size: 2000 },
              { index: 1, path: "ubuntu/readme.txt", size: 1000 },
            ],
          },
        ],
        rejected: [],
      }),
    ),
    http.post("*/api/v1/tasks", async ({ request }) => {
      body = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json(
        { created: [task("selected")], rejected: [] },
        { status: 201 },
      );
    }),
  );
  mount();
  await screen.findByRole("dialog");
  fireEvent.change(urisBox(), { target: { value: magnet } });
  fireEvent.click(
    screen.getByRole("checkbox", {
      name: "Show dialog to select files for download",
    }),
  );
  fireEvent.click(screen.getByRole("button", { name: "Next" }));

  // Second page: the FileTree renders the manifest's files.
  expect(
    await screen.findByText("Select files to download — ubuntu"),
  ).toBeTruthy();
  // Skip readme.txt; a skipped file posts selected:false + priority skip.
  fireEvent.click(
    await screen.findByRole("checkbox", { name: "Select readme.txt" }),
  );
  fireEvent.click(screen.getByRole("button", { name: "Create" }));

  await waitFor(() => expect(body).not.toBeNull());
  expect(body).toEqual({
    uris: [magnet],
    paused: false,
    sequential: false,
    create_subfolder: false,
    tags: [],
    select_files: [
      { index: 0, selected: true, priority: "normal" },
      { index: 1, selected: false, priority: "skip" },
    ],
  });
});

test("TestSelectFilesLaterPageReadOnly", async () => {
  useTasks.getState().setConnection("live");
  const magnetOne =
    "magnet:?xt=urn:btih:aaaabbbbccccdddd000011112222333344445555";
  const magnetTwo =
    "magnet:?xt=urn:btih:66667777888899990000aaaabbbbccccddddeeee";
  let body: Record<string, unknown> | null = null;
  server.use(
    http.post("*/api/v1/tasks/inspect", () =>
      HttpResponse.json({
        manifests: [
          {
            source_uri: magnetOne,
            kind: "torrent",
            name: "one",
            total_size: 2000,
            file_count: 2,
            metadata_pending: false,
            infohash_v1: "aaaabbbbccccdddd000011112222333344445555",
            infohash_v2: null,
            files: [
              { index: 0, path: "one/alpha.iso", size: 1500 },
              { index: 1, path: "one/notes.txt", size: 500 },
            ],
          },
          {
            source_uri: magnetTwo,
            kind: "torrent",
            name: "two",
            total_size: 4000,
            file_count: 2,
            metadata_pending: false,
            infohash_v1: "66667777888899990000aaaabbbbccccddddeeee",
            infohash_v2: null,
            files: [
              { index: 0, path: "two/beta.iso", size: 3000 },
              { index: 1, path: "two/readme.txt", size: 1000 },
            ],
          },
        ],
        rejected: [],
      }),
    ),
    http.post("*/api/v1/tasks", async ({ request }) => {
      body = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json(
        { created: [task("one"), task("two")], rejected: [] },
        { status: 201 },
      );
    }),
  );
  mount();
  await screen.findByRole("dialog");
  fireEvent.change(urisBox(), {
    target: { value: `${magnetOne}\n${magnetTwo}` },
  });
  fireEvent.click(
    screen.getByRole("checkbox", {
      name: "Show dialog to select files for download",
    }),
  );
  fireEvent.click(screen.getByRole("button", { name: "Next" }));

  expect(
    await screen.findByText("Select files to download — one"),
  ).toBeTruthy();

  // The second torrent is not the select_files target (doc 05 §5.2): its
  // page previews the defaults instead of offering edits that would be
  // silently dropped at submission.
  fireEvent.click(screen.getByRole("button", { name: "Next torrent" }));
  expect(
    await screen.findByText("Select files to download — two"),
  ).toBeTruthy();
  expect(screen.getByText(/downloads at its defaults/)).toBeTruthy();
  const betaBox = (await screen.findByRole("checkbox", {
    name: "Select beta.iso",
  })) as HTMLInputElement;
  expect(betaBox.disabled).toBe(true);
  // The preview shows the default the torrent will download with.
  expect(betaBox.checked).toBe(true);
  expect(
    (screen.getByRole("button", { name: "All" }) as HTMLButtonElement).disabled,
  ).toBe(true);
  expect(
    (screen.getByRole("button", { name: "None" }) as HTMLButtonElement)
      .disabled,
  ).toBe(true);
  expect(
    (screen.getByRole("button", { name: "Invert" }) as HTMLButtonElement)
      .disabled,
  ).toBe(true);

  // The first torrent's page stays editable and owns the submission.
  fireEvent.click(screen.getByRole("button", { name: "Previous torrent" }));
  fireEvent.click(
    await screen.findByRole("checkbox", { name: "Select notes.txt" }),
  );
  fireEvent.click(screen.getByRole("button", { name: "Create" }));

  await waitFor(() => expect(body).not.toBeNull());
  expect(body).toEqual({
    uris: [magnetOne, magnetTwo],
    paused: false,
    sequential: false,
    create_subfolder: false,
    tags: [],
    select_files: [
      { index: 0, selected: true, priority: "normal" },
      { index: 1, selected: false, priority: "skip" },
    ],
  });
});

// The search screen's "Download to…" draft: no URL entry or dropzone, the
// opaque res_ ids post as search_result_ids (doc 09 §7).
test("TestSearchResultDraftPostsIDs", async () => {
  let body: Record<string, unknown> | null = null;
  server.use(
    http.post("*/api/v1/tasks", async ({ request }) => {
      body = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ created: [], rejected: [] }, { status: 201 });
    }),
  );
  mount({ initialSearchResultIds: ["res_one", "res_two"] });

  await screen.findByRole("dialog");
  expect(screen.getByText("2 search results selected")).toBeTruthy();
  expect(screen.queryByRole("textbox", { name: "Enter URL" })).toBeNull();
  expect(screen.queryByRole("button", { name: /drop \.torrent/ })).toBeNull();
  // The file-selection checkbox stays disabled — /tasks/inspect takes no
  // res_ ids.
  const selectFiles = screen.getByRole("checkbox", {
    name: "Show dialog to select files for download",
  }) as HTMLButtonElement;
  expect(selectFiles.disabled).toBe(true);

  fireEvent.click(screen.getByRole("button", { name: "Create" }));
  await waitFor(() => expect(body).not.toBeNull());
  // One source family: the uris member is absent entirely, not null.
  expect(body).toEqual({
    search_result_ids: ["res_one", "res_two"],
    paused: false,
    sequential: false,
    create_subfolder: false,
    tags: [],
  });
});

// Over-50 res_ ids go out in ≤50-id chunks like every bulk path, and each
// chunk's outcome is reported so the search screen can mark its rows.
test("TestSearchResultDraftChunksAt50", async () => {
  const posted: { search_result_ids: string[] }[] = [];
  const outcomes: {
    ids: string[];
    rejected: { search_result_id?: string }[] | null;
  }[] = [];
  server.use(
    http.post("*/api/v1/tasks", async ({ request }) => {
      const body = (await request.json()) as {
        search_result_ids: string[];
      };
      posted.push(body);
      const ids = body.search_result_ids;
      if (posted.length === 2)
        return HttpResponse.json(
          {
            created: [],
            rejected: [
              {
                search_result_id: ids[0],
                type: "/problems/not-found",
                detail: "unknown result",
              },
            ],
          },
          { status: 201 },
        );
      return HttpResponse.json({ created: [], rejected: [] }, { status: 201 });
    }),
  );
  mount({
    initialSearchResultIds: Array.from({ length: 120 }, (_, i) => `res_${i}`),
    onSearchResultOutcome: (ids, _created, rejected) =>
      outcomes.push({ ids, rejected }),
  });

  await screen.findByRole("dialog");
  fireEvent.click(screen.getByRole("button", { name: "Create" }));
  await waitFor(() => expect(posted).toHaveLength(3));
  expect(posted.map((b) => b.search_result_ids.length)).toEqual([50, 50, 20]);
  await waitFor(() => expect(outcomes).toHaveLength(3));
  expect(outcomes[1].rejected?.[0]?.search_result_id).toBe("res_50");
});
