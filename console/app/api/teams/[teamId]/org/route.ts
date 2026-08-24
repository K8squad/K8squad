// app/api/teams/[teamId]/org/route.ts — BFF proxy for a Team's org diagram (story 8.10). GET-ONLY.
//
// Proxies the apiserver read-model that projects the Team → Agent → Role hierarchy from the
// `Team`/`Agent`/`Role` CRDs (§5.1, read-only — no new backend). The apiserver owns the
// AUTHORITATIVE deny-by-default authZ gate (§12.3 / ADR-013); this route forwards the caller's
// session identity and surfaces the response VERBATIM — a deny is existence-hiding (404, never
// re-mapped to 403), so a Team-B caller cannot tell a Team-A org from a missing one. No mutating
// verb is routed here (compose/edit stays 8.5, R6 scope guard) → POST/PUT/PATCH/DELETE = 405.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ teamId: string }> },
): Promise<Response> {
  const teamId = encodeURIComponent((await params).teamId);
  return proxyJson(req, `/api/teams/${teamId}/org`);
}
