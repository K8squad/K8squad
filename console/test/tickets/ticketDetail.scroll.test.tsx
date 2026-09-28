// test/tickets/ticketDetail.scroll.test.tsx — ISI-5157.
//
// Scroll ergonomics for a tall ticket. Three board asks; two carry testable
// deltas at the component level (the pinned-rail change is pure CSS):
//   #1 a jump-to-bottom FAB, shown only when content sits below the fold and
//      smooth-scrolling the page to the newest activity on click; and
//   #3 the comment + assign composer lifted OUT of the scrolling Activity card
//      into a bar docked to the bottom of the view.
// This renders the real TicketDetail; only fetch, the SSE transport, and the
// scroll-metric globals jsdom does not implement are stubbed. The composer's own
// comment/assign/@-mention contract is covered by the sibling ticketDetail.* tests
// — here we prove only that it is DOCKED and that the FAB is wired.

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

// jsdom ships neither ResizeObserver nor a usable window.scrollTo; the component
// guards the former and calls the latter. Stub both so the effects run cleanly.
class NoopResizeObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
}
vi.stubGlobal("ResizeObserver", NoopResizeObserver as unknown as typeof ResizeObserver);

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

const SQUAD = [{ id: "ag-1", name: "agent:builder" }];

function routeFetch() {
  fetchMock.mockImplementation((url: string) => {
    const u = String(url);
    if (u.includes("/api/session")) {
      return Promise.resolve(jsonResponse({ globalRole: "contributor" }));
    }
    if (u.includes("/api/squad/agents")) {
      return Promise.resolve(jsonResponse({ agents: SQUAD }));
    }
    if (u.includes("/api/runs")) return Promise.resolve(jsonResponse([]));
    if (u.includes("/api/work-items/")) return Promise.resolve(jsonResponse(THREAD));
    if (u.includes("/work-items")) return Promise.resolve(jsonResponse([]));
    return Promise.resolve(jsonResponse([]));
  });
}

/** Force the page to look scrollable (content below the fold) and notify listeners. */
function setPageScroll({ scrollHeight, scrollY, innerHeight }: {
  scrollHeight: number;
  scrollY: number;
  innerHeight: number;
}) {
  Object.defineProperty(document.documentElement, "scrollHeight", {
    configurable: true,
    value: scrollHeight,
  });
  Object.defineProperty(window, "scrollY", { configurable: true, value: scrollY });
  Object.defineProperty(window, "innerHeight", {
    configurable: true,
    value: innerHeight,
  });
  fireEvent.scroll(window);
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
beforeEach(() => fetchMock.mockReset());

describe("TicketDetail — docked composer + jump-to-bottom FAB (ISI-5157)", () => {
  it("docks the comment + assign composer at the bottom, outside the Activity card", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);

    const dock = await screen.findByTestId("detail-composer-dock");
    // The very same composer — its own testids are unchanged — now lives INSIDE
    // the dock footer, not inside the scrolling Activity section.
    const composer = await screen.findByTestId("detail-composer");
    expect(dock.contains(composer)).toBe(true);

    const activity = screen.getByTestId("detail-activity");
    expect(activity.contains(composer)).toBe(false);
  });

  it("keeps the jump-to-bottom FAB present but inert while there is no overflow", async () => {
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await screen.findByTestId("detail-composer-dock");

    // No content below the fold → hidden, out of the tab order, hidden from AT.
    setPageScroll({ scrollHeight: 700, scrollY: 0, innerHeight: 768 });
    const fab = screen.getByTestId("detail-jump-bottom");
    await waitFor(() => expect(fab).toHaveAttribute("data-visible", "false"));
    expect(fab).toHaveAttribute("aria-hidden", "true");
    expect(fab).toHaveAttribute("tabindex", "-1");
    expect(fab).toHaveAttribute("aria-label", "Scroll to latest");
  });

  it("reveals the FAB when content sits below the fold and scrolls to the bottom on click", async () => {
    const scrollTo = vi.fn();
    vi.stubGlobal("scrollTo", scrollTo);
    routeFetch();
    render(<TicketDetail projectId="ns/demo" workItemId="wi-1" />);
    await screen.findByTestId("detail-composer-dock");

    // A tall ticket: lots of content below the fold → the FAB shows and joins the
    // tab order.
    setPageScroll({ scrollHeight: 5000, scrollY: 0, innerHeight: 768 });
    const fab = screen.getByTestId("detail-jump-bottom");
    await waitFor(() => expect(fab).toHaveAttribute("data-visible", "true"));
    expect(fab).toHaveAttribute("aria-hidden", "false");
    expect(fab).toHaveAttribute("tabindex", "0");

    fireEvent.click(fab);
    expect(scrollTo).toHaveBeenCalledTimes(1);
    const arg = scrollTo.mock.calls[0][0] as ScrollToOptions;
    expect(arg.top).toBe(5000);
  });
});
