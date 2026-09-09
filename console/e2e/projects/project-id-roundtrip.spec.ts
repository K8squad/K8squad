// ISI-3982 · Project id ("namespace/name") round-trips through the BFF (Playwright).
//
// Live evidence (k8squad-test, 2026-09-08): clicking a Project row landed on an error
// Tickets page because the "ns/name" id was re-encoded at every hop (row → page redirect
// → browser client → BFF proxy → apiserver) until the mux 404'd on a triple-mangled id.
// This spec proves the user-visible journey the fix restores: a Project whose id carries a
// slash opens to Tickets (list or honest empty state), and Runs loads — without a 404 and
// without a double-encoded "%252F" ever leaving the browser for the BFF.
//
// Companion deterministic proof: test/projects/projectId.test.ts pins the single-encoding
// invariant at the normalizers and every BFF route. This spec is the browser leg.
//
// Convention (mirrors e2e/auth/a5-password-reset.spec.ts): semantic locators, user-visible
// assertions, and source-scaffolded skip-with-reason until a live console + a Project whose
// id is namespace-qualified ("ns/name") is reachable. Never silently dropped — the case
// stays visible in the Playwright report.

import { test, expect } from "@playwright/test";

const LIVE = process.env.KSQUAD_CONSOLE_E2E === "1";
const SKIP_REASON =
  "ISI-3982 ns/name id round-trip: set KSQUAD_CONSOLE_E2E=1 against a live console with a " +
  "namespace-qualified Project (e.g. bmad-squad/bmad-demo-project) to activate.";

// A namespace-qualified Project id — the shape that produced the double-encode 404.
const PROJECT_ID =
  process.env.KSQUAD_E2E_PROJECT_ID ?? "bmad-squad/bmad-demo-project";
const encoded = encodeURIComponent(PROJECT_ID); // exactly one layer: "ns%2Fname"

test.describe("ISI-3982 · project Tickets/Runs id round-trip", () => {
  test.skip(!LIVE, SKIP_REASON);

  test("Projects → project row lands on Tickets (list or honest empty state), no 404", async ({
    page,
  }) => {
    // Fail the test if any BFF work-items/rooms/stream call goes out double-encoded — the
    // exact regression this story fixes.
    page.on("request", (req) => {
      const url = req.url();
      if (url.includes("/api/projects/")) {
        expect(url, "BFF id must be single-encoded (no %252F)").not.toContain(
          "%252F",
        );
      }
    });

    await page.goto("/projects");
    await page.getByTestId("projects-row-link").first().click();

    // The root redirect (page.tsx) resolves to the Tickets surface, single-encoded.
    await expect(page).toHaveURL(new RegExp(`/projects/${encoded}/tickets`));

    // Tickets renders — either work items or an honest empty state — NOT an error page.
    await expect(page.getByText(/error|failed to load|404/i)).toHaveCount(0);
    await expect(
      page.getByRole("heading", { name: /tickets|issues/i }).first(),
    ).toBeVisible();
  });

  test("Runs tab loads for the same slash-bearing project id", async ({
    page,
  }) => {
    await page.goto(`/projects/${encoded}/runs`);
    await expect(page).toHaveURL(new RegExp(`/projects/${encoded}/runs`));
    await expect(
      page.getByRole("heading", { name: /runs/i }).first(),
    ).toBeVisible();
    await expect(page.getByText(/error|failed to load|404/i)).toHaveCount(0);
  });
});
