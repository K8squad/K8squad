// test/tickets/useTicketRunStream.test.tsx — ISI-5206 (S3 of ISI-5202).
//
// The ticket view must live-tail ANY active run, not only a run it dispatched
// itself this session. This proves:
//   • the gating rule (ambientRunId): open the stream for the holding run only
//     when it is live AND the self-dispatch ladder isn't already streaming it;
//   • a `thinking` frame on that ambient stream surfaces as a LiveThinkingRow;
//   • the run's terminal `ended` milestone fires onEnded exactly once (the caller
//     re-fetches the thread → durable tail + running state cleared, no refresh);
//   • an inert run (no id / terminal lane / self-dispatch overlap) opens nothing.

import { describe, it, expect, afterEach, vi } from "vitest";
import { renderHook, act, cleanup } from "@testing-library/react";
import {
  ambientRunId,
  useTicketRunStream,
} from "@/lib/tickets/useTicketRunStream";

// Controllable EventSource stub (jsdom has none) — captures per-name listeners so
// a test can push named SSE frames and assert what the hook accumulated.
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

describe("ambientRunId — the gating rule (ISI-5206)", () => {
  it("opens the holding run when it is live and unclaimed by the ladder", () => {
    expect(ambientRunId("run-7", "in_progress", undefined)).toBe("run-7");
    expect(ambientRunId("run-7", "in_review", "")).toBe("run-7");
  });

  it("is inert with no holding run", () => {
    expect(ambientRunId("", "in_progress", undefined)).toBe("");
  });

  it("is inert on a terminal / intake lane (nothing live to tail)", () => {
    expect(ambientRunId("run-7", "done", undefined)).toBe("");
    expect(ambientRunId("run-7", "backlog", undefined)).toBe("");
    expect(ambientRunId("run-7", "todo", undefined)).toBe("");
    expect(ambientRunId("run-7", "cancelled", undefined)).toBe("");
  });

  it("defers to the self-dispatch ladder when it already streams this run (one EventSource)", () => {
    expect(ambientRunId("run-7", "in_progress", "run-7")).toBe("");
    // a DIFFERENT self-dispatch run does not suppress the ambient one
    expect(ambientRunId("run-7", "in_progress", "run-other")).toBe("run-7");
  });
});

describe("useTicketRunStream — live tail + terminal resolve (ISI-5206)", () => {
  it("opens exactly one EventSource against the ambient run's BFF stream", () => {
    const instances = stubEventSource();
    renderHook(() => useTicketRunStream("run-7", "in_progress", undefined, () => {}));
    expect(instances).toHaveLength(1);
    expect(instances[0].url).toBe("/api/runs/run-7/stream");
  });

  it("opens NO stream when inert (terminal lane)", () => {
    const instances = stubEventSource();
    renderHook(() => useTicketRunStream("run-7", "done", undefined, () => {}));
    expect(instances).toHaveLength(0);
  });

  it("opens NO stream when the self-dispatch ladder already owns the run", () => {
    const instances = stubEventSource();
    renderHook(() => useTicketRunStream("run-7", "in_progress", "run-7", () => {}));
    expect(instances).toHaveLength(0);
  });

  it("surfaces a `thinking` frame as a LiveThinkingRow (author/body/at)", () => {
    const instances = stubEventSource();
    const { result } = renderHook(() =>
      useTicketRunStream("run-7", "in_progress", undefined, () => {}),
    );
    act(() => {
      instances[0].emit("thinking", {
        runId: "run-7",
        author: "agent:sam",
        body: "[run run-7][tool:read/start] repo.ts",
        at: "2026-09-29T10:00:00.000Z",
      });
    });
    expect(result.current).toEqual([
      {
        author: "agent:sam",
        body: "[run run-7][tool:read/start] repo.ts",
        at: "2026-09-29T10:00:00.000Z",
      },
    ]);
  });

  it("fires onEnded exactly once on the run's terminal `ended` milestone", () => {
    const instances = stubEventSource();
    const onEnded = vi.fn();
    renderHook(() => useTicketRunStream("run-7", "in_progress", undefined, onEnded));
    act(() => {
      instances[0].emit("ended", { run_id: "run-7", agent: "sam" });
    });
    expect(onEnded).toHaveBeenCalledTimes(1);
    // a replayed `ended` (SSE reconnect) on the SAME stream must not re-fire.
    act(() => {
      instances[0].emit("ended", { run_id: "run-7", agent: "sam" });
    });
    expect(onEnded).toHaveBeenCalledTimes(1);
  });
});
