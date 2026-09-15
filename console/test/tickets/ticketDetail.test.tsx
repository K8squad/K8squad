// test/tickets/ticketDetail.test.tsx — the ticket-detail screen (ISI-4399 S3 /
// ISI-4447 S1 redesign): it renders the thread (header/description/activity/
// sub-tickets) from the two reads it owns, degrades honestly on a 404, and now
// (ISI-4454) hosts the LIVE human comment composer — contributor+ posts with an
// optimistic append + reconciling re-fetch, a viewer stays read-only, and a
// 404/501 from the endpoint keeps the honest "not wired here" gap (FR-I3).

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  render,
  screen,
  cleanup,
  waitFor,
  within,
  fireEvent,
} from "@testing-library/react";
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

const POSTED_COMMENT = {
  author: "user:me",
  body: "hello agents",
  createdAt: "2026-09-14T10:10:00Z",
};

/**
 * Route the stubbed fetch. `role` drives /api/session (fetchViewerRole); a POST to
 * …/comments returns `postStatus` (default 201 POSTED_COMMENT). The thread GET is
 * STATEFUL: once a 201 post lands it returns THREAD + the posted comment, mirroring
 * the real backend so the reconciling re-fetch keeps (not drops) the new comment.
 */
function routeFetch(opts?: {
  threadStatus?: number;
  role?: string;
  postStatus?: number;
}) {
  const threadStatus = opts?.threadStatus ?? 200;
  const role = opts?.role ?? "viewer";
  const postStatus = opts?.postStatus ?? 201;
  let posted = false;
  fetchMock.mockImplementation((url: string, init?: RequestInit) => {
    const u = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    if (u.includes("/api/session")) {
      return Promise.resolve(jsonResponse({ role }));
    }
    if (u.includes("/api/work-items/") && u.includes("/comments") && method === "POST") {
      if (postStatus === 201) posted = true;
      return Promise.resolve(
        postStatus === 201
          ? jsonResponse(POSTED_COMMENT, 201)
          : jsonResponse({ error: "x" }, postStatus),
      );
    }
    if (u.includes("/api/work-items/")) {
      if (threadStatus !== 200) {
        return Promise.resolve(jsonResponse({ error: "x" }, threadStatus));
      }
      const body = posted
        ? { ...THREAD, Comments: [...THREAD.Comments, POSTED_COMMENT] }
        : THREAD;
      return Promise.resolve(jsonResponse(body));
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

    // A viewer (default fail-closed role) sees the read-only composer + the
    // honest trace-pending surface (no faked writes).
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
    routeFetch({ threadStatus: 404 });
    render(<TicketDetail projectId="ns/demo" workItemId="missing" />);
    await waitFor(() => expect(screen.getByTestId("detail-unavailable")).toBeTruthy());
    expect(screen.queryByTestId("detail-description")).toBeNull();
  });

  it("shows an error state (not a blank page) on a 500", async () => {
    routeFetch({ threadStatus: 500 });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await waitFor(() => expect(screen.getByTestId("detail-error")).toBeTruthy());
  });

  it("lets a contributor post a comment (optimistic append + reconciling refetch)", async () => {
    routeFetch({ role: "contributor" });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    // Contributor gets the LIVE composer, not the read-only surface.
    await waitFor(() => expect(screen.getByTestId("detail-composer")).toBeTruthy());
    expect(screen.queryByTestId("detail-composer-disabled")).toBeNull();

    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "hello agents" },
    });
    fireEvent.click(screen.getByTestId("detail-composer-submit"));

    // The posted comment appears and survives the reconciling re-fetch (3 total).
    await waitFor(() =>
      expect(screen.getAllByTestId("activity-comment")).toHaveLength(3),
    );
    expect(screen.getByText("hello agents")).toBeTruthy();
    // Input cleared after a successful post.
    expect(
      (screen.getByTestId("detail-composer-input") as HTMLTextAreaElement).value,
    ).toBe("");
  });

  it("keeps the honest gap copy when the endpoint isn't wired (POST 404)", async () => {
    routeFetch({ role: "contributor", postStatus: 404 });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    await waitFor(() => expect(screen.getByTestId("detail-composer")).toBeTruthy());
    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "hello agents" },
    });
    fireEvent.click(screen.getByTestId("detail-composer-submit"));

    // Falls back to the read-only surface with honest "not wired here" copy.
    await waitFor(() =>
      expect(screen.getByTestId("detail-composer-disabled")).toBeTruthy(),
    );
    expect(screen.getByText(/isn.t wired here/)).toBeTruthy();
    // No fabricated comment was appended.
    expect(screen.getAllByTestId("activity-comment")).toHaveLength(2);
  });

  it("surfaces an inline error on a non-404 4xx and keeps the composer usable", async () => {
    routeFetch({ role: "contributor", postStatus: 400 });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    await waitFor(() => expect(screen.getByTestId("detail-composer")).toBeTruthy());
    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "hello agents" },
    });
    fireEvent.click(screen.getByTestId("detail-composer-submit"));

    await waitFor(() =>
      expect(screen.getByTestId("detail-composer-error")).toBeTruthy(),
    );
    // Composer stays live so the caller can retry (not swapped to read-only).
    expect(screen.getByTestId("detail-composer")).toBeTruthy();
    expect(screen.getAllByTestId("activity-comment")).toHaveLength(2);
  });
});
