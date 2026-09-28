// test/tickets/ticketDetail.mention.test.tsx — ISI-5159.
//
// The ticket-detail comment composer grows an `@`-mention affordance: typing `@`
// opens an autocomplete over the SAME squad roster the "Assign to…" select loads,
// and picking an agent inserts the `@Name ` token AND arms the assignee — so the
// existing Comment-&-assign verb dispatches the mentioned agent onto the ticket
// (the board's "same experience we have in Paperclip"). This integration test
// renders the real TicketDetail + Composer + shared MentionPopover; only fetch and
// the SSE transport are stubbed. The dispatch wiring itself is covered by
// ticketDetail.dispatch.test.tsx — here we prove the mention → arm → dispatch path.

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
  body: "please look @agent:reviewer",
  createdAt: "2026-09-24T10:00:00Z",
};

function routeFetch() {
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
      return Promise.resolve(jsonResponse([]));
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

/** Wait for the composer to be live (roster loaded → assign options present). */
async function readyComposer() {
  await waitFor(() => expect(screen.getByTestId("detail-composer")).toBeTruthy());
  await waitFor(() =>
    expect(
      screen.getAllByRole("option", { name: "agent:reviewer" }).length,
    ).toBeGreaterThan(0),
  );
}

afterEach(cleanup);
beforeEach(() => fetchMock.mockReset());

describe("TicketDetail — @-mention autocomplete + assign (ISI-5159)", () => {
  it("typing @ opens the roster popover, filterable by the fragment", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await readyComposer();

    // `@` alone offers the whole roster…
    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "please look @" },
    });
    await waitFor(() => expect(screen.getByTestId("mention-popover")).toBeTruthy());
    expect(screen.getAllByTestId("mention-option")).toHaveLength(2);
    // …and every row is an agent (glyph `@`, not `#`).
    for (const opt of screen.getAllByTestId("mention-option")) {
      expect(opt).toHaveAttribute("data-mention-type", "agent");
    }
  });

  it("a non-matching fragment shows the empty state", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await readyComposer();

    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "hey @zzz" },
    });
    await waitFor(() => expect(screen.getByTestId("mention-empty")).toBeTruthy());
  });

  it("selecting an agent inserts the token AND arms the assignee select", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await readyComposer();

    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "please look @agent" },
    });
    await waitFor(() =>
      expect(screen.getAllByTestId("mention-option").length).toBe(2),
    );
    // Click the reviewer row.
    const reviewer = screen
      .getAllByTestId("mention-option")
      .find((o) => o.textContent?.includes("agent:reviewer"))!;
    fireEvent.click(reviewer);

    // Token inserted into the draft…
    const input = screen.getByTestId("detail-composer-input") as HTMLTextAreaElement;
    expect(input.value).toContain("@agent:reviewer ");
    // …popover dismissed…
    expect(screen.queryByTestId("mention-popover")).toBeNull();
    // …and the existing assign select is now armed with that agent (the dispatch
    // target), so Comment-&-assign is enabled with no separate select action.
    const assign = screen.getByTestId("detail-composer-assignee") as HTMLSelectElement;
    expect(assign.value).toBe("agent:reviewer");
  });

  it("mention → Comment & assign dispatches the mentioned agent", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await readyComposer();

    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "please look @agent" },
    });
    await waitFor(() =>
      expect(screen.getAllByTestId("mention-option").length).toBe(2),
    );
    fireEvent.click(
      screen
        .getAllByTestId("mention-option")
        .find((o) => o.textContent?.includes("agent:reviewer"))!,
    );
    fireEvent.click(screen.getByTestId("detail-composer-submit-assign"));

    // The comment lands…
    await waitFor(() =>
      expect(screen.getByText("please look @agent:reviewer")).toBeInTheDocument(),
    );
    // …and the dispatch POST carried the @-mentioned agent NAME.
    const dispatchCall = fetchMock.mock.calls.find(
      ([u, init]) =>
        String(u).includes("/dispatch") &&
        (init?.method ?? "GET").toUpperCase() === "POST",
    );
    expect(dispatchCall?.[1]?.body).toBe(
      JSON.stringify({ agentId: "agent:reviewer" }),
    );
  });
});
