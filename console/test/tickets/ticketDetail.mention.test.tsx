// test/tickets/ticketDetail.mention.test.tsx — ISI-5159 / ISI-5281.
//
// The ticket-detail comment composer carries an `@`-mention affordance: typing `@`
// opens an autocomplete over the SAME squad roster the "Assign to…" select loads.
// ISI-5281 drops the old dropdown gate — the popover is now PURE autocomplete, and
// the PRIMARY "Comment" action dispatches whatever single resolvable @name the
// submitted body actually carries (so a popover pick and a hand-typed `@Name`
// dispatch identically, "same experience we have in Paperclip"). This integration
// test renders the real TicketDetail + Composer + shared MentionPopover; only fetch
// and the SSE transport are stubbed.

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
  { id: "ag-1", name: "builder" },
  { id: "ag-2", name: "reviewer" },
];

const POSTED_COMMENT = {
  author: "user:me",
  body: "please look @reviewer",
  createdAt: "2026-09-24T10:00:00Z",
};

// A plain comment with no resolvable @-mention (asserts the zero-mention path).
const PLAIN_COMMENT = {
  author: "user:me",
  body: "just a heads up, no mention",
  createdAt: "2026-09-24T10:05:00Z",
};

function routeFetch(posted = { body: POSTED_COMMENT }) {
  let didPost = false;
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
      didPost = true;
      return Promise.resolve(jsonResponse(posted.body, 201));
    }
    if (u.includes("/dispatch") && method === "POST") {
      return Promise.resolve(
        jsonResponse({
          workItemId: "wi-1",
          fromState: "backlog",
          toState: "todo",
          requestedAgent: "reviewer",
        }),
      );
    }
    if (u.includes("/api/work-items/")) {
      const body = didPost ? { ...THREAD, Comments: [posted.body] } : THREAD;
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
      screen.getAllByRole("option", { name: "reviewer" }).length,
    ).toBeGreaterThan(0),
  );
}

function dispatchCall() {
  return fetchMock.mock.calls.find(
    ([u, init]) =>
      String(u).includes("/dispatch") &&
      (init?.method ?? "GET").toUpperCase() === "POST",
  );
}

afterEach(cleanup);
beforeEach(() => fetchMock.mockReset());

describe("TicketDetail — @-mention autocomplete + body dispatch (ISI-5159/ISI-5281)", () => {
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

  it("selecting an agent inserts the token but does NOT arm the explicit assign select (pure autocomplete)", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await readyComposer();

    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "please look @rev" },
    });
    await waitFor(() =>
      expect(screen.getAllByTestId("mention-option").length).toBe(1),
    );
    fireEvent.click(
      screen
        .getAllByTestId("mention-option")
        .find((o) => o.textContent?.includes("reviewer"))!,
    );

    // Token inserted into the draft…
    const input = screen.getByTestId("detail-composer-input") as HTMLTextAreaElement;
    expect(input.value).toContain("@reviewer ");
    // …popover dismissed…
    expect(screen.queryByTestId("mention-popover")).toBeNull();
    // …and the explicit assign select stays UNARMED — ISI-5281 drops the dropdown
    // gate; dispatch now rides the typed body, not an armed select.
    const assign = screen.getByTestId("detail-composer-assignee") as HTMLSelectElement;
    expect(assign.value).toBe("");
  });

  it("primary Comment dispatches the single resolvable @name typed in the body", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await readyComposer();

    // Hand-type the body — no popover pick, no separate 'Comment & assign' step.
    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "please look @reviewer" },
    });
    fireEvent.click(screen.getByTestId("detail-composer-submit"));

    // The comment lands…
    await waitFor(() =>
      expect(screen.getByText("please look @reviewer")).toBeInTheDocument(),
    );
    // …and the dispatch POST carried the resolved agent NAME.
    await waitFor(() => expect(dispatchCall()).toBeTruthy());
    expect(dispatchCall()?.[1]?.body).toBe(JSON.stringify({ agentId: "reviewer" }));
  });

  it("a body with zero resolvable mentions posts a plain comment and does NOT dispatch", async () => {
    routeFetch({ body: PLAIN_COMMENT });
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await readyComposer();

    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "just a heads up, no mention" },
    });
    fireEvent.click(screen.getByTestId("detail-composer-submit"));

    await waitFor(() =>
      expect(
        screen.getByText("just a heads up, no mention"),
      ).toBeInTheDocument(),
    );
    expect(dispatchCall()).toBeUndefined();
  });

  it("the explicit Comment-&-assign select still dispatches its picked agent", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await readyComposer();

    fireEvent.change(screen.getByTestId("detail-composer-input"), {
      target: { value: "please look at this" },
    });
    fireEvent.change(screen.getByTestId("detail-composer-assignee"), {
      target: { value: "reviewer" },
    });
    fireEvent.click(screen.getByTestId("detail-composer-submit-assign"));

    await waitFor(() => expect(dispatchCall()).toBeTruthy());
    expect(dispatchCall()?.[1]?.body).toBe(JSON.stringify({ agentId: "reviewer" }));
  });
});
