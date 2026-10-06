// test/tickets/ticketsScreen.subtickets.test.tsx — ISI-5547: sub-tickets surface
// in BOTH the Kanban and List views driven by the real TicketsScreen.
//
// Regression guard for the delegated-from-ISI-5544 defect: the board read returns
// every `source='board'` item in a Project — roots AND sub-tickets, each carrying
// its `parentId` up-edge — but the screen used to discard the children with
// `.filter(isRoot)`. With no child ever kept, the tree never learned a parent HAD
// children, so TicketTreeToggle (which only renders when the count > 0) showed no
// disclosure caret and the sub-tickets were unreachable in either view.
//
// These cases mount the WHOLE screen (not the view units — views.test.tsx already
// proves the views render a supplied tree) with a single board read returning a
// parent + its child, and assert:
//   • the child is NOT laid out as a top-level card/row (it is nested, not dropped
//     AND not duplicated at the root), yet
//   • the parent shows the caret + child-count badge, and expanding it reveals the
//     child — in the Kanban view and, after toggling, the List view.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent, within } from "@testing-library/react";
import { TicketsScreen } from "@/components/tickets/TicketsScreen";

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

// The repro parent (sympozium-todo-demo) with one sub-ticket in a DIFFERENT lane,
// so we also prove a child is placed by the tree (under its parent), never bucketed
// as its own root card.
const PARENT_ID = "dd9f8ff5-181d-4514-837a-66c53fcc6abb";
const CHILD_ID = "aaaaaaaa-0000-4000-8000-000000000001";

const BOARD = [
  {
    id: PARENT_ID,
    projectId: "p1",
    parentId: null,
    title: "Parent epic",
    state: "todo",
    blockedReason: null,
    labels: [],
    updatedAt: "2026-09-01T00:00:00Z",
  },
  {
    id: CHILD_ID,
    projectId: "p1",
    parentId: PARENT_ID,
    title: "Child sub-ticket",
    state: "in_progress",
    blockedReason: null,
    labels: [],
    updatedAt: "2026-09-02T00:00:00Z",
  },
];

// Route the reads TicketsScreen fires: the board list (full set, roots + children),
// /api/session (fetchViewerRole), and the /api/squad/overview live-run poll — the
// catch-all keeps the live set empty so the markers never interfere.
function routeBoard(items: unknown) {
  fetchMock.mockImplementation((url: string) => {
    const u = String(url);
    if (u.includes("/api/session")) {
      return Promise.resolve(jsonResponse({ globalRole: "user", username: "ada" }));
    }
    if (u.includes("/work-items")) return Promise.resolve(jsonResponse(items));
    return Promise.resolve(jsonResponse([]));
  });
}

describe("TicketsScreen sub-tickets surface in both views (ISI-5547)", () => {
  beforeEach(() => {
    fetchMock.mockReset();
    window.localStorage.clear();
    // Clear any ?view= the view-toggle wrote to the (shared, non-reset) jsdom URL in
    // a prior case, so each test starts from the default Kanban view.
    window.history.replaceState(null, "", "/");
  });
  afterEach(() => cleanup());

  it("Kanban: the child is nested (not a top-level card) and revealed on expand", async () => {
    routeBoard(BOARD);
    render(<TicketsScreen projectId="ns/demo" />);

    // Parent root lands in its lane; the child is NOT laid out as a top-level card.
    await screen.findByTestId(`card-${PARENT_ID}`);
    expect(screen.queryByTestId(`card-${CHILD_ID}`)).toBeNull();

    // The parent advertises its single child via the caret + count badge (8.17 AC1),
    // derived from the full board read — no `childCount` field, no extra fetch.
    const caret = screen.getByTestId(`tree-caret-${PARENT_ID}`);
    expect(screen.getByTestId(`child-count-${PARENT_ID}`)).toHaveTextContent("1");

    // Expanding reveals the child card, nested under the parent (same column).
    fireEvent.click(caret);
    const childCard = await screen.findByTestId(`card-${CHILD_ID}`);
    expect(childCard).toHaveTextContent("Child sub-ticket");
    const todoLane = screen.getByTestId("column-todo");
    expect(within(todoLane).getByTestId(`card-${CHILD_ID}`)).toBeInTheDocument();

    // No second board read was needed to reveal the child (children came from the
    // full set already in hand); only the initial list + session + overview ran.
    const listCalls = fetchMock.mock.calls.filter((c) => String(c[0]).includes("/work-items"));
    expect(listCalls).toHaveLength(1);
  });

  it("List: the child is nested (not a top-level row) and revealed on expand", async () => {
    routeBoard(BOARD);
    render(<TicketsScreen projectId="ns/demo" />);

    // Switch to the List view.
    const toList = await screen.findByTestId("view-toggle-list");
    fireEvent.click(toList);

    // Parent root row present; the child row is hidden until the parent expands.
    await screen.findByTestId(`row-${PARENT_ID}`);
    expect(screen.queryByTestId(`row-${CHILD_ID}`)).toBeNull();
    expect(screen.getByTestId(`child-count-${PARENT_ID}`)).toHaveTextContent("1");

    // Expanding the parent reveals the child as an indented sibling band (depth 1),
    // carrying its own status chip.
    fireEvent.click(screen.getByTestId(`tree-caret-${PARENT_ID}`));
    const childRow = await screen.findByTestId(`row-${CHILD_ID}`);
    expect(childRow.getAttribute("data-tree-depth")).toBe("1");
    expect(within(childRow).getByTestId(`row-state-${CHILD_ID}`)).toHaveTextContent("In Progress");
  });

  it("a leaf root (no children) shows no caret — the tree index only carets real parents", async () => {
    routeBoard([BOARD[0]]); // parent alone, child filtered out of the read
    render(<TicketsScreen projectId="ns/demo" />);

    await screen.findByTestId(`card-${PARENT_ID}`);
    expect(screen.queryByTestId(`tree-caret-${PARENT_ID}`)).toBeNull();
  });
});
