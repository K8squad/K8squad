import { describe, it, expect, afterEach, vi, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/react";
import { SquadOverview, classifyOverviewStatus, phaseTone } from "@/components/SquadOverview";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const overviewPayload = {
  team: { name: "alpha", namespace: "squad-alpha", uid: "u1" },
  // Wire shape: Go marshals a nil slice as `null` — a Team with no Projects sends
  // projects: null, and a Project with no Runs sends runs: null (overview.go builds
  // both with append/map-lookup and no omitempty). Fixtures pin that shape.
  projects: [
    {
      name: "webapp",
      namespace: "squad-alpha",
      repoUrl: "https://git.example/webapp",
      runs: [
        { name: "run-1", workItem: "ticket-9", phase: "Running", claimedAt: "2026-08-20T10:00:00Z" },
        { name: "run-2", phase: "Succeeded" },
      ],
      phaseCounts: { Running: 1, Succeeded: 1 },
    },
    { name: "infra", namespace: "squad-alpha", runs: null, phaseCounts: {} },
  ],
};

function stubFetch(status: number, body: unknown = null) {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({
      ok: status >= 200 && status < 300,
      status,
      json: () => Promise.resolve(body),
    }),
  );
}

// Story 8.1 wiring: the screen FETCHES the BFF route and renders the projection.

describe("<SquadOverview> — story 8.1 wiring (ISI-2900)", () => {
  it("fetches /api/squad/overview and renders Team, Projects, Run rows", async () => {
    stubFetch(200, overviewPayload);
    render(<SquadOverview />);
    await waitFor(() => expect(screen.getByTestId("overview-ready")).toBeTruthy());
    expect(screen.getByTestId("overview-team").textContent).toBe("alpha");
    expect(screen.getAllByTestId("overview-project").length).toBe(2);
    expect(screen.getAllByTestId("overview-run-row").length).toBe(2);
    // run-1 carries a workItem, so the deep-link is /runs/run-1?wi=ticket-9 (the
    // kill-run story appended the ?wi= context param). Match the run deep-link by
    // prefix so the assertion tracks "links to Run detail" without over-coupling to
    // the query string.
    const link = document.querySelector('a[href^="/runs/run-1"]') as HTMLAnchorElement | null;
    expect(link).toBeTruthy();
    expect(link?.getAttribute("href")).toBe("/runs/run-1?wi=ticket-9");
  });

  it("renders run rows deep-linked to Run detail and phase chips toned by phase", async () => {
    stubFetch(200, overviewPayload);
    render(<SquadOverview />);
    await waitFor(() => expect(screen.getByTestId("overview-ready")).toBeTruthy());
    const chips = screen.getAllByTestId("overview-phase-count");
    expect(chips.length).toBe(2);
    expect(chips[0].getAttribute("data-tone")).toBe("running");
  });

  it("renders the empty-Projects card when the Team has none (wire sends null)", async () => {
    stubFetch(200, { team: overviewPayload.team, projects: null });
    render(<SquadOverview />);
    await waitFor(() => expect(screen.getByTestId("overview-empty")).toBeTruthy());
  });

  it("renders a Project's no-Runs row without crashing when the wire sends runs: null", async () => {
    stubFetch(200, overviewPayload);
    render(<SquadOverview />);
    await waitFor(() => expect(screen.getByTestId("overview-ready")).toBeTruthy());
    // The "infra" project arrives with runs: null — it must render its card with the
    // "No Runs." row, not crash the console root (cursor review: nil-slice → null).
    const projects = screen.getAllByTestId("overview-project");
    expect(projects.length).toBe(2);
    expect(projects[1].textContent).toContain("No Runs.");
    expect(screen.getAllByTestId("overview-run-row").length).toBe(2);
  });

  it("renders the no-team card on 404 (session Team has no projection)", async () => {
    stubFetch(404);
    render(<SquadOverview />);
    await waitFor(() => expect(screen.getByTestId("overview-no-team")).toBeTruthy());
  });

  it("renders the unauthenticated card on 401", async () => {
    stubFetch(401);
    render(<SquadOverview />);
    await waitFor(() => expect(screen.getByTestId("overview-unauthenticated")).toBeTruthy());
  });

  it("renders the not-wired card on the documented 501", async () => {
    stubFetch(501);
    render(<SquadOverview />);
    await waitFor(() => expect(screen.getByTestId("overview-not-wired")).toBeTruthy());
  });

  it("renders the retryable error card on 5xx", async () => {
    stubFetch(502);
    render(<SquadOverview />);
    await waitFor(() => expect(screen.getByTestId("overview-error")).toBeTruthy());
  });
});

describe("classifyOverviewStatus / phaseTone — unit contract", () => {
  it("maps every relayed status to its distinct honest state", () => {
    expect(classifyOverviewStatus(401).kind).toBe("unauthenticated");
    expect(classifyOverviewStatus(404).kind).toBe("no-team");
    expect(classifyOverviewStatus(501).kind).toBe("not-wired");
    expect(classifyOverviewStatus(500).kind).toBe("error");
  });

  it("tones active/paused/terminal phases onto the status channel", () => {
    expect(phaseTone("Running")).toBe("running");
    expect(phaseTone("Collecting")).toBe("running");
    expect(phaseTone("Paused")).toBe("paused");
    expect(phaseTone("Failed")).toBe("blocked");
    expect(phaseTone("Succeeded")).toBe("idle");
    expect(phaseTone("Pending")).toBe("idle");
  });
});
