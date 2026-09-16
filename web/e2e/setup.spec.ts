import fs from "node:fs";
import path from "node:path";
import { expect, test } from "@playwright/test";
import {
  ADMIN,
  BASE_URL,
  STATE_DIR,
  ensureAdmin,
  loginAsAdmin,
  readProgressNow,
  readSetupToken,
  scriptDuration,
  stubTasks,
} from "./fixtures";

const ROW_COUNT = 10_000;
const MEASURED_TICKS = 10;
const TICK_MS = 1_000;
const BUDGET_MS = 8;
const MAX_MOUNTED_ROWS = 200;
const httpConflict = 409;

// The second test relies on the account the first one creates.
test.describe.configure({ mode: "serial" });

test("first run creates the admin", async ({ page }) => {
  await page.goto("/");
  await expect(page).toHaveURL(/\/setup$/);
  await expect(
    page.getByRole("heading", { name: "Set up dl-tool" }),
  ).toBeVisible();
  await page.getByLabel("Setup token").fill(await readSetupToken());
  await page.getByLabel("Username").fill(ADMIN.username);
  await page.getByLabel("Password", { exact: true }).fill(ADMIN.password);
  await page.getByLabel("Confirm password").fill(ADMIN.password);
  await page.getByRole("button", { name: "Create account" }).click();
  await expect(page.getByTestId("app-root")).toBeVisible();
  await expect(page.getByRole("grid")).toBeVisible();
});

test("a second setup attempt is rejected", async ({ request }) => {
  const response = await request.post(`${BASE_URL}/api/v1/auth/setup`, {
    data: {
      setup_token: "second-attempt",
      ...ADMIN,
      locale: "en",
    },
  });
  expect(response.status()).toBe(httpConflict);
  expect((await response.json()).type).toBe("/problems/setup-already-complete");
});

// Nearest-rank p95 over ten ticks is the maximum, so one environmental spike
// (GC, CPU steal on a shared runner) fails the run even when the grid is well
// inside budget. Scope a single retry to this test — Playwright only retries
// tests whose own budget is unspent, so the serial pair above never re-runs —
// while a genuinely slow implementation still fails on the retry.
test.describe("grid performance", () => {
  test.describe.configure({ retries: 1 });

  test("the grid stays inside the scripting budget", async ({
    page,
    context,
    request,
  }) => {
    test.setTimeout(180_000);
    await ensureAdmin(request);
    const fixture = await stubTasks(page, ROW_COUNT);
    await loginAsAdmin(page);

    // Initial snapshot over the fixture stream, then the paginated list
    // hydrates the same rows (doc 13 section 6.3).
    await fixture.emitInitial();
    const grid = page.getByRole("grid");
    await expect(grid).toHaveAttribute("aria-rowcount", String(ROW_COUNT));
    const mountedRows = page.locator("[data-task-id]");
    await expect(mountedRows.first()).toBeVisible();

    // Fix the changed-ID set: every mounted data row plus the final offscreen
    // task (last in addedOn-desc row order).
    const mountedIds = await mountedRows.evaluateAll((elements) =>
      elements.map((el) => el.getAttribute("data-task-id")),
    );
    const watchId = mountedIds[0];
    const lastId = fixture.ids[fixture.ids.length - 1];
    const changedIds = [...new Set([...mountedIds, lastId])];
    const measuredTicks = Array.from(
      { length: MEASURED_TICKS },
      (_, i) => i + 2,
    );

    // Warm-up: one unmeasured delivery proves the seed rendered and settles
    // JIT before the CDP baseline.
    await fixture.emitDelta(1, changedIds);
    await expect
      .poll(() => readProgressNow(page, watchId))
      .toBeCloseTo(fixture.progressAt(1) * 100, 4);

    const cdp = await context.newCDPSession(page);
    await cdp.send("Performance.enable");
    const tracingDone = new Promise((resolve) =>
      cdp.once("Tracing.tracingComplete", resolve),
    );
    await cdp.send("Tracing.start", {
      categories: "-*,devtools.timeline,v8.execute",
      transferMode: "ReturnAsStream",
    });

    const samples = [await scriptDuration(cdp)];
    for (const t of measuredTicks) {
      await fixture.emitDelta(t, changedIds);
      // Exactly one verified update per interval: a missed, coalesced or late
      // render leaves last tick's value in the DOM and fails the poll.
      await expect
        .poll(() => readProgressNow(page, watchId), { timeout: 10_000 })
        .toBeCloseTo(fixture.progressAt(t) * 100, 4);
      expect(await mountedRows.count()).toBeLessThan(MAX_MOUNTED_ROWS);
      samples.push(await scriptDuration(cdp));
      await page.waitForTimeout(TICK_MS);
    }

    await cdp.send("Tracing.end");
    const { stream } = await tracingDone;
    fs.mkdirSync(STATE_DIR, { recursive: true });
    let trace = "";
    for (;;) {
      const chunk = await cdp.send("IO.read", { handle: stream });
      if (chunk.data) {
        trace += chunk.base64Encoded
          ? Buffer.from(chunk.data, "base64").toString("utf8")
          : chunk.data;
      }
      if (chunk.eof) break;
    }
    await cdp.send("IO.close", { handle: stream });
    fs.writeFileSync(path.join(STATE_DIR, "cdp-trace.json"), trace);

    const deltas = samples
      .slice(1)
      .map((value, i) => (value - samples[i]) * 1000);
    const sorted = [...deltas].sort((a, b) => a - b);
    // Nearest-rank p95 over ten samples is the maximum: the budget tolerates no
    // outlier tick at all.
    const p95 = sorted[Math.ceil(0.95 * sorted.length) - 1];
    console.log(
      `grid perf: ${changedIds.length} changed rows per tick, ${deltas.length} measured ticks`,
    );
    console.log(
      `grid perf deltas (ms): ${deltas.map((d) => d.toFixed(3)).join(", ")}`,
    );
    console.log(`grid perf p95: ${p95.toFixed(3)} ms (budget ${BUDGET_MS} ms)`);
    expect(p95).toBeLessThan(BUDGET_MS);

    // The offscreen task received the same ticks; scrolling must show its final
    // value, not a stale seed.
    await grid.evaluate((el) => {
      el.scrollTop = el.scrollHeight;
    });
    const lastRow = page.locator(`[data-task-id="${lastId}"]`);
    await expect(lastRow).toBeVisible();
    await expect
      .poll(() => readProgressNow(page, lastId))
      .toBeCloseTo(fixture.progressAt(MEASURED_TICKS + 1) * 100, 4);
  });
});
