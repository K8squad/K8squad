// Teams left-nav expandable agents tree — console rail leg (Playwright, semantic locators).
//
// Story ISI-3995 / build ISI-4001. Companion to the Vitest component suite in
// test/nav/TeamsNavTree.test.tsx (which proves the outcome machine + rail-derivation at the
// component boundary); this spec proves the user-visible browser legs — that expanding Teams in
// the rail reveals team(s), expanding a team reveals its agents, an agent leaf lands on
// /agents/{id}, and every disclosure is keyboard-operable with correct aria-expanded (AC1/AC3/AC4/AC7).
//
// Source-scaffolding (§13): mechanism-concrete now, skipped-with-reason until a live console +
// a Teams read model + at least one team-with-agents fixture are reachable. Set KSQUAD_CONSOLE_E2E=1
// with such a fixture to activate. `test.skip` keeps the case visible in the Playwright report —
// never silently dropped.
//
// Convention: semantic locators only (getByRole / getByLabel / getByText) — no CSS/test-id
// selectors (§8/§10, persona E2E conventions). User-visible outcome assertions.

import { test, expect } from "@playwright/test";

const LIVE = process.env.KSQUAD_CONSOLE_E2E === "1";
const SKIP_REASON =
  "ISI-4001 Teams nav tree: pending a live console + Teams read model + a team-with-agents " +
  "fixture. Set KSQUAD_CONSOLE_E2E=1 against such an environment to activate.";

test.describe("ISI-4001 · Teams left-nav expandable agents tree", () => {
  test.skip(!LIVE, SKIP_REASON);

  test("expand Teams → expand a team → click an agent lands on /agents/{id}", async ({ page }) => {
    await page.goto("/overview");

    // AC1: Teams is collapsed by default; a distinct expand control (not the label link) opens it.
    const expandTeams = page.getByRole("button", { name: /expand teams/i });
    await expect(expandTeams).toHaveAttribute("aria-expanded", "false");
    await expandTeams.click();
    await expect(page.getByRole("button", { name: /collapse teams/i })).toHaveAttribute(
      "aria-expanded",
      "true",
    );

    // AC2/AC3: at least one team appears; expanding it lazy-loads its agents.
    const teamGroup = page.getByRole("group", { name: /^teams$/i });
    const firstTeamToggle = teamGroup.getByRole("button", { name: /^expand /i }).first();
    await firstTeamToggle.click();
    await expect(firstTeamToggle).toHaveAttribute("aria-expanded", "true");

    // AC4: an agent leaf deep-links to /agents/{id}.
    const agentsGroup = page.getByRole("group", { name: /agents$/i });
    const firstAgent = agentsGroup.getByRole("link").first();
    await firstAgent.click();
    await expect(page).toHaveURL(/\/agents\/[^/]+$/);
    // The rail marks the agent (and its ancestor Teams) as the active path.
    await expect(page.getByRole("link", { name: "Teams" })).toHaveAttribute("data-active", "true");
  });

  test("AC7: the whole disclosure path is keyboard-operable", async ({ page }) => {
    await page.goto("/overview");

    // Focus the Teams expand control and toggle with the keyboard.
    const expandTeams = page.getByRole("button", { name: /expand teams/i });
    await expandTeams.focus();
    await page.keyboard.press("Enter");
    await expect(page.getByRole("button", { name: /collapse teams/i })).toHaveAttribute(
      "aria-expanded",
      "true",
    );

    // The label link and the disclosure button are DISTINCT, separately-focusable affordances.
    const teamsLink = page.getByRole("link", { name: "Teams" });
    await expect(teamsLink).toHaveAttribute("href", "/teams");

    // Expand the first team via the keyboard too.
    const teamGroup = page.getByRole("group", { name: /^teams$/i });
    const firstTeamToggle = teamGroup.getByRole("button", { name: /^expand /i }).first();
    await firstTeamToggle.focus();
    await page.keyboard.press("Enter");
    await expect(firstTeamToggle).toHaveAttribute("aria-expanded", "true");
  });
});
