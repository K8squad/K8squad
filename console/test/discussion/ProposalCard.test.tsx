import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { ProposalCard, proposalActionLabel } from "@/components/discussion/ProposalCard";
import { MessageItem } from "@/components/discussion/MessageItem";
import type { Message, Proposal, ProposalPayload } from "@/lib/discussion/types";

afterEach(cleanup);

function msg(over: Partial<Message> = {}): Message {
  return {
    id: "m1",
    threadId: "t1",
    parentId: null,
    authorPrincipal: "planner-1",
    authorAgentId: "agent-1",
    authorRunId: null,
    body: "propose a thing",
    createdAt: "2026-09-09T10:00:00Z",
    ...over,
  };
}

function proposal(over: Partial<Proposal> = {}): Proposal {
  return {
    Message: msg({ kind: "proposal", payload: { action: "create_ticket", title: "Ship the room" } }),
    TeamID: "team-1",
    Payload: { action: "create_ticket", title: "Ship the room" },
    phase: "proposed",
    ...over,
  };
}

describe("proposalActionLabel — action one-liner (plan §6)", () => {
  it("labels create_ticket with its title", () => {
    expect(
      proposalActionLabel({ action: "create_ticket", title: "Ship" }),
    ).toBe('Create ticket "Ship"');
  });
  it("labels assign_agent with its agent + ticket", () => {
    expect(
      proposalActionLabel({
        action: "assign_agent",
        assigneeAgentId: "kimi",
        ticketId: "wi-1",
      }),
    ).toBe("Assign kimi to ticket wi-1");
  });
  it("labels party_run with its title", () => {
    expect(proposalActionLabel({ action: "party_run", title: "Do it" })).toBe(
      'Start a party run "Do it"',
    );
  });
});

describe("<ProposalCard> — dashed accent card + phase (ISI-4930)", () => {
  it("renders the action + phase and offers confirm/dismiss while proposed", () => {
    render(<ProposalCard proposal={proposal()} />);
    expect(screen.getByTestId("proposal-card")).toHaveAttribute(
      "data-phase",
      "proposed",
    );
    expect(screen.getByTestId("proposal-action")).toBeTruthy();
    expect(screen.getByTestId("proposal-confirm")).toBeTruthy();
    expect(screen.getByTestId("proposal-dismiss")).toBeTruthy();
  });

  it("hides confirm/dismiss once decided (executed) and shows the phase", () => {
    render(
      <ProposalCard
        proposal={proposal({ phase: "executed", decidedBy: "user:reviewer" })}
      />,
    );
    expect(screen.getByTestId("proposal-card")).toHaveAttribute(
      "data-phase",
      "executed",
    );
    expect(screen.queryByTestId("proposal-confirm")).toBeNull();
    expect(screen.queryByTestId("proposal-dismiss")).toBeNull();
  });

  it("fires onConfirm/onDismiss with the message id", () => {
    let confirmed = "";
    let dismissed = "";
    render(
      <ProposalCard
        proposal={proposal()}
        onConfirm={(id) => (confirmed = id)}
        onDismiss={(id) => (dismissed = id)}
      />,
    );
    screen.getByTestId("proposal-confirm").click();
    screen.getByTestId("proposal-dismiss").click();
    expect(confirmed).toBe("m1");
    expect(dismissed).toBe("m1");
  });
});

describe("<MessageItem> — proposal card + result post-back chip", () => {
  it("renders a proposal card for a kind='proposal' message with a join entry", () => {
    const p = proposal();
    render(
      <ul>
        <MessageItem
          message={p.Message}
          proposalByMessageId={{ [p.Message.id]: p }}
        />
      </ul>,
    );
    expect(screen.getByTestId("proposal-card")).toBeTruthy();
  });

  it("renders the result chip for a structured post-back with a workItemId", () => {
    const postBack = msg({
      id: "m2",
      parentId: "m1",
      kind: "structured",
      body: "Proposal confirmed.",
      payload: { action: "create_ticket", workItemId: "wi-123" },
    });
    render(
      <ul>
        <MessageItem message={postBack} projectId="proj-1" />
      </ul>,
    );
    const chip = screen.getByTestId("result-chip");
    expect(chip).toBeTruthy();
    expect(chip).toHaveAttribute("href", "/projects/proj-1/issues/wi-123");
  });
});
