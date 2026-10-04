// Agents & Team nav consolidation — console rail + legacy redirects (Playwright).
//
// ISI-5432 (follow-up to the ISI-5357/5362 unified surface). Proves the user-visible legs of
// the consolidation: the rail carries ONE "Agents & Team" node at /agents-team (no separate
// Teams/Agents nodes), the node lights up on the unified surface, and the legacy /teams +
// /agents list routes redirect there — with the admin `?team=<TeamUID>` cross-squad scoping
// param preserved verbatim. Companion Vitest units pin the tree derivation itself
// (test/nav/nav.test.ts).
//
// Convention (mirrors e2e/nav/settings-nav.spec.ts): semantic locators only, user-visible
// outcome assertions, and a source-scaffolded skip-with-reason until a live console is
// reachable. Set KSQUAD_CONSOLE_E2E=1 to activate.
//
// NOTE: the redirect legs only assert against a LIVE console (they need the Next.js server's
// HTTP redirect behavior); the rail legs are likewise live-console scoped to keep one gate.

import { test, expect } from "@playwright/test";

const LIVE = process.env.KSQUAD_CONSOLE_E2E === "1";
const SKIP_REASON =
  "ISI-5432 nav consolidation: set KSQUAD_CONSOLE_E2E=1 against a live console to activate.";

test.describe("ISI-5432 · Agents & Team rail node + legacy redirects", () => {
  test.skip(!LIVE, SKIP_REASON);

  test("the rail carries ONE Agents & Team node (no legacy Teams/Agents nodes)", async ({
    page,
  }) => {
    await page.goto("/overview");

    const unified = page.getByRole("link", { name: "Agents & Team" });
    await expect(unified).toHaveAttribute("href", "/agents-team");
    // The legacy nodes are retired from the rail entirely.
    await expect(page.getByRole("navigation", { name: "Primary" }).getByRole("link", { name: "Teams", exact: true })).toHaveCount(0);
    await expect(page.getByRole("navigation", { name: "Primary" }).getByRole("link", { name: "Agents", exact: true })).toHaveCount(0);
  });

  test("the unified surface lights the rail node as the current page", async ({ page }) => {
    await page.goto("/agents-team");
    const unified = page.getByRole("link", { name: "Agents & Team" });
    await expect(unified).toHaveAttribute("data-active", "true");
  });

  test("/teams redirects to the unified surface", async ({ page }) => {
    await page.goto("/teams");
    await expect(page).toHaveURL(/\/agents-team$/);
  });

  test("/agents redirects to the unified surface, preserving the admin ?team= scope", async ({
    page,
  }) => {
    await page.goto("/agents");
    await expect(page).toHaveURL(/\/agents-team$/);
    await page.goto("/agents?team=uid-squad-alpha");
    await expect(page).toHaveURL(/\/agents-team\?team=uid-squad-alpha$/);
  });
});
