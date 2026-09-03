import { afterEach, describe, expect, test, vi } from "vitest";

import { api, apiUrl, basePath, eventsUrl, setCsrfToken } from "./client";

type FetchStub = (input: Request) => Promise<Response>;

// The server injects <base href="{base}/"> into index.html at serve time (doc 10 section 7.3
// rule 4); the tests inject the same element instead of mocking document.baseURI, so happy-dom
// resolves URLs exactly like the browser does.
function injectBaseHref(href: string): void {
  document.querySelector("base")?.remove();
  const base = document.createElement("base");
  base.setAttribute("href", href);
  document.head.appendChild(base);
}

function stubFetch(): ReturnType<typeof vi.fn<FetchStub>> {
  return vi.fn<FetchStub>(() =>
    Promise.resolve(
      new Response("{}", {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    ),
  );
}

function lastRequest(stub: ReturnType<typeof vi.fn<FetchStub>>): Request {
  return stub.mock.calls[0][0];
}

// The M0 schema exposes only GET and POST paths, so the PATCH and DELETE rows of the mutation
// matrix go through the untyped request() escape hatch; the middleware sees the same Request.
const raw = api as unknown as {
  request(
    method: string,
    url: string,
    init: { fetch: FetchStub },
  ): Promise<unknown>;
};

function callWithMethod(method: string, fetch: FetchStub): Promise<unknown> {
  if (method === "GET") return api.GET("/auth/me", { fetch });
  if (method === "POST") return api.POST("/auth/logout", { fetch });
  return raw.request(method, "/auth/me", { fetch });
}

afterEach(() => {
  document.querySelector("base")?.remove();
  setCsrfToken(null);
  vi.restoreAllMocks();
});

describe("TestApiUrlUsesInjectedBase", () => {
  test.each([
    ["", "http://localhost:3000/api/v1/tasks"],
    ["/dl-tool", "http://localhost:3000/dl-tool/api/v1/tasks"],
  ])("apiUrl('/tasks') resolves under base %s", (base, expected) => {
    injectBaseHref(base + "/");

    expect(apiUrl("/tasks")).toBe(expected);
    expect(basePath()).toBe(base);
  });

  test.each([
    ["", "http://localhost:3000/api/v1/events"],
    ["/dl-tool", "http://localhost:3000/dl-tool/api/v1/events"],
  ])("eventsUrl() resolves under base %s", (base, expected) => {
    injectBaseHref(base + "/");

    expect(eventsUrl()).toBe(expected);
  });
});

describe("TestCsrfHeaderOnMutationsOnly", () => {
  test.each([
    ["GET", false],
    ["POST", true],
    ["PATCH", true],
    ["DELETE", true],
  ])("%s carries X-DLTOOL-CSRF: %s", async (method, carried) => {
    setCsrfToken("test-csrf-token");
    const stub = stubFetch();

    await callWithMethod(method, stub);

    expect(lastRequest(stub).headers.get("X-DLTOOL-CSRF")).toBe(
      carried ? "test-csrf-token" : null,
    );
  });

  test("a POST with no stored token sends no header", async () => {
    const stub = stubFetch();

    await api.POST("/auth/logout", { fetch: stub });

    expect(lastRequest(stub).headers.get("X-DLTOOL-CSRF")).toBeNull();
  });

  test("an empty token is treated as absent", async () => {
    setCsrfToken("");
    const stub = stubFetch();

    await api.POST("/auth/logout", { fetch: stub });

    expect(lastRequest(stub).headers.get("X-DLTOOL-CSRF")).toBeNull();
  });
});

test("TestApiClientResolvesUnderInjectedBase", async () => {
  injectBaseHref("/dl-tool/");
  // The client captures document.baseURI at import time, so re-import with the base present —
  // mirroring production, where the server-injected <base> precedes the module scripts.
  vi.resetModules();
  try {
    const { api: apiUnderBase } = await import("./client");
    const stub = stubFetch();

    await apiUnderBase.GET("/auth/me", { fetch: stub });

    expect(lastRequest(stub).url).toBe(
      "http://localhost:3000/dl-tool/api/v1/auth/me",
    );
  } finally {
    // Drop the injected <base> and the base-captured client instance so later
    // tests resolve against the default origin.
    document.querySelector("base")?.remove();
    vi.resetModules();
  }
});

test("TestCredentialsSameOrigin", async () => {
  const stub = stubFetch();

  await api.GET("/auth/me", { fetch: stub });

  expect(lastRequest(stub).credentials).toBe("same-origin");
});
