// test/nav/navTree.teams-marker.test.ts — AC6: navTree() stays pure/static; the ONLY delta from
// the canonical tree is the additive dynamicChildren marker on the Teams node (ISI-4001). Every
// other node — its href, scope, order, and the Settings section + its children — is untouched.

import { describe, it, expect } from "vitest";
import { navTree, type NavNode } from "@/lib/nav";

describe("navTree() — Teams dynamic-children marker (ISI-4001, AC6)", () => {
  it("stamps dynamicChildren:'teams' on the Teams node and keeps its /teams link", () => {
    const teams = navTree().find((n) => n.id === "teams") as NavNode;
    expect(teams).toBeDefined();
    expect(teams.dynamicChildren).toBe("teams");
    expect(teams.href).toBe("/teams");
    expect(teams.scope).toBe("global");
    // The marker is additive — it does NOT introduce a static children accordion.
    expect(teams.children).toBeUndefined();
  });

  it("leaves every other top-level node byte-for-byte unchanged", () => {
    const tree = navTree();
    // Order + ids unchanged.
    expect(tree.map((n) => n.id)).toEqual([
      "overview",
      "compose",
      "teams",
      "projects",
      "agents",
      "runs",
      "settings",
    ]);
    // Teams and Projects (ISI-4090) are the two dynamic sub-trees; no other node carries a marker.
    expect(tree.find((n) => n.id === "projects")?.dynamicChildren).toBe("projects");
    for (const n of tree) {
      if (n.id !== "teams" && n.id !== "projects") {
        expect(n.dynamicChildren).toBeUndefined();
      }
    }
    // Settings stays a section header with its four children intact.
    const settings = tree.find((n) => n.id === "settings") as NavNode;
    expect(settings.section).toBe(true);
    expect((settings.children ?? []).map((c) => c.id)).toEqual([
      "otel",
      "credentials",
      "plugins",
      "users",
    ]);
  });

  it("is pure — repeated calls return equal trees", () => {
    expect(navTree()).toEqual(navTree());
  });
});
