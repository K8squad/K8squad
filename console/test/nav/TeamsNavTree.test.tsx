// test/nav/TeamsNavTree.test.tsx — the Teams rail sub-tree island (ISI-4001, story ISI-3995).
//
// Component-boundary coverage of the ACs: expandable + collapsed by default (AC1); scoped, sorted
// team enumeration incl. the tenant one-team case (AC2); lazy per-team agent load, cached, with a
// neutral "No agents" leaf (AC3); agent leaf href = /agents/{id} + active-from-pathname (AC4/AC6);
// honest states at each sub-level — 501/error/loading/empty (AC5); a11y disclosure semantics (AC7).

import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, waitFor, within, fireEvent } from "@testing-library/react";
import { TeamsNavTree } from "@/components/nav/TeamsNavTree";
import type { TeamSummary } from "@/lib/teams";

// next/link → a plain anchor (no AppRouter context in jsdom); usePathname is prop-overridden below.
vi.mock("next/link", () => ({
  __esModule: true,
  default: ({ href, children, ...rest }: any) => (
    <a href={typeof href === "string" ? href : "#"} {...rest}>
      {children}
    </a>
  ),
}));
vi.mock("next/navigation", () => ({ usePathname: () => "/" }));

afterEach(cleanup);

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

const twoTeams: TeamSummary[] = [
  { name: "bravo", namespace: "squad-b", uid: "uid-b" },
  { name: "alpha", namespace: "squad-a", uid: "uid-a" },
];

const orgAlpha = {
  teamId: "uid-a",
  teamName: "alpha",
  agents: [
    { id: "ag-2", name: "Zoe", runtimeType: "openclaw", status: "idle", roles: [] },
    { id: "ag-1", name: "Amir", runtimeType: "hermes", status: "running", roles: [] },
  ],
};

describe("<TeamsNavTree> — ISI-4001", () => {
  it("AC1: collapsed by default — the label links to /teams, the chevron is a separate toggle", () => {
    render(<TeamsNavTree loadTeams={async () => jsonResponse(200, { teams: twoTeams })} />);
    // Label link present + pointing at /teams; sub-tree not rendered yet.
    const label = screen.getByRole("link", { name: "Teams" });
    expect(label).toHaveAttribute("href", "/teams");
    const toggle = screen.getByTestId("teams-toggle");
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("group", { name: "Teams" })).toBeNull();
  });

  it("AC2: expanding lists teams sorted by name (fleet admin sees both)", async () => {
    render(<TeamsNavTree loadTeams={async () => jsonResponse(200, { teams: twoTeams, fleet: true })} />);
    fireEvent.click(screen.getByTestId("teams-toggle"));
    expect(screen.getByTestId("teams-toggle")).toHaveAttribute("aria-expanded", "true");
    await waitFor(() => screen.getByTestId("team-toggle-alpha"));
    // Deterministic order: alpha before bravo.
    const group = screen.getByRole("group", { name: "Teams" });
    const teamLinks = within(group).getAllByRole("link").map((a) => a.textContent);
    expect(teamLinks).toEqual(["alpha", "bravo"]);
  });

  it("AC2: a tenant payload shows exactly one team and no other", async () => {
    render(
      <TeamsNavTree
        defaultExpanded
        loadTeams={async () => jsonResponse(200, { teams: [twoTeams[1]] })}
      />,
    );
    await waitFor(() => screen.getByTestId("team-toggle-alpha"));
    expect(screen.queryByTestId("team-toggle-bravo")).toBeNull();
  });

  it("AC3: expanding a team lazy-loads its agents, then caches (no refetch on re-open)", async () => {
    const loadOrg = vi.fn(async () => jsonResponse(200, orgAlpha));
    render(
      <TeamsNavTree
        defaultExpanded
        loadTeams={async () => jsonResponse(200, { teams: [twoTeams[1]] })}
        loadOrg={loadOrg}
      />,
    );
    await waitFor(() => screen.getByTestId("team-toggle-alpha"));
    expect(loadOrg).not.toHaveBeenCalled(); // lazy: nothing fetched until the team opens

    fireEvent.click(screen.getByTestId("team-toggle-alpha"));
    await waitFor(() => screen.getByRole("link", { name: "Amir" }));
    expect(loadOrg).toHaveBeenCalledTimes(1);
    // Agents sorted by name: Amir before Zoe.
    const agentGroup = screen.getByRole("group", { name: "alpha agents" });
    expect(within(agentGroup).getAllByRole("link").map((a) => a.textContent)).toEqual([
      "Amir",
      "Zoe",
    ]);

    // Collapse + re-open → served from cache, loadOrg not called again.
    fireEvent.click(screen.getByTestId("team-toggle-alpha"));
    fireEvent.click(screen.getByTestId("team-toggle-alpha"));
    await waitFor(() => screen.getByRole("link", { name: "Amir" }));
    expect(loadOrg).toHaveBeenCalledTimes(1);
  });

  it("AC3: a team with zero agents shows a neutral 'No agents' leaf (not blank, not fabricated)", async () => {
    render(
      <TeamsNavTree
        defaultExpanded
        loadTeams={async () => jsonResponse(200, { teams: [twoTeams[1]] })}
        loadOrg={async () => jsonResponse(200, { teamId: "uid-a", teamName: "alpha", agents: [] })}
      />,
    );
    fireEvent.click(await screen.findByTestId("team-toggle-alpha"));
    await waitFor(() => screen.getByTestId("agents-empty-alpha"));
    expect(screen.getByTestId("agents-empty-alpha")).toHaveTextContent("No agents");
  });

  it("AC4/AC6: agent leaf href = /agents/{id}; active team + agent derived from the pathname", async () => {
    render(
      <TeamsNavTree
        active
        defaultExpanded
        pathname="/agents/ag-1"
        loadTeams={async () => jsonResponse(200, { teams: [twoTeams[1]] })}
        loadOrg={async () => jsonResponse(200, orgAlpha)}
      />,
    );
    fireEvent.click(await screen.findByTestId("team-toggle-alpha"));
    const leaf = await screen.findByRole("link", { name: "Amir" });
    expect(leaf).toHaveAttribute("href", "/agents/ag-1");
    expect(leaf).toHaveAttribute("aria-current", "page");
    // The ancestor team is marked active because the URL's agent belongs to it (AC4).
    const teamLink = screen.getByRole("link", { name: "alpha" });
    expect(teamLink).toHaveAttribute("data-active", "true");
  });

  it("AC5: teams 501 → 'not wired yet' (honest, no fabricated rows)", async () => {
    render(<TeamsNavTree defaultExpanded loadTeams={async () => jsonResponse(501, {})} />);
    await waitFor(() => screen.getByTestId("teams-tree-unconfigured"));
    expect(screen.getByTestId("teams-tree-unconfigured")).toHaveTextContent("not wired yet");
  });

  it("AC5: teams fetch error → inline retry that refetches", async () => {
    const load = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(500, {}))
      .mockResolvedValueOnce(jsonResponse(200, { teams: [twoTeams[1]] }));
    render(<TeamsNavTree defaultExpanded loadTeams={load} />);
    await waitFor(() => screen.getByTestId("teams-tree-error"));
    fireEvent.click(within(screen.getByTestId("teams-tree-error")).getByText("Retry"));
    await waitFor(() => screen.getByTestId("team-toggle-alpha"));
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("AC5: a per-team agents 501 renders 'not wired yet' under that team only", async () => {
    render(
      <TeamsNavTree
        defaultExpanded
        loadTeams={async () => jsonResponse(200, { teams: [twoTeams[1]] })}
        loadOrg={async () => jsonResponse(501, {})}
      />,
    );
    fireEvent.click(await screen.findByTestId("team-toggle-alpha"));
    await waitFor(() => screen.getByTestId("agents-unconfigured-alpha"));
  });
});
