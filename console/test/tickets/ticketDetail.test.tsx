// test/tickets/ticketDetail.test.tsx — the ticket-detail screen (ISI-4399 S3 /
// ISI-4447 S1 redesign): it renders the thread (header/description/activity/
// sub-tickets) from the two reads it owns, degrades honestly on a 404, and now
// (ISI-4454) hosts the LIVE human comment composer — contributor+ posts with an
// optimistic append + reconciling re-fetch, a viewer stays read-only, and a
// 404/501 from the endpoint keeps the honest "not wired here" gap (FR-I3).
// ISI-4579 (ISI-4567 §2.3): the composer's Comment-&-assign chains the comment
// POST with the dispatch POST sequentially — an assign 403 keeps the comment +
// mirrors the rail error inline, an assign 501 latches just the assign button.

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

const THREAD: Record<string, unknown> = {
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

/** Squad roster (fleetlist.go FleetAgentList shape) for the rail AssigneeControl. */
const SQUAD = [
  { id: "ag-1", name: "agent:builder" },
  { id: "ag-2", name: "agent:reviewer" },
];

/** A backlog ticket — unheld, never dispatched. */
const BACKLOG_THREAD = { ...THREAD, State: "backlog", Holder: "", RunID: "" };

/** An unclaimed todo ticket already carrying a requested agent (ISI-4567 §1/§2.2). */
const TODO_THREAD = {
  ...THREAD,
  State: "todo",
  Holder: "",
  RunID: "",
  RequestedAgent: "agent:builder",
};

/** A mid-flight ticket — custody held, rail must stay read-only. */
const IN_PROGRESS_THREAD = { ...THREAD, State: "in_progress" };

const POSTED_COMMENT = {
  author: "user:me",
  body: "hello agents",
  createdAt: "2026-09-14T10:10:00Z",
};

/**
 * Route the stubbed fetch. `role` drives /api/session (fetchViewerRole); a POST to
 * …/comments returns `postStatus` (default 201 POSTED_COMMENT) and a POST to
 * …/dispatch returns `dispatchStatus` (default 200, ISI-4579). The thread GET is
 * STATEFUL: once a 201 post lands it returns THREAD + the posted comment, and once
 * a dispatch POST lands it returns the swapped RequestedAgent — mirroring the real
 * backend so the reconciling re-fetch keeps (not drops) server truth.
 */
function routeFetch(opts?: {
  thread?: Record<string, unknown>;
  threadStatus?: number;
  role?: string;
  postStatus?: number;
  dispatchStatus?: number;
}) {
  const threadBody = opts?.thread ?? THREAD;
  const baseComments = Array.isArray(threadBody.Comments) ? threadBody.Comments : [];
  const threadStatus = opts?.threadStatus ?? 200;
  const role = opts?.role ?? "viewer";
  const postStatus = opts?.postStatus ?? 201;
  const dispatchStatus = opts?.dispatchStatus ?? 200;
  let posted = false;
  let requestedAgent =
    typeof threadBody.RequestedAgent === "string" ? threadBody.RequestedAgent : null;
  fetchMock.mockImplementation((url: string, init?: RequestInit) => {
    const u = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    if (u.includes("/api/session")) {
      // /auth/me carries the caller's role as `globalRole` (ISI-4496), not `role`.
      return Promise.resolve(jsonResponse({ globalRole: role }));
    }
    if (u.includes("/api/squad/agents")) {
      return Promise.resolve(jsonResponse({ agents: SQUAD }));
    }
    if (u.includes("/api/work-items/") && u.includes("/comments") && method === "POST") {
      if (postStatus === 201) posted = true;
      return Promise.resolve(
        postStatus === 201
          ? jsonResponse(POSTED_COMMENT, 201)
          : jsonResponse({ error: "x" }, postStatus),
      );
    }
    if (u.includes("/api/work-items/") && u.includes("/dispatch") && method === "POST") {
      const body = JSON.parse(String(init?.body ?? "{}")) as { agentId?: string };
      if (dispatchStatus === 200 && typeof body.agentId === "string") {
        requestedAgent = body.agentId;
      }
      return Promise.resolve(
        dispatchStatus === 200
          ? jsonResponse({
              workItemId: threadBody.WorkItemID,
              fromState: threadBody.State,
              toState: threadBody.State,
              requestedAgent,
            })
          : jsonResponse({ error: "x" }, dispatchStatus),
      );
    }
    if (u.includes("/api/work-items/")) {
      if (threadStatus !== 200) {
        return Promise.resolve(jsonResponse({ error: "x" }, threadStatus));
      }
      const body = posted
        ? { ...threadBody, Comments: [...baseComments, POSTED_COMMENT] }
        : threadBody;
      return Promise.resolve(
        jsonResponse(requestedAgent ? { ...body, RequestedAgent: requestedAgent } : body),
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

  // ---- Composer Comment-&-assign (ISI-4567 §2.3 / ISI-4579) ----

  it("Comment & assign chains the comment POST then the dispatch POST, in order", async () => {
    routeFetch({ role: "contributor", thread: BACKLOG_THREAD });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    await waitFor(() => expect(screen.getByTestId("detail-composer")).toBeTruthy());
    // The composer's own roster select loads beside Comment (rail renders one
    // too — both draw from GET /api/squad/agents, hence TWO reviewer options).
    await waitFor(() =>
      expect(screen.getAllByRole("option", { name: "agent:reviewer" })).toHaveLength(2),
    );
    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "hello agents" },
    });
    fireEvent.change(screen.getByTestId("detail-composer-assignee"), {
      target: { value: "agent:reviewer" },
    });
    fireEvent.click(screen.getByTestId("detail-composer-submit-assign"));

    // The comment landed and survives the reconciling re-fetch (3 total).
    await waitFor(() =>
      expect(screen.getAllByTestId("activity-comment")).toHaveLength(3),
    );
    expect(screen.getByText("hello agents")).toBeTruthy();
    // Sequential, in order: comments POST strictly BEFORE dispatch POST, and
    // the dispatch carried the picked agent NAME (ISI-4501 wire contract).
    const seq = fetchMock.mock.calls.map(([u, init]) => ({
      u: String(u),
      method: (init?.method ?? "GET").toUpperCase(),
    }));
    const commentIdx = seq.findIndex(
      (c) => c.u.includes("/comments") && c.method === "POST",
    );
    const dispatchIdx = seq.findIndex(
      (c) => c.u.includes("/dispatch") && c.method === "POST",
    );
    expect(commentIdx).toBeGreaterThanOrEqual(0);
    expect(dispatchIdx).toBeGreaterThan(commentIdx);
    expect(fetchMock.mock.calls[dispatchIdx][1]?.body).toBe(
      JSON.stringify({ agentId: "agent:reviewer" }),
    );
    // The re-fetch surfaces the stamped requested agent on the rail select.
    await waitFor(() =>
      expect(
        (screen.getByTestId("detail-assignee-select") as HTMLSelectElement).value,
      ).toBe("agent:reviewer"),
    );
  });

  it("keeps the comment + inline error when the assign half 403s (never loses the text)", async () => {
    routeFetch({ role: "contributor", thread: BACKLOG_THREAD, dispatchStatus: 403 });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    await waitFor(() => expect(screen.getByTestId("detail-composer")).toBeTruthy());
    await waitFor(() =>
      expect(screen.getAllByRole("option", { name: "agent:reviewer" })).toHaveLength(2),
    );
    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "hello agents" },
    });
    fireEvent.change(screen.getByTestId("detail-composer-assignee"), {
      target: { value: "agent:reviewer" },
    });
    fireEvent.click(screen.getByTestId("detail-composer-submit-assign"));

    // The comment half LANDED — it stays in the thread (the human's text is
    // kept as a posted comment, not rolled back with the failed assign).
    await waitFor(() =>
      expect(screen.getAllByTestId("activity-comment")).toHaveLength(3),
    );
    expect(screen.getByText("hello agents")).toBeTruthy();
    // The assign error mirrors the rail's 403 copy inline…
    const err = await screen.findByTestId("detail-composer-assign-error");
    expect(err.textContent).toBe("That agent isn't a member of this project's team.");
    // …and the composer stays live for another plain comment (no latch).
    expect(screen.getByTestId("detail-composer")).toBeTruthy();
    expect(
      (screen.getByTestId("detail-composer-submit-assign") as HTMLButtonElement)
        .disabled,
    ).toBe(false);
  });

  it("degrades honestly when the assign half 501s: assign button off, composer still posts", async () => {
    routeFetch({ role: "contributor", thread: BACKLOG_THREAD, dispatchStatus: 501 });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    await waitFor(() => expect(screen.getByTestId("detail-composer")).toBeTruthy());
    await waitFor(() =>
      expect(screen.getAllByRole("option", { name: "agent:reviewer" })).toHaveLength(2),
    );
    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "hello agents" },
    });
    fireEvent.change(screen.getByTestId("detail-composer-assignee"), {
      target: { value: "agent:reviewer" },
    });
    fireEvent.click(screen.getByTestId("detail-composer-submit-assign"));

    // Comment kept; honest not-hosted copy inline; JUST the assign button
    // latches off — the plain Comment path still works afterwards.
    await waitFor(() =>
      expect(screen.getAllByTestId("activity-comment")).toHaveLength(3),
    );
    const err = await screen.findByTestId("detail-composer-assign-error");
    expect(err.textContent).toBe("Assigning agents isn't hosted on this deployment yet.");
    expect(
      (screen.getByTestId("detail-composer-submit-assign") as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "still here" },
    });
    fireEvent.click(screen.getByTestId("detail-composer-submit"));
    await waitFor(() =>
      expect(
        (screen.getByTestId("detail-composer-input") as HTMLTextAreaElement).value,
      ).toBe(""),
    );
  });

  // ---- Rail AssigneeControl (ISI-4567 §2.2 assign-where-legal + §2.4 display) ----

  it("renders the assignee select for a contributor on a BACKLOG ticket", async () => {
    routeFetch({ role: "contributor", thread: BACKLOG_THREAD });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    const select = await screen.findByTestId("detail-assignee-select");
    expect(screen.queryByTestId("detail-holder")).toBeNull();
    // No requested agent yet → the honest placeholder, never a fabricated name.
    await screen.findByText("Assign agent…");
    expect((select as HTMLSelectElement).value).toBe("");
    // Backlog hint copy (§2.2): assign == dispatch == start.
    expect(
      screen.getByText("Assigning dispatches the agent to start this ticket."),
    ).toBeTruthy();
    // The roster options come from GET /api/squad/agents.
    expect(await screen.findByRole("option", { name: "agent:reviewer" })).toBeTruthy();
  });

  it("renders the assignee select for a contributor on a TODO ticket, value = the requested agent", async () => {
    routeFetch({ role: "contributor", thread: TODO_THREAD });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    const select = await screen.findByTestId("detail-assignee-select");
    // The select's VALUE reflects reality: the pre-claim requested agent…
    await waitFor(() =>
      expect((select as HTMLSelectElement).value).toBe("agent:builder"),
    );
    // …labelled honestly as a request, not a claim.
    expect(screen.getByText("Requested: agent:builder")).toBeTruthy();
    // Todo hint copy (§2.2) + the dispatch-pending marker (§2.4).
    expect(screen.getByText("Assignment applies before the agent starts.")).toBeTruthy();
    expect(screen.getByTestId("detail-requested-pending").textContent).toContain(
      "dispatch pending",
    );
  });

  it("keeps in_progress read-only: holder display + Kill/re-dispatch hint, no select", async () => {
    routeFetch({ role: "contributor", thread: IN_PROGRESS_THREAD });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    await waitFor(() => expect(screen.getByTestId("detail-holder")).toBeTruthy());
    expect(screen.queryByTestId("detail-assignee-select")).toBeNull();
    expect(
      within(screen.getByTestId("detail-holder")).getByText("agent:builder"),
    ).toBeTruthy();
    expect(
      screen.getByText("Agent changes use the Kill + re-dispatch flow."),
    ).toBeTruthy();
  });

  it("re-assigns a todo ticket: the pick POSTs dispatch and the re-fetch shows the swap", async () => {
    routeFetch({ role: "contributor", thread: TODO_THREAD });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    const select = await screen.findByTestId("detail-assignee-select");
    await waitFor(() =>
      expect((select as HTMLSelectElement).value).toBe("agent:builder"),
    );

    fireEvent.change(select, { target: { value: "agent:reviewer" } });

    // The dispatch verb fired exactly once, with the picked agent NAME.
    const dispatchCalls = fetchMock.mock.calls.filter(([u]) =>
      String(u).includes("/dispatch"),
    );
    expect(dispatchCalls).toHaveLength(1);
    expect(dispatchCalls[0][1]?.body).toBe(JSON.stringify({ agentId: "agent:reviewer" }));
    // …and the reconciling thread re-fetch surfaces the swapped requested agent —
    // the select's value can only become agent:reviewer via server truth.
    await waitFor(() =>
      expect(
        (screen.getByTestId("detail-assignee-select") as HTMLSelectElement).value,
      ).toBe("agent:reviewer"),
    );
    expect(screen.getByText("Requested: agent:reviewer")).toBeTruthy();
  });

  it("keeps a viewer read-only: neither the assignee select nor the status select renders", async () => {
    routeFetch({ role: "viewer", thread: TODO_THREAD });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    // Viewer on an unclaimed todo with a requested agent → §2.4 read-only display.
    const requested = await screen.findByTestId("detail-requested");
    expect(requested.textContent).toContain("Requested: agent:builder");
    expect(requested.textContent).toContain("dispatch pending");
    // Neither rail write control renders for a viewer (fail-closed, §12.3).
    expect(screen.queryByTestId("detail-assignee-select")).toBeNull();
    expect(screen.queryByTestId("detail-status-select")).toBeNull();
  });
});
