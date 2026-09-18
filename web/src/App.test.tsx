import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { StrictMode } from "react";
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
import App, { safeNext } from "./App";
import * as client from "./api/client";

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
const status = {
  unauthorized: 401,
  created: 201,
  conflict: 409,
  throttled: 429,
  unavailable: 503,
};
const taskPage = {
  items: [
    {
      id: "route-task",
      name: "Route download",
      state: "queued",
      source_kind: "http",
      total_bytes: 1024,
      completed_bytes: 0,
      progress: 0,
      download_rate: 0,
      upload_rate: 0,
      eta_seconds: null,
      ratio: 0,
      total_peers: 0,
      uploaded_bytes: 0,
      queue_position: null,
      destination: "/downloads",
      added_at: "2026-09-01T00:00:00Z",
      completed_at: null,
    },
  ],
  total: 1,
  next_cursor: null,
};
const server = setupServer();
let taskRequests: URL[] = [];
let bootCalls = 0;

function boot(problem?: string) {
  server.use(
    http.get("*/api/v1/auth/me", () => {
      bootCalls++;
      if (problem)
        return HttpResponse.json(
          { type: `/problems/${problem}` },
          { status: status.unauthorized },
        );
      return HttpResponse.json(session);
    }),
  );
}
function mount(path = "/", base = "/") {
  document.querySelector("base")!.setAttribute("href", base);
  window.history.replaceState(null, "", path);
  return render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}
function fill(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label, { exact: true }), {
    target: { value },
  });
}
function login() {
  fill("Username", "operator");
  fill("Password", "correct horse battery");
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
}
function setup(password = "correct horse battery", confirmation = password) {
  fill("Setup token", "one-time-token");
  fill("Username", "operator");
  fill("Password", password);
  fill("Confirm password", confirmation);
}
function noPersistedToken() {
  expect(client.csrfToken()).toBe(session.csrf_token);
  expect(JSON.stringify(localStorage)).not.toContain(session.csrf_token);
  expect(JSON.stringify(sessionStorage)).not.toContain(session.csrf_token);
  expect(document.cookie).not.toContain(session.csrf_token);
  expect(window.location.href).not.toContain(session.csrf_token);
}

beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
beforeEach(() => {
  bootCalls = 0;
  taskRequests = [];
  server.use(
    http.get("*/api/v1/tasks", ({ request }) => {
      taskRequests.push(new URL(request.url));
      return HttpResponse.json(taskPage);
    }),
    // An authenticated session hydrates the preference document.
    http.get("*/api/v1/prefs", () => HttpResponse.json({})),
    http.put("*/api/v1/prefs", () => HttpResponse.json({})),
  );
  vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockReturnValue(320);
  vi.spyOn(HTMLElement.prototype, "offsetWidth", "get").mockReturnValue(1024);
  localStorage.clear();
  sessionStorage.clear();
  client.setCsrfToken(null);
  const base = document.createElement("base");
  base.href = "/";
  document.head.appendChild(base);
});
afterEach(() => {
  cleanup();
  server.resetHandlers();
  vi.restoreAllMocks();
  document.querySelector("base")?.remove();
  window.history.replaceState(null, "", "/");
  client.setCsrfToken(null);
});
afterAll(() => server.close());

test("TestBootRoutesToSetupWizard", async () => {
  boot("setup-required");
  mount();
  await screen.findByRole("heading", { name: "Set up dl-tool" });
  expect(window.location.pathname).toBe("/setup");
  expect(bootCalls).toBe(1);
});
test("TestBootRoutesToLogin", async () => {
  boot("unauthenticated");
  mount("/search?query=linux");
  await screen.findByRole("heading", { name: "Sign in" });
  expect(window.location.pathname).toBe("/login");
  expect(new URLSearchParams(window.location.search).get("next")).toBe(
    "/search?query=linux",
  );
  expect(bootCalls).toBe(1);
});
test("TestBootRendersLayout", async () => {
  boot();
  mount();
  await screen.findByRole("main");
  const banner = screen.getByRole("banner");
  expect(within(banner).getByRole("button", { name: "Add" })).toBeTruthy();
  expect(
    within(banner).getByRole("textbox", { name: "Filter tasks by name" }),
  ).toBeTruthy();
  const sidebar = screen.getByRole("complementary");
  expect(
    within(sidebar).getByRole("link", { name: /Downloading/ }),
  ).toBeTruthy();
  expect(within(sidebar).getByRole("link", { name: /Settings/ })).toBeTruthy();
  const statusBar = screen.getByRole("contentinfo");
  expect(within(statusBar).getByRole("status")).toBeTruthy();
  expect(screen.getByTestId("app-root").className).toContain(
    "grid-cols-[220px_minmax(0,1fr)]",
  );
  expect(screen.getByTestId("app-root").className).toContain(
    "grid-rows-[48px_minmax(0,1fr)_28px]",
  );
  expect(bootCalls).toBe(1);
  noPersistedToken();
});
test("TestLoginStoresCsrfToken", async () => {
  boot("unauthenticated");
  const capture = vi.spyOn(client, "setCsrfToken");
  server.use(
    http.post("*/api/v1/auth/login", async ({ request }) => {
      expect(await request.json()).toEqual({
        username: "operator",
        password: "correct horse battery",
      });
      return HttpResponse.json(session);
    }),
  );
  mount("/login?next=%2Fsearch%3Fquery%3Dlinux%23results");
  await screen.findByRole("heading", { name: "Sign in" });
  login();
  await screen.findByRole("heading", { name: "Search", level: 1 });
  expect(
    window.location.pathname + window.location.search + window.location.hash,
  ).toBe("/search?query=linux#results");
  expect(capture).toHaveBeenCalledWith(session.csrf_token);
  expect(bootCalls).toBe(1);
  noPersistedToken();
});
test.each([
  "//evil.example",
  "https://x",
  "\\\\x",
  "/\\evil.example",
  "/search\n",
  "",
  null,
])("TestSafeNextRejectsAbsoluteAndProtocolRelative %s", (raw) => {
  expect(safeNext(raw)).toBe("/");
});
test.each([
  "/search?query=linux#results",
  "//evil.example",
  "https://x",
  "/\\\\evil.example",
])("TestLoginRedirectUnderBase %s", async (next) => {
  boot("unauthenticated");
  server.use(
    http.post("*/api/v1/auth/login", () => HttpResponse.json(session)),
  );
  mount(`/dl-tool/login?${new URLSearchParams({ next })}`, "/dl-tool/");
  await screen.findByRole("heading", { name: "Sign in" });
  login();
  await screen.findByRole("main");
  expect(
    window.location.pathname + window.location.search + window.location.hash,
  ).toBe(next.startsWith("/search") ? `/dl-tool${next}` : "/dl-tool");
  noPersistedToken();
});

test("TestSafeNextPreservesLocalRoute", () => {
  expect(safeNext("/search?q=linux#results")).toBe("/search?q=linux#results");
});

const routes = [
  "/",
  ...[
    "all",
    "downloading",
    "completed",
    "active",
    "inactive",
    "stopped",
    "error",
  ].map((filter) => `/tasks/${filter}`),
  "/tasks/category/Linux",
  "/tasks/tag/archive",
  "/search",
  "/rss/feeds",
  "/rss/rules",
  ...[
    "general",
    "connection",
    "bandwidth",
    "bittorrent",
    "downloads",
    "rss",
    "indexers",
    "users",
    "notifications",
    "advanced",
  ].map((section) => `/settings/${section}`),
  "/logs",
  "/unknown",
];
test.each(routes)("TestAuthenticatedRouteUnderBase %s", async (path) => {
  boot();
  mount(`/dl-tool${path}`, "/dl-tool/");
  await screen.findByRole("main");
  expect(screen.getByRole("banner")).toBeTruthy();
  expect(screen.getByRole("main").textContent).not.toBe("");
  expect(window.location.pathname).toBe(`/dl-tool${path}`);
  expect(bootCalls).toBe(1);
});
test.each([
  ["/", "all", null, null],
  ["/tasks/downloading", "downloading", null, null],
  ["/tasks/category/Linux", "all", "Linux", null],
  ["/tasks/tag/archive", "all", null, "archive"],
])(
  "TestTaskRoutesRequestServerFilters %s",
  async (path, state, category, tag) => {
    boot();
    mount(`/dl-tool${path}`, "/dl-tool/");
    await screen.findByText("Route download");
    expect(taskRequests.length).toBeGreaterThan(0);
    for (const url of taskRequests) {
      expect(url.pathname).toBe("/dl-tool/api/v1/tasks");
      expect(url.searchParams.get("state")).toBe(state);
      expect(url.searchParams.get("category")).toBe(category);
      expect(url.searchParams.get("tag")).toBe(tag);
      expect(url.searchParams.get("limit")).toBe("500");
      expect(url.searchParams.get("cursor")).toBeNull();
    }
    expect(screen.getByRole("main").className).toContain("overflow-hidden");
    expect(bootCalls).toBe(1);
    noPersistedToken();
  },
);

test.each(["/login", "/setup"])(
  "TestAuthenticatedPublicRoute %s",
  async (path) => {
    boot();
    mount(`/dl-tool${path}`, "/dl-tool/");
    await screen.findByRole("main");
    expect(window.location.pathname).toBe("/dl-tool");
  },
);
test("TestSetupUnavailableAfterConfiguration", async () => {
  boot("unauthenticated");
  mount("/setup");
  await screen.findByRole("heading", { name: "Sign in" });
  expect(window.location.pathname).toBe("/login");
});
test("TestSetupValidationAndCsrf", async () => {
  boot("setup-required");
  let submissions = 0;
  server.use(
    http.post("*/api/v1/auth/setup", async ({ request }) => {
      submissions++;
      expect(await request.json()).toEqual({
        setup_token: "one-time-token",
        username: "operator",
        password: "correct horse battery",
        locale: "en",
      });
      return HttpResponse.json(session, { status: status.created });
    }),
  );
  mount("/dl-tool/setup", "/dl-tool/");
  await screen.findByRole("heading", { name: "Set up dl-tool" });
  expect(screen.getByText("Use at least 12 characters.")).toBeTruthy();
  setup("short");
  const submit = screen.getByRole("button", {
    name: "Create account",
  }) as HTMLButtonElement;
  expect(submit.disabled).toBe(true);
  setup("correct horse battery", "different password");
  expect(submit.disabled).toBe(true);
  expect(submissions).toBe(0);
  setup();
  expect(submit.disabled).toBe(false);
  fireEvent.click(submit);
  await screen.findByRole("main");
  expect(window.location.pathname).toBe("/dl-tool");
  expect(submissions).toBe(1);
  noPersistedToken();
});
test("TestSetupAlreadyCompleteRedirectsWithToast", async () => {
  boot("setup-required");
  server.use(
    http.post("*/api/v1/auth/setup", () =>
      HttpResponse.json(
        { type: "/problems/setup-already-complete" },
        { status: status.conflict },
      ),
    ),
  );
  mount("/setup");
  await screen.findByRole("heading", { name: "Set up dl-tool" });
  setup();
  fireEvent.click(screen.getByRole("button", { name: "Create account" }));
  await screen.findByRole("heading", { name: "Sign in" });
  expect(
    await screen.findByText("Setup is already complete. Sign in."),
  ).toBeTruthy();
  expect(window.location.pathname).toBe("/login");
});
test.each([status.unauthorized, status.throttled])(
  "TestLoginError %s",
  async (code) => {
    boot("unauthenticated");
    server.use(
      http.post("*/api/v1/auth/login", () =>
        HttpResponse.json(
          { detail: "Invalid username or password." },
          { status: code, headers: { "Retry-After": "37" } },
        ),
      ),
    );
    mount("/login");
    await screen.findByRole("heading", { name: "Sign in" });
    login();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(
      code === status.throttled
        ? "Try again in 37 seconds."
        : "Invalid username or password.",
    );
    expect(client.csrfToken()).toBeNull();
  },
);
test("TestBootFailureOffersRetryWithoutGuessingAuth", async () => {
  server.use(
    http.get("*/api/v1/auth/me", () =>
      HttpResponse.json({}, { status: status.unavailable }),
    ),
  );
  mount("/search");
  await screen.findByRole("alert");
  expect(window.location.pathname).toBe("/search");
  expect(screen.queryByRole("main")).toBeNull();
  boot();
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await screen.findByRole("main");
  await waitFor(() => expect(bootCalls).toBe(1));
});

test("TestSignOutFailureKeepsSession", async () => {
  boot();
  server.use(
    http.post("*/api/v1/auth/logout", () =>
      HttpResponse.json(
        {
          type: "/problems/logout-failed",
          title: "Logout failed",
          detail: "session store unavailable",
        },
        { status: status.unavailable },
      ),
    ),
  );
  mount();
  await screen.findByRole("main");
  fireEvent.keyDown(screen.getByRole("button", { name: "User menu" }), {
    key: "Enter",
  });
  fireEvent.click(await screen.findByRole("menuitem", { name: "Sign out" }));
  await screen.findByText("Sign out failed: session store unavailable");
  // A failed logout must not flip local auth: the shell stays mounted and
  // the CSRF token is still held for the live session.
  expect(screen.getByRole("main")).toBeTruthy();
  expect(window.location.pathname).toBe("/");
  expect(client.csrfToken()).toBe(session.csrf_token);
});
test("TestSignOutSuccessClearsSession", async () => {
  boot();
  server.use(
    http.post(
      "*/api/v1/auth/logout",
      () => new HttpResponse(null, { status: 204 }),
    ),
  );
  mount();
  await screen.findByRole("main");
  fireEvent.keyDown(screen.getByRole("button", { name: "User menu" }), {
    key: "Enter",
  });
  fireEvent.click(await screen.findByRole("menuitem", { name: "Sign out" }));
  await screen.findByRole("heading", { name: "Sign in" });
  expect(window.location.pathname).toBe("/login");
  expect(client.csrfToken()).toBeNull();
});
