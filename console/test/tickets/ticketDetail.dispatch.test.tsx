// test/tickets/ticketDetail.dispatch.test.tsx — ISI-4881 (S3 of ISI-4853).
//
// The "run is on its way" wiring on the ticket-detail screen: on a Composer dispatch
// 200, submitAssign seeds useDispatchWatch (ISI-4879) so the DispatchPendingCard
// (ISI-4880) mounts at the stream head AND the composer status line renders — the board's
// OQ1 answer (surface = BOTH). The line auto-dismisses the instant the ladder advances
// past "queued" (a Run row is observed). On a NON-200 dispatch nothing is seeded and the
// existing role="alert" error path stands. The existing optimistic-append + re-assign
// path is unchanged (regression).
//
// This is an INTEGRATION test over the real TicketDetail + Composer + hook + card; only
// the network (fetch) and the SSE transport (EventSource, absent in jsdom) are stubbed.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import { TicketDetail } from "@/components/tickets/TicketDetail";

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

// jsdom has no EventSource; the hook opens one the instant a runId is discovered. This
// no-op stub lets that happen without a throw — the picking_up transition here rides the
// poll's phase snapshot, not SSE, so the stub never needs to emit.
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

const THREAD: Record<string, unknown> = {
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

const SQUAD = [
  { id: "ag-1", name: "agent:builder" },
  { id: "ag-2", name: "agent:reviewer" },
];

const POSTED_COMMENT = {
  author: "user:me",
  body: "go go go",
  createdAt: "2026-09-24T10:00:00Z",
};

/**
 * Stubs every read/write the detail screen makes. `dispatchStatus` toggles the
 * dispatch outcome; `runs` is the array `GET /api/runs` returns (the OQ4 discovery
 * bridge) — default [] so the ladder stays "queued".
 */
function routeFetch(opts?: { dispatchStatus?: number; runs?: unknown[] }) {
  const dispatchStatus = opts?.dispatchStatus ?? 200;
  const runs = opts?.runs ?? [];
  let posted = false;
  fetchMock.mockImplementation((url: string, init?: RequestInit) => {
    const u = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    if (u.includes("/api/session")) {
      return Promise.resolve(jsonResponse({ globalRole: "contributor" }));
    }
    if (u.includes("/api/squad/agents")) {
      return Promise.resolve(jsonResponse({ agents: SQUAD }));
    }
    if (u.includes("/api/runs")) {
      return Promise.resolve(jsonResponse(runs));
    }
    if (u.includes("/comments") && method === "POST") {
      posted = true;
      return Promise.resolve(jsonResponse(POSTED_COMMENT, 201));
    }
    if (u.includes("/dispatch") && method === "POST") {
      return Promise.resolve(
        dispatchStatus === 200
          ? jsonResponse({
              workItemId: "wi-1",
              fromState: "backlog",
              toState: "todo",
              requestedAgent: "agent:reviewer",
            })
          : jsonResponse({ error: "x" }, dispatchStatus),
      );
    }
    if (u.includes("/api/work-items/")) {
      const body = posted
        ? { ...THREAD, Comments: [POSTED_COMMENT] }
        : THREAD;
      return Promise.resolve(jsonResponse(body));
    }
    if (u.includes("/work-items")) return Promise.resolve(jsonResponse([]));
    return Promise.resolve(jsonResponse([]));
  });
}

/** Fill the composer + pick the agent + click Comment & assign. */
async function commentAndAssign() {
  await waitFor(() => expect(screen.getByTestId("detail-composer")).toBeTruthy());
  await waitFor(() =>
    expect(screen.getAllByRole("option", { name: "agent:reviewer" }).length).toBeGreaterThan(0),
  );
  fireEvent.change(screen.getByTestId("detail-composer-input"), {
    target: { value: "go go go" },
  });
  fireEvent.change(screen.getByTestId("detail-composer-assignee"), {
    target: { value: "agent:reviewer" },
  });
  fireEvent.click(screen.getByTestId("detail-composer-submit-assign"));
}

afterEach(cleanup);
beforeEach(() => fetchMock.mockReset());

describe("TicketDetail — dispatch signal wiring (ISI-4881 / S3)", () => {
  it("AC1+AC2: on a dispatch 200, the placeholder card + queued status line appear", async () => {
    routeFetch(); // runs=[] → ladder pins at queued (no Run row yet)
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await commentAndAssign();

    // The card lands in the stream, before any Run row exists…
    await waitFor(() =>
      expect(screen.getByTestId("dispatch-pending-card")).toBeInTheDocument(),
    );
    expect(screen.getByTestId("dispatch-pending-card")).toHaveAttribute(
      "data-state",
      "queued",
    );
    expect(screen.getByTestId("dispatch-label")).toHaveTextContent("Queued");

    // …and the composer status line names the agent, with role="status".
    const line = screen.getByTestId("detail-dispatch-status");
    expect(line).toHaveAttribute("role", "status");
    expect(line).toHaveTextContent("Dispatched to agent:reviewer");
    expect(line).toHaveTextContent(/waiting for the operator to start the run/);
  });

  it("AC2: the status line auto-dismisses once the card goes live (state ≥ picking_up)", async () => {
    // The discovery poll finds a Pending Run row for this work item → picking_up.
    routeFetch({
      runs: [
        { id: "run-x", phase: "Pending", workItemRef: "wi-1", agents: ["agent:reviewer"] },
      ],
    });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await commentAndAssign();

    // The card advances off "queued"…
    await waitFor(() =>
      expect(screen.getByTestId("dispatch-pending-card")).toHaveAttribute(
        "data-state",
        "picking_up",
      ),
    );
    expect(screen.getByTestId("dispatch-label")).toHaveTextContent("Picking up…");
    // …and the composer status line is gone (it only lives while queued).
    expect(screen.queryByTestId("detail-dispatch-status")).toBeNull();
  });

  it("AC4: a non-200 dispatch seeds NO card/line — the role=alert error path stands", async () => {
    routeFetch({ dispatchStatus: 403 });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await commentAndAssign();

    // The assign half's inline error appears (role=alert)…
    await waitFor(() =>
      expect(screen.getByTestId("detail-composer-assign-error")).toBeInTheDocument(),
    );
    expect(screen.getByTestId("detail-composer-assign-error")).toHaveAttribute(
      "role",
      "alert",
    );
    // …and NOTHING was seeded: no card, no status line.
    expect(screen.queryByTestId("dispatch-pending-card")).toBeNull();
    expect(screen.queryByTestId("detail-dispatch-status")).toBeNull();
  });

  it("regression: the comment still optimistically appends on assign, card notwithstanding", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await commentAndAssign();

    // The human's comment survives (optimistic append + reconciling re-fetch) …
    await waitFor(() => expect(screen.getByText("go go go")).toBeInTheDocument());
    // … and the dispatch POST carried the picked agent NAME (wire contract unchanged).
    const dispatchCall = fetchMock.mock.calls.find(
      ([u, init]) =>
        String(u).includes("/dispatch") &&
        (init?.method ?? "GET").toUpperCase() === "POST",
    );
    expect(dispatchCall?.[1]?.body).toBe(JSON.stringify({ agentId: "agent:reviewer" }));
  });

  it("regression: a plain Comment (no assign) never seeds a dispatch card", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await waitFor(() => expect(screen.getByTestId("detail-composer")).toBeTruthy());
    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "just a comment" },
    });
    fireEvent.click(screen.getByTestId("detail-composer-submit"));

    await waitFor(() => expect(screen.getByText("just a comment")).toBeInTheDocument());
    expect(screen.queryByTestId("dispatch-pending-card")).toBeNull();
    expect(screen.queryByTestId("detail-dispatch-status")).toBeNull();
  });
});
