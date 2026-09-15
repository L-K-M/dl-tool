import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
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
import { FolderBrowserDialog } from "./FolderBrowserDialog";

interface Dir {
  name: string;
  path: string;
  writable: boolean;
}

// The in-memory server filesystem each test mutates through the handlers.
let dirs: Record<string, { parent: string | null; dirs: Dir[] }>;
let browseCalls: string[];
let freeSpaceCalls: string[];
let mkdirBodies: { path: string; name: string }[];

function dir(path: string, writable = true): Dir {
  return { name: path.slice(path.lastIndexOf("/") + 1), path, writable };
}

function listing(path: string) {
  const node = dirs[path];
  return HttpResponse.json({
    path,
    parent: node?.parent ?? null,
    separator: "/",
    writable: true,
    free_bytes: 442381537280,
    total_bytes: 2000398934016,
    directories: node?.dirs ?? [],
  });
}

const server = setupServer(
  http.get("*/api/v1/fs/roots", () =>
    HttpResponse.json({
      roots: [
        {
          path: "/data",
          writable: true,
          free_bytes: 442381537280,
          total_bytes: 2000398934016,
        },
      ],
    }),
  ),
  http.get("*/api/v1/fs/browse", ({ request }) => {
    const path = new URL(request.url).searchParams.get("path") ?? "";
    browseCalls.push(path);
    if (path === "/etc") {
      return HttpResponse.json(
        { type: "/problems/path-rejected", title: "Forbidden", status: 403 },
        { status: 403 },
      );
    }
    if (!(path in dirs)) {
      return HttpResponse.json(
        { type: "/problems/not-found", title: "Not Found", status: 404 },
        { status: 404 },
      );
    }
    return listing(path);
  }),
  http.get("*/api/v1/fs/free-space", ({ request }) => {
    const path = new URL(request.url).searchParams.get("path") ?? "";
    freeSpaceCalls.push(path);
    return HttpResponse.json({
      path,
      free_bytes: 1073741824,
      total_bytes: 2147483648,
    });
  }),
  http.post("*/api/v1/fs/mkdir", async ({ request }) => {
    const body = (await request.json()) as { path: string; name: string };
    mkdirBodies.push(body);
    const node = dirs[body.path];
    if (node && !node.dirs.some((d) => d.name === body.name)) {
      const created = {
        name: body.name,
        path: body.path + "/" + body.name,
        writable: true,
      };
      node.dirs.push(created);
      dirs[created.path] = { parent: body.path, dirs: [] };
      return HttpResponse.json(
        { path: created.path, writable: true },
        { status: 201 },
      );
    }
    return HttpResponse.json(
      { type: "/problems/conflict", title: "Conflict", status: 409 },
      { status: 409 },
    );
  }),
);

beforeAll(() => {
  initI18n();
  server.listen({ onUnhandledRequest: "error" });
});
beforeEach(() => {
  dirs = {
    "/data": {
      parent: null,
      dirs: [dir("/data/incoming", false), dir("/data/iso")],
    },
    "/data/incoming": { parent: "/data", dirs: [] },
    "/data/iso": { parent: "/data", dirs: [dir("/data/iso/archive")] },
    "/data/iso/archive": { parent: "/data/iso", dirs: [] },
  };
  browseCalls = [];
  freeSpaceCalls = [];
  mkdirBodies = [];
});
afterEach(() => {
  cleanup();
  server.resetHandlers();
});
afterAll(() => server.close());

function mount() {
  const onSelect = vi.fn();
  const onOpenChange = vi.fn();
  render(
    <FolderBrowserDialog
      open
      onSelect={onSelect}
      onOpenChange={onOpenChange}
    />,
  );
  return { onSelect, onOpenChange };
}

test("TestDescendUpdatesBreadcrumb", async () => {
  const { onSelect, onOpenChange } = mount();
  await screen.findByRole("dialog");
  await screen.findByText("iso");

  // Arrow keys move the cursor; Enter descends into the highlighted row.
  const list = screen.getByRole("listbox");
  fireEvent.keyDown(list, { key: "ArrowDown" }); // incoming
  fireEvent.keyDown(list, { key: "ArrowDown" }); // iso
  fireEvent.keyDown(list, { key: "Enter" });
  await screen.findByText("archive");

  const crumbs = screen.getByRole("navigation");
  expect(within(crumbs).getByRole("button", { name: "iso" })).toBeTruthy();
  expect(within(crumbs).getByRole("button", { name: "data" })).toBeTruthy();

  // The ".." row walks back up; Backspace does the same.
  fireEvent.keyDown(list, { key: "Backspace" });
  await waitFor(() => expect(screen.queryByText("archive")).toBeNull());
  await screen.findByText("iso");

  // Select returns the current path through onSelect and closes.
  const select = screen.getByRole("button", { name: "Select" });
  fireEvent.click(select);
  await waitFor(() => expect(onSelect).toHaveBeenCalledWith("/data"));
  expect(onOpenChange).toHaveBeenCalledWith(false);
});

test("TestBrowserKeepsListingOnPathRejected", async () => {
  mount();
  await screen.findByText("iso");

  const field = screen.getByRole("textbox", { name: "Path" });
  fireEvent.change(field, { target: { value: "/etc" } });
  fireEvent.blur(field);

  await screen.findByText("That folder is outside the allowed download roots.");
  // The previous listing stays on screen.
  expect(screen.getByText("iso")).toBeTruthy();
  expect(screen.getByText("incoming")).toBeTruthy();
});

test("TestBrowserShowsNotFound", async () => {
  mount();
  await screen.findByText("iso");

  const field = screen.getByRole("textbox", { name: "Path" });
  fireEvent.change(field, { target: { value: "/data/missing" } });
  fireEvent.blur(field);

  await screen.findByText("Folder not found or unreadable.");
});

test("TestUnwritableFolderCannotBeSelected", async () => {
  mount();
  const row = await screen.findByText("incoming");

  // The lock badge rides the unwritable row.
  const lock = screen.getByRole("img", {
    name: "This folder is not writable.",
  });
  expect(row.closest("[role=option]")!.contains(lock)).toBe(true);

  // Highlighting it makes it the prospective selection — and Select
  // refuses it with a tooltip.
  fireEvent.click(row);
  const select = screen.getByRole("button", {
    name: "Select",
  }) as HTMLButtonElement;
  expect(select.disabled).toBe(true);
  expect(select.getAttribute("title")).toBe("This folder is not writable.");

  // The free-space line followed the selection through GET /fs/free-space.
  await waitFor(() => expect(freeSpaceCalls).toContain("/data/incoming"));
});

test("TestNewFolderPostsAndRelists", async () => {
  mount();
  await screen.findByText("iso");
  const before = browseCalls.length;

  fireEvent.click(screen.getByRole("button", { name: "New folder" }));
  const nameField = screen.getByRole("textbox", { name: "Folder name" });
  fireEvent.change(nameField, { target: { value: "fresh" } });
  fireEvent.keyDown(nameField, { key: "Enter" });

  // The new directory appears after the re-list.
  await screen.findByText("fresh");
  expect(mkdirBodies).toEqual([{ path: "/data", name: "fresh" }]);
  expect(browseCalls.length).toBeGreaterThan(before);
  expect(browseCalls[browseCalls.length - 1]).toBe("/data");
});
