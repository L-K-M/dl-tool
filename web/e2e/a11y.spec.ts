import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";
import { stubTasks } from "./fixtures";

/** Screens that exist at M3. The Search and RSS screens are appended by the milestones that add
 *  them, so doc 13 section 6.2's five-screen list is complete once M4 and M5 land. */
export const SCREENS: { name: string; path: string }[] = [
  { name: "setup", path: "/setup" },
  { name: "login", path: "/login" },
  { name: "tasks", path: "/" },
  { name: "detail", path: "/tasks/all" },
  { name: "settings", path: "/settings/general" },
];

const AXE_TAGS = [
  "wcag2a",
  "wcag2aa",
  "wcag21a",
  "wcag21aa",
  "wcag22aa",
] as const;
const httpUnauthorized = 401;
const BANNER_WAIT_MS = 20_000;
const MAX_MOUNTED_ROWS = 200;
const STUB_ROWS = 60;

const AUTHENTICATED = {
  csrf_token: "e2e-csrf-token",
  user: {
    id: "usr_e2e",
    username: "e2e-admin",
    enabled: true,
    locale: "en",
    last_login_at: null,
    created_at: "2026-01-01T00:00:00Z",
  },
};
const FS_ROOT = {
  path: "/data",
  writable: true,
  free_bytes: 500_000_000_000,
  total_bytes: 1_000_000_000_000,
};

declare global {
  interface Window {
    __dltoolE2E?: {
      openSources(): { emit(type: string, payload?: unknown): void }[];
      emitSync(payload: unknown): void;
    };
  }
}

/**
 * Every screen renders from the /auth/me answer; stub it so all three session
 * states are deterministic. Authenticated runs also stub the read-only
 * endpoints the dialogs pull on open. No spec may mint the first-run account:
 * setup.spec.ts owns it and can be scheduled late on a parallel CI runner, so
 * an early /auth/setup would break its wizard assertion. Duplicated in
 * keyboard.spec.ts; the e2e specs cannot share a helper module without
 * widening this task's Files table.
 */
async function stubSession(
  page: Page,
  kind: "setup-required" | "unauthenticated" | "authenticated",
): Promise<void> {
  if (kind === "authenticated") {
    await page.route("**/api/v1/auth/me", (route) =>
      route.fulfill({ json: AUTHENTICATED }),
    );
    await page.route("**/api/v1/categories", (route) =>
      route.fulfill({ json: { categories: [] } }),
    );
    await page.route("**/api/v1/tags", (route) =>
      route.fulfill({ json: { tags: [] } }),
    );
    await page.route("**/api/v1/fs/roots", (route) =>
      route.fulfill({ json: { roots: [FS_ROOT] } }),
    );
    await page.route("**/api/v1/fs/free-space*", (route) =>
      route.fulfill({
        json: {
          path: FS_ROOT.path,
          free_bytes: FS_ROOT.free_bytes,
          total_bytes: FS_ROOT.total_bytes,
        },
      }),
    );
    await page.route("**/api/v1/fs/browse*", (route) =>
      route.fulfill({
        json: {
          path: "/data",
          parent: null,
          separator: "/",
          writable: true,
          free_bytes: FS_ROOT.free_bytes,
          total_bytes: FS_ROOT.total_bytes,
          directories: [{ name: "media", path: "/data/media", writable: true }],
        },
      }),
    );
    return;
  }
  await page.route("**/api/v1/auth/me", (route) =>
    route.fulfill({
      status: httpUnauthorized,
      contentType: "application/problem+json",
      body: JSON.stringify({
        type: `/problems/${kind}`,
        title: "Unauthorized",
        status: httpUnauthorized,
      }),
    }),
  );
}

/** Navigates to an authenticated screen under the stubbed session. */
async function openAuthenticated(page: Page, path: string): Promise<void> {
  await stubSession(page, "authenticated");
  await page.goto(path);
}

/** Doc 13 section 6.2: zero serious or critical violations, printed per screen. */
async function runAxe(page: Page, label: string): Promise<void> {
  const results = await new AxeBuilder({ page }).withTags(AXE_TAGS).analyze();
  const serious = results.violations.filter(
    (v) => v.impact === "serious",
  ).length;
  const critical = results.violations.filter(
    (v) => v.impact === "critical",
  ).length;
  console.log(
    `axe ${label}: ${serious} serious, ${critical} critical, ` +
      `${results.violations.length} total`,
  );
  const blocking = results.violations.filter(
    (v) => v.impact === "serious" || v.impact === "critical",
  );
  expect(blocking, JSON.stringify(blocking, null, 2)).toEqual([]);
}

for (const screen of SCREENS) {
  test(`axe scan: ${screen.name}`, async ({ page }) => {
    switch (screen.name) {
      case "setup":
        await stubSession(page, "setup-required");
        await page.goto(screen.path);
        await expect(
          page.getByRole("heading", { name: "Set up dl-tool" }),
        ).toBeVisible();
        break;
      case "login":
        await stubSession(page, "unauthenticated");
        await page.goto(screen.path);
        await expect(
          page.getByRole("heading", { name: "Sign in" }),
        ).toBeVisible();
        break;
      case "detail": {
        const fixture = await stubTasks(page, STUB_ROWS);
        await openAuthenticated(page, screen.path);
        // Open the pane on a task so the scan covers the populated detail view.
        await expect(
          page.locator(`[data-task-id="${fixture.ids[0]}"]`),
        ).toBeVisible();
        await page.locator(`[data-task-id="${fixture.ids[0]}"]`).focus();
        await page.keyboard.press("Enter");
        await expect(
          page
            .getByRole("region", { name: "Task details" })
            .getByRole("tab", { name: "Transfer" }),
        ).toBeVisible();
        break;
      }
      case "settings":
        await openAuthenticated(page, screen.path);
        await expect(
          page.getByRole("heading", { name: "General" }),
        ).toBeVisible();
        break;
      default:
        // tasks
        await stubTasks(page, STUB_ROWS);
        await openAuthenticated(page, screen.path);
        await expect(page.getByRole("grid")).toBeVisible();
        await expect(page.locator("[data-task-id]").first()).toBeVisible();
    }
    await runAxe(page, screen.name);
  });
}

test("axe scan: add-task dialog", async ({ page }) => {
  await stubTasks(page, STUB_ROWS);
  await openAuthenticated(page, "/");
  await page.getByRole("button", { name: "Add", exact: true }).click();
  await expect(
    page.getByRole("dialog", { name: "Create Download Task" }),
  ).toBeVisible();
  await runAxe(page, "add-task dialog");
});

test("axe scan: folder browser dialog", async ({ page }) => {
  await stubTasks(page, STUB_ROWS);
  await openAuthenticated(page, "/");
  await page.getByRole("button", { name: "Add", exact: true }).click();
  const addDialog = page.getByRole("dialog", {
    name: "Create Download Task",
  });
  await expect(addDialog).toBeVisible();
  await addDialog.getByRole("button", { name: "Select" }).click();
  await expect(
    page.getByRole("dialog", { name: "Select destination" }),
  ).toBeVisible();
  await expect(page.getByRole("listbox", { name: "Folders" })).toBeVisible();
  await runAxe(page, "folder browser dialog");
});

test("aria-rowcount is the total row count", async ({ page }) => {
  await stubTasks(page, 10_000);
  await openAuthenticated(page, "/");
  const grid = page.getByRole("grid");
  await expect(grid).toHaveAttribute("aria-rowcount", "10000");
  expect(await page.getByRole("row").count()).toBeLessThan(MAX_MOUNTED_ROWS);
  // aria-colcount covers the visible columns, and the roving tabindex leaves
  // exactly one stop inside the grid (doc 09 section 10.4).
  const colcount = Number(await grid.getAttribute("aria-colcount"));
  expect(colcount).toBe(await page.getByRole("columnheader").count());
  expect(await grid.locator('[tabindex="0"]').count()).toBe(1);
});

test("live regions announce status changes", async ({ page }) => {
  await stubTasks(page, STUB_ROWS);
  await openAuthenticated(page, "/");

  // The status bar is a polite live region.
  const status = page.locator("footer").getByRole("status");
  await expect(status).toHaveAttribute("aria-live", "polite");

  // A dropped stream surfaces the reconnect banner as an assertive alert.
  await page.waitForFunction(
    () => (window.__dltoolE2E?.openSources().length ?? 0) > 0,
  );
  await page.evaluate(() =>
    window.__dltoolE2E?.openSources().at(-1)?.emit("error"),
  );
  const banner = page
    .getByRole("alert")
    .filter({ hasText: "Lost connection to the server." });
  await expect(banner).toBeVisible({ timeout: BANNER_WAIT_MS });
});

test("the settings dirty bar announces its count", async ({ page }) => {
  await openAuthenticated(page, "/settings/general");
  await expect(page.getByRole("heading", { name: "General" })).toBeVisible();
  const announcer = page.locator("main").locator('p[aria-live="polite"]');
  await expect(announcer).toHaveText("");
  await page.getByLabel("Density").selectOption("compact");
  // The polite announcer and the visible bar carry the same count.
  await expect(announcer).toHaveText("1 unsaved change");
  await expect(page.getByRole("button", { name: "Revert" })).toBeVisible();
});
