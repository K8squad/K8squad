// test/discussion/DiscussionRoom.scroll.test.tsx — ISI-5623 (impl of ISI-5597),
// retargeted for ISI-5741 (parent ISI-5737).
//
// Scroll ergonomics for a long discussion transcript. Three board asks; all three
// carry a testable component-level delta:
//   P1 a jump-to-bottom FAB, shown only when transcript sits below the fold and
//      scrolling the HISTORY COLUMN to the newest message on click;
//   P2 the composer lifted into a bar DOCKED at the bottom of the room (out of
//      the scrolling thread list); and
//   P3 a narrow-viewport roster mini-bar ("N agents · M running") that expands
//      on tap.
// ISI-5741 made the room a fixed-height viewport: the HISTORY COLUMN
// (`.ksq-room__history`), not the page/document, is the scroll owner, so the FAB
// reads that container's scroll metrics and `jumpToBottom` scrolls it. This
// renders the real DiscussionRoom; only the scroll-metric globals jsdom does not
// implement are stubbed. The composer's own @-mention/audience contract is
// covered by the sibling DiscussionRoom/Composer tests — here we prove only that
// it is DOCKED, that the FAB is wired, and that the roster mini-bar toggles.

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

/** Force the history column to look scrollable (content below the fold) and
 *  notify listeners. ISI-5741: the history container — not the page — is the
 *  scroll owner, so the FAB reads these metrics off it. */
function setHistoryScroll({
  scrollHeight,
  scrollTop,
  clientHeight,
}: {
  scrollHeight: number;
  scrollTop: number;
  clientHeight: number;
}) {
  const el = screen.getByTestId("room-history");
  Object.defineProperty(el, "scrollHeight", {
    configurable: true,
    value: scrollHeight,
  });
  Object.defineProperty(el, "scrollTop", {
    configurable: true,
    value: scrollTop,
  });
  Object.defineProperty(el, "clientHeight", {
    configurable: true,
    value: clientHeight,
  });
  fireEvent.scroll(el);
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
  // Drop the per-test Element.prototype.scrollTo stub so it never leaks to a
  // sibling suite that asserts the method is absent.
  delete (Element.prototype as { scrollTo?: unknown }).scrollTo;
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
    // History fully in view (nothing below the fold).
    setHistoryScroll({ scrollHeight: 700, scrollTop: 0, clientHeight: 768 });
    const fab = screen.getByTestId("room-jump-bottom");
    await waitFor(() => expect(fab).toHaveAttribute("data-visible", "false"));
    expect(fab).toHaveAttribute("aria-hidden", "true");
    expect(fab).toHaveAttribute("tabindex", "-1");
    expect(fab).toHaveAttribute("aria-label", "Scroll to latest");
  });

  it("reveals the FAB when content sits below the history fold and scrolls the column to the bottom on click", async () => {
    // jsdom does not implement Element.prototype.scrollTo; stub it so the handler
    // (which scrolls the history container, not the window) is observable.
    const scrollTo = vi.fn();
    Element.prototype.scrollTo =
      scrollTo as unknown as typeof Element.prototype.scrollTo;
    await mount();

    setHistoryScroll({ scrollHeight: 5000, scrollTop: 0, clientHeight: 768 });
    const fab = screen.getByTestId("room-jump-bottom");
    await waitFor(() => expect(fab).toHaveAttribute("data-visible", "true"));
    expect(fab).toHaveAttribute("aria-hidden", "false");
    expect(fab).toHaveAttribute("tabindex", "0");

    fireEvent.click(fab);
    expect(scrollTo).toHaveBeenCalledTimes(1);
    const self = scrollTo.mock.instances[0] as HTMLElement;
    expect(self).toBe(screen.getByTestId("room-history"));
    const arg = scrollTo.mock.calls[0][0] as ScrollToOptions;
    expect(arg.top).toBe(5000);
  });

  it("scrolls only the history column — the dock and roster stay outside it (ISI-5741 AC2/AC4)", async () => {
    await mount();
    const history = screen.getByTestId("room-history");
    const dock = screen.getByTestId("room-composer-dock");
    const roster = screen.getByTestId("roster");
    const threads = screen.getByTestId("threads");

    // The transcript lives INSIDE the one scrolling region…
    expect(history.contains(threads)).toBe(true);
    // …while the composer dock and the roster are OUTSIDE it, so scrolling the
    // history can never move or hide them.
    expect(history.contains(dock)).toBe(false);
    expect(history.contains(roster)).toBe(false);
    // The FAB is pinned in the center column, not inside the scrolling history.
    const fab = screen.getByTestId("room-jump-bottom");
    expect(history.contains(fab)).toBe(false);
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
