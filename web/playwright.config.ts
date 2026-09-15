import path from "node:path";
import { defineConfig, devices } from "@playwright/test";
import { BASE_URL, STATE_DIR } from "./e2e/fixtures";

export default defineConfig({
  testDir: "./e2e",
  // Retry traces and .last-run.json land under the throwaway state directory,
  // not the repository — keeping `npx prettier --check .` and the tree clean.
  outputDir: path.join(STATE_DIR, "test-results"),
  fullyParallel: false,
  reporter: [["list"]],
  use: { baseURL: BASE_URL, trace: "on-first-retry" },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    command: "npm run e2e:server",
    url: `${BASE_URL}/healthz`,
    timeout: 180_000,
    reuseExistingServer: false,
    env: {
      DLTOOL_HTTP_ADDR: "127.0.0.1:8099",
      DLTOOL_CONFIG_DIR: STATE_DIR,
      DLTOOL_DB_PATH: path.join(STATE_DIR, "dl-tool.db"),
      DLTOOL_DATA_ROOTS: path.join(STATE_DIR, "data"),
      DLTOOL_LOG_FORMAT: "text",
    },
  },
});
