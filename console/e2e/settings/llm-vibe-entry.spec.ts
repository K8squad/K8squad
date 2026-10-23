// e2e/settings/llm-vibe-entry.spec.ts — the vibe entry on the LLM settings surface (P4.1 UI half,
// ISI-6158 C2 / ISI-6202). One settings-path case: an admin opening Settings → LLM Settings can
// reach the Mistral (vibe) backend from the BYO picker — masked API key + the optional BYO
// base-URL field. Companion Vitest units pin the same contract at the pure layer
// (test/compose/modelHints.test.ts, test/compose/ProviderModelPicker.test.tsx,
// test/settings/credentialEntries.test.ts).
//
// Convention (mirrors e2e/nav/settings-nav.spec.ts): semantic locators only, user-visible outcome
// assertions, and a source-scaffolded skip-with-reason until a live console is reachable. Set
// KSQUAD_CONSOLE_E2E=1 to activate.

import { test, expect } from "@playwright/test";

const LIVE = process.env.KSQUAD_CONSOLE_E2E === "1";
const SKIP_REASON =
  "ISI-6202 vibe entry on /settings/llm: set KSQUAD_CONSOLE_E2E=1 against a live console to " +
  "activate.";

test.describe("ISI-6202 · LLM Settings: Mistral (vibe) entry", () => {
  test.skip(!LIVE, SKIP_REASON);

  test("admin reaches Mistral (vibe) from the LLM settings BYO picker — masked key + optional base URL", async ({
    page,
  }) => {
    await page.goto("/settings/llm");

    // The OpenCode adapter branch owns the BYO picker (RuntimeAdapterStep, Frame-4).
    await page.getByRole("radio", { name: "OpenCode" }).click();

    const backend = page.getByLabel("Model backend");
    await expect(backend).toBeVisible();

    // The vibe entry is offered alongside the existing backends.
    await expect(
      backend.locator("option", { hasText: "Mistral (vibe)" }),
    ).toHaveCount(1);

    await backend.selectOption("mistral");

    // Masked API key (never plaintext) + the optional BYO base-URL field both appear.
    await expect(page.getByLabel("Model API key")).toHaveAttribute(
      "type",
      "password",
    );
    await expect(
      page.getByLabel("Model endpoint URL", { exact: true }),
    ).toBeVisible();
  });
});
