// test/discussion/DiscussionRoom.test.tsx — the room's live chat behavior
// (ISI-5208): posting an @-mention immediately shows the "{agent} is working…"
// chip on the DISCUSSION COMPOSER (not just documented), the agent's `thinking`
// envelopes stream inline beneath the triggering message over the ONE project
// channel, and the reply landing over that same channel resolves the chip.

import { describe, it, expect, afterEach, vi } from "vitest";
import {
  render,
  screen,
  cleanup,
  waitFor,
  fireEvent,
  within,
  act,
} from "@testing-library/react";
import { DiscussionRoom } from "@/components/discussion/DiscussionRoom";
import type { DiscussionClient } from "@/lib/discussion/api";
import type { Message } from "@/lib/discussion/types";
import type { RoomStreamEvent } from "@/lib/discussion/liveFeed";
import type { RosterAgent } from "@/components/discussion/Roster";

function humanMsg(id: string, body: string): Message {
  return {
    id,
    threadId: "th-1",
    parentId: null,
    authorPrincipal: "henrik",
    authorAgentId: null,
    authorRunId: null,
    body,
    createdAt: "2026-09-29T10:00:00Z",
  };
}

function agentReply(id: string): Message {
  return {
    id,
    threadId: "th-1",
    parentId: null,
    authorPrincipal: "john",
    authorAgentId: "agent-john",
    authorRunId: "r1",
    body: "on it — here is the plan",
    createdAt: "2026-09-29T10:01:00Z",
  };
}

function makeClient(overrides: Partial<DiscussionClient> = {}): DiscussionClient {
  return {
    listThreads: vi.fn(),
    getThread: vi.fn().mockResolvedValue([]),
    getThreadInfo: vi.fn(),
    openThread: vi.fn(),
    postMessage: vi.fn().mockResolvedValue(humanMsg("m-new", "@john please help")),
    searchMentions: vi.fn().mockResolvedValue([]),
    getRoster: vi.fn(),
    retractMessage: vi.fn(),
    listProposals: vi.fn().mockResolvedValue([]),
    ...overrides,
  } as DiscussionClient;
}

const roster: RosterAgent[] = [{ id: "john", name: "john", status: "idle" }];

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

async function mountAndPost() {
  let emit: (e: RoomStreamEvent) => void = () => {};
  const subscribe = (onEvent: (e: RoomStreamEvent) => void) => {
    emit = onEvent;
    return () => {};
  };
  const client = makeClient();
  render(
    <DiscussionRoom
      projectId="ns/proj"
      threadId="th-1"
      client={client}
      subscribe={subscribe}
      loadRoster={() => Promise.resolve(roster)}
    />,
  );
  // Wait for the room to finish loading (roster resolved).
  await waitFor(() => screen.getByTestId("discussion-room"));

  // Post an @-mention through the real composer (not a direct seed).
  const body = screen.getByTestId("composer-body");
  fireEvent.change(body, { target: { value: "@john please help" } });
  fireEvent.submit(screen.getByTestId("composer"));
  await waitFor(() => expect(client.postMessage).toHaveBeenCalled());
  return { emit };
}

describe("DiscussionRoom — live chat (ISI-5208)", () => {
  it("seeds the 'is working…' chip on the composer's @-mention post", async () => {
    await mountAndPost();
    await waitFor(() => {
      const chip = screen.getByTestId("working-indicator");
      expect(chip).toHaveAttribute("data-phase", "working");
      expect(chip).toHaveAttribute("data-agent", "john");
    });
    expect(screen.getByText(/john is working/i)).toBeTruthy();
  });

  it("streams the agent's thinking inline beneath the triggering message", async () => {
    const { emit } = await mountAndPost();
    await waitFor(() => screen.getByTestId("working-indicator"));

    act(() =>
      emit({
        kind: "thinking",
        row: {
          runId: "r1",
          author: "agent:john",
          body: "[run r1] reading the thread",
          seq: 1,
          at: "2026-09-29T10:00:30Z",
        },
      }),
    );

    await waitFor(() => {
      const rows = screen.getAllByTestId("thinking-row");
      expect(rows).toHaveLength(1);
      expect(rows[0].textContent).toContain("reading the thread");
    });
  });

  it("resolves the chip to 'replied' when the agent reply lands over the same channel", async () => {
    const { emit } = await mountAndPost();
    await waitFor(() => screen.getByTestId("working-indicator"));

    act(() =>
      emit({ kind: "room", event: { type: "message.created", message: agentReply("m-reply") } }),
    );

    await waitFor(() => {
      const chip = screen.getByTestId("working-indicator");
      expect(chip).toHaveAttribute("data-phase", "replied");
    });
    // The reply itself also live-appended as a message row.
    const threads = screen.getByTestId("threads");
    expect(within(threads).getByText(/on it — here is the plan/i)).toBeTruthy();
  });

  it("drops a thinking envelope when no dispatch is active in this thread", async () => {
    // Mount WITHOUT posting — no working watch exists, so a thinking envelope for a
    // run this thread did not dispatch (e.g. a ticket run in the same project)
    // correlates to nothing and renders nothing.
    let emit: (e: RoomStreamEvent) => void = () => {};
    render(
      <DiscussionRoom
        projectId="ns/proj"
        threadId="th-1"
        client={makeClient()}
        subscribe={(onEvent) => {
          emit = onEvent;
          return () => {};
        }}
        loadRoster={() => Promise.resolve(roster)}
      />,
    );
    await waitFor(() => screen.getByTestId("discussion-room"));

    act(() =>
      emit({
        kind: "thinking",
        row: { runId: "rX", author: "agent:mary", body: "unrelated", seq: 1, at: "z" },
      }),
    );

    await new Promise((r) => setTimeout(r, 0));
    expect(screen.queryByTestId("thinking-row")).toBeNull();
  });
});
