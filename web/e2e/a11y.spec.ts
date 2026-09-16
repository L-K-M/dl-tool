import fs from "node:fs";
import path from "node:path";
import AxeBuilder from "@axe-core/playwright";
import {
  expect,
  test,
  type APIRequestContext,
  type Page,
} from "@playwright/test";
import { BASE_URL, STATE_DIR, loginAsAdmin, stubTasks } from "./fixtures";

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
const ADMIN_WAIT_MS = 240_000;
const ADMIN_POLL_MS = 500;
const BANNER_WAIT_MS = 20_000;
const MAX_MOUNTED_ROWS = 200;
const STUB_ROWS = 60;

declare global {
  interface Window {
    __dltoolE2E?: {
      openSources(): { emit(type: string, payload?: unknown): void }[];
      emitSync(payload: unknown): void;
    };
  }
}

/**
 * The first-run account belongs to setup.spec.ts: spec files run on parallel
 * workers, and it can be scheduled late on a CI runner, so no other file may
 * ever call ensureAdmin — creating the account before its first test asserts
 * the /setup wizard breaks that spec. Poll /auth/me until it reports
 * something other than setup-required; a run without setup.spec.ts has no
 * admin at all, in which case fail with a clear message rather than minting
 * one. Duplicated in
 * keyboard.spec.ts; the e2e specs cannot share a helper module without
 * widening this task's Files table.
 */
async function adminExists(request: APIRequestContext): Promise<boolean> {
  const response = await request.get(`${BASE_URL}/api/v1/auth/me`);
  if (response.status() !== httpUnauthorized) return true;
  const body = (await response.json().catch(() => null)) as {
    type?: string;
  } | null;
  return body?.type !== "/problems/setup-required";
}

async function waitForAdmin(request: APIRequestContext): Promise<void> {
  const deadline = Date.now() + ADMIN_WAIT_MS;
  while (!(await adminExists(request))) {
    if (Date.now() >= deadline) {
      throw new Error(
        "the first-run admin never appeared; setup.spec.ts owns account " +
          "creation, so run the full make e2e suite",
      );
    }
    await new Promise((resolve) => setTimeout(resolve, ADMIN_POLL_MS));
  }
}

const SESSION_COOKIE = "dltool_session";
const SESSION_PATH = path.join(STATE_DIR, "e2e-session.json");

/**
 * The login throttle admits only a handful of attempts per source-IP window
 * and every worker counts as the same source, so the suite cannot afford one
 * login per test. The first test to sign in — on whichever worker — stores
 * the session cookie here; later tests inject it and skip the login
 * round-trip. STATE_DIR is wiped on every run, so a stale file cannot leak
 * across runs. Duplicated in keyboard.spec.ts; the e2e specs cannot share a
 * helper module without widening this task's Files table.
 */
async function readSharedSession(): Promise<{
  name: string;
  value: string;
} | null> {
  try {
    const raw = JSON.parse(
      await fs.promises.readFile(SESSION_PATH, "utf8"),
    ) as {
      name?: string;
      value?: string;
    };
    return raw.name === SESSION_COOKIE && typeof raw.value === "string"
      ? { name: raw.name, value: raw.value }
      : null;
  } catch {
    return null;
  }
}

async function shareSession(page: Page): Promise<void> {
  const cookie = (await page.context().cookies(BASE_URL)).find(
    (c) => c.name === SESSION_COOKIE,
  );
  if (cookie === undefined) return;
  await fs.promises.writeFile(
    SESSION_PATH,
    JSON.stringify({ name: cookie.name, value: cookie.value }),
  );
}

/** Signs in once per run, then reuses the shared session cookie. */
async function signIn(page: Page, request: APIRequestContext): Promise<void> {
  const shared = await readSharedSession();
  if (shared !== null) {
    await page.context().addCookies([{ ...shared, url: BASE_URL }]);
    await page.goto("/");
    try {
      await expect(page.getByRole("grid")).toBeVisible({ timeout: 15_000 });
      return;
    } catch {
      // The shared session was rejected; fall through to a real login.
    }
  }
  await waitForAdmin(request);
  await loginAsAdmin(page);
  await shareSession(page);
}

/**
 * The auth screens render from the /auth/me answer; stub it so the setup
 * wizard stays reachable after the account exists and the login form before
 * it. The scanned DOM is identical to the real state.
 */
async function stubSession(
  page: Page,
  type: "setup-required" | "unauthenticated",
): Promise<void> {
  await page.route("**/api/v1/auth/me", (route) =>
    route.fulfill({
      status: httpUnauthorized,
      contentType: "application/problem+json",
      body: JSON.stringify({
        type: `/problems/${type}`,
        title: "Unauthorized",
        status: httpUnauthorized,
      }),
    }),
  );
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
  test(`axe scan: ${screen.name}`, async ({ page, request }) => {
    test.setTimeout(300_000);
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
        await signIn(page, request);
        await page.goto(screen.path);
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
        await signIn(page, request);
        await page.goto(screen.path);
        await expect(
          page.getByRole("heading", { name: "General" }),
        ).toBeVisible();
        break;
      default:
        // tasks
        await stubTasks(page, STUB_ROWS);
        await signIn(page, request);
        await expect(page.getByRole("grid")).toBeVisible();
        await expect(page.locator("[data-task-id]").first()).toBeVisible();
    }
    await runAxe(page, screen.name);
  });
}

test("axe scan: add-task dialog", async ({ page, request }) => {
  test.setTimeout(300_000);
  await stubTasks(page, STUB_ROWS);
  await signIn(page, request);
  await page.getByRole("button", { name: "Add", exact: true }).click();
  await expect(
    page.getByRole("dialog", { name: "Create Download Task" }),
  ).toBeVisible();
  await runAxe(page, "add-task dialog");
});

test("axe scan: folder browser dialog", async ({ page, request }) => {
  test.setTimeout(300_000);
  await stubTasks(page, STUB_ROWS);
  await signIn(page, request);
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

test("aria-rowcount is the total row count", async ({ page, request }) => {
  test.setTimeout(300_000);
  await stubTasks(page, 10_000);
  await signIn(page, request);
  const grid = page.getByRole("grid");
  await expect(grid).toHaveAttribute("aria-rowcount", "10000");
  expect(await page.getByRole("row").count()).toBeLessThan(MAX_MOUNTED_ROWS);
  // aria-colcount covers the visible columns, and the roving tabindex leaves
  // exactly one stop inside the grid (doc 09 section 10.4).
  const colcount = Number(await grid.getAttribute("aria-colcount"));
  expect(colcount).toBe(await page.getByRole("columnheader").count());
  expect(await grid.locator('[tabindex="0"]').count()).toBe(1);
});

test("live regions announce status changes", async ({ page, request }) => {
  test.setTimeout(300_000);
  await stubTasks(page, STUB_ROWS);
  await signIn(page, request);

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

test("the settings dirty bar announces its count", async ({
  page,
  request,
}) => {
  test.setTimeout(300_000);
  await signIn(page, request);
  await page.goto("/settings/general");
  await expect(page.getByRole("heading", { name: "General" })).toBeVisible();
  const announcer = page.locator("main").locator('p[aria-live="polite"]');
  await expect(announcer).toHaveText("");
  await page.getByLabel("Density").selectOption("compact");
  // The polite announcer and the visible bar carry the same count.
  await expect(announcer).toHaveText("1 unsaved change");
  await expect(page.getByRole("button", { name: "Revert" })).toBeVisible();
});
