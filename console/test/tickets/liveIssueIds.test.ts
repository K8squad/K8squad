// test/tickets/liveIssueIds.test.ts — ISI-5528: the pure board live-marker derivation.
// No fetch, no DOM — the gating rule (which phases count as live) and the project-sliced
// id-set projection are asserted directly off the shared overview wire shape.

import { describe, it, expect } from "vitest";
import {
  isLiveRunPhase,
  liveIssueIdsFromOverview,
} from "@/lib/tickets/liveIssueIds";
import type { SquadOverviewData } from "@/components/SquadOverview";

describe("isLiveRunPhase — task-list live semantics (ISI-5520)", () => {
  it("treats actively-working phases (phaseTone running) as live", () => {
    for (const p of ["Running", "Claiming", "Dispatching", "Collecting"]) {
      expect(isLiveRunPhase(p)).toBe(true);
    }
  });

  it("treats not-yet-started phases (queued / waiting) as live", () => {
    expect(isLiveRunPhase("Queued")).toBe(true);
    expect(isLiveRunPhase("WaitingForEndpointSlot")).toBe(true);
  });

  it("does NOT treat terminal or paused phases as live", () => {
    for (const p of ["Succeeded", "Failed", "Cancelled", "Canceled", "Paused"]) {
      expect(isLiveRunPhase(p)).toBe(false);
    }
  });
});

/** Build a minimal overview payload with one project's runs. */
function overview(
  name: string,
  namespace: string,
  runs: { name: string; workItem?: string; phase: string }[],
): SquadOverviewData {
  return {
    team: { name: "t", namespace: "t-squad", uid: "u" },
    projects: [
      {
        name,
        namespace,
        runs: runs.map((r) => ({ ...r, claimedAt: null })),
        phaseCounts: {},
      },
    ],
  };
}

describe("liveIssueIdsFromOverview — project-sliced live-issue set", () => {
  it("collects work-item ids with a live run, skipping terminal runs", () => {
    const data = overview("demo", "demo-ns", [
      { name: "r1", workItem: "wi-a", phase: "Running" },
      { name: "r2", workItem: "wi-b", phase: "Queued" },
      { name: "r3", workItem: "wi-c", phase: "Succeeded" },
    ]);
    const ids = liveIssueIdsFromOverview(data, "demo");
    expect([...ids].sort()).toEqual(["wi-a", "wi-b"]);
    expect(ids.has("wi-c")).toBe(false);
  });

  it("matches the project by bare name OR namespace/name composite", () => {
    const data = overview("demo", "demo-ns", [
      { name: "r1", workItem: "wi-a", phase: "Running" },
    ]);
    expect(liveIssueIdsFromOverview(data, "demo").has("wi-a")).toBe(true);
    expect(liveIssueIdsFromOverview(data, "demo-ns/demo").has("wi-a")).toBe(true);
    expect(liveIssueIdsFromOverview(data, "other").size).toBe(0);
  });

  it("is null-safe and ignores runs with no work-item ref", () => {
    expect(liveIssueIdsFromOverview(null, "demo").size).toBe(0);
    const data = overview("demo", "demo-ns", [
      { name: "r1", phase: "Running" }, // no workItem
    ]);
    expect(liveIssueIdsFromOverview(data, "demo").size).toBe(0);
  });
});
