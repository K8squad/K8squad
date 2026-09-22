// test/projects/ProjectLanding.test.tsx — Project-Overview landing panels.
// Covers Runs, Stats (three board buckets + "—" vs 0), Latest Tickets (incl. 501
// not-available), Team Composition (roster + role breakdown, namespace-scoped),
// Workspace (identity + honest PVC "—"), per-panel isolation, and read-only (GET-only).

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor, within } from "@testing-library/react";
import {
  ProjectLanding,
  selectProject,
  recentRuns,
  recentTickets,
  ticketStats,
  selectProjectAgents,
  agentRoleBreakdown,
  lastActivity,
  type SquadAgentEntry,
} from "@/components/projects/ProjectLanding";
import type { SquadOverviewData } from "@/components/SquadOverview";
import type { WorkItem } from "@/lib/tickets/types";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const overview: SquadOverviewData = {
  team: { name: "alpha", namespace: "squad-alpha", uid: "u1" },
  projects: [
    {
      name: "webapp",
      namespace: "squad-alpha",
      repoUrl: "https://git.example/webapp",
      runs: [
        { name: "run-old", workItem: "wi-1", phase: "Succeeded", claimedAt: "2026-08-20T10:00:00Z" },
        { name: "run-new", phase: "Running", claimedAt: "2026-08-22T10:00:00Z" },
      ],
      phaseCounts: { Running: 1, Succeeded: 1 },
    },
    { name: "infra", namespace: "squad-alpha", runs: null, phaseCounts: {} },
  ],
};

const workItems: WorkItem[] = [
  { id: "t1", projectId: "webapp", parentId: null, title: "Fix login", state: "in_progress", blockedReason: null, updatedAt: "2026-08-21T09:00:00Z" },
  { id: "t2", projectId: "webapp", parentId: null, title: "Ship docs", state: "done", blockedReason: null, updatedAt: "2026-08-23T09:00:00Z" },
  { id: "t3", projectId: "webapp", parentId: null, title: "Add metrics", state: "in_review", blockedReason: null, updatedAt: "2026-08-19T09:00:00Z" },
];

const agents: SquadAgentEntry[] = [
  { id: "a1", name: "cade", namespace: "squad-alpha", role: "developer", skillCount: 3 },
  { id: "a2", name: "archie", namespace: "squad-alpha", role: "architect", skillCount: 5 },
  { id: "a3", name: "devy", namespace: "squad-alpha", role: "developer", skillCount: 2 },
  { id: "a4", name: "stray", namespace: "squad-beta", role: "developer", skillCount: 1 }, // other squad — filtered out
];

/** Route-aware fetch stub: overview + work-items + agents each get their own status/body. */
function stubRoutes(cfg: {
  overview: { status: number; body?: unknown };
  workItems: { status: number; body?: unknown };
  agents?: { status: number; body?: unknown };
}) {
  const agentsCfg = cfg.agents ?? { status: 200, body: { agents } };
  const spy = vi.fn((input: RequestInfo | URL, _init?: RequestInit) => {
    const url = String(input);
    const pick = url.includes("/api/squad/overview")
      ? cfg.overview
      : url.includes("/api/squad/agents")
        ? agentsCfg
        : cfg.workItems;
    return Promise.resolve({
      ok: pick.status >= 200 && pick.status < 300,
      status: pick.status,
      json: () => Promise.resolve(pick.body ?? null),
      text: () => Promise.resolve(JSON.stringify(pick.body ?? null)),
    } as Response);
  });
  vi.stubGlobal("fetch", spy);
  return spy;
}

describe("ProjectLanding — pure projections", () => {
  it("selectProject matches by name; returns null when absent", () => {
    expect(selectProject(overview, "webapp")?.name).toBe("webapp");
    expect(selectProject(overview, "nope")).toBeNull();
  });

  it("selectProject also matches the namespace/name composite id (ISI-4795)", () => {
    // Nav via project.id delivers the composite; the slice must still land on the
    // project, or the runs/active-run tiles render empty on fleet data that has them.
    expect(selectProject(overview, "squad-alpha/webapp")?.name).toBe("webapp");
    expect(selectProject(overview, "squad-beta/webapp")).toBeNull(); // wrong namespace: no match
  });

  it("recentRuns sorts claimedAt desc, unclaimed last, capped", () => {
    const runs = recentRuns(selectProject(overview, "webapp"), 8);
    expect(runs.map((r) => r.name)).toEqual(["run-new", "run-old"]);
    expect(recentRuns(selectProject(overview, "infra"), 8)).toEqual([]);
    expect(recentRuns(selectProject(overview, "webapp"), 1)).toHaveLength(1);
  });

  it("recentTickets sorts updatedAt desc; ticketStats splits open/in_review/done", () => {
    expect(recentTickets(workItems, 6).map((t) => t.id)).toEqual(["t2", "t1", "t3"]);
    expect(ticketStats(workItems)).toEqual({ open: 1, inReview: 1, done: 1 });
    expect(ticketStats([])).toEqual({ open: 0, inReview: 0, done: 0 });
  });

  it("selectProjectAgents filters by namespace; passes through when unknown", () => {
    expect(selectProjectAgents(agents, "squad-alpha").map((a) => a.name)).toEqual(["cade", "archie", "devy"]);
    expect(selectProjectAgents(agents, undefined)).toHaveLength(4);
    expect(selectProjectAgents(agents, "squad-gamma")).toEqual([]);
  });

  it("agentRoleBreakdown counts by role desc; role-less ⇒ unassigned", () => {
    expect(agentRoleBreakdown(selectProjectAgents(agents, "squad-alpha"))).toEqual([
      { role: "developer", count: 2 },
      { role: "architect", count: 1 },
    ]);
    expect(agentRoleBreakdown([{ id: "x", name: "n", namespace: "n" }])).toEqual([
      { role: "unassigned", count: 1 },
    ]);
  });

  it("lastActivity is the max across runs + tickets; null when nothing known", () => {
    expect(lastActivity(selectProject(overview, "webapp"), workItems)).toBe("2026-08-23T09:00:00.000Z");
    expect(lastActivity(null, [])).toBeNull();
  });
});

describe("ProjectLanding — populated panels", () => {
  it("renders Runs, Stats, Tickets, Team, Workspace from GET reads", async () => {
    stubRoutes({
      overview: { status: 200, body: overview },
      workItems: { status: 200, body: { items: workItems } },
      agents: { status: 200, body: { agents } },
    });
    render(<ProjectLanding projectId="webapp" />);

    await waitFor(() => expect(screen.getAllByTestId("landing-run-row").length).toBe(2));
    // Runs: newest run first, deep-linked to /runs/{id} with ?wi threading.
    const firstRow = screen.getAllByTestId("landing-run-row")[0];
    expect(within(firstRow).getByRole("link")).toHaveAttribute("href", "/runs/run-new");

    // Stats: phase chips + authoritative three-bucket counts.
    expect(screen.getAllByTestId("landing-stats-phase-count").length).toBe(2);
    expect(screen.getByTestId("landing-stats-open").textContent).toBe("1");
    expect(screen.getByTestId("landing-stats-inreview").textContent).toBe("1");
    expect(screen.getByTestId("landing-stats-done").textContent).toBe("1");

    // Latest tickets newest-first, view-all → Issues tab.
    expect(screen.getAllByTestId("landing-ticket-row").length).toBe(3);
    expect(screen.getByTestId("landing-tickets-viewall")).toHaveAttribute("href", "/projects/webapp/issues");

    // Team Composition: roster scoped to squad-alpha (stray squad-beta agent filtered out) + role breakdown.
    await waitFor(() => expect(screen.getByTestId("landing-team-count").textContent).toContain("3"));
    const roles = screen.getAllByTestId("landing-team-role").map((el) => el.textContent);
    expect(roles).toEqual(["developer · 2", "architect · 1"]);

    // Workspace: project identity from the overview; PVC honestly not reported (ISI-4127).
    expect(within(screen.getByTestId("landing-workspace-repo")).getByRole("link")).toHaveAttribute(
      "href",
      "https://git.example/webapp",
    );
    expect(screen.getByTestId("landing-workspace-namespace").textContent).toContain("squad-alpha");
    expect(screen.getByTestId("landing-workspace-pvc").textContent).toContain("not reported");
  });
});

describe("ProjectLanding — empty states", () => {
  it("shows neutral empty states, never fabricated rows", async () => {
    stubRoutes({
      overview: { status: 200, body: { team: overview.team, projects: [{ name: "webapp", namespace: "squad-alpha", runs: null, phaseCounts: {} }] } },
      workItems: { status: 200, body: { items: [] } },
      agents: { status: 200, body: { agents: [] } },
    });
    render(<ProjectLanding projectId="webapp" />);
    await waitFor(() => expect(screen.getByTestId("landing-runs-empty")).toBeTruthy());
    expect(screen.getByTestId("landing-tickets-empty")).toBeTruthy();
    // Zero runs known → "No runs." (authoritative); counts authoritative 0.
    expect(screen.getByTestId("landing-stats-phase-empty")).toBeTruthy();
    expect(screen.getByTestId("landing-stats-open").textContent).toBe("0");
    expect(screen.getByTestId("landing-stats-inreview").textContent).toBe("0");
    expect(screen.getByTestId("landing-stats-done").textContent).toBe("0");
    await waitFor(() => expect(screen.getByTestId("landing-team-empty")).toBeTruthy());
  });
});

describe("ProjectLanding — per-panel isolation + '—' vs 0", () => {
  it("overview 200 + work-items 501: Runs/Stats(phase)/Team/Workspace populate, tickets degrade, counts show —", async () => {
    stubRoutes({
      overview: { status: 200, body: overview },
      workItems: { status: 501, body: "not wired" },
      agents: { status: 200, body: { agents } },
    });
    render(<ProjectLanding projectId="webapp" />);

    // Runs + phase stats stay populated despite the failing work-items read.
    await waitFor(() => expect(screen.getAllByTestId("landing-run-row").length).toBe(2));
    expect(screen.getAllByTestId("landing-stats-phase-count").length).toBe(2);

    // Latest Tickets renders its OWN honest not-available state.
    expect(screen.getByTestId("landing-tickets-unavailable")).toBeTruthy();

    // Fabrication discipline: unavailable source ⇒ "—", not 0.
    expect(screen.getByTestId("landing-stats-open").textContent).toBe("—");
    expect(screen.getByTestId("landing-stats-inreview").textContent).toBe("—");
    expect(screen.getByTestId("landing-stats-done").textContent).toBe("—");

    // Team + Workspace unaffected by the work-items failure.
    await waitFor(() => expect(screen.getByTestId("landing-team-count").textContent).toContain("3"));
    expect(screen.getByTestId("landing-workspace-namespace").textContent).toContain("squad-alpha");
  });

  it("overview 501 + work-items 200: tickets populate, overview panels show honest problem/—", async () => {
    stubRoutes({
      overview: { status: 501, body: null },
      workItems: { status: 200, body: { items: workItems } },
      agents: { status: 501, body: null },
    });
    render(<ProjectLanding projectId="webapp" />);
    await waitFor(() => expect(screen.getAllByTestId("landing-ticket-row").length).toBe(3));
    expect(screen.getByTestId("landing-runs-problem")).toBeTruthy();
    expect(screen.getByTestId("landing-stats-phase-unavailable")).toBeTruthy();
    expect(screen.getByTestId("landing-workspace-problem")).toBeTruthy();
    // counts still authoritative from the good work-items read.
    expect(screen.getByTestId("landing-stats-open").textContent).toBe("1");
    // Team read model unavailable ⇒ its own honest state.
    await waitFor(() => expect(screen.getByTestId("landing-team-unavailable")).toBeTruthy());
  });
});

describe("ProjectLanding — read-only", () => {
  it("issues only GET reads to overview + work-items + agents; no mutation", async () => {
    const spy = stubRoutes({
      overview: { status: 200, body: overview },
      workItems: { status: 200, body: { items: workItems } },
      agents: { status: 200, body: { agents } },
    });
    render(<ProjectLanding projectId="webapp" />);
    await waitFor(() => expect(screen.getByTestId("project-landing")).toBeTruthy());
    await waitFor(() => expect(screen.getAllByTestId("landing-ticket-row").length).toBe(3));
    for (const call of spy.mock.calls) {
      const init = call[1] as RequestInit | undefined;
      const method = (init?.method ?? "GET").toUpperCase();
      expect(method).toBe("GET");
    }
    const urls = spy.mock.calls.map((c) => String(c[0]));
    expect(urls.some((u) => u.includes("/api/squad/overview"))).toBe(true);
    expect(urls.some((u) => u.includes("/api/projects/webapp/work-items"))).toBe(true);
    expect(urls.some((u) => u.includes("/api/squad/agents"))).toBe(true);
  });
});
