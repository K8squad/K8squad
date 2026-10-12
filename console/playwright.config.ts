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
//
// ISI-5713 (WS-2 of ISI-5711, mobile-UX): the browser lane is now MULTI-PROJECT. A
// desktop `chromium` project runs every historical spec; two MOBILE projects —
// `mobile-pixel` (Pixel 5) and `mobile-iphone` (iPhone 13) — run ONLY the mobile
// smoke suite under e2e/mobile/**, at phone viewports (DPR, touch, mobile UA). Both
// mobile projects ride the CHROMIUM engine (Chrome-DevTools-style device emulation):
// CI installs chromium only (see .github/workflows/e2e.yml), so pinning browserName
// keeps the lane webkit-free while still exercising the <768 mobile breakpoint the
// adaptive shell (lib/nav.ts mobileNav + ConsoleShell) re-expresses. The split is by
// testMatch/testIgnore so a desktop spec never runs at a phone viewport and a mobile
// spec never runs at desktop width — each surface is asserted at exactly its breakpoint.
import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: process.env.CI ? 2 : undefined,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  use: {
    // Live runs target a reachable console via KSQUAD_CONSOLE_BASE_URL; left unset in
    // CI, where every spec skips-with-reason (KSQUAD_CONSOLE_E2E unset) so no relative
    // page.goto() is ever issued. Additive: undefined === the historical behavior.
    baseURL: process.env.KSQUAD_CONSOLE_BASE_URL,
    trace: "on-first-retry",
  },
  outputDir: "test-results/playwright",
  projects: [
    {
      // Desktop lane (the historical default) — every spec EXCEPT the mobile suite.
      name: "chromium",
      testIgnore: ["**/mobile/**"],
      use: { ...devices["Desktop Chrome"] },
    },
    {
      // Mobile — Pixel 5 preset (native chromium engine). Mobile suite only.
      name: "mobile-pixel",
      testMatch: ["**/mobile/**"],
      use: { ...devices["Pixel 5"] },
    },
    {
      // Mobile — iPhone 13 preset. The device descriptor (390×664 viewport, DPR 3,
      // touch, mobile Safari UA) with the chromium ENGINE forced, so the lane needs no
      // webkit install while still driving the mobile breakpoint + touch affordances.
      name: "mobile-iphone",
      testMatch: ["**/mobile/**"],
      use: { ...devices["iPhone 13"], browserName: "chromium" },
    },
  ],
});
