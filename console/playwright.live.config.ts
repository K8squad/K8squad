// playwright.live.config.ts — the LIVE Console UI E2E lane (ISI-3536 / parent ISI-3534).
//
// SEPARATE from playwright.config.ts on purpose: that config scopes the nightly browser lane to
// console/e2e/**/*.spec.ts with NO baseURL (it drives a locally-served console). This config drives
// a REMOTE, already-running Console (default http://10.0.0.219) parameterized entirely by env, and
// scopes itself to console/e2e/live only — so the two lanes can never collide.
//
// It is NOT wired as a required GitHub CI gate (there is no ephemeral env to test against). Run it
// with `npm run e2e:live` (see e2e/live/README.md) or the optional workflow_dispatch job.
//
// Observability (coordinated with the Observability Agent, ISI-3539): trace, screenshot, and video
// are captured for every failing row, and console/network diagnostics are attached on failure by
// the capture fixture in e2e/live/config.ts.

import { defineConfig } from "@playwright/test";

const BASE_URL = process.env.KSQUAD_BASE_URL?.trim() || "http://10.0.0.219";

export default defineConfig({
  testDir: "./e2e/live",
  // Diagnostic clarity over raw speed: run rows in a deterministic top-to-bottom order so the
  // scoreboard reads like the matrix (gateway → login → pages).
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  // A flaky live env shouldn't mask a real red; but one retry smooths transient network blips.
  retries: process.env.KSQUAD_E2E_RETRIES
    ? Number(process.env.KSQUAD_E2E_RETRIES)
    : 1,
  timeout: 45_000,
  expect: { timeout: 15_000 },
  reporter: [
    ["list"],
    ["./e2e/live/feature-summary-reporter.ts"],
    ["html", { outputFolder: "playwright-report/live", open: "never" }],
    ["json", { outputFile: "test-results/live/results.json" }],
  ],
  outputDir: "test-results/live",
  use: {
    baseURL: BASE_URL,
    ignoreHTTPSErrors: true,
    trace: "on",
    screenshot: "only-on-failure",
    video: "retain-on-failure",
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
  },
});
