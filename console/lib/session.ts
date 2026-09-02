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

/** The nav shell's view of the signed-in caller: coarse access level + display username (ISI-3570). */
export interface Viewer {
  access: AccessLevel;
  /** The signed-in username to show in the account/sign-out footer; null when identity is unresolved. */
  username: string | null;
}

/**
 * viewer resolves the caller's coarse nav access level AND display username from the session cookie
 * by asking the apiserver's /auth/me (a single upstream call — the sign-out footer needs the name,
 * the nav needs the role). Returns "admin" only for globalRole==="admin"; everything else (including
 * any failure) is "user" — deny-by-default for the nav surface. On any error / missing cookie / non-200
 * it FAILS CLOSED to { access: "user", username: null }: an absent /auth/me never widens the visible
 * surface, and the footer degrades to a generic "Account" label (still fully functional — sign-out
 * does not depend on the name).
 */
export async function viewer(): Promise<Viewer> {
  const closed: Viewer = { access: "user", username: null };
  try {
    const store = await cookies();
    const token = store.get(sessionCookieName())?.value;
    if (!token) return closed;
    const res = await fetch(`${apiserverBaseUrl()}/auth/me`, {
      headers: {
        cookie: `${sessionCookieName()}=${token}`,
        accept: "application/json",
      },
      cache: "no-store",
    });
    if (!res.ok) return closed;
    const me = (await res.json()) as { globalRole?: string; username?: string };
    return {
      access: me.globalRole === "admin" ? "admin" : "user",
      username: me.username ?? null,
    };
  } catch {
    return closed;
  }
}

/**
 * canCompose is the client-side UX gate for the compose entry points (ISI-3554 Story A "+ New Agent"
 * / "Edit"). It answers only what the console's session source (/auth/me → {@link Viewer}) can
 * honestly know: is there a signed-in caller?
 *
 * The server-side write authority (composecrd.go, §13 choke point) gates on PER-PROJECT contributor
 * membership — a role that /auth/me does NOT expose and that is existence-hidden (a foreign project
 * is 404, not 403). The browser therefore CANNOT know a non-admin's contributor tier, and hiding the
 * action from a legitimate contributor would mislead (the same reason the Compose nav node is left
 * ungated). So this gate mirrors exactly the one authz fact the client holds: an unauthenticated
 * caller (no resolved identity) can never write and is gated out; any signed-in caller is offered the
 * action, and the apiserver remains the authority — a viewer-tier caller who proceeds sees the
 * server's 403 verbatim on submit (no dead-end write, just an honest server verdict).
 */
export function canCompose(v: Viewer): boolean {
  return v.username != null;
}
