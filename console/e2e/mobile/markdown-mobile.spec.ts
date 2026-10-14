// ISI-5785 (WS-6 of ISI-5776) · Markdown render + Preview at a phone viewport (mobile lane).
//
// Runs ONLY under the mobile Playwright projects (mobile-pixel = Pixel 5, mobile-iphone = iPhone 13;
// see playwright.config.ts), i.e. at the <768 breakpoint where the adaptive shell re-expresses the
// desktop rail as a bottom nav + drawer. It proves the WS-2/WS-3/WS-4 Markdown surfaces survive the
// phone layout: rendered content stays rich AND does not overflow the viewport horizontally (the
// classic "content spills past the edge" breakage mobile UX guards, mirrored from mobile-responsive).
//
// Scope: ticket description render, discussion message render, and the composer Write→Preview toggle.
// Mutation is intentionally out of scope on mobile — the desktop lane owns the post path.
//
// Convention: source-scaffolded, skipped-WITH-REASON until a live console is reachable. Set
// KSQUAD_CONSOLE_E2E=1 and KSQUAD_CONSOLE_BASE_URL to activate; never silently dropped.

import { test, expect, type Page, type Locator } from "@playwright/test";

const LIVE = process.env.KSQUAD_CONSOLE_E2E === "1";
const SKIP_REASON =
  "ISI-5785 mobile Markdown: set KSQUAD_CONSOLE_E2E=1 and KSQUAD_CONSOLE_BASE_URL to a live console. " +
  "Runs under the mobile-pixel / mobile-iphone Playwright projects.";

const PROJECT_ID =
  process.env.KSQUAD_E2E_PROJECT_ID ?? "bmad-squad/bmad-demo-project";
const encoded = encodeURIComponent(PROJECT_ID);
const TICKET_ID = process.env.KSQUAD_E2E_TICKET_ID ?? "ISI-5776";

const discussionUrl = `/projects/${encoded}/discussion`;
const ticketUrl = `/projects/${encoded}/issues/${encodeURIComponent(TICKET_ID)}`;

/** No element may push the document wider than its own viewport (allow a 1px rounding slop). */
async function expectNoHorizontalOverflow(page: Page) {
  const overflow = await page.evaluate(() => {
    const el = document.scrollingElement || document.documentElement;
    return el.scrollWidth - el.clientWidth;
  });
  expect(overflow).toBeLessThanOrEqual(1);
}

async function expectRich(root: Locator) {
  await expect(root).toBeVisible();
  await expect(root).toHaveClass(/ksq-markdown/);
  await expect(
    root.locator("h1, h2, h3, strong, em, code, li, a").first(),
  ).toBeAttached();
}

test.describe("ISI-5785 · WS-6 Markdown on mobile", () => {
  test.skip(!LIVE, SKIP_REASON);

  test("ticket description renders rich Markdown without horizontal overflow", async ({
    page,
  }) => {
    await page.goto(ticketUrl);
    await expectRich(page.locator(".ksq-prose.ksq-markdown").first());
    await expectNoHorizontalOverflow(page);
  });

  test("discussion message renders rich Markdown without horizontal overflow", async ({
    page,
  }) => {
    await page.goto(discussionUrl);
    await expectRich(page.getByTestId("message-body").first());
    await expectNoHorizontalOverflow(page);
  });

  test("composer Write→Preview works at a phone viewport", async ({ page }) => {
    await page.goto(discussionUrl);

    const textarea = page.getByTestId("composer-body");
    await expect(textarea).toBeVisible();
    await textarea.fill("Mobile **preview** check\n\n- a\n- b");

    await page.getByTestId("composer-editor-tab-preview").click();
    const preview = page.getByTestId("composer-editor-preview");
    await expect(preview).toBeVisible();
    await expect(preview.locator(".ksq-markdown strong")).toHaveText("preview");
    // Rendered preview — including a wide code/table — never forces a horizontal scrollbar.
    await expectNoHorizontalOverflow(page);
  });
});
