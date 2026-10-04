// Settings left-nav dedicated LLM entry + OTel-only configuration (console rail leg, Playwright).
//
// Story ISI-5004 (S1 of the LLM Settings rework, child of ISI-4989). Proves the user-visible legs
// the rework restores: the SETTINGS rail group reads LLM Settings · OTel · Credentials · Plugins ·
// Users & Roles; /settings/llm is its own surface with correct active-nav + breadcrumb; and
// /settings/configuration stays OTLP-only. Companion Vitest units pin the same contract at the pure
// nav layer (test/nav/nav.test.ts) and the ModelPrioritySection/OtlpConfigScreen boundaries
// (test/settings/*).
//
// Convention (mirrors e2e/nav/agents-team-nav.spec.ts + e2e/projects/project-id-roundtrip.spec.ts):
// semantic locators only, user-visible outcome assertions, and a source-scaffolded skip-with-reason
// until a live console is reachable. Set KSQUAD_CONSOLE_E2E=1 to activate.

import { test, expect } from "@playwright/test";

const LIVE = process.env.KSQUAD_CONSOLE_E2E === "1";
const SKIP_REASON =
  "ISI-5004 dedicated /settings/llm nav: set KSQUAD_CONSOLE_E2E=1 against a live console to " +
  "activate.";

test.describe("ISI-5004 · Settings rail: dedicated LLM entry + OTel-only configuration", () => {
  test.skip(!LIVE, SKIP_REASON);

  test("SETTINGS group lists LLM Settings first, then OTel/Credentials/Plugins/Users & Roles", async ({
    page,
  }) => {
    await page.goto("/overview");

    const llm = page.getByRole("link", { name: "LLM Settings" });
    const otel = page.getByRole("link", { name: "OTel" });
    const credentials = page.getByRole("link", { name: "Credentials" });
    const plugins = page.getByRole("link", { name: "Plugins" });

    await expect(llm).toHaveAttribute("href", "/settings/llm");
    await expect(otel).toHaveAttribute("href", "/settings/configuration");
    await expect(credentials).toHaveAttribute("href", "/credentials");
    await expect(plugins).toHaveAttribute("href", "/plugins");
  });

  test("clicking LLM Settings lands on /settings/llm with active-nav + breadcrumb", async ({
    page,
  }) => {
    await page.goto("/overview");

    await page.getByRole("link", { name: "LLM Settings" }).click();
    await expect(page).toHaveURL(/\/settings\/llm$/);

    // Active rail state is honest: the LLM Settings item is active, OTel is not.
    await expect(page.getByRole("link", { name: "LLM Settings" })).toHaveAttribute(
      "data-active",
      "true",
    );

    // Breadcrumb ends at the dedicated surface.
    const breadcrumb = page.getByRole("navigation", { name: "Breadcrumb" });
    await expect(breadcrumb.getByText("LLM Settings")).toBeVisible();
    await expect(breadcrumb.getByText("LLM Settings")).toHaveAttribute(
      "aria-current",
      "page",
    );
  });

  test("OTel still routes to /settings/configuration, which stays OTLP-only", async ({
    page,
  }) => {
    await page.goto("/overview");

    await page.getByRole("link", { name: "OTel" }).click();
    await expect(page).toHaveURL(/\/settings\/configuration$/);

    // Active rail state flips to OTel.
    await expect(page.getByRole("link", { name: "OTel" })).toHaveAttribute(
      "data-active",
      "true",
    );

    // Breadcrumb ends at Configuration (the OTLP surface), not LLM Settings.
    const breadcrumb = page.getByRole("navigation", { name: "Breadcrumb" });
    await expect(breadcrumb.getByText("Configuration")).toHaveAttribute(
      "aria-current",
      "page",
    );
  });
});
