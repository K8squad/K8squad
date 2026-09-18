// test/overview/projectOverviewDashboard.test.tsx — Frame 02, the project-filtered overview
// (ISI-4508 S3). Covers the ISI-4577 wiring: the Latest runs / Live agent runs panels consume
// the fixed GET /api/squad/overview projection, and the latest-thinking inset shows the REAL
// latest thinking/comment (else latest step) from GET /api/runs/{runId} — degrading per-row to
// the honest waiting note when a run has reported nothing or its detail read fails.
// Route-aware fetch stub, same discipline as fleetOverview.test.tsx.

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/react";
import {
  ProjectOverviewDashboard,
  latestSnippet,
} from "@/components/overview/ProjectOverviewDashboard";
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
        { name: "run-new", workItem: "wi-1", phase: "Running", claimedAt: "2026-09-16T10:00:00Z" },
        { name: "run-old", workItem: "wi-2", phase: "Succeeded", claimedAt: "2026-09-15T10:00:00Z" },
      ],
      phaseCounts: { Running: 1, Succeeded: 1 },
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

function stubRoutes(cfg: {
  overview?: StubResponse;
  workItems?: StubResponse;
  agents?: StubResponse;
  series?: StubResponse;
  runDetail?: (run: string) => StubResponse;
}) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/api/squad/overview") {
        const r = cfg.overview ?? { status: 200, body: overview };
        return Promise.resolve(jsonResponse(r.status, r.body));
      }
      if (url.startsWith("/api/projects/webapp/work-items")) {
        const r = cfg.workItems ?? { status: 200, body: [] };
        return Promise.resolve(jsonResponse(r.status, r.body));
      }
      if (url === "/api/squad/agents") {
        const r = cfg.agents ?? { status: 200, body: { agents: [] } };
        return Promise.resolve(jsonResponse(r.status, r.body));
      }
      if (url.startsWith("/api/projects/webapp/overview")) {
        const r = cfg.series ?? { status: 501 };
        return Promise.resolve(jsonResponse(r.status, r.body));
      }
      const rd = url.match(/^\/api\/runs\/([^/?]+)$/);
      if (rd) {
        const r: StubResponse = (cfg.runDetail ?? (() => ({ status: 404 })))(
          decodeURIComponent(rd[1]),
        );
        return Promise.resolve(jsonResponse(r.status, r.body));
      }
      return Promise.resolve(jsonResponse(501, { error: "not wired" }));
    }),
  );
}

describe("latestSnippet", () => {
  it("prefers the newest thinking/comment content", () => {
    expect(
      latestSnippet({
        thinking: [
          { type: "comment", content: "older", timestamp: "2026-09-16T10:00:00Z" },
          { type: "comment", content: "newest thought", timestamp: "2026-09-16T10:05:00Z" },
        ],
        steps: [{ name: "claimed" }],
      }),
    ).toBe("newest thought");
  });

  it("falls back to the latest step label when nothing was thought", () => {
    expect(latestSnippet({ steps: [{ name: "claimed" }, { name: "dispatching" }] })).toBe(
      "dispatching",
    );
    expect(latestSnippet({ steps: [{ name: "x", description: "reading repo" }] })).toBe(
      "reading repo",
    );
  });

  it("returns null when the run has reported nothing", () => {
    expect(latestSnippet({})).toBeNull();
    expect(latestSnippet({ thinking: [], steps: [] })).toBeNull();
  });
});

describe("ProjectOverviewDashboard render", () => {
  it("lists the project's runs from the fixed overview and shows real latest thinking", async () => {
    stubRoutes({
      runDetail: (run) =>
        run === "run-new"
          ? {
              status: 200,
              body: {
                run: { metadata: { name: "run-new" } },
                steps: [{ id: "1", name: "claimed", status: "completed" }],
                thinking: [
                  {
                    id: "c1",
                    type: "comment",
                    content: "Investigating the failing probe",
                    timestamp: "2026-09-16T10:01:00Z",
                  },
                ],
              },
            }
          : { status: 404 },
    });
    render(<ProjectOverviewDashboard projectId="webapp" />);

    await waitFor(() =>
      expect(screen.getByTestId("project-overview-dashboard")).toBeTruthy(),
    );

    // Latest runs panel: both runs from the fixed overview, newest claim first.
    const latest = screen.getByTestId("latest-runs");
    await waitFor(() => expect(latest.textContent).toContain("run-new"));
    expect(latest.textContent).toContain("run-old");
    expect(latest.textContent?.indexOf("run-new")).toBeLessThan(
      latest.textContent?.indexOf("run-old") ?? 0,
    );

    // Latest thinking: real comment content for run-new; honest waiting note for run-old (404).
    const thinking = screen.getByTestId("live-thinking");
    await waitFor(() =>
      expect(thinking.textContent).toContain("Investigating the failing probe"),
    );
    expect(thinking.textContent).toContain("waiting for first step…");

    // View-all links land on the real runs list routes.
    const links = screen.getAllByRole("link", { name: "View all runs →" });
    expect(links.length).toBeGreaterThan(0);
    for (const l of links) {
      expect(l.getAttribute("href")).toBe("/projects/webapp/runs");
    }
  });

  it("falls back to the latest step when a run has no thinking yet", async () => {
    stubRoutes({
      runDetail: (run) =>
        run === "run-new"
          ? {
              status: 200,
              body: {
                run: { metadata: { name: "run-new" } },
                steps: [
                  { id: "1", name: "claimed", status: "completed" },
                  { id: "2", name: "dispatching", status: "running" },
                ],
                thinking: [],
              },
            }
          : { status: 404 },
    });
    render(<ProjectOverviewDashboard projectId="webapp" />);

    await waitFor(() =>
      expect(screen.getByTestId("live-thinking").textContent).toContain("dispatching"),
    );
  });
});
