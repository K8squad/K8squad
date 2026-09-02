// e2e/live/config.ts — shared parameterization + fixtures for the LIVE Console UI E2E suite.
//
// ISI-3536 (parent ISI-3534): a runnable Playwright suite that exercises a LIVE Console and
// reports a CLEAR per-feature pass/fail status, so we can identify bugs fast instead of taking
// baby steps. It is NOT a required GitHub CI gate — there is no ephemeral env to test against
// (see e2e/live/README.md and .github/workflows/ui-e2e-live.yml, workflow_dispatch only).
//
// PARAMETERIZATION (nothing is hardcoded except the board-requested default host):
//   KSQUAD_BASE_URL   base origin of the live Console       (default http://10.0.0.219)
//   KSQUAD_LOGIN_PATH login route incl. ?next=              (default /login?next=%2F)
//   KSQUAD_USERNAME   admin username                        (default "admin")
//   KSQUAD_PASSWORD   admin password                        (NO default — never ship a secret)
//   KSQUAD_PROJECT_ID optional project id for project pages (default: auto-discover, else skip)
//
// The authenticated rows SKIP-WITH-REASON (never silently drop) when KSQUAD_PASSWORD is unset,
// so `npm run e2e:live` is safe to run without creds and the report still lists every feature row.

import {
  test as base,
  expect,
  type Page,
  type TestInfo,
} from "@playwright/test";

export const BASE_URL = process.env.KSQUAD_BASE_URL?.trim() || "http://10.0.0.219";
export const LOGIN_PATH =
  process.env.KSQUAD_LOGIN_PATH?.trim() || "/login?next=%2F";
export const USERNAME = process.env.KSQUAD_USERNAME?.trim() || "admin";
export const PASSWORD = process.env.KSQUAD_PASSWORD ?? "";
export const PROJECT_ID = process.env.KSQUAD_PROJECT_ID?.trim() || "";

/** Whether admin credentials are available to run the authenticated rows. */
export const CREDS_PRESENT = PASSWORD.length > 0;

export const NO_CREDS_REASON =
  "authenticated row skipped: set KSQUAD_PASSWORD (and optionally KSQUAD_USERNAME, default " +
  '"admin") to run against a live Console. No password default is shipped — secrets are never ' +
  "hardcoded (ISI-3536).";

/**
 * feature() tags the running test with a human-readable feature label the feature-summary
 * reporter uses for the one-glance PASS/FAIL table. Prefer the static `{ annotation }` form on
 * `test(...)` where possible; this is the runtime escape hatch for parameterized rows.
 */
export function feature(name: string): void {
  test.info().annotations.push({ type: "feature", description: name });
}

// wireCapture wires console + network diagnostics onto a page and, on failure, attaches them to
// the Playwright report (coordinated with the Observability Agent, ISI-3539: every failing row
// carries console errors, page errors, failed requests and >=400 responses alongside the trace,
// screenshot, and video the live config records).
function wireCapture(page: Page, testInfo: TestInfo): () => Promise<void> {
  const logs: string[] = [];
  page.on("console", (m) => {
    const t = m.type();
    if (t === "error" || t === "warning") logs.push(`console.${t}: ${m.text()}`);
  });
  page.on("pageerror", (e) => logs.push(`pageerror: ${e.message}`));
  page.on("requestfailed", (r) =>
    logs.push(
      `requestfailed: ${r.method()} ${r.url()} — ${r.failure()?.errorText ?? "unknown"}`,
    ),
  );
  page.on("response", (r) => {
    if (r.status() >= 400)
      logs.push(`http ${r.status()}: ${r.request().method()} ${r.url()}`);
  });
  return async () => {
    if (testInfo.status !== testInfo.expectedStatus && logs.length) {
      await testInfo.attach("console-network.log", {
        body: logs.join("\n"),
        contentType: "text/plain",
      });
    }
  };
}

/** doLogin drives the A1 login leg with semantic locators (getByLabel / getByRole). */
export async function doLogin(
  page: Page,
  opts: { username?: string; password?: string } = {},
): Promise<void> {
  const username = opts.username ?? USERNAME;
  const password = opts.password ?? PASSWORD;
  await page.goto(LOGIN_PATH);
  await page.getByLabel(/username/i).fill(username);
  await page.getByLabel(/password/i).fill(password);
  await page.getByRole("button", { name: /sign ?in|log ?in/i }).click();
}

// The extended test:
//   * `page`       — the default page, with console/network capture-on-failure wired in.
//   * `authedPage` — a fresh logged-in page (own context) for the authenticated page-coverage
//                    rows. Isolated per test so a crash on one page never poisons another.
export const test = base.extend<{ authedPage: Page }>({
  page: async ({ page }, use, testInfo) => {
    const teardown = wireCapture(page, testInfo);
    await use(page);
    await teardown();
  },
  authedPage: async ({ browser }, use, testInfo) => {
    const context = await browser.newContext({
      baseURL: BASE_URL,
      ignoreHTTPSErrors: true,
    });
    const page = await context.newPage();
    const teardown = wireCapture(page, testInfo);
    await doLogin(page);
    // Land on the app root before handing the page to the test.
    await expect(page, "login did not leave the /login screen").not.toHaveURL(
      /\/login/,
      { timeout: 15_000 },
    );
    await use(page);
    await teardown();
    await context.close();
  },
});

export { expect };
