// test/tickets/ticketDetail.ticketref.test.tsx — ISI-5214 (parent ISI-5212 S2 / WS-B).
//
// The ticket-detail composer grows the discussion room's `#` affordance: typing `#`
// pops a picker of THIS project's work items (via the reused discussion mentions
// endpoint), and picking one inserts a `#Title ` token AND collects a structured
// { workItemId, title } LINK carried on the comment POST — a link, never a dispatch.
// A comment that carries references renders them as deep-linked chips. This renders
// the real TicketDetail + Composer + shared MentionPopover; only fetch + SSE are stubbed.

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

// The mentions endpoint returns both agent + work_item rows; the `#` trigger scopes
// the shown list to work_item client-side.
const MENTIONS = {
  query: "auth",
  results: [
    { type: "agent", id: "agent:builder", displayName: "agent:builder", projectId: "", state: "online", rank: 0 },
    { type: "work_item", id: "wi-42", displayName: "Fix the auth bug", projectId: "uid-demo", state: "todo", rank: 1 },
    { type: "work_item", id: "wi-43", displayName: "Auth token refresh", projectId: "uid-demo", state: "in_progress", rank: 0.5 },
  ],
};

// The 201 echo canonicalizes the reference title from the server (ISI-5214).
const POSTED_COMMENT = {
  author: "user:me",
  body: "blocked on #Fix the auth bug",
  createdAt: "2026-09-29T10:00:00Z",
  references: [{ workItemId: "wi-42", title: "Fix the auth bug" }],
};

let lastCommentBody: unknown = null;

function routeFetch() {
  let posted = false;
  lastCommentBody = null;
  fetchMock.mockImplementation((url: string, init?: RequestInit) => {
    const u = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    if (u.includes("/api/session")) {
      return Promise.resolve(jsonResponse({ globalRole: "contributor" }));
    }
    if (u.includes("/api/squad/agents")) {
      return Promise.resolve(jsonResponse({ agents: [{ id: "ag-1", name: "agent:builder" }] }));
    }
    if (u.includes("/discussion/mentions")) {
      return Promise.resolve(jsonResponse(MENTIONS));
    }
    if (u.includes("/api/runs")) {
      return Promise.resolve(jsonResponse([]));
    }
    if (u.includes("/comments") && method === "POST") {
      posted = true;
      lastCommentBody = init?.body ? JSON.parse(String(init.body)) : null;
      return Promise.resolve(jsonResponse(POSTED_COMMENT, 201));
    }
    if (u.includes("/api/work-items/")) {
      const body = posted ? { ...THREAD, Comments: [POSTED_COMMENT] } : THREAD;
      return Promise.resolve(jsonResponse(body));
    }
    if (u.includes("/work-items")) return Promise.resolve(jsonResponse([]));
    return Promise.resolve(jsonResponse([]));
  });
}

async function readyComposer() {
  await waitFor(() => expect(screen.getByTestId("detail-composer")).toBeTruthy());
}

afterEach(cleanup);
beforeEach(() => fetchMock.mockReset());

describe("TicketDetail — # ticket picker + references (ISI-5214)", () => {
  it("typing # opens the picker scoped to work items", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await readyComposer();

    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "blocked on #auth" },
    });
    await waitFor(() =>
      expect(screen.getAllByTestId("mention-option").length).toBe(2),
    );
    // Every row is a work_item (the `#` trigger drops the agent row).
    for (const opt of screen.getAllByTestId("mention-option")) {
      expect(opt).toHaveAttribute("data-mention-type", "work_item");
    }
  });

  it("picking a ticket inserts the #Title token without arming the assignee", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await readyComposer();

    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "blocked on #auth" },
    });
    await waitFor(() =>
      expect(screen.getAllByTestId("mention-option").length).toBe(2),
    );
    fireEvent.click(
      screen
        .getAllByTestId("mention-option")
        .find((o) => o.textContent?.includes("Fix the auth bug"))!,
    );

    const input = screen.getByTestId("detail-composer-input") as HTMLTextAreaElement;
    expect(input.value).toContain("#Fix the auth bug ");
    // A ticket link is NOT a dispatch — the assign select stays empty.
    const assign = screen.getByTestId("detail-composer-assignee") as HTMLSelectElement;
    expect(assign.value).toBe("");
  });

  it("posting carries the collected references on the comment body", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await readyComposer();

    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "blocked on #auth" },
    });
    await waitFor(() =>
      expect(screen.getAllByTestId("mention-option").length).toBe(2),
    );
    fireEvent.click(
      screen
        .getAllByTestId("mention-option")
        .find((o) => o.textContent?.includes("Fix the auth bug"))!,
    );
    fireEvent.click(screen.getByTestId("detail-composer-submit"));

    await waitFor(() => expect(lastCommentBody).not.toBeNull());
    expect(lastCommentBody).toEqual({
      body: "blocked on #Fix the auth bug",
      references: [{ workItemId: "wi-42", title: "Fix the auth bug" }],
    });
  });

  it("a comment with references renders deep-linked ticket chips", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await readyComposer();

    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "blocked on #auth" },
    });
    await waitFor(() =>
      expect(screen.getAllByTestId("mention-option").length).toBe(2),
    );
    fireEvent.click(
      screen
        .getAllByTestId("mention-option")
        .find((o) => o.textContent?.includes("Fix the auth bug"))!,
    );
    fireEvent.click(screen.getByTestId("detail-composer-submit"));

    const chip = await screen.findByTestId("comment-ticket-ref");
    expect(chip).toHaveTextContent("Fix the auth bug");
    expect(chip).toHaveAttribute(
      "href",
      "/projects/ns%2Fdemo/issues/wi-42",
    );
  });
});
