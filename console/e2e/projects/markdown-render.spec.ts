// ISI-5785 (WS-6 of ISI-5776, Markdown support) · Markdown render + authoring E2E — desktop lane.
//
// Closes out the ISI-5776 feature by proving the three merged work-streams hold together in a real
// browser, end-to-end, and that neither of the pre-existing discussion affordances regressed:
//   - WS-2 (ISI-5782): ticket description + agent narration render as rich Markdown.
//   - WS-3 (ISI-5783): settled discussion messages render as rich Markdown (ThinkingStream stays plain).
//   - WS-4 (ISI-5784): both composers carry a Write/Preview toggle + minimal formatting toolbar.
//   - Regression (ISI-5214 / ISI-4567): the @mention popover and the # ticket-ref chips are the SAME
//     separate affordances they always were — Markdown rendering never swallowed them.
//
// Acceptance (from the approved plan ISI-5776, WS-6): green e2e; no regressions on mentions or ref chips.
//
// Companion deterministic proof already lives at the component boundary with canned wire responses:
//   test/markdown/Markdown.test.tsx, test/markdown/MarkdownEditor.test.tsx, test/discussion/MessageItem.test.tsx,
//   test/tickets/ticketDetail.ticketref.test.tsx. THIS spec is the live-browser leg — it drives the
//   user-visible journey over real HTTP through the Next BFF, not a stubbed fetch.
//
// Convention (mirrors e2e/projects/file-explorer.spec.ts + e2e/mobile/mobile-responsive.spec.ts):
// source-scaffolded, skipped-WITH-REASON until a live console + seeded data are reachable — never
// silently dropped; it stays visible-and-skipped in the Playwright report. Locators prefer the
// component's data-testid, with semantic getByRole where that is the clearest signal.
//
// Seed contract (the live fixture for KSQUAD_E2E_PROJECT_ID must provide these; the assertions are
// by test-id + rendered structure, so exact copy does not matter):
//   - KSQUAD_E2E_TICKET_ID: a work item whose DESCRIPTION is Markdown (at least one heading, one
//     **bold** or `code` span, and a list) and which carries at least one agent narration entry.
//   - The project discussion room (/discussion) has at least one SETTLED message whose body is
//     Markdown. Ref-chip + mention assertions self-gate on presence so a thin room still passes.
//   - Mutation cases (compose → Preview → post) are additionally gated behind KSQUAD_E2E_WRITE=1 so
//     the read-only lane never writes into a shared environment.

import { test, expect, type Page } from "@playwright/test";

const LIVE = process.env.KSQUAD_CONSOLE_E2E === "1";
const WRITE = process.env.KSQUAD_E2E_WRITE === "1";
const SKIP_REASON =
  "ISI-5785 Markdown e2e: set KSQUAD_CONSOLE_E2E=1 and KSQUAD_CONSOLE_BASE_URL to a live console " +
  "with a seeded Markdown ticket (KSQUAD_E2E_TICKET_ID) and a discussion room. Mutation cases also " +
  "need KSQUAD_E2E_WRITE=1.";
const WRITE_SKIP_REASON =
  "ISI-5785 compose/post cases mutate shared data — set KSQUAD_E2E_WRITE=1 to activate.";

// A namespace-qualified Project id (the "ns/name" shape the whole workspace reads, ISI-3982).
const PROJECT_ID =
  process.env.KSQUAD_E2E_PROJECT_ID ?? "bmad-squad/bmad-demo-project";
const encoded = encodeURIComponent(PROJECT_ID);
const TICKET_ID = process.env.KSQUAD_E2E_TICKET_ID ?? "ISI-5776";

const discussionUrl = `/projects/${encoded}/discussion`;
const ticketUrl = `/projects/${encoded}/issues/${encodeURIComponent(TICKET_ID)}`;

// The literal Markdown tokens that MUST NOT survive to the rendered surface — their presence as
// visible text is exactly the "renders as literal plain text" bug ISI-5776 set out to kill.
const LITERAL_MARKDOWN = /(^|\s)(#{1,3}\s|\*\*|__)/;

/** A Markdown root has rendered rich structure iff it carries at least one real block/inline element. */
async function expectRichMarkdown(
  page: Page,
  root: ReturnType<Page["locator"]>,
) {
  await expect(root).toBeVisible();
  await expect(root).toHaveClass(/ksq-markdown/);
  // At least one genuine rendered element — a heading, emphasis, code, list, link or table cell.
  await expect(
    root
      .locator("h1, h2, h3, strong, em, code, li, a, th, td, blockquote")
      .first(),
  ).toBeAttached();
  // And the raw syntax is gone (degraded pre-wrap fallback would still show the tokens).
  await expect(root).not.toHaveClass(/ksq-markdown--plain/);
}

test.describe("ISI-5785 · WS-6 Markdown render + authoring (desktop)", () => {
  test.skip(!LIVE, SKIP_REASON);

  test.describe("ticket detail render (WS-2 / ISI-5782)", () => {
    test("renders the ticket description as rich Markdown, not literal source", async ({
      page,
    }) => {
      await page.goto(ticketUrl);

      // The description uses the shared renderer with its legacy ksq-prose class preserved.
      const desc = page.locator(".ksq-prose.ksq-markdown").first();
      await expectRichMarkdown(page, desc);
      // Belt-and-braces: no bare "##"/"**" leaked through as visible text.
      await expect(desc).not.toHaveText(LITERAL_MARKDOWN);
    });

    test("renders agent narration as rich Markdown", async ({ page }) => {
      await page.goto(ticketUrl);

      const narration = page.getByTestId("progress-narration").first();
      // Self-gate: a ticket with no agent activity yet simply skips this leg.
      test.skip(
        (await narration.count()) === 0,
        "seed ticket has no agent narration entry yet",
      );
      await expectRichMarkdown(page, narration);
    });
  });

  test.describe("discussion render (WS-3 / ISI-5783)", () => {
    test("renders a settled message body as rich Markdown", async ({
      page,
    }) => {
      await page.goto(discussionUrl);

      const body = page.getByTestId("message-body").first();
      await expect(body).toBeVisible();
      await expectRichMarkdown(page, body);
    });

    test("REGRESSION: ref chips (ISI-5214) survive as a separate affordance", async ({
      page,
    }) => {
      await page.goto(discussionUrl);

      const refs = page.getByTestId("ticket-refs");
      // Self-gate: only assert when the room actually has a message carrying refs.
      test.skip(
        (await refs.count()) === 0,
        "no message with # ticket-ref chips in the seeded room",
      );
      // The chips render as their OWN list, a sibling of the Markdown body — not swallowed into it.
      await expect(refs.first()).toBeVisible();
      await expect(
        refs.first().getByTestId("ticket-ref").first(),
      ).toBeVisible();
      // The chip is NOT a descendant of the Markdown body (the affordance stayed separate).
      await expect(
        page.getByTestId("message-body").locator('[data-testid="ticket-ref"]'),
      ).toHaveCount(0);
    });
  });

  test.describe("discussion composer — Write/Preview (WS-4 / ISI-5784)", () => {
    test("Preview tab renders typed Markdown via the shared renderer", async ({
      page,
    }) => {
      await page.goto(discussionUrl);

      const textarea = page.getByTestId("composer-body");
      await expect(textarea).toBeVisible();
      await textarea.fill(
        "## Heading\n\nSome **bold** and `code`.\n\n- one\n- two",
      );

      await page.getByTestId("composer-editor-tab-preview").click();
      const preview = page.getByTestId("composer-editor-preview");
      await expect(preview).toBeVisible();
      // The preview reuses <Markdown>, so the same rich structure appears.
      await expect(preview.locator(".ksq-markdown h2")).toBeVisible();
      await expect(preview.locator(".ksq-markdown strong")).toHaveText("bold");
      await expect(preview.locator(".ksq-markdown li")).toHaveCount(2);

      // Back to Write: the textarea (and its source) is intact.
      await page.getByTestId("composer-editor-tab-write").click();
      await expect(textarea).toBeVisible();
      await expect(textarea).toHaveValue(/## Heading/);
    });

    test("the formatting toolbar wraps a selection in Markdown syntax", async ({
      page,
    }) => {
      await page.goto(discussionUrl);

      const textarea = page.getByTestId("composer-body");
      await textarea.fill("word");
      await textarea.selectText();
      await page.getByTestId("composer-editor-fmt-bold").click();
      await expect(textarea).toHaveValue("**word**");
    });

    test("REGRESSION: @mention popover opens in Write, hides in Preview, returns in Write", async ({
      page,
    }) => {
      await page.goto(discussionUrl);

      const textarea = page.getByTestId("composer-body");
      await textarea.click();
      await textarea.fill("@");
      // The mention popover is a Write-mode affordance and still triggers on "@".
      await expect(page.getByTestId("mention-popover")).toBeVisible();

      // Preview mode hides the popover (it is gated on composeMode === "write").
      await page.getByTestId("composer-editor-tab-preview").click();
      await expect(page.getByTestId("mention-popover")).toHaveCount(0);

      // Returning to Write restores the affordance — no permanent regression.
      await page.getByTestId("composer-editor-tab-write").click();
      await textarea.click();
      await textarea.fill("@");
      await expect(page.getByTestId("mention-popover")).toBeVisible();
    });

    test("posts a Markdown message that renders rich in the transcript", async ({
      page,
    }) => {
      test.skip(!WRITE, WRITE_SKIP_REASON);
      await page.goto(discussionUrl);

      const stamp = `e2e **bold** marker ${Date.now()}`;
      await page.getByTestId("composer-body").fill(stamp);
      await page.getByTestId("composer-submit").click();

      // The freshly posted (settled) message renders Markdown — its bold span is real emphasis.
      const posted = page
        .getByTestId("message-body")
        .filter({ hasText: "bold marker" })
        .last();
      await expect(posted).toBeVisible();
      await expect(posted.locator("strong")).toHaveText("bold");
    });
  });

  test.describe("ticket comment composer — Write/Preview (WS-4 / ISI-5784)", () => {
    test("Preview tab renders typed Markdown; mention popover is Write-only", async ({
      page,
    }) => {
      await page.goto(ticketUrl);

      const input = page.getByTestId("detail-composer-input");
      await expect(input).toBeVisible();
      await input.fill("A comment with **emphasis** and a list:\n\n- a\n- b");

      await page.getByTestId("detail-composer-editor-tab-preview").click();
      const preview = page.getByTestId("detail-composer-editor-preview");
      await expect(preview).toBeVisible();
      await expect(preview.locator(".ksq-markdown strong")).toHaveText(
        "emphasis",
      );
      await expect(preview.locator(".ksq-markdown li")).toHaveCount(2);

      // Regression: mention popover does not appear while previewing…
      await expect(page.getByTestId("mention-popover")).toHaveCount(0);
      // …and returns in Write mode on "@".
      await page.getByTestId("detail-composer-editor-tab-write").click();
      await input.click();
      await input.fill("@");
      await expect(page.getByTestId("mention-popover")).toBeVisible();
    });

    test("posts a Markdown comment that renders rich as narration", async ({
      page,
    }) => {
      test.skip(!WRITE, WRITE_SKIP_REASON);
      await page.goto(ticketUrl);

      const stamp = `e2e ticket \`code\` marker ${Date.now()}`;
      await page.getByTestId("detail-composer-input").fill(stamp);
      await page.getByTestId("detail-composer-submit").click();

      const posted = page
        .getByTestId("progress-narration")
        .filter({ hasText: "marker" })
        .last();
      await expect(posted).toBeVisible();
      await expect(posted.locator("code").first()).toBeVisible();
    });
  });

  test.describe("light / dark parity", () => {
    for (const theme of ["light", "dark"] as const) {
      test(`ticket description renders rich Markdown in ${theme} theme`, async ({
        page,
      }) => {
        await page.goto(ticketUrl);
        // v1 theme is a token swap keyed off <html data-theme> (components/ThemeProvider.tsx).
        await page.evaluate(
          (t) => document.documentElement.setAttribute("data-theme", t),
          theme,
        );
        await expect(page.locator("html")).toHaveAttribute("data-theme", theme);

        const desc = page.locator(".ksq-prose.ksq-markdown").first();
        await expectRichMarkdown(page, desc);
        // The rendered text is actually painted (non-transparent) in this theme.
        const color = await desc.evaluate((el) => getComputedStyle(el).color);
        expect(color).not.toBe("rgba(0, 0, 0, 0)");
      });

      test(`discussion message renders rich Markdown in ${theme} theme`, async ({
        page,
      }) => {
        await page.goto(discussionUrl);
        await page.evaluate(
          (t) => document.documentElement.setAttribute("data-theme", t),
          theme,
        );
        await expect(page.locator("html")).toHaveAttribute("data-theme", theme);
        await expectRichMarkdown(
          page,
          page.getByTestId("message-body").first(),
        );
      });
    }
  });
});
