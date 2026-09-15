// test/tickets/ticketDetail.test.tsx — the ticket-detail screen (ISI-4399 S3):
// it renders the thread (header/description/activity/sub-tickets) from the two
// reads it owns, degrades honestly on a 404, and shows the deferred write/trace
// surfaces (disabled composer + trace-pending) rather than faking them.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup, waitFor, within } from "@testing-library/react";
import { TicketDetail } from "@/components/tickets/TicketDetail";

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

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
  State: "in_review",
  BlockedReason: "",
  Comments: [
    { author: "agent:builder", body: "seam wired", createdAt: "2026-09-14T10:00:00Z" },
    { author: "user:alice", body: "looks good", createdAt: "2026-09-14T10:05:00Z" },
  ],
  ChangeRefs: [
    { kind: "commit", ref: "deadbeef", author: "agent:builder", createdAt: "2026-09-14T10:02:00Z" },
  ],
  Holder: "agent:builder",
  RunID: "run-9",
  statusHistory: [
    { fromState: "in_progress", toState: "in_review", principal: "agent:builder", occurredAt: "2026-09-14T10:01:00Z" },
  ],
};

const CHILDREN = [
  { id: "wi-2", projectId: "ns/demo", parentId: "wi-1", title: "sub a", state: "done", blockedReason: null, updatedAt: "2026-09-14T00:00:00Z" },
  { id: "wi-3", projectId: "ns/demo", parentId: "wi-1", title: "sub b", state: "todo", blockedReason: null, updatedAt: "2026-09-14T00:00:00Z" },
];

function routeFetch(threadStatus = 200) {
  fetchMock.mockImplementation((url: string) => {
    const u = String(url);
    if (u.includes("/api/work-items/")) {
      return Promise.resolve(
        threadStatus === 200 ? jsonResponse(THREAD) : jsonResponse({ error: "x" }, threadStatus),
      );
    }
    if (u.includes("/work-items")) return Promise.resolve(jsonResponse(CHILDREN));
    return Promise.resolve(jsonResponse([], 200));
  });
}

afterEach(cleanup);
beforeEach(() => fetchMock.mockReset());

describe("TicketDetail", () => {
  it("renders the ticket header, description, activity and sub-tickets", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    await waitFor(() => expect(screen.getByTestId("detail-description")).toBeTruthy());

    expect(screen.getByText("ship the thing")).toBeTruthy();
    expect(within(screen.getByTestId("detail-description")).getByText("the full description")).toBeTruthy();

    // Activity: two comments + one event + one change, newest last.
    expect(screen.getAllByTestId("activity-comment")).toHaveLength(2);
    expect(screen.getByTestId("activity-event")).toBeTruthy();
    expect(screen.getByTestId("activity-change")).toBeTruthy();
    // Role pills differentiate agent vs user authors.
    const roles = screen.getAllByTestId("activity-role").map((n) => n.textContent);
    expect(roles).toContain("agent");
    expect(roles).toContain("user");

    // Sub-tickets progress "1 of 2 done".
    expect(screen.getByTestId("detail-subtickets-progress").textContent).toContain("1 of 2 done");
    expect(screen.getAllByTestId("detail-subticket")).toHaveLength(2);

    // Deferred surfaces are present but honest (not faked writes).
    expect(screen.getByTestId("detail-composer-disabled")).toBeTruthy();
    expect(screen.getByTestId("detail-trace-pending")).toBeTruthy();
    // Run id shown in the sidebar.
    expect(within(screen.getByTestId("detail-run-trace")).getByText("run-9")).toBeTruthy();
  });

  it("renders the newest agent comment as a GitHub-style run bubble (S3 anatomy)", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    await waitFor(() => expect(screen.getByTestId("detail-description")).toBeTruthy());

    // The agent comment carries the run meta strip: run-id + a LIVE status dot
    // (the fixture ticket is in_review — an in-flight Review phase — so the run
    // pulses) + a trace ribbon deep-linking to the internal Run-detail surface.
    const meta = screen.getByTestId("runcomment-meta");
    expect(within(meta).getByText("run-9")).toBeTruthy();
    expect(screen.getByTestId("runcomment-status").getAttribute("data-live")).toBe("true");
    const trace = screen.getByTestId("runcomment-trace");
    expect(trace.getAttribute("href")).toBe("/runs/run-9");
    expect(trace.textContent).toContain("View trace");

    // The human reply is a bubble too, but carries NO run meta (never fabricated).
    expect(screen.getAllByTestId("runcomment-meta")).toHaveLength(1);
  });

  it("lays out the ISI-4447 redesign: header card + pinned rail with Properties and Sub-tickets status", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    await waitFor(() => expect(screen.getByTestId("detail-header")).toBeTruthy());

    // Header + Description live in one card at the top of the main column.
    const header = screen.getByTestId("detail-header");
    expect(within(header).getByText("ship the thing")).toBeTruthy();
    expect(within(header).getByTestId("detail-description")).toBeTruthy();

    // Right-rail Properties expose all seven fields; the ones the M1.5 read model
    // doesn't carry yet render an honest em-dash, never a fabricated value.
    expect(within(screen.getByTestId("prop-project")).getByText("ns/demo")).toBeTruthy();
    for (const id of ["prop-priority", "prop-workmode", "prop-parent", "prop-labels"]) {
      expect(screen.getByTestId(id).textContent).toContain("—");
    }

    // Sub-tickets STATUS card (rail): roll-up + progress bar + three count tiles.
    const statusCard = screen.getByTestId("detail-subticket-status");
    expect(within(statusCard).getByTestId("detail-subticket-summary").textContent).toContain(
      "1 of 2 done",
    );
    const bar = within(statusCard).getByTestId("detail-progressbar");
    expect(bar.getAttribute("aria-valuenow")).toBe("1");
    expect(bar.getAttribute("aria-valuemax")).toBe("2");
    expect(within(screen.getByTestId("detail-count-done")).getByText("1")).toBeTruthy();
    expect(within(screen.getByTestId("detail-count-inprogress")).getByText("0")).toBeTruthy();
    expect(within(screen.getByTestId("detail-count-todo")).getByText("1")).toBeTruthy();
    expect(screen.getByTestId("detail-add-subticket")).toBeTruthy();
  });

  it("degrades honestly to not-available on a 404", async () => {
    routeFetch(404);
    render(<TicketDetail projectId="ns/demo" workItemId="missing" />);
    await waitFor(() => expect(screen.getByTestId("detail-unavailable")).toBeTruthy());
    expect(screen.queryByTestId("detail-description")).toBeNull();
  });

  it("shows an error state (not a blank page) on a 500", async () => {
    routeFetch(500);
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await waitFor(() => expect(screen.getByTestId("detail-error")).toBeTruthy());
  });
});
