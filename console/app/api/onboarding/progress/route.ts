// app/api/onboarding/progress/route.ts — BFF onboarding-progress proxy (E1-S1, ISI-3673, AD-2).
//
// GET-ONLY. Proxies the Go apiserver's derived 4-milestone onboarding projection
// (GET /api/onboarding/progress, internal/apiserver/onboarding.go). The AUTHORITATIVE Team
// scoping lives in the apiserver: the caller's ksquad_session cookie resolves to an
// AuthorContext whose TeamID scopes the projection server-side — this route forwards the
// session identity and surfaces the apiserver's response VERBATIM (401 unauthenticated, 501
// read model not wired). A first-run tenant with no Team CR gets the honest zero projection
// ({step:1, done:0, total:4, nextMilestone:"team"}), never an error. No mutating verb is routed
// here (POST/PUT/PATCH/DELETE are structurally absent → 405).

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/onboarding/progress");
}
