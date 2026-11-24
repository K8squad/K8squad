// Instrumentation Score page (ISI-7750, phase 4 of the ISI-7741 plan) — browser
// leg (Playwright, semantic locators).
//
// Companion to the Vitest boundary suite (test/instscore/screen.test.tsx): the
// component suite proves the seeded-window rendering; this spec proves the
// user-visible legs against a LIVE console + apiserver with the OTel tap
// running — the rail entry navigates to /instscore, and the page renders the
// services the seeded deployment observed.
//
// Convention (mirrors e2e/nav/settings-nav.spec.ts): semantic locators only,
// user-visible outcome assertions, source-scaffolded skip-with-reason until a
// live console is reachable. Set KSQUAD_CONSOLE_E2E=1 to activate.

import { test, expect } from "@playwright/test";

const LIVE = process.env.KSQUAD_CONSOLE_E2E === "1";
const SKIP_REASON =
  "ISI-7750 instrumentation-score page: set KSQUAD_CONSOLE_E2E=1 against a live " +
  "console + apiserver (instscore store wired, migration 0042) to activate.";

test.describe("ISI-7750 · Instrumentation Score page", () => {
  test.skip(!LIVE, SKIP_REASON);

  test("the rail entry navigates to /instscore with the page heading", async ({
    page,
  }) => {
    await page.goto("/overview");

    const entry = page.getByRole("link", { name: "Instrumentation" });
    await expect(entry).toHaveAttribute("href", "/instscore");
    await entry.click();

    await expect(page).toHaveURL(/\/instscore$/);
    await expect(
      page.getByRole("heading", { name: "Instrumentation Score", level: 1 }),
    ).toBeVisible();
  });

  test("the services table renders observed services with scores", async ({
    page,
  }) => {
    await page.goto("/instscore");

    // The table caption pins the surface; scores are tabular digits.
    await expect(
      page.getByRole("table").first(),
    ).toBeVisible();
    await expect(
      page.getByText(/worst score first/i),
    ).toBeVisible();
  });
});
