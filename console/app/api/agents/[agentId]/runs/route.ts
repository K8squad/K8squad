// app/api/agents/[agentId]/runs/route.ts — BFF proxy for an Agent's Run history (story 8.11).
// GET-ONLY.
//
// Proxies the apiserver read-model listing an Agent's Runs (status / duration / token usage) from
// the `Run` CRDs (§8, read-only — no new backend). The apiserver owns the AUTHORITATIVE authZ gate
// (§12.3); this route forwards the caller's session identity and surfaces the response VERBATIM — a
// deny is existence-hiding (404, never re-mapped to 403). The ?limit/?offset query is forwarded
// verbatim for server-side pagination. Read surface (R6): no mutating verb → 405.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ agentId: string }> },
): Promise<Response> {
  const agentId = encodeURIComponent((await params).agentId);
  const search = req.nextUrl.search; // includes leading '?' or ''
  return proxyJson(req, `/api/agents/${agentId}/runs${search}`);
}
