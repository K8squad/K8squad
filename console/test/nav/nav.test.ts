// test/nav/nav.test.ts — role-based adaptive nav (story 8.16, ISI-2911).
//
// The RBAC seam is `visibleNav`: an admin-only node (Users & Roles) must be ABSENT from the tree for
// a non-admin at every breakpoint (rail, mobile bottom nav, drawer) — hidden = removed from the DOM,
// never display:none-as-authz. These pure-function units pin that behaviour plus the breadcrumb label
// for the new /users route.

import { describe, it, expect } from "vitest";
import {
  breadcrumbFor,
  mobileNav,
  navTree,
  visibleNav,
  type AccessLevel,
  type NavNode,
} from "@/lib/nav";

/** Top-level node ids in access-pruned order. */
function ids(access: AccessLevel): string[] {
  return visibleNav(navTree(), access).map((n) => n.id);
}

/** Every node id (top-level + section children) after RBAC pruning — the seam is recursive. */
function allIds(access: AccessLevel): string[] {
  const out: string[] = [];
  const walk = (nodes: NavNode[]) => {
    for (const n of nodes) {
      out.push(n.id);
      if (n.children) walk(n.children);
    }
  };
  walk(visibleNav(navTree(), access));
  return out;
}

describe("navTree — item-set + order match the ISI-3641 mock (ISI-3725)", () => {
  it("has the mock's top-level rail order, with SETTINGS as a section (not a link)", () => {
    expect(ids("user")).toEqual([
      "overview",
      "compose",
      "teams",
      "projects",
      "agents",
      "runs",
      "settings",
    ]);
    const settings = navTree().find((n) => n.id === "settings");
    expect(settings?.section).toBe(true);
    expect(settings?.href).toBe(""); // a header never navigates
  });

  it("groups OTel · Credentials · Plugins · Users&Roles under SETTINGS", () => {
    const settings = navTree().find((n) => n.id === "settings");
    expect(settings?.children?.map((c) => c.id)).toEqual([
      "otel",
      "credentials",
      "plugins",
      "users",
    ]);
    const otel = settings?.children?.find((c) => c.id === "otel");
    expect(otel?.href).toBe("/settings/configuration"); // OTLP surface (ISI-3717 Track 2 target)
  });
});

describe("visibleNav — the 8.16 RBAC seam prunes by access level (now recursive into SETTINGS)", () => {
  it("hides the admin-only Users & Roles node (nested under SETTINGS) from a plain user", () => {
    expect(allIds("user")).not.toContain("users");
  });

  it("shows Users & Roles to an admin", () => {
    expect(allIds("admin")).toContain("users");
  });

  it("keeps every non-admin node visible to both roles (only 'users' differs)", () => {
    const userIds = allIds("user");
    const adminIds = allIds("admin");
    // Admin sees a strict superset; the only difference is the admin-gated node.
    expect(adminIds).toEqual(expect.arrayContaining(userIds));
    const extra = adminIds.filter((id) => !userIds.includes(id));
    expect(extra).toEqual(["users"]);
  });
});

describe("mobileNav — role filtering happens BEFORE the 5-item budget (story 8.20)", () => {
  it("never surfaces the admin node in a non-admin's bottom nav or drawer", () => {
    const { bottom, drawer } = mobileNav("user");
    expect(bottom.map((n) => n.id)).not.toContain("users");
    expect(drawer.map((n) => n.id)).not.toContain("users");
  });

  it("includes the admin node in an admin's drawer", () => {
    const { drawer } = mobileNav("admin");
    expect(drawer.map((n) => n.id)).toContain("users");
  });
});

describe("breadcrumbFor — /users reads as a labelled trail", () => {
  it("labels the users route 'Users & Roles' as the current (unlinked) crumb", () => {
    const crumbs = breadcrumbFor("/users");
    expect(crumbs[0]).toEqual({ label: "Dashboard", href: "/" });
    const last = crumbs[crumbs.length - 1];
    expect(last).toEqual({ label: "Users & Roles", href: null });
  });
});
