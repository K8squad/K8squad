// test/discussion/DiscussionRoom.scroll.test.tsx — ISI-5623 (impl of ISI-5597).
//
// Scroll ergonomics for a long discussion transcript, ported from ticket-detail
// (ISI-5157). Three board asks; all three carry a testable component-level delta:
//   P1 a jump-to-bottom FAB, shown only when content sits below the fold and
//      smooth-scrolling the page to the newest message on click;
//   P2 the composer lifted into a bar DOCKED at the bottom of the room (out of
//      the scrolling thread list); and
//   P3 a narrow-viewport roster mini-bar ("N agents · M running") that expands
//      on tap. The page (document) is the scroll owner (design LOCKED).
// This renders the real DiscussionRoom; only the scroll-metric globals jsdom
// does not implement are stubbed. The composer's own @-mention/audience contract
// is covered by the sibling DiscussionRoom/Composer tests — here we prove only
// that it is DOCKED, that the FAB is wired, and that the roster mini-bar toggles.

import { describe, it, expect, afterEach, vi } from "vitest";
import {
  render,
  screen,
  cleanup,
  waitFor,
  fireEvent,
} from "@testing-library/react";
import { DiscussionRoom } from "@/components/discussion/DiscussionRoom";
import type { DiscussionClient } from "@/lib/discussion/api";
import type { Message } from "@/lib/discussion/types";
import type { RosterAgent } from "@/components/discussion/Roster";
import type { AgentRunPresence } from "@/lib/overview/liveRuns";

// jsdom ships neither ResizeObserver nor a usable window.scrollTo; the component
// guards the former and calls the latter. Stub both so the effects run cleanly.
class NoopResizeObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
}
vi.stubGlobal(
  "ResizeObserver",
  NoopResizeObserver as unknown as typeof ResizeObserver,
);

function humanMsg(id: string, body: string): Message {
  return {
    id,
    threadId: "th-1",
    parentId: null,
    authorPrincipal: "henrik",
    authorAgentId: null,
    authorRunId: null,
    body,
    createdAt: "2026-10-09T10:00:00Z",
  };
}

function makeClient(
  overrides: Partial<DiscussionClient> = {},
): DiscussionClient {
  return {
    listThreads: vi.fn(),
    getThread: vi.fn().mockResolvedValue([humanMsg("m-1", "hello room")]),
    getThreadInfo: vi.fn(),
    openThread: vi.fn(),
    postMessage: vi.fn(),
    searchMentions: vi.fn().mockResolvedValue([]),
    getRoster: vi.fn(),
    retractMessage: vi.fn(),
    listProposals: vi.fn().mockResolvedValue([]),
    ...overrides,
  } as DiscussionClient;
}

const roster: RosterAgent[] = [
  { id: "john", name: "john", status: "idle" },
  { id: "jane", name: "jane", status: "running" },
];

/** Force the page to look scrollable (content below the fold) and notify listeners. */
function setPageScroll({
  scrollHeight,
  scrollY,
  innerHeight,
}: {
  scrollHeight: number;
  scrollY: number;
  innerHeight: number;
}) {
  Object.defineProperty(document.documentElement, "scrollHeight", {
    configurable: true,
    value: scrollHeight,
  });
  Object.defineProperty(window, "scrollY", {
    configurable: true,
    value: scrollY,
  });
  Object.defineProperty(window, "innerHeight", {
    configurable: true,
    value: innerHeight,
  });
  fireEvent.scroll(window);
}

async function mount(liveRuns?: Record<string, AgentRunPresence>) {
  const client = makeClient();
  render(
    <DiscussionRoom
      projectId="ns/proj"
      threadId="th-1"
      client={client}
      loadRoster={() => Promise.resolve(roster)}
      liveRuns={liveRuns}
    />,
  );
  await waitFor(() => screen.getByTestId("discussion-room"));
  // Roster resolves after the thread; wait for it so the mini-bar counts are live.
  await waitFor(() =>
    expect(screen.getByTestId("roster-minibar")).toBeTruthy(),
  );
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("DiscussionRoom — docked composer + jump FAB + roster mini-bar (ISI-5623)", () => {
  it("docks the composer at the bottom, outside the scrolling thread list", async () => {
    await mount();
    const dock = screen.getByTestId("room-composer-dock");
    const composer = screen.getByTestId("composer");
    expect(dock.contains(composer)).toBe(true);

    const threads = screen.getByTestId("threads");
    expect(threads.contains(composer)).toBe(false);
  });

  it("keeps the jump FAB present but inert while there is no overflow", async () => {
    await mount();
    setPageScroll({ scrollHeight: 700, scrollY: 0, innerHeight: 768 });
    const fab = screen.getByTestId("room-jump-bottom");
    await waitFor(() => expect(fab).toHaveAttribute("data-visible", "false"));
    expect(fab).toHaveAttribute("aria-hidden", "true");
    expect(fab).toHaveAttribute("tabindex", "-1");
    expect(fab).toHaveAttribute("aria-label", "Scroll to latest");
  });

  it("reveals the FAB when content sits below the fold and scrolls to the bottom on click", async () => {
    const scrollTo = vi.fn();
    vi.stubGlobal("scrollTo", scrollTo);
    await mount();

    setPageScroll({ scrollHeight: 5000, scrollY: 0, innerHeight: 768 });
    const fab = screen.getByTestId("room-jump-bottom");
    await waitFor(() => expect(fab).toHaveAttribute("data-visible", "true"));
    expect(fab).toHaveAttribute("aria-hidden", "false");
    expect(fab).toHaveAttribute("tabindex", "0");

    fireEvent.click(fab);
    expect(scrollTo).toHaveBeenCalledTimes(1);
    const arg = scrollTo.mock.calls[0][0] as ScrollToOptions;
    expect(arg.top).toBe(5000);
  });

  it("summarises the roster in the mini-bar and toggles the body on tap", async () => {
    // jane is running → '2 agents · 1 running'. Only actively-running agents count.
    await mount({
      jane: { state: "running", phase: "running", workItem: "ISI-1" },
    });
    const minibar = screen.getByTestId("roster-minibar");
    expect(minibar.textContent).toContain("2 agents");
    expect(minibar.textContent).toContain("1 running");

    const aside = screen.getByTestId("roster");
    // Default collapsed (the narrow room opens with the mini-bar, not the list).
    expect(aside).toHaveAttribute("data-expanded", "false");
    expect(minibar).toHaveAttribute("aria-expanded", "false");

    fireEvent.click(minibar);
    expect(aside).toHaveAttribute("data-expanded", "true");
    expect(minibar).toHaveAttribute("aria-expanded", "true");

    fireEvent.click(minibar);
    expect(aside).toHaveAttribute("data-expanded", "false");
  });
});
