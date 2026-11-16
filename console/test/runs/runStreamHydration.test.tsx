// test/runs/runStreamHydration.test.tsx — ISI-7484 (WS-3, parents
// ISI-7482/ISI-7480): durable Activity hydration.
//
// A reload / late join on a post-ISI-7074 run must NOT show an empty Activity:
// useRunStream seeds its event list from the durable REST tail (GET
// /api/runs/{id}/events) on mount, then hands off to the live SSE tail with
// dedupe on the shared outbox row id. Proves:
//   • hydrated history renders on mount (fetch called once, right URL);
//   • live frames append AFTER hydrated rows in id order, even when the
//     hydration fetch resolves after the first live frame;
//   • a replayed id (reconnect replay of an already-hydrated row) does not
//     duplicate;
//   • hydration failure (non-OK) is best-effort: the live tail still renders.

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, act, cleanup, waitFor } from "@testing-library/react";

import { RunStream } from "@/components/RunStream";

// Controllable EventSource stub (jsdom has none) — same shape as
// runStream.test.tsx, so the two suites stay interchangeable.
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

function jsonResponse(body: unknown, ok = true) {
  return {
    ok,
    json: async () => body,
  } as unknown as Response;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("RunStream durable hydration (ISI-7484)", () => {
  it("hydrates history on mount, then appends live frames in id order without duplicates", async () => {
    const instances = stubEventSource();
    const fetchMock = vi.fn(async (input: unknown) =>
      jsonResponse({
        runId: "uid-1",
        count: 2,
        lastId: "9",
        events: [
          {
            id: "5",
            type: "thinking",
            data: { author: "sam", body: "hydrated first step", at: "2026-11-16T08:00:00Z" },
            at: "2026-11-16T08:00:00Z",
          },
          {
            id: "9",
            type: "ended",
            data: { agent: "sam", from_step: "drive", to_step: "settle" },
            at: "2026-11-16T08:00:05Z",
          },
        ],
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    render(<RunStream runId="intake-r1" />);

    // A live frame arrives BEFORE the hydration fetch resolves.
    await act(async () => {
      instances[0].emit("thinking", { author: "sam", body: "live tail frame" }, "12");
    });

    // Now let the hydration promise land.
    await waitFor(() => {
      expect(document.body.textContent).toContain("hydrated first step");
    });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0][0]).toBe("/api/runs/intake-r1/events");
    // Exactly ONE EventSource — hydration never opens a second stream (AC4).
    expect(instances).toHaveLength(1);

    const rows = Array.from(document.querySelectorAll(".run-event"));
    // 5 (hydrated), 9 (hydrated ended), 12 (live) — id order, no dupes.
    expect(rows).toHaveLength(3);
    expect(rows[0].textContent).toContain("hydrated first step");
    expect(rows[1].textContent).toContain("ended");
    expect(rows[2].textContent).toContain("live tail frame");

    // A reconnect replay of an already-hydrated id must NOT duplicate.
    await act(async () => {
      instances[0].emit(
        "thinking",
        { author: "sam", body: "hydrated first step", at: "2026-11-16T08:00:00Z" },
        "5",
      );
    });
    expect(document.querySelectorAll(".run-event")).toHaveLength(3);
  });

  it("keeps the honest empty state when the durable tail is empty and nothing is live", async () => {
    stubEventSource();
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => jsonResponse({ runId: "uid-1", count: 0, events: [] })),
    );
    render(<RunStream runId="intake-r2" />);
    await act(async () => {});
    expect(document.body.textContent).toContain("No events yet");
  });

  it("survives a failed hydration fetch — the live tail still renders", async () => {
    const instances = stubEventSource();
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "nope" }, false)));
    render(<RunStream runId="intake-r3" />);
    await act(async () => {
      instances[0].emit("thinking", { author: "sam", body: "live only" }, "2");
    });
    await waitFor(() => {
      expect(document.body.textContent).toContain("live only");
    });
    expect(document.querySelectorAll(".run-event")).toHaveLength(1);
  });
});

