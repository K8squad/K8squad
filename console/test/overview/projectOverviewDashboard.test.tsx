// test/overview/projectOverviewDashboard.test.tsx — the per-project overview redesigned to
// Mock D (ISI-5709 WS-4). Covers the four composed contracts and their degrade-don't-blank
// (ISI-4229) / fabrication (FR-I3) discipline: the real Open-PRs tile + GitHub strip from
// GET /api/squad/github (this project's repo row), the agent-card grid folded from
// GET /api/squad/agents/activity by deep-link attribution, and the Jump-into-Issues CTA.
// Route-aware fetch stub, same discipline as fleetOverview.test.tsx.

import { describe, it, expect, afterEach, vi } from "vitest";
import {
  render,
  screen,
  cleanup,
  waitFor,
  within,
} from "@testing-library/react";
import {
  ProjectOverviewDashboard,
  activeRunCount,
} from "@/components/overview/ProjectOverviewDashboard";
import type { SquadOverviewData } from "@/components/SquadOverview";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const PROJECT = "squad-alpha/webapp"; // composite id, as the page passes it (decodeProjectId)
const WORK_ITEMS_URL = "/api/projects/squad-alpha%2Fwebapp/work-items";

const overview: SquadOverviewData = {
  team: { name: "alpha", namespace: "squad-alpha", uid: "u1" },
  projects: [
    {
      name: "webapp",
      namespace: "squad-alpha",
      repoUrl: "https://github.com/K8squad/K8squad",
      runs: [
        {
          name: "run-live",
          workItem: "wi-1",
          phase: "Running",
          claimedAt: "2026-09-16T10:00:00Z",
        },
        {
          name: "run-done",
          workItem: "wi-2",
          phase: "Succeeded",
          claimedAt: "2026-09-15T10:00:00Z",
        },
      ],
      phaseCounts: { Running: 1, Succeeded: 1 },
    },
  ],
};

const githubSummary = {
  totals: {
    issuesAssigned: 2,
    issuesOpen: 6,
    prsOpened: 4,
    prsReviewed: 2,
    prsMerged: 7,
    checksRed: 1,
  },
  mergeSparkline7d: [0, 1, 0, 2, 1, 0, 3],
  byRepo: [
    {
      repo: "k8squad/k8squad",
      assigned: 2,
      open: 6,
      prs: 4,
      reviewed: 2,
      checksRed: 1,
    },
  ],
};

const activity = {
  counts: { running: 1, idle: 1, blocked: 0 },
  agents: [
    {
      agentId: "alice",
      name: "Alice",
      role: "Backend",
      state: "running",
      currentOrLastRun: {
        runId: "r1",
        phase: "Running",
        workItemRef: "ISI-1",
        workItemUrl: "/projects/squad-alpha%2Fwebapp/issues/ISI-1",
        lastThinkingSnippet: "wiring the proxy route",
        lastActivityAt: "2026-09-16T10:00:00Z",
      },
    },
    {
      // different project — must NOT appear in this project's grid
      agentId: "bob",
      name: "Bob",
      state: "running",
      currentOrLastRun: {
        runId: "r2",
        phase: "Running",
        workItemUrl: "/projects/squad-beta%2Fother/issues/ISI-9",
      },
    },
  ],
};

const workItems = [
  {
    id: "ISI-1",
    title: "First ticket",
    state: "in_progress",
    updatedAt: "2026-09-16T09:00:00Z",
  },
  {
    id: "ISI-2",
    title: "Done ticket",
    state: "done",
    updatedAt: "2026-09-15T09:00:00Z",
  },
];

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

type StubResponse = { status: number; body?: unknown };

function stubRoutes(cfg: {
  overview?: StubResponse;
  workItems?: StubResponse;
  github?: StubResponse;
  activity?: StubResponse;
}) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/api/squad/overview") {
        const r = cfg.overview ?? { status: 200, body: overview };
        return Promise.resolve(jsonResponse(r.status, r.body));
      }
      if (url.startsWith(WORK_ITEMS_URL)) {
        const r = cfg.workItems ?? { status: 200, body: workItems };
        return Promise.resolve(jsonResponse(r.status, r.body));
      }
      if (url === "/api/squad/github") {
        const r = cfg.github ?? { status: 200, body: githubSummary };
        return Promise.resolve(jsonResponse(r.status, r.body));
      }
      if (url === "/api/squad/agents/activity") {
        const r = cfg.activity ?? { status: 200, body: activity };
        return Promise.resolve(jsonResponse(r.status, r.body));
      }
      return Promise.resolve(jsonResponse(404, { error: `unstubbed ${url}` }));
    }),
  );
}

describe("activeRunCount", () => {
  it("counts only live-work runs (phaseTone running)", () => {
    expect(activeRunCount(overview.projects![0])).toBe(1);
    expect(activeRunCount(null)).toBe(0);
  });
});

describe("ProjectOverviewDashboard — Mock D", () => {
  it("renders the real Open-PRs tile + GitHub strip from this project's repo row", async () => {
    stubRoutes({});
    render(<ProjectOverviewDashboard projectId={PROJECT} />);

    // Open PRs tile shows the authoritative 4 (not the old hard-coded "—").
    const openPrs = await screen.findByLabelText("Open PRs");
    expect(within(openPrs).getByText("4")).toBeInTheDocument();

    // GitHub strip renders every chip, keyed off the matched byRepo row.
    const strip = await screen.findByTestId("gh-strip");
    expect(within(strip).getByTestId("gh-chip-prs")).toHaveTextContent("4");
    expect(within(strip).getByTestId("gh-chip-checksRed")).toHaveTextContent(
      "1",
    );
    expect(within(strip).getByTestId("gh-chip-reviewed")).toHaveTextContent(
      "2",
    );
  });

  it("renders only the agents attributed to THIS project, with classified thinking", async () => {
    stubRoutes({});
    render(<ProjectOverviewDashboard projectId={PROJECT} />);

    const cards = await screen.findAllByTestId("agent-run-card");
    expect(cards).toHaveLength(1); // Bob is on squad-beta/other → excluded
    expect(screen.getByText("Alice")).toBeInTheDocument();
    expect(screen.getByText("wiring the proxy route")).toBeInTheDocument();
    expect(screen.queryByText("Bob")).not.toBeInTheDocument();

    // Agents stat tile reflects the per-project count (1), not the fleet roster (2).
    const agentsTile = screen.getByLabelText("Agents");
    expect(within(agentsTile).getByText("1")).toBeInTheDocument();
  });

  it("offers a Jump-into-Issues CTA deep-linking the project issues route", async () => {
    stubRoutes({});
    render(<ProjectOverviewDashboard projectId={PROJECT} />);
    const cta = await screen.findByTestId("cta-issues");
    expect(cta).toHaveAttribute(
      "href",
      "/projects/squad-alpha%2Fwebapp/issues",
    );
  });

  it("degrades the GitHub panel + Open-PRs tile to '—' when the summary is unavailable (FR-I3)", async () => {
    stubRoutes({ github: { status: 501 } });
    render(<ProjectOverviewDashboard projectId={PROJECT} />);

    await waitFor(() => {
      expect(
        screen.getByText(
          "GitHub summary not available yet on this deployment.",
        ),
      ).toBeInTheDocument();
    });
    const openPrs = screen.getByLabelText("Open PRs");
    expect(within(openPrs).getByText("—")).toBeInTheDocument();
    expect(screen.queryByTestId("gh-strip")).not.toBeInTheDocument();
  });

  it("shows an honest empty agent grid when no agent works this project", async () => {
    stubRoutes({
      activity: {
        status: 200,
        body: { counts: { running: 0, idle: 0, blocked: 0 }, agents: [] },
      },
    });
    render(<ProjectOverviewDashboard projectId={PROJECT} />);
    await waitFor(() => {
      expect(
        screen.getByText("No agents are working on this project right now."),
      ).toBeInTheDocument();
    });
    expect(screen.queryByTestId("agent-run-card")).not.toBeInTheDocument();
  });

  it("surfaces 'no mirrored repo' instead of fabricating counts when the repo is absent", async () => {
    stubRoutes({
      github: {
        status: 200,
        body: {
          totals: githubSummary.totals,
          mergeSparkline7d: [],
          byRepo: [],
        },
      },
    });
    render(<ProjectOverviewDashboard projectId={PROJECT} />);
    await waitFor(() => {
      expect(screen.getByTestId("gh-strip-no-repo")).toBeInTheDocument();
    });
    const openPrs = screen.getByLabelText("Open PRs");
    expect(within(openPrs).getByText("—")).toBeInTheDocument();
  });
});
