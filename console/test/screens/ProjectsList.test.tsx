import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import { ProjectsList, projectId } from "@/components/ProjectsList";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

// Fleet-wide wire shape (ISI-3943): admin caller ⇒ every squad's projects, each row carrying
// its owning Team UID/Name. Two namespaces deliberately share nothing — the qualified-id link
// below must carry the namespace so a same-name project in another squad can't be shadowed.
const fleetPayload = {
  fleet: true,
  projects: [
    {
      name: "webapp",
      namespace: "squad-alpha",
      teamUid: "uid-alpha",
      teamName: "alpha",
      repoUrl: "https://git.example/webapp",
      phaseCounts: { Running: 1, Succeeded: 2 },
    },
    {
      name: "webapp",
      namespace: "squad-beta",
      teamUid: "uid-beta",
      teamName: "beta",
      phaseCounts: {},
    },
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

describe("<ProjectsList> — S6 AC2 row retarget into the S1 workspace (ISI-3967)", () => {
  it("fetches /api/squad/projects and renders one row per fleet project", async () => {
    stubFetch(200, fleetPayload);
    render(<ProjectsList />);
    await waitFor(() => expect(screen.getByTestId("projects-ready")).toBeTruthy());
    expect(screen.getAllByTestId("projects-row").length).toBe(2);
    expect(screen.getAllByTestId("projects-phase-count").length).toBe(2);
  });

  it("links each row's primary click to /projects/{namespace/name} — the S1 workspace, not the interim /agents?team=", async () => {
    stubFetch(200, fleetPayload);
    render(<ProjectsList />);
    await waitFor(() => expect(screen.getByTestId("projects-ready")).toBeTruthy());
    const links = screen.getAllByTestId("projects-row-link");
    expect(links.length).toBe(2);
    // Real, keyboard-focusable anchors (not an onClick div) — S6 accessibility pin.
    expect(links[0].tagName).toBe("A");
    // Namespace-qualified + encoded exactly like SquadOverview (ISI-3960 AC1): the route
    // decodeURIComponent's the param, so the whole {namespace}/{name} id is one encoded
    // segment. The fleet list MUST qualify — both fixtures are named "webapp".
    expect(links[0].getAttribute("href")).toBe(
      `/projects/${encodeURIComponent(projectId("squad-alpha", "webapp"))}`,
    );
    expect(links[1].getAttribute("href")).toBe(
      `/projects/${encodeURIComponent(projectId("squad-beta", "webapp"))}`,
    );
    expect(links[0].textContent).toBe("webapp");
    // The retarget's whole point (AC2): the primary row anchor no longer points at the
    // interim agents-org jump.
    links.forEach((l) => expect(l.getAttribute("href")?.startsWith("/agents?team=")).toBe(false));
  });

  it("keeps the secondary squad-agents jump when the owning Team UID resolved (ISI-3943 AC2)", async () => {
    stubFetch(200, fleetPayload);
    render(<ProjectsList />);
    await waitFor(() => expect(screen.getByTestId("projects-ready")).toBeTruthy());
    const agentsLinks = screen.getAllByTestId("projects-agents-link");
    expect(agentsLinks.length).toBe(2);
    expect(agentsLinks[0].getAttribute("href")).toBe("/agents?team=uid-alpha");
  });

  it("renders the empty state when the wire sends projects: null (nil-slice contract)", async () => {
    stubFetch(200, { fleet: false, projects: null });
    const originalLocation = window.location;
    const fakeLocation = { href: "" };
    Object.defineProperty(window, "location", { value: fakeLocation, configurable: true });
    render(<ProjectsList />);
    await waitFor(() => expect(screen.getByTestId("projects-empty")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Create a project" }));
    expect(fakeLocation.href).toBe("/compose?kind=project");
    Object.defineProperty(window, "location", { value: originalLocation, configurable: true });
  });

  it("renders the honest terminal-state cards the BFF relays (classifyOverviewStatus contract)", async () => {
    stubFetch(401);
    render(<ProjectsList />);
    await waitFor(() => expect(screen.getByTestId("projects-unauthenticated")).toBeTruthy());

    cleanup();
    stubFetch(501);
    render(<ProjectsList />);
    await waitFor(() => expect(screen.getByTestId("projects-not-wired")).toBeTruthy());
  });
});
