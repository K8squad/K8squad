// Playwright E2E configuration — scopes the browser lane to console/e2e only.
//
// The console package ALSO carries Vitest unit/component suites under console/test
// (added with the discussion feature). Playwright's default testMatch
// (**/*.@(spec|test).?(c|m)[jt]s?(x)) sweeps those .test.ts(x) files in as if they
// were Playwright tests, and loading them fails hard: Vitest's CJS entry refuses
// require() ("Vitest cannot be imported in a CommonJS module using require()"),
// which red-walled the nightly console E2E lane (ISI-2852).
//
// Browser E2E lives in console/e2e/**/*.spec.ts; Vitest owns console/test — this
// config pins that boundary so the two runners can never collide again.
import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: process.env.CI ? 2 : undefined,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  use: {
    trace: "on-first-retry",
  },
  outputDir: "test-results/playwright",
});
