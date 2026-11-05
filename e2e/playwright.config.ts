import { defineConfig, devices } from "@playwright/test";

// The harness (e2e/harness) serves the real web app + API + SQLite with fake
// destination accounts and a fake Blossom server; see README.md.
const PORT = Number(process.env.E2E_PORT || 4173);
const baseURL = `http://127.0.0.1:${PORT}`;

export default defineConfig({
  testDir: "./tests",
  // One shared server whose fakes record calls: run tests one at a time so a
  // test's assertions only ever see its own calls.
  workers: 1,
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  timeout: 30_000,
  expect: { timeout: 7_000 },
  reporter: process.env.CI ? [["github"], ["html", { open: "never" }]] : [["list"]],
  use: {
    baseURL,
    trace: "on-first-retry",
    screenshot: "only-on-failure",
    // The app formats dates for Europe/Paris; pin the browser so assertions
    // don't depend on the runner's clock settings.
    timezoneId: "Europe/Paris",
    locale: "en-GB",
  },
  projects: [
    { name: "desktop-chromium", use: { ...devices["Desktop Chrome"] } },
    { name: "mobile-chromium", use: { ...devices["Pixel 7"] }, grep: /@mobile/ },
  ],
  webServer: {
    command: "go run ./e2e/harness",
    cwd: "..",
    env: { PORT: String(PORT) },
    url: `${baseURL}/readyz`,
    reuseExistingServer: !process.env.CI,
    timeout: 180_000,
    stdout: "ignore",
    stderr: "pipe",
  },
});
