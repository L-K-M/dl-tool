import fs from "node:fs";
import path from "node:path";
import {
  expect,
  test,
  type APIRequestContext,
  type Page,
} from "@playwright/test";
import { ADMIN, BASE_URL, STATE_DIR, stubTasks } from "./fixtures";

/**
 * The doc 09 section 3.6 table, in order. Every row is exercised with
 * keyboard input only; the last test asserts that the file never calls
 * pointer-style locator methods.
 */
export const SHORTCUTS = [
  { keys: "ArrowDown", expect: "focus moves to the next row" },
  { keys: "ArrowUp", expect: "focus moves to the previous row" },
  { keys: "Home", expect: "focus moves to the first row" },
  { keys: "End", expect: "focus moves to the last row" },
  { keys: "PageDown", expect: "focus moves one viewport down" },
  { keys: "PageUp", expect: "focus moves one viewport up" },
  { keys: "Space", expect: "the focused row toggles selection" },
  { keys: "Control+a", expect: "every row in the filter is selected" },
  { keys: "Enter", expect: "the detail pane opens on the focused row" },
  {
    keys: "Delete",
    expect: "the remove dialog opens with the delete-files box unticked",
  },
  {
    keys: "Shift+Delete",
    expect: "the remove dialog opens with the box ticked",
  },
  { keys: "Control+f", expect: "the toolbar filter box takes focus" },
  {
    keys: "Escape",
    expect: "the selection clears, or the focused dialog closes",
  },
  { keys: "?", expect: "the shortcut cheat-sheet opens" },
] as const;

const ROWS = 60;
const httpUnauthorized = 401;
const ADMIN_WAIT_MS = 240_000;
const ADMIN_POLL_MS = 500;
const MAX_TAB_STOPS = 64;

/**
 * The first-run account belongs to setup.spec.ts: spec files run on parallel
 * workers, and it can be scheduled late on a CI runner, so no other file may
 * ever call ensureAdmin — creating the account before its first test asserts
 * the /setup wizard breaks that spec. Poll /auth/me until it reports
 * something other than setup-required; a run without setup.spec.ts has no
 * admin at all, in which case fail with a clear message rather than minting
 * one. Duplicated in
 * a11y.spec.ts; the e2e specs cannot share a helper module without widening
 * this task's Files table.
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

/** Signs in through the form with the keyboard: fill, then Enter to submit. */
async function signInWithKeyboard(page: Page): Promise<void> {
  await page.goto("/login");
  await page.getByLabel("Username").fill(ADMIN.username);
  await page.getByLabel("Password", { exact: true }).fill(ADMIN.password);
  await page.getByLabel("Password", { exact: true }).press("Enter");
  await expect(page.getByRole("grid")).toBeVisible();
}

const SESSION_COOKIE = "dltool_session";
const SESSION_PATH = path.join(STATE_DIR, "e2e-session.json");

/**
 * The login throttle admits only a handful of attempts per source-IP window
 * and every worker counts as the same source, so the suite cannot afford one
 * login per test. The first test to sign in — on whichever worker — stores
 * the session cookie here; later tests inject it and skip the login
 * round-trip. STATE_DIR is wiped on every run, so a stale file cannot leak
 * across runs. Duplicated in a11y.spec.ts; the e2e specs cannot share a
 * helper module without widening this task's Files table.
 */
async function readSharedSession(): Promise<{
  name: string;
  value: string;
} | null> {
  try {
    const raw = JSON.parse(
      await fs.promises.readFile(SESSION_PATH, "utf8"),
    ) as { name?: string; value?: string };
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
  await signInWithKeyboard(page);
  await shareSession(page);
}

function focusedTaskId(page: Page): Promise<string | null> {
  return page.evaluate(
    () =>
      document.activeElement
        ?.closest("[data-task-id]")
        ?.getAttribute("data-task-id") ?? null,
  );
}

/** The focused row's 1-based aria-rowindex, including the header offset. */
function focusedRowIndex(page: Page): Promise<number | null> {
  return page.evaluate(() => {
    const row = document.activeElement?.closest('[role="row"]');
    const index = row?.getAttribute("aria-rowindex");
    return index === null || index === undefined ? null : Number(index);
  });
}

function insideGrid(page: Page): Promise<boolean> {
  return page.evaluate(
    () =>
      document
        .querySelector('[role="grid"]')
        ?.contains(document.activeElement) ?? false,
  );
}

const rowOf = (page: Page, id: string) =>
  page.locator(`[data-task-id="${id}"]`);

const selectedRows = (page: Page) =>
  page.locator('[data-task-id][aria-selected="true"]').count();

/**
 * Assert FIRST that Tab from the toolbar reaches the grid, then that the grid
 * is exactly one tab stop: the next Tab leaves it entirely.
 */
test("Tab moves through the grid exactly once", async ({ page, request }) => {
  test.setTimeout(300_000);
  await stubTasks(page, ROWS);
  await signIn(page, request);

  // The last toolbar control is the user menu; Tab forward until focus lands
  // inside the grid.
  await page.getByRole("button", { name: "User menu" }).focus();
  let reached = false;
  for (let i = 0; i < MAX_TAB_STOPS && !reached; i += 1) {
    await page.keyboard.press("Tab");
    reached = await insideGrid(page);
  }
  expect(reached, "Tab never landed inside the grid").toBe(true);

  // The roving tabindex makes the whole grid one stop: the very next Tab
  // leaves it.
  await page.keyboard.press("Tab");
  expect(await insideGrid(page)).toBe(false);
});

test("every documented shortcut works with keyboard input only", async ({
  page,
  request,
}) => {
  test.setTimeout(300_000);
  const fixture = await stubTasks(page, ROWS);
  await signIn(page, request);

  const grid = page.getByRole("grid");
  const detail = page.getByRole("region", { name: "Task details" });
  const filterBox = page.getByLabel("Filter tasks by name");

  // The roving tab stop starts on the first row; focusing it is not a pointer
  // gesture.
  await rowOf(page, fixture.ids[0]).focus();
  await expect.poll(() => focusedTaskId(page)).toBe(fixture.ids[0]);

  // PageDown/PageUp step by floor(grid height / row height); measure it so the
  // assertion holds at any viewport size.
  const gridHeight = await grid.evaluate((el) => el.clientHeight);
  const rowHeight = await rowOf(page, fixture.ids[0]).evaluate(
    (el) => el.getBoundingClientRect().height,
  );
  const pageStep = Math.max(1, Math.floor(gridHeight / rowHeight));

  for (const shortcut of SHORTCUTS) {
    await test.step(`${shortcut.keys} — ${shortcut.expect}`, async () => {
      switch (shortcut.keys) {
        case "ArrowDown": {
          const before = await focusedRowIndex(page);
          await page.keyboard.press("ArrowDown");
          expect(await focusedRowIndex(page)).toBe((before ?? 0) + 1);
          break;
        }
        case "ArrowUp": {
          const before = await focusedRowIndex(page);
          await page.keyboard.press("ArrowUp");
          expect(await focusedRowIndex(page)).toBe((before ?? 1) - 1);
          break;
        }
        case "Home": {
          await page.keyboard.press("ArrowDown");
          await page.keyboard.press("ArrowDown");
          await page.keyboard.press("Home");
          await expect.poll(() => focusedTaskId(page)).toBe(fixture.ids[0]);
          break;
        }
        case "End": {
          await page.keyboard.press("End");
          await expect
            .poll(() => focusedTaskId(page), { timeout: 15_000 })
            .toBe(fixture.ids[ROWS - 1]);
          break;
        }
        case "PageDown": {
          await page.keyboard.press("Home");
          await page.keyboard.press("PageDown");
          // aria-rowindex is 1-based plus the header row: index pageStep.
          expect(await focusedRowIndex(page)).toBe(pageStep + 2);
          break;
        }
        case "PageUp": {
          await page.keyboard.press("PageUp");
          expect(await focusedRowIndex(page)).toBe(2);
          break;
        }
        case "Space": {
          const focused = (await focusedTaskId(page)) ?? fixture.ids[0];
          await page.keyboard.press("Space");
          await expect(rowOf(page, focused)).toHaveAttribute(
            "aria-selected",
            "true",
          );
          break;
        }
        case "Control+a": {
          await page.keyboard.press("Control+a");
          const mounted = await page.locator("[data-task-id]").count();
          await expect.poll(() => selectedRows(page)).toBe(mounted);
          await expect(detail).toContainText(`${ROWS} tasks`);
          break;
        }
        case "Enter": {
          await page.keyboard.press("Enter");
          await expect(
            detail.getByRole("tab", { name: "Transfer" }),
          ).toBeVisible();
          break;
        }
        case "Delete": {
          await page.keyboard.press("Delete");
          const dialog = page.getByRole("dialog", {
            name: "Remove 1 task?",
          });
          await expect(dialog).toBeVisible();
          await expect(
            dialog.getByRole("checkbox", {
              name: "Also delete downloaded files",
            }),
          ).not.toBeChecked();
          // Escape closes the dialog; the grid never sees it because the
          // dialog is portaled outside the grid element.
          await page.keyboard.press("Escape");
          await expect(dialog).toBeHidden();
          // Doc 09: closing a dialog returns focus to the row that opened it.
          await expect.poll(() => focusedTaskId(page)).toBe(fixture.ids[0]);
          break;
        }
        case "Shift+Delete": {
          await page.keyboard.press("Shift+Delete");
          const dialog = page.getByRole("dialog", {
            name: "Remove 1 task?",
          });
          await expect(dialog).toBeVisible();
          await expect(dialog).toContainText("You used Shift+Delete");
          await expect(
            dialog.getByRole("checkbox", {
              name: "Also delete downloaded files",
            }),
          ).toBeChecked();
          await page.keyboard.press("Escape");
          await expect(dialog).toBeHidden();
          await expect.poll(() => focusedTaskId(page)).toBe(fixture.ids[0]);
          break;
        }
        case "Control+f": {
          await page.keyboard.press("Control+f");
          await expect(filterBox).toBeFocused();
          break;
        }
        case "Escape": {
          // Focus is in the filter box after Control+f and the editable-target
          // guard keeps it there, so step back onto the row before asserting
          // the documented selection clear.
          await rowOf(page, fixture.ids[0]).focus();
          await page.keyboard.press("Escape");
          await expect.poll(() => selectedRows(page)).toBe(0);
          break;
        }
        case "?": {
          await page.keyboard.press("?");
          const dialog = page.getByRole("dialog", {
            name: "Keyboard shortcuts",
          });
          await expect(dialog).toBeVisible();
          await page.keyboard.press("Escape");
          await expect(dialog).toBeHidden();
          break;
        }
      }
    });
  }
});

test("typing in the filter box does not fire a shortcut", async ({
  page,
  request,
}) => {
  test.setTimeout(300_000);
  const fixture = await stubTasks(page, ROWS);
  await signIn(page, request);

  // Select a row so the guard is observable: Escape would clear it and Delete
  // would open the remove dialog if the keydown handler did not bail out.
  await rowOf(page, fixture.ids[0]).focus();
  await page.keyboard.press("Space");
  await expect(rowOf(page, fixture.ids[0])).toHaveAttribute(
    "aria-selected",
    "true",
  );

  const filterBox = page.getByLabel("Filter tasks by name");
  await page.keyboard.press("Control+f");
  await expect(filterBox).toBeFocused();

  // "?" would open the cheat-sheet, Delete the remove dialog, Escape would
  // clear the selection and Enter would open the detail pane. Inside the
  // input every one of them must edit text instead.
  await filterBox.pressSequentially("a?z");
  await page.keyboard.press("Delete");
  await page.keyboard.press("Escape");
  await page.keyboard.press("Enter");
  await page.keyboard.press("Control+a");

  await expect(filterBox).toHaveValue("a?z");
  await expect(filterBox).toBeFocused();
  expect(await page.getByRole("dialog").count()).toBe(0);
  await expect(rowOf(page, fixture.ids[0])).toHaveAttribute(
    "aria-selected",
    "true",
  );
});

// The whole point of this spec is pointer-free operation: guard it by
// scanning its own source for pointer-style calls.
test("this spec never uses pointer input", () => {
  const source = fs.readFileSync(test.info().file, "utf8");
  expect(source).not.toMatch(
    /\.(click|dblclick|tap|dragTo|hover)\(|page\.mouse/,
  );
});
