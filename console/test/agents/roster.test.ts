import { describe, it, expect } from "vitest";

import {
  buildRoster,
  dedupeByName,
  filterRoster,
  isSelected,
  teamHierarchy,
  teamSkillSummary,
  teamUidForNamespace,
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

// ISI-5439(1): the fleet role/skill lists can carry the same library object more than once; the
// rail renders one row per logical object (keyed by name, its UI identity). buildRoster dedupes.
describe("dedupeByName / buildRoster role dedupe", () => {
  it("keeps the first occurrence of each name, preserving order", () => {
    const dups = [
      { name: "Backend Engineer", uid: "r1", namespace: "ns-plat", defaultSkills: [] },
      { name: "Backend Engineer", uid: "r1b", namespace: "ns-growth", defaultSkills: [] },
      { name: "Product Manager", uid: "r2", namespace: "ns-plat", defaultSkills: [] },
    ];
    expect(dedupeByName(dups).map((d) => d.uid)).toEqual(["r1", "r2"]);
  });

  it("collapses duplicate roles/skills so each name appears exactly once in the roster", () => {
    const dupRoles: RoleWire[] = [
      { name: "Backend Engineer", namespace: "ns-plat", uid: "r1", defaultSkills: ["git"] },
      { name: "Backend Engineer", namespace: "ns-plat", uid: "r1-again", defaultSkills: ["git"] },
      { name: "Product Manager", namespace: "ns-plat", uid: "r2", defaultSkills: [] },
    ];
    const dupSkills: SkillWire[] = [
      { name: "git", namespace: "ns-plat", uid: "s1" },
      { name: "git", namespace: "ns-plat", uid: "s1-again" },
      { name: "kubectl", namespace: "ns-plat", uid: "s2" },
    ];
    const r = buildRoster(teams, agents, dupRoles, dupSkills);
    expect(r.roles.map((x) => x.name)).toEqual(["Backend Engineer", "Product Manager"]);
    expect(r.skills.map((x) => x.name)).toEqual(["git", "kubectl"]);
  });
});

// ISI-5439(3): the Team detail draws a reporting diagram (coordinator → members) and a skills
// rollup (union of the team's roles' default skills, attributed to the role that loads each).
describe("teamHierarchy", () => {
  const r = buildRoster(teams, agents, roles, skills);
  const plat = r.teams.find((t) => t.uid === "t-plat")!;

  it("splits a team's agents into coordinator root(s) and members", () => {
    const h = teamHierarchy(plat);
    expect(h.coordinators.map((a) => a.id)).toEqual(["a2"]); // agent-9 is coordinator:true
    expect(h.members.map((a) => a.id)).toEqual(["a1"]);
  });

  it("yields empty coordinators for a team with none flagged", () => {
    const flat = buildRoster(
      teams,
      [{ id: "x1", name: "solo", namespace: "ns-plat", skillCount: 0 }],
      [],
      [],
    ).teams.find((t) => t.uid === "t-plat")!;
    const h = teamHierarchy(flat);
    expect(h.coordinators).toEqual([]);
    expect(h.members.map((a) => a.id)).toEqual(["x1"]);
  });
});

describe("teamSkillSummary", () => {
  it("rolls up team roles' default skills, deduped and attributed to the role", () => {
    const rolesWithSkills: RoleWire[] = [
      { name: "Backend Engineer", namespace: "ns-plat", uid: "r1", defaultSkills: ["git", "kubectl"] },
      { name: "SRE", namespace: "ns-plat", uid: "r3", defaultSkills: ["kubectl"] },
      // A role in a DIFFERENT namespace must not leak into this team's rollup.
      { name: "Growth PM", namespace: "ns-growth", uid: "r4", defaultSkills: ["ga"] },
    ];
    const r = buildRoster(teams, agents, rolesWithSkills, skills);
    const plat = r.teams.find((t) => t.uid === "t-plat")!;
    const rollup = teamSkillSummary(r, plat);
    expect(rollup).toEqual([
      { skill: "git", roles: ["Backend Engineer"] },
      { skill: "kubectl", roles: ["Backend Engineer", "SRE"] },
    ]);
  });

  it("is empty when the team's roles declare no default skills", () => {
    const r = buildRoster(teams, agents, roles, skills);
    const growth = r.teams.find((t) => t.uid === "t-growth")!;
    expect(teamSkillSummary(r, growth)).toEqual([]);
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

// ISI-5416: a fleet admin's authoring DETAIL reads must carry ?team=<owning Team UID> — the
// server resolves the selector by UID (detailNamespace), and without it every inline-editor
// read 404s. The node-derived scope comes from the node's namespace via the roster's ORG axis.
describe("teamUidForNamespace", () => {
  const r = buildRoster(teams, agents, roles, skills);

  it("resolves an agent/role namespace to its owning Team's UID", () => {
    expect(teamUidForNamespace(r, "ns-plat")).toBe("t-plat");
    expect(teamUidForNamespace(r, "ns-growth")).toBe("t-growth");
  });

  it("returns undefined for a namespace no listed Team occupies (caller falls back)", () => {
    expect(teamUidForNamespace(r, "ns-gone")).toBeUndefined();
    expect(teamUidForNamespace(r, "")).toBeUndefined();
  });
});
