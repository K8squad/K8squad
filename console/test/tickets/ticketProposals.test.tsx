// test/tickets/ticketProposals.test.tsx — ISI-5284 (WS-4).
//
// The ticket surface renders coordinator propose-mode proposal cards (the SAME
// ProposalCard the discussion room uses), confirm/dismiss flows into the existing
// proposalconfirm.go seam (via lib/discussion/api.ts), a confirmed create reloads
// the sub-ticket tree, and the honest D3 fallback shows when no coordinator thread
// is reachable — never a fabricated card. The DiscussionClient is injected as a
// stub so these are pure component tests (no fetch/SSE).

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  render,
  screen,
  cleanup,
  waitFor,
  fireEvent,
} from "@testing-library/react";
import { TicketProposals } from "@/components/tickets/TicketProposals";
import type { DiscussionClient } from "@/lib/discussion/api";
import type { Proposal } from "@/lib/discussion/types";

function proposal(over: Partial<Proposal> = {}): Proposal {
  return {
    Message: {
      id: over.Message?.id ?? "msg-1",
      threadId: "thr-1",
      parentId: null,
      authorPrincipal: "agent:coordinator",
      authorAgentId: "coordinator",
      body: "Proposed new ticket: wire the thing",
      createdAt: "2026-10-01T10:00:00Z",
      kind: "proposal",
    },
    TeamID: "team-1",
    Payload: { action: "create_ticket", title: "wire the thing" },
    phase: "proposed",
    ...over,
  };
}

/** A DiscussionClient stub exposing only the three methods this surface uses. */
function stubClient(over: Partial<DiscussionClient> = {}): DiscussionClient {
  return {
    listProposals: vi.fn(async () => [proposal()]),
    confirmProposal: vi.fn(async () => ({
      status: "executed" as const,
      proposalId: "msg-1",
      result: { action: "create_ticket" as const, workItemId: "wi-child-1" },
    })),
    dismissProposal: vi.fn(async () => ({ status: "dismissed" as const })),
    ...over,
  } as unknown as DiscussionClient;
}

afterEach(cleanup);
beforeEach(() => vi.clearAllMocks());

describe("TicketProposals — coordinator propose-mode cards on the ticket surface (ISI-5284)", () => {
  it("renders a ProposalCard for each card in the linked thread", async () => {
    const client = stubClient();
    render(
      <TicketProposals
        projectId="ns/demo"
        threadId="thr-1"
        issuesHref="/projects/ns%2Fdemo/issues"
        client={client}
      />,
    );
    await waitFor(() =>
      expect(screen.getByTestId("proposal-card")).toBeTruthy(),
    );
    expect(screen.getByTestId("proposal-action").textContent).toContain(
      'Create ticket "wire the thing"',
    );
    expect(client.listProposals).toHaveBeenCalledWith("ns/demo", "thr-1");
  });

  it("confirm fans into the seam, advances the phase to executed, and reloads children", async () => {
    const client = stubClient();
    const onExecuted = vi.fn();
    render(
      <TicketProposals
        projectId="ns/demo"
        threadId="thr-1"
        issuesHref="/i"
        onExecuted={onExecuted}
        client={client}
      />,
    );
    await waitFor(() => expect(screen.getByTestId("proposal-confirm")).toBeTruthy());

    fireEvent.click(screen.getByTestId("proposal-confirm"));

    await waitFor(() =>
      expect(client.confirmProposal).toHaveBeenCalledWith("ns/demo", "msg-1"),
    );
    // Phase advanced to the server's verdict…
    await waitFor(() =>
      expect(screen.getByTestId("proposal-phase").getAttribute("data-phase")).toBe(
        "executed",
      ),
    );
    // …and the resulting sub-ticket triggers a children reload.
    expect(onExecuted).toHaveBeenCalledTimes(1);
    // Confirm/dismiss verbs are gone once the card leaves `proposed`.
    expect(screen.queryByTestId("proposal-confirm")).toBeNull();
  });

  it("dismiss records the decision and does NOT reload children", async () => {
    const client = stubClient();
    const onExecuted = vi.fn();
    render(
      <TicketProposals
        projectId="ns/demo"
        threadId="thr-1"
        issuesHref="/i"
        onExecuted={onExecuted}
        client={client}
      />,
    );
    await waitFor(() => expect(screen.getByTestId("proposal-dismiss")).toBeTruthy());

    fireEvent.click(screen.getByTestId("proposal-dismiss"));

    await waitFor(() =>
      expect(client.dismissProposal).toHaveBeenCalledWith("ns/demo", "msg-1"),
    );
    await waitFor(() =>
      expect(screen.getByTestId("proposal-phase").getAttribute("data-phase")).toBe(
        "dismissed",
      ),
    );
    expect(onExecuted).not.toHaveBeenCalled();
  });

  it("D3 fallback: no coordinator configured → an assign-individually prompt, NOT a card", async () => {
    const client = stubClient({ listProposals: vi.fn(async () => []) });
    render(
      <TicketProposals
        projectId="ns/demo"
        threadId={null}
        hasCoordinator={false}
        issuesHref="/projects/ns%2Fdemo/issues"
        client={client}
      />,
    );
    await waitFor(() =>
      expect(screen.getByTestId("detail-proposals-no-coordinator")).toBeTruthy(),
    );
    expect(screen.getByTestId("detail-proposals-assign-link")).toBeTruthy();
    expect(screen.queryByTestId("proposal-card")).toBeNull();
    // threadId null ⇒ no listProposals call (nothing to fetch, never fabricated).
    expect(client.listProposals).not.toHaveBeenCalled();
  });

  it("neutral empty state when a thread is linked but carries no cards", async () => {
    const client = stubClient({ listProposals: vi.fn(async () => []) });
    render(
      <TicketProposals
        projectId="ns/demo"
        threadId="thr-1"
        issuesHref="/i"
        client={client}
      />,
    );
    await waitFor(() =>
      expect(screen.getByTestId("detail-proposals-empty")).toBeTruthy(),
    );
    expect(screen.queryByTestId("proposal-card")).toBeNull();
  });

  it("shows the 'manager is structuring the work' pill while the coordinator run is live", async () => {
    const client = stubClient({ listProposals: vi.fn(async () => []) });
    render(
      <TicketProposals
        projectId="ns/demo"
        threadId="thr-1"
        manager={{ name: "coordinator", state: "working" }}
        issuesHref="/i"
        client={client}
      />,
    );
    await waitFor(() =>
      expect(screen.getByTestId("detail-proposals-working")).toBeTruthy(),
    );
    const pill = screen.getByTestId("ticket-working-indicator");
    expect(pill.getAttribute("data-phase")).toBe("working");
    expect(screen.getByText("coordinator is structuring the work…")).toBeTruthy();
  });
});
