// e2e/settings/llm-claude-entry.spec.ts — the Claude entry on the LLM settings surface
// (ISI-6702 WS-4). One settings-path case: an admin opening Settings → LLM Settings can
// attach an Anthropic API key from the Claude tab — the ISI-3983 BYO paste form (masked
// key, runtime pinned to Claude Code) with the ANTHROPIC_API_KEY test-connection probe.
// The OAuth CTA stays disabled (ToS-gated, ISI-3661). Companion Vitest units pin the
// same contract (test/compose/RuntimeAdapterStep.test.tsx, test/credentials/
// CreateCredentialForm.test.tsx). Convention mirrors e2e/settings/llm-vibe-entry.spec.ts.

import { test, expect } from "@playwright/test";

const LIVE = process.env.KSQUAD_CONSOLE_E2E === "1";
const SKIP_REASON =
  "ISI-6702 claude entry on /settings/llm: set KSQUAD_CONSOLE_E2E=1 against a live console to activate.";

test.describe("ISI-6702 · LLM Settings: Claude Code API-key entry", () => {
  test.skip(!LIVE, SKIP_REASON);

  test("admin attaches an Anthropic API key from the Claude tab — masked key, pinned runtime, disabled OAuth CTA", async ({
    page,
  }) => {
    await page.goto("/settings/llm");

    // The Claude adapter branch owns the key-attach lane (RuntimeAdapterStep, Frame-4).
    await page.getByRole("radio", { name: "Claude" }).click();

    await expect(page.getByTestId("modelpriority-claude")).toBeVisible();

    // Masked API key (never plaintext) — the BYO paste form contract.
    await expect(page.getByLabel("Service-account key")).toHaveAttribute(
      "type",
      "password",
    );

    // The runtime is pinned to Claude Code — no other runtime is offered.
    const runtime = page.getByTestId("creds-runtime");
    await expect(runtime).toBeDisabled();
    await expect(runtime).toHaveValue("claude-code");

    // The subscription path is ToS-gated (ISI-3661): never a live OAuth click.
    await expect(page.getByTestId("claude-oauth-cta")).toBeDisabled();
  });
});
