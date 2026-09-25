// test/tickets/useDispatchWatch.test.tsx — ISI-4879 (S1 of ISI-4853).
//
// Two layers:
//  1. The PURE reducer as the "mock signal source" — every ladder transition, the honesty guard
//     (never "working" before phase Running / SSE started), monotonicity, terminal freeze, and both
//     degrade thresholds (only while queued, cleared on advance).
//  2. The hook wiring — synchronous "queued" on seed, the OQ4 discovery poll → runId → SSE hand-off,
//     the wall-clock degrade timers, and full teardown (no leaked timer / EventSource).

import { describe, it, expect, afterEach, vi } from "vitest";
import { renderHook, act, cleanup } from "@testing-library/react";
import {
  reduceDispatchWatch,
  initialDispatchMachine,
  toDispatchWatch,
  pickDispatchRun,
  useDispatchWatch,
  DISPATCH_SOFT_MS,
  DISPATCH_STALLED_MS,
  DISPATCH_POLL_INTERVAL_MS,
  type DispatchMachine,
  type WatchSignal,
} from "@/lib/tickets/useDispatchWatch";

// Drive a chain of signals through the pure reducer from a fresh (or given) machine.
function run(sigs: WatchSignal[], from?: DispatchMachine): DispatchMachine {
  return sigs.reduce(reduceDispatchWatch, from ?? initialDispatchMachine());
}

// ── Layer 1: pure reducer ───────────────────────────────────────────────────────────────────────

describe("reduceDispatchWatch — ladder transitions", () => {
  it("seeds at queued (label 'Queued', not 'Working')", () => {
    const w = toDispatchWatch(initialDispatchMachine());
    expect(w.state).toBe("queued");
    expect(w.label).toBe("Queued");
    expect(w.tone).toBe("idle");
    expect(w.degrade).toBe("none");
  });

  it("run_discovered ⇒ picking_up and records the runId", () => {
    const m = run([{ type: "run_discovered", runId: "run-1" }]);
    expect(m.state).toBe("picking_up");
    expect(m.runId).toBe("run-1");
  });

  it("advances queued → picking_up on phase Pending / Claiming and SSE assigned/scheduled/sandbox_bound", () => {
    for (const sig of [
      { type: "phase", phase: "Pending" } as const,
      { type: "phase", phase: "Claiming" } as const,
      { type: "phase", phase: "Paused" } as const,
      { type: "lifecycle", name: "assigned" } as const,
      { type: "lifecycle", name: "scheduled" } as const,
      { type: "lifecycle", name: "sandbox_bound" } as const,
    ]) {
      expect(run([sig]).state).toBe("picking_up");
    }
  });

  it("advances to working ONLY on phase Running or SSE started (honesty guard)", () => {
    expect(run([{ type: "phase", phase: "Running" }]).state).toBe("working");
    expect(run([{ type: "lifecycle", name: "started" }]).state).toBe("working");
    expect(toDispatchWatch(run([{ type: "phase", phase: "Running" }])).label).toBe(
      "Working…",
    );
  });

  it("NEVER reaches working via picking-up signals (Claiming/assigned/scheduled/sandbox_bound)", () => {
    for (const sig of [
      { type: "phase", phase: "Claiming" } as const,
      { type: "lifecycle", name: "assigned" } as const,
      { type: "lifecycle", name: "scheduled" } as const,
      { type: "lifecycle", name: "sandbox_bound" } as const,
    ]) {
      const w = toDispatchWatch(run([sig]));
      expect(w.state).not.toBe("working");
      expect(w.label).not.toBe("Working…");
    }
  });

  it("resolves terminal: Succeeded/ended → succeeded, Failed/Cancelled → failed", () => {
    expect(run([{ type: "phase", phase: "Succeeded" }]).state).toBe("succeeded");
    expect(run([{ type: "phase", phase: "Failed" }]).state).toBe("failed");
    expect(run([{ type: "phase", phase: "Cancelled" }]).state).toBe("failed");
    expect(run([{ type: "lifecycle", name: "ended" }]).state).toBe("succeeded");
  });

  it("is monotonic — a late/duplicate lower signal can't regress the ladder", () => {
    const m = run([
      { type: "phase", phase: "Running" }, // working
      { type: "lifecycle", name: "assigned" }, // stale picking_up signal
      { type: "phase", phase: "Pending" }, // stale
    ]);
    expect(m.state).toBe("working");
  });

  it("freezes once terminal — no zombie transitions", () => {
    const m = run([
      { type: "phase", phase: "Failed" },
      { type: "lifecycle", name: "ended" },
      { type: "phase", phase: "Running" },
    ]);
    expect(m.state).toBe("failed");
  });

  it("reset rearms to a fresh queued machine", () => {
    const m = run([
      { type: "phase", phase: "Running" },
      { type: "reset" },
    ]);
    expect(m).toEqual(initialDispatchMachine());
  });
});

describe("reduceDispatchWatch — degrade ladder", () => {
  it("emits soft then stalled while still queued", () => {
    let m = run([{ type: "degrade", level: "soft" }]);
    expect(m.degrade).toBe("soft");
    expect(m.state).toBe("queued");
    m = run([{ type: "degrade", level: "stalled" }], m);
    expect(m.degrade).toBe("stalled");
  });

  it("clears degrade the instant the ladder advances past queued", () => {
    const m = run([
      { type: "degrade", level: "stalled" },
      { type: "phase", phase: "Pending" }, // → picking_up
    ]);
    expect(m.state).toBe("picking_up");
    expect(m.degrade).toBe("none");
  });

  it("ignores degrade once past queued (never degrades a live run)", () => {
    const m = run([
      { type: "phase", phase: "Running" }, // working
      { type: "degrade", level: "stalled" },
    ]);
    expect(m.degrade).toBe("none");
  });
});

// ── pickDispatchRun (the discovery matcher) ──────────────────────────────────────────────────────

describe("pickDispatchRun", () => {
  it("matches on workItemRef and returns null when nothing matches", () => {
    expect(pickDispatchRun([], "wi-1", "alice")).toBeNull();
    expect(
      pickDispatchRun([{ id: "r1", workItemRef: "other" }], "wi-1", "alice"),
    ).toBeNull();
    expect(
      pickDispatchRun([{ workItemRef: "wi-1" }], "wi-1", "alice"),
    ).toBeNull(); // no id → not usable
  });

  it("prefers the row whose agents include the dispatched agent", () => {
    const row = pickDispatchRun(
      [
        { id: "r-bob", workItemRef: "wi-1", agents: ["bob"] },
        { id: "r-alice", workItemRef: "wi-1", agents: ["alice"] },
      ],
      "wi-1",
      "alice",
    );
    expect(row?.id).toBe("r-alice");
  });

  it("takes the newest by startedAt among matches", () => {
    const row = pickDispatchRun(
      [
        { id: "old", workItemRef: "wi-1", startedAt: "2026-01-01T00:00:00Z" },
        { id: "new", workItemRef: "wi-1", startedAt: "2026-06-01T00:00:00Z" },
      ],
      "wi-1",
      "alice",
    );
    expect(row?.id).toBe("new");
  });

  // ISI-4918: a re-dispatched ticket carries run rows from its PREVIOUS dispatch — the
  // matcher must ignore them or the ladder collapses to the old run's terminal state.
  it("ignores rows that predate the dispatch (notBeforeMs)", () => {
    const now = Date.parse("2026-09-25T10:06:00Z");
    const stale = { id: "prev", workItemRef: "wi-1", agents: ["alice"], phase: "Succeeded",
                    startedAt: "2026-09-25T09:58:00Z" };
    const freshPending = { id: "next", workItemRef: "wi-1", agents: ["alice"], phase: "Pending" };

    // No notBefore → legacy behavior (matches the newest row, even a stale one).
    expect(pickDispatchRun([stale], "wi-1", "alice")?.id).toBe("prev");
    // With notBefore: the stale Succeeded row is excluded, the timestamp-less Pending row wins.
    expect(pickDispatchRun([stale, freshPending], "wi-1", "alice", now)?.id).toBe("next");
    // Only stale rows → nothing eligible yet (ladder stays queued until the new Run mints).
    expect(pickDispatchRun([stale], "wi-1", "alice", now)).toBeNull();
  });

  // ISI-4918 live regression: the apiserver's runListItem emits Go zero-time endedAt
  // ("0001-01-01T00:00:00Z") for every non-complete/failed run. A staleness check over that
  // field makes EVERY row look stale and pins the ladder at queued forever. We must only trust
  // startedAt, and treat zero/negative epochs as "no timestamp".
  it("does not treat Go zero-time startedAt as stale (live regression)", () => {
    const now = Date.parse("2026-09-25T10:06:00Z");
    const freshClaiming = { id: "r22", workItemRef: "wi-1", agents: ["alice"], phase: "Claiming",
                            startedAt: "2026-09-25T10:05:58Z" };
    // A just-minted run whose startedAt is the Go zero time (never claimed yet) must still match.
    const zeroStarted = { id: "r23", workItemRef: "wi-1", agents: ["alice"], phase: "Pending",
                          startedAt: "0001-01-01T00:00:00Z" };
    expect(pickDispatchRun([freshClaiming, zeroStarted], "wi-1", "alice", now)?.id).toBe("r22");
    // With ONLY a zero-time row (and no real staleness signal), the row is kept, not filtered.
    expect(pickDispatchRun([zeroStarted], "wi-1", "alice", now)?.id).toBe("r23");
  });

  it("admits a row started slightly before the dispatch (clock-skew floor)", () => {
    const now = Date.parse("2026-09-25T10:06:00Z");
    // Started 30s before the client's dispatch stamp — inside the 60s skew allowance.
    const row = pickDispatchRun(
      [{ id: "just-now", workItemRef: "wi-1", agents: ["alice"], phase: "Running",
         startedAt: "2026-09-25T10:05:30Z" }],
      "wi-1",
      "alice",
      now,
    );
    expect(row?.id).toBe("just-now");
  });

  it("never falls back into stale rows when only the stale ones match the agent", () => {
    const now = Date.parse("2026-09-25T10:06:00Z");
    const staleAlice = { id: "prev", workItemRef: "wi-1", agents: ["alice"], phase: "Succeeded",
                         startedAt: "2026-09-25T09:58:00Z" };
    const freshBob = { id: "next", workItemRef: "wi-1", agents: ["bob"], phase: "Pending" };
    // Agent match exists only among stale rows → the fallback pool is the FRESH rows, not stale.
    expect(pickDispatchRun([staleAlice, freshBob], "wi-1", "alice", now)?.id).toBe("next");
  });
});

// ── Layer 2: the hook ────────────────────────────────────────────────────────────────────────────

// Controllable EventSource stub (jsdom has none) — captures instances + the per-name listeners so a
// test can push a lifecycle event and assert teardown closed the socket.
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
  emit(name: string, data: unknown) {
    this.listeners.get(name)?.({
      data: JSON.stringify(data),
      lastEventId: "evt-1",
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

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "content-type": "application/json" },
  });
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("useDispatchWatch — hook wiring", () => {
  it("returns null when no dispatch is in flight, queued synchronously on seed", () => {
    stubEventSource();
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse([])));

    const { result, rerender } = renderHook(
      ({ id }: { id: string | null }) => useDispatchWatch(id, "alice"),
      { initialProps: { id: null as string | null } },
    );
    expect(result.current).toBeNull();

    rerender({ id: "wi-1" });
    // Synchronous — the first render after seeding already reads queued (no await on a Run row).
    expect(result.current?.state).toBe("queued");
    expect(result.current?.label).toBe("Queued");
  });

  it("fires soft@15s then stalled@45s while queued, and tears the timers down on unmount", () => {
    vi.useFakeTimers();
    stubEventSource();
    // fetch resolves to no matching run so the ladder stays queued.
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse([])));

    const { result, unmount } = renderHook(() =>
      useDispatchWatch("wi-1", "alice"),
    );
    expect(result.current?.degrade).toBe("none");

    act(() => {
      vi.advanceTimersByTime(DISPATCH_SOFT_MS);
    });
    expect(result.current?.degrade).toBe("soft");

    act(() => {
      vi.advanceTimersByTime(DISPATCH_STALLED_MS - DISPATCH_SOFT_MS);
    });
    expect(result.current?.degrade).toBe("stalled");

    // No throw / no further state churn after unmount ⇒ timers were cleared.
    unmount();
    act(() => {
      vi.advanceTimersByTime(60_000);
    });
  });

  it("bridges queued → picking_up via the discovery poll, then rides SSE to working", async () => {
    const instances = stubEventSource();
    const fetchMock = vi.fn(async () =>
      jsonResponse([
        { id: "run-9", workItemRef: "wi-1", phase: "Pending", agents: ["alice"] },
      ]),
    );
    vi.stubGlobal("fetch", fetchMock as unknown as typeof fetch);

    const { result } = renderHook(() => useDispatchWatch("wi-1", "alice"));

    // First poll lands the Run row → picking_up, runId captured, SSE opens on that runId.
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(result.current?.state).toBe("picking_up");
    expect(result.current?.runId).toBe("run-9");
    expect(fetchMock).toHaveBeenCalledWith("/api/runs", expect.anything());
    const es = instances.find((i) => i.url.includes("run-9"));
    expect(es).toBeTruthy();

    // A live `started` milestone on the shared stream advances to working (honest — phase Running).
    act(() => {
      es!.emit("started", { event: "started", run_id: "run-9" });
    });
    expect(result.current?.state).toBe("working");
    expect(result.current?.label).toBe("Working…");
  });

  it("ignores the PREVIOUS dispatch's run row and stays queued until the new Run mints (ISI-4918)", async () => {
    vi.useFakeTimers();
    stubEventSource();
    let rows: unknown[] = [
      {
        id: "prev-run", workItemRef: "wi-1", phase: "Succeeded", agents: ["alice"],
        startedAt: "2026-01-01T00:00:00Z", endedAt: "2026-01-01T00:01:00Z",
      },
    ];
    const fetchMock = vi.fn(async () => jsonResponse(rows));
    vi.stubGlobal("fetch", fetchMock as unknown as typeof fetch);

    const { result } = renderHook(() => useDispatchWatch("wi-1", "alice"));

    // First poll sees only the PREVIOUS dispatch's Succeeded row → NOT a match: the ladder
    // must stay queued (never collapse to the old run's "Finished" while the new one mints).
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
      await vi.runOnlyPendingTimersAsync();
    });
    expect(result.current?.state).toBe("queued");
    expect(result.current?.runId).toBeUndefined();

    // The operator mints the new Run (Pending, no startedAt yet) → discovered → picking_up.
    rows = [
      ...rows,
      { id: "new-run", workItemRef: "wi-1", phase: "Pending", agents: ["alice"] },
    ];
    await act(async () => {
      await vi.advanceTimersByTimeAsync(DISPATCH_POLL_INTERVAL_MS + 5);
    });
    expect(result.current?.state).toBe("picking_up");
    expect(result.current?.runId).toBe("new-run");
  });

  it("re-seeds on a new rearmKey even for the SAME work item (second nudge, ISI-4918)", async () => {
    stubEventSource();
    const justNow = new Date(Date.now() - 1_000).toISOString();
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse([
          {
            id: "run-1", workItemRef: "wi-1", phase: "Succeeded", agents: ["alice"],
            startedAt: justNow, endedAt: justNow,
          },
        ]),
      ) as unknown as typeof fetch,
    );

    const { result, rerender } = renderHook(
      ({ key }: { key: number }) => useDispatchWatch("wi-1", "alice", key),
      { initialProps: { key: 1 } },
    );
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(result.current?.state).toBe("succeeded"); // first dispatch ran to terminal

    // A second plain-comment nudge on the SAME ticket: new nonce → the ladder re-seeds at
    // queued instead of staying parked on the previous dispatch's terminal state.
    rerender({ key: 2 });
    expect(result.current?.state).toBe("queued");
    expect(result.current?.label).toBe("Queued");
  });

  it("closes the EventSource on terminal and on unmount (no zombie stream)", async () => {
    const instances = stubEventSource();
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse([{ id: "run-x", workItemRef: "wi-1", phase: "Running" }]),
      ) as unknown as typeof fetch,
    );

    const { result, unmount } = renderHook(() =>
      useDispatchWatch("wi-1", "alice"),
    );
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    const es = instances.find((i) => i.url.includes("run-x"));
    expect(es).toBeTruthy();

    act(() => {
      es!.emit("ended", { event: "ended", run_id: "run-x" });
    });
    expect(result.current?.state).toBe("succeeded");

    unmount();
    expect(es!.closed).toBe(true);
  });
});
