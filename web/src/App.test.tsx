import { StrictMode } from "react";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  expect,
  test,
  vi,
} from "vitest";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";

// Match production: the injected base precedes the API client's module evaluation.
const { base } = vi.hoisted(() => {
  const base = "/dl-tool";
  const element = document.createElement("base");
  element.href = `${base}/`;
  document.head.appendChild(element);
  return { base };
});
import App, { AUTH_STATUS, safeNext } from "./App";
import * as client from "./api/client";

const HTTP_OK = 200;
const HTTP_CREATED = 201;
const HTTP_UNAVAILABLE = 503;
const envelope = {
  csrf_token: "session-csrf-secret",
  user: {
    id: "usr_test",
    username: "operator",
    enabled: true,
    locale: "en",
    last_login_at: null,
    created_at: "2026-09-01T00:00:00Z",
  },
};
const server = setupServer();
let bootRequests = 0;

function me(status: number, type = "/problems/unauthenticated") {
  server.use(
    http.get(client.apiUrl("auth/me"), () => {
      bootRequests++;
      return HttpResponse.json(status === HTTP_OK ? envelope : { type }, {
        status,
      });
    }),
  );
}

function open(path = "/") {
  window.history.replaceState(null, "", `${base}${path}`);
  return render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}

function fill(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

function fillSetup(password = "twelve-characters") {
  fill("Setup token", "one-time-token");
  fill("Username", "operator");
  fill("Password", password);
  fill("Confirm password", password);
}

function assertPrivateToken() {
  expect(client.csrfToken()).toBe(envelope.csrf_token);
  expect(JSON.stringify(localStorage)).not.toContain(envelope.csrf_token);
  expect(JSON.stringify(sessionStorage)).not.toContain(envelope.csrf_token);
  expect(document.cookie).not.toContain(envelope.csrf_token);
  expect(window.location.href).not.toContain(envelope.csrf_token);
}

beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
beforeEach(() => {
  bootRequests = 0;
  client.setCsrfToken(null);
  localStorage.clear();
  sessionStorage.clear();
  me(AUTH_STATUS.unauthorized);
});
afterEach(() => {
  cleanup();
  server.resetHandlers();
  vi.restoreAllMocks();
});
afterAll(() => {
  server.close();
  document.querySelector("base")?.remove();
});

test("TestBootRoutesToSetupWizard", async () => {
  me(AUTH_STATUS.unauthorized, "/problems/setup-required");
  open();
  expect(
    await screen.findByRole("heading", { name: "Set up dl-tool" }),
  ).toBeTruthy();
  expect(window.location.pathname).toBe(`${base}/setup`);
  expect(screen.getByLabelText("Setup token")).toBeTruthy();
  expect(bootRequests).toBe(1);
});

test("TestBootRoutesToLogin", async () => {
  client.setCsrfToken("stale-token");
  open();
  expect(await screen.findByRole("heading", { name: "Sign in" })).toBeTruthy();
  expect(window.location.pathname).toBe(`${base}/login`);
  expect(client.csrfToken()).toBeNull();
  expect(bootRequests).toBe(1);
});

test("TestBootRendersLayout", async () => {
  me(HTTP_OK);
  open();
  expect(
    await screen.findByRole("heading", { name: "Downloads" }),
  ).toBeTruthy();
  for (const role of ["banner", "complementary", "contentinfo"]) {
    expect(screen.getByRole(role).childNodes.length).toBe(0);
  }
  expect(screen.getByRole("banner").parentElement?.className).toContain(
    "grid-rows-[48px_minmax(0,1fr)_28px]",
  );
  expect(screen.getByRole("complementary").parentElement?.className).toContain(
    "grid-cols-[220px_minmax(0,1fr)]",
  );
  assertPrivateToken();
  expect(bootRequests).toBe(1);
});

test("TestLoginStoresCsrfToken", async () => {
  const tokenSpy = vi.spyOn(client, "setCsrfToken");
  let body: unknown;
  server.use(
    http.post(client.apiUrl("auth/login"), async ({ request }) => {
      body = await request.json();
      return HttpResponse.json(envelope);
    }),
  );
  open("/search?query=linux#results");
  await screen.findByRole("heading", { name: "Sign in" });
  fill("Username", "operator");
  fill("Password", "correct-password");
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  await screen.findByRole("heading", { name: "Search" });
  expect(body).toEqual({ username: "operator", password: "correct-password" });
  expect(tokenSpy).toHaveBeenCalledWith(envelope.csrf_token);
  assertPrivateToken();
  expect(
    window.location.pathname + window.location.search + window.location.hash,
  ).toBe(`${base}/search?query=linux#results`);
  expect(bootRequests).toBe(1);

  server.use(
    http.post(client.apiUrl("auth/logout"), ({ request }) => {
      expect(request.headers.get("X-DLTOOL-CSRF")).toBe(envelope.csrf_token);
      return new HttpResponse(null, { status: 204 });
    }),
  );
  await client.api.POST("/auth/logout");
});

test.each([
  "//evil.example",
  "https://x",
  "\\\\x",
  "/\\evil.example",
  "/\n/evil.example",
  null,
])("TestSafeNextRejectsAbsoluteAndProtocolRelative: %s", (raw) => {
  expect(safeNext(raw)).toBe("/");
});

test("TestSafeNextPreservesInternalPath", () => {
  expect(safeNext("/tasks/active?sort=name#row")).toBe(
    "/tasks/active?sort=name#row",
  );
});

test.each(["//evil.example", "https://x", "\\\\x"])(
  "TestLoginRejectsExternalNext: %s",
  async (next) => {
    server.use(
      http.post(client.apiUrl("auth/login"), () => HttpResponse.json(envelope)),
    );
    open(`/login?next=${encodeURIComponent(next)}`);
    await screen.findByRole("heading", { name: "Sign in" });
    fill("Username", "operator");
    fill("Password", "correct-password");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await screen.findByRole("heading", { name: "Downloads" });
    expect(window.location.pathname).toBe(`${base}/`);
  },
);

const routes = [
  ["/", "Downloads"],
  ...[
    "all",
    "downloading",
    "completed",
    "active",
    "inactive",
    "stopped",
    "error",
  ].map((filter) => [`/tasks/${filter}`, "Downloads"]),
  ["/tasks/category/linux", "Downloads"],
  ["/tasks/tag/linux", "Downloads"],
  ["/search", "Search"],
  ["/rss/feeds", "RSS feeds"],
  ["/rss/rules", "RSS rules"],
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
  ].map((section) => [`/settings/${section}`, "Settings"]),
  ["/logs", "Logs"],
  ["/unknown/deep/path", "Page not found"],
];
test.each(routes)("TestRoutesUnderInjectedBase: %s", async (path, heading) => {
  me(HTTP_OK);
  open(path);
  await screen.findByRole("heading", { name: heading });
  expect(screen.getByRole("banner")).toBeTruthy();
  expect(window.location.pathname).toBe(`${base}${path}`);
  expect(bootRequests).toBe(1);
});

test.each(["/setup", "/login"])(
  "TestAuthenticatedAuthRouteRedirect: %s",
  async (path) => {
    me(HTTP_OK);
    open(path);
    await screen.findByRole("heading", { name: "Downloads" });
    expect(window.location.pathname).toBe(`${base}/`);
  },
);

test("TestSetupCannotOpenAfterSetup", async () => {
  open("/setup");
  await screen.findByRole("heading", { name: "Sign in" });
  expect(window.location.pathname).toBe(`${base}/login`);
});

test("TestLoginRedirectsWhenSetupRequired", async () => {
  me(AUTH_STATUS.unauthorized, "/problems/setup-required");
  open("/login");
  await screen.findByRole("heading", { name: "Set up dl-tool" });
  expect(window.location.pathname).toBe(`${base}/setup`);
});

test("TestSetupValidatesPasswordAndStoresCsrfToken", async () => {
  me(AUTH_STATUS.unauthorized, "/problems/setup-required");
  let body: unknown;
  server.use(
    http.post(client.apiUrl("auth/setup"), async ({ request }) => {
      body = await request.json();
      return HttpResponse.json(envelope, { status: HTTP_CREATED });
    }),
  );
  open();
  await screen.findByRole("heading", { name: "Set up dl-tool" });
  const submit = screen.getByRole("button", {
    name: "Create account",
  }) as HTMLButtonElement;
  expect(submit.disabled).toBe(true);
  expect(screen.getByText("Use at least 12 characters.")).toBeTruthy();
  fillSetup("12345678901");
  expect(submit.disabled).toBe(true);
  fillSetup("😀😀😀😀😀😀");
  expect(submit.disabled).toBe(true);
  fillSetup("123456789012");
  fill("Confirm password", "does-not-match");
  expect(submit.disabled).toBe(true);
  expect(screen.getByText("Passwords must match.")).toBeTruthy();
  fill("Confirm password", "123456789012");
  expect(submit.disabled).toBe(false);
  fireEvent.click(submit);
  await screen.findByRole("heading", { name: "Downloads" });
  expect(body).toEqual({
    setup_token: "one-time-token",
    username: "operator",
    password: "123456789012",
    locale: "en",
  });
  assertPrivateToken();
  expect(window.location.pathname).toBe(`${base}/`);
});

test("TestSetupRaceRedirectsWithToast", async () => {
  me(AUTH_STATUS.unauthorized, "/problems/setup-required");
  server.use(
    http.post(client.apiUrl("auth/setup"), () =>
      HttpResponse.json(
        { type: "/problems/setup-already-complete" },
        { status: AUTH_STATUS.conflict },
      ),
    ),
  );
  open();
  await screen.findByRole("heading", { name: "Set up dl-tool" });
  fillSetup();
  fireEvent.click(screen.getByRole("button", { name: "Create account" }));
  await screen.findByRole("heading", { name: "Sign in" });
  expect(
    await screen.findByText("Setup is already complete. Sign in to continue."),
  ).toBeTruthy();
  expect(client.csrfToken()).toBeNull();
});

test.each([
  [
    AUTH_STATUS.unauthorized,
    "Wrong username or password.",
    {},
    "Wrong username or password.",
  ],
  [
    AUTH_STATUS.rateLimited,
    "Rate limited",
    { "Retry-After": "37" },
    "Too many attempts. Retry after 37 seconds.",
  ],
] as const)("TestLoginErrors: %s", async (status, detail, headers, message) => {
  server.use(
    http.post(client.apiUrl("auth/login"), () =>
      HttpResponse.json({ detail }, { status, headers }),
    ),
  );
  open();
  await screen.findByRole("heading", { name: "Sign in" });
  fill("Username", "unknown");
  fill("Password", "wrong");
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  expect((await screen.findByRole("alert")).textContent).toBe(message);
  expect(client.csrfToken()).toBeNull();
});

test("TestBootFailureDoesNotGuessAuthentication", async () => {
  me(HTTP_UNAVAILABLE);
  open();
  expect((await screen.findByRole("alert")).textContent).toBe(
    "Cannot reach the server. Try again.",
  );
  expect(screen.queryByRole("heading", { name: "Sign in" })).toBeNull();
  expect(bootRequests).toBe(1);
  me(HTTP_OK);
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await screen.findByRole("heading", { name: "Downloads" });
  await waitFor(() => expect(bootRequests).toBe(2));
});
