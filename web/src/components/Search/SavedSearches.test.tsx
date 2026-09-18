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
import { useUiPrefs, type SavedSearch } from "../../store/useUiPrefs";
import { Toaster } from "../ui/sonner";
import { SearchScreen } from "./SearchScreen";
import {
  SaveSearchButton,
  SavedSearchesMenu,
  validateSavedName,
} from "./SavedSearches";
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
    results: unknown[];
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

const entry = (
  name: string,
  patch: Partial<SavedSearch> = {},
): SavedSearch => ({
  id: `sv_${name}`,
  name,
  query: "ubuntu",
  indexerIds: ["ix_a"],
  categories: [],
  createdAt: "2026-09-01T00:00:00Z",
  lastTotal: 0,
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
  useUiPrefs.setState(useUiPrefs.getInitialState(), true);
  // jsdom reports zero boxes; the virtualiser needs a viewport to mount rows.
  vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockReturnValue(4000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(4000);
  server.use(
    http.get("*/api/v1/indexers", () =>
      HttpResponse.json({
        indexers: [indexer("ix_a", "Indexer A"), indexer("ix_b", "Indexer B")],
      }),
    ),
    http.get("*/api/v1/indexers/categories", () =>
      HttpResponse.json({
        categories: [{ id: 2000, name: "Movies" }],
      }),
    ),
    http.get("*/api/v1/fs/roots", () => HttpResponse.json({ roots: [] })),
    http.get("*/api/v1/prefs", () => HttpResponse.json({ version: 1 })),
    http.put("*/api/v1/prefs", async ({ request }) =>
      HttpResponse.json(await request.json()),
    ),
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

function mountScreen() {
  render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <SearchScreen />
        <Toaster theme="system" />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

test("TestSaveCapturesQueryAndSelection", async () => {
  const puts: Record<string, unknown>[] = [];
  server.use(
    http.put("*/api/v1/prefs", async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      puts.push(body);
      return HttpResponse.json(body);
    }),
  );
  useUiPrefs.setState({ hydrated: true });
  render(
    <>
      <SaveSearchButton
        query="ubuntu"
        indexerIds={["ix_a", "ix_b"]}
        categories={[2000]}
      />
      <Toaster theme="system" />
    </>,
  );

  fireEvent.click(screen.getByRole("button", { name: "Save…" }));
  fireEvent.change(screen.getByLabelText("Name"), {
    target: { value: "weekly" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save" }));

  const saved = useUiPrefs.getState().search.saved;
  expect(saved).toHaveLength(1);
  expect(saved[0]).toMatchObject({
    name: "weekly",
    query: "ubuntu",
    indexerIds: ["ix_a", "ix_b"],
    categories: [2000],
    lastTotal: 0,
  });
  // The capture lands in the server document through the debounced PUT.
  await waitFor(() => expect(puts).toHaveLength(1), { timeout: 3000 });
  expect(puts[0].search).toMatchObject({
    saved: [{ name: "weekly", indexerIds: ["ix_a", "ix_b"] }],
  });
});

test("TestRunSavedSearchUsesStoredIndexers", async () => {
  const posted: unknown[] = [];
  server.use(
    http.post("*/api/v1/search", async ({ request }) => {
      posted.push(await request.json());
      return HttpResponse.json({ id: "sch_saved" }, { status: 202 });
    }),
    http.get("*/api/v1/search/:id", () =>
      HttpResponse.json(
        jobBody({ finished: true, total: 3, engines: [engine()] }),
      ),
    ),
  );
  // The live selection is ix_a; the saved entry stores ix_b and category
  // 2000 — the re-run must post the stored selection, never the live one.
  useUiPrefs.setState({
    hydrated: true,
    search: {
      indexerIds: ["ix_a"],
      categories: [],
      saved: [
        entry("weekly", {
          query: "debian",
          indexerIds: ["ix_b"],
          categories: [2000],
          lastTotal: 1,
        }),
      ],
    },
  });

  mountScreen();
  fireEvent.click(screen.getByRole("button", { name: /^Saved/ }));
  fireEvent.click(await screen.findByRole("button", { name: "weekly" }));

  await waitFor(() => expect(posted).toHaveLength(1));
  expect(posted[0]).toEqual({
    query: "debian",
    indexer_ids: ["ix_b"],
    categories: [2000],
  });
  // The finished run wrote lastTotal back to the entry and badged the gain.
  await waitFor(() =>
    expect(useUiPrefs.getState().search.saved[0]?.lastTotal).toBe(3),
  );
});

test("TestFailedSavedRunChargesNothingToTheEntry", async () => {
  let posts = 0;
  let polls = 0;
  server.use(
    http.post("*/api/v1/search", () => {
      posts += 1;
      // The saved run's start is refused; the follow-up normal search
      // succeeds and finishes with a higher total than the entry stores.
      return posts === 1
        ? HttpResponse.json({ detail: "no indexers" }, { status: 500 })
        : HttpResponse.json({ id: "sch_normal" }, { status: 202 });
    }),
    http.get("*/api/v1/search/sch_normal", () => {
      polls += 1;
      return HttpResponse.json(
        jobBody({ finished: true, total: 9, engines: [engine()] }),
      );
    }),
  );
  useUiPrefs.setState({
    hydrated: true,
    search: {
      indexerIds: ["ix_a"],
      categories: [],
      saved: [entry("weekly", { lastTotal: 1 })],
    },
  });

  mountScreen();
  fireEvent.click(screen.getByRole("button", { name: /^Saved/ }));
  fireEvent.click(await screen.findByRole("button", { name: "weekly" }));
  await waitFor(() => expect(posts).toBe(1));

  // An unrelated search finishing next must not be charged to the saved
  // entry — its lastTotal stays at the stored value.
  fireEvent.change(screen.getByLabelText("Search query"), {
    target: { value: "fedora" },
  });
  await waitFor(() =>
    expect(
      (screen.getByRole("button", { name: "Search" }) as HTMLButtonElement)
        .disabled,
    ).toBe(false),
  );
  fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await waitFor(() => expect(posts).toBe(2));
  // Once the poll lands and the finished render commits (Stop disables),
  // any incorrect write-back to the entry would already have landed.
  await waitFor(() => expect(polls).toBeGreaterThan(0));
  await waitFor(() =>
    expect(
      (
        screen.getByRole("button", {
          name: "Stop this search",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true),
  );
  expect(useUiPrefs.getState().search.saved[0]?.lastTotal).toBe(1);
});

test("TestFinishedSavedRunChargesItsEntry", async () => {
  // The positive counterpart to the refused-start test: a saved run whose
  // job id matches the live job must write lastTotal back — a regression
  // that drops every run would otherwise look identical to the negative
  // case above.
  server.use(
    http.post("*/api/v1/search", () =>
      HttpResponse.json({ id: "sch_saved" }, { status: 202 }),
    ),
    http.get("*/api/v1/search/sch_saved", () =>
      HttpResponse.json(
        jobBody({ finished: true, total: 5, engines: [engine()] }),
      ),
    ),
  );
  useUiPrefs.setState({
    hydrated: true,
    search: {
      indexerIds: ["ix_a"],
      categories: [],
      saved: [entry("weekly", { lastTotal: 2 })],
    },
  });

  mountScreen();
  fireEvent.click(screen.getByRole("button", { name: /^Saved/ }));
  fireEvent.click(await screen.findByRole("button", { name: "weekly" }));
  await waitFor(() =>
    expect(useUiPrefs.getState().search.saved[0]?.lastTotal).toBe(5),
  );
});

test("TestDuplicateNameRefused", async () => {
  const saved = [entry("daily")];
  useUiPrefs.setState({
    hydrated: true,
    search: { indexerIds: [], categories: [], saved },
  });
  expect(validateSavedName("daily", saved)).toBe("duplicate");
  expect(validateSavedName("fresh", saved)).toBeNull();
  expect(validateSavedName("   ", saved)).toBe("empty");

  render(
    <>
      <SaveSearchButton query="ubuntu" indexerIds={["ix_a"]} categories={[]} />
      <Toaster theme="system" />
    </>,
  );
  fireEvent.click(screen.getByRole("button", { name: "Save…" }));
  fireEvent.change(screen.getByLabelText("Name"), {
    target: { value: "daily" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save" }));

  expect(
    await screen.findByText("A saved search with that name already exists."),
  ).toBeTruthy();
  expect(useUiPrefs.getState().search.saved).toHaveLength(1);
});

test("TestFiftyEntryCap", async () => {
  const saved = Array.from({ length: 50 }, (_, i) => entry(`s${i}`));
  useUiPrefs.setState({
    hydrated: true,
    search: { indexerIds: [], categories: [], saved },
  });
  render(
    <>
      <SaveSearchButton query="ubuntu" indexerIds={["ix_a"]} categories={[]} />
      <Toaster theme="system" />
    </>,
  );
  fireEvent.click(screen.getByRole("button", { name: "Save…" }));
  fireEvent.change(screen.getByLabelText("Name"), {
    target: { value: "fifty-one" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save" }));

  // The 51st save is refused with a toast naming the cap.
  expect(
    await screen.findByText(/Saved searches are capped at 50/),
  ).toBeTruthy();
  expect(useUiPrefs.getState().search.saved).toHaveLength(50);
});

test("TestPrefsDocumentRoundTrips", async () => {
  const puts: Record<string, unknown>[] = [];
  server.use(
    http.put("*/api/v1/prefs", async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      puts.push(body);
      return HttpResponse.json(body);
    }),
  );
  useUiPrefs.setState({ hydrated: true });
  const search = {
    indexerIds: ["ix_a"],
    categories: [2000],
    saved: [entry("weekly")],
  };
  useUiPrefs.getState().patch({ search });
  await waitFor(() => expect(puts).toHaveLength(1), { timeout: 3000 });
  // The member travels whole inside the document.
  expect(puts[0].search).toEqual(search);

  // …and the same document read back through GET /prefs restores it.
  useUiPrefs.getState().reset();
  server.use(http.get("*/api/v1/prefs", () => HttpResponse.json(puts[0])));
  await useUiPrefs.getState().hydrate();
  expect(useUiPrefs.getState().search).toEqual(search);
});

test("TestSanitizeDropsDuplicateSavedIds", async () => {
  // A document another client corrupted: two entries share an id, and the
  // stored selections repeat members. Consumers key entries off id, so the
  // boundary keeps only the first occurrence of each.
  server.use(
    http.get("*/api/v1/prefs", () =>
      HttpResponse.json({
        version: 1,
        search: {
          indexerIds: ["ix_a", "ix_a", "ix_b"],
          categories: [2000, 2000],
          saved: [
            entry("weekly"),
            { ...entry("nightly"), id: "sv_weekly" },
            entry("daily"),
          ],
        },
      }),
    ),
  );
  await useUiPrefs.getState().hydrate();

  const search = useUiPrefs.getState().search;
  expect(search.indexerIds).toEqual(["ix_a", "ix_b"]);
  expect(search.categories).toEqual([2000]);
  expect(search.saved.map((s) => s.id)).toEqual(["sv_weekly", "sv_daily"]);
});

test("TestRenameAndDeleteUpdateTheDocument", async () => {
  const saved = [entry("daily"), entry("weekly")];
  const runs: SavedSearch[] = [];
  const puts: Record<string, unknown>[] = [];
  server.use(
    http.put("*/api/v1/prefs", async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      puts.push(body);
      return HttpResponse.json(body);
    }),
  );
  useUiPrefs.setState({
    hydrated: true,
    search: { indexerIds: [], categories: [], saved },
  });
  render(
    <>
      <SavedSearchesMenu
        onRun={(s) => {
          runs.push(s);
        }}
      />
      <Toaster theme="system" />
    </>,
  );

  fireEvent.click(screen.getByRole("button", { name: /^Saved/ }));
  // Rename "daily" to "nightly"; the new name stays unique-checked.
  fireEvent.click(await screen.findByRole("button", { name: "Rename daily" }));
  const input = screen.getByRole("textbox", { name: "Rename daily" });
  fireEvent.change(input, { target: { value: "weekly" } });
  fireEvent.click(screen.getByRole("button", { name: "Confirm rename" }));
  expect(
    await screen.findByText("A saved search with that name already exists."),
  ).toBeTruthy();
  fireEvent.change(input, { target: { value: "nightly" } });
  fireEvent.click(screen.getByRole("button", { name: "Confirm rename" }));
  await waitFor(() =>
    expect(useUiPrefs.getState().search.saved.map((s) => s.name)).toEqual([
      "nightly",
      "weekly",
    ]),
  );

  // Deleting is immediate.
  fireEvent.click(await screen.findByRole("button", { name: "Delete weekly" }));
  await waitFor(() =>
    expect(useUiPrefs.getState().search.saved).toHaveLength(1),
  );
  expect(useUiPrefs.getState().search.saved[0]?.name).toBe("nightly");

  // The rename and the delete each mark the document dirty; the debounced
  // writer may coalesce them, so the wait holds until the last PUT body the
  // server saw carries the surviving entry — flushing the writes before
  // afterEach resets the handlers.
  await waitFor(
    () =>
      expect(
        (puts.at(-1)?.search as { saved?: { name: string }[] })?.saved,
      ).toEqual([expect.objectContaining({ name: "nightly" })]),
    { timeout: 3000 },
  );
});
