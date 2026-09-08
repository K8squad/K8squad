// test/agents/fleetTeamPicker.test.tsx — the fleet team picker for a global admin (ISI-3964).
//
// Covers the pure classifyTeamList mapper AND the rendered component: it fetches the fleet-aware
// GET /api/squad/teams and lists each squad as a card deep-linking to /agents?team={uid} — the
// override the Agents page honors to render TeamOrgDiagram. Terminal HTTP states render honestly.

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/react";
import {
  FleetTeamPicker,
  classifyTeamList,
} from "@/components/agents/FleetTeamPicker";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

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

const fleetPayload = {
  fleet: true,
  teams: [
    { name: "bmad-squad", namespace: "bmad-demo", uid: "uid-bmad", agentCount: 13, projectCount: 1 },
    { name: "alpha", namespace: "squad-alpha", uid: "uid-alpha", agentCount: 1, projectCount: 2 },
  ],
};

describe("classifyTeamList (pure mapper)", () => {
  it("401 → unauthenticated, 501 → not-wired, 5xx → error", () => {
    expect(classifyTeamList(401, null).kind).toBe("unauthenticated");
    expect(classifyTeamList(501, null).kind).toBe("not-wired");
    expect(classifyTeamList(500, null)).toEqual({ kind: "error", status: 500 });
  });

  it("200 defaults a null/absent teams slice to [] and fleet to false", () => {
    expect(classifyTeamList(200, { teams: null })).toEqual({
      kind: "ready",
      teams: [],
      fleet: false,
    });
  });

  it("200 carries the fleet flag and rows through", () => {
    const s = classifyTeamList(200, fleetPayload);
    expect(s.kind).toBe("ready");
    if (s.kind === "ready") {
      expect(s.fleet).toBe(true);
      expect(s.teams.length).toBe(2);
    }
  });
});

describe("<FleetTeamPicker>", () => {
  it("fetches /api/squad/teams and renders one card per fleet squad, deep-linking by UID", async () => {
    stubFetch(200, fleetPayload);
    render(<FleetTeamPicker />);
    await waitFor(() =>
      expect(screen.getByTestId("fleet-team-picker")).toBeTruthy(),
    );
    const links = screen.getAllByTestId("fleet-team-link");
    expect(links.length).toBe(2);
    // bmad-squad must be listed with a working link into the org view (AC).
    expect(links[0]).toHaveAttribute("href", "/agents?team=uid-bmad");
    expect(screen.getByText("bmad-squad")).toBeInTheDocument();
  });

  it("renders an empty-fleet state when no squads exist", async () => {
    stubFetch(200, { fleet: true, teams: [] });
    render(<FleetTeamPicker />);
    await waitFor(() =>
      expect(screen.getByTestId("fleet-teams-empty")).toBeTruthy(),
    );
  });

  it("renders the not-wired state on 501 (dev deployment)", async () => {
    stubFetch(501);
    render(<FleetTeamPicker />);
    await waitFor(() =>
      expect(screen.getByTestId("fleet-teams-not-wired")).toBeTruthy(),
    );
  });
});
