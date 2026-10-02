import { describe, it, expect } from "vitest";

import {
  buildRoster,
  filterRoster,
  isSelected,
  type AgentWire,
  type RoleWire,
  type SkillWire,
  type TeamWire,
} from "@/lib/agents/roster";

// ISI-5362 / S5 — the two-axis roster model is assembled + filtered by PURE functions so the tree
// shape and the filter facet are testable without a network. These cover the backend-honest
// grouping (agents under a team by NAMESPACE), the role-chip filter facet, and the selection test.

const teams: TeamWire[] = [
  { name: "Platform Team", namespace: "ns-plat", uid: "t-plat", agentCount: 2, projectCount: 1 },
  { name: "Growth Team", namespace: "ns-growth", uid: "t-growth", agentCount: 0, projectCount: 0 },
];
const agents: AgentWire[] = [
  { id: "a1", name: "agent-7", namespace: "ns-plat", role: "Backend", skillCount: 4, coordinator: false },
  { id: "a2", name: "agent-9", namespace: "ns-plat", role: "PM", skillCount: 2, coordinator: true },
  { id: "a3", name: "orphan", namespace: "ns-gone", role: "Backend", skillCount: 0 },
];
const roles: RoleWire[] = [
  { name: "Backend Engineer", namespace: "ns-plat", uid: "r1", defaultSkills: ["git"] },
  { name: "Product Manager", namespace: "ns-plat", uid: "r2", defaultSkills: [] },
];
const skills: SkillWire[] = [
  { name: "git", namespace: "ns-plat", uid: "s1", permissions: ["repo"] },
  { name: "kubectl", namespace: "ns-plat", uid: "s2" },
];

describe("buildRoster", () => {
  it("groups agents under their team by namespace", () => {
    const r = buildRoster(teams, agents, roles, skills);
    const plat = r.teams.find((t) => t.uid === "t-plat")!;
    expect(plat.agents.map((a) => a.id)).toEqual(["a1", "a2"]);
    expect(r.teams.find((t) => t.uid === "t-growth")!.agents).toEqual([]);
  });

  it("drops an agent whose namespace matches no team (projection skew, not fabricated)", () => {
    const r = buildRoster(teams, agents, roles, skills);
    const allAgentIds = r.teams.flatMap((t) => t.agents.map((a) => a.id));
    expect(allAgentIds).not.toContain("a3");
  });

  it("carries roles, skills, and the synthetic org-default root", () => {
    const r = buildRoster(teams, agents, roles, skills);
    expect(r.roles.map((x) => x.name)).toEqual(["Backend Engineer", "Product Manager"]);
    expect(r.skills.map((x) => x.name)).toEqual(["git", "kubectl"]);
    expect(r.orgDefault).toEqual({ kind: "orgDefault", name: "Default model" });
  });
});

describe("filterRoster", () => {
  const r = buildRoster(teams, agents, roles, skills);

  it("returns the same reference for an empty query", () => {
    expect(filterRoster(r, "   ")).toBe(r);
  });

  it("keeps a team whose own name matches, with all its agents", () => {
    const f = filterRoster(r, "platform");
    expect(f.teams.map((t) => t.uid)).toEqual(["t-plat"]);
    expect(f.teams[0].agents).toHaveLength(2);
  });

  it("matches agents by their role chip and narrows the team to the hits", () => {
    const f = filterRoster(r, "backend");
    // Team name doesn't match 'backend' but agent-7's role does → team kept with only that agent.
    const plat = f.teams.find((t) => t.uid === "t-plat")!;
    expect(plat.agents.map((a) => a.id)).toEqual(["a1"]);
    // The role library object "Backend Engineer" also matches.
    expect(f.roles.map((x) => x.name)).toContain("Backend Engineer");
  });

  it("filters library skills independently", () => {
    const f = filterRoster(r, "kubectl");
    expect(f.skills.map((s) => s.name)).toEqual(["kubectl"]);
    expect(f.roles).toHaveLength(0);
  });
});

describe("isSelected", () => {
  const r = buildRoster(teams, agents, roles, skills);
  const agent = r.teams[0].agents[0];

  it("matches an agent by id and rejects a different kind at the same key", () => {
    expect(isSelected({ kind: "agent", id: "a1" }, agent)).toBe(true);
    expect(isSelected({ kind: "team", uid: "a1" }, agent)).toBe(false);
    expect(isSelected(null, agent)).toBe(false);
  });

  it("matches the org-default singleton on kind alone", () => {
    expect(isSelected({ kind: "orgDefault" }, r.orgDefault)).toBe(true);
  });
});
