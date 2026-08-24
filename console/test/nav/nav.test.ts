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
} from "@/lib/nav";

function ids(access: AccessLevel): string[] {
  return visibleNav(navTree(), access).map((n) => n.id);
}

describe("visibleNav — the 8.16 RBAC seam prunes by access level", () => {
  it("hides the admin-only Users & Roles node from a plain user", () => {
    expect(ids("user")).not.toContain("users");
  });

  it("shows Users & Roles to an admin", () => {
    expect(ids("admin")).toContain("users");
  });

  it("keeps every non-admin node visible to both roles (only 'users' differs)", () => {
    const userIds = ids("user");
    const adminIds = ids("admin");
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
