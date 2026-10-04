import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { render, screen, cleanup, within } from "@testing-library/react";

import { NodeDetail } from "@/components/agents/roster/NodeDetail";
import {
  buildRoster,
  type AgentWire,
  type RoleWire,
  type SkillWire,
  type TeamWire,
} from "@/lib/agents/roster";

// ISI-5439(2,3c): the right-pane tab strip is CONTEXT-AWARE. A Skill and a Team must not show the
// Model tab (nor any other tab with no meaning for them); only Agents and Roles — which carry an
// effective-model override and an editable skill grant — get the full Overview·Model·Skills·Advanced
// strip. The Team Overview also renders the reporting diagram + skills rollup (3a/3b).

// The Agent/Role Overview mounts EffectiveModelReadout, which fetches; stub it so the render is
// synchronous and asserting on the (synchronously-rendered) tab buttons is deterministic.
beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify({}), { status: 200 })),
  );
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const teams: TeamWire[] = [
  { name: "Platform Team", namespace: "ns-plat", uid: "t-plat", agentCount: 2, projectCount: 1 },
];
const agents: AgentWire[] = [
  { id: "a1", name: "agent-7", namespace: "ns-plat", role: "Backend", skillCount: 4, coordinator: false },
  { id: "a2", name: "agent-9", namespace: "ns-plat", role: "PM", skillCount: 2, coordinator: true },
];
const roles: RoleWire[] = [
  { name: "Backend Engineer", namespace: "ns-plat", uid: "r1", defaultSkills: ["git", "kubectl"] },
];
const skills: SkillWire[] = [{ name: "git", namespace: "ns-plat", uid: "s1", permissions: ["repo"] }];

const roster = buildRoster(teams, agents, roles, skills);

function tabNames(): string[] {
  return screen.queryAllByRole("tab").map((t) => t.textContent ?? "");
}

describe("NodeDetail tab relevance", () => {
  it("shows the full Overview·Model·Skills·Advanced strip for an Agent", () => {
    render(<NodeDetail roster={roster} selection={{ kind: "agent", id: "a1" }} onRosterChanged={() => {}} />);
    expect(tabNames()).toEqual(["Overview", "Model", "Skills", "Advanced"]);
  });

  it("shows the full strip for a Role", () => {
    render(
      <NodeDetail
        roster={roster}
        selection={{ kind: "role", name: "Backend Engineer" }}
        onRosterChanged={() => {}}
      />,
    );
    expect(tabNames()).toEqual(["Overview", "Model", "Skills", "Advanced"]);
  });

  it("shows NO Model tab for a Skill — only Overview (strip hidden for a single tab)", () => {
    render(<NodeDetail roster={roster} selection={{ kind: "skill", name: "git" }} onRosterChanged={() => {}} />);
    expect(tabNames()).not.toContain("Model");
    // A single-tab node hides the strip entirely.
    expect(screen.queryByRole("tab")).toBeNull();
  });

  it("shows NO Model tab for a Team", () => {
    render(<NodeDetail roster={roster} selection={{ kind: "team", uid: "t-plat" }} onRosterChanged={() => {}} />);
    expect(tabNames()).not.toContain("Model");
  });
});

describe("NodeDetail team overview enrichments (ISI-5439(3))", () => {
  beforeEach(() => {
    render(<NodeDetail roster={roster} selection={{ kind: "team", uid: "t-plat" }} onRosterChanged={() => {}} />);
  });

  it("renders a reporting-structure diagram with coordinator + members", () => {
    const diagram = screen.getByRole("group", { name: /reporting structure/i });
    expect(within(diagram).getByText("agent-9")).toBeTruthy(); // coordinator
    expect(within(diagram).getByText("agent-7")).toBeTruthy(); // member
    expect(within(diagram).getByText("coordinator")).toBeTruthy();
  });

  it("renders the team skills rollup with per-role attribution", () => {
    // Backend Engineer loads git + kubectl as defaults.
    expect(screen.getByText("git")).toBeTruthy();
    expect(screen.getByText("kubectl")).toBeTruthy();
    expect(screen.getAllByText(/via Backend Engineer/i).length).toBeGreaterThan(0);
  });
});
