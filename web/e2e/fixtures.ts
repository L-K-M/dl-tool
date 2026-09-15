import fs from "node:fs";
import path from "node:path";
import os from "node:os";
import { expect } from "@playwright/test";

/** Wiped and recreated by the e2e:server script on every run. */
export const STATE_DIR = path.join(os.tmpdir(), "dl-tool-e2e");
export const BASE_URL = "http://127.0.0.1:8099";
export const ADMIN = {
  username: "e2e-admin",
  password: "correct horse battery staple",
};

const SETUP_TOKEN_PATH = path.join(STATE_DIR, "setup-token");
const SETUP_TOKEN_POLL_MS = 10_000;
const SETUP_TOKEN_POLL_STEP_MS = 100;
const httpCreated = 201;
const httpConflict = 409;
const httpUnauthorized = 401;

/**
 * Reads <STATE_DIR>/setup-token, polling for up to 10 s while the server boots.
 * @returns {Promise<string>}
 */
export async function readSetupToken() {
  const deadline = Date.now() + SETUP_TOKEN_POLL_MS;
  for (;;) {
    try {
      return fs.readFileSync(SETUP_TOKEN_PATH, "utf8").trim();
    } catch (error) {
      if (Date.now() >= deadline) throw error;
      await new Promise((resolve) =>
        setTimeout(resolve, SETUP_TOKEN_POLL_STEP_MS),
      );
    }
  }
}

/**
 * POSTs /api/v1/auth/setup once; a 409 means a previous spec already created the
 * admin. The endpoint is callable without CSRF while no user exists and is the
 * only way to mint the throwaway account the browser specs sign in with.
 * @param {import("@playwright/test").APIRequestContext} request
 * @returns {Promise<void>}
 */
export async function ensureAdmin(request) {
  const attempt = (setupToken) =>
    request.post(`${BASE_URL}/api/v1/auth/setup`, {
      data: { setup_token: setupToken, ...ADMIN, locale: "en" },
    });
  // The token file is deleted on the first successful setup; when it is gone a
  // non-empty dummy token still yields 409 (an empty one fails validation
  // first), which is the success path for repeat calls.
  let token = "already-set-up";
  if (fs.existsSync(SETUP_TOKEN_PATH)) token = await readSetupToken();
  let response = await attempt(token);
  if (response.status() === httpUnauthorized) {
    // Boot race: users is empty but the token file has not landed yet.
    response = await attempt(await readSetupToken());
  }
  if (response.status() === httpConflict) return;
  if (response.status() !== httpCreated) {
    throw new Error(
      `auth/setup failed: ${response.status()} ${await response.text()}`,
    );
  }
}

/**
 * Signs in through the /login form and waits for the grid container.
 * @param {import("@playwright/test").Page} page
 * @returns {Promise<void>}
 */
export async function loginAsAdmin(page) {
  await page.goto("/login");
  await page.getByLabel("Username").fill(ADMIN.username);
  await page.getByLabel("Password", { exact: true }).fill(ADMIN.password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("grid")).toBeVisible();
}

/**
 * Delivers one named "sync" MessageEvent through the EventSource double that
 * stubTasks installed, waiting until the transport has opened a stream. The
 * double is test code only; the shipped transport, applySync and subscriptions
 * are untouched (doc 13 section 6.3).
 * @param {import("@playwright/test").Page} page
 * @param {object} payload sync.Delta JSON body
 * @returns {Promise<void>}
 */
export async function emitSync(page, payload) {
  await page.waitForFunction(
    () => window.__dltoolE2E && window.__dltoolE2E.openSources().length > 0,
  );
  await page.evaluate((msg) => window.__dltoolE2E.emitSync(msg), payload);
}

/**
 * Reads the mounted aria-valuenow of one row's progressbar; null when the row
 * is not mounted (virtualised away) or has no progressbar yet.
 * @param {import("@playwright/test").Page} page
 * @param {string} taskId
 * @returns {Promise<number | null>}
 */
export async function readProgressNow(page, taskId) {
  return page.evaluate((id) => {
    const el = document.querySelector(
      `[data-task-id="${id}"] [role="progressbar"]`,
    );
    return el === null ? null : Number(el.getAttribute("aria-valuenow"));
  }, taskId);
}

/** Current ScriptDuration total (seconds) from the CDP Performance domain. */
export async function scriptDuration(cdp) {
  const { metrics } = await cdp.send("Performance.getMetrics");
  const metric = metrics.find((m) => m.name === "ScriptDuration");
  return metric ? metric.value : 0;
}

const BASE_TIME_MS = Date.parse("2026-09-01T00:00:00Z");
const SNAPSHOT_RID = 1000;
const SYNC_RID_BASE = 2000;
const DELTA_RID_BASE = 3000;
const PAGE_SIZE_LIMIT = 500;

/**
 * Seeds list/sync responses and installs the EventSource fixture before
 * navigation (doc 13 section 6.3). /tasks returns the same n tasks the initial
 * snapshot carries; /sync always answers the snapshot at the last delivered
 * tick so a recovery refetch cannot erase the seed.
 *
 * The returned handle owns tick delivery and measurement helpers; there are no
 * production hooks.
 * @param {import("@playwright/test").Page} page
 * @param {number} n task count
 */
export async function stubTasks(page, n) {
  const ids = Array.from(
    { length: n },
    (_, i) => `tsk_${String(i).padStart(6, "0")}`,
  );
  // appliedTick tracks the newest delivered tick so the /sync stub stays
  // current; it moves only after a delivery succeeds.
  let appliedTick = 0;

  const progressAt = (t) => 0.3 + 0.04 * t;
  const rateAt = (t) => 2_000_000 + 50_000 * t;
  const stats = () => ({
    speed_down: rateAt(appliedTick),
    speed_up: 0,
    active: n,
    queued: 0,
  });
  const makeTask = (i, t) => {
    const progress = progressAt(t);
    return {
      id: ids[i],
      engine: "aria2",
      source_kind: "http",
      source_uri: `https://example.invalid/e2e/${i}`,
      name: `e2e-task-${i}`,
      state: "downloading",
      error_code: null,
      error_message: null,
      destination: "/data",
      requested_destination: null,
      content_path: null,
      category: null,
      tags: [],
      infohash_v1: null,
      infohash_v2: null,
      total_bytes: 1_000_000_000,
      completed_bytes: Math.round(progress * 1_000_000_000),
      uploaded_bytes: 0,
      progress,
      download_rate: rateAt(t),
      upload_rate: 0,
      eta_seconds: null,
      ratio: 0,
      total_peers: 0,
      connected_seeders: 0,
      connected_leechers: 0,
      dl_limit: 0,
      ul_limit: 0,
      ratio_limit: null,
      seeding_time_limit: null,
      sequential: false,
      queue_position: null,
      unzip_progress: null,
      file_count: null,
      // added_at descends with i so the default addedOn-desc sort keeps row
      // order equal to fixture order: ids[n-1] is the final offscreen task.
      added_at: new Date(BASE_TIME_MS - i * 1000).toISOString(),
      started_at: new Date(BASE_TIME_MS - i * 1000).toISOString(),
      completed_at: null,
      updated_at: new Date(BASE_TIME_MS + t * 1000).toISOString(),
    };
  };
  const snapshotPayload = (t, rid) => ({
    rid,
    full_update: true,
    seq_gap: false,
    tasks: Object.fromEntries(ids.map((id, i) => [id, makeTask(i, t)])),
    tasks_removed: [],
    stats: stats(),
  });
  const deltaPayload = (t, changedIds) => ({
    rid: DELTA_RID_BASE + t,
    full_update: false,
    seq_gap: false,
    tasks: Object.fromEntries(
      changedIds.map((id) => [
        id,
        {
          progress: progressAt(t),
          download_rate: rateAt(t),
          updated_at: new Date(BASE_TIME_MS + t * 1000).toISOString(),
        },
      ]),
    ),
    tasks_removed: [],
    stats: stats(),
  });

  await page.route("**/api/v1/tasks*", (route) => {
    const url = new URL(route.request().url());
    const offset = Number(url.searchParams.get("cursor") ?? 0) || 0;
    const limit =
      Number(url.searchParams.get("limit") ?? PAGE_SIZE_LIMIT) ||
      PAGE_SIZE_LIMIT;
    const items = ids
      .slice(offset, offset + limit)
      .map((id, k) => makeTask(offset + k, appliedTick));
    const nextCursor = offset + limit < n ? String(offset + limit) : null;
    return route.fulfill({
      json: { items, total: n, next_cursor: nextCursor },
    });
  });
  // Recovery returns the current snapshot with a rid above every pending delta
  // but below the not-yet-delivered ones, so a mid-run refetch stays consistent.
  await page.route("**/api/v1/sync*", (route) =>
    route.fulfill({
      json: snapshotPayload(appliedTick, SYNC_RID_BASE + appliedTick),
    }),
  );

  // The EventSource double mirrors the unit-test FakeEventSource contract:
  // open/readyState, listener registration and close. The test drives named
  // "sync" MessageEvents through window.__dltoolE2E.emitSync.
  await page.addInitScript(() => {
    class E2EEventSource {
      static instances = [];

      constructor(url, init) {
        this.url = String(url);
        this.withCredentials = Boolean(init && init.withCredentials);
        this.closed = false;
        this.readyState = 0;
        this.listeners = new Map();
        E2EEventSource.instances.push(this);
        setTimeout(() => {
          if (this.closed) return;
          this.readyState = 1;
          this.dispatch("open", new Event("open"));
        }, 0);
      }

      addEventListener(type, listener) {
        const set = this.listeners.get(type) ?? new Set();
        set.add(listener);
        this.listeners.set(type, set);
      }

      removeEventListener(type, listener) {
        const set = this.listeners.get(type);
        if (set) set.delete(listener);
      }

      close() {
        this.closed = true;
        this.readyState = 2;
      }

      dispatch(type, event) {
        for (const listener of this.listeners.get(type) ?? []) listener(event);
      }

      emit(type, payload) {
        const event =
          payload === undefined
            ? new Event(type)
            : new MessageEvent(type, {
                data: JSON.stringify(payload),
                lastEventId: String(payload.rid ?? ""),
              });
        this.dispatch(type, event);
      }
    }
    Object.defineProperty(window, "EventSource", {
      value: E2EEventSource,
      writable: true,
      configurable: true,
    });
    window.__dltoolE2E = {
      openSources: () => E2EEventSource.instances.filter((s) => !s.closed),
      emitSync(payload) {
        const open = this.openSources();
        if (open.length === 0) throw new Error("no open EventSource");
        open[open.length - 1].emit("sync", payload);
      },
    };
  });

  return {
    ids,
    progressAt,
    rateAt,
    /** Delivers the initial full snapshot (rid below every delta). */
    async emitInitial() {
      await emitSync(page, snapshotPayload(0, SNAPSHOT_RID));
      appliedTick = 0;
    },
    /** Delivers one incremental update over changedIds and advances /sync. */
    async emitDelta(t, changedIds) {
      await emitSync(page, deltaPayload(t, changedIds));
      appliedTick = t;
    },
  };
}
