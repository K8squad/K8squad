// test/tickets/ticketDetail.dispatch.degrade.test.tsx — ISI-4883 (S5 of ISI-4853).
//
// QA / degrade & honesty verification. Where S3's ticketDetail.dispatch.test.tsx proves the
// WIRING (card + status line seed on a dispatch 200), this drives the SAME fully-integrated
// TicketDetail — real useDispatchWatch (S1) + DispatchPendingCard (S2) + composer/rail (S3/S4) —
// through WALL-CLOCK time and terminal resolution. The three S5 acceptance criteria, verified at
// the integration boundary (not just the hook/unit boundary the dev tests already cover):
//
//   AC-1  operator-slow: no Run row ever appears → soft note at ~15s, stalled at ~45s
//         (board-locked thresholds, interaction f3d5c62e).
//   AC-2  honesty: a run that reaches Claiming but not Running NEVER shows "Working…".
//   AC-3  terminal: a Failed/Succeeded run resolves the card to a STATIC terminal state and
//         stops all timers/subscriptions (no zombie transitions, no leaked poll).
//
// Fake-timer strategy: `shouldAdvanceTime: true` lets real time drive React/RTL microtasks and
// `waitFor` (advancing the fake clock only microscopically), while `advanceTimersByTimeAsync`
// jumps the clock across the 15s / 45s degrade thresholds and flushes the async discovery poll.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent, act } from "@testing-library/react";
import { TicketDetail } from "@/components/tickets/TicketDetail";
import {
  DISPATCH_SOFT_MS,
  DISPATCH_STALLED_MS,
} from "@/lib/tickets/useDispatchWatch";

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

// jsdom has no EventSource; the hook opens one the instant a runId is discovered. A no-op stub
// lets that happen without a throw — every honesty/terminal transition below rides the poll's
// phase snapshot, not SSE, so the stub never needs to emit.
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

/** Count of `GET /api/runs` discovery polls issued so far (AC-3 leak check). */
function runsPollCount(): number {
  return fetchMock.mock.calls.filter(([u]) => String(u).includes("/api/runs"))
    .length;
}

/**
 * Stubs every read/write the detail screen makes. `runs` is the array `GET /api/runs` returns
 * (the OQ4 discovery bridge) — default [] so the ladder stays "queued" forever (operator-slow).
 */
function routeFetch(opts?: { runs?: unknown[] }) {
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
        jsonResponse({
          workItemId: "wi-1",
          fromState: "backlog",
          toState: "todo",
          requestedAgent: "agent:reviewer",
        }),
      );
    }
    if (u.includes("/api/work-items/")) {
      const body = posted ? { ...THREAD, Comments: [POSTED_COMMENT] } : THREAD;
      return Promise.resolve(jsonResponse(body));
    }
    if (u.includes("/work-items")) return Promise.resolve(jsonResponse([]));
    return Promise.resolve(jsonResponse([]));
  });
}

/** Fill the composer + pick the agent + click Comment & assign (dispatch 200). */
async function commentAndAssign() {
  await waitFor(() => expect(screen.getByTestId("detail-composer")).toBeTruthy());
  await waitFor(() =>
    expect(
      screen.getAllByRole("option", { name: "agent:reviewer" }).length,
    ).toBeGreaterThan(0),
  );
  fireEvent.change(screen.getByTestId("detail-composer-input"), {
    target: { value: "go go go" },
  });
  fireEvent.change(screen.getByTestId("detail-composer-assignee"), {
    target: { value: "agent:reviewer" },
  });
  fireEvent.click(screen.getByTestId("detail-composer-submit-assign"));
}

beforeEach(() => {
  fetchMock.mockReset();
  vi.useFakeTimers({ shouldAdvanceTime: true });
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("TicketDetail — degrade & honesty (ISI-4883 / S5)", () => {
  it("AC-1: operator-slow (no Run row) → soft note ~15s → stalled note ~45s, through the real screen", async () => {
    routeFetch({ runs: [] }); // the discovery poll never finds a row → ladder pins at queued
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await commentAndAssign();

    // The placeholder card lands at the stream head, queued, with NO degrade note yet.
    await waitFor(() =>
      expect(screen.getByTestId("dispatch-pending-card")).toBeInTheDocument(),
    );
    expect(screen.getByTestId("dispatch-pending-card")).toHaveAttribute(
      "data-state",
      "queued",
    );
    expect(screen.queryByTestId("dispatch-degrade")).toBeNull();

    // Cross the soft threshold — the card surfaces the SOFT degrade note (still queued).
    await act(async () => {
      await vi.advanceTimersByTimeAsync(DISPATCH_SOFT_MS);
    });
    await waitFor(() =>
      expect(screen.getByTestId("dispatch-degrade")).toHaveTextContent(
        "Still waiting for the operator",
      ),
    );
    expect(screen.getByTestId("dispatch-pending-card")).toHaveAttribute(
      "data-state",
      "queued",
    );

    // Cross the stalled threshold — the note escalates to STALLED, card still honestly queued.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(DISPATCH_STALLED_MS - DISPATCH_SOFT_MS);
    });
    await waitFor(() =>
      expect(screen.getByTestId("dispatch-degrade")).toHaveTextContent(
        "Operator hasn't picked this up yet",
      ),
    );
    // Never fabricated progress: an operator-slow dispatch never claims to be working.
    expect(screen.getByTestId("dispatch-label")).not.toHaveTextContent("Working…");
  });

  it("AC-2: a run that reaches Claiming but not Running NEVER shows 'Working…'", async () => {
    // A Run row exists but is only Claiming — the honesty guard caps the card at "Picking up…".
    routeFetch({
      runs: [
        { id: "run-c", workItemRef: "wi-1", phase: "Claiming", agents: ["agent:reviewer"] },
      ],
    });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await commentAndAssign();

    await waitFor(() =>
      expect(screen.getByTestId("dispatch-pending-card")).toHaveAttribute(
        "data-state",
        "picking_up",
      ),
    );
    expect(screen.getByTestId("dispatch-label")).toHaveTextContent("Picking up…");

    // Let wall-clock run well past both degrade thresholds: Claiming must NOT drift to Working…,
    // and past-queued the degrade note is meaningless (never renders).
    await act(async () => {
      await vi.advanceTimersByTimeAsync(DISPATCH_STALLED_MS + 30_000);
    });
    expect(screen.getByTestId("dispatch-pending-card")).toHaveAttribute(
      "data-state",
      "picking_up",
    );
    expect(screen.getByTestId("dispatch-label")).not.toHaveTextContent("Working…");
    expect(screen.queryByTestId("dispatch-degrade")).toBeNull();
  });

  it("AC-3: a Failed run resolves the card to a static terminal state and stops all timers/subscriptions", async () => {
    routeFetch({
      runs: [
        { id: "run-f", workItemRef: "wi-1", phase: "Failed", agents: ["agent:reviewer"] },
      ],
    });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await commentAndAssign();

    await waitFor(() =>
      expect(screen.getByTestId("dispatch-pending-card")).toHaveAttribute(
        "data-state",
        "failed",
      ),
    );
    expect(screen.getByTestId("dispatch-label")).toHaveTextContent("Failed");
    expect(screen.getByTestId("dispatch-pending-card")).toHaveAttribute(
      "data-tone",
      "blocked",
    );

    // The discovery poll has done its job and handed off — snapshot its call count, then let a full
    // minute of wall-clock elapse. A terminal card must freeze: no state churn, no degrade note, and
    // NO further /api/runs polls (the subscription is torn down — no leaked interval).
    const pollsAtTerminal = runsPollCount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(screen.getByTestId("dispatch-pending-card")).toHaveAttribute(
      "data-state",
      "failed",
    );
    expect(screen.queryByTestId("dispatch-degrade")).toBeNull();
    expect(runsPollCount()).toBe(pollsAtTerminal);
  });

  it("AC-3: a Succeeded run resolves the card to the static 'Finished' terminal state", async () => {
    routeFetch({
      runs: [
        { id: "run-s", workItemRef: "wi-1", phase: "Succeeded", agents: ["agent:reviewer"] },
      ],
    });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await commentAndAssign();

    await waitFor(() =>
      expect(screen.getByTestId("dispatch-pending-card")).toHaveAttribute(
        "data-state",
        "succeeded",
      ),
    );
    expect(screen.getByTestId("dispatch-label")).toHaveTextContent("Finished");

    // Frozen terminal: time passing never regresses it or re-arms a degrade note.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(screen.getByTestId("dispatch-pending-card")).toHaveAttribute(
      "data-state",
      "succeeded",
    );
    expect(screen.queryByTestId("dispatch-degrade")).toBeNull();
  });
});
