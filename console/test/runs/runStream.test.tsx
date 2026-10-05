// test/runs/runStream.test.tsx — ISI-5457 (Blueprint item 5, Lane C G1).
//
// RunStream must notify its parent (RunDetail) whenever a lifecycle milestone
// arrives so the otherwise fetch-once run-detail snapshot can refetch and stop
// going stale (a finished run showing "Running" until a manual reload). Proves:
//   • onMilestone fires on each distinct lifecycle milestone (started, ended…);
//   • it does NOT fire on non-lifecycle frames (thinking/comment/step);
//   • a replayed milestone (SSE reconnect, same outbox id) does NOT re-fire;
//   • only one EventSource is opened (no second stream for the refetch trigger).

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, act, cleanup } from "@testing-library/react";

import { RunStream } from "@/components/RunStream";

// Controllable EventSource stub (jsdom has none) — captures per-name listeners so
// a test can push named SSE frames. Mirrors the stub in useTicketRunStream.test.
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
    this.listeners
      .get(name)
      ?.({ data: JSON.stringify(data), lastEventId } as MessageEvent);
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

describe("RunStream onMilestone — refetch trigger (ISI-5457)", () => {
  it("opens exactly one EventSource (no second stream for the trigger)", () => {
    const instances = stubEventSource();
    render(<RunStream runId="run-7" onMilestone={() => {}} />);
    expect(instances).toHaveLength(1);
    expect(instances[0].url).toBe("/api/runs/run-7/stream");
  });

  it("fires onMilestone on each distinct lifecycle milestone", () => {
    const instances = stubEventSource();
    const onMilestone = vi.fn();
    render(<RunStream runId="run-7" onMilestone={onMilestone} />);
    act(() => {
      instances[0].emit("started", { run_id: "run-7", agent: "sam" }, "evt-1");
    });
    expect(onMilestone).toHaveBeenCalledTimes(1);
    act(() => {
      instances[0].emit("ended", { run_id: "run-7", agent: "sam" }, "evt-2");
    });
    expect(onMilestone).toHaveBeenCalledTimes(2);
  });

  it("does NOT fire on non-lifecycle frames (thinking)", () => {
    const instances = stubEventSource();
    const onMilestone = vi.fn();
    render(<RunStream runId="run-7" onMilestone={onMilestone} />);
    act(() => {
      instances[0].emit(
        "thinking",
        { runId: "run-7", author: "agent:sam", body: "working", at: "2026-09-29T10:00:00.000Z" },
        "evt-9",
      );
    });
    expect(onMilestone).not.toHaveBeenCalled();
  });

  it("does NOT re-fire on a replayed milestone (same outbox id, SSE reconnect)", () => {
    const instances = stubEventSource();
    const onMilestone = vi.fn();
    render(<RunStream runId="run-7" onMilestone={onMilestone} />);
    act(() => {
      instances[0].emit("ended", { run_id: "run-7", agent: "sam" }, "evt-5");
    });
    expect(onMilestone).toHaveBeenCalledTimes(1);
    // reconnect replays the SAME event id → must not re-trigger a refetch.
    act(() => {
      instances[0].emit("ended", { run_id: "run-7", agent: "sam" }, "evt-5");
    });
    expect(onMilestone).toHaveBeenCalledTimes(1);
  });
});
