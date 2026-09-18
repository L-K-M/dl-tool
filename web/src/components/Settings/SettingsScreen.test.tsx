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
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
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
  defaultPrefs,
  useUiPrefs,
  type UiPrefsState,
} from "../../store/useUiPrefs";
import {
  ARRIVAL,
  IMPLEMENTED,
  SECTIONS,
  SettingsScreen,
  useSettingsDirty,
} from "./SettingsScreen";

const engines = [
  {
    id: "eng_aria2",
    kind: "aria2",
    name: "aria2",
    enabled: true,
    url: "http://aria2:6800/jsonrpc",
    connected: true,
    version: "1.37.0",
    capabilities: ["http", "ftp", "metalink", "per_task_limits"],
    last_seen_at: "2026-09-01T09:41:50Z",
    last_error: null,
  },
  {
    id: "eng_qbittorrent",
    kind: "qbittorrent",
    name: "qBittorrent",
    enabled: true,
    url: "http://qbittorrent:8080",
    connected: false,
    version: null,
    capabilities: ["bittorrent", "per_file_select", "trackers"],
    last_seen_at: "2026-09-01T09:12:03Z",
    last_error: "engine conformance check failed at boot",
  },
];

const server = setupServer();
let qc: QueryClient;
let settingsApiCalls: string[];
let putBodies: Record<string, unknown>[];

function PathProbe() {
  const location = useLocation();
  return <output data-testid="path-probe">{location.pathname}</output>;
}

function mount(path: string) {
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route
            path="/settings/:section"
            element={
              <>
                <SettingsScreen />
                <PathProbe />
              </>
            }
          />
          <Route path="*" element={<PathProbe />} />
        </Routes>
      </MemoryRouter>
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
  settingsApiCalls = [];
  putBodies = [];
  useUiPrefs.setState({
    ...structuredClone(defaultPrefs),
    startupFilter: undefined,
    rememberLastDestination: undefined,
    confirmOnDelete: undefined,
    doubleClickDownloading: undefined,
    doubleClickCompleted: undefined,
  } as Partial<UiPrefsState>);
  useSettingsDirty.setState({ report: null });
  server.use(
    http.get("*/api/v1/engines", () => HttpResponse.json({ engines })),
    http.put("*/api/v1/prefs", async ({ request }) => {
      putBodies.push((await request.json()) as Record<string, unknown>);
      return HttpResponse.json({});
    }),
    http.all("*/api/v1/settings", ({ request }) => {
      settingsApiCalls.push(request.url);
      return HttpResponse.json({});
    }),
    http.all("*/api/v1/settings/*", ({ request }) => {
      settingsApiCalls.push(request.url);
      return HttpResponse.json({});
    }),
  );
});
afterEach(() => {
  cleanup();
  server.resetHandlers();
  vi.restoreAllMocks();
});
afterAll(() => server.close());

test("TestUnknownSectionRedirects", async () => {
  mount("/settings/bogus");
  await waitFor(() =>
    expect(screen.getByTestId("path-probe").textContent).toBe(
      "/settings/general",
    ),
  );
  await screen.findByLabelText("Density");
});

test("TestUsersAliasRendersAccountNote", async () => {
  // Doc 09 §2.1 names the account route `users`; it stays reachable and keeps
  // its path rather than redirecting away.
  mount("/settings/users");
  await screen.findByText("This section arrives with M6.");
  expect(screen.getByTestId("path-probe").textContent).toBe("/settings/users");
});

test("TestGeneralWritesPrefs", async () => {
  mount("/settings/general");
  const labels = [
    "Theme",
    "Density",
    "Default sidebar filter on startup",
    "Remember last destination",
    "Confirm on delete",
    "Downloading tasks",
    "Completed tasks",
  ];
  for (const label of labels)
    expect(await screen.findByLabelText(label)).toBeTruthy();

  fireEvent.change(screen.getByLabelText("Density"), {
    target: { value: "compact" },
  });
  expect(useUiPrefs.getState().grid.density).toBe("compact");
  await waitFor(() =>
    expect(
      (putBodies.at(-1)?.grid as Record<string, unknown> | undefined)?.density,
    ).toBe("compact"),
  );

  fireEvent.change(screen.getByLabelText("Default sidebar filter on startup"), {
    target: { value: "completed" },
  });
  const extra = useUiPrefs.getState() as { startupFilter?: string };
  expect(extra.startupFilter).toBe("completed");
  // Unknown members land in the stored document verbatim (doc 05 §11.4).
  await waitFor(() =>
    expect(putBodies.at(-1)?.startupFilter).toBe("completed"),
  );
  // The unknown member must also survive the store's debounced writer, which
  // serializes every non-function state member. Patching a declared member
  // arms another write; once its result is visible in the PUT body, a flush
  // has provably run — no fixed sleep.
  useUiPrefs.getState().patch({ sidebarCollapsed: true });
  await waitFor(() => expect(putBodies.at(-1)?.sidebarCollapsed).toBe(true));
  expect(putBodies.at(-1)?.startupFilter).toBe("completed");
});

test("TestDirtyBarAppearsAndAnnounces", async () => {
  mount("/settings/general");
  expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  fireEvent.change(await screen.findByLabelText("Density"), {
    target: { value: "compact" },
  });
  await waitFor(() =>
    expect(document.querySelector('[aria-live="polite"]')?.textContent).toBe(
      "1 unsaved change",
    ),
  );
  expect(screen.getByRole("button", { name: "Save" })).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() =>
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull(),
  );
  expect(useUiPrefs.getState().grid.density).toBe("compact");
});

test("TestRevertRestoresBaseline", async () => {
  mount("/settings/general");
  fireEvent.change(await screen.findByLabelText("Density"), {
    target: { value: "compact" },
  });
  fireEvent.click(await screen.findByRole("button", { name: "Revert" }));
  await waitFor(() =>
    expect(useUiPrefs.getState().grid.density).toBe("comfortable"),
  );
  await waitFor(() =>
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull(),
  );
});

test("TestConnectionRendersEngines", async () => {
  mount("/settings/connection");
  const aria2 = await screen.findByText("aria2", {
    selector: "span.font-medium",
  });
  const row = aria2.closest("tr")!;
  expect(
    within(row).getByDisplayValue("http://aria2:6800/jsonrpc"),
  ).toBeTruthy();
  expect(within(row).getByText("Connected")).toBeTruthy();
  expect(within(row).getByText("1.37.0")).toBeTruthy();
  expect(within(row).getByText(/per_task_limits/)).toBeTruthy();
  await screen.findByText("qBittorrent");
  expect(screen.getByText("Not connected")).toBeTruthy();
  expect(
    screen.getByText(
      "dl-tool assumes exclusive control of its engines and ignores transfers it did not create.",
    ),
  ).toBeTruthy();
});

test("TestEngineTestFailureRendersError", async () => {
  const toastError = vi.spyOn(toast, "error");
  server.use(
    http.post("*/api/v1/engines/eng_qbittorrent/test", () =>
      HttpResponse.json({
        ok: false,
        version: null,
        elapsed_ms: 4,
        error: "dial tcp: connection refused",
      }),
    ),
  );
  mount("/settings/connection");
  const name = await screen.findByText("qBittorrent");
  const row = name.closest("tr")!;
  fireEvent.click(within(row).getByRole("button", { name: "Test" }));
  await within(row).findByText("dial tcp: connection refused");
  expect(within(row).getByText("failed")).toBeTruthy();
  expect(toastError).not.toHaveBeenCalled();
});

test("TestEngineTestRequestErrorToastsFriendlyDetail", async () => {
  const toastError = vi.spyOn(toast, "error");
  server.use(
    http.post("*/api/v1/engines/eng_qbittorrent/test", () =>
      // detail present but empty: only ||, not ??, falls through.
      HttpResponse.json({ detail: "" }, { status: 502 }),
    ),
  );
  mount("/settings/connection");
  const name = await screen.findByText("qBittorrent");
  const row = name.closest("tr")!;
  fireEvent.click(within(row).getByRole("button", { name: "Test" }));
  await waitFor(() => expect(toastError).toHaveBeenCalled());
  const message = String(toastError.mock.calls[0]![0]);
  expect(message).toContain("Network error");
  expect(message).not.toContain("shell.networkError");
});

test("TestEngineRowsSurviveFailedPostTestRefetch", async () => {
  server.use(
    http.post("*/api/v1/engines/eng_aria2/test", () =>
      HttpResponse.json({
        ok: true,
        version: "1.37.0",
        elapsed_ms: 3,
        error: null,
      }),
    ),
  );
  mount("/settings/connection");
  const aria2 = await screen.findByText("aria2", {
    selector: "span.font-medium",
  });
  const row = aria2.closest("tr")!;
  // The probe's follow-up refetch fails; the cached rows and the fresh
  // result must stay rendered behind an inline retry notice.
  server.use(
    http.get("*/api/v1/engines", () => HttpResponse.json({}, { status: 500 })),
  );
  fireEvent.click(within(row).getByRole("button", { name: "Test" }));
  await within(row).findByText("passed");
  await screen.findByRole("alert");
  // Re-query after the failure: the captured row could be a detached node.
  const liveRow = screen
    .getByText("aria2", { selector: "span.font-medium" })
    .closest("tr")!;
  expect(within(liveRow).getByText("passed")).toBeTruthy();
});

test("TestConformanceWarningRendersFromLastError", async () => {
  mount("/settings/connection");
  const name = await screen.findByText("qBittorrent");
  const row = name.closest("tr")!;
  await within(row).findByText("engine conformance check failed at boot");
});

test("TestUnimplementedSectionsRenderNoteNotForm", async () => {
  for (const section of SECTIONS.filter((s) => !IMPLEMENTED.includes(s))) {
    cleanup();
    useSettingsDirty.setState({ report: null });
    mount(`/settings/${section}`);
    await screen.findByText(
      `This section arrives with ${ARRIVAL[section as keyof typeof ARRIVAL]}.`,
    );
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Test" })).toBeNull();
  }
  expect(SECTIONS).toContain("account");
});

test("TestScreenMakesNoSettingsApiCalls", async () => {
  mount("/settings/general");
  await screen.findByLabelText("Density");
  fireEvent.change(screen.getByLabelText("Density"), {
    target: { value: "compact" },
  });
  fireEvent.click(await screen.findByRole("button", { name: "Save" }));
  cleanup();
  useSettingsDirty.setState({ report: null });
  mount("/settings/connection");
  await screen.findByText("aria2", { selector: "span.font-medium" });
  // Arm deferred work after the second section mounts, then wait for the
  // store's debounced flush — the same no-sleep pattern as
  // TestGeneralWritesPrefs — so a late settings call is still recorded.
  useUiPrefs.getState().patch({ sidebarCollapsed: true });
  await waitFor(() => expect(putBodies.at(-1)?.sidebarCollapsed).toBe(true));
  expect(settingsApiCalls).toEqual([]);
});
