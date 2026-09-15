// test/tickets/createButton.test.tsx — the "+ New issue" affordance on the Tickets
// screen (ISI-4496). Two guarantees the parent reporter's "no way to test it" needs:
//
//   1. A signed-in caller (globalRole "user" | "admin") gets an ENABLED button that
//      opens the create sheet — the root-cause fix is that fetchViewerRole reads
//      /auth/me's `globalRole`, not the never-present `role` (which pinned everyone
//      to the "viewer" sentinel and hid the button for admins too).
//   2. An unresolved caller (non-200 / missing role) gets a DISABLED button WITH a
//      hint — a create-gated state is now diagnosable, never invisible (the previous
//      hide made "gated" indistinguishable from "feature not shipped").

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import { TicketsScreen } from "@/components/tickets/TicketsScreen";

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

// Route the two reads TicketsScreen fires on mount: the board list and /api/session
// (fetchViewerRole). `session` lets a case simulate an unresolved caller.
function routeFetch(session: Response) {
  fetchMock.mockImplementation((url: string) => {
    const u = String(url);
    if (u.includes("/api/session")) return Promise.resolve(session);
    if (u.includes("/work-items")) return Promise.resolve(jsonResponse([]));
    return Promise.resolve(jsonResponse([]));
  });
}

describe("TicketsScreen '+ New issue' RBAC affordance (ISI-4496)", () => {
  beforeEach(() => fetchMock.mockReset());
  afterEach(() => cleanup());

  it("a signed-in caller gets an ENABLED button that opens the create sheet", async () => {
    routeFetch(jsonResponse({ globalRole: "user", username: "ada" }));
    render(<TicketsScreen projectId="ns/demo" />);

    const btn = await screen.findByTestId("new-issue");
    await waitFor(() => expect(btn).toBeEnabled());
    expect(btn.getAttribute("title")).toBeNull();

    fireEvent.click(btn);
    // The CreateTicketSheet mounts its title field once opened.
    expect(await screen.findByTestId("create-ticket-sheet")).toBeInTheDocument();
  });

  it("an unresolved caller gets a DISABLED button WITH a diagnostic hint, not a vanished one", async () => {
    routeFetch(jsonResponse({ error: "no valid session" }, 401));
    render(<TicketsScreen projectId="ns/demo" />);

    // The button is ALWAYS present (diagnosable), never conditionally removed.
    const btn = await screen.findByTestId("new-issue");
    await waitFor(() => expect(btn).toBeDisabled());
    expect(btn.getAttribute("title")).toMatch(/create access/i);

    // A disabled click opens nothing.
    fireEvent.click(btn);
    expect(screen.queryByTestId("create-ticket-sheet")).toBeNull();
  });
});
