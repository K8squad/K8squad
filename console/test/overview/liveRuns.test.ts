// test/overview/liveRuns.test.ts — ISI-5526: the per-project live-run projection that feeds the
// discussion-room nav badge. Pure, no DOM — mirrors the task-list live idiom (running + queued).

import { describe, it, expect } from "vitest";
import { isLiveRunPhase, liveRunCountByProject } from "@/lib/overview/liveRuns";
import type { SquadOverviewData } from "@/components/SquadOverview";

describe("isLiveRunPhase — ISI-5526", () => {
  it("counts actively-working phases (phaseTone 'running')", () => {
    for (const p of ["Running", "Claiming", "Dispatching", "Collecting"]) {
      expect(isLiveRunPhase(p)).toBe(true);
    }
  });

  it("counts waiting-to-start phases as live (queued semantics)", () => {
    expect(isLiveRunPhase("Queued")).toBe(true);
    expect(isLiveRunPhase("WaitingForEndpointSlot")).toBe(true);
  });

  it("does NOT count idle, paused, or terminal phases", () => {
    for (const p of ["Succeeded", "Failed", "Canceled", "Paused", "", "Idle"]) {
      expect(isLiveRunPhase(p)).toBe(false);
    }
  });
});

function overview(
  projects: SquadOverviewData["projects"],
): SquadOverviewData {
  return { team: { name: "t", namespace: "t-squad", uid: "u" }, projects };
}

describe("liveRunCountByProject — ISI-5526", () => {
  it("keys per-project live counts by the project CR name, omitting idle projects", () => {
    const data = overview([
      {
        name: "alpha",
        namespace: "alpha-squad",
        phaseCounts: {},
        runs: [
          { name: "r1", phase: "Running" },
          { name: "r2", phase: "Queued" },
          { name: "r3", phase: "Succeeded" }, // not live
        ],
      },
      {
        name: "beta",
        namespace: "beta-squad",
        phaseCounts: {},
        runs: [{ name: "r4", phase: "Failed" }], // all terminal → omitted
      },
    ]);
    // Keyed as namespace/name to match the /api/projects id the nav tree joins on.
    expect(liveRunCountByProject(data)).toEqual({ "alpha-squad/alpha": 2 });
  });

  it("is null-safe: null overview, null projects, and null runs all yield {}", () => {
    expect(liveRunCountByProject(null)).toEqual({});
    expect(liveRunCountByProject(overview(null))).toEqual({});
    expect(
      liveRunCountByProject(
        overview([{ name: "alpha", namespace: "a", phaseCounts: {}, runs: null }]),
      ),
    ).toEqual({});
  });
});
