// middleware.ts — Next.js edge middleware: the Epic 15.5 route-protection gate (ISI-2921).
//
// This is a ROUTING gate, not an authorization layer (see lib/authGate.ts). It keeps an anonymous
// browser out of the authenticated app shell by redirecting a session-less navigation to /login; it
// NEVER decodes or role-checks the cookie. The Go apiserver remains the ONE authz choke point (§13 /
// ADR-013) — a present-but-invalid cookie passes here and is rejected upstream, fail-closed, with
// existence-hiding intact.

import { NextResponse } from "next/server";
import type { NextRequest } from "next/server";

import { gateDecision } from "@/lib/authGate";

/** Session cookie name (mirrors lib/bff.ts sessionCookieName; middleware cannot import "server-only"). */
function sessionCookieName(): string {
  return process.env.KSQUAD_SESSION_COOKIE ?? "ksquad_session";
}

export function middleware(req: NextRequest): NextResponse {
  const { pathname, search } = req.nextUrl;
  const cookie = req.cookies.get(sessionCookieName());
  const hasSession = Boolean(cookie?.value);

  const decision = gateDecision(pathname, search, hasSession);
  if (decision.action === "redirect" && decision.location) {
    return NextResponse.redirect(new URL(decision.location, req.url));
  }
  return NextResponse.next();
}

// Run on all navigations except Next internals and static files. The finer public-path logic
// (/login, /api/*, assets) lives in gateDecision so it is unit-testable; this matcher is only a
// coarse performance filter that skips the framework's own asset pipeline.
export const config = {
  matcher: ["/((?!_next/static|_next/image|favicon.ico).*)"],
};
