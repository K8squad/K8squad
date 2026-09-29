// test/tickets/useRunStream.thinking.test.tsx — ISI-5193 (S4 of ISI-5185/WS-A).
//
// The per-run SSE client learns a new NAMED event, `thinking`: the operator's
// progress mirror echoed onto the EXISTING run stream (no second EventSource,
// ISI-5174 AC4) so the ticket Activity feed goes live. This proves the wire
// contract — a `thinking` frame becomes a THINKING RunEvent carrying the
// mirrored comment's (author→actor, body→summary, at→ts) — and that a body-less
// frame is dropped.

import { describe, it, expect, afterEach, vi } from "vitest";
import { renderHook, act, cleanup } from "@testing-library/react";
import { useRunStream } from "@/lib/useRunStream";

// Controllable EventSource stub (jsdom has none): captures the per-name listeners
// so a test can push a `thinking` frame and assert what the hook accumulated.
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
    this.listeners.get(name)?.({ data: JSON.stringify(data), lastEventId } as MessageEvent);
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

describe("useRunStream — thinking event (ISI-5193)", () => {
  it("opens exactly one EventSource against the BFF run stream", () => {
    const instances = stubEventSource();
    renderHook(() => useRunStream("run-9"));
    expect(instances).toHaveLength(1);
    expect(instances[0].url).toBe("/api/runs/run-9/stream");
  });

  it("shapes a `thinking` frame into a THINKING RunEvent", () => {
    const instances = stubEventSource();
    const { result } = renderHook(() => useRunStream("run-9"));
    act(() => {
      instances[0].emit(
        "thinking",
        {
          runId: "run-9",
          author: "agent:sam",
          body: "[run run-9][untrusted] reading the repo…",
          seq: 3,
          at: "2026-09-29T10:00:00.000Z",
        },
        "42",
      );
    });
    expect(result.current.events).toHaveLength(1);
    expect(result.current.events[0]).toEqual({
      id: "42",
      kind: "THINKING",
      actor: "agent:sam",
      ts: "2026-09-29T10:00:00.000Z",
      summary: "[run run-9][untrusted] reading the repo…",
    });
  });

  it("drops a body-less `thinking` frame (nothing to render)", () => {
    const instances = stubEventSource();
    const { result } = renderHook(() => useRunStream("run-9"));
    act(() => {
      instances[0].emit("thinking", { runId: "run-9", author: "agent:sam", body: "" });
    });
    expect(result.current.events).toHaveLength(0);
  });

  it("removes the thinking listener and closes the socket on unmount", () => {
    const instances = stubEventSource();
    const { unmount } = renderHook(() => useRunStream("run-9"));
    expect(instances[0].listeners.has("thinking")).toBe(true);
    unmount();
    expect(instances[0].listeners.has("thinking")).toBe(false);
    expect(instances[0].closed).toBe(true);
  });
});
