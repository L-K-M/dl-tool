import { expect, test } from "@playwright/test";
import { stubTasks } from "./fixtures";

const RESULT_TITLE = "ubuntu-26.04-desktop-amd64.iso";

// The session is stubbed, never minted: setup.spec.ts owns the first-run
// account and can be scheduled late on a parallel CI runner, so an
// ensureAdmin here could race its wizard assertion — the same constraint
// a11y.spec.ts and keyboard.spec.ts document. Every endpoint the screen
// pulls is stubbed, so the spec needs neither a real account nor an engine
// daemon.
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

const INDEXERS = [
  {
    id: "internet-archive",
    name: "Internet Archive",
    kind: "torznab",
    enabled: true,
    url: "https://archive.org",
    api_key_set: false,
    categories: null,
    definition_id: null,
    definition_source: null,
    last_error: null,
    last_test_at: null,
    legal_tier: "safe",
    priority: 0,
    provenance: null,
    seeders_unknown: false,
  },
  {
    id: "ix_b",
    name: "Indexer B",
    kind: "torznab",
    enabled: true,
    url: "https://ix.test",
    api_key_set: false,
    categories: null,
    definition_id: null,
    definition_source: null,
    last_error: null,
    last_test_at: null,
    legal_tier: "safe",
    priority: 0,
    provenance: null,
    seeders_unknown: false,
  },
];

// One finished job with one done engine and one row.
const SEARCH_JOB = {
  id: "sch_e2e",
  query: "ubuntu",
  finished: true,
  total: 1,
  engines: [
    {
      id: "internet-archive",
      name: "Internet Archive",
      status: "done",
      count: 1,
      error: null,
    },
  ],
  results: [
    {
      id: "res_e2e",
      indexer_id: "internet-archive",
      indexer_name: "Internet Archive",
      title: RESULT_TITLE,
      info_hash: null,
      size_bytes: 5_600_000_000,
      seeders: 412,
      leechers: 37,
      published_at: "2026-09-01T00:00:00Z",
      category_ids: [],
      album: null,
      artist: null,
      author: null,
      category_desc: null,
      download_volume_factor: 1,
      genre: null,
      grabs: null,
      imdb_id: null,
      language: null,
      minimum_ratio: null,
      minimum_seed_time_seconds: null,
      publisher: null,
      tmdb_id: null,
      tvdb_id: null,
      upload_volume_factor: 1,
      year: null,
    },
  ],
  next_cursor: null,
};

/**
 * Stubs the session, the preference document (held in page-side memory so a
 * PUT round-trips through a later GET), the indexer lists, POST/GET/DELETE
 * /search and POST /tasks. Returns every body POST /search received.
 */
async function stubSearchScreen(page) {
  const searchPosts = [];
  await stubTasks(page, 0);
  await page.route("**/api/v1/auth/me", (route) =>
    route.fulfill({ json: AUTHENTICATED }),
  );
  await page.route("**/api/v1/indexers/categories", (route) =>
    route.fulfill({ json: { categories: [{ id: 2000, name: "Movies" }] } }),
  );
  await page.route("**/api/v1/indexers", (route) =>
    route.fulfill({ json: { indexers: INDEXERS } }),
  );
  await page.route("**/api/v1/categories", (route) =>
    route.fulfill({ json: { categories: [] } }),
  );
  await page.route("**/api/v1/tags", (route) =>
    route.fulfill({ json: { tags: [] } }),
  );
  await page.route("**/api/v1/fs/roots", (route) =>
    route.fulfill({ json: { roots: [] } }),
  );
  let prefsDoc: unknown = { version: 1 };
  await page.route("**/api/v1/prefs", async (route) => {
    if (route.request().method() === "PUT") {
      prefsDoc = route.request().postDataJSON();
      return route.fulfill({ json: prefsDoc });
    }
    return route.fulfill({ json: prefsDoc });
  });
  await page.route("**/api/v1/search**", (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (request.method() === "POST" && path === "/api/v1/search") {
      searchPosts.push(request.postDataJSON());
      return route.fulfill({ status: 202, json: { id: "sch_e2e" } });
    }
    if (request.method() === "DELETE") return route.fulfill({ status: 204 });
    return route.fulfill({ json: SEARCH_JOB });
  });
  await page.route("**/api/v1/tasks", (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    return route.fulfill({
      status: 201,
      json: {
        created: [
          {
            id: "tsk_e2e",
            name: RESULT_TITLE,
            source_uri: "search-result:res_e2e",
          },
        ],
        rejected: [],
      },
    });
  });
  return searchPosts;
}

test("a search result adds a task in one click", async ({ page }) => {
  await stubSearchScreen(page);
  await page.goto("/search");

  await page.getByLabel("Search query").fill("ubuntu");
  await page.getByRole("button", { name: "Search", exact: true }).click();

  // The per-indexer strip reaches done, then the row's ⬇ becomes a ✓ within
  // five seconds of the click and the toast names the created task.
  const strip = page.getByLabel("Per-indexer search status");
  await expect(strip).toContainText("●");
  await page.getByRole("button", { name: `Download ${RESULT_TITLE}` }).click();
  await expect(
    page.getByRole("link", { name: `${RESULT_TITLE} added — view task` }),
  ).toBeVisible({ timeout: 5_000 });
  await expect(page.getByText(`Added ${RESULT_TITLE}`)).toBeVisible({
    timeout: 5_000,
  });
});

test("a saved search re-runs with the stored selection", async ({ page }) => {
  const searchPosts = await stubSearchScreen(page);
  await page.goto("/search");

  await page.getByLabel("Search query").fill("ubuntu");
  await page.getByRole("button", { name: "Search", exact: true }).click();
  const strip = page.getByLabel("Per-indexer search status");
  await expect(strip).toContainText("●");

  // Save the search, then wait for the debounced PUT so the document is on
  // the server before the reload. The shell's name filter shares the label
  // text, so the dialog field is matched exactly.
  await page.getByRole("button", { name: "Save…" }).click();
  await page.getByRole("textbox", { name: "Name", exact: true }).fill("weekly");
  const putDone = page.waitForResponse(
    (response) =>
      response.url().includes("/api/v1/prefs") &&
      response.request().method() === "PUT",
  );
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await putDone;

  // A reload restores the document through GET /prefs; re-running posts the
  // same indexer_ids the live selection sent.
  await page.reload();
  await page.getByRole("button", { name: /^Saved/ }).click();
  await page.getByRole("button", { name: "weekly", exact: true }).click();

  await expect.poll(() => searchPosts.length).toBe(2);
  expect(searchPosts[0].indexer_ids.length).toBeGreaterThan(0);
  expect(searchPosts[1].indexer_ids).toEqual(searchPosts[0].indexer_ids);
  expect(searchPosts[1].query).toBe("ubuntu");
});
