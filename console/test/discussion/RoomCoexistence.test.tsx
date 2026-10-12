// test/discussion/RoomCoexistence.test.tsx — ISI-5675 (ISI-5598 WS-E).
//
// Room-layout COEXISTENCE: the ISI-5598 discussion-history left panel (WS-C) must compose with the
// ISI-5597/5623 room affordances (docked composer, jump FAB, roster) and the WS-D /reset empty state in
// ONE mount, with no region clobbering another. The per-region behaviours are proven elsewhere
// (DiscussionsPanel.test, RoomClient.test, DiscussionRoom.scroll.test = ISI-5597 dock/FAB/roster,
// DiscussionRoom.reset.test = WS-D). THIS suite renders the REAL DiscussionRoom through the real
// DiscussionRoomClient (nothing under test is mocked) and asserts all three regions co-render in the
// single `.ksq-roomlayout` tree — the regression guard the shared-file sequencing (5597↔5598) called for.
//
// jsdom computes no layout and evaluates no media queries, so the desktop/tablet/mobile · light/dark ·
// reduced-motion matrix is verified by the static CSS audit in the WS-E report (tokens-only, append-only,
// mock-locked by ISI-5605); here we pin the composed DOM contract that must hold at every breakpoint.

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/react";
import type { DiscussionClient } from "@/lib/discussion/api";
import type { Message, Thread } from "@/lib/discussion/types";
import type { RosterAgent } from "@/components/discussion/Roster";

// next/navigation: a stable ?thread so the client resolves a single active thread.
let params = new URLSearchParams("thread=th-1");
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  usePathname: () => "/projects/p1/discussion",
  useSearchParams: () => params,
}));

// jsdom ships neither ResizeObserver nor scrollTo; the docked composer + FAB use both.
class NoopResizeObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
}
vi.stubGlobal(
  "ResizeObserver",
  NoopResizeObserver as unknown as typeof ResizeObserver,
);

import { DiscussionRoomClient } from "@/app/(app)/projects/[projectId]/discussion/RoomClient";

afterEach(() => {
  cleanup();
  params = new URLSearchParams("thread=th-1");
});

const THREADS: Thread[] = [
  {
    id: "th-1",
    projectId: "p1",
    teamId: "t1",
    title: "Planning",
    createdBy: "alice",
    createdAt: "2026-10-12T11:30:00Z",
  },
];

const roster: RosterAgent[] = [
  { id: "john", name: "john", status: "idle" },
  { id: "jane", name: "jane", status: "running" },
];

function humanMsg(id: string, body: string): Message {
  return {
    id,
    threadId: "th-1",
    parentId: null,
    authorPrincipal: "henrik",
    authorAgentId: null,
    authorRunId: null,
    body,
    createdAt: "2026-10-12T10:00:00Z",
  };
}

// A client that satisfies BOTH the wrapper (listThreads/openThread/getRoster/searchMentions) and the
// real room's load path (getThread/getThreadInfo/listProposals/…).
function makeClient(over: Partial<DiscussionClient> = {}): DiscussionClient {
  return {
    listThreads: vi.fn().mockResolvedValue(THREADS),
    openThread: vi.fn(),
    getThread: vi.fn().mockResolvedValue([humanMsg("m-1", "hello room")]),
    getThreadInfo: vi
      .fn()
      .mockResolvedValue({ ...THREADS[0], resetAt: null, resetBy: null }),
    getRoster: vi.fn().mockResolvedValue(roster),
    searchMentions: vi.fn().mockResolvedValue([]),
    postMessage: vi.fn(),
    retractMessage: vi.fn(),
    resetThread: vi.fn(),
    listProposals: vi.fn().mockResolvedValue([]),
    ...over,
  } as unknown as DiscussionClient;
}

const noOverview = () => Promise.resolve({ ok: false } as Response);

function renderClient(client: DiscussionClient) {
  render(
    <DiscussionRoomClient
      projectId="p1"
      client={client}
      loadOverview={noOverview}
    />,
  );
}

describe("discussion room-layout coexistence (ISI-5675 / WS-E)", () => {
  it("renders the WS-C panel, the room, the ISI-5597 dock + FAB + roster in one .ksq-roomlayout tree", async () => {
    renderClient(makeClient());

    // The 3-column coexistence wrapper (rail │ main) is the single root.
    const layout = await screen.findByTestId("discussion-room-layout");
    expect(layout.className).toContain("ksq-roomlayout");

    // WS-C left panel is present inside the wrapper.
    const panel = screen.getByTestId("discussions-panel");
    expect(layout.contains(panel)).toBe(true);

    // The room resolved and mounted alongside the panel.
    const room = await screen.findByTestId("discussion-room");
    expect(layout.contains(room)).toBe(true);

    // ISI-5597 affordances all co-render and live INSIDE the room (not the panel):
    //   docked composer, jump FAB, roster.
    const dock = screen.getByTestId("room-composer-dock");
    const fab = screen.getByTestId("room-jump-bottom");
    const rosterEl = await screen.findByTestId("roster");
    expect(room.contains(dock)).toBe(true);
    expect(room.contains(fab)).toBe(true);
    expect(room.contains(rosterEl)).toBe(true);

    // The panel and the room are SIBLINGS — the panel never swallows a room region.
    expect(panel.contains(dock)).toBe(false);
    expect(panel.contains(rosterEl)).toBe(false);
  });

  it("the WS-D post-reset empty state coexists with the panel (reset thread on load)", async () => {
    // A thread whose latest read is reset: getThread floored → no bubbles, getThreadInfo carries the
    // reset metadata. The empty state must render inside the room beside the still-present panel.
    const client = makeClient({
      getThread: vi.fn().mockResolvedValue([]),
      getThreadInfo: vi.fn().mockResolvedValue({
        ...THREADS[0],
        resetAt: "2026-10-12T12:00:00Z",
        resetBy: "user:alice",
      }),
    });
    renderClient(client);

    const empty = await screen.findByTestId("room-reset-empty");
    const panel = screen.getByTestId("discussions-panel");
    const layout = screen.getByTestId("discussion-room-layout");
    // Both the reset empty state and the panel live under the one coexistence wrapper.
    expect(layout.contains(empty)).toBe(true);
    expect(layout.contains(panel)).toBe(true);
    // The empty state belongs to the room, not the panel.
    expect(panel.contains(empty)).toBe(false);
  });
});
