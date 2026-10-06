// test/nav/ProjectsNavTree.test.tsx — the Projects rail sub-tree island (ISI-4090).
//
// Component-boundary coverage: expandable + collapsed by default (label → /projects, chevron a
// separate toggle); sorted project enumeration; static Overview/Issues/Runs/Discussion/File
// Explorer/GitHub sections (ISI-3957 vocabulary, shared via lib/nav PROJECT_SECTIONS; label
// renamed Landing → Overview by ISI-4505) with correct
// hrefs on project expand; deep-link → active project auto-expanded + active
// section highlighted; honest states (loading / empty / error + retry); a11y disclosure semantics.

import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, waitFor, within, fireEvent } from "@testing-library/react";
import { ProjectsNavTree } from "@/components/nav/ProjectsNavTree";

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

const twoProjects = [
  { id: "squad-b/beta", name: "beta" },
  { id: "squad-a/alpha", name: "alpha" },
];

describe("<ProjectsNavTree> — ISI-4090", () => {
  it("collapsed by default — the label links to /projects, the chevron is a separate toggle", () => {
    render(<ProjectsNavTree loadProjects={async () => jsonResponse(200, { projects: twoProjects })} />);
    const label = screen.getByRole("link", { name: "Projects" });
    expect(label).toHaveAttribute("href", "/projects");
    const toggle = screen.getByTestId("projects-toggle");
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("group", { name: "Projects" })).toBeNull();
  });

  it("expanding lists projects sorted by name", async () => {
    render(<ProjectsNavTree loadProjects={async () => jsonResponse(200, { projects: twoProjects })} />);
    fireEvent.click(screen.getByTestId("projects-toggle"));
    expect(screen.getByTestId("projects-toggle")).toHaveAttribute("aria-expanded", "true");
    await waitFor(() => screen.getByTestId("project-toggle-alpha"));
    const group = screen.getByRole("group", { name: "Projects" });
    const names = within(group).getAllByRole("link").map((a) => a.textContent);
    expect(names).toEqual(["alpha", "beta"]);
  });

  it("expanding a project reveals the icon'd sections with correct hrefs", async () => {
    render(
      <ProjectsNavTree
        defaultExpanded
        loadProjects={async () => jsonResponse(200, { projects: [twoProjects[1]] })}
      />,
    );
    await waitFor(() => screen.getByTestId("project-toggle-alpha"));
    fireEvent.click(screen.getByTestId("project-toggle-alpha"));
    const sections = screen.getByRole("group", { name: "alpha sections" });
    const links = within(sections).getAllByRole("link");
    expect(links.map((a) => a.textContent)).toEqual([
      "Overview",
      "Issues",
      "Runs",
      "Discussion",
      "File Explorer",
      "GitHub",
      "Settings",
    ]);
    // "squad-a/alpha" is encoded exactly once → "squad-a%2Falpha". Overview (id "landing") is the
    // bare project root (the workspace default), not a /landing sub-path (ISI-3957 AC2).
    expect(links[0]).toHaveAttribute("href", "/projects/squad-a%2Falpha");
    expect(links[1]).toHaveAttribute("href", "/projects/squad-a%2Falpha/issues");
    expect(links[4]).toHaveAttribute("href", "/projects/squad-a%2Falpha/files");
    expect(links[5]).toHaveAttribute("href", "/projects/squad-a%2Falpha/github");
    expect(links[6]).toHaveAttribute("href", "/projects/squad-a%2Falpha/settings");
  });

  it("a deep link auto-expands the active project and highlights the active section", async () => {
    render(
      <ProjectsNavTree
        pathname="/projects/squad-a%2Falpha/runs"
        loadProjects={async () => jsonResponse(200, { projects: [twoProjects[1]] })}
      />,
    );
    // Top-level opened (active project present) → sections visible without any click.
    const sections = await screen.findByRole("group", { name: "alpha sections" });
    const runs = within(sections).getByRole("link", { name: /Runs/ });
    expect(runs).toHaveAttribute("aria-current", "page");
    const issues = within(sections).getByRole("link", { name: /Issues/ });
    expect(issues).not.toHaveAttribute("aria-current");
  });

  it("empty list shows a neutral leaf", async () => {
    render(<ProjectsNavTree defaultExpanded loadProjects={async () => jsonResponse(200, { projects: [] })} />);
    await waitFor(() => screen.getByTestId("projects-tree-empty"));
  });

  it("a load failure degrades to an error with a working retry", async () => {
    const loadProjects = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(500, {}))
      .mockResolvedValueOnce(jsonResponse(200, { projects: twoProjects }));
    render(<ProjectsNavTree defaultExpanded loadProjects={loadProjects} />);
    await waitFor(() => screen.getByTestId("projects-tree-error"));
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => screen.getByTestId("project-toggle-alpha"));
    expect(loadProjects).toHaveBeenCalledTimes(2);
  });
});

// ── Live-run presence badge on the discussion-room entry (ISI-5526 / parent ISI-5520) ──────────
// The signal is GET /api/squad/overview (the ONLY live-runs source); a run is live when it is
// running OR queued. Count is per-project, rendered on that project's Discussion leaf: an always-on
// corner pulse dot (the collapsed-rail cue) + an "N live" pill (the expanded-rail cue). Idle → nil.

describe("<ProjectsNavTree> live-run badge — ISI-5526", () => {
  // namespace/name composites match the /api/projects id the nav keys on (squad-a/alpha, squad-b/beta).
  const overview = {
    projects: [
      {
        name: "alpha",
        namespace: "squad-a",
        phaseCounts: {},
        runs: [
          { name: "r1", phase: "Running" },
          { name: "r2", phase: "Queued" }, // queued counts as live
          { name: "r3", phase: "Succeeded" }, // terminal → not live
        ],
      },
      {
        name: "beta",
        namespace: "squad-b",
        phaseCounts: {},
        runs: [{ name: "r4", phase: "Failed" }], // all terminal → idle
      },
    ],
  };

  async function renderExpandedProjectSections(loadOverview: () => Promise<Response>) {
    render(
      <ProjectsNavTree
        defaultExpanded
        loadProjects={async () => jsonResponse(200, { projects: twoProjects })}
        loadOverview={loadOverview}
      />,
    );
    await waitFor(() => screen.getByTestId("project-toggle-alpha"));
    fireEvent.click(screen.getByTestId("project-toggle-alpha"));
    return screen.getByRole("group", { name: "alpha sections" });
  }

  it("renders the pulse dot + 'N live' pill on a room with live runs, counting queued", async () => {
    const sections = await renderExpandedProjectSections(async () => jsonResponse(200, overview));
    // Count folds running + queued (2), excludes the succeeded run.
    const pill = await within(sections).findByTestId("nav-live-badge-alpha");
    expect(pill).toHaveTextContent("2 live");
    // The corner dot — the collapsed-rail cue — renders alongside the expanded pill.
    expect(within(sections).getByTestId("nav-live-dot-alpha")).toBeInTheDocument();
  });

  it("folds the count into the Discussion link's accessible name ('<room>, N live')", async () => {
    const sections = await renderExpandedProjectSections(async () => jsonResponse(200, overview));
    await within(sections).findByTestId("nav-live-badge-alpha");
    expect(
      within(sections).getByRole("link", { name: "alpha Discussion, 2 live" }),
    ).toBeInTheDocument();
  });

  it("renders nothing on an idle room, and never on non-discussion sections", async () => {
    const sections = await renderExpandedProjectSections(async () => jsonResponse(200, overview));
    await within(sections).findByTestId("nav-live-badge-alpha");
    // beta has only a terminal run → no badge anywhere for it.
    expect(within(sections).queryByTestId("nav-live-badge-beta")).toBeNull();
    expect(within(sections).queryByTestId("nav-live-dot-beta")).toBeNull();
    // Even alpha's live count lands ONLY on Discussion, not Runs/Issues/etc.
    const discussion = within(sections).getByRole("link", { name: /^alpha Discussion/ });
    expect(within(discussion).getByTestId("nav-live-dot-alpha")).toBeInTheDocument();
    for (const name of ["Overview", "Issues", "Runs", "File Explorer", "GitHub"]) {
      const link = within(sections).getByRole("link", { name });
      expect(within(link).queryByTestId("nav-live-dot-alpha")).toBeNull();
    }
  });

  it("stays idle (no badge) when the overview read fails — cosmetic only", async () => {
    const sections = await renderExpandedProjectSections(async () => jsonResponse(500, {}));
    // Give the swallowed rejection a tick; the Discussion leaf must render unbadged.
    await within(sections).findByRole("link", { name: "Discussion" });
    expect(within(sections).queryByTestId("nav-live-badge-alpha")).toBeNull();
  });
});
