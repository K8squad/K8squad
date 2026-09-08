// test/projects/ProjectLanding.test.tsx — S2 (ISI-3958) landing panels.
// Covers AC1 Runs, AC2 Stats ("—" vs 0), AC3 Latest Tickets (incl. 501 not-available),
// AC4 per-panel isolation (overview 200 + work-items 501), AC5 read-only (GET-only).

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor, within } from "@testing-library/react";
import {
  ProjectLanding,
  selectProject,
  recentRuns,
  recentTickets,
  partitionIssues,
  lastActivity,
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
  { id: "t3", projectId: "webapp", parentId: null, title: "Add metrics", state: "todo", blockedReason: null, updatedAt: "2026-08-19T09:00:00Z" },
];

/** Route-aware fetch stub: overview + work-items each get their own status/body. */
function stubRoutes(cfg: {
  overview: { status: number; body?: unknown };
  workItems: { status: number; body?: unknown };
}) {
  const spy = vi.fn((input: RequestInfo | URL, _init?: RequestInit) => {
    const url = String(input);
    const pick = url.includes("/api/squad/overview") ? cfg.overview : cfg.workItems;
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

  it("recentRuns sorts claimedAt desc, unclaimed last, capped", () => {
    const runs = recentRuns(selectProject(overview, "webapp"), 8);
    expect(runs.map((r) => r.name)).toEqual(["run-new", "run-old"]);
    expect(recentRuns(selectProject(overview, "infra"), 8)).toEqual([]);
    expect(recentRuns(selectProject(overview, "webapp"), 1)).toHaveLength(1);
  });

  it("recentTickets sorts updatedAt desc; partitionIssues splits done vs rest", () => {
    expect(recentTickets(workItems, 6).map((t) => t.id)).toEqual(["t2", "t1", "t3"]);
    expect(partitionIssues(workItems)).toEqual({ open: 2, closed: 1 });
    expect(partitionIssues([])).toEqual({ open: 0, closed: 0 });
  });

  it("lastActivity is the max across runs + tickets; null when nothing known", () => {
    expect(lastActivity(selectProject(overview, "webapp"), workItems)).toBe("2026-08-23T09:00:00.000Z");
    expect(lastActivity(null, [])).toBeNull();
  });
});

describe("ProjectLanding — AC1/AC2/AC3 populated", () => {
  it("renders three populated panels from GET reads", async () => {
    stubRoutes({ overview: { status: 200, body: overview }, workItems: { status: 200, body: { items: workItems } } });
    render(<ProjectLanding projectId="webapp" />);

    await waitFor(() => expect(screen.getAllByTestId("landing-run-row").length).toBe(2));
    // AC1: newest run first, deep-linked to /runs/{id} with ?wi threading.
    const firstRow = screen.getAllByTestId("landing-run-row")[0];
    expect(within(firstRow).getByRole("link")).toHaveAttribute("href", "/runs/run-new");

    // AC2: phase chips + authoritative open/closed counts.
    expect(screen.getAllByTestId("landing-stats-phase-count").length).toBe(2);
    expect(screen.getByTestId("landing-stats-open").textContent).toBe("2");
    expect(screen.getByTestId("landing-stats-closed").textContent).toBe("1");

    // AC3: latest tickets newest-first, view-all → Issues tab.
    expect(screen.getAllByTestId("landing-ticket-row").length).toBe(3);
    expect(screen.getByTestId("landing-tickets-viewall")).toHaveAttribute("href", "/projects/webapp/issues");
  });
});

describe("ProjectLanding — AC1/AC3 empty states", () => {
  it("shows neutral empty states, never fabricated rows", async () => {
    stubRoutes({
      overview: { status: 200, body: { team: overview.team, projects: [{ name: "webapp", namespace: "n", runs: null, phaseCounts: {} }] } },
      workItems: { status: 200, body: { items: [] } },
    });
    render(<ProjectLanding projectId="webapp" />);
    await waitFor(() => expect(screen.getByTestId("landing-runs-empty")).toBeTruthy());
    expect(screen.getByTestId("landing-tickets-empty")).toBeTruthy();
    // AC2: zero runs known → "No runs." (authoritative), open/closed authoritative 0.
    expect(screen.getByTestId("landing-stats-phase-empty")).toBeTruthy();
    expect(screen.getByTestId("landing-stats-open").textContent).toBe("0");
    expect(screen.getByTestId("landing-stats-closed").textContent).toBe("0");
  });
});

describe("ProjectLanding — AC4 per-panel isolation + AC2 '—' vs 0", () => {
  it("overview 200 + work-items 501: Runs/Stats(phase) populate, tickets degrade, counts show —", async () => {
    stubRoutes({ overview: { status: 200, body: overview }, workItems: { status: 501, body: "not wired" } });
    render(<ProjectLanding projectId="webapp" />);

    // Runs + phase stats stay populated despite the failing work-items read.
    await waitFor(() => expect(screen.getAllByTestId("landing-run-row").length).toBe(2));
    expect(screen.getAllByTestId("landing-stats-phase-count").length).toBe(2);

    // Latest Tickets renders its OWN honest not-available state.
    expect(screen.getByTestId("landing-tickets-unavailable")).toBeTruthy();

    // AC2 fabrication discipline: unavailable source ⇒ "—", not 0.
    expect(screen.getByTestId("landing-stats-open").textContent).toBe("—");
    expect(screen.getByTestId("landing-stats-closed").textContent).toBe("—");
  });

  it("overview 501 + work-items 200: tickets populate, overview panels show honest problem/—", async () => {
    stubRoutes({ overview: { status: 501, body: null }, workItems: { status: 200, body: { items: workItems } } });
    render(<ProjectLanding projectId="webapp" />);
    await waitFor(() => expect(screen.getAllByTestId("landing-ticket-row").length).toBe(3));
    expect(screen.getByTestId("landing-runs-problem")).toBeTruthy();
    expect(screen.getByTestId("landing-stats-phase-unavailable")).toBeTruthy();
    // open/closed still authoritative from the good work-items read.
    expect(screen.getByTestId("landing-stats-open").textContent).toBe("2");
  });
});

describe("ProjectLanding — AC5 read-only", () => {
  it("issues only GET reads to overview + work-items; no mutation", async () => {
    const spy = stubRoutes({ overview: { status: 200, body: overview }, workItems: { status: 200, body: { items: workItems } } });
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
  });
});
