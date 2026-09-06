// test/nav/onboarding-lock.test.ts — E1-S3 soft nav lock (AD-10, FR-1.4, ISI-3675).
//
// The lock is the SOFTER sibling of the 8.16 RBAC prune: a locked node stays IN the tree
// (visibleNav never removes it) and renders visible-but-disabled with a padlock + text
// equivalent. These units pin the derivation: which surfaces gate behind "a Team exists",
// child inheritance, unlock-on-Team, and that the lock survives every breakpoint
// re-expression (rail / bottom nav / drawer).

import { describe, it, expect } from "vitest";
import {
  mobileNav,
  navTree,
  visibleNav,
  withOnboardingLock,
  ONBOARDING_MILESTONE_HREF,
  type NavNode,
} from "@/lib/nav";

function byId(nodes: NavNode[]): Map<string, NavNode> {
  return new Map(nodes.map((n) => [n.id, n]));
}

describe("withOnboardingLock — FR-1.4 gating until a Team exists", () => {
  it("locks projects/agents but keeps Dashboard, Overview, Compose and Settings open", () => {
    const locked = byId(withOnboardingLock(navTree(), false));
    expect(locked.get("projects")?.locked).toBe(true);
    expect(locked.get("agents")?.locked).toBe(true);
    // Settings + children stay OPEN pre-Team (ISI-3870): an admin on an empty tenant must
    // reach OTel config / Credentials; the padlock is a journey affordance, not RBAC.
    expect(locked.get("settings")?.locked).toBeFalsy();
    const usersNode = locked.get("settings")?.children?.find((c) => c.id === "users");
    expect(usersNode?.locked).toBeFalsy();
    // Setup surfaces stay reachable: Compose authors milestone ① (the Team CR), Overview
    // carries the Launchpad (E1-S2), Dashboard is the landing root.
    expect(locked.get("dashboard")?.locked).toBeFalsy();
    expect(locked.get("overview")?.locked).toBeFalsy();
    expect(locked.get("compose")?.locked).toBeFalsy();
  });

  it("keeps a gated section's children open when the section is open (settings children)", () => {
    const locked = byId(withOnboardingLock(navTree(), false));
    for (const child of locked.get("settings")?.children ?? []) {
      expect(child.locked).toBeFalsy();
    }
  });

  it("returns the tree untouched once a Team exists (AC4 — everything unlocks)", () => {
    const tree = navTree();
    expect(withOnboardingLock(tree, true)).toBe(tree);
    expect(byId(withOnboardingLock(tree, true)).get("agents")?.locked).toBeFalsy();
  });

  it("never mutates the canonical tree", () => {
    const tree = navTree();
    withOnboardingLock(tree, false);
    expect(byId(tree).get("agents")?.locked).toBeFalsy();
  });
});

describe("lock vs prune — locked nodes stay VISIBLE at every breakpoint", () => {
  it("visibleNav prunes by access but never by lock", () => {
    const lockedTree = withOnboardingLock(navTree(), false);
    const ids = visibleNav(lockedTree, "user").map((n) => n.id);
    // 'agents' is locked yet still present; 'users' is admin-only AND locked — pruned by
    // access, the only node a plain user never sees.
    expect(ids).toContain("agents");
    expect(ids).not.toContain("users");
  });

  it("the lock survives the mobile bottom-nav budget and the drawer flattening", () => {
    const lockedTree = withOnboardingLock(navTree(), false);
    const { bottom, drawer } = mobileNav("user", lockedTree);
    const agentsInBottom = bottom.find((n) => n.id === "agents");
    const agentsInDrawer = drawer.find((n) => n.id === "agents");
    expect(agentsInBottom?.locked).toBe(true);
    expect(agentsInDrawer?.locked).toBe(true);
  });

  it("carries the optional badge field through the derivation (AD-10 model)", () => {
    const tree: NavNode[] = [{ id: "overview", label: "Overview", href: "/overview", badge: "2/4" }];
    const out = visibleNav(withOnboardingLock(tree, false), "user");
    expect(out[0].badge).toBe("2/4");
  });
});

describe("ONBOARDING_MILESTONE_HREF — the FR-1.3 routing table", () => {
  it("covers all four journey milestones", () => {
    for (const id of ["team", "agents", "models", "project"]) {
      expect(ONBOARDING_MILESTONE_HREF[id]).toMatch(/^\//);
    }
  });
});
