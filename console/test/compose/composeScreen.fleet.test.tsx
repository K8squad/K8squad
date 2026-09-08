// test/compose/composeScreen.fleet.test.tsx — Compose fleet wiring + no-own-team gate (ISI-3964).
//
// Two seams land here:
//  1. Left-pane lists for teams/skills/roles now fetch the fleet-aware GET /api/squad/* routes
//     (were hardcoded empty "None yet"). A fleet admin sees every squad's objects.
//  2. The center form is gated behind a "create your team first" prompt for any team-scoped kind
//     when the caller has no resolvable OWN team (a fleet admin, or a dangling-team tenant) — the
//     form would otherwise 404 on submit. Teams themselves are never gated (the escape hatch), and
//     the gate fails OPEN (loading/error ⇒ form) so a transient failure never blocks composing.

import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import { ComposeScreen } from "@/components/compose/ComposeScreen";

let current = new URLSearchParams();
vi.mock("next/navigation", () => ({
  useSearchParams: () => current,
}));

type Route = { status: number; body?: unknown };

/** URL-aware fetch stub: first matching prefix wins; unmatched URLs 404. Returns the spy. */
function stubRoutes(routes: Record<string, Route>) {
  const spy = vi.fn((input: string) => {
    const url = String(input);
    const key = Object.keys(routes).find((k) => url.startsWith(k));
    const r = key ? routes[key] : { status: 404 };
    return Promise.resolve({
      ok: r.status >= 200 && r.status < 300,
      status: r.status,
      text: () => Promise.resolve(JSON.stringify(r.body ?? null)),
      json: () => Promise.resolve(r.body ?? null),
    });
  });
  vi.stubGlobal("fetch", spy);
  return spy;
}

const tenantTeams = { fleet: false, teams: [{ name: "own", namespace: "own-ns", uid: "u-own", agentCount: 0, projectCount: 0 }] };
const adminTeams = { fleet: true, teams: [{ name: "bmad-squad", namespace: "bmad-demo", uid: "u-bmad", agentCount: 13, projectCount: 1 }] };

beforeEach(() => {
  current = new URLSearchParams();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("Compose left-pane fleet wiring (ISI-3964)", () => {
  it("kind=skills lists fleet skills from /api/squad/skills", async () => {
    current = new URLSearchParams("kind=skills");
    stubRoutes({
      "/api/squad/teams": { status: 200, body: tenantTeams },
      "/api/squad/skills": {
        status: 200,
        body: { skills: [{ name: "web-search", namespace: "bmad-demo", sourceType: "inline" }] },
      },
    });
    render(<ComposeScreen />);
    await waitFor(() => expect(screen.getByText("web-search")).toBeInTheDocument());
  });

  it("kind=roles lists fleet roles from /api/squad/roles", async () => {
    current = new URLSearchParams("kind=roles");
    stubRoutes({
      "/api/squad/teams": { status: 200, body: tenantTeams },
      "/api/squad/roles": {
        status: 200,
        body: { roles: [{ name: "planner", namespace: "bmad-demo", prompt: "plan-prompt" }] },
      },
    });
    render(<ComposeScreen />);
    await waitFor(() => expect(screen.getByText("planner")).toBeInTheDocument());
  });

  // ISI-3985: the agents kind was left on the dead /api/teams/current/org path (the literal
  // "current" never resolves to a team), so the pane never populated. It now uses the fleet-aware
  // /api/squad/agents like the other kinds, and surfaces each agent's skill count.
  it("kind=agents lists fleet agents from /api/squad/agents with a skill count", async () => {
    current = new URLSearchParams("kind=agents");
    const spy = stubRoutes({
      "/api/squad/teams": { status: 200, body: tenantTeams },
      "/api/squad/agents": {
        status: 200,
        body: { agents: [{ name: "cade", runtime: "claude-code", skillCount: 3 }] },
      },
    });
    render(<ComposeScreen />);
    await waitFor(() => expect(screen.getByText("cade")).toBeInTheDocument());
    expect(screen.getByText("3 skills")).toBeInTheDocument();
    // The dead legacy endpoint must never be called again.
    expect(spy.mock.calls.some((c) => String(c[0]).includes("/api/teams/current/org"))).toBe(false);
  });

  it("singularizes the skill-count subtitle for a one-skill agent", async () => {
    current = new URLSearchParams("kind=agents");
    stubRoutes({
      "/api/squad/teams": { status: 200, body: tenantTeams },
      "/api/squad/agents": {
        status: 200,
        body: { agents: [{ name: "solo", runtime: "claude-code", skillCount: 1 }] },
      },
    });
    render(<ComposeScreen />);
    await waitFor(() => expect(screen.getByText("solo")).toBeInTheDocument());
    expect(screen.getByText("1 skill")).toBeInTheDocument();
  });

  it("clicking an agent in the list opens it in Edit mode with the name pre-filled (ISI-3985)", async () => {
    current = new URLSearchParams("kind=agents");
    stubRoutes({
      "/api/squad/teams": { status: 200, body: tenantTeams },
      "/api/squad/agents": {
        status: 200,
        body: { agents: [{ name: "cade", runtime: "claude-code", skillCount: 3 }] },
      },
    });
    render(<ComposeScreen />);
    await waitFor(() => expect(screen.getByText("cade")).toBeInTheDocument());
    fireEvent.click(screen.getByText("cade"));
    expect(screen.getByRole("button", { name: "Edit by name" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect((screen.getByPlaceholderText("my-resource") as HTMLInputElement).value).toBe("cade");
  });
});

describe("Compose no-own-team gate (ISI-3964)", () => {
  it("a fleet admin (fleet:true, no home team) is gated on kind=agents", async () => {
    current = new URLSearchParams("kind=agents");
    stubRoutes({
      "/api/squad/teams": { status: 200, body: adminTeams },
      "/api/squad/agents": { status: 200, body: { agents: [] } },
    });
    render(<ComposeScreen />);
    await waitFor(() =>
      expect(screen.getByTestId("compose-no-team-gate")).toBeInTheDocument(),
    );
    // The guided ModelSelector (the Agent form body) must NOT render while gated.
    expect(
      screen.queryByRole("button", { name: /bring your own endpoint/i }),
    ).toBeNull();
  });

  it("clicking 'Create a Team' from the gate switches to the un-gated Teams form", async () => {
    current = new URLSearchParams("kind=agents");
    stubRoutes({
      "/api/squad/teams": { status: 200, body: adminTeams },
      "/api/squad/agents": { status: 200, body: { agents: [] } },
    });
    render(<ComposeScreen />);
    await waitFor(() =>
      expect(screen.getByTestId("compose-gate-create-team")).toBeInTheDocument(),
    );
    fireEvent.click(screen.getByTestId("compose-gate-create-team"));
    // Teams kind is never gated — the gate is gone and the Teams tab is active.
    expect(screen.queryByTestId("compose-no-team-gate")).toBeNull();
    const active = screen
      .getAllByRole("tab")
      .find((t) => t.getAttribute("aria-selected") === "true");
    expect(active).toHaveTextContent("Team");
  });

  it("a tenant WITH an own team sees the Agent form, not the gate", async () => {
    current = new URLSearchParams("kind=agents");
    stubRoutes({
      "/api/squad/teams": { status: 200, body: tenantTeams },
      "/api/squad/agents": { status: 200, body: { agents: [] } },
    });
    render(<ComposeScreen />);
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: /bring your own endpoint/i }),
      ).toBeInTheDocument(),
    );
    expect(screen.queryByTestId("compose-no-team-gate")).toBeNull();
  });

  it("kind=teams is never gated, even for a fleet admin", async () => {
    current = new URLSearchParams("kind=teams");
    stubRoutes({ "/api/squad/teams": { status: 200, body: adminTeams } });
    render(<ComposeScreen />);
    // Give the async scope fetch a tick to resolve, then assert the gate never appears.
    await waitFor(() =>
      expect(screen.getByText("bmad-squad")).toBeInTheDocument(),
    );
    expect(screen.queryByTestId("compose-no-team-gate")).toBeNull();
  });
});
