// ISI-5713 (WS-2 of ISI-5711) · Mobile-responsive smoke harness (Playwright).
//
// Institutionalizes mobile-UX testing so the WS-3 responsive fixes can't silently regress. This
// suite runs ONLY under the mobile Playwright projects (mobile-pixel = Pixel 5, mobile-iphone =
// iPhone 13; see playwright.config.ts) — i.e. at the <768 breakpoint where the adaptive shell
// (lib/nav.ts mobileNav + components/nav/ConsoleShell.tsx) re-expresses the desktop rail as a
// 5-item bottom nav + slide-in drawer. It proves two things at every phone viewport:
//   1. the shell paints its MOBILE chrome (bottom nav + top-bar menu, desktop rail hidden), and
//   2. each surface loads WITHOUT an error state AND without horizontal overflow — the classic
//      "content spills past the viewport" breakage the ISI-5711 user flagged (the WS-1 audit).
//
// Scope (ISI-5713): every top surface + the user-flagged flows — open project → its sections, and
// the team/roles surfaces (/agents-team, /users, /teams). An optional visual-snapshot block is
// gated behind KSQUAD_E2E_VISUAL so screenshots are opt-in (they drift across runners otherwise).
//
// Convention (mirrors e2e/nav/*.spec.ts + e2e/projects/*.spec.ts): semantic locators, user-visible
// outcome assertions, and source-scaffolded skip-with-reason until a live console is reachable.
// Set KSQUAD_CONSOLE_E2E=1 (and point KSQUAD_CONSOLE_BASE_URL at a live console) to activate; the
// case is never silently dropped — it stays visible-and-skipped in the Playwright report.

import { test, expect, type Page } from "@playwright/test";

const LIVE = process.env.KSQUAD_CONSOLE_E2E === "1";
const SKIP_REASON =
  "ISI-5713 mobile harness: set KSQUAD_CONSOLE_E2E=1 and KSQUAD_CONSOLE_BASE_URL to a live " +
  "console to activate. Runs under the mobile-pixel / mobile-iphone Playwright projects.";

// A namespace-qualified Project id (the "ns/name" shape the whole workspace reads, ISI-3982).
const PROJECT_ID =
  process.env.KSQUAD_E2E_PROJECT_ID ?? "bmad-squad/bmad-demo-project";
const encoded = encodeURIComponent(PROJECT_ID);

// The top-level surfaces the rail offers (lib/nav.ts navTree + the SETTINGS section children).
// Each must load at a phone viewport with mobile chrome and no overflow.
const TOP_SURFACES: ReadonlyArray<{ label: string; path: string }> = [
  { label: "Overview", path: "/overview" },
  { label: "Inbox", path: "/inbox" },
  { label: "Compose", path: "/compose" },
  { label: "Agents & Team", path: "/agents-team" },
  { label: "Projects", path: "/projects" },
  { label: "Runs", path: "/runs" },
  { label: "LLM Settings", path: "/settings/llm" },
  { label: "OTel", path: "/settings/configuration" },
  { label: "Credentials", path: "/credentials" },
  { label: "Plugins", path: "/plugins" },
];

// The project-detail sections (lib/nav.ts PROJECT_SECTIONS). Landing is the bare root.
const PROJECT_SECTIONS: ReadonlyArray<{ label: string; path: string }> = [
  { label: "Overview", path: `/projects/${encoded}` },
  { label: "Issues", path: `/projects/${encoded}/issues` },
  { label: "Runs", path: `/projects/${encoded}/runs` },
  { label: "Discussion", path: `/projects/${encoded}/discussion` },
  { label: "File Explorer", path: `/projects/${encoded}/files` },
  { label: "GitHub", path: `/projects/${encoded}/github` },
  { label: "Settings", path: `/projects/${encoded}/settings` },
];

// The team/roles surfaces the user flagged (/teams server-redirects to /agents-team).
const TEAM_ROLES_SURFACES: ReadonlyArray<{ label: string; path: string }> = [
  { label: "Agents & Team", path: "/agents-team" },
  { label: "Users & Roles", path: "/users" },
  { label: "Teams (legacy → unified)", path: "/teams" },
];

/**
 * Asserts the page painted the MOBILE shell variant (ConsoleShell.tsx): the 5-item bottom nav is
 * on screen and the desktop labeled rail is NOT — the single clearest proof we are below the <768
 * breakpoint and the adaptive shell re-expressed itself, not a desktop layout on a narrow window.
 */
async function expectMobileChrome(page: Page) {
  // Bottom nav exists on mobile only (aria-label "Primary mobile", ConsoleShell.tsx).
  await expect(
    page.getByRole("navigation", { name: "Primary mobile" }),
  ).toBeVisible();
  // The top bar's drawer-opener (mobile-only control) is reachable.
  await expect(
    page.getByRole("button", { name: "Open navigation" }),
  ).toBeVisible();
  // The desktop rail is display:none at this breakpoint — present in DOM but not visible.
  await expect(
    page.getByRole("navigation", { name: "Primary", exact: true }),
  ).toBeHidden();
}

/** Fails if any surface spilled horizontally past the viewport — the core mobile breakage. */
async function expectNoHorizontalOverflow(page: Page) {
  const overflow = await page.evaluate(() => {
    const el = document.scrollingElement ?? document.documentElement;
    // A few px of sub-pixel rounding is tolerated; a real spill is tens-to-hundreds of px.
    return el.scrollWidth - el.clientWidth;
  });
  expect(
    overflow,
    "page must not scroll horizontally at a phone viewport",
  ).toBeLessThanOrEqual(2);
}

/** A surface loaded honestly when no error/404 banner is on screen. */
async function expectNoErrorState(page: Page) {
  await expect(
    page.getByText(/error|failed to load|404|something went wrong/i),
  ).toHaveCount(0);
}

test.describe("ISI-5713 · mobile-responsive smoke (phone viewports)", () => {
  test.skip(!LIVE, SKIP_REASON);

  for (const surface of TOP_SURFACES) {
    test(`top surface loads with mobile chrome + no overflow: ${surface.label}`, async ({
      page,
    }) => {
      await page.goto(surface.path);
      await expectMobileChrome(page);
      await expectNoErrorState(page);
      await expectNoHorizontalOverflow(page);
    });
  }

  test("mobile drawer opens the overflow navigation and closes again", async ({
    page,
  }) => {
    await page.goto("/overview");

    await page.getByRole("button", { name: "Open navigation" }).click();
    const drawer = page.getByRole("dialog", { name: "Navigation" });
    await expect(drawer).toBeVisible();
    // The drawer carries the real destinations (flattened mobileNav) — a known link proves it.
    await expect(drawer.getByRole("link", { name: "Compose" })).toBeVisible();

    await drawer.getByRole("button", { name: "Close navigation" }).click();
    await expect(drawer).toBeHidden();
  });

  test.describe("flagged flow · open project → sections", () => {
    test("Projects → first project row lands on the workspace (mobile)", async ({
      page,
    }) => {
      await page.goto("/projects");
      await expectMobileChrome(page);

      // A seeded tenant renders project rows; tap the first one and land on its workspace.
      const firstProject = page.getByTestId("projects-row-link").first();
      await expect(firstProject).toBeVisible();
      await firstProject.click();

      await expect(page).toHaveURL(/\/projects\/[^/]+/);
      await expectNoErrorState(page);
      await expectNoHorizontalOverflow(page);
    });

    for (const section of PROJECT_SECTIONS) {
      test(`project section loads at phone width: ${section.label}`, async ({
        page,
      }) => {
        await page.goto(section.path);
        await expectMobileChrome(page);
        await expectNoErrorState(page);
        await expectNoHorizontalOverflow(page);
      });
    }
  });

  test.describe("flagged flow · team / roles surfaces", () => {
    for (const surface of TEAM_ROLES_SURFACES) {
      test(`team/roles surface loads at phone width: ${surface.label}`, async ({
        page,
      }) => {
        await page.goto(surface.path);
        // /teams redirects to the unified surface (ISI-5432); either way, mobile chrome + no spill.
        await expect(page).toHaveURL(/\/(agents-team|users)/);
        await expectMobileChrome(page);
        await expectNoErrorState(page);
        await expectNoHorizontalOverflow(page);
      });
    }
  });

  // Optional visual snapshots at mobile widths (ISI-5713 "optional"). Opt-in: screenshots drift
  // across runner fonts/AA, so they stay behind KSQUAD_E2E_VISUAL rather than redden the shared gate.
  test.describe("optional · mobile visual snapshots", () => {
    test.skip(
      !process.env.KSQUAD_E2E_VISUAL,
      "ISI-5713 visual snapshots: set KSQUAD_E2E_VISUAL=1 (with a pinned runner image) to activate.",
    );

    for (const surface of [TOP_SURFACES[0], TOP_SURFACES[3], TOP_SURFACES[4]]) {
      test(`snapshot: ${surface.label}`, async ({ page }) => {
        await page.goto(surface.path);
        await expectMobileChrome(page);
        // Per-project snapshot name keyed by the Playwright project (Pixel vs iPhone) automatically.
        await expect(page).toHaveScreenshot(
          `${surface.label.replace(/\W+/g, "-")}.png`,
          {
            fullPage: true,
            maxDiffPixelRatio: 0.02,
          },
        );
      });
    }
  });
});
