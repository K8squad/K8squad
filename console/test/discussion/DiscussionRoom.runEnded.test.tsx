// test/discussion/DiscussionRoom.runEnded.test.tsx — ISI-7273 (WS-3 of parent
// ISI-7262): the run-ended re-fetch fallback. The room collects the run id off
// a `thinking` envelope, tails that run's per-run SSE for the terminal
// `ended` milestone, and on it re-fetches the thread — so the durable final
// message materializes with NO message.created frame having arrived. Plus the
// guard rails: one fire per run (replay-proof), a coalesced re-fetch across a
// fan-out's near-simultaneous endings, and no watch for legacy envelopes that
// carry no run id.

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor, act } from "@testing-library/react";
import { DiscussionRoom } from "@/components/discussion/DiscussionRoom";
import {
  RunEndedWatch,
  isEndedMilestone,
} from "@/components/discussion/RunEndedWatch";
import type { DiscussionClient } from "@/lib/discussion/api";
import type { Message } from "@/lib/discussion/types";
import type { RoomStreamEvent } from "@/lib/discussion/liveFeed";

// ---- FakeEventSource: records per-URL instances so tests can find the stream
// a mounted watch opened, and can emit named SSE events into its listeners.

type Listener = (ev: { lastEventId: string; data: string }) => void;

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  url: string;
  closed = false;
  onopen: unknown = null;
  onerror: unknown = null;
  onmessage: unknown = null;
  private listeners = new Map<string, Listener[]>();
  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }
  addEventListener(name: string, fn: Listener) {
    const arr = this.listeners.get(name) ?? [];
    arr.push(fn);
    this.listeners.set(name, arr);
  }
  removeEventListener(name: string, fn: Listener) {
    this.listeners.set(
      name,
      (this.listeners.get(name) ?? []).filter((f) => f !== fn),
    );
  }
  close() {
    this.closed = true;
  }
  /** Emit a named SSE event whose data payload is JSON.stringify(payload). */
  emit(name: string, payload: unknown) {
    for (const fn of this.listeners.get(name) ?? []) {
      fn({ lastEventId: "1", data: JSON.stringify(payload) });
    }
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
    createdAt: "2026-11-10T10:00:00Z",
  };
}

function agentFinal(id: string): Message {
  return {
    id,
    threadId: "th-1",
    parentId: null,
    authorPrincipal: "john",
    authorAgentId: "agent-john",
    authorRunId: "r1",
    body: "the durable final answer",
    createdAt: "2026-11-10T10:01:00Z",
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
    postMessage: vi.fn(),
    searchMentions: vi.fn().mockResolvedValue([]),
    getRoster: vi.fn(),
    retractMessage: vi.fn(),
    listProposals: vi.fn().mockResolvedValue([]),
    ...overrides,
  } as DiscussionClient;
}

function thinkingEvent(runId: string): RoomStreamEvent {
  return {
    kind: "thinking",
    row: {
      runId,
      author: "agent:john",
      body: "[run r1] reading the thread",
      seq: 1,
      at: "2026-11-10T10:00:30Z",
    },
  };
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.unstubAllGlobals();
  FakeEventSource.instances = [];
});

describe("isEndedMilestone", () => {
  it("matches only the terminal milestone forms", () => {
    expect(isEndedMilestone("ended")).toBe(true);
    expect(isEndedMilestone("ended: review_comment → done")).toBe(true);
    expect(isEndedMilestone("started")).toBe(false);
    expect(isEndedMilestone("assigned: intake → queued")).toBe(false);
    expect(isEndedMilestone(undefined)).toBe(false);
  });
});

describe("DiscussionRoom — run-ended re-fetch (ISI-7273)", () => {
  it("re-fetches the thread on the dispatched run's ended milestone — no message.created frame needed", async () => {
    vi.stubGlobal("EventSource", FakeEventSource);
    let emit: (e: RoomStreamEvent) => void = () => {};
    const subscribe = (onEvent: (e: RoomStreamEvent) => void) => {
      emit = onEvent;
      return () => {};
    };
    const getThread = vi
      .fn()
      .mockResolvedValueOnce([humanMsg("m1", "hello room")])
      .mockResolvedValue([humanMsg("m1", "hello room"), agentFinal("m2")]);
    const client = makeClient({ getThread });

    render(
      <DiscussionRoom
        projectId="ns/proj"
        threadId="th-1"
        client={client}
        subscribe={subscribe}
      />,
    );
    await waitFor(() => screen.getByTestId("discussion-room"));
    expect(getThread).toHaveBeenCalledTimes(1);

    // The dispatched run streams a thinking envelope — the room learns its id
    // (the only FE-side run-id source) and opens the per-run stream.
    act(() => emit(thinkingEvent("r1")));
    await waitFor(() => {
      expect(
        FakeEventSource.instances.some((e) => e.url === "/api/runs/r1/stream"),
      ).toBe(true);
    });

    // The run ends. NO message.created frame is ever emitted (the missed live
    // push — the parent ISI-7262 symptom).
    const es = FakeEventSource.instances.find(
      (e) => e.url === "/api/runs/r1/stream",
    )!;
    act(() => es.emit("ended", { event: "ended", run_id: "r1" }));

    // The coalesced re-fetch materializes the durable final message.
    await waitFor(
      () => expect(screen.getByText("the durable final answer")).toBeTruthy(),
      { timeout: 3000 },
    );
    expect(getThread).toHaveBeenCalledTimes(2);
    // The watch is dropped once fired — its per-run stream is closed.
    await waitFor(() => expect(es.closed).toBe(true));
  });

  it("collapses a replayed ended and a same-window second ending into ONE re-fetch", async () => {
    vi.stubGlobal("EventSource", FakeEventSource);
    let emit: (e: RoomStreamEvent) => void = () => {};
    const subscribe = (onEvent: (e: RoomStreamEvent) => void) => {
      emit = onEvent;
      return () => {};
    };
    const getThread = vi
      .fn()
      .mockResolvedValueOnce([humanMsg("m1", "hello room")])
      .mockResolvedValue([humanMsg("m1", "hello room"), agentFinal("m2")]);
    const client = makeClient({ getThread });

    render(
      <DiscussionRoom
        projectId="ns/proj"
        threadId="th-1"
        client={client}
        subscribe={subscribe}
      />,
    );
    await waitFor(() => screen.getByTestId("discussion-room"));

    // A two-agent fan-out: both runs stream thinking envelopes.
    act(() => emit(thinkingEvent("r1")));
    act(() => emit(thinkingEvent("r2")));
    await waitFor(() => expect(FakeEventSource.instances.length).toBe(2));
    const es1 = FakeEventSource.instances[0];
    const es2 = FakeEventSource.instances[1];

    // r1 ends, the milestone REPLAYS (SSE reconnect), and r2 ends — all inside
    // the coalescing window. Exactly ONE re-fetch may run.
    act(() => es1.emit("ended", { event: "ended", run_id: "r1" }));
    act(() => es1.emit("ended", { event: "ended", run_id: "r1" }));
    act(() => es2.emit("ended", { event: "ended", run_id: "r2" }));

    await waitFor(
      () => expect(screen.getByText("the durable final answer")).toBeTruthy(),
      { timeout: 3000 },
    );
    expect(getThread).toHaveBeenCalledTimes(2); // initial load + ONE re-fetch
    await waitFor(() => expect(es1.closed && es2.closed).toBe(true));
  });

  it("opens no per-run stream for legacy thinking envelopes without a run id", async () => {
    vi.stubGlobal("EventSource", FakeEventSource);
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
      />,
    );
    await waitFor(() => screen.getByTestId("discussion-room"));

    act(() => emit(thinkingEvent("")));
    await act(async () => {
      await new Promise((r) => setTimeout(r, 50));
    });
    expect(FakeEventSource.instances.length).toBe(0);
  });
});

describe("RunEndedWatch (unit)", () => {
  it("fires onEnded exactly once even when the ended milestone replays", async () => {
    vi.stubGlobal("EventSource", FakeEventSource);
    const onEnded = vi.fn();
    render(<RunEndedWatch runId="r9" onEnded={onEnded} />);
    const es = FakeEventSource.instances.find(
      (e) => e.url === "/api/runs/r9/stream",
    )!;
    expect(es).toBeTruthy();

    act(() => es.emit("ended", { event: "ended", run_id: "r9" }));
    await waitFor(() => expect(onEnded).toHaveBeenCalledTimes(1));

    // A reconnect replays the durable tail — the same ended re-arrives.
    act(() => es.emit("ended", { event: "ended", run_id: "r9" }));
    await act(async () => {
      await new Promise((r) => setTimeout(r, 50));
    });
    expect(onEnded).toHaveBeenCalledTimes(1);
  });
});
