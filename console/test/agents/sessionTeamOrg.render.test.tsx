import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import { SessionTeamOrg } from "@/components/agents/SessionTeamOrg";

// Companion to sessionTeamOrg.test.ts (which pins the pure resolveTeamState mapper). Copilot review
// of PR #224: the mapper test alone lets the landing's essential wiring regress silently — it could
// stop fetching /api/squad/overview, or resolve a UID and fail to hand it to <TeamOrgDiagram>, and
// the suite would stay green. This renders the real component (as the SquadOverview tests do),
// stubs the BFF responses, and asserts BOTH the request and the rendered diagram / terminal states.

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

// TeamOrgDiagram opens a native EventSource (useTeamStatus, live status bus) which jsdom does not
// implement. Stub it so rendering through to the diagram doesn't throw; the test asserts the initial
// server snapshot, not live deltas.
class FakeEventSource {
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((e: MessageEvent) => void) | null = null;
  url: string;
  constructor(url: string) {
    this.url = url;
  }
  close() {}
}

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
      json: () => Promise.resolve(r.body ?? null),
    });
  });
  vi.stubGlobal("fetch", spy);
  vi.stubGlobal("EventSource", FakeEventSource as unknown as typeof EventSource);
  return spy;
}

const teamOrg = {
  teamId: "u1",
  teamName: "alpha",
  agents: [
    {
      id: "agent-1",
      name: "planner",
      runtimeType: "openclaw",
      status: "running" as const,
      pausedReason: null,
      roles: [{ id: "r1", name: "Planner" }],
      currentRunId: null,
    },
  ],
};

describe("<SessionTeamOrg> — landing wiring (ISI-3543, PR #224 Copilot review)", () => {
  it("fetches /api/squad/overview, threads the resolved UID to the org endpoint, and renders the diagram", async () => {
    const fetchSpy = stubRoutes({
      "/api/squad/overview": { status: 200, body: { team: { uid: "u1" } } },
      "/api/teams/u1/org": { status: 200, body: teamOrg },
    });
    render(<SessionTeamOrg />);

    // The resolved diagram (proves the UID reached <TeamOrgDiagram> and it rendered the snapshot).
    await waitFor(() =>
      expect(screen.getByText("alpha")).toBeInTheDocument(),
    );
    expect(screen.getByText("planner")).toBeInTheDocument();

    // The wiring itself: overview was fetched, and the resolved UID (not a hard-coded value) was
    // threaded into the Team-scoped org request.
    const urls = fetchSpy.mock.calls.map((c) => String(c[0]));
    expect(urls.some((u) => u.startsWith("/api/squad/overview"))).toBe(true);
    expect(urls.some((u) => u.startsWith("/api/teams/u1/org"))).toBe(true);
  });

  it("renders a zero-agent Team with an EmptyState CTA into Compose (ISI-3686)", async () => {
    const originalLocation = window.location;
    const fakeLocation = { href: "" };
    Object.defineProperty(window, "location", { value: fakeLocation, configurable: true });
    stubRoutes({
      "/api/squad/overview": { status: 200, body: { team: { uid: "u1" } } },
      "/api/teams/u1/org": { status: 200, body: { ...teamOrg, agents: [] } },
    });
    render(<SessionTeamOrg />);
    await waitFor(() => expect(screen.getByTestId("org-empty-agents")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Add an agent" }));
    expect(fakeLocation.href).toBe("/compose?kind=agent");
    Object.defineProperty(window, "location", { value: originalLocation, configurable: true });
  });

  it("renders the unauthenticated surface on 401", async () => {
    stubRoutes({ "/api/squad/overview": { status: 401 } });
    render(<SessionTeamOrg />);
    await waitFor(() =>
      expect(screen.getByText(/sign in/i)).toBeInTheDocument(),
    );
  });

  it("renders the no-team surface on 404 without claiming a zero-agent team", async () => {
    stubRoutes({ "/api/squad/overview": { status: 404 } });
    render(<SessionTeamOrg />);
    await waitFor(() =>
      expect(
        screen.getByText(/no team org chart is available/i),
      ).toBeInTheDocument(),
    );
    // Regression guard for the Copilot finding: the old copy mis-diagnosed this as an empty team.
    expect(screen.queryByText(/no agents yet/i)).toBeNull();
  });

  it("renders the retryable error surface on 5xx", async () => {
    stubRoutes({ "/api/squad/overview": { status: 502 } });
    render(<SessionTeamOrg />);
    await waitFor(() =>
      expect(screen.getByText(/couldn’t resolve your team/i)).toBeInTheDocument(),
    );
  });
});
