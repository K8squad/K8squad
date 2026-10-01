// test/tickets/useActiveRunDiscovery.test.tsx — ISI-5287 (child of ISI-5285).
//
// The "run started AFTER the tab was already open" gap: an idle ticket tab never
// learns a run started afterwards, so the ambient SSE stream (useTicketRunStream)
// never engages and the tab stays stuck on "working". This proves:
//   • the pure picker (pickActiveRun): newest LIVE run for the item, ignoring
//     terminal / not-yet-held (Pending) / other-item / id-less rows;
//   • the hook is INERT while a stream is already open (streaming=true) or with no
//     ticket — no poll, no reload;
//   • while idle it polls GET /api/runs and fires onRunAppeared once a live run on
//     this ticket appears — exactly once per run (no reload loop);
//   • a run the tab already tracks (holdingRunId) is never re-discovered;
//   • the poll timer is torn down on unmount (no leak).

import { describe, it, expect, afterEach, vi } from "vitest";
import { renderHook, act, cleanup } from "@testing-library/react";
import {
  pickActiveRun,
  useActiveRunDiscovery,
  ACTIVE_RUN_POLL_INTERVAL_MS,
} from "@/lib/tickets/useActiveRunDiscovery";

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

// ── Layer 1: pure picker ─────────────────────────────────────────────────────

describe("pickActiveRun", () => {
  it("returns the first live (Claiming/Running) run for the work item", () => {
    const row = pickActiveRun(
      [
        { id: "new", workItemRef: "wi-1", phase: "Running" },
        { id: "old", workItemRef: "wi-1", phase: "Running" },
      ],
      "wi-1",
    );
    expect(row?.id).toBe("new"); // listing is newest-first; first live match wins
  });

  it("accepts Claiming (holder set) as live", () => {
    expect(
      pickActiveRun([{ id: "r", workItemRef: "wi-1", phase: "Claiming" }], "wi-1")
        ?.id,
    ).toBe("r");
  });

  it("ignores terminal, Pending (not yet held), other-item, and id-less rows", () => {
    for (const phase of ["Succeeded", "Failed", "Cancelled", "Pending", "Paused"]) {
      expect(
        pickActiveRun([{ id: "r", workItemRef: "wi-1", phase }], "wi-1"),
      ).toBeNull();
    }
    expect(
      pickActiveRun([{ id: "r", workItemRef: "wi-2", phase: "Running" }], "wi-1"),
    ).toBeNull();
    expect(
      pickActiveRun([{ workItemRef: "wi-1", phase: "Running" }], "wi-1"),
    ).toBeNull();
    expect(pickActiveRun([], "wi-1")).toBeNull();
  });
});

// ── Layer 2: hook wiring ─────────────────────────────────────────────────────

describe("useActiveRunDiscovery — hook wiring", () => {
  it("is inert (no fetch) while a stream is already open", () => {
    const fetchMock = vi.fn(async () => jsonResponse([]));
    vi.stubGlobal("fetch", fetchMock as unknown as typeof fetch);
    renderHook(() =>
      useActiveRunDiscovery("wi-1", "", true /* streaming */, () => {}),
    );
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("is inert (no fetch) with no work item", () => {
    const fetchMock = vi.fn(async () => jsonResponse([]));
    vi.stubGlobal("fetch", fetchMock as unknown as typeof fetch);
    renderHook(() => useActiveRunDiscovery("", "", false, () => {}));
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("fires onRunAppeared once when a live run appears, then not again for the same run", async () => {
    vi.useFakeTimers();
    let rows: unknown[] = []; // idle: no live run yet
    const fetchMock = vi.fn(async () => jsonResponse(rows));
    vi.stubGlobal("fetch", fetchMock as unknown as typeof fetch);
    const onRunAppeared = vi.fn();

    renderHook(() =>
      useActiveRunDiscovery("wi-1", "", false, onRunAppeared),
    );

    // First poll (immediate) sees nothing → no reload.
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(onRunAppeared).not.toHaveBeenCalled();

    // A live run mints; the next poll discovers it → exactly one reload.
    rows = [{ id: "run-9", workItemRef: "wi-1", phase: "Running" }];
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ACTIVE_RUN_POLL_INTERVAL_MS + 5);
    });
    expect(onRunAppeared).toHaveBeenCalledTimes(1);

    // The same live run on subsequent polls must NOT loop the reload (actedFor guard).
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ACTIVE_RUN_POLL_INTERVAL_MS + 5);
      await vi.advanceTimersByTimeAsync(ACTIVE_RUN_POLL_INTERVAL_MS + 5);
    });
    expect(onRunAppeared).toHaveBeenCalledTimes(1);
  });

  it("never re-discovers the run the tab already tracks (holdingRunId)", async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn(async () =>
      jsonResponse([{ id: "run-held", workItemRef: "wi-1", phase: "Running" }]),
    );
    vi.stubGlobal("fetch", fetchMock as unknown as typeof fetch);
    const onRunAppeared = vi.fn();

    renderHook(() =>
      // holdingRunId === the only live run ⇒ nothing new to subscribe to.
      useActiveRunDiscovery("wi-1", "run-held", false, onRunAppeared),
    );

    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
      await vi.advanceTimersByTimeAsync(ACTIVE_RUN_POLL_INTERVAL_MS + 5);
    });
    expect(onRunAppeared).not.toHaveBeenCalled();
  });

  it("tears the poll timer down on unmount (no leak)", async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn(async () => jsonResponse([]));
    vi.stubGlobal("fetch", fetchMock as unknown as typeof fetch);

    const { unmount } = renderHook(() =>
      useActiveRunDiscovery("wi-1", "", false, () => {}),
    );
    await act(async () => {
      await Promise.resolve();
    });
    const callsBefore = fetchMock.mock.calls.length;
    unmount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ACTIVE_RUN_POLL_INTERVAL_MS * 3);
    });
    // No further polls after unmount.
    expect(fetchMock.mock.calls.length).toBe(callsBefore);
  });
});
