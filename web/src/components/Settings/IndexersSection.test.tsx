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
import { toast } from "sonner";
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
import {
  IndexersSection,
  swapPriority,
  type IndexerRow,
} from "./IndexersSection";

const indexer = (over: Partial<IndexerRow>): IndexerRow => ({
  id: "idx_x",
  name: "x",
  kind: "dlsearch",
  enabled: true,
  url: null,
  api_key_set: false,
  definition_id: null,
  definition_source: "bundled",
  provenance: "shipped with dl-tool",
  legal_tier: "legitimate",
  priority: 50,
  seeders_unknown: true,
  categories: [{ id: 8000, name: "Other" }],
  last_test_at: null,
  last_error: null,
  ...over,
});

const server = setupServer();
let qc: QueryClient;
let rows: IndexerRow[];
let testCalls: string[];
let patchCalls: { id: string; body: Record<string, unknown> }[];

function mount() {
  return render(
    <QueryClientProvider client={qc}>
      <IndexersSection />
    </QueryClientProvider>,
  );
}

function rowOf(name: string): HTMLElement {
  return screen
    .getByText(name, { selector: "span.font-medium" })
    .closest("tr") as HTMLElement;
}

beforeAll(() => {
  initI18n();
  server.listen({ onUnhandledRequest: "error" });
});
beforeEach(() => {
  qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  // Server order is ORDER BY priority, name; the table must not re-sort it.
  rows = [
    indexer({
      id: "idx_archive",
      name: "Internet Archive",
      priority: 10,
      last_test_at: "2026-09-01T08:12:04Z",
    }),
    indexer({
      id: "idx_arch",
      name: "Arch Linux",
      kind: "torznab",
      url: "https://jackett.example/api/v2.0/indexers/archlinux/results/torznab/",
      api_key_set: true,
      definition_source: null,
      provenance: null,
      legal_tier: "user-supplied",
      priority: 20,
      seeders_unknown: false,
      categories: [
        { id: 2000, name: "Movies" },
        { id: 5000, name: "TV" },
      ],
      last_error: "HTTP 503 from upstream",
    }),
    indexer({
      id: "idx_academic",
      name: "Academic Torrents",
      kind: "torznab",
      enabled: false,
      url: "https://academictorrents.example/torznab",
      priority: 30,
    }),
    indexer({
      id: "idx_ubuntu",
      name: "Ubuntu",
      kind: "newznab",
      url: "https://newznab.example/api",
      priority: 40,
    }),
  ];
  testCalls = [];
  patchCalls = [];
  server.use(
    http.get("*/api/v1/indexers", () => HttpResponse.json({ indexers: rows })),
    http.post("*/api/v1/indexers/:id/test", ({ params }) => {
      testCalls.push(String(params.id));
      return HttpResponse.json({
        ok: true,
        elapsed_ms: 12,
        categories_found: 9,
        server: "TestServer 1.0",
        error: null,
      });
    }),
    http.patch("*/api/v1/indexers/:id", async ({ params, request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      patchCalls.push({ id: String(params.id), body });
      const base = rows.find((row) => row.id === params.id) ?? {};
      return HttpResponse.json({ ...base, ...body });
    }),
  );
});
afterEach(() => {
  cleanup();
  qc.clear();
  server.resetHandlers();
  vi.restoreAllMocks();
});
afterAll(() => server.close());

test("TestRowsRenderInServerOrder", async () => {
  const toastError = vi.spyOn(toast, "error");
  mount();
  await screen.findByText("Internet Archive", {
    selector: "span.font-medium",
  });
  const names = [
    ...document.querySelectorAll("tbody tr td:first-child span.font-medium"),
  ].map((cell) => cell.textContent);
  expect(names).toEqual([
    "Internet Archive",
    "Arch Linux",
    "Academic Torrents",
    "Ubuntu",
  ]);

  // The seven columns of doc 09 §9, under those names, in that order.
  const headers = [...document.querySelectorAll("thead th")].map(
    (th) => th.textContent,
  );
  expect(headers.slice(0, 7)).toEqual([
    "Name",
    "Type",
    "URL",
    "Categories",
    "Enabled",
    "Priority",
    "Last test",
  ]);
  // last_test_at:null renders the empty state; a stored last_error is a
  // warning line under the name.
  expect(screen.getAllByText("Never tested").length).toBe(3);
  expect(screen.getByText("HTTP 503 from upstream")).toBeTruthy();
  expect(
    screen.getByText(
      "Imported definitions arrive disabled and are converted by static analysis only; no third-party code is executed.",
    ),
  ).toBeTruthy();

  // Enabling is optimistic: the box checks while the PATCH is in flight and
  // rolls back on a 403, with an error toast (doc 09 §10.6).
  let release: ((response: Response) => void) | undefined;
  server.use(
    http.patch("*/api/v1/indexers/idx_academic", async ({ request }) => {
      patchCalls.push({
        id: "idx_academic",
        body: (await request.json()) as Record<string, unknown>,
      });
      return new Promise<Response>((resolve) => {
        release = resolve;
      });
    }),
  );
  const academic = rowOf("Academic Torrents");
  const box = within(academic).getByRole("checkbox", {
    name: "Enable Academic Torrents",
  });
  expect(box.getAttribute("aria-checked")).toBe("false");
  fireEvent.click(box);
  await waitFor(() => expect(box.getAttribute("aria-checked")).toBe("true"));
  await waitFor(() => expect(release).toBeDefined());
  release!(
    HttpResponse.json(
      {
        type: "/problems/ssrf-blocked",
        title: "Forbidden",
        detail: "private-network indexers need allow_private_network",
        status: 403,
      },
      { status: 403 },
    ),
  );
  await waitFor(() => {
    const live = within(rowOf("Academic Torrents")).getByRole("checkbox", {
      name: "Enable Academic Torrents",
    });
    expect(live.getAttribute("aria-checked")).toBe("false");
  });
  // The PATCH carried only the toggle, and the toast surfaces the 403 detail.
  expect(patchCalls).toEqual([{ id: "idx_academic", body: { enabled: true } }]);
  expect(toastError).toHaveBeenCalledTimes(1);
  expect(toastError).toHaveBeenCalledWith(
    expect.stringContaining(
      "private-network indexers need allow_private_network",
    ),
  );

  // Clearing a stored URL in Edit is sent to the server (which 422s naming
  // url), never silently dropped from the PATCH body.
  fireEvent.click(
    within(rowOf("Arch Linux")).getByRole("button", { name: "Edit" }),
  );
  const dialog = await screen.findByRole("dialog");
  const urlInput = within(dialog).getByLabelText("URL") as HTMLInputElement;
  expect(urlInput.value).toBe(
    "https://jackett.example/api/v2.0/indexers/archlinux/results/torznab/",
  );
  fireEvent.change(urlInput, { target: { value: "" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(patchCalls.length).toBe(2));
  expect(patchCalls[1].id).toBe("idx_arch");
  expect(patchCalls[1].body).toMatchObject({ url: "" });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
});

test("TestProbeFailureRendersAsData", async () => {
  const toastError = vi.spyOn(toast, "error");
  server.use(
    http.post("*/api/v1/indexers/idx_arch/test", () =>
      HttpResponse.json({
        ok: false,
        elapsed_ms: 5,
        categories_found: 0,
        server: "Jackett 0.24.x",
        error: "HTTP 404 on the caps endpoint",
      }),
    ),
  );
  mount();
  await screen.findByText("Arch Linux", { selector: "span.font-medium" });
  const row = rowOf("Arch Linux");
  fireEvent.click(within(row).getByRole("button", { name: "Test" }));
  await screen.findByText("HTTP 404 on the caps endpoint");
  const live = rowOf("Arch Linux");
  const verdict = within(live).getByText("failed").closest("dl") as HTMLElement;
  expect(within(verdict).getByText("Jackett 0.24.x")).toBeTruthy();
  expect(within(verdict).getByText("5")).toBeTruthy();
  expect(toastError).not.toHaveBeenCalled();
});

test("TestTestAllRunsEveryRow", async () => {
  mount();
  await screen.findByText("Internet Archive", {
    selector: "span.font-medium",
  });
  fireEvent.click(screen.getByRole("button", { name: "Test all" }));
  await waitFor(() =>
    expect(testCalls).toEqual([
      "idx_archive",
      "idx_arch",
      "idx_academic",
      "idx_ubuntu",
    ]),
  );
  await waitFor(() => expect(screen.getAllByText("passed").length).toBe(4));
});

test("TestMoveUpSwapsPriorities", async () => {
  mount();
  await screen.findByText("Arch Linux", { selector: "span.font-medium" });
  fireEvent.click(
    within(rowOf("Arch Linux")).getByRole("button", {
      name: "Move Arch Linux up",
    }),
  );
  await waitFor(() => expect(patchCalls.length).toBe(2));
  expect(patchCalls).toEqual([
    { id: "idx_arch", body: { priority: 10 } },
    { id: "idx_archive", body: { priority: 20 } },
  ]);

  // The exported helper decides the PATCH set: the first row and an
  // equal-priority neighbour are both no-ops, issuing no request.
  expect(swapPriority(rows, "idx_archive", -1)).toEqual([]);
  expect(swapPriority(rows, "idx_ubuntu", 1)).toEqual([]);
  const tied = rows.map((row) =>
    row.id === "idx_arch" ? { ...row, priority: 10 } : row,
  );
  expect(swapPriority(tied, "idx_arch", -1)).toEqual([]);
  expect(swapPriority(tied, "idx_arch", 1)).toEqual([
    { id: "idx_arch", priority: 30 },
    { id: "idx_academic", priority: 10 },
  ]);

  // If the second swap PATCH fails, the already-applied first PATCH is
  // undone so the pair never shares a priority the UI cannot reorder.
  const toastError = vi.spyOn(toast, "error");
  patchCalls.length = 0;
  server.use(
    http.patch("*/api/v1/indexers/idx_archive", async ({ request }) => {
      patchCalls.push({
        id: "idx_archive",
        body: (await request.json()) as Record<string, unknown>,
      });
      return HttpResponse.json(
        {
          type: "/problems/conflict",
          title: "Conflict",
          detail: "priority changed by another client",
          status: 409,
        },
        { status: 409 },
      );
    }),
  );
  fireEvent.click(
    within(rowOf("Arch Linux")).getByRole("button", {
      name: "Move Arch Linux up",
    }),
  );
  await waitFor(() => expect(patchCalls.length).toBe(3));
  expect(patchCalls).toEqual([
    { id: "idx_arch", body: { priority: 10 } },
    { id: "idx_archive", body: { priority: 20 } },
    { id: "idx_arch", body: { priority: 20 } },
  ]);
  expect(toastError).toHaveBeenCalledWith(
    expect.stringContaining("priority changed by another client"),
  );
  // The post-move refetch re-renders the server's (unchanged) order.
  await waitFor(() => {
    const names = [
      ...document.querySelectorAll("tbody tr td:first-child span.font-medium"),
    ].map((cell) => cell.textContent);
    expect(names).toEqual([
      "Internet Archive",
      "Arch Linux",
      "Academic Torrents",
      "Ubuntu",
    ]);
  });
});

test("TestImportedIndexerRendersDisabledWithProvenance", async () => {
  const imported = indexer({
    id: "idx_imported",
    name: "Imported One",
    enabled: false,
    definition_id: "imported-one",
    definition_source: "imported",
    provenance: "imported from jackett.dlm",
    legal_tier: "user-supplied",
    priority: 60,
  });
  let importContentType = "";
  server.use(
    http.post("*/api/v1/indexers/import", async ({ request }) => {
      importContentType = request.headers.get("content-type") ?? "";
      const form = await request.formData();
      const file = form.get("file");
      if (
        file === null ||
        typeof file === "string" ||
        file.name !== "jackett.dlm"
      )
        throw new Error("the file part did not arrive");
      rows = [...rows, imported];
      return HttpResponse.json(
        {
          indexer: imported,
          warnings: [
            "search.php used a POST form; converted to a GET query",
            "capabilities block was dropped",
          ],
        },
        { status: 201 },
      );
    }),
  );
  mount();
  await screen.findByText("Internet Archive", {
    selector: "span.font-medium",
  });
  fireEvent.click(screen.getByRole("button", { name: "Import" }));
  const dialog = await screen.findByRole("dialog");
  fireEvent.change(within(dialog).getByLabelText("Definition file"), {
    target: { files: [new File(["%YAML%"], "jackett.dlm")] },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Import" }));
  // The 201 body's warnings render verbatim; the import is never optimistic.
  await within(dialog).findByText(
    "search.php used a POST form; converted to a GET query",
  );
  await within(dialog).findByText("capabilities block was dropped");
  expect(importContentType).toContain("multipart/form-data");
  fireEvent.click(within(dialog).getByRole("button", { name: "Done" }));

  // After the refetch the imported row is there, disabled, with provenance.
  await screen.findByText("Imported One", { selector: "span.font-medium" });
  const row = rowOf("Imported One");
  expect(within(row).getByText("imported from jackett.dlm")).toBeTruthy();
  expect(
    within(row)
      .getByRole("checkbox", { name: "Enable Imported One" })
      .getAttribute("aria-checked"),
  ).toBe("false");

  // A file import blocked by SSRF names the file, not an empty address.
  const toastError = vi.spyOn(toast, "error");
  server.use(
    http.post("*/api/v1/indexers/import", () =>
      HttpResponse.json(
        {
          type: "/problems/ssrf-blocked",
          title: "Forbidden",
          detail: "the indexer's fetch_url is a private address",
          status: 403,
        },
        { status: 403 },
      ),
    ),
  );
  fireEvent.click(screen.getByRole("button", { name: "Import" }));
  const retry = await screen.findByRole("dialog");
  fireEvent.change(within(retry).getByLabelText("Definition file"), {
    target: { files: [new File(["%YAML%"], "blocked.dlm")] },
  });
  fireEvent.click(within(retry).getByRole("button", { name: "Import" }));
  await waitFor(() => expect(toastError).toHaveBeenCalledTimes(1));
  expect(toastError).toHaveBeenCalledWith(
    expect.stringContaining("An address inside blocked.dlm is blocked"),
  );
});

test("TestApiKeyNeverRendered", async () => {
  // The stubbed key is typed into each dialog's password field; no DOM text
  // node may ever contain it. (Attribute serialization is not asserted:
  // happy-dom reflects the input's value *property* into the attribute list,
  // which no real DOM does and which is never rendered.)
  const STUBBED = "k3y-that-must-never-render";
  const assertAbsent = () => {
    const walker = document.createTreeWalker(
      document.body,
      NodeFilter.SHOW_TEXT,
    );
    let node = walker.nextNode();
    while (node !== null) {
      expect(node.textContent ?? "").not.toContain(STUBBED);
      node = walker.nextNode();
    }
  };
  mount();
  await screen.findByText("Internet Archive", {
    selector: "span.font-medium",
  });

  // Add mode.
  fireEvent.click(screen.getByRole("button", { name: "Add" }));
  let dialog = await screen.findByRole("dialog");
  fireEvent.change(within(dialog).getByLabelText("API key"), {
    target: { value: STUBBED },
  });
  assertAbsent();
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

  // Edit mode: the stored key never seeds the field either.
  fireEvent.click(
    within(rowOf("Arch Linux")).getByRole("button", { name: "Edit" }),
  );
  dialog = await screen.findByRole("dialog");
  const editKey = within(dialog).getByLabelText("API key") as HTMLInputElement;
  expect(editKey.value).toBe("");
  fireEvent.change(editKey, { target: { value: STUBBED } });
  assertAbsent();
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

  // Import mode.
  fireEvent.click(screen.getByRole("button", { name: "Import" }));
  dialog = await screen.findByRole("dialog");
  fireEvent.change(within(dialog).getByLabelText("API key"), {
    target: { value: STUBBED },
  });
  assertAbsent();
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
});
