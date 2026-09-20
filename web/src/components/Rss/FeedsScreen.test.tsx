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
import { Toaster } from "../ui/sonner";
import { FeedsScreen } from "./FeedsScreen";
import type { components } from "../../api/schema";

type FeedDTO = components["schemas"]["FeedDTO"];
type FeedItemDTO = components["schemas"]["FeedItemDTO"];

const feed = (over: Partial<FeedDTO>): FeedDTO => ({
  id: "fed_x",
  url: "https://x.example/feed",
  title: "X",
  enabled: true,
  refresh_interval_s: 0,
  item_cap: 50,
  priority: 0,
  unread_count: 0,
  last_fetch_at: null,
  last_success_at: null,
  next_fetch_at: "2026-09-20T00:00:00Z",
  escalation_level: 0,
  disabled_till: null,
  last_error: null,
  ...over,
});

const item = (over: Partial<FeedItemDTO>): FeedItemDTO => ({
  id: "itm_x",
  feed_id: "fed_arch",
  title: "x",
  link: null,
  download_url: "https://x.example/dl",
  info_hash: null,
  size_bytes: null,
  published_at: null,
  read: false,
  matched_rules: [],
  ...over,
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
let feeds: FeedDTO[];
let itemsByFeed: Record<string, FeedItemDTO[]>;

function mount() {
  return render(
    <QueryClientProvider client={qc}>
      <FeedsScreen />
      <Toaster theme="system" />
    </QueryClientProvider>,
  );
}

beforeAll(() => {
  initI18n();
  server.listen({ onUnhandledRequest: "error" });
});
beforeEach(() => {
  qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  feeds = [
    feed({
      id: "fed_arch",
      title: "Arch",
      url: "https://arch.example/feed",
      unread_count: 9,
    }),
    feed({
      id: "fed_acad",
      title: "Academic",
      url: "https://acad.example/feed",
      unread_count: 3,
    }),
    feed({
      id: "fed_broken",
      title: "BrokenFeed",
      url: "https://broken.example/feed",
      last_error: "HTTP 503",
    }),
  ];
  itemsByFeed = {
    fed_arch: [
      item({
        id: "itm_1",
        title: "archlinux-2026.09.01-x86_64.iso",
        link: "https://arch.example/releng/2026.09.01/",
        download_url: "https://arch.example/1.torrent",
        size_bytes: 1181116006,
        published_at: "2026-09-01T02:15:00Z",
        matched_rules: [{ id: "rul_iso", name: "Linux ISOs" }],
      }),
      item({
        id: "itm_2",
        title: "archlinux-2026.08.01-x86_64.iso",
        download_url: "https://arch.example/2.torrent",
        published_at: "2026-08-01T02:15:00Z",
        read: true,
      }),
    ],
    fed_acad: [
      item({
        id: "itm_3",
        feed_id: "fed_acad",
        title: "Some.Dataset.2026",
        download_url: "https://acad.example/3.torrent",
        published_at: "2026-08-30T00:00:00Z",
      }),
    ],
    fed_broken: [],
  };
  server.use(
    http.get("*/api/v1/feeds", () => HttpResponse.json({ feeds })),
    http.get("*/api/v1/feeds/:id/items", ({ params }) =>
      HttpResponse.json({
        items: itemsByFeed[String(params.id)] ?? [],
        next_cursor: null,
        total: (itemsByFeed[String(params.id)] ?? []).length,
      }),
    ),
  );
  localStorage.clear();
  const base = document.createElement("base");
  base.dataset.test = "feeds-screen";
  base.href = "/";
  document.head.appendChild(base);
});
afterEach(() => {
  cleanup();
  qc.clear();
  server.resetHandlers();
  vi.restoreAllMocks();
  // Only the base tag this suite appended — an App-provided one is not ours.
  document.querySelector('base[data-test="feeds-screen"]')?.remove();
  window.history.replaceState(null, "", "/");
});
afterAll(() => server.close());

test("TestFeedListRendersStatesAndCounts", async () => {
  // A refresh that settles only when released keeps the feed in the
  // loading state for the assertion.
  const gate: { release?: () => void } = {};
  server.use(
    http.post(
      "*/api/v1/feeds/:id/refresh",
      () =>
        new Promise((resolve) => {
          gate.release = () =>
            resolve(
              HttpResponse.json({
                fetched: true,
                not_modified: false,
                items_added: 0,
                elapsed_ms: 1,
                error: null,
              }),
            );
        }),
    ),
  );
  mount();
  const arch = await screen.findByRole("button", { name: /Arch/ });
  expect(within(arch).getByText("●")).toBeTruthy();
  expect(within(arch).getByText("9")).toBeTruthy();
  const acad = screen.getByRole("button", { name: /Academic/ });
  expect(within(acad).getByText("3")).toBeTruthy();
  const broken = screen.getByRole("button", { name: /BrokenFeed/ });
  expect(within(broken).getByText("⚠")).toBeTruthy();
  expect(broken.getAttribute("title")).toBe("HTTP 503");

  fireEvent.click(arch);
  fireEvent.click(screen.getByRole("button", { name: "Update" }));
  await waitFor(() => expect(within(arch).getByText("◐")).toBeTruthy());
  gate.release?.();
  // Let the mutation settle so the test does not end mid-refresh.
  await waitFor(() => expect(within(arch).queryByText("◐")).toBeNull());
});

test("TestRefreshShowsItemsAdded", async () => {
  const refreshCalls: string[] = [];
  server.use(
    http.post("*/api/v1/feeds/:id/refresh", ({ params }) => {
      refreshCalls.push(String(params.id));
      return HttpResponse.json({
        fetched: true,
        not_modified: false,
        items_added: 2,
        elapsed_ms: 318,
        error: null,
      });
    }),
  );
  mount();
  fireEvent.click(await screen.findByRole("button", { name: /Arch/ }));
  fireEvent.click(screen.getByRole("button", { name: "Update" }));
  await screen.findByText("2 new items");
  expect(refreshCalls).toEqual(["fed_arch"]);
});

test("TestDownloadSelectedPostsDownloadUrls", async () => {
  let tasksBody: unknown = null;
  server.use(
    http.post("*/api/v1/tasks", async ({ request }) => {
      tasksBody = await request.json();
      return HttpResponse.json({
        created: [{ id: "tsk_1" }, { id: "tsk_2" }],
        rejected: [],
      });
    }),
  );
  mount();
  await screen.findByText("archlinux-2026.09.01-x86_64.iso");
  fireEvent.click(
    screen.getByRole("checkbox", {
      name: "Select archlinux-2026.09.01-x86_64.iso",
    }),
  );
  fireEvent.click(
    screen.getByRole("checkbox", {
      name: "Select archlinux-2026.08.01-x86_64.iso",
    }),
  );
  fireEvent.click(screen.getByRole("button", { name: "Download selected" }));
  await screen.findByText("2 tasks created");
  expect(tasksBody).toEqual({
    uris: ["https://arch.example/1.torrent", "https://arch.example/2.torrent"],
  });
});

test("TestDuplicateFeedShowsConflictMessage", async () => {
  server.use(
    http.post("*/api/v1/feeds", () =>
      HttpResponse.json(
        {
          type: "/problems/conflict",
          title: "Conflict",
          detail: "a feed with this url already exists",
        },
        { status: 409 },
      ),
    ),
  );
  mount();
  await screen.findByRole("button", { name: /Arch/ });
  fireEvent.click(screen.getByRole("button", { name: "Add feed" }));
  const dialog = await screen.findByRole("dialog");
  fireEvent.change(within(dialog).getByLabelText("URL"), {
    target: { value: "https://arch.example/feed" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Add feed" }));
  await within(dialog).findByText("This feed is already subscribed");
});

test("TestItemPageLoadsAndPaginates", async () => {
  const itemUrls: URL[] = [];
  server.use(
    http.get("*/api/v1/feeds/fed_arch/items", ({ request }) => {
      const url = new URL(request.url);
      itemUrls.push(url);
      if (url.searchParams.get("cursor") === "c2")
        return HttpResponse.json({
          items: [item({ id: "itm_2p", title: "Second page item" })],
          next_cursor: null,
          total: 2,
        });
      return HttpResponse.json({
        items: [item({ id: "itm_1p", title: "First page item" })],
        next_cursor: "c2",
        total: 2,
      });
    }),
  );
  mount();
  await screen.findByText("First page item");
  await screen.findByText("Second page item");
  expect(itemUrls.length).toBe(2);
  expect(itemUrls[0].searchParams.get("limit")).toBe("500");
  expect(itemUrls[0].searchParams.get("cursor")).toBeNull();
  expect(itemUrls[1].searchParams.get("cursor")).toBe("c2");
});

test("TestFeedListErrorShowsRetryNotEmptyState", async () => {
  server.use(
    http.get("*/api/v1/feeds", () =>
      HttpResponse.json(
        { type: "/problems/internal", title: "Internal", detail: "boom" },
        { status: 500 },
      ),
    ),
  );
  mount();
  expect(await screen.findByText("Could not load the feed list.")).toBeTruthy();
  expect(screen.queryByText(/Add an RSS feed/)).toBeNull();
  server.use(http.get("*/api/v1/feeds", () => HttpResponse.json({ feeds })));
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await screen.findByRole("button", { name: /Arch/ });
});

test("TestItemRowKeyboardActivatesPreview", async () => {
  mount();
  const title = await screen.findByText("archlinux-2026.09.01-x86_64.iso");
  const row = title.closest("tr");
  expect(row).not.toBeNull();
  fireEvent.keyDown(row as Element, { key: "Enter" });
  const preview = screen.getByRole("region", { name: "Preview" });
  await within(preview).findByText("archlinux-2026.09.01-x86_64.iso");
});

test("TestNonHttpItemLinkIsNotRendered", async () => {
  itemsByFeed.fed_arch = [
    item({
      id: "itm_js",
      title: "scripted item",
      link: "javascript:alert(1)",
      download_url: "https://arch.example/js.torrent",
    }),
  ];
  mount();
  const title = await screen.findByText("scripted item");
  fireEvent.click(title.closest("tr") as Element);
  const preview = screen.getByRole("region", { name: "Preview" });
  await within(preview).findByText("scripted item");
  expect(within(preview).queryByRole("link", { name: "Open link" })).toBeNull();
});

test("TestRemoveFeedClearsFolderAssignment", async () => {
  const deleted: string[] = [];
  server.use(
    http.delete("*/api/v1/feeds/:id", ({ params }) => {
      deleted.push(String(params.id));
      feeds = feeds.filter((f) => f.id !== params.id);
      return new HttpResponse(null, { status: 204 });
    }),
  );
  // Seed the map before mount so the screen hydrates it.
  localStorage.setItem(
    "dl.rss.folders.v1",
    JSON.stringify({ fed_arch: "linux" }),
  );
  mount();
  const arch = await screen.findByRole("button", { name: /Arch/ });
  fireEvent.contextMenu(arch);
  fireEvent.click(await screen.findByRole("menuitem", { name: "Remove" }));
  const dialog = await screen.findByRole("dialog");
  fireEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
  await waitFor(() =>
    expect(screen.queryByRole("button", { name: /Arch/ })).toBeNull(),
  );
  expect(deleted).toEqual(["fed_arch"]);
  expect(JSON.parse(localStorage.getItem("dl.rss.folders.v1") ?? "{}")).toEqual(
    {},
  );
});

test("TestRssFeedsRouteResolvesAndSidebarMarksCurrent", async () => {
  server.use(
    http.get("*/api/v1/auth/me", () => HttpResponse.json(session)),
    http.get("*/api/v1/prefs", () => HttpResponse.json({})),
    http.put("*/api/v1/prefs", () => HttpResponse.json({})),
    http.get("*/api/v1/tasks", () =>
      HttpResponse.json({ items: [], total: 0, next_cursor: null }),
    ),
    // The stream's outage fallback probes /sync; answer it so the mount
    // stays quiet under onUnhandledRequest: "error".
    http.get("*/api/v1/sync", () =>
      HttpResponse.json({
        rid: 1,
        full_update: true,
        seq_gap: false,
        tasks: {},
        stats: { active: 0, queued: 0, speed_down: 0, speed_up: 0 },
      }),
    ),
  );
  window.history.replaceState(null, "", "/rss/feeds");
  render(<App />);
  const link = await screen.findByRole("link", { name: "Feeds" });
  expect(link.getAttribute("aria-current")).toBe("page");
  await within(screen.getByRole("main")).findByText("Arch");
});
