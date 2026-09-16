import path from "node:path";
import { defineConfig, devices } from "@playwright/test";
import { BASE_URL, STATE_DIR } from "./e2e/fixtures";

export default defineConfig({
  testDir: "./e2e",
  // Failure traces and .last-run.json land under the throwaway state
  // directory, not the repository — keeping `npx prettier --check .` and the
  // tree clean. retain-on-failure (not on-first-retry): retries stay at the
  // default 0 so a perf-budget regression cannot pass on a retry.
  outputDir: path.join(STATE_DIR, "test-results"),
  fullyParallel: false,
  reporter: [["list"]],
  use: { baseURL: BASE_URL, trace: "retain-on-failure" },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    // The wipe is anchored to the throwaway STATE_DIR constant here so the
    // npm script stays safe to run by hand against any config directory.
    command: `rm -rf "${STATE_DIR}" && npm run e2e:server`,
    url: `${BASE_URL}/healthz`,
    timeout: 180_000,
    reuseExistingServer: false,
    env: {
      DLTOOL_HTTP_ADDR: new URL(BASE_URL).host,
      DLTOOL_CONFIG_DIR: STATE_DIR,
      DLTOOL_DB_PATH: path.join(STATE_DIR, "dl-tool.db"),
      DLTOOL_DATA_ROOTS: path.join(STATE_DIR, "data"),
      DLTOOL_LOG_FORMAT: "text",
    },
  },
});
