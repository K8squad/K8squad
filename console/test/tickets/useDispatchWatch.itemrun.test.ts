// test/tickets/useDispatchWatch.itemrun.test.ts — ISI-7863 (parent ISI-7862).
//
// The originating tab's live run-streaming: the honest ladder must FOLLOW the
// work item's current active run across a fail → re-dispatch cycle instead of
// freezing on the run it dispatched. Proves the pure seams of the fix:
//   - pickItemRun        selects the item's newest live run + the watched run's
//                        phase out of the GET /api/runs listing
//   - follow_run         re-seeds the ladder onto a new run — including THROUGH
//                        a terminal freeze (the exact stuck "is working…" repro)
//   - phase reconcile    a Failed listing row settles a working ladder without
//                        any SSE ended milestone, and never regresses the ladder

import { describe, expect, it } from "vitest";
import {
  initialDispatchMachine,
  pickItemRun,
  reduceDispatchWatch,
  type WatchSignal,
} from "@/lib/tickets/useDispatchWatch";

const run = (sigs: WatchSignal[]) =>
  sigs.reduce(reduceDispatchWatch, initialDispatchMachine());

describe("pickItemRun — the item-level follower's view of GET /api/runs", () => {
  it("picks the item's newest live run (listing is newest-first)", () => {
    const rows = [
      { id: "r3", phase: "Running", workItemRef: "wi1" },
      { id: "r2", phase: "Failed", workItemRef: "wi1" },
      { id: "r1", phase: "Failed", workItemRef: "wi1" },
    ];
    const { liveRun, watchedPhase } = pickItemRun(rows, "wi1", "r1");
    expect(liveRun?.id).toBe("r3");
    expect(watchedPhase).toBe("Failed");
  });

  it("reports no live run when every run for the item is terminal", () => {
    const rows = [
      { id: "r2", phase: "Failed", workItemRef: "wi1" },
      { id: "r1", phase: "Cancelled", workItemRef: "wi1" },
    ];
    expect(pickItemRun(rows, "wi1", "r2").liveRun).toBeNull();
  });

  it("treats a Pending row as not live (no holder yet) but still reports its phase", () => {
    const rows = [{ id: "r1", phase: "Pending", workItemRef: "wi1" }];
    const { liveRun, watchedPhase } = pickItemRun(rows, "wi1", "r1");
    expect(liveRun).toBeNull();
    expect(watchedPhase).toBe("Pending");
  });

  it("ignores rows for other work items", () => {
    const rows = [{ id: "r9", phase: "Running", workItemRef: "wi2" }];
    expect(pickItemRun(rows, "wi1", "r1")).toEqual({
      liveRun: null,
      watchedPhase: null,
    });
  });

  it("handles an absent watched run id", () => {
    const rows = [{ id: "r1", phase: "Running", workItemRef: "wi1" }];
    const { liveRun, watchedPhase } = pickItemRun(rows, "wi1", "");
    expect(liveRun?.id).toBe("r1");
    expect(watchedPhase).toBeNull();
  });
});

describe("reduceDispatchWatch follow_run — re-subscribe on re-dispatch (ISI-7863)", () => {
  it("re-seeds a working ladder onto the item's new run", () => {
    const m = run([
      { type: "run_discovered", runId: "r1" },
      { type: "phase", phase: "Running" },
    ]);
    expect(m.state).toBe("working");
    const m2 = reduceDispatchWatch(m, { type: "follow_run", runId: "r2" });
    expect(m2).toEqual({ state: "picking_up", degrade: "none", runId: "r2" });
  });

  it("breaks a terminal freeze — the exact stuck-'is working…' repro", () => {
    // run #1 failed without an SSE ended frame: the ladder must still follow run #2
    const m = run([
      { type: "run_discovered", runId: "87824d65" },
      { type: "phase", phase: "Running" },
      { type: "phase", phase: "Failed" },
    ]);
    expect(m.state).toBe("failed");
    const m2 = reduceDispatchWatch(m, {
      type: "follow_run",
      runId: "49e6f8fe",
    });
    expect(m2.state).toBe("picking_up");
    expect(m2.runId).toBe("49e6f8fe");
    // …and the new run advances to working through the usual signals
    const m3 = reduceDispatchWatch(m2, { type: "phase", phase: "Running" });
    expect(m3.state).toBe("working");
    expect(m3.runId).toBe("49e6f8fe");
  });

  it("no-ops when the followed run is the one already watched", () => {
    const m = run([
      { type: "run_discovered", runId: "r1" },
      { type: "phase", phase: "Running" },
    ]);
    expect(reduceDispatchWatch(m, { type: "follow_run", runId: "r1" })).toBe(m);
  });
});

describe("phase REST-reconcile — clear the pill without an SSE ended frame", () => {
  it("a Failed listing row settles a working ladder (bounded reconcile)", () => {
    const m = run([
      { type: "run_discovered", runId: "r1" },
      { type: "phase", phase: "Running" },
    ]);
    expect(m.state).toBe("working");
    expect(
      reduceDispatchWatch(m, { type: "phase", phase: "Failed" }).state,
    ).toBe("failed");
  });

  it("a Succeeded listing row settles a working ladder", () => {
    const m = run([
      { type: "run_discovered", runId: "r1" },
      { type: "phase", phase: "Running" },
    ]);
    expect(
      reduceDispatchWatch(m, { type: "phase", phase: "Succeeded" }).state,
    ).toBe("succeeded");
  });

  it("phase signals never regress the ladder (Pending after working)", () => {
    const m = run([
      { type: "run_discovered", runId: "r1" },
      { type: "phase", phase: "Running" },
    ]);
    expect(reduceDispatchWatch(m, { type: "phase", phase: "Pending" })).toBe(m);
  });
});
