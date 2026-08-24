// app/api/agents/[agentId]/route.ts — BFF proxy for an Agent's detail header (story 8.11). GET-ONLY.
//
// Proxies the apiserver read-model projecting a single `Agent` CRD (+ resolved `AgentRuntime`/`Role`
// + derived status, §5.1/§5.3). The apiserver owns the AUTHORITATIVE authZ gate (§12.3); this route
// forwards the caller's session identity and surfaces the response VERBATIM — a deny is
// existence-hiding (404, never re-mapped to 403). Read/legibility surface (R6): no mutating verb is
// routed here → POST/PUT/PATCH/DELETE = 405.

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
  return proxyJson(req, `/api/agents/${agentId}`);
}
