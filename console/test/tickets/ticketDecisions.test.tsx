// test/tickets/ticketDecisions.test.tsx — ISI-5536 (ISI-5531 E2).
//
// The ticket surface renders decision_request cards (the SAME DecisionCard the Inbox
// row uses), answer/reject flows into the apiserver answer/reject seam (via
// lib/discussion/api.ts), a decision advances the card to its returned phase and
// re-syncs the ticket, and the surface is SILENT (renders nothing) when no thread is
// reachable or no cards exist — never a fabricated card. The DiscussionClient is
// injected as a stub so these are pure component tests (no fetch/SSE).

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import { TicketDecisions } from "@/components/tickets/TicketDecisions";
import type { DiscussionClient } from "@/lib/discussion/api";
import type { DecisionRequest } from "@/lib/discussion/types";

function decision(over: Partial<DecisionRequest> = {}): DecisionRequest {
  return {
    Message: {
      id: over.Message?.id ?? "msg-1",
      threadId: "thr-1",
      parentId: null,
      authorPrincipal: "agent:winston",
      authorAgentId: "winston",
      body: "Which HTTP client should the service use?",
      createdAt: "2026-10-01T10:00:00Z",
      kind: "decision_request",
    },
    TeamID: "team-1",
    Payload: {
      version: 1,
      mode: "choose_one",
      title: "Which HTTP client?",
      options: [
        { id: "reqwest", label: "reqwest", recommended: true },
        { id: "hyper", label: "hyper" },
      ],
      allowReject: true,
    },
    workItemId: "wi-1",
    idempotencyKey: "decision:wi-1:http:r1",
    continuation: "resume_agent_on_answer",
    phase: "open",
    ...over,
  };
}

function stubClient(over: Partial<DiscussionClient> = {}): DiscussionClient {
  return {
    listDecisionRequests: vi.fn(async () => [decision()]),
    answerDecisionRequest: vi.fn(async () => ({
      status: "answered" as const,
      decisionId: "msg-1",
    })),
    rejectDecisionRequest: vi.fn(async () => ({
      status: "rejected" as const,
      decisionId: "msg-1",
    })),
    ...over,
  } as unknown as DiscussionClient;
}

afterEach(cleanup);
beforeEach(() => vi.clearAllMocks());

describe("TicketDecisions — decision_request cards on the ticket surface (ISI-5536)", () => {
  it("renders a DecisionCard for each open card in the linked thread", async () => {
    const client = stubClient();
    render(<TicketDecisions projectId="ns/demo" threadId="thr-1" client={client} />);
    await waitFor(() => expect(screen.getByTestId("decision-card")).toBeTruthy());
    expect(screen.getByTestId("decision-title").textContent).toContain("Which HTTP client?");
    expect(client.listDecisionRequests).toHaveBeenCalledWith("ns/demo", "thr-1");
  });

  it("answer fans into the seam, advances the phase to answered, and re-syncs", async () => {
    const client = stubClient();
    const onDecided = vi.fn();
    render(
      <TicketDecisions projectId="ns/demo" threadId="thr-1" onDecided={onDecided} client={client} />,
    );
    await waitFor(() => expect(screen.getByTestId("decision-submit")).toBeTruthy());

    // The recommended option pre-selects, so Submit is enabled immediately.
    fireEvent.click(screen.getByTestId("decision-submit"));

    await waitFor(() =>
      expect(client.answerDecisionRequest).toHaveBeenCalledWith("ns/demo", "msg-1", {
        selectedOptionIds: ["reqwest"],
      }),
    );
    await waitFor(() => expect(screen.getByTestId("decision-decided")).toBeTruthy());
    expect(onDecided).toHaveBeenCalledTimes(1);
    // The answer form is gone once the card leaves `open`.
    expect(screen.queryByTestId("decision-submit")).toBeNull();
  });

  it("reject-with-reason fans into the seam and advances the phase to rejected", async () => {
    const client = stubClient();
    const onDecided = vi.fn();
    render(
      <TicketDecisions projectId="ns/demo" threadId="thr-1" onDecided={onDecided} client={client} />,
    );
    await waitFor(() => expect(screen.getByTestId("decision-reject-open")).toBeTruthy());

    fireEvent.click(screen.getByTestId("decision-reject-open"));
    const reason = await screen.findByTestId("decision-reject-reason-input");
    fireEvent.change(reason, { target: { value: "neither fits" } });
    fireEvent.click(screen.getByTestId("decision-reject-confirm"));

    await waitFor(() =>
      expect(client.rejectDecisionRequest).toHaveBeenCalledWith("ns/demo", "msg-1", "neither fits"),
    );
    await waitFor(() => expect(screen.getByTestId("decision-decided")).toBeTruthy());
    expect(onDecided).toHaveBeenCalledTimes(1);
  });

  it("renders nothing when a thread is linked but carries no cards (anti-nag)", async () => {
    const client = stubClient({ listDecisionRequests: vi.fn(async () => []) });
    const { container } = render(
      <TicketDecisions projectId="ns/demo" threadId="thr-1" client={client} />,
    );
    await waitFor(() => expect(client.listDecisionRequests).toHaveBeenCalled());
    expect(screen.queryByTestId("detail-decisions")).toBeNull();
    expect(container.querySelector("section")).toBeNull();
  });

  it("renders nothing and does not fetch when no thread is reachable", async () => {
    const client = stubClient();
    render(<TicketDecisions projectId="ns/demo" threadId={null} client={client} />);
    expect(screen.queryByTestId("detail-decisions")).toBeNull();
    expect(client.listDecisionRequests).not.toHaveBeenCalled();
  });
});
