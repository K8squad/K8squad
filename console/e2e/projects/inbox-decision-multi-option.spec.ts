// ISI-7226 · WS-6.4 of the ISI-7074 comment-quality plan — multi-option board decisions
// in the inbox: the browser leg (Playwright), convention mirrors
// e2e/projects/dispatch-run-minted.spec.ts (ISI-6278).
//
// Guards the WS-6.4 acceptance surface: when an agent raises a decision, the board sees
// an inbox row whose choose_one card carries >=2 CONCRETE labeled options — letter chips
// a/b/c… (Henrik 2026-11-08: "suggest a,b,c,d… options; always multiple"), a visible
// Recommended badge on the recommended option, an inline answer in the row itself, and
// number keys 1–5 as the keyboard path. Picking an option POSTs the typed answer shell
// (selectedOptionIds) through the BFF; the row leaves the open arm immediately — the
// same answer the requesting agent resumes on (the agent-side emission/resume path is
// WS-6.2 / ISI-7224; this spec drives the board half through the same wire).
//
// Companion DETERMINISTIC proofs (always-green, no live backend) pin each seam:
//   - console/test/inbox/DecisionCard.test.tsx — letter chips on every option,
//     recommended badge + pre-select, number keys 1–5 with a no-op beyond (WS-6.4).
//   - console/test/inbox/InboxList.test.tsx — inline letter chips + visible
//     Recommended badge in the row, number-key inline answer, cap-at-5 no-op.
//   - internal/apiserver/inbox_test.go TestSquadInboxDecisionArm — the inbox aggregate
//     joins the open decision arm onto the row (the data this journey renders).
//
// Convention (mirrors dispatch-run-minted): source-scaffolded, skipped-with-reason until
// a live console + session is reachable. Set KSQUAD_CONSOLE_E2E=1 (with
// KSQUAD_CONSOLE_BASE_URL) to activate; never silently dropped — the case stays visible
// in the Playwright report. Locators prefer the component's data-testid, the same
// boundary the unit suites exercise.
//
// Seed / session contract (the live run MUST satisfy these, else the spec fails honestly):
//   - a signed-in session that can read /inbox for a team with at least one OPEN
//     choose_one decision_request carrying >=2 options (agent-raised or seeded).

import { test, expect } from "@playwright/test";

const LIVE = process.env.KSQUAD_CONSOLE_E2E === "1";
const SKIP_REASON =
  "ISI-7226 WS-6.4 inbox multi-option e2e: set KSQUAD_CONSOLE_E2E=1 against a live " +
  "console (KSQUAD_CONSOLE_BASE_URL) with a signed-in session whose inbox has an OPEN " +
  "choose_one decision_request carrying >=2 options.";

test.describe("ISI-7226 · inbox multi-option decision journey (WS-6.4)", () => {
  test.skip(!LIVE, SKIP_REASON);

  test("a >=2-option decision row answers inline: letter chips, recommended badge, typed answer lands", async ({
    page,
  }) => {
    await page.goto("/inbox");

    // The first choose_one row with inline options (the WS-6.4 surface).
    const row = page
      .locator("li.inbox__row", {
        has: page.getByTestId("inline-choose"),
      })
      .first();
    await expect(row).toBeVisible();

    // >=2 concrete options — the starved-card regression WS-6.1 rejects server-side.
    const options = row.getByTestId("inline-option");
    expect(await options.count()).toBeGreaterThanOrEqual(2);

    // Letter chips a/b on the first two options (labeled-options contract).
    await expect(options.nth(0).getByTestId("inline-option-letter")).toHaveText(
      "a",
    );
    await expect(options.nth(1).getByTestId("inline-option-letter")).toHaveText(
      "b",
    );

    // The recommended option — when the card marks one — carries the visible badge.
    const recommended = row.getByTestId("inline-option-recommended");
    if ((await recommended.count()) > 0) {
      await expect(recommended.first()).toHaveText("Recommended");
    }

    // The board picks option b: the answer POSTs and the row leaves the open arm
    // immediately (resolved locally, no 5s-poll wait) — the typed answer the agent
    // resumes with.
    await options.nth(1).click();
    await expect(row).toBeHidden();
  });

  test("number keys 1–5 answer the focused row inline", async ({ page }) => {
    await page.goto("/inbox");

    const row = page
      .locator("li.inbox__row", {
        has: page.getByTestId("inline-choose"),
      })
      .first();
    await expect(row).toBeVisible();

    // Keyboard path: focus the row, pick option 2 with its number key.
    await row.focus();
    await page.keyboard.press("2");
    await expect(row).toBeHidden();
  });
});
