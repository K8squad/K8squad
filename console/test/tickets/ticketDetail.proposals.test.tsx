// test/tickets/ticketDetail.proposals.test.tsx — ISI-5284 (WS-4) integration.
//
// The REAL TicketDetail (not the isolated TicketProposals unit) renders the
// coordinator propose-mode surface + the shared sub-ticket tree, driven only by
// stubbed fetch + a no-op SSE. Proves the wiring end-to-end:
//   - given a linked proposal thread, the BFF GET …/proposals cards render on the
//     ticket surface with their confirm verb;
//   - with no linked thread (the merged-main default), the honest empty fallback
//     shows instead of a fabricated card;
//   - the ticket's children render through SubTicketTree's nodes.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup, waitFor, within } from "@testing-library/react";
import { TicketDetail } from "@/components/tickets/TicketDetail";

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

class NoopEventSource {
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: (() => void) | null = null;
  addEventListener() {}
  removeEventListener() {}
  close() {}
}
vi.stubGlobal("EventSource", NoopEventSource as unknown as typeof EventSource);

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

const THREAD = {
  WorkItemID: "wi-1",
  Title: "ship the thing",
  Description: "the full description",
  State: "backlog",
  BlockedReason: "",
  Comments: [],
  ChangeRefs: [],
  Holder: "",
  RunID: "",
  statusHistory: [],
};

const CHILDREN = [
  { id: "wi-2", projectId: "ns/demo", parentId: "wi-1", title: "sub a", state: "done", blockedReason: null, updatedAt: "2026-09-14T00:00:00Z" },
  { id: "wi-3", projectId: "ns/demo", parentId: "wi-1", title: "sub b", state: "todo", blockedReason: null, updatedAt: "2026-09-14T00:00:00Z" },
];

const PROPOSAL = {
  Message: {
    id: "msg-1",
    threadId: "thr-1",
    parentId: null,
    authorPrincipal: "agent:coordinator",
    body: "Proposed new ticket: wire the thing",
    createdAt: "2026-10-01T10:00:00Z",
    kind: "proposal",
  },
  TeamID: "team-1",
  Payload: { action: "create_ticket", title: "wire the thing" },
  phase: "proposed",
};

function routeFetch(opts?: { proposals?: unknown[] }) {
  fetchMock.mockImplementation((url: string, init?: RequestInit) => {
    const u = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    if (u.includes("/api/session")) return Promise.resolve(jsonResponse({ globalRole: "contributor" }));
    if (u.includes("/api/squad/agents")) return Promise.resolve(jsonResponse({ agents: [] }));
    if (u.includes("/api/runs")) return Promise.resolve(jsonResponse([]));
    if (u.includes("/discussion/threads/") && u.includes("/proposals") && method === "GET") {
      return Promise.resolve(jsonResponse(opts?.proposals ?? []));
    }
    if (u.includes("/api/work-items/")) return Promise.resolve(jsonResponse(THREAD));
    if (u.includes("/work-items")) return Promise.resolve(jsonResponse(CHILDREN));
    return Promise.resolve(jsonResponse([], 200));
  });
}

afterEach(cleanup);
beforeEach(() => fetchMock.mockReset());

describe("TicketDetail — proposals + sub-ticket tree (ISI-5284 WS-4)", () => {
  it("renders coordinator proposal cards from the linked thread on the ticket surface", async () => {
    routeFetch({ proposals: [PROPOSAL] });
    render(
      <TicketDetail projectId="ns/demo" workItemId="wi-1" proposalThreadId="thr-1" />,
    );

    const section = await screen.findByTestId("detail-proposals");
    await waitFor(() =>
      expect(within(section).getByTestId("proposal-card")).toBeTruthy(),
    );
    expect(within(section).getByTestId("proposal-action").textContent).toContain(
      'Create ticket "wire the thing"',
    );
    expect(within(section).getByTestId("proposal-confirm")).toBeTruthy();
  });

  it("shows the honest empty fallback (no card) when no proposal thread is linked", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    const section = await screen.findByTestId("detail-proposals");
    await waitFor(() =>
      expect(within(section).getByTestId("detail-proposals-empty")).toBeTruthy(),
    );
    expect(within(section).queryByTestId("proposal-card")).toBeNull();
    // threadId null ⇒ the BFF proposals endpoint is never even queried.
    expect(
      fetchMock.mock.calls.some(([u]) => String(u).includes("/proposals")),
    ).toBe(false);
  });

  it("renders the ticket's children through the shared sub-ticket tree", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    const tree = await screen.findByTestId("detail-subticket-tree");
    expect(within(tree).getAllByTestId("detail-subticket")).toHaveLength(2);
    expect(within(tree).getByText("sub a")).toBeTruthy();
    expect(within(tree).getByText("sub b")).toBeTruthy();
  });
});
