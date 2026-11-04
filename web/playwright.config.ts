import { defineConfig, devices } from "@playwright/test";

import { AUTH_FILE, BOB_AUTH_FILE } from "./e2e/auth-file";

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: [["list"], ["html", { open: "never" }]],
  use: {
    baseURL: process.env.E2E_BASE_URL ?? "http://localhost:18080",
    trace: "retain-on-failure",
  },
  projects: [
    { name: "setup", testMatch: /auth\.setup\.ts/, use: { ...devices["Desktop Chrome"] } },
    // Switches the global fixture set: done before the parallel specs start.
    {
      name: "seed",
      testMatch: /seed\.setup\.ts/,
      dependencies: ["setup"],
      use: { ...devices["Desktop Chrome"], storageState: BOB_AUTH_FILE },
    },
    {
      name: "chromium",
      dependencies: ["seed"],
      use: { ...devices["Desktop Chrome"], storageState: AUTH_FILE },
    },
  ],
});
