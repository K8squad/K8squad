// test/tickets/ticketDetail.retrigger.test.tsx — ISI-4918 (S7 of ISI-4853).
//
// The PLAIN-comment re-trigger path: a normal "Comment" (NOT "Comment & assign") on a
// parked, unheld ticket in an engine lane re-dispatches it server-side (pkg/coord
// AppendHumanComment → reTriggered:true, moved → todo). S7 seeds the SAME honest ladder
// the explicit-assign path uses (ISI-4879/4880/4881), so the most common daily
// interaction — comment to nudge a ticket — finally gets the Queued → Picking up… →
// Working… signal instead of the old static one-liner + dead-air.
//
// INTEGRATION test over the real TicketDetail + Composer + useDispatchWatch + card;
// only fetch and the (absent-in-jsdom) EventSource transport are stubbed.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import { TicketDetail } from "@/components/tickets/TicketDetail";

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

class NoopEventSource {
  close() {}
  addEventListener() {}
  removeEventListener() {}
  onmessage: unknown = null;
  onerror: unknown = null;
  onopen: unknown = null;
}
vi.stubGlobal("EventSource", NoopEventSource as unknown as typeof EventSource);

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

// A parked, unheld ticket on an engine lane (in_progress) whose last/requested agent is
// agent:builder — the one Intake will mint the re-triggered Run for.
const THREAD: Record<string, unknown> = {
  WorkItemID: "wi-1",
  Title: "ship the thing",
  Description: "the full description",
  State: "in_progress",
  BlockedReason: "",
  Comments: [],
  ChangeRefs: [],
  Holder: "",
  RunID: "",
  RequestedAgent: "agent:builder",
  statusHistory: [],
};

// The comment 201 the apiserver returns when the comment RE-DISPATCHED the ticket.
const RETRIGGER_COMMENT = {
  author: "user:me",
  body: "nudge nudge",
  createdAt: "2026-09-25T10:00:00Z",
  reTriggered: true,
  fromState: "in_progress",
  toState: "todo",
};

// A plain comment that did NOT re-trigger (backlog/todo, or live-holder guard).
const PLAIN_COMMENT = {
  author: "user:me",
  body: "nudge nudge",
  createdAt: "2026-09-25T10:00:00Z",
};

/**
 * Stubs the reads/writes the detail screen makes. `comment` is the 201 body the
 * comment POST returns (toggles reTriggered); `runs` is what GET /api/runs returns
 * (default [] → the ladder pins at queued, no Run row yet).
 */
function routeFetch(opts?: { comment?: unknown; runs?: unknown[] }) {
  const comment = opts?.comment ?? RETRIGGER_COMMENT;
  const runs = opts?.runs ?? [];
  let posted = false;
  fetchMock.mockImplementation((url: string, init?: RequestInit) => {
    const u = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    if (u.includes("/api/session")) {
      return Promise.resolve(jsonResponse({ globalRole: "contributor" }));
    }
    if (u.includes("/api/squad/agents")) {
      return Promise.resolve(jsonResponse({ agents: [] }));
    }
    if (u.includes("/api/runs")) {
      return Promise.resolve(jsonResponse(runs));
    }
    if (u.includes("/comments") && method === "POST") {
      posted = true;
      return Promise.resolve(jsonResponse(comment, 201));
    }
    if (u.includes("/api/work-items/")) {
      const body = posted ? { ...THREAD, Comments: [PLAIN_COMMENT] } : THREAD;
      return Promise.resolve(jsonResponse(body));
    }
    if (u.includes("/work-items")) return Promise.resolve(jsonResponse([]));
    return Promise.resolve(jsonResponse([]));
  });
}

/** Fill the composer + click the PLAIN "Comment" button (never the assign pair). */
async function plainComment() {
  await waitFor(() => expect(screen.getByTestId("detail-composer")).toBeTruthy());
  fireEvent.change(screen.getByTestId("detail-composer-input"), {
    target: { value: "nudge nudge" },
  });
  fireEvent.click(screen.getByTestId("detail-composer-submit"));
}

afterEach(cleanup);
beforeEach(() => fetchMock.mockReset());

describe("TicketDetail — plain-comment re-trigger ladder (ISI-4918 / S7)", () => {
  it("AC1: a plain comment that re-dispatches seeds the full ladder (queued card + status line)", async () => {
    routeFetch(); // runs=[] → ladder pins at queued
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await plainComment();

    await waitFor(() =>
      expect(screen.getByTestId("dispatch-pending-card")).toBeInTheDocument(),
    );
    expect(screen.getByTestId("dispatch-pending-card")).toHaveAttribute(
      "data-state",
      "queued",
    );
    expect(screen.getByTestId("dispatch-label")).toHaveTextContent("Queued");

    const line = screen.getByTestId("detail-dispatch-status");
    expect(line).toHaveAttribute("role", "status");
    expect(line).toHaveTextContent("Dispatched to agent:builder");
  });

  it("AC2: the ladder card names the re-triggered agent (thread requested/last-run agent)", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await plainComment();

    const card = await screen.findByTestId("dispatch-pending-card");
    expect(card).toHaveTextContent("agent:builder");
  });

  it("AC1: the ladder advances to picking_up when the re-triggered Run row appears", async () => {
    routeFetch({
      runs: [
        { id: "run-x", phase: "Pending", workItemRef: "wi-1", agents: ["agent:builder"] },
      ],
    });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await plainComment();

    await waitFor(() =>
      expect(screen.getByTestId("dispatch-pending-card")).toHaveAttribute(
        "data-state",
        "picking_up",
      ),
    );
    expect(screen.getByTestId("dispatch-label")).toHaveTextContent("Picking up…");
  });

  // ISI-4918 follow-up, found in LIVE verification: a nudged ticket carries run rows from its
  // PREVIOUS dispatch. Matching one collapsed the ladder straight to the old run's "Finished"
  // while the new run was still minting — the discovery must ignore rows that predate the nudge.
  it("AC1: a previous dispatch's SUCCEEDED run row never collapses the ladder to Finished", async () => {
    routeFetch({
      runs: [
        {
          id: "run-prev",
          phase: "Succeeded",
          workItemRef: "wi-1",
          agents: ["agent:builder"],
          startedAt: "2026-01-01T00:00:00Z",
          endedAt: "2026-01-01T00:01:00Z",
        },
      ],
    });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await plainComment();

    await waitFor(() =>
      expect(screen.getByTestId("dispatch-pending-card")).toHaveAttribute(
        "data-state",
        "queued",
      ),
    );
    // Give the poll a beat — the stale row must keep being ignored, not "discovered".
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.getByTestId("dispatch-pending-card")).toHaveAttribute(
      "data-state",
      "queued",
    );
    expect(screen.getByTestId("dispatch-label")).toHaveTextContent("Queued");
  });

  it("AC3: no static '▶ Agent re-triggered' note competes with the ladder", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await plainComment();

    await screen.findByTestId("dispatch-pending-card");
    expect(screen.queryByTestId("detail-retrigger-note")).toBeNull();
    expect(screen.queryByText(/Agent re-triggered/)).toBeNull();
  });

  it("AC4: a plain comment that did NOT re-trigger seeds no ladder", async () => {
    routeFetch({ comment: PLAIN_COMMENT });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await plainComment();

    // The comment still lands…
    await waitFor(() => expect(screen.getByText("nudge nudge")).toBeInTheDocument());
    // …but nothing was seeded (reTriggered=false).
    expect(screen.queryByTestId("dispatch-pending-card")).toBeNull();
    expect(screen.queryByTestId("detail-dispatch-status")).toBeNull();
  });
});
