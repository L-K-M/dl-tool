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
let feedRows: Record<string, unknown>[];
let ruleRows: Record<string, unknown>[];
let settingsPatches: Record<string, unknown>[];
let feedPatches: { id: string; body: Record<string, unknown> }[];

function freshQueryClient() {
  qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
}

function mount() {
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/settings/rss"]}>
        <Routes>
          <Route path="/settings/:section" element={<SettingsScreen />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function feed(id: string, item_cap: number): Record<string, unknown> {
  return {
    id,
    url: `https://example.com/${id}.xml`,
    title: id,
    enabled: true,
    refresh_interval_s: 0,
    item_cap,
    priority: 0,
    unread_count: 0,
    disabled_till: null,
    escalation_level: 0,
    last_error: null,
    last_fetch_at: null,
    last_success_at: null,
    next_fetch_at: "2026-09-24T10:00:00Z",
  };
}

function rule(id: string, enabled: boolean): Record<string, unknown> {
  return {
    id,
    name: id,
    enabled,
    priority: 0,
    last_match_at: null,
    created_at: "2026-09-24T08:00:00Z",
    updated_at: "2026-09-24T08:00:00Z",
    definition: { name: id, match: {}, action: {} },
  };
}

beforeAll(() => {
  initI18n();
  server.listen({ onUnhandledRequest: "error" });
});
beforeEach(() => {
  freshQueryClient();
  settingsBody = { rss_enabled: true, rss_interval_s: 1800 };
  feedRows = [];
  ruleRows = [];
  settingsPatches = [];
  feedPatches = [];
  useSettingsDirty.setState({ report: null });
  server.use(
    http.get("*/api/v1/settings", () => HttpResponse.json(settingsBody)),
    http.get("*/api/v1/feeds", () => HttpResponse.json({ feeds: feedRows })),
    http.get("*/api/v1/rules", () => HttpResponse.json({ rules: ruleRows })),
    http.patch("*/api/v1/settings", async ({ request }) => {
      settingsPatches.push((await request.json()) as Record<string, unknown>);
      return HttpResponse.json(settingsBody);
    }),
    http.patch("*/api/v1/feeds/:id", async ({ request, params }) => {
      feedPatches.push({
        id: params.id as string,
        body: (await request.json()) as Record<string, unknown>,
      });
      return HttpResponse.json({});
    }),
  );
});
afterEach(() => {
  cleanup();
  server.resetHandlers();
});
afterAll(() => server.close());

test("TestIntervalRendersMinutesAndSavesSeconds", async () => {
  mount();
  // The API stores seconds; the input edits minutes.
  const seeded = (await screen.findByLabelText(
    "Update interval",
  )) as HTMLInputElement;
  expect(seeded.value).toBe("30");

  // Re-seed with a different interval so that typing "30" is a change.
  settingsBody = { rss_enabled: true, rss_interval_s: 3600 };
  cleanup();
  useSettingsDirty.setState({ report: null });
  freshQueryClient();
  mount();
  const input = (await screen.findByLabelText(
    "Update interval",
  )) as HTMLInputElement;
  expect(input.value).toBe("60");

  fireEvent.change(input, { target: { value: "30" } });
  fireEvent.click(await screen.findByRole("button", { name: "Save" }));
  // Only the changed key is sent, in seconds.
  await waitFor(() =>
    expect(settingsPatches).toEqual([{ rss_interval_s: 1800 }]),
  );
  expect(feedPatches).toEqual([]);
  await waitFor(() =>
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull(),
  );
});

test("TestMixedItemCapFansOutToEveryFeed", async () => {
  feedRows = [feed("fed_a", 30), feed("fed_b", 50)];
  mount();
  const cap = (await screen.findByLabelText(
    "Maximum articles kept per feed",
  )) as HTMLInputElement;
  expect(cap.value).toBe("");
  expect(cap.getAttribute("placeholder")).toBe("Mixed");

  fireEvent.change(cap, { target: { value: "40" } });
  fireEvent.click(await screen.findByRole("button", { name: "Save" }));
  await waitFor(() => expect(feedPatches).toHaveLength(2));
  // One PATCH /feeds/{id} per feed whose cap differed, and nothing else.
  expect(feedPatches).toEqual([
    { id: "fed_a", body: { item_cap: 40 } },
    { id: "fed_b", body: { item_cap: 40 } },
  ]);
  expect(settingsPatches).toEqual([]);
});

test("TestSettingsValidationErrorSkipsFeedWrites", async () => {
  feedRows = [feed("fed_a", 30), feed("fed_b", 50)];
  server.use(
    http.patch("*/api/v1/settings", () =>
      HttpResponse.json(
        {
          type: "/problems/validation-failed",
          title: "Validation failed",
          status: 422,
          detail: "rss_interval_s must be at least 300",
          errors: [
            {
              location: "body.rss_interval_s",
              message: "must be at least 300",
              value: 180,
            },
          ],
        },
        { status: 422 },
      ),
    ),
  );
  mount();
  fireEvent.change(await screen.findByLabelText("Update interval"), {
    target: { value: "3" },
  });
  fireEvent.change(
    await screen.findByLabelText("Maximum articles kept per feed"),
    { target: { value: "40" } },
  );
  fireEvent.click(await screen.findByRole("button", { name: "Save" }));

  // The 422 lands on the interval input; the feed fan-out is never sent.
  await screen.findByText("must be at least 300");
  const input = screen.getByLabelText("Update interval") as HTMLInputElement;
  expect(input.getAttribute("aria-invalid")).toBe("true");
  expect(input.value).toBe("3");
  expect(feedPatches).toEqual([]);
  // The form stays dirty: the bar and its count are still up.
  expect(screen.getByRole("button", { name: "Save" })).toBeTruthy();
});

test("TestSmartFilterPatternsAreReadOnly", async () => {
  ruleRows = [rule("rul_a", true), rule("rul_b", true), rule("rul_c", false)];
  mount();
  const group = await screen.findByRole("group", {
    name: "Smart episode filter",
  });
  const pre = group.querySelector("pre")!;
  for (const pattern of [
    String.raw`s(\d+)e(\d+)`,
    String.raw`(\d+)x(\d+)`,
    String.raw`(\d{4}[.\-]\d{1,2}[.\-]\d{1,2})`,
    String.raw`(\d{1,2}[.\-]\d{1,2}[.\-]\d{4})`,
  ])
    expect(pre.textContent).toContain(pattern);
  // No editable control exists in either read-only row.
  for (const role of ["checkbox", "spinbutton", "textbox", "combobox"])
    expect(within(group).queryByRole(role)).toBeNull();
  const status = screen.getByRole("group", { name: "Auto-downloader" });
  expect(within(status).getByText(/2 of 3 rules enabled/)).toBeTruthy();
  for (const role of ["checkbox", "spinbutton", "textbox", "combobox"])
    expect(within(status).queryByRole(role)).toBeNull();

  // Exactly the five controls of doc 09 §9's RSS row, in that order, and only
  // the first three are editable anywhere on the section.
  const labels = [
    "Enable RSS fetching",
    "Update interval",
    "Maximum articles kept per feed",
    "Auto-downloader",
    "Smart episode filter",
  ];
  const text = document.body.textContent ?? "";
  let at = -1;
  for (const label of labels) {
    const found = text.indexOf(label);
    expect(found).toBeGreaterThan(at);
    at = found;
  }
  expect(screen.getAllByRole("checkbox")).toHaveLength(1);
  expect(screen.getAllByRole("spinbutton")).toHaveLength(2);
});

test("TestRevertRestoresSeededStateWithoutRequests", async () => {
  mount();
  // With no feeds the cap renders the server default, disabled.
  const cap = (await screen.findByLabelText(
    "Maximum articles kept per feed",
  )) as HTMLInputElement;
  expect(cap.disabled).toBe(true);
  expect(cap.value).toBe("50");

  const enabled = screen.getByLabelText("Enable RSS fetching");
  expect(enabled.getAttribute("aria-checked")).toBe("true");
  fireEvent.click(enabled);
  fireEvent.change(screen.getByLabelText("Update interval"), {
    target: { value: "45" },
  });
  await waitFor(() =>
    expect(document.querySelector('[aria-live="polite"]')?.textContent).toBe(
      "2 unsaved changes",
    ),
  );

  fireEvent.click(screen.getByRole("button", { name: "Revert" }));
  await waitFor(() =>
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull(),
  );
  expect(
    screen.getByLabelText("Enable RSS fetching").getAttribute("aria-checked"),
  ).toBe("true");
  expect(
    (screen.getByLabelText("Update interval") as HTMLInputElement).value,
  ).toBe("30");
  expect(settingsPatches).toEqual([]);
  expect(feedPatches).toEqual([]);
});
