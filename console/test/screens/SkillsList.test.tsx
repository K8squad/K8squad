import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import { SkillsList } from "@/components/SkillsList";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

// Fleet-wide list shape (ISI-3961 SkillListEntry): admin caller ⇒ every squad's skills. The list row
// carries only name/namespace/sourceType — the owning-Team deep link lives on the {name} detail.
const fleetList = {
  fleet: true,
  skills: [
    { name: "web-search", namespace: "squad-alpha", uid: "sk-1", sourceType: "inline" },
    { name: "deploy", namespace: "squad-beta", uid: "sk-2", sourceType: "git" },
  ],
};

// GET /api/squad/skills/{name} detail (SkillView): capability envelope + owning Team.
const detailView = {
  name: "deploy",
  namespace: "squad-beta",
  uid: "sk-2",
  teamUid: "uid-beta",
  teamName: "beta",
  sourceType: "git",
  repoRef: "https://git.example/skills",
  ref: "main",
  path: "deploy",
  mcpToolRefs: ["kubectl", "helm"],
  permissions: ["cluster:write"],
  toolchains: ["go"],
  sidecars: ["dind"],
};

/** Route the mock by URL so a list fetch and a detail fetch resolve independently. */
function stubRoutedFetch(
  listStatus: number,
  listBody: unknown,
  detailStatus = 200,
  detailBody: unknown = detailView,
) {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      const isDetail = /\/api\/squad\/skills\/[^/?]+/.test(url);
      const status = isDetail ? detailStatus : listStatus;
      const body = isDetail ? detailBody : listBody;
      return Promise.resolve({
        ok: status >= 200 && status < 300,
        status,
        json: () => Promise.resolve(body),
      });
    }),
  );
}

describe("<SkillsList> — ISI-3962 S2 Skills list/view surface", () => {
  it("fetches /api/squad/skills and renders one row per fleet skill (AC4 list)", async () => {
    stubRoutedFetch(200, fleetList);
    render(<SkillsList />);
    await waitFor(() => expect(screen.getByTestId("skills-ready")).toBeTruthy());
    expect(screen.getAllByTestId("skills-row").length).toBe(2);
    expect(screen.getByText("web-search")).toBeTruthy();
    expect(screen.getByText("deploy")).toBeTruthy();
  });

  it("expands a row to the single-skill view via GET /api/squad/skills/{name} (AC4 view)", async () => {
    stubRoutedFetch(200, fleetList);
    render(<SkillsList />);
    await waitFor(() => expect(screen.getByTestId("skills-ready")).toBeTruthy());

    // Open the "deploy" row (2nd toggle).
    fireEvent.click(screen.getAllByTestId("skills-row-toggle")[1]);
    await waitFor(() => expect(screen.getByTestId("skills-detail")).toBeTruthy());

    // Capability envelope surfaced.
    expect(screen.getByTestId("skills-detail-mcp").textContent).toContain("kubectl");
    expect(screen.getByTestId("skills-detail-perms").textContent).toContain("cluster:write");
    expect(screen.getByTestId("skills-detail-toolchains").textContent).toContain("go");
    expect(screen.getByTestId("skills-detail-sidecars").textContent).toContain("dind");
    // Git provenance rendered as repoRef@ref:path.
    expect(screen.getByTestId("skills-detail-repo").textContent).toBe(
      "https://git.example/skills@main:deploy",
    );
  });

  it("surfaces the owning-Team deep link on an admin's expanded skill (fleet, ISI-3943 AC2 idiom)", async () => {
    stubRoutedFetch(200, fleetList);
    render(<SkillsList />);
    await waitFor(() => expect(screen.getByTestId("skills-ready")).toBeTruthy());
    fireEvent.click(screen.getAllByTestId("skills-row-toggle")[1]);
    await waitFor(() => expect(screen.getByTestId("skills-detail-team-link")).toBeTruthy());
    const link = screen.getByTestId("skills-detail-team-link");
    expect(link.tagName).toBe("A");
    expect(link.getAttribute("href")).toBe("/agents?team=uid-beta");
  });

  it("does NOT surface a team deep link for a tenant (fleet:false)", async () => {
    const tenantList = {
      fleet: false,
      skills: [{ name: "web-search", namespace: "squad-alpha", sourceType: "inline" }],
    };
    const tenantDetail = { name: "web-search", namespace: "squad-alpha", sourceType: "inline" };
    stubRoutedFetch(200, tenantList, 200, tenantDetail);
    render(<SkillsList />);
    await waitFor(() => expect(screen.getByTestId("skills-ready")).toBeTruthy());
    fireEvent.click(screen.getAllByTestId("skills-row-toggle")[0]);
    await waitFor(() => expect(screen.getByTestId("skills-detail")).toBeTruthy());
    expect(screen.queryByTestId("skills-detail-team-link")).toBeNull();
  });

  it("renders the empty state when the wire sends skills: null (nil-slice contract)", async () => {
    stubRoutedFetch(200, { fleet: false, skills: null });
    const originalLocation = window.location;
    const fakeLocation = { href: "" };
    Object.defineProperty(window, "location", { value: fakeLocation, configurable: true });
    render(<SkillsList />);
    await waitFor(() => expect(screen.getByTestId("skills-empty")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Compose a skill" }));
    expect(fakeLocation.href).toBe("/compose?kind=skill");
    Object.defineProperty(window, "location", { value: originalLocation, configurable: true });
  });

  it("renders the honest terminal-state cards the BFF relays (classifyOverviewStatus contract)", async () => {
    stubRoutedFetch(401, null);
    render(<SkillsList />);
    await waitFor(() => expect(screen.getByTestId("skills-unauthenticated")).toBeTruthy());

    cleanup();
    stubRoutedFetch(501, null);
    render(<SkillsList />);
    await waitFor(() => expect(screen.getByTestId("skills-not-wired")).toBeTruthy());
  });

  it("shows a detail error card when the {name} fetch fails, without breaking the list", async () => {
    stubRoutedFetch(200, fleetList, 404, null);
    render(<SkillsList />);
    await waitFor(() => expect(screen.getByTestId("skills-ready")).toBeTruthy());
    fireEvent.click(screen.getAllByTestId("skills-row-toggle")[0]);
    await waitFor(() => expect(screen.getByTestId("skills-detail-error")).toBeTruthy());
    // The list itself is intact.
    expect(screen.getAllByTestId("skills-row").length).toBe(2);
  });
});
