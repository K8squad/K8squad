// test/overview/fleetOverview.test.tsx — Frame 01, the fleet control-room global overview
// (ISI-4507 S2). Covers the pure projections (mix fold, active-run count, fleet runs, time
// formatting), the ready render (stat band, project cards as whole-card links, mix-bar
// distribution, recent-ticket deep links, live-runs feed), and the honest degrade paths
// (501 not-wired overview, tokens "—"). Route-aware fetch stub, same discipline as
// ProjectLanding.test.tsx.

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/react";
import {
  FleetOverview,
  mixFromPhaseCounts,
  activeRunCount,
  fleetRuns,
  formatWhen,
  partitionProjects,
} from "@/components/overview/FleetOverview";
import type { SquadOverviewData } from "@/components/SquadOverview";

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
        { name: "run-fix", workItem: "wi-1", phase: "Running", claimedAt: "2026-09-16T10:00:00Z" },
        { name: "run-old", workItem: "wi-2", phase: "Succeeded", claimedAt: "2026-09-15T10:00:00Z" },
      ],
      phaseCounts: { Running: 1, Succeeded: 1 },
    },
    {
      name: "infra",
      namespace: "squad-alpha",
      runs: [{ name: "run-infra", phase: "Paused", claimedAt: "2026-09-16T12:00:00Z" }],
      phaseCounts: { Paused: 1 },
    },
  ],
};

const webappItems = [
  {
    id: "t1",
    projectId: "webapp",
    parentId: null,
    title: "Fix login",
    state: "in_progress" as const,
    blockedReason: null,
    updatedAt: "2026-09-16T09:00:00Z",
  },
  {
    id: "t2",
    projectId: "webapp",
    parentId: null,
    title: "Ship docs",
    state: "done" as const,
    blockedReason: null,
    updatedAt: "2026-09-14T09:00:00Z",
  },
];

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

/** Route-aware fetch stub: overview + agents + per-project work-items + per-project series. */
type StubResponse = { status: number; body?: unknown };

function stubRoutes(cfg: {
  overview: StubResponse;
  agents?: StubResponse;
  workItems?: (project: string) => StubResponse;
  series?: (project: string) => StubResponse;
}) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/api/squad/overview") {
        return Promise.resolve(jsonResponse(cfg.overview.status, cfg.overview.body));
      }
      if (url === "/api/squad/agents") {
        const a: StubResponse = cfg.agents ?? { status: 501 };
        return Promise.resolve(jsonResponse(a.status, a.body));
      }
      const wi = url.match(/^\/api\/projects\/([^/?]+)\/work-items/);
      if (wi) {
        const r: StubResponse = (cfg.workItems ?? (() => ({ status: 501 })))(
          decodeURIComponent(wi[1]),
        );
        return Promise.resolve(jsonResponse(r.status, r.body));
      }
      const se = url.match(/^\/api\/projects\/([^/?]+)\/overview\?window=30d/);
      if (se) {
        const r: StubResponse = (cfg.series ?? (() => ({ status: 501 })))(
          decodeURIComponent(se[1]),
        );
        return Promise.resolve(jsonResponse(r.status, r.body));
      }
      return Promise.resolve(jsonResponse(501, { error: "not wired" }));
    }),
  );
}

describe("pure projections", () => {
  it("mixFromPhaseCounts folds phase counts through phaseTone", () => {
    expect(mixFromPhaseCounts({ Running: 2, "Collecting output": 1, Paused: 1, Failed: 2 })).toEqual({
      running: 3,
      paused: 1,
      blocked: 2,
      idle: 0,
    });
    expect(mixFromPhaseCounts({})).toEqual({ running: 0, paused: 0, blocked: 0, idle: 0 });
  });

  it("activeRunCount counts only live-work phases fleet-wide", () => {
    expect(activeRunCount(overview.projects)).toBe(1);
    expect(activeRunCount(null)).toBe(0);
  });

  it("fleetRuns flattens newest-claim-first and caps the limit", () => {
    const runs = fleetRuns(overview.projects, 2);
    expect(runs.map((r) => r.name)).toEqual(["run-infra", "run-fix"]);
    expect(runs[0].project.name).toBe("infra");
  });

  it("formatWhen renders compact UTC or an honest dash", () => {
    expect(formatWhen("2026-09-16T15:04:05Z")).toBe("2026-09-16 15:04");
    expect(formatWhen(null)).toBe("—");
    expect(formatWhen("not-a-date")).toBe("—");
  });

  it("partitionProjects splits the synthetic Unassigned/other bucket out (ISI-4570)", () => {
    const withBucket = [
      ...overview.projects!,
      {
        name: "Unassigned/other",
        namespace: "squad-alpha",
        runs: [{ name: "run-orphan", phase: "Running", claimedAt: "2026-09-16T13:00:00Z" }],
        phaseCounts: { Running: 1 },
      },
    ];
    const { projects, unassigned } = partitionProjects(withBucket);
    expect(projects.map((p) => p.name)).toEqual(["webapp", "infra"]);
    expect(unassigned?.runs).toHaveLength(1);
    expect(partitionProjects(null)).toEqual({ projects: [], unassigned: null });
    expect(partitionProjects(overview.projects).unassigned).toBeNull();
  });
});

describe("FleetOverview render", () => {
  it("renders the stat band, whole-card project links, mix bars, ticket deep links, and live runs", async () => {
    stubRoutes({
      overview: { status: 200, body: overview },
      agents: { status: 200, body: { agents: [{ id: "a1", name: "cade", namespace: "squad-alpha", role: "developer", skillCount: 3 }] } },
      workItems: (p) =>
        p === "webapp" ? { status: 200, body: webappItems } : { status: 200, body: [] },
      series: (p) =>
        p === "webapp"
          ? { status: 200, body: { tokens: { available: true, total: 1234 } } }
          : { status: 200, body: { tokens: { available: false } } },
    });
    render(<FleetOverview />);

    await waitFor(() =>
      expect(screen.getByTestId("fleet-overview")).toBeTruthy(),
    );

    // Stat band: 1 active run (webapp's Running), 2 projects, 1 open ticket (t1; t2 is done).
    expect(screen.getByRole("group", { name: "Active Runs" }).textContent).toContain("1");
    expect(screen.getByRole("group", { name: "Projects" }).textContent).toContain("2");
    await waitFor(() =>
      expect(screen.getByRole("group", { name: "Open Tickets" }).textContent).toContain("1"),
    );
    expect(screen.getByRole("group", { name: "Agents" }).textContent).toContain("1");
    await waitFor(() =>
      expect(screen.getByRole("group", { name: "Tokens · 30d" }).textContent).toContain("1.2k"),
    );

    // Project cards: whole-card links into the project overview route, namespace shown.
    const cards = screen.getAllByTestId("fleet-project-card");
    expect(cards).toHaveLength(2);
    expect(cards[0].getAttribute("href")).toBe("/projects/webapp");

    // Mix bar: webapp = 1 running + 1 idle (Succeeded), infra = 1 paused.
    expect(screen.getByLabelText("1 running, 0 paused, 0 blocked, 1 idle")).toBeTruthy();
    expect(screen.getByLabelText("0 running, 1 paused, 0 blocked, 0 idle")).toBeTruthy();

    // Recent ticket work: newest first, deep link into the issue route with the item id.
    const links = screen.getAllByTestId("fleet-recent-ticket-link");
    expect(links[0].textContent).toBe("Fix login"); // t1 updated 09-16 > t2's 09-14
    expect(links[0].getAttribute("href")).toBe("/projects/webapp/issues/t1");
    expect(links).toHaveLength(2);

    // Live runs: newest claim first (run-infra), phase chip present.
    const live = screen.getByTestId("fleet-live-runs");
    expect(live.textContent).toContain("run-infra");
    expect(live.textContent).toContain("infra");
  });

  it("degrades honestly when the overview read model is not wired (501)", async () => {
    stubRoutes({ overview: { status: 501 } });
    render(<FleetOverview />);
    await waitFor(() =>
      expect(screen.getByTestId("fleet-not-wired")).toBeTruthy(),
    );
  });

  it("shows the unassigned bucket as a count, never as a project card (ISI-4570/ISI-4577)", async () => {
    const withBucket: SquadOverviewData = {
      ...overview,
      projects: [
        ...overview.projects!,
        {
          name: "Unassigned/other",
          namespace: "squad-alpha",
          runs: [
            { name: "run-orphan-1", workItem: "wi-9", phase: "Running", claimedAt: "2026-09-16T13:00:00Z" },
            { name: "run-orphan-2", workItem: "wi-10", phase: "Failed", claimedAt: "2026-09-16T12:30:00Z" },
          ],
          phaseCounts: { Running: 1, Failed: 1 },
        },
      ],
    };
    stubRoutes({
      overview: { status: 200, body: withBucket },
      agents: { status: 200, body: { agents: [] } },
      workItems: () => ({ status: 200, body: [] }),
      series: () => ({ status: 200, body: { tokens: { available: false } } }),
    });
    render(<FleetOverview />);

    await waitFor(() => expect(screen.getByTestId("fleet-overview")).toBeTruthy());

    // The bucket is not a project: still exactly 2 cards, Projects tile still 2.
    expect(screen.getAllByTestId("fleet-project-card")).toHaveLength(2);
    expect(screen.getByRole("group", { name: "Projects" }).textContent).toContain("2");
    expect(screen.queryByText("Unassigned/other")).toBeNull();

    // No work-items/series fan-out for the synthetic bucket.
    const calls = (fetch as ReturnType<typeof vi.fn>).mock.calls.map((c) => String(c[0]));
    expect(calls.some((u) => u.includes("Unassigned"))).toBe(false);

    // Its runs surface as a count bucket on the Live Agent Runs panel, not as rows.
    await waitFor(() =>
      expect(screen.getByTestId("fleet-unassigned-runs").textContent).toContain("2 unassigned runs"),
    );
    const live = screen.getByTestId("fleet-live-runs");
    expect(live.textContent).not.toContain("run-orphan-1");
  });

  it("renders tokens as an honest dash when no project reports them", async () => {
    stubRoutes({
      overview: { status: 200, body: overview },
      workItems: () => ({ status: 200, body: [] }),
      series: () => ({ status: 200, body: { tokens: { available: false } } }),
    });
    render(<FleetOverview />);
    await waitFor(() =>
      expect(screen.getByTestId("fleet-overview")).toBeTruthy(),
    );
    await waitFor(() =>
      expect(screen.getByRole("group", { name: "Tokens · 30d" }).textContent).toContain("—"),
    );
  });
});
