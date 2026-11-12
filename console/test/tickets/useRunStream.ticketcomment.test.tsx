// test/tickets/useRunStream.ticketcomment.test.tsx — ISI-7274 (parent ISI-7262 WS-2).
//
// The per-run SSE client learns a new NAMED event, `ticket_comment`: the
// durable work-item comment echo (the agent's final task-io comment and the
// settle's terminal rows) bridged onto the EXISTING run stream so the ticket
// page materializes the thread's final comment live — without racing the run's
// `ended` milestone (the settle commits after it). This proves the wire
// contract: a `ticket_comment` frame becomes a COMMENT RunEvent carrying the
// durable comment's (author→actor, body→summary, at→ts), a body-less frame is
// dropped, and the listener is torn down on unmount.

import { describe, it, expect, afterEach, vi } from "vitest";
import { renderHook, act, cleanup } from "@testing-library/react";
import { useRunStream } from "@/lib/useRunStream";

// Controllable EventSource stub (jsdom has none): captures the per-name listeners
// so a test can push a `ticket_comment` frame and assert what the hook accumulated.
class FakeEventSource {
  static CONNECTING = 0 as const;
  static OPEN = 1 as const;
  static CLOSED = 2 as const;
  url: string;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((e: MessageEvent) => void) | null = null;
  listeners = new Map<string, (e: MessageEvent) => void>();
  closed = false;
  constructor(url: string) {
    this.url = url;
  }
  addEventListener(name: string, fn: (e: MessageEvent) => void) {
    this.listeners.set(name, fn);
  }
  removeEventListener(name: string) {
    this.listeners.delete(name);
  }
  emit(name: string, data: unknown, lastEventId = "evt-1") {
    this.listeners.get(name)?.({
      data: JSON.stringify(data),
      lastEventId,
    } as MessageEvent);
  }
  close() {
    this.closed = true;
  }
}

function stubEventSource() {
  const instances: FakeEventSource[] = [];
  const ctor = vi.fn((url: string) => {
    const es = new FakeEventSource(url);
    instances.push(es);
    return es;
  });
  Object.assign(ctor, {
    CONNECTING: FakeEventSource.CONNECTING,
    OPEN: FakeEventSource.OPEN,
    CLOSED: FakeEventSource.CLOSED,
  });
  vi.stubGlobal("EventSource", ctor as unknown as typeof EventSource);
  return instances;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("useRunStream — ticket_comment event (ISI-7274)", () => {
  it("shapes a `ticket_comment` frame into a COMMENT RunEvent (durable triple)", () => {
    const instances = stubEventSource();
    const { result } = renderHook(() => useRunStream("run-9"));
    act(() => {
      instances[0].emit(
        "ticket_comment",
        {
          workItemId: "wi-1",
          runId: "run-9",
          author: "agent:sam",
          body: "What I did: delivered the seam (ISI-7274).",
          kind: "summary",
          at: "2026-11-12T10:00:00.000Z",
        },
        "77",
      );
    });
    expect(result.current.events).toHaveLength(1);
    expect(result.current.events[0]).toEqual({
      id: "77",
      kind: "COMMENT",
      actor: "agent:sam",
      ts: "2026-11-12T10:00:00.000Z",
      summary: "What I did: delivered the seam (ISI-7274).",
    });
  });

  it("drops a body-less `ticket_comment` frame (nothing durable to materialize)", () => {
    const instances = stubEventSource();
    const { result } = renderHook(() => useRunStream("run-9"));
    act(() => {
      instances[0].emit("ticket_comment", {
        workItemId: "wi-1",
        runId: "run-9",
        author: "agent:sam",
        body: "",
      });
    });
    expect(result.current.events).toHaveLength(0);
  });

  it("falls back to arrival time when the echo's `at` is absent", () => {
    const instances = stubEventSource();
    const { result } = renderHook(() => useRunStream("run-9"));
    const before = new Date().toISOString();
    act(() => {
      instances[0].emit("ticket_comment", {
        workItemId: "wi-1",
        runId: "run-9",
        author: "ksquad-operator",
        body: "Run succeeded.",
      });
    });
    expect(result.current.events).toHaveLength(1);
    expect(result.current.events[0].ts >= before).toBe(true);
  });

  it("removes the ticket_comment listener and closes the socket on unmount", () => {
    const instances = stubEventSource();
    const { unmount } = renderHook(() => useRunStream("run-9"));
    expect(instances[0].listeners.has("ticket_comment")).toBe(true);
    unmount();
    expect(instances[0].listeners.has("ticket_comment")).toBe(false);
    expect(instances[0].closed).toBe(true);
  });
});
