// test/overview/projectActivity.test.ts — the per-project agent attribution behind the WS-4
// agent-card grid (ISI-5709). Pure: no fetch, no DOM. Verifies the deep-link-prefix filter
// matches the SAME encoded project id the /projects/[projectId] routes use.

import { describe, it, expect } from "vitest";
import { projectAgents } from "@/lib/overview/projectActivity";
import type { AgentActivity } from "@/lib/overview/agentActivity";

const PROJECT = "squad-alpha/webapp"; // encodeProjectId → "squad-alpha%2Fwebapp"

function agent(name: string, run: Partial<AgentActivity["currentOrLastRun"]> | null): AgentActivity {
  return {
    agentId: name,
    name,
    state: "running",
    currentOrLastRun: run
      ? { runId: `${name}-run`, phase: "Running", ...run }
      : null,
  };
}

describe("projectAgents", () => {
  it("keeps agents whose run deep-links into this project (work item or discussion)", () => {
    const agents = [
      agent("alice", { workItemUrl: "/projects/squad-alpha%2Fwebapp/issues/ISI-1" }),
      agent("bob", { discussionUrl: "/projects/squad-alpha%2Fwebapp/discussion" }),
    ];
    expect(projectAgents(agents, PROJECT).map((a) => a.name)).toEqual(["alice", "bob"]);
  });

  it("drops agents linked to a different project", () => {
    const agents = [
      agent("carol", { workItemUrl: "/projects/squad-beta%2Fother/issues/ISI-9" }),
    ];
    expect(projectAgents(agents, PROJECT)).toEqual([]);
  });

  it("drops agents with no run, or a run that links nowhere (no fabricated attribution)", () => {
    const agents = [
      agent("dave", null),
      agent("erin", { phase: "Running" }), // no urls
    ];
    expect(projectAgents(agents, PROJECT)).toEqual([]);
  });

  it("preserves server order and is safe on empty/nullish input", () => {
    expect(projectAgents(null, PROJECT)).toEqual([]);
    expect(projectAgents([], PROJECT)).toEqual([]);
    const agents = [
      agent("z", { workItemUrl: "/projects/squad-alpha%2Fwebapp/issues/ISI-2" }),
      agent("a", { workItemUrl: "/projects/squad-alpha%2Fwebapp/issues/ISI-3" }),
    ];
    expect(projectAgents(agents, PROJECT).map((a) => a.name)).toEqual(["z", "a"]);
  });

  it("does not prefix-match a sibling project whose id shares a prefix", () => {
    // encodeProjectId keeps the slash as %2F and the trailing "/" in the prefix guards this,
    // but assert it explicitly: "webapp" must not match "webapp-staging".
    const agents = [
      agent("mallory", { workItemUrl: "/projects/squad-alpha%2Fwebapp-staging/issues/ISI-7" }),
    ];
    expect(projectAgents(agents, PROJECT)).toEqual([]);
  });
});
