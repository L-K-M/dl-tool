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

test("TestEntrypointRendersTaskGridUnderBase", async () => {
  let bootRequests = 0;
  vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockReturnValue(320);
  vi.spyOn(HTMLElement.prototype, "offsetWidth", "get").mockReturnValue(1024);
  server.use(
    http.get("*/dl-tool/api/v1/tasks", ({ request }) => {
      const url = new URL(request.url);
      expect(url.searchParams.get("state")).toBe("all");
      expect(url.searchParams.get("limit")).toBe("500");
      return HttpResponse.json({
        items: [
          {
            id: "entry-task",
            name: "Entrypoint download",
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
      });
    }),
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
  const task = await screen.findByText("Entrypoint download");
  expect(screen.getByRole("grid").contains(task)).toBe(true);
  expect(screen.getByRole("grid").getAttribute("aria-rowcount")).toBe("1");
  expect(host.contains(screen.getByTestId("app-root"))).toBe(true);
  expect(bootRequests).toBe(1);
});
