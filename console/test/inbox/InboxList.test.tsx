import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/react";
import { InboxList } from "@/components/inbox/InboxList";

// ISI-5535 F1 regression: the Inbox must actually mark its unread items seen when opened, so the
// unread dots + nav badge clear. Before the fix the seen BFF route and this call didn't exist, so
// unread turned ON and never OFF. This test pins that opening the Inbox POSTs the unread keys to
// /api/inbox/seen exactly once (not per 5s poll), and leaves already-read items out of the payload.

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const payload = {
  items: [
    { key: "proposal:m-1", projectId: "squad-a/web", title: "Needs approval", decisionType: "proposal", unread: true },
    { key: "inReview:wi-9", projectId: "squad-a/web", title: "Already read", decisionType: "review", unread: false },
  ],
};

describe("InboxList mark-seen (F1)", () => {
  it("POSTs the unread keys to /api/inbox/seen once on open", async () => {
    const seenBodies: unknown[] = [];
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url === "/api/inbox/seen") {
        seenBodies.push(JSON.parse(String(init?.body ?? "{}")));
        return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ ok: true }) });
      }
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(payload) });
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<InboxList />);

    // The list renders both rows…
    await screen.findByText("Needs approval");
    expect(screen.getByText("Already read")).toBeTruthy();

    // …and exactly one mark-seen POST fires, carrying only the unread key.
    await waitFor(() => expect(seenBodies.length).toBe(1));
    expect(seenBodies[0]).toEqual({ keys: ["proposal:m-1"] });
  });
});
