import { expect, test } from "@playwright/test";
import { ensureAdmin, loginAsAdmin } from "./fixtures";

const RESULT_TITLE = "ubuntu-26.04-desktop-amd64.iso";

// One finished job with one done engine and one row; both routes are stubbed
// so the spec needs no engine daemon.
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
 * Stubs POST /search, GET/DELETE /search/{id} and POST /tasks; the indexer and
 * category lists and the prefs document stay real (fresh state seeds the
 * bundled indexers). Returns every body POST /search received.
 */
async function stubSearchAndTasks(page) {
  const searchPosts = [];
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
  await page.route("**/api/v1/tasks", (route) =>
    route.fulfill({
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
    }),
  );
  return searchPosts;
}

test("a search result adds a task in one click", async ({ page, request }) => {
  await ensureAdmin(request);
  await stubSearchAndTasks(page);
  await loginAsAdmin(page);
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

test("a saved search re-runs with the stored selection", async ({
  page,
  request,
}) => {
  await ensureAdmin(request);
  const searchPosts = await stubSearchAndTasks(page);
  await loginAsAdmin(page);
  await page.goto("/search");

  await page.getByLabel("Search query").fill("ubuntu");
  await page.getByRole("button", { name: "Search", exact: true }).click();
  const strip = page.getByLabel("Per-indexer search status");
  await expect(strip).toContainText("●");

  // Save the search, then wait for the debounced PUT so the document is on
  // the server before the reload.
  await page.getByRole("button", { name: "Save…" }).click();
  await page.getByLabel("Name").fill("weekly");
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
  await page.getByRole("button", { name: "weekly" }).click();

  await expect.poll(() => searchPosts.length).toBe(2);
  expect(searchPosts[0].indexer_ids.length).toBeGreaterThan(0);
  expect(searchPosts[1].indexer_ids).toEqual(searchPosts[0].indexer_ids);
  expect(searchPosts[1].query).toBe("ubuntu");
});
