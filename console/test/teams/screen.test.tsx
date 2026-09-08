import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/react";
import { TeamsScreen } from "@/components/teams/TeamsScreen";
import type { TeamsList } from "@/lib/teams";

afterEach(cleanup);

// The Teams LIST surface (ISI-3953) at the component boundary: rows render from
// the read model, the admin fleet marker shows, and each honest state (empty /
// documented 501 unconfigured / deny-collapsed not-found / error) renders its
// own frame — never a fabricated row, never the old stub copy.

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function list(over: Partial<TeamsList> = {}): TeamsList {
  return {
    teams: [
      { name: "alpha", namespace: "squad-a", uid: "uid-a" },
      { name: "bravo", namespace: "squad-b", uid: "uid-b" },
    ],
    ...over,
  };
}

describe("<TeamsScreen> — ISI-3953", () => {
  it("renders a row per Team with namespace + an org deep-link keyed by uid (AC7)", async () => {
    render(<TeamsScreen load={async () => jsonResponse(200, list({ fleet: true }))} />);
    await waitFor(() => screen.getByTestId("teams-table"));
    expect(screen.getByText("alpha")).toBeTruthy();
    expect(screen.getByText("bravo")).toBeTruthy();
    expect(screen.getByText("squad-a")).toBeTruthy();
    // The org link carries the uid so ISI-3950's picker can open /api/teams/{uid}/org.
    const links = screen.getAllByText("View org").map((a) => a.getAttribute("href"));
    expect(links).toContain("/agents?team=uid-a");
    expect(links).toContain("/agents?team=uid-b");
    // Admin fleet marker copy is present.
    expect(screen.getByText(/viewing as an admin/i)).toBeTruthy();
  });

  it("renders the neutral empty state when the caller sees no Teams (AC7)", async () => {
    render(<TeamsScreen load={async () => jsonResponse(200, { teams: [] })} />);
    await waitFor(() => screen.getByTestId("teams-empty"));
    expect(screen.getByText(/No teams yet/i)).toBeTruthy();
    // No fabricated table.
    expect(screen.queryByTestId("teams-table")).toBeNull();
  });

  it("renders the unconfigured state on the documented 501 (AC7)", async () => {
    render(<TeamsScreen load={async () => jsonResponse(501, { error: "not implemented" })} />);
    await waitFor(() => screen.getByTestId("teams-unconfigured"));
    expect(screen.getByText(/not configured/i)).toBeTruthy();
  });

  it("renders not-found on a deny/absence collapse (401/403/404) (AC7)", async () => {
    render(<TeamsScreen load={async () => jsonResponse(404, {})} />);
    await waitFor(() => screen.getByTestId("teams-not-found"));
  });

  it("renders the error state on a 5xx (AC7)", async () => {
    render(<TeamsScreen load={async () => jsonResponse(502, {})} />);
    await waitFor(() => screen.getByTestId("teams-error"));
  });

  it("does not render the old stub copy once populated (AC8)", async () => {
    render(<TeamsScreen load={async () => jsonResponse(200, list())} />);
    await waitFor(() => screen.getByTestId("teams-table"));
    expect(screen.queryByText(/A dedicated Teams surface is coming/i)).toBeNull();
  });
});
