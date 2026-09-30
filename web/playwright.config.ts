import { defineConfig, devices } from "@playwright/test";

// The spec builds and starts the real `cmd/web --web` itself (see tests/e2e.spec.ts),
// so no webServer entry is used here. Only the locally installed Chromium runs.
export default defineConfig({
  testDir: "tests",
  timeout: 180_000,
  fullyParallel: false,
  workers: 1,
  reporter: [["list"]],
  use: { ...devices["Desktop Chrome"], trace: "off", screenshot: "off", video: "off" },
});
