import { expect, test } from "@playwright/test";
import { BASE_URL } from "./fixtures";

const httpOk = 200;
const httpUnauthorized = 401;

// This spec never creates the admin account: the files run concurrently on
// separate workers, so an ensureAdmin here could race setup.spec.ts's
// first-run redirect assertion. Every page of the SPA serves the same
// index.html, so the manifest link, theme meta and service-worker
// registration are all reachable while signed out.
async function loadApp(page: import("@playwright/test").Page) {
  await page.goto("/");
}

async function fetchManifest(page: import("@playwright/test").Page) {
  const result = await page.evaluate(async () => {
    const link = document.querySelector('link[rel="manifest"]');
    if (!link) return { href: null, status: 0, body: "" };
    const href = new URL(link.getAttribute("href") ?? "", document.baseURI)
      .href;
    const res = await fetch(href);
    return { href, status: res.status, body: await res.text() };
  });
  expect(result.href).not.toBeNull();
  expect(result.status).toBe(httpOk);
  return { href: result.href as string, json: JSON.parse(result.body) };
}

test("manifest is installable", async ({ page }) => {
  // The install copy must match the dark --bg; emulate dark so the applied
  // theme resolves there regardless of the host's preferred scheme.
  await page.emulateMedia({ colorScheme: "dark" });
  await loadApp(page);
  const { json } = await fetchManifest(page);

  const darkBg = await page.evaluate(() =>
    getComputedStyle(document.documentElement).getPropertyValue("--bg").trim(),
  );
  expect(darkBg).toBe("#0f1115");
  expect(json.name).toBe("dl-tool");
  expect(json.display).toBe("standalone");
  expect(json.theme_color).toBe(darkBg);
  expect(json.background_color).toBe(darkBg);
  expect(json.description).toContain("nothing works offline");

  const metaTheme = await page
    .locator('meta[name="theme-color"]')
    .getAttribute("content");
  expect(metaTheme).toBe(darkBg);

  // Every URL stays relative so the app installs under a sub-path.
  expect(json.start_url).toBe("./");
  expect(json.scope).toBe("./");
  for (const icon of json.icons) {
    expect(icon.src.startsWith("/")).toBe(false);
  }
});

test("icons are maskable", async ({ page }) => {
  await loadApp(page);
  const { href, json } = await fetchManifest(page);
  const declared = new Map(
    json.icons.map((icon: { src: string }) => [
      icon.src,
      icon.sizes.split("x").map(Number),
    ]),
  );
  expect([...declared.keys()].sort()).toEqual([
    "icons/icon-192.png",
    "icons/icon-512.png",
  ]);

  for (const icon of json.icons) {
    expect(icon.purpose).toContain("maskable");
    expect(icon.type).toBe("image/png");
    const url = new URL(icon.src, href).href;
    const probe = await page.evaluate(async (iconUrl) => {
      const res = await fetch(iconUrl);
      const blob = await res.blob();
      const bitmap = await createImageBitmap(blob);
      const dims = { w: bitmap.width, h: bitmap.height };
      bitmap.close();
      return {
        status: res.status,
        type: res.headers.get("content-type"),
        dims,
      };
    }, url);
    expect(probe.status).toBe(httpOk);
    expect(probe.type).toContain("image/png");
    expect([probe.dims.w, probe.dims.h]).toEqual(declared.get(icon.src));
  }
});

test("service worker registers", async ({ page }) => {
  await loadApp(page);
  await page.waitForFunction(() =>
    navigator.serviceWorker.getRegistration().then((reg) => reg !== null),
  );
  const info = await page.evaluate(async () => {
    const reg = await navigator.serviceWorker.ready;
    return {
      scope: reg.scope,
      script: reg.active?.scriptURL ?? null,
      state: reg.active?.state ?? null,
    };
  });
  expect(info.script).toBe(`${BASE_URL}/sw.js`);
  expect(info.scope).toBe(`${BASE_URL}/`);
  expect(info.state).toBe("activated");
});

test("api requests bypass the cache", async ({ page }) => {
  await loadApp(page);
  // clients.claim() makes the registered worker control this page, so the
  // next fetch really passes through its fetch handler.
  await page.waitForFunction(() => navigator.serviceWorker.controller !== null);

  const result = await page.evaluate(async () => {
    const api = await fetch(new URL("api/v1/tasks", document.baseURI));
    // An API path that merely contains an assets-looking segment must not be
    // intercepted either.
    await fetch(new URL("api/v1/assets/probe", document.baseURI));
    const asset =
      document.querySelector<HTMLScriptElement>('script[type="module"]')?.src ??
      null;
    if (asset) await fetch(asset);
    return { apiStatus: api.status, asset };
  });

  // Signed out, the real server answers 401 — a response only the network
  // path can produce.
  expect(result.apiStatus).toBe(httpUnauthorized);
  expect(result.asset).not.toBeNull();

  // The worker's cache.put is fire-and-forget inside respondWith, so poll
  // for the asset entry instead of racing it. The 401 API response above
  // must never appear. The cache name carries the registration scope the
  // same way sw.js builds it: dl-tool@<scope path>:assets-v1.
  const readKeys = () =>
    page.evaluate(async () => {
      const scope = (await navigator.serviceWorker.ready).scope;
      const cache = await caches.open(
        `dl-tool@${new URL(scope).pathname}:assets-v1`,
      );
      return (await cache.keys()).map((r) => r.url);
    });
  await expect.poll(readKeys).toContain(result.asset);
  expect((await readKeys()).filter((url) => url.includes("/api/"))).toEqual([]);
});
