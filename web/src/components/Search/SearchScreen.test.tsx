import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { HttpResponse, http } from "msw";
import { setupServer } from "msw/node";
import { MemoryRouter } from "react-router-dom";
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  expect,
  test,
  vi,
} from "vitest";

import { setCsrfToken } from "../../api/client";
import { initI18n } from "../../i18n";
import { useUiPrefs } from "../../store/useUiPrefs";
import { Toaster } from "../ui/sonner";
import { SearchScreen } from "./SearchScreen";
import type { SearchResultView } from "./ResultsGrid";
import type { components } from "../../api/schema";

type IndexerDTO = components["schemas"]["IndexerDTO"];
type EngineStatus = components["schemas"]["EngineStatus"];

const indexer = (
  id: string,
  name: string,
  patch: Partial<IndexerDTO> = {},
): IndexerDTO => ({
  id,
  name,
  kind: "torznab",
  enabled: true,
  url: "https://ix.test",
  api_key_set: false,
  categories: null,
  definition_id: null,
  definition_source: null,
  last_error: null,
  last_test_at: null,
  legal_tier: "safe",
  priority: 0,
  provenance: null,
  seeders_unknown: false,
  ...patch,
});

const result = (
  id: string,
  title: string,
  patch: Partial<SearchResultView> = {},
): SearchResultView => ({
  id,
  indexer_id: "ix_a",
  indexer_name: "Indexer A",
  title,
  info_hash: null,
  size_bytes: 1024,
  seeders: 10,
  leechers: 2,
  published_at: "2026-09-01T00:00:00Z",
  category_ids: null,
  album: null,
  artist: null,
  author: null,
  category_desc: null,
  download_volume_factor: 1,
  genre: null,
  grabs: null,
  imdb_id: null,
  language: null,
  minimum_ratio: null,
  minimum_seed_time_seconds: null,
  publisher: null,
  tmdb_id: null,
  tvdb_id: null,
  upload_volume_factor: 1,
  year: null,
  ...patch,
});

const engine = (patch: Partial<EngineStatus> = {}): EngineStatus => ({
  id: "ix_a",
  name: "Indexer A",
  status: "done",
  count: 1,
  error: null,
  ...patch,
});

const jobBody = (
  patch: Partial<{
    finished: boolean;
    total: number;
    engines: EngineStatus[];
    results: SearchResultView[];
  }> = {},
) => ({
  id: "sch_test",
  query: "ubuntu",
  finished: true,
  total: 0,
  engines: [],
  results: [],
  next_cursor: null,
  ...patch,
});

const server = setupServer();
let qc: QueryClient;

beforeAll(() => {
  initI18n();
  server.listen({ onUnhandledRequest: "error" });
});
beforeEach(() => {
  setCsrfToken("test-csrf");
  qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  sessionStorage.clear();
  useUiPrefs.setState({ lastDestination: "/data" });
  // jsdom reports zero boxes; the virtualiser needs a viewport to mount rows.
  vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockReturnValue(4000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(4000);
  server.use(
    http.get("*/api/v1/indexers", () =>
      HttpResponse.json({ indexers: [indexer("ix_a", "Indexer A")] }),
    ),
    http.get("*/api/v1/indexers/categories", () =>
      HttpResponse.json({
        categories: [{ id: 2000, name: "Movies" }],
      }),
    ),
    http.get("*/api/v1/fs/roots", () => HttpResponse.json({ roots: [] })),
  );
});
afterEach(() => {
  cleanup();
  qc.clear();
  server.resetHandlers();
  sessionStorage.clear();
  vi.restoreAllMocks();
});
afterAll(() => server.close());

function mount() {
  render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <SearchScreen />
        <Toaster theme="system" />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

async function startSearch(query = "ubuntu") {
  fireEvent.change(screen.getByLabelText("Search query"), {
    target: { value: query },
  });
  await waitFor(() =>
    expect(
      (screen.getByRole("button", { name: "Search" }) as HTMLButtonElement)
        .disabled,
    ).toBe(false),
  );
  fireEvent.click(screen.getByRole("button", { name: "Search" }));
}

test("TestPollStopsWhenFinished", async () => {
  let polls = 0;
  server.use(
    http.post("*/api/v1/search", () =>
      HttpResponse.json({ id: "sch_test" }, { status: 202 }),
    ),
    http.get("*/api/v1/search/:id", () => {
      polls += 1;
      return HttpResponse.json(
        jobBody({ finished: true, engines: [engine()], total: 0 }),
      );
    }),
  );

  mount();
  await startSearch();
  await waitFor(() => expect(polls).toBeGreaterThan(0));
  const count = polls;
  // finished:true stops the 1 s poll loop — one extra interval must not poll.
  await new Promise((resolve) => setTimeout(resolve, 1500));
  expect(polls).toBe(count);
});

test("TestPartialResultsRenderBeforeFinish", async () => {
  server.use(
    http.post("*/api/v1/search", () =>
      HttpResponse.json({ id: "sch_test" }, { status: 202 }),
    ),
    http.get("*/api/v1/search/:id", () =>
      HttpResponse.json(
        jobBody({
          finished: false,
          total: 1,
          engines: [engine({ status: "searching", count: 0 })],
          results: [result("res_a", "Alpha ISO")],
        }),
      ),
    ),
  );

  mount();
  await startSearch();
  // The row lands while the engine chip still shows ◐ searching.
  expect(await screen.findByText("Alpha ISO")).toBeTruthy();
  const stop = screen.getByRole("button", { name: "Stop this search" });
  expect((stop as HTMLButtonElement).disabled).toBe(false);
  const grid = screen.getByRole("grid", { name: "Search results" });
  expect(grid.getAttribute("aria-rowcount")).toBe("1");
});

test("TestEngineErrorShownInStrip", async () => {
  server.use(
    http.post("*/api/v1/search", () =>
      HttpResponse.json({ id: "sch_test" }, { status: 202 }),
    ),
    http.get("*/api/v1/search/:id", () =>
      HttpResponse.json(
        jobBody({
          finished: true,
          engines: [engine({ status: "error", error: "connection timed out" })],
        }),
      ),
    ),
  );

  mount();
  await startSearch();
  const chip = await screen.findByRole("button", { name: /Indexer A/ });
  // Hover exposes the engine's error text…
  fireEvent.pointerMove(chip, { pointerType: "mouse" });
  await screen.findByText("connection timed out");
  // …and so does taking keyboard focus (what a click produces).
  chip.focus();
  await screen.findAllByText("connection timed out");
});

test("TestThreeZeroStates", async () => {
  // (1) No enabled indexers: the call to action is the settings link.
  server.use(
    http.get("*/api/v1/indexers", () => HttpResponse.json({ indexers: [] })),
  );
  mount();
  expect(await screen.findByText("No indexers are enabled.")).toBeTruthy();
  expect(
    screen
      .getByRole("link", { name: "Configure indexers" })
      .getAttribute("href"),
  ).toBe("/settings/indexers");
  cleanup();
  qc.clear();
  sessionStorage.clear();

  // (2) Finished job, every indexer answered zero results.
  server.use(
    http.get("*/api/v1/indexers", () =>
      HttpResponse.json({ indexers: [indexer("ix_a", "Indexer A")] }),
    ),
    http.post("*/api/v1/search", () =>
      HttpResponse.json({ id: "sch_test" }, { status: 202 }),
    ),
    http.get("*/api/v1/search/:id", () =>
      HttpResponse.json(
        jobBody({ finished: true, engines: [engine({ count: 0 })], total: 0 }),
      ),
    ),
  );
  mount();
  await startSearch();
  expect(
    await screen.findByText('No results for "ubuntu" across 1 indexer.'),
  ).toBeTruthy();
  expect(screen.getByText("Try fewer filters")).toBeTruthy();
  cleanup();
  qc.clear();
  sessionStorage.clear();

  // (3) Every engine errored: the failures list and a Retry button.
  server.use(
    http.post("*/api/v1/search", () =>
      HttpResponse.json({ id: "sch_test" }, { status: 202 }),
    ),
    http.get("*/api/v1/search/:id", () =>
      HttpResponse.json(
        jobBody({
          finished: true,
          engines: [engine({ status: "error", error: "connection timed out" })],
        }),
      ),
    ),
  );
  mount();
  await startSearch();
  expect(await screen.findByText("All indexers failed.")).toBeTruthy();
  expect(screen.getByText(/connection timed out/)).toBeTruthy();
  expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
});

test("TestNullCountsRenderEmDash", async () => {
  server.use(
    http.get("*/api/v1/indexers", () =>
      HttpResponse.json({
        indexers: [indexer("ix_a", "Indexer A", { seeders_unknown: true })],
      }),
    ),
    http.post("*/api/v1/search", () =>
      HttpResponse.json({ id: "sch_test" }, { status: 202 }),
    ),
    http.get("*/api/v1/search/:id", () =>
      HttpResponse.json(
        jobBody({
          finished: true,
          total: 1,
          engines: [engine()],
          results: [
            result("res_a", "Alpha ISO", {
              seeders: null,
              leechers: null,
              size_bytes: null,
              published_at: null,
            }),
          ],
        }),
      ),
    ),
  );

  mount();
  await startSearch();
  await screen.findByText("Alpha ISO");
  // Null size, seeders, leechers and age all render the em dash, never 0/-1.
  const dashes = screen.getAllByText("—");
  expect(dashes.length).toBeGreaterThanOrEqual(4);
});

test("TestRowDownloadPostsSearchResultID", async () => {
  const posted: { body: unknown; csrf: string | null }[] = [];
  server.use(
    http.post("*/api/v1/search", () =>
      HttpResponse.json({ id: "sch_test" }, { status: 202 }),
    ),
    http.get("*/api/v1/search/:id", () =>
      HttpResponse.json(
        jobBody({
          finished: true,
          total: 1,
          engines: [engine()],
          results: [result("res_a", "Alpha ISO")],
        }),
      ),
    ),
    http.post("*/api/v1/tasks", async ({ request }) => {
      posted.push({
        body: await request.json(),
        csrf: request.headers.get("x-dltool-csrf"),
      });
      return HttpResponse.json(
        { created: [{ id: "tsk_one" }], rejected: [] },
        { status: 201 },
      );
    }),
  );

  mount();
  await startSearch();
  await screen.findByText("Alpha ISO");
  fireEvent.click(screen.getByRole("button", { name: "Download Alpha ISO" }));

  await waitFor(() => expect(posted).toHaveLength(1));
  const body = posted[0].body as Record<string, unknown>;
  // Only the opaque id and the default destination leave the client — no
  // provider URL, no magnet, no uris family.
  expect(body).toEqual({
    search_result_ids: ["res_a"],
    destination: "/data",
  });
  expect(posted[0].csrf).toBe("test-csrf");

  // The row flashes then swaps its action for a ✓ linking to the task list.
  const link = await screen.findByRole("link", {
    name: "Alpha ISO added — view task",
  });
  expect(link.getAttribute("href")).toBe("/tasks/all");
  expect(
    screen.queryByRole("button", { name: "Download Alpha ISO" }),
  ).toBeNull();
});

test("TestBulkChunkPartialRejection", async () => {
  // 101 results → three chunks: ids 0-49, 50-99, 100.
  const results = Array.from({ length: 101 }, (_, i) =>
    result(`res_${String(i).padStart(3, "0")}`, `Result ${i}`, {
      seeders: 1000 - i,
    }),
  );
  const posted: unknown[] = [];
  server.use(
    http.post("*/api/v1/search", () =>
      HttpResponse.json({ id: "sch_test" }, { status: 202 }),
    ),
    http.get("*/api/v1/search/:id", () =>
      HttpResponse.json(
        jobBody({
          finished: true,
          total: results.length,
          engines: [engine()],
          results,
        }),
      ),
    ),
    http.post("*/api/v1/tasks", async ({ request }) => {
      const body = (await request.json()) as { search_result_ids: string[] };
      posted.push(body);
      const first = body.search_result_ids[0];
      if (first === "res_000")
        // Every id in the chunk failed: the all-gone 404 has no rejected[].
        return HttpResponse.json(
          {
            type: "/problems/not-found",
            title: "Not Found",
            status: 404,
            detail: "no search results are available",
          },
          { status: 404 },
        );
      if (first === "res_050")
        // Partial: res_051 is a conflict (terminal ✓), res_052 a real
        // rejection; every other submitted id created a task.
        return HttpResponse.json(
          {
            created: body.search_result_ids
              .filter((id) => id !== "res_051" && id !== "res_052")
              .map((id) => ({ id: `tsk_${id}` })),
            rejected: [
              {
                search_result_id: "res_051",
                type: "/problems/conflict",
                detail: "already queued",
              },
              {
                search_result_id: "res_052",
                type: "/problems/not-found",
                detail: "result expired",
              },
            ],
          },
          { status: 201 },
        );
      return HttpResponse.json(
        { created: [{ id: "tsk_c" }], rejected: [] },
        { status: 201 },
      );
    }),
  );

  mount();
  await startSearch();
  await screen.findByText("Result 0");

  fireEvent.click(screen.getByRole("checkbox", { name: "Select all results" }));
  expect(screen.getByText(/101 selected/)).toBeTruthy();
  fireEvent.pointerDown(
    screen.getByRole("button", { name: /Download selected/ }),
    { button: 0, ctrlKey: false },
  );
  fireEvent.click(
    await screen.findByRole("menuitem", { name: "Download immediately" }),
  );

  await waitFor(() => expect(posted).toHaveLength(3));
  // Chunk sizes honour the 50-id cap.
  expect(
    posted.map(
      (b) => (b as { search_result_ids: string[] }).search_result_ids.length,
    ),
  ).toEqual([50, 50, 1]);

  // One error toast for the all-fail 404 chunk.
  await screen.findByText(/50 selected results failed to submit/);
  // One summary toast for the 201 chunk's non-conflict rejection, naming
  // the first rejected title; the conflict entry is excluded from it.
  await screen.findByText(/could not be added — first: Result 52/);

  // Only resolved ids carry the ✓ link; the conflict id is one of them.
  expect(
    screen.getByRole("link", { name: "Result 51 added — view task" }),
  ).toBeTruthy();
  expect(
    screen.getByRole("link", { name: "Result 100 added — view task" }),
  ).toBeTruthy();
  // The conflict-resolved row's checkbox is disabled; the real rejection
  // stays selectable for retry, as do the all-fail chunk's rows.
  expect(
    (
      screen.getByRole("checkbox", {
        name: "Select Result 51",
      }) as HTMLButtonElement
    ).disabled,
  ).toBe(true);
  expect(
    (
      screen.getByRole("checkbox", {
        name: "Select Result 52",
      }) as HTMLButtonElement
    ).disabled,
  ).toBe(false);
  expect(
    (
      screen.getByRole("checkbox", {
        name: "Select Result 0",
      }) as HTMLButtonElement
    ).disabled,
  ).toBe(false);
  expect(
    screen.queryByRole("link", { name: "Result 52 added — view task" }),
  ).toBeNull();
  // Failed rows are marked ⟲, resolved rows are not.
  expect(
    screen.getByRole("button", { name: "Download Result 52" }).textContent,
  ).toBe("⟲");
  expect(
    screen.getByRole("button", { name: "Download Result 0" }).textContent,
  ).toBe("⟲");
});

test("TestStopDeletesTheJob", async () => {
  let deleted = false;
  server.use(
    http.post("*/api/v1/search", () =>
      HttpResponse.json({ id: "sch_test" }, { status: 202 }),
    ),
    http.get("*/api/v1/search/:id", () =>
      HttpResponse.json(
        jobBody({
          finished: false,
          engines: [engine({ status: "searching" })],
        }),
      ),
    ),
    http.delete("*/api/v1/search/:id", () => {
      deleted = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  mount();
  await startSearch();
  await screen.findByText("Indexer A");
  fireEvent.click(screen.getByRole("button", { name: "Stop this search" }));
  await waitFor(() => expect(deleted).toBe(true));
  // The job is gone: the screen returns to its never-run state.
  await screen.findByText("Pick your indexers and search.");
});
