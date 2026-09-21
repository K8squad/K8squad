import { describe, it, expect, afterEach, vi } from "vitest";
import { renderHook, act, cleanup } from "@testing-library/react";
import {
  useTeamStatus,
  RECONNECT_GRACE_MS,
} from "@/lib/agents/useTeamStatus";

// ISI-4737 symptom A: the team chip text IS the SSE stream state, and a native EventSource
// auto-reconnects on every ingress/gateway recycle. Escalating to "error" on the first onerror made
// the chip flap live↔error. These tests pin the grace-window behaviour: a brief reconnect never
// surfaces "error"; a fatal (CLOSED) socket surfaces it at once; a sustained outage escalates only
// after the grace window.

// Controllable EventSource stub — jsdom has none, and we need to drive onopen/onerror + readyState.
class FakeEventSource {
  static CONNECTING = 0 as const;
  static OPEN = 1 as const;
  static CLOSED = 2 as const;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((e: MessageEvent) => void) | null = null;
  url: string;
  readyState: number = FakeEventSource.CONNECTING;
  closed = false;
  constructor(url: string) {
    this.url = url;
  }
  close() {
    this.closed = true;
    this.readyState = FakeEventSource.CLOSED;
  }
}

function stubEventSource() {
  const instances: FakeEventSource[] = [];
  const ctor = vi.fn((url: string) => {
    const es = new FakeEventSource(url);
    instances.push(es);
    return es;
  });
  // Expose the static readyState enum the hook reads (EventSource.CLOSED).
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
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("useTeamStatus SSE resilience", () => {
  it("starts connecting and flips to open on connect", () => {
    const instances = stubEventSource();
    const { result } = renderHook(() => useTeamStatus("team-1"));
    expect(result.current.status).toBe("connecting");

    act(() => {
      instances[0].readyState = FakeEventSource.OPEN;
      instances[0].onopen?.();
    });
    expect(result.current.status).toBe("open");
  });

  it("holds 'open' through a brief reconnect — no error flap within the grace window", () => {
    vi.useFakeTimers();
    const instances = stubEventSource();
    const { result } = renderHook(() => useTeamStatus("team-1"));

    act(() => {
      instances[0].readyState = FakeEventSource.OPEN;
      instances[0].onopen?.();
    });
    expect(result.current.status).toBe("open");

    // Transient drop: browser is reconnecting (readyState CONNECTING).
    act(() => {
      instances[0].readyState = FakeEventSource.CONNECTING;
      instances[0].onerror?.();
    });
    // Still "open" — a brief blip must not surface as an alarming error.
    expect(result.current.status).toBe("open");

    // Advance PART of the grace window, then reconnect succeeds.
    act(() => {
      vi.advanceTimersByTime(RECONNECT_GRACE_MS - 1);
      instances[0].readyState = FakeEventSource.OPEN;
      instances[0].onopen?.();
    });
    expect(result.current.status).toBe("open");

    // The escalation timer must have been cancelled — even past the original window.
    act(() => {
      vi.advanceTimersByTime(RECONNECT_GRACE_MS);
    });
    expect(result.current.status).toBe("open");
  });

  it("escalates to 'error' only after the grace window on a sustained outage", () => {
    vi.useFakeTimers();
    const instances = stubEventSource();
    const { result } = renderHook(() => useTeamStatus("team-1"));

    act(() => {
      instances[0].readyState = FakeEventSource.OPEN;
      instances[0].onopen?.();
    });
    act(() => {
      instances[0].readyState = FakeEventSource.CONNECTING;
      instances[0].onerror?.();
    });
    expect(result.current.status).toBe("open");

    act(() => {
      vi.advanceTimersByTime(RECONNECT_GRACE_MS);
    });
    expect(result.current.status).toBe("error");
  });

  it("surfaces 'error' immediately when the socket is CLOSED (fatal, no reconnect)", () => {
    const instances = stubEventSource();
    const { result } = renderHook(() => useTeamStatus("team-1"));

    act(() => {
      instances[0].readyState = FakeEventSource.CLOSED;
      instances[0].onerror?.();
    });
    expect(result.current.status).toBe("error");
  });
});
