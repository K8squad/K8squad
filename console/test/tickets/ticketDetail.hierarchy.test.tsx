// test/tickets/ticketDetail.hierarchy.test.tsx — ISI-5312 (impl of ISI-5311):
// the ticket-detail RIGHT RAIL surfaces the work-item hierarchy both directions,
// mirroring Paperclip's pattern:
//   • a PARENT ticket lists its sub-tickets as quick links, each with its status
//     pill at a glance, clickable to the child;
//   • a SUB-TICKET shows a quick link UP to its parent (title + status pill).
// Both are resolved from reads the console already owns (the per-parent children
// list + the Project card list, which carries parentId/title/state) — no new
// backend. A root ticket keeps the honest em-dash in the Parent row (FR-I3).

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup, waitFor, within } from "@testing-library/react";
import { TicketDetail } from "@/components/tickets/TicketDetail";

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

// jsdom has no EventSource; the ambient run stream (ISI-5206) arms on a thread
// carrying a RunID. A no-op stub keeps the transport inert.
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

// BoardItem rows as the list endpoint returns them (camelCase wire, workitemread.go):
// one root parent + two children. The Project card list carries parentId + title +
// state for every card, which is all the rail hierarchy links need.
const PARENT = {
  id: "wi-1",
  projectId: "ns/demo",
  parentId: null,
  title: "ship the thing",
  state: "in_review",
  blockedReason: null,
  updatedAt: "2026-09-14T00:00:00Z",
};
const CHILD_A = {
  id: "wi-2",
  projectId: "ns/demo",
  parentId: "wi-1",
  title: "sub a",
  state: "done",
  blockedReason: null,
  updatedAt: "2026-09-14T00:00:00Z",
};
const CHILD_B = {
  id: "wi-3",
  projectId: "ns/demo",
  parentId: "wi-1",
  title: "sub b",
  state: "todo",
  blockedReason: null,
  updatedAt: "2026-09-14T00:00:00Z",
};
const ALL_ITEMS = [PARENT, CHILD_A, CHILD_B];

function threadFor(item: { id: string; title: string; state: string }) {
  return {
    WorkItemID: item.id,
    Title: item.title,
    Description: "x",
    State: item.state,
    BlockedReason: "",
    Comments: [],
    ChangeRefs: [],
    Holder: "",
    RunID: "",
    statusHistory: [],
  };
}

/**
 * Route the stubbed fetch for a given detail `workItemId`. The two `/work-items`
 * reads are distinguished by their query string: `?parentId=` is the children
 * list (useChildren), bare is the full Project card list (useParentLink).
 */
function routeFetch(workItemId: string) {
  const self = ALL_ITEMS.find((i) => i.id === workItemId)!;
  fetchMock.mockImplementation((url: string, init?: RequestInit) => {
    const u = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    if (u.includes("/api/session")) {
      return Promise.resolve(jsonResponse({ globalRole: "contributor" }));
    }
    if (u.includes("/api/squad/agents")) {
      return Promise.resolve(jsonResponse({ agents: [] }));
    }
    // The per-ticket thread read (never the list — it carries /api/work-items/{id}).
    if (u.includes("/api/work-items/") && method === "GET") {
      return Promise.resolve(jsonResponse(threadFor(self)));
    }
    // The children list (useChildren) — scoped to this item's direct children.
    if (u.includes("/work-items") && u.includes("parentId=")) {
      const kids = ALL_ITEMS.filter((i) => i.parentId === workItemId);
      return Promise.resolve(jsonResponse(kids));
    }
    // The full Project card list (useParentLink) — every card with parentId.
    if (u.includes("/work-items")) {
      return Promise.resolve(jsonResponse(ALL_ITEMS));
    }
    return Promise.resolve(jsonResponse([], 200));
  });
}

afterEach(cleanup);
beforeEach(() => fetchMock.mockReset());

describe("TicketDetail hierarchy rail (ISI-5312)", () => {
  it("lists sub-ticket quick links with status pills on a parent ticket", async () => {
    routeFetch("wi-1");
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    const links = await screen.findByTestId("detail-subticket-links");
    const rows = within(links).getAllByTestId("detail-subticket-link");
    expect(rows).toHaveLength(2);

    // Each quick link carries the child title + its status pill, and deep-links to
    // the child ticket (project id percent-encoded in the href).
    const a = rows[0];
    expect(within(a).getByText("sub a")).toBeTruthy();
    expect(within(a).getByText("Done")).toBeTruthy();
    expect(a.getAttribute("href")).toBe("/projects/ns%2Fdemo/issues/wi-2");

    const b = rows[1];
    expect(within(b).getByText("sub b")).toBeTruthy();
    expect(within(b).getByText("Todo")).toBeTruthy();
    expect(b.getAttribute("href")).toBe("/projects/ns%2Fdemo/issues/wi-3");

    // A parent (root) ticket keeps the honest em-dash in the Parent row.
    expect(screen.getByTestId("prop-parent").textContent).toContain("—");
  });

  it("shows a parent quick link with status on a sub-ticket", async () => {
    routeFetch("wi-2");
    render(<TicketDetail projectId="ns/demo" workItemId="wi-2" />);

    const parent = await screen.findByTestId("detail-parent-link");
    // The link lives in the Properties "Parent" row.
    expect(screen.getByTestId("prop-parent").contains(parent)).toBe(true);
    expect(within(parent).getByText("ship the thing")).toBeTruthy();
    expect(within(parent).getByText("In Review")).toBeTruthy();
    expect(parent.getAttribute("href")).toBe("/projects/ns%2Fdemo/issues/wi-1");

    // A leaf sub-ticket has no children of its own — the rail quick-links list is absent.
    await waitFor(() =>
      expect(screen.getByTestId("detail-subticket-summary").textContent).toContain(
        "0 of 0 done",
      ),
    );
    expect(screen.queryByTestId("detail-subticket-links")).toBeNull();
  });
});
