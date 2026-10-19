// ISI-6278 · "UI dispatch → Run minted" conformance — the browser leg (Playwright).
//
// Guards the ISI-6267 regression CLASS: a silent break anywhere on the UI-reachable
// dispatch path. In that incident GET /api/squad/projects/{name} (ProjectDetail)
// 404'd for an unbound fleet admin, so board assignment went silently dead — no
// error reached the user and NO coord.audit_log dispatch row was written (zero rows
// Oct 17 22:03Z → the 12:46Z probe). ae5fa224 fixed ProjectDetail; this spec proves
// the whole user journey an unbound admin takes — board loads, dispatch an agent,
// a Run is minted — so that class of breakage can never recur silently again.
//
// What it asserts end-to-end, through the BFF choke point (browser → Next BFF →
// apiserver → operator Intake), never the apiserver directly:
//   - the board loads for an unbound admin with NO 404 on any project-scope read
//     (the exact ISI-6267 signature: a project-detail / work-items / settings 404)
//   - assigning an agent to a dispatchable ticket succeeds (no assign error), and
//   - the honest dispatch ladder (ISI-4853) advances past "Queued" to a live Run —
//     reaching `picking_up`/`working` requires a Run ROW to exist, which only the
//     operator Intake sweep can mint (intake.go). That transition IS the
//     user-visible proof a Run CR was minted (AC1).
//
// Companion DETERMINISTIC proofs (always-green, no live backend) that pin each seam
// this journey composes — cited so a reviewer can see the layered coverage:
//   - internal/apiserver/fleetlist_test.go  TestProjectDetailScoping — unbound-admin
//     ProjectDetail resolves fleet-wide / 409 on collision / 404 on absent (ISI-6267).
//   - internal/apiserver/workitemdispatch_test.go  TestWorkItemDispatchOK — dispatch
//     advances backlog→todo and stamps requested_agent (the Run-minting precondition).
//   - pkg/controller/rundrive/intake_test.go  TestIntakeSweepHonorsRequestedAgent —
//     a todo item carrying requested_agent mints its Run addressed to that agent.
//   - console/test/tickets/ticketDetail.test.tsx — the dispatch 409 copy (live_run /
//     not_dispatchable / concurrent_change) renders in the rail (ISI-5295 / ISI-6278 AC2).
//
// Convention (mirrors e2e/projects/file-explorer.spec.ts): source-scaffolded,
// skipped-with-reason until a live console + an unbound-admin session is reachable.
// Set KSQUAD_CONSOLE_E2E=1 (and KSQUAD_E2E_PROJECT_ID) to activate; never silently
// dropped — the case stays visible in the Playwright report. Locators prefer the
// component's data-testid, the same boundary the unit suites already exercise.
//
// Seed / session contract (the live run MUST satisfy these, else the spec skips honestly):
//   - the session is an UNBOUND fleet admin (no ?team= is ever sent — the ISI-6267 path)
//   - KSQUAD_E2E_PROJECT_ID names a fleet-unique project whose squad has ≥1 agent in
//     its composition (so the assignee select has a dispatchable target)

import { test, expect, type Page } from "@playwright/test";

const LIVE = process.env.KSQUAD_CONSOLE_E2E === "1";
const SKIP_REASON =
  "ISI-6278 UI-dispatch→Run-minted e2e: set KSQUAD_CONSOLE_E2E=1 against a live console " +
  "signed in as an UNBOUND fleet admin, with KSQUAD_E2E_PROJECT_ID a fleet-unique project " +
  "whose squad has at least one agent.";

// A namespace-qualified Project id — the shape the whole board reads (ns/name).
const PROJECT_ID =
  process.env.KSQUAD_E2E_PROJECT_ID ?? "bmad-squad/bmad-demo-project";
const encoded = encodeURIComponent(PROJECT_ID); // exactly one layer: "ns%2Fname"
const boardUrl = `/projects/${encoded}/tickets`;

// Attach a guard that FAILS the test if any project-scope read 404s during the
// journey — the precise ISI-6267 signature (ProjectDetail / work-items / settings
// silently returning 404 so the board/dispatch dies without a user-visible error).
function failOnProjectScope404(page: Page): void {
  page.on("response", (res) => {
    const url = res.url();
    const scoped =
      url.includes("/api/squad/projects/") || url.includes("/api/projects/");
    if (scoped && res.status() === 404) {
      throw new Error(
        `ISI-6267 regression: project-scope read 404'd (${res.status()} ${url}). ` +
          `An unbound admin must resolve project reads fleet-wide, never 404.`,
      );
    }
  });
}

test.describe("ISI-6278 · UI dispatch → Run minted (unbound admin)", () => {
  test.skip(!LIVE, SKIP_REASON);

  test("the board loads for an unbound admin with no project-scope 404 (ISI-6267 signature)", async ({
    page,
  }) => {
    failOnProjectScope404(page);
    await page.goto(boardUrl);

    // The tickets screen renders — list or honest empty state, but never the
    // detail-404 error page the ISI-6267 break produced.
    await expect(page.getByTestId("tickets-screen")).toBeVisible();
    // Switch to the kanban board explicitly (the CTO's journey: "load a project board").
    await page.getByTestId("view-toggle-kanban").click();
    await expect(page.getByTestId("kanban-board")).toBeVisible();
  });

  test("assigning an agent to a fresh ticket mints a Run — the honest ladder reaches a live Run (AC1)", async ({
    page,
  }) => {
    failOnProjectScope404(page);
    await page.goto(boardUrl);
    await expect(page.getByTestId("tickets-screen")).toBeVisible();

    // Create a FRESH, unassigned ticket so the journey is deterministic regardless
    // of the board's current lane mix (the CTO's note: all existing tickets may sit
    // in_progress). A new ticket lands in backlog — directly dispatchable (the
    // backlog→todo verb, workitemdispatch.go).
    const title = `isi-6278 dispatch conformance ${Date.now()}`;
    await page.getByTestId("new-issue").click();
    await expect(page.getByTestId("create-ticket-sheet")).toBeVisible();
    await page.getByTestId("create-ticket-title").fill(title);
    await page.getByTestId("create-ticket-submit").click();
    await expect(page.getByTestId("create-ticket-sheet")).toBeHidden();

    // Open the new ticket from the board by its title link (card-title-*).
    await page.getByRole("link", { name: title }).first().click();

    // The detail rail's assignee control — the board's "assign agent → start a Run"
    // verb. Pick the first real agent in the squad roster (index 0 is the
    // "Assign agent…" placeholder).
    const select = page.getByTestId("detail-assignee-select");
    await expect(select).toBeVisible();
    await select.selectOption({ index: 1 });

    // The dispatch must NOT silently fail: no assign error, and the honest ladder
    // appears at "Queued" (dispatch 200, client-side; no Run row yet).
    await expect(page.getByTestId("detail-assignee-error")).toHaveCount(0);
    const card = page.getByTestId("dispatch-pending-card");
    await expect(card).toBeVisible();

    // THE conformance assertion: the ladder advances off "Queued". `picking_up`
    // (and beyond) requires a Run ROW discovered via GET /api/runs matching this
    // ticket's workItemRef — a row only the operator Intake sweep mints. Reaching
    // it is the user-visible proof a Run CR was minted (AC1). Generous timeout:
    // Intake runs on its own tick (IntakeInterval), not synchronously on dispatch.
    await expect(card).toHaveAttribute(
      "data-state",
      /picking_up|working|succeeded/,
      { timeout: 60_000 },
    );
  });
});
