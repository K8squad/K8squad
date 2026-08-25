// lib/session.ts — the SERVER-ONLY viewer-identity read the nav shell needs (story 8.16).
//
// Story 8.16 (role-based adaptive nav) turns the ConsoleShell's `access` prop from the hardcoded
// "user" default into the REAL viewer's global role, so an admin sees the admin-only nodes (Users &
// Roles, 8.15) and a plain user does not. The mapping is deliberately coarse: the nav's AccessLevel
// is global ("user" | "admin"), which is exactly what the apiserver's /auth/me returns as
// `globalRole`. Per-Project roles (viewer/contributor/maintainer) are a different, route-scoped axis
// enforced upstream by the 15.4 wall — they never drive the global nav tree.
//
// This is NOT an authorization boundary. It is a UX mirror: nodes are hidden for legibility, and the
// Go apiserver (§13 / ADR-013) stays the ONE authz choke point — a hand-crafted request to /users
// still hits requireAdmin upstream. So this resolver FAILS CLOSED to "user" on any error, missing
// cookie, or non-200: an absent /auth/me never widens the visible surface.

import "server-only";
import { cookies } from "next/headers";

import { apiserverBaseUrl, sessionCookieName } from "@/lib/bff";
import type { AccessLevel } from "@/lib/nav";

/**
 * viewerAccess resolves the caller's coarse nav access level from the session cookie by asking the
 * apiserver's /auth/me. Returns "admin" only for globalRole==="admin"; everything else (including
 * any failure) is "user" — deny-by-default for the nav surface.
 */
export async function viewerAccess(): Promise<AccessLevel> {
  try {
    const store = await cookies();
    const token = store.get(sessionCookieName())?.value;
    if (!token) return "user";
    const res = await fetch(`${apiserverBaseUrl()}/auth/me`, {
      headers: {
        cookie: `${sessionCookieName()}=${token}`,
        accept: "application/json",
      },
      cache: "no-store",
    });
    if (!res.ok) return "user";
    const me = (await res.json()) as { globalRole?: string };
    return me.globalRole === "admin" ? "admin" : "user";
  } catch {
    return "user";
  }
}
