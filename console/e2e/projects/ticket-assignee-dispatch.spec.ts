// e2e/projects/ticket-assignee-dispatch.spec.ts - ISI-6290 "dropdown pick ==
// dispatched agentId" conformance (browser leg).
//
// Guards the ISI-6290 regression class: the ticket-detail composer has TWO agent
// gestures - the explicit assignee dropdown (detail-composer-assignee) and the
// typed @mention body (ISI-5281) - and the PRIMARY Comment submit must honor the
// EXPLICIT pick. In the incident, the human commented, picked an agent in the
// dropdown and hit Comment: the pick was silently ignored, no dispatch POST was
// ever sent, and on a parked ticket the comment nudge (AppendHumanComment
// re-trigger, ISI-4495) re-minted a Run for the STALE requested agent -
// "picked john, tess started working".
//
// What this spec proves end-to-end through the BFF choke point (browser -> Next
// BFF -> apiserver -> coord.audit_log), never the apiserver directly:
//   - arming the explicit dropdown pick and clicking the PRIMARY Comment submit
//     sends exactly ONE POST /api/work-items/{id}/dispatch whose body carries the
//     PICKED agentId - even when the comment body also contains a resolvable
//     @mention of a DIFFERENT agent (the hijack vector),
//   - the ticket thread re-syncs to the picked agent (requested-agent stamp).
//
// Companion deterministic proof: console/test/tickets/ticketDetail.test.tsx
// "ISI-6290" cases pin the same precedence in the component suite (armed pick +
// primary submit; pick beats typed mention).
//
// Convention (mirrors e2e/projects/dispatch-run-minted.spec.ts): skipped with
// reason until a live console + admin session is reachable. Set
// KSQUAD_CONSOLE_E2E=1, KSQUAD_CONSOLE_BASE_URL, KSQUAD_E2E_PROJECT_ID (default
// the incident project) and KSQUAD_E2E_USER / KSQUAD_E2E_PASSWORD. The probe
// ticket is KILLED in cleanup so Intake never mints a live Run for it.

import { test, expect } from "@playwright/test";

const LIVE = process.env.KSQUAD_CONSOLE_E2E === "1";
const SKIP_REASON =
  "ISI-6290 assignee-pick e2e: set KSQUAD_CONSOLE_E2E=1 against a live console " +
  "with KSQUAD_CONSOLE_BASE_URL, KSQUAD_E2E_PROJECT_ID and admin " +
  "KSQUAD_E2E_USER/KSQUAD_E2E_PASSWORD (a squad with >=2 agents).";

const PROJECT_ID =
  process.env.KSQUAD_E2E_PROJECT_ID ?? "sympozium-squad/sympozium-todo-demo";
const encoded = encodeURIComponent(PROJECT_ID); // exactly one layer: ns%2Fname
const boardUrl = "/projects/" + encoded + "/tickets";

test.describe("ISI-6290 - dropdown pick == dispatched agentId", () => {
  test.skip(!LIVE, SKIP_REASON);

  test("an armed pick + the PRIMARY Comment submit dispatches the PICKED agent, never a typed @mention", async ({
    page,
  }) => {
    test.setTimeout(120_000); // first next-dev compile can be slow

    // Seed the admin session through the BFF (same choke point the UI login
    // uses); page.request shares the browser context's cookie jar.
    const login = await page.request.post("/api/session", {
      data: {
        username: process.env.KSQUAD_E2E_USER,
        password: process.env.KSQUAD_E2E_PASSWORD,
      },
    });
    expect(login.status(), "admin session seed via POST /api/session").toBe(200);

    // Capture every dispatch POST; the conformance target.
    const dispatchBodies: Array<{ url: string; agentId?: string }> = [];
    page.on("request", (req) => {
      if (req.method() === "POST" && req.url().includes("/dispatch")) {
        let agentId: string | undefined;
        try {
          agentId = (req.postDataJSON() as { agentId?: string }).agentId;
        } catch {
          agentId = undefined;
        }
        dispatchBodies.push({ url: req.url(), agentId });
      }
    });

    // Fresh backlog ticket via the board UI (deterministic regardless of the
    // board's current lane mix - backlog is directly dispatchable).
    await page.goto(boardUrl, { timeout: 60_000 });
    await expect(page.getByTestId("tickets-screen")).toBeVisible();
    const title = "isi-6290 pick conformance " + Date.now();
    await page.getByTestId("new-issue").click();
    await expect(page.getByTestId("create-ticket-sheet")).toBeVisible();
    await page.getByTestId("create-ticket-title").fill(title);
    await page.getByTestId("create-ticket-submit").click();
    await expect(page.getByTestId("create-ticket-sheet")).toBeHidden();
    await page.getByRole("link", { name: title }).first().click();

    // The composer roster select - same load the rail waits on.
    const composerSelect = page.getByTestId("detail-composer-assignee");
    await expect(composerSelect).toBeVisible({ timeout: 30_000 });
    const options = composerSelect.locator("option:not([value=''])");
    // <option> elements inside a closed <select> are never "visible" — poll the
    // COUNT instead: the roster GET landing means real agent options exist.
    await expect
      .poll(async () => await options.count(), { timeout: 30_000 })
      .toBeGreaterThan(1);

    // TWO distinct roster agents: the mention bait (first) and the pick (second).
    // Values are the canonical agent NAMES (the wire identity dispatch uses).
    const values = await options.evaluateAll((els) =>
      (els as Array<HTMLOptionElement>).map((o) => o.value),
    );
    expect(values.length, "squad roster with at least two agents").toBeGreaterThanOrEqual(2);
    const mentionBait = values[0]!;
    const picked = values[1]!;

    // The repro gesture: a body mentioning a DIFFERENT agent (the ISI-5281
    // hijack vector), the explicit pick armed, then the PRIMARY Comment submit.
    await page
      .getByTestId("detail-composer-input")
      .fill("@" + mentionBait + " handing this to " + picked + " - please take it");
    await composerSelect.selectOption(picked);
    await page.getByTestId("detail-composer-submit").click();

    // The ONE dispatch POST carries the PICKED agentId - never the bait.
    await expect
      .poll(async () => dispatchBodies.length, { timeout: 30_000 })
      .toBeGreaterThanOrEqual(1);
    expect(dispatchBodies, "exactly one dispatch POST").toHaveLength(1);
    expect(dispatchBodies[0]!.agentId).toBe(picked);

    // The thread re-syncs to the picked agent: the reconciling GET shows the
    // requested-agent STAMP. Read the thread API rather than a DOM testid -
    // Intake claims the item within seconds (lane leaves todo), so the rail's
    // "dispatch pending" hint may already be gone by the time we look.
    const itemId = dispatchBodies[0]!.url.match(/work-items\/([^/]+)\/dispatch/)?.[1];
    expect(itemId, "work item id from the dispatch URL").toBeTruthy();
    const thread = await page.request.get("/api/work-items/" + itemId);
    expect(thread.ok(), "thread re-fetch after dispatch").toBeTruthy();
    const threadBody = (await thread.json()) as { RequestedAgent?: string };
    expect(threadBody.RequestedAgent, "the requested-agent stamp is the PICK").toBe(picked);

    // Cleanup: kill the probe ticket so Intake never mints a live Run.
    if (itemId) {
      await page.request.post("/api/work-items/" + itemId + "/kill").catch(() => {});
    }
  });
});
