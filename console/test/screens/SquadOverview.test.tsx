import { describe, it, expect, afterEach, vi, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
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
    // Run rows deep-link to Run detail; when the run carries a workItem the link
    // also threads it as `?wi=` (ISI-2884 / PR #138 kill-run wiring), so match on
    // the /runs/<name> prefix rather than an exact bare href.
    const link = document.querySelector('a[href^="/runs/run-1"]') as HTMLAnchorElement | null;
    expect(link).toBeTruthy();
    expect(link?.getAttribute("href")).toBe("/runs/run-1?wi=ticket-9");
  });

  it("links each project card title into its S1 workspace /projects/{name} (ISI-3960 AC1)", async () => {
    stubFetch(200, overviewPayload);
    render(<SquadOverview />);
    await waitFor(() => expect(screen.getByTestId("overview-ready")).toBeTruthy());
    const links = screen.getAllByTestId("overview-project-link");
    expect(links.length).toBe(2);
    // Real, keyboard-focusable anchor (not an onClick div) with the encoded deep-link.
    expect(links[0].tagName).toBe("A");
    expect(links[0].getAttribute("href")).toBe("/projects/webapp");
    expect(links[0].textContent).toBe("webapp");
    // Run-row /runs/{id} links inside the same card stay intact (no nested-anchor breakage).
    const runLink = document.querySelector('a[href^="/runs/run-1"]') as HTMLAnchorElement | null;
    expect(runLink?.getAttribute("href")).toBe("/runs/run-1?wi=ticket-9");
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
    const originalLocation = window.location;
    const fakeLocation = { href: "" };
    Object.defineProperty(window, "location", { value: fakeLocation, configurable: true });
    render(<SquadOverview />);
    await waitFor(() => expect(screen.getByTestId("overview-empty")).toBeTruthy());
    // ISI-3686: never a dead-end — the empty card carries a one-click fix into the E0
    // shared create form (Compose deep-link contract, parseComposeParams).
    fireEvent.click(screen.getByRole("button", { name: "Create a project" }));
    expect(fakeLocation.href).toBe("/compose?kind=project");
    Object.defineProperty(window, "location", { value: originalLocation, configurable: true });
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

// ISI-3965 / ISI-3950 S1: fleet-wide admin Overview render. The backend (ISI-3932) already answers
// fleet-wide for a global admin — fleet:true, synthetic Team "*", Projects spanning every squad,
// each keeping its own namespace. The console must render that honestly (fleet header, grouped by
// squad, namespace-qualified keys) while leaving the tenant path byte-for-byte unchanged.
const fleetPayload = {
  fleet: true,
  team: { name: "*", namespace: "", uid: "" },
  projects: [
    {
      name: "webapp",
      namespace: "squad-alpha",
      runs: [{ name: "run-a", phase: "Running" }],
      phaseCounts: { Running: 1 },
    },
    // Same project NAME as the one above, but a different squad (namespace). Pre-fix this collided
    // on the React key `p.name` and one row was dropped — AC3 pins that both now render.
    {
      name: "webapp",
      namespace: "squad-beta",
      runs: null,
      phaseCounts: {},
    },
    { name: "infra", namespace: "squad-beta", runs: null, phaseCounts: {} },
  ],
};

describe("<SquadOverview> — fleet-wide admin render (ISI-3965 / ISI-3950 S1)", () => {
  it("AC1: renders a fleet header, never leaking '*' or an empty namespace as a Team name", async () => {
    stubFetch(200, fleetPayload);
    render(<SquadOverview />);
    await waitFor(() => expect(screen.getByTestId("overview-ready")).toBeTruthy());
    // Fleet header present; single-Team header absent.
    expect(screen.getByTestId("overview-fleet").textContent).toContain("Fleet overview");
    expect(screen.queryByTestId("overview-team")).toBeNull();
    // The synthetic "*" marker never surfaces as a heading.
    expect(screen.getByTestId("overview-fleet").textContent).not.toContain("*");
  });

  it("AC2: attributes each project to its squad (namespace) group", async () => {
    stubFetch(200, fleetPayload);
    render(<SquadOverview />);
    await waitFor(() => expect(screen.getByTestId("overview-ready")).toBeTruthy());
    const labels = screen.getAllByTestId("overview-squad-label").map((el) => el.textContent);
    // Two squads, sorted by namespace.
    expect(labels.length).toBe(2);
    expect(labels[0]).toContain("squad-alpha");
    expect(labels[1]).toContain("squad-beta");
  });

  it("AC3: two same-named projects in different squads both render (no key collision)", async () => {
    stubFetch(200, fleetPayload);
    render(<SquadOverview />);
    await waitFor(() => expect(screen.getByTestId("overview-ready")).toBeTruthy());
    // Three projects total: webapp@alpha, webapp@beta, infra@beta — none dropped/merged.
    expect(screen.getAllByTestId("overview-project").length).toBe(3);
  });

  it("AC5: an empty fleet renders the fleet empty state, not the tenant CTA, without crashing on null projects", async () => {
    stubFetch(200, { fleet: true, team: { name: "*", namespace: "", uid: "" }, projects: null });
    render(<SquadOverview />);
    await waitFor(() => expect(screen.getByTestId("overview-ready")).toBeTruthy());
    expect(screen.getByTestId("overview-fleet-empty")).toBeTruthy();
    // Never the tenant "Create a project" CTA (it assumes a single home namespace).
    expect(screen.queryByTestId("overview-empty")).toBeNull();
    expect(screen.queryByRole("button", { name: "Create a project" })).toBeNull();
  });

  it("AC4: a tenant response (fleet absent) renders the exact single-Team path unchanged", async () => {
    stubFetch(200, overviewPayload);
    render(<SquadOverview />);
    await waitFor(() => expect(screen.getByTestId("overview-ready")).toBeTruthy());
    // Single-Team header present, fleet header absent — byte-for-byte tenant path.
    expect(screen.getByTestId("overview-team").textContent).toBe("alpha");
    expect(screen.queryByTestId("overview-fleet")).toBeNull();
    expect(screen.queryByTestId("overview-squad-label")).toBeNull();
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
