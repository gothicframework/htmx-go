import { defineConfig, devices } from "@playwright/test";

// Playwright config for the htmx-go WASM validation harness.
//
// The webServer builds the wasm (via run.sh → build.sh) and starts the Go host
// server. Readiness is signalled by index.html via body[data-wasm-ready="1"] and
// the window "wasm-ready" event — tests MUST wait on one of those, NEVER on
// networkidle (the `every 1s` polling trigger keeps the network perpetually busy).
const PORT = process.env.PORT || "8899";

export default defineConfig({
  testDir: ".",
  fullyParallel: false,
  reporter: [["list"]],
  use: {
    baseURL: `http://localhost:${PORT}`,
    trace: "retain-on-failure",
  },
  projects: [
    { name: "chromium", use: { ...devices["Desktop Chrome"] } },
  ],
  webServer: {
    command: "./run.sh",
    url: `http://localhost:${PORT}/`,
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
    env: { PORT },
  },
});
