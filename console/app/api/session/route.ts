// app/api/session/route.ts — BFF for the caller's SESSION lifecycle (ISI-3522) + role summary.
//
//   GET    → /auth/me     : the caller's role summary (story 8.14b UI RBAC gate; UX mirror only —
//                           the client FAILS CLOSED to viewer on any non-200/error).
//   POST   → /auth/login  : sign in. Relays {username,password} to the apiserver login route and
//                           relays its Set-Cookie (HttpOnly `ksquad_session`) back to the browser.
//   DELETE → /auth/logout : sign out. Relays the cookie-clearing Set-Cookie back.
//
// The Go apiserver stays the ONE authz choke point (§13 / ADR-013): it is the sole credential
// verifier and session-cookie issuer. The BFF adds NO second authz path — it forwards identity and
// surfaces the apiserver's status VERBATIM (401 invalid creds, 429 rate-limited, 200 success).

import type { NextRequest } from "next/server";
import { proxyAuth, proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/auth/me");
}

export async function POST(req: NextRequest): Promise<Response> {
  return proxyAuth(req, "/auth/login", "POST");
}

export async function DELETE(req: NextRequest): Promise<Response> {
  return proxyAuth(req, "/auth/logout", "DELETE");
}
