// test/overview/fleetOverview.test.tsx — the global/fleet overview recomposed per
// Mock A (ISI-5708 WS-3). Covers the surviving pure projections (mix fold, active-run
// count, fleet runs, time formatting, project partition, total runs) and the five-row
// Mock-A assembly: ① agent-state stat band (running/idle/blocked), ② time-series row,
// ③ GitHub summary strip, ④ current-runs agent-card grid (WS-2 AgentRunCard), ⑤ the
// per-project jump row with authoritative open-PR counts + issue deep links. Also the
// honest per-panel degrade paths (overview 501, activity/github/series unavailable).
// Route-aware fetch stub, same discipline as the S2/S3 suites.

import { describe, it, expect, afterEach, vi } from "vitest";
import {
  render,
  screen,
  cleanup,
  waitFor,
  within,
} from "@testing-library/react";
import {
  FleetOverview,
  mixFromPhaseCounts,
  activeRunCount,
  fleetRuns,
  formatWhen,
  totalRuns,
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
      repoUrl: "https://github.com/acme/webapp.git",
      runs: [
        {
          name: "run-fix",
          workItem: "wi-1",
          phase: "Running",
          claimedAt: "2026-09-16T10:00:00Z",
        },
        {
          name: "run-old",
          workItem: "wi-2",
          phase: "Succeeded",
          claimedAt: "2026-09-15T10:00:00Z",
        },
      ],
      phaseCounts: { Running: 1, Succeeded: 1 },
    },
    {
      name: "infra",
      namespace: "squad-alpha",
      runs: [
        {
          name: "run-infra",
          phase: "Paused",
          claimedAt: "2026-09-16T12:00:00Z",
        },
      ],
      phaseCounts: { Paused: 1 },
    },
  ],
};

const activity = {
  agents: [
    {
      agentId: "a1",
      name: "Winston",
      role: "Backend Engineer",
      state: "running",
      currentOrLastRun: {
        runId: "r1",
        phase: "Running",
        workItemRef: "ISI-1",
        workItemUrl: "/projects/webapp/issues/ISI-1",
        lastThinkingSnippet: "wiring the apiserver handler",
        lastActivityAt: "2026-10-12T15:00:00Z",
      },
    },
    {
      agentId: "a2",
      name: "Alfred",
      state: "idle",
      currentOrLastRun: {
        runId: "r0",
        phase: "Succeeded",
        workItemRef: "ISI-0",
        workItemUrl: "/projects/webapp/issues/ISI-0",
        lastThinkingSnippet: "merged the PR",
        lastActivityAt: "2026-10-12T12:00:00Z",
      },
    },
  ],
  counts: { running: 1, idle: 1, blocked: 0 },
  fleet: false,
};

const series = {
  window: { from: "2026-09-12", to: "2026-10-12", bucket: "day" },
  ticketsByStatus: {
    available: true,
    snapshots: [
      { date: "2026-10-10", counts: { backlog: 3, in_progress: 2, done: 1 } },
      { date: "2026-10-11", counts: { backlog: 2, in_progress: 3, done: 2 } },
      {
        date: "2026-10-12",
        counts: { backlog: 1, in_progress: 2, in_review: 1, done: 4 },
      },
    ],
  },
  runsByStatus: {
    available: true,
    byPhase: { succeeded: 5, running: 2 },
    total: 7,
  },
  tokens: { available: true, total: 1234 },
};

const github = {
  totals: {
    issuesOpen: 10,
    issuesAssigned: 4,
    prsOpened: 3,
    prsReviewed: 5,
    prsMerged: 7,
    checksRed: 1,
  },
  mergeSparkline7d: [0, 1, 2, 0, 3, 1, 0],
  byRepo: [
    {
      repo: "acme/webapp",
      assigned: 1,
      open: 2,
      prs: 3,
      reviewed: 2,
      checksRed: 0,
    },
  ],
};

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

type StubResponse = { status: number; body?: unknown };

/** Route-aware fetch stub for the four fleet reads. */
function stubRoutes(cfg: {
  overview: StubResponse;
  activity?: StubResponse;
  series?: StubResponse;
  github?: StubResponse;
}) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/api/squad/overview") {
        return Promise.resolve(
          jsonResponse(cfg.overview.status, cfg.overview.body),
        );
      }
      if (url === "/api/squad/agents/activity") {
        const a = cfg.activity ?? { status: 501 };
        return Promise.resolve(jsonResponse(a.status, a.body));
      }
      if (url.startsWith("/api/squad/overview/series")) {
        const s = cfg.series ?? { status: 501 };
        return Promise.resolve(jsonResponse(s.status, s.body));
      }
      if (url === "/api/squad/github") {
        const g = cfg.github ?? { status: 501 };
        return Promise.resolve(jsonResponse(g.status, g.body));
      }
      return Promise.resolve(jsonResponse(501, { error: "not wired" }));
    }),
  );
}

describe("pure projections", () => {
  it("mixFromPhaseCounts folds phase counts through phaseTone", () => {
    expect(
      mixFromPhaseCounts({
        Running: 2,
        "Collecting output": 1,
        Paused: 1,
        Failed: 2,
      }),
    ).toEqual({
      running: 3,
      paused: 1,
      blocked: 2,
      idle: 0,
    });
    expect(mixFromPhaseCounts({})).toEqual({
      running: 0,
      paused: 0,
      blocked: 0,
      idle: 0,
    });
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

  it("totalRuns sums every phase bucket", () => {
    expect(totalRuns(overview.projects![0])).toBe(2);
    expect(totalRuns(overview.projects![1])).toBe(1);
    expect(
      totalRuns({ name: "x", namespace: "n", runs: [], phaseCounts: {} }),
    ).toBe(0);
  });

  it("partitionProjects splits the synthetic Unassigned/other bucket out (ISI-4570)", () => {
    const withBucket = [
      ...overview.projects!,
      {
        name: "Unassigned/other",
        namespace: "squad-alpha",
        runs: [
          {
            name: "run-orphan",
            phase: "Running",
            claimedAt: "2026-09-16T13:00:00Z",
          },
        ],
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

describe("FleetOverview render (Mock A)", () => {
  it("renders the agent-state stat band, time-series, GitHub strip, agent grid, and project jump row", async () => {
    stubRoutes({
      overview: { status: 200, body: overview },
      activity: { status: 200, body: activity },
      series: { status: 200, body: series },
      github: { status: 200, body: github },
    });
    render(<FleetOverview />);

    await waitFor(() =>
      expect(screen.getByTestId("fleet-overview")).toBeTruthy(),
    );

    // ① stat band: agent states first (running/idle/blocked), then projects + tokens.
    await waitFor(() =>
      expect(
        screen.getByRole("group", { name: "Running" }).textContent,
      ).toContain("1"),
    );
    expect(screen.getByRole("group", { name: "Idle" }).textContent).toContain(
      "1",
    );
    expect(
      screen.getByRole("group", { name: "Blocked" }).textContent,
    ).toContain("0");
    expect(
      screen.getByRole("group", { name: "Projects" }).textContent,
    ).toContain("2");
    await waitFor(() =>
      expect(
        screen.getByRole("group", { name: "Tokens · 30d" }).textContent,
      ).toContain("1.2k"),
    );

    // ② status-breakdown row: issues-by-status snapshot distribution from the latest snapshot
    //    ({backlog:1, in_progress:2, in_review:1, done:4}). Todo folds backlog+todo.
    const issues = await screen.findByTestId("fleet-issues-by-status");
    const issueRows = issues.querySelectorAll(".ov-hbar__row");
    const byLabel = (label: string) =>
      Array.from(issueRows).find(
        (r) => r.querySelector(".ov-hbar__label")?.textContent === label,
      );
    expect(byLabel("Todo")?.querySelector(".ov-hbar__value")?.textContent).toBe(
      "1",
    );
    expect(
      byLabel("In progress")?.querySelector(".ov-hbar__value")?.textContent,
    ).toBe("2");
    expect(
      byLabel("In review")?.querySelector(".ov-hbar__value")?.textContent,
    ).toBe("1");
    expect(byLabel("Done")?.querySelector(".ov-hbar__value")?.textContent).toBe(
      "4",
    );
    // Blocked is a work-item condition, not a §13 board state → unavailable bucket renders an
    // em-dash with NO bar, never a fabricated 0 (FR-I3).
    const blocked = byLabel("Blocked");
    expect(blocked?.getAttribute("data-unavailable")).toBe("true");
    expect(blocked?.querySelector(".ov-hbar__value")?.textContent).toBe("—");
    expect(blocked?.querySelector(".ov-hbar__fill")).toBeNull();

    // ③ over-time row present; runs-over-time degrades honestly (read model pending backend).
    expect(screen.getByTestId("fleet-series")).toBeTruthy();
    expect(screen.getByText(/run-phase history read model/i)).toBeTruthy();

    // ③ GitHub strip: authoritative counts + merge sparkline.
    const gh = await screen.findByTestId("fleet-github");
    expect(gh.textContent).toContain("10"); // issues open
    expect(gh.textContent).toContain("7"); // merged
    expect(gh.querySelector(".ov-sparkline")).toBeTruthy();

    // ④ agent grid: one card per agent, named (never intake-<uuid>).
    const grid = await screen.findByTestId("fleet-agent-grid");
    const cards = within(grid).getAllByTestId("agent-run-card");
    expect(cards).toHaveLength(2);
    expect(grid.textContent).toContain("Winston");
    expect(grid.textContent).toContain("wiring the apiserver handler");

    // ⑤ project jump row: webapp gets its authoritative open-PR count, infra has no repo.
    const rows = screen.getAllByTestId("fleet-project-jump");
    expect(rows).toHaveLength(2);
    const webapp = rows[0];
    expect(within(webapp).getByText("webapp").getAttribute("href")).toBe(
      "/projects/webapp",
    );
    expect(within(webapp).getByTestId("fleet-project-prs").textContent).toBe(
      "3 open PRs",
    );
    expect(
      within(webapp)
        .getByTestId("fleet-project-issues-link")
        .getAttribute("href"),
    ).toBe("/projects/webapp/issues");
    // infra reports no repo → honest "— PRs", never an invented 0 (FR-I3).
    expect(within(rows[1]).getByTestId("fleet-project-prs").textContent).toBe(
      "— PRs",
    );
  });

  it("degrades honestly when the overview read model is not wired (501)", async () => {
    stubRoutes({ overview: { status: 501 } });
    render(<FleetOverview />);
    await waitFor(() =>
      expect(screen.getByTestId("fleet-not-wired")).toBeTruthy(),
    );
  });

  it("renders agent-state tiles as honest dashes when the activity read is unavailable", async () => {
    stubRoutes({
      overview: { status: 200, body: overview },
      activity: { status: 501 },
      series: { status: 200, body: series },
      github: { status: 200, body: github },
    });
    render(<FleetOverview />);
    await waitFor(() =>
      expect(screen.getByTestId("fleet-overview")).toBeTruthy(),
    );
    await waitFor(() =>
      expect(
        screen.getByRole("group", { name: "Running" }).textContent,
      ).toContain("—"),
    );
    expect(screen.getByRole("group", { name: "Idle" }).textContent).toContain(
      "—",
    );
    // Grid shows the honest empty state, never a blank or fabricated card.
    expect(screen.queryByTestId("fleet-agent-grid")).toBeNull();
    expect(
      screen.getByText(/Agent activity read model not available/i),
    ).toBeTruthy();
  });

  it("degrades the GitHub strip and tokens without sinking the page", async () => {
    stubRoutes({
      overview: { status: 200, body: overview },
      activity: { status: 200, body: activity },
      series: { status: 501 },
      github: { status: 501 },
    });
    render(<FleetOverview />);
    await waitFor(() =>
      expect(screen.getByTestId("fleet-overview")).toBeTruthy(),
    );
    // Tokens tile is an honest dash when the series read is unavailable.
    await waitFor(() =>
      expect(
        screen.getByRole("group", { name: "Tokens · 30d" }).textContent,
      ).toContain("—"),
    );
    // GitHub strip degrades to its empty state; the agent grid still renders.
    expect(screen.getByText(/GitHub summary not available/i)).toBeTruthy();
    expect(screen.queryByTestId("fleet-github")).toBeNull();
    expect(await screen.findByTestId("fleet-agent-grid")).toBeTruthy();
    // Project jump row still lists projects (open-PR counts fall back to "—").
    expect(screen.getAllByTestId("fleet-project-jump")).toHaveLength(2);
    expect(screen.getAllByTestId("fleet-project-prs")[0].textContent).toBe(
      "— PRs",
    );
  });
});
