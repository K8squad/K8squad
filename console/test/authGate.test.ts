import { describe, it, expect } from "vitest";

import { gateDecision, isPublicPath } from "@/lib/authGate";

// Epic 15.5 route-protection gate (ISI-2921). This is a ROUTING decision, not authz — the apiserver
// owns the real deny wall. These tests pin the gate's shape: anonymous → /login; session or public → next.

describe("isPublicPath", () => {
  it("treats /api/* as public (BFF route handlers own their own auth → JSON 401, not an HTML redirect)", () => {
    expect(isPublicPath("/api/projects/checkout/dashboard")).toBe(true);
    expect(isPublicPath("/api/runs/abc/stream")).toBe(true);
  });
  it("treats the login route and framework/static prefixes as public", () => {
    expect(isPublicPath("/login")).toBe(true);
    expect(isPublicPath("/login/callback")).toBe(true);
    expect(isPublicPath("/_next/static/chunk.js")).toBe(true);
    expect(isPublicPath("/favicon.ico")).toBe(true);
    expect(isPublicPath("/robots.txt")).toBe(true);
  });
  it("treats app routes as protected", () => {
    expect(isPublicPath("/")).toBe(false);
    expect(isPublicPath("/overview")).toBe(false);
    expect(isPublicPath("/projects/checkout")).toBe(false);
    // A path that merely contains "login" but is not under /login is protected.
    expect(isPublicPath("/projects/login-service")).toBe(false);
  });
});

describe("gateDecision", () => {
  it("lets an authenticated navigation through", () => {
    expect(gateDecision("/overview", "", true)).toEqual({ action: "next" });
  });
  it("lets an anonymous navigation to a public route through (no redirect loop at /login)", () => {
    expect(gateDecision("/login", "?next=%2Foverview", false)).toEqual({ action: "next" });
    expect(gateDecision("/api/runs/x", "", false)).toEqual({ action: "next" });
  });
  it("redirects an anonymous navigation to a protected route to /login, preserving the destination", () => {
    const r = gateDecision("/projects/checkout", "?tab=runs", false);
    expect(r.action).toBe("redirect");
    expect(r.location).toBe("/login?next=" + encodeURIComponent("/projects/checkout?tab=runs"));
  });
  it("preserves a bare protected path with no query", () => {
    const r = gateDecision("/overview", "", false);
    expect(r.location).toBe("/login?next=" + encodeURIComponent("/overview"));
  });
});
