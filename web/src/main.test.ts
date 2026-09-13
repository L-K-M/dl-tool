import { act, screen } from "@testing-library/react";
import * as ReactDOM from "react-dom/client";
import type { Root } from "react-dom/client";
import { HttpResponse, http } from "msw";
import { setupServer } from "msw/node";
import { afterAll, afterEach, beforeAll, expect, test, vi } from "vitest";

vi.mock("react-dom/client", { spy: true });

const server = setupServer();
let root: Root | undefined;
let host: HTMLDivElement | undefined;

beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(async () => {
  await act(() => root?.unmount());
  host?.remove();
  server.resetHandlers();
  (await import("./api/client")).setCsrfToken(null);
  document.querySelector("base")?.remove();
  window.history.replaceState(null, "", "/");
  vi.restoreAllMocks();
});
afterAll(() => server.close());

test("renders the application root", async () => {
  let bootRequests = 0;
  server.use(
    http.get("*/dl-tool/api/v1/auth/me", () => {
      bootRequests++;
      return HttpResponse.json({
        user: {
          id: "usr_test",
          username: "operator",
          enabled: true,
          locale: "en",
          last_login_at: null,
          created_at: "2026-09-01T09:00:00Z",
        },
        csrf_token: "mounted-session",
      });
    }),
  );
  const base = document.createElement("base");
  base.href = "/dl-tool/";
  document.head.appendChild(base);
  window.history.replaceState(null, "", "/dl-tool");
  host = document.createElement("div");
  host.id = "root";
  document.body.appendChild(host);
  const mount = vi.mocked(ReactDOM.createRoot);

  // Execute the real entrypoint, including its StrictMode and root lookup.
  await act(async () => {
    await import("./main");
    root = mount.mock.results[0].value as Root;
  });
  await screen.findByRole("main");
  expect(mount).toHaveBeenCalledWith(host);
  expect(screen.getByTestId("app-root").textContent).toBe("Downloads");
  expect(host.contains(screen.getByTestId("app-root"))).toBe(true);
  expect(bootRequests).toBe(1);
});
