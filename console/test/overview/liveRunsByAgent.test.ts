// test/overview/liveRunsByAgent.test.ts — ISI-5527 (parent ISI-5520): the per-agent live-run
// fold that drives the discussion-room roster flip. A pure projection over the squad/overview
// wire shape (SquadOverviewData) — no fetch, no DOM. Covers: running vs queued classification,
// agent attribution via run.agents, the agent-less (Team-fanned) run NOT mis-attributed,
// running-beats-queued when an agent holds >1 live run, project scoping, and the nullable wire.

import { describe, it, expect } from "vitest";
import {
  isLiveRunPhase,
  liveRunsByAgent,
} from "@/lib/overview/liveRuns";
import type { SquadOverviewData } from "@/components/SquadOverview";

function data(
  runs: {
    name: string;
    phase: string;
    workItem?: string;
    agents?: string[] | null;
  }[],
  projectName = "proj-a",
): SquadOverviewData {
  return {
    team: { name: "t", namespace: "t-ns", uid: "u" },
    projects: [{ name: projectName, namespace: "t-ns", runs, phaseCounts: {} }],
  };
}

describe("isLiveRunPhase", () => {
  it("counts running-tone and queued/waiting phases as live, not terminal/paused", () => {
    expect(isLiveRunPhase("Running")).toBe(true);
    expect(isLiveRunPhase("Claiming")).toBe(true);
    expect(isLiveRunPhase("queued")).toBe(true);
    expect(isLiveRunPhase("WaitingForEndpointSlot")).toBe(true);
    expect(isLiveRunPhase("Succeeded")).toBe(false);
    expect(isLiveRunPhase("Failed")).toBe(false);
    expect(isLiveRunPhase("Paused")).toBe(false);
  });
});

describe("liveRunsByAgent", () => {
  it("attributes a running run to its agent with the ticket ref", () => {
    const out = liveRunsByAgent(
      data([{ name: "run-1", phase: "Running", workItem: "ISI-42", agents: ["amelia"] }]),
      "proj-a",
    );
    expect(out.amelia).toEqual({
      state: "running",
      phase: "Running",
      workItem: "ISI-42",
    });
  });

  it("classifies a queued/waiting run as queued (still live, OQ4)", () => {
    const out = liveRunsByAgent(
      data([{ name: "run-2", phase: "Pending", workItem: "ISI-7", agents: ["winston"] }]),
      "proj-a",
    );
    expect(out.winston.state).toBe("queued");
  });

  it("never mis-attributes an agent-less (Team-fanned) run to one agent", () => {
    const out = liveRunsByAgent(
      data([{ name: "run-3", phase: "Running", agents: null }]),
      "proj-a",
    );
    expect(Object.keys(out)).toHaveLength(0);
  });

  it("running beats queued when an agent holds more than one live run", () => {
    const out = liveRunsByAgent(
      data([
        { name: "run-q", phase: "Pending", workItem: "ISI-1", agents: ["amelia"] },
        { name: "run-r", phase: "Running", workItem: "ISI-2", agents: ["amelia"] },
      ]),
      "proj-a",
    );
    expect(out.amelia.state).toBe("running");
    expect(out.amelia.workItem).toBe("ISI-2");
  });

  it("omits terminal/paused runs entirely", () => {
    const out = liveRunsByAgent(
      data([
        { name: "done", phase: "Succeeded", agents: ["amelia"] },
        { name: "parked", phase: "Paused", agents: ["winston"] },
      ]),
      "proj-a",
    );
    expect(out).toEqual({});
  });

  it("scopes to the named project — other projects' runs do not leak in", () => {
    const d = data([{ name: "run-1", phase: "Running", agents: ["amelia"] }], "proj-b");
    expect(liveRunsByAgent(d, "proj-a")).toEqual({});
    expect(liveRunsByAgent(d, "proj-b").amelia.state).toBe("running");
  });

  it("matches the project by its canonical namespace/name id (the room's route key)", () => {
    // The discussion room is routed by `namespace/name` (lib/projectId.ts), so the roster must
    // match on the same key the nav badge joins on — not the bare CR name.
    const d: SquadOverviewData = {
      team: { name: "t", namespace: "t-ns", uid: "u" },
      projects: [
        {
          name: "proj-a",
          namespace: "alpha-squad",
          runs: [{ name: "run-1", phase: "Running", workItem: "ISI-9", agents: ["amelia"] }],
          phaseCounts: {},
        },
      ],
    };
    expect(liveRunsByAgent(d, "alpha-squad/proj-a").amelia.state).toBe("running");
    // The bare name still resolves (fallback), but a wrong namespace must not.
    expect(liveRunsByAgent(d, "proj-a").amelia.state).toBe("running");
    expect(liveRunsByAgent(d, "beta-squad/proj-a")).toEqual({});
  });

  it("counts a Pending run as queued (the honest k8squad queued phase)", () => {
    const out = liveRunsByAgent(
      data([{ name: "run-p", phase: "Pending", workItem: "ISI-3", agents: ["amelia"] }]),
      "proj-a",
    );
    expect(out.amelia.state).toBe("queued");
  });

  it("is null-safe for the nullable wire shape", () => {
    expect(liveRunsByAgent(null, "proj-a")).toEqual({});
    expect(
      liveRunsByAgent(
        { team: { name: "t", namespace: "n", uid: "u" }, projects: null },
        "proj-a",
      ),
    ).toEqual({});
  });
});
