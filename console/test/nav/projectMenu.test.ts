// test/nav/projectMenu.test.ts — the Project-Detail Workspace left-menu model (ISI-3957 S1).
// Covers AC1 (six entries, order), AC2 (Landing at the bare root, not a /landing sub-path),
// AC3 (active-id derived from the pathname), AC6 (legacy routes don't false-highlight).

import { describe, it, expect } from "vitest";
import { projectMenu, projectSubnav, projectMenuActiveId } from "@/lib/nav";

describe("projectMenu — AC1 entries in UX order", () => {
  it("returns Landing · Issues · Runs · Discussion · File Explorer · GitHub · Settings as project-scoped nodes", () => {
    const menu = projectMenu("webapp");
    expect(menu.map((n) => n.id)).toEqual([
      "landing",
      "issues",
      "runs",
      "discussion",
      "files",
      "github",
      "settings",
    ]);
    expect(menu.map((n) => n.label)).toEqual([
      "Landing",
      "Issues",
      "Runs",
      "Discussion",
      "File Explorer",
      "GitHub",
      "Settings",
    ]);
    expect(menu.every((n) => n.scope === "project")).toBe(true);
  });

  it("AC2 — Landing resolves to the bare project root; the rest map to /{section}", () => {
    const byId = Object.fromEntries(projectMenu("webapp").map((n) => [n.id, n.href]));
    expect(byId.landing).toBe("/projects/webapp");
    expect(byId.issues).toBe("/projects/webapp/issues");
    expect(byId.runs).toBe("/projects/webapp/runs");
    expect(byId.files).toBe("/projects/webapp/files");
    expect(byId.github).toBe("/projects/webapp/github");
    expect(byId.settings).toBe("/projects/webapp/settings");
  });

  it("encodes a namespaced project id exactly once (ISI-3982)", () => {
    const byId = Object.fromEntries(projectMenu("squad-alpha/webapp").map((n) => [n.id, n.href]));
    expect(byId.landing).toBe("/projects/squad-alpha%2Fwebapp");
    expect(byId.issues).toBe("/projects/squad-alpha%2Fwebapp/issues");
  });

  it("projectSubnav stays the lower-level projection behind projectMenu", () => {
    expect(projectSubnav("webapp").map((s) => s.id)).toEqual(
      projectMenu("webapp").map((n) => n.id),
    );
  });
});

describe("projectMenuActiveId — AC3 active-tab derives from the pathname (URL is the state)", () => {
  it("the bare project root is Landing", () => {
    expect(projectMenuActiveId("/projects/webapp")).toBe("landing");
    expect(projectMenuActiveId("/projects/webapp/")).toBe("landing");
    expect(projectMenuActiveId("/projects/squad-alpha%2Fwebapp")).toBe("landing");
  });

  it("a section segment lights that entry", () => {
    expect(projectMenuActiveId("/projects/webapp/issues")).toBe("issues");
    expect(projectMenuActiveId("/projects/webapp/runs")).toBe("runs");
    expect(projectMenuActiveId("/projects/webapp/discussion")).toBe("discussion");
    expect(projectMenuActiveId("/projects/webapp/files")).toBe("files");
    expect(projectMenuActiveId("/projects/webapp/github")).toBe("github");
    expect(projectMenuActiveId("/projects/webapp/settings")).toBe("settings");
  });

  it("a deeper path still lights the section, ignoring query/hash", () => {
    expect(projectMenuActiveId("/projects/webapp/issues/wi-123")).toBe("issues");
    expect(projectMenuActiveId("/projects/webapp/runs?view=grid#top")).toBe("runs");
  });

  it("AC6 — the legacy /tickets and /build routes highlight nothing (not in the menu)", () => {
    expect(projectMenuActiveId("/projects/webapp/tickets")).toBeNull();
    expect(projectMenuActiveId("/projects/webapp/build")).toBeNull();
  });

  it("a non-project path is null", () => {
    expect(projectMenuActiveId("/overview")).toBeNull();
    expect(projectMenuActiveId("/")).toBeNull();
  });
});
