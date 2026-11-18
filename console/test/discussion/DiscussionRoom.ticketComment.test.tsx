// test/discussion/DiscussionRoom.ticketComment.test.tsx — ISI-7485 (WS-4 of
// parent ISI-7482): the bridged project-bus subset reaches the ROOM surface.
// A run's final work-item comment (`ticket_comment`, ISI-7274) renders live as
// the dispatch's deliverable and resolves its working watch; a bridged
// `run_ended` fires the coalesced re-fetch WITHOUT the room ever learning the
// run id from a thinking envelope (the per-run RunEndedWatch stays as the
// replay-capable backstop, no longer the only path).

import { describe, it, expect, afterEach, vi } from "vitest";
import {
  render,
  screen,
  cleanup,
  waitFor,
  fireEvent,
  act,
} from "@testing-library/react";
import { DiscussionRoom } from "@/components/discussion/DiscussionRoom";
import type { DiscussionClient } from "@/lib/discussion/api";
import type { Message } from "@/lib/discussion/types";
import type { RoomStreamEvent } from "@/lib/discussion/liveFeed";
import type { RosterAgent } from "@/components/discussion/Roster";

// Recording EventSource: proves the room opened NO per-run stream in the
// run_ended test (no run id was ever learned, none is needed).
class RecordingEventSource {
  static instances: RecordingEventSource[] = [];
  url: string;
  closed = false;
  constructor(url: string) {
    this.url = url;
    RecordingEventSource.instances.push(this);
  }
  addEventListener() {}
  close() {
    this.closed = true;
  }
}

function humanMsg(id: string, body: string): Message {
  return {
    id,
    threadId: "th-1",
    parentId: null,
    authorPrincipal: "henrik",
    authorAgentId: null,
    authorRunId: null,
    body,
    createdAt: "2026-11-16T10:00:00Z",
  };
}

function makeClient(
  overrides: Partial<DiscussionClient> = {},
): DiscussionClient {
  return {
    listThreads: vi.fn(),
    getThread: vi.fn().mockResolvedValue([]),
    getThreadInfo: vi.fn(),
    openThread: vi.fn(),
    postMessage: vi
      .fn()
      .mockResolvedValue(humanMsg("m-new", "@john please help")),
    searchMentions: vi.fn().mockResolvedValue([]),
    getRoster: vi.fn(),
    retractMessage: vi.fn(),
    listProposals: vi.fn().mockResolvedValue([]),
    ...overrides,
  } as DiscussionClient;
}

const roster: RosterAgent[] = [{ id: "john", name: "john", status: "idle" }];

// Mount the room and dispatch @john through the real composer (seeds a working
// watch), returning the bus emit seam.
async function mountAndPost(getThread: DiscussionClient["getThread"]) {
  let emit: (e: RoomStreamEvent) => void = () => {};
  const subscribe = (onEvent: (e: RoomStreamEvent) => void) => {
    emit = onEvent;
    return () => {};
  };
  const client = makeClient({ getThread });
  render(
    <DiscussionRoom
      projectId="ns/proj"
      threadId="th-1"
      client={client}
      subscribe={subscribe}
      loadRoster={() => Promise.resolve(roster)}
    />,
  );
  await waitFor(() => screen.getByTestId("discussion-room"));
  fireEvent.change(screen.getByTestId("composer-body"), {
    target: { value: "@john please help" },
  });
  fireEvent.submit(screen.getByTestId("composer"));
  await waitFor(() => screen.getByTestId("working-indicator"));
  return { emit };
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.unstubAllGlobals();
  RecordingEventSource.instances = [];
});

describe("DiscussionRoom — bridged ticket_comment (ISI-7485)", () => {
  it("renders the run's final work-item comment as the deliverable tier and resolves the working watch", async () => {
    const getThread = vi.fn().mockResolvedValue([humanMsg("m1", "hello room")]);
    const { emit } = await mountAndPost(getThread);

    // The bridged echo: NO thinking envelope and NO in-thread reply ever
    // arrive — the final comment alone must surface the answer.
    act(() =>
      emit({
        kind: "ticket_comment",
        row: {
          runId: "run-9",
          workItemId: "wi-1",
          author: "agent:john",
          body: "final recap: shipped the bridge",
          kind: "summary",
          at: "2026-11-16T10:01:00Z",
        },
      }),
    );

    // The final answer renders in the deliverable (upright final) tier.
    await waitFor(() => {
      const finals = screen.getAllByTestId("thinking-final");
      expect(
        finals.some((n) => n.textContent?.includes("shipped the bridge")),
      ).toBe(true);
    });
    // The final comment IS the agent's response — the chip flips to replied.
    await waitFor(() => {
      const chip = screen.getByTestId("working-indicator");
      expect(chip).toHaveAttribute("data-phase", "replied");
    });
  });
});

describe("DiscussionRoom — bridged run_ended (ISI-7485)", () => {
  it("re-fetches on terminal state with NO thinking envelope and NO per-run stream", async () => {
    vi.stubGlobal("EventSource", RecordingEventSource);
    const getThread = vi
      .fn()
      .mockResolvedValueOnce([humanMsg("m1", "hello room")])
      .mockResolvedValue([
        humanMsg("m1", "hello room"),
        // The triggering post IS durable in the thread — the refetched list
        // keeps the watch's anchor message, so the chip stays observable.
        humanMsg("m-new", "@john please help"),
        humanMsg("m2", "durable answer materialized by the refetch"),
      ]);
    const { emit } = await mountAndPost(getThread);
    expect(getThread).toHaveBeenCalledTimes(1);

    // The terminal milestone, bridged project-wide. The room never saw a
    // thinking envelope, so it never learned the run id — and needs it not.
    act(() =>
      emit({
        kind: "run_ended",
        row: { runId: "run-9", agent: "john" },
      }),
    );

    // The coalesced re-fetch materializes durable truth; the watch keeps
    // working until the reply/echo resolves it (settle commits AFTER ended).
    await waitFor(() => expect(getThread).toHaveBeenCalledTimes(2), {
      timeout: 3000,
    });
    await waitFor(() =>
      expect(
        screen.getByText("durable answer materialized by the refetch"),
      ).toBeTruthy(),
    );
    expect(RecordingEventSource.instances).toHaveLength(0);
    expect(screen.getByTestId("working-indicator")).toHaveAttribute(
      "data-phase",
      "working",
    );
  });
});
