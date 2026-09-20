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
import { initI18n } from "../../i18n";
import { Toaster } from "../ui/sonner";
import { RuleEditor, PREVIEW_LIMIT } from "./RuleEditor";
import type { components } from "../../api/schema";

type RuleDTO = components["schemas"]["RuleDTO"];
type RuleDoc = components["schemas"]["RuleDoc"];
type FeedDTO = components["schemas"]["FeedDTO"];
type DryRunItem = components["schemas"]["DryRunItem"];

const doc = (over: Partial<RuleDoc>): RuleDoc => ({
  name: "Linux ISOs",
  enabled: true,
  match: { mode: "wildcard", fields: ["title"], any_of: ["*iso*"] },
  action: { destination: "/data/iso", category: "linux", paused: true },
  ...over,
});

const rule = (over: Partial<RuleDTO>): RuleDTO => ({
  id: "rul_iso",
  name: "Linux ISOs",
  enabled: true,
  priority: 0,
  definition: doc({}),
  last_match_at: null,
  created_at: "2026-09-01T00:00:00Z",
  updated_at: "2026-09-01T00:00:00Z",
  ...over,
});

const feed = (over: Partial<FeedDTO>): FeedDTO => ({
  id: "fed_arch",
  url: "https://arch.example/feed",
  title: "Arch",
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

const result = (over: Partial<DryRunItem>): DryRunItem => ({
  feed_id: "fed_arch",
  feed: "Arch",
  title: "archlinux-2026.09.01-x86_64.iso",
  published_at: "2026-09-01T02:15:00Z",
  download_url: "https://arch.example/1.torrent",
  matched: true,
  score: 10,
  matched_by: { any_of: "*iso*" },
  ...over,
});

const report = (results: DryRunItem[]) => ({
  evaluated: results.length,
  matched: results.filter((row) => row.matched).length,
  elapsed_ms: 4,
  results,
});

const server = setupServer();
let qc: QueryClient;
let rules: RuleDTO[];
let dryRunBodies: Record<string, unknown>[];
let testResponse: () => ReturnType<typeof HttpResponse.json>;

function mount() {
  return render(
    <QueryClientProvider client={qc}>
      <RuleEditor />
      <Toaster theme="system" />
    </QueryClientProvider>,
  );
}

async function settlePreview() {
  await waitFor(() => expect(dryRunBodies.length).toBeGreaterThan(0));
}

beforeAll(() => {
  initI18n();
  server.listen({ onUnhandledRequest: "error" });
});
beforeEach(() => {
  qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  rules = [rule({})];
  dryRunBodies = [];
  testResponse = () =>
    HttpResponse.json(
      report([
        result({ title: "archlinux-2026.09.01-x86_64.iso" }),
        result({
          title: "Some.Dataset.2026",
          matched: false,
          reason: "no_match",
          reason_detail: 'any_of[0] token "iso" not found',
          matched_by: undefined,
          score: undefined,
        }),
      ]),
    );
  server.use(
    http.get("*/api/v1/feeds", () => HttpResponse.json({ feeds: [feed({})] })),
    http.get("*/api/v1/rules", () => HttpResponse.json({ rules })),
    http.get("*/api/v1/categories", () =>
      HttpResponse.json({
        categories: [
          { name: "linux", save_path: "/data/linux", task_count: 2 },
        ],
      }),
    ),
    http.post("*/api/v1/rules/test", async ({ request }) => {
      dryRunBodies.push((await request.json()) as Record<string, unknown>);
      return testResponse();
    }),
  );
  const base = document.createElement("base");
  base.dataset.test = "rule-editor";
  base.href = "/";
  document.head.appendChild(base);
});
afterEach(() => {
  cleanup();
  qc.clear();
  server.resetHandlers();
  vi.restoreAllMocks();
  document.querySelector('base[data-test="rule-editor"]')?.remove();
});
afterAll(() => server.close());

test("TestPreviewDebouncesToOneRequest", async () => {
  mount();
  await settlePreview();
  const before = dryRunBodies.length;

  const nameInput = screen.getByLabelText("Name");
  for (const char of "abcde")
    fireEvent.change(nameInput, {
      target: { value: `Linux ISOs${char}` },
    });

  await waitFor(() => expect(dryRunBodies.length).toBe(before + 1));
  // Past the 250 ms window no second request may land.
  await new Promise((resolve) => setTimeout(resolve, 350));
  expect(dryRunBodies.length).toBe(before + 1);
});

test("TestPreviewListsNonMatchesWithReason", async () => {
  mount();
  await screen.findByText("archlinux-2026.09.01-x86_64.iso");
  expect(screen.getByText("Some.Dataset.2026")).toBeTruthy();
  expect(
    screen.getByText(/no match — any_of\[0\] token "iso" not found/),
  ).toBeTruthy();
  // The headline counts matches among the displayed rows.
  expect(
    screen.getByText(`matches 1 of the last ${PREVIEW_LIMIT} items`),
  ).toBeTruthy();
});

test("TestInvalidRegexShowsInlineErrorAndKeepsPreview", async () => {
  mount();
  await screen.findByText("archlinux-2026.09.01-x86_64.iso");

  server.use(
    http.post("*/api/v1/rules/test", async ({ request }) => {
      dryRunBodies.push((await request.json()) as Record<string, unknown>);
      return HttpResponse.json(
        {
          type: "/problems/validation-failed",
          title: "Validation failed",
          status: 422,
          errors: [
            {
              location: "body.rule.match.any_of[0]",
              message: "the pattern does not compile: unterminated group",
            },
          ],
        },
        { status: 422 },
      );
    }),
  );

  fireEvent.click(screen.getByLabelText("Use regular expressions"));
  fireEvent.change(screen.getByLabelText("Must contain"), {
    target: { value: "(unterminated" },
  });

  await screen.findByRole("alert");
  expect(
    screen.getByText(/the pattern does not compile: unterminated group/),
  ).toBeTruthy();
  // The last valid preview stays on screen.
  expect(screen.getByText("archlinux-2026.09.01-x86_64.iso")).toBeTruthy();
  expect(
    screen.getByText(`matches 1 of the last ${PREVIEW_LIMIT} items`),
  ).toBeTruthy();
});

test("TestSaveSendsMappedDocument", async () => {
  const posted: Record<string, unknown>[] = [];
  server.use(
    http.post("*/api/v1/rules", async ({ request }) => {
      posted.push((await request.json()) as Record<string, unknown>);
      return HttpResponse.json(rule({ id: "rul_new" }), { status: 201 });
    }),
  );
  mount();
  await settlePreview();

  fireEvent.click(screen.getByRole("button", { name: "Add rule" }));
  fireEvent.change(screen.getByLabelText("Name"), {
    target: { value: "ISO rules" },
  });
  fireEvent.change(screen.getByLabelText("Must contain"), {
    target: { value: "x86_64 iso\namd64\n\n" },
  });
  fireEvent.change(screen.getByLabelText("Must not contain"), {
    target: { value: "bootstrap" },
  });
  fireEvent.change(screen.getByLabelText("Episode filter"), {
    target: { value: "1x2;5;8-15;" },
  });
  fireEvent.click(screen.getByLabelText("Use smart episode filter"));
  fireEvent.change(screen.getByLabelText("Destination"), {
    target: { value: "/data/iso" },
  });
  fireEvent.click(screen.getByLabelText("Add stopped"));

  const tagInput = screen.getByLabelText("Tags");
  fireEvent.change(tagInput, { target: { value: "iso" } });
  fireEvent.click(screen.getByRole("button", { name: "Add tag" }));
  fireEvent.change(tagInput, { target: { value: "linux" } });
  fireEvent.click(screen.getByRole("button", { name: "Add tag" }));

  fireEvent.click(screen.getByRole("button", { name: "Save" }));

  await waitFor(() => expect(posted.length).toBe(1));
  const body = posted[0] as {
    name: string;
    definition: RuleDoc & { action: { tags?: string[] } };
  };
  expect(body.name).toBe("ISO rules");
  expect(body.definition.name).toBe("ISO rules");
  // One entry per line, empty lines dropped, never a "|"-joined string.
  expect(body.definition.match.any_of).toEqual(["x86_64 iso", "amd64"]);
  expect(body.definition.match.none_of).toEqual(["bootstrap"]);
  expect(body.definition.episode?.filter).toBe("1x2;5;8-15;");
  expect(body.definition.episode?.smart).toBe(true);
  expect(body.definition.action.destination).toBe("/data/iso");
  expect(body.definition.action.paused).toBe(true);
  expect(body.definition.action.tags).toEqual(["iso", "linux"]);
});

test("TestHighlightConvertsUtf8Offsets", async () => {
  // "täysi ubuntu" — 'ä' is two UTF-8 bytes, so the [7,13) byte span is the
  // [6,12) code-unit span: raw slicing would mark "buntu-" instead.
  testResponse = () =>
    HttpResponse.json(
      report([
        result({
          title: "täysi ubuntu-26.04.1-desktop-amd64.iso",
          highlight: [7, 13],
        }),
      ]),
    );
  mount();
  const mark = await screen.findByText("ubuntu", { selector: "mark" });
  expect(mark.textContent).toBe("ubuntu");
});

test("TestTitlePanelPostsTitles", async () => {
  testResponse = () =>
    HttpResponse.json(
      report([
        {
          feed_id: null,
          feed: null,
          title: "ubuntu 26.04 desktop amd64",
          published_at: null,
          download_url: null,
          matched: true,
          matched_by: { any_of: "*iso*" },
          would_do: {
            destination: "/data/iso",
            category: "linux",
            paused: false,
          },
        },
      ]),
    );
  mount();
  await settlePreview();
  const before = dryRunBodies.length;

  fireEvent.change(screen.getByPlaceholderText("Test a title"), {
    target: { value: "ubuntu 26.04 desktop amd64" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Test" }));

  await screen.findByText("✓ MATCH");
  await waitFor(() => expect(dryRunBodies.length).toBe(before + 1));
  const body = dryRunBodies[dryRunBodies.length - 1];
  expect(body.titles).toEqual(["ubuntu 26.04 desktop amd64"]);
  expect(body.feeds).toBeUndefined();
});

test("TestRunPostsToRulesRun", async () => {
  const ran: string[] = [];
  server.use(
    http.post("*/api/v1/rules/:id/run", ({ params }) => {
      ran.push(String(params.id));
      return HttpResponse.json({
        evaluated: 12,
        matched: 3,
        created_task_ids: ["tsk_a", "tsk_b"],
        elapsed_ms: 8,
      });
    }),
  );
  mount();
  await screen.findByRole("button", { name: "Linux ISOs" });
  fireEvent.click(screen.getByRole("button", { name: "Linux ISOs" }));

  fireEvent.click(
    screen.getByRole("button", { name: "Run rule against existing items" }),
  );
  const dialog = await screen.findByRole("dialog");
  fireEvent.click(within(dialog).getByRole("button", { name: "Run" }));

  await waitFor(() => expect(ran).toEqual(["rul_iso"]));
});

test("TestIgnoreStateToggleReposts", async () => {
  mount();
  await settlePreview();
  const before = dryRunBodies.length;

  fireEvent.click(screen.getByLabelText("Ignore already-downloaded"));

  await waitFor(() => expect(dryRunBodies.length).toBe(before + 1));
  expect(dryRunBodies[dryRunBodies.length - 1].ignore_state).toBe(false);
});

test("TestThirteenLabelsInOrder", async () => {
  mount();
  await screen.findByText("Last match: never");
  for (const label of [
    "Enabled",
    "Use regular expressions",
    "Must contain",
    "Must not contain",
    "Episode filter",
    "Use smart episode filter",
    "Apply to feeds",
    "Destination",
    "Category",
    "Tags",
    "Add stopped",
    "Ignore subsequent matches for … days",
  ]) {
    expect(screen.getByText(label, { exact: true })).toBeTruthy();
  }
  expect(screen.getByText(/Last match:/)).toBeTruthy();
});
