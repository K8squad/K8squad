// app/api/runs/[runId]/logs/[tab]/route.ts — BFF proxy for a Run's tabbed logs (story 8.11).
// GET-ONLY.
//
// Five tabs — task | tool | llm | build | error — projected from `run_events` streamed by the shim
// over A2A (§7.1/§5.2). The `build` tab is a legibility index that DEEP-LINKS to the build browser
// (8.7) — the file bytes themselves still flow through the dedicated build route (existence-hiding
// intact). The apiserver owns the AUTHORITATIVE authZ gate (§12.3); this route forwards the
// caller's session identity and surfaces the response VERBATIM — a deny (or unknown Run) is
// existence-hiding (404, never re-mapped to 403). An unknown tab is a 404 too, indistinguishable
// from a denied/missing Run. Read surface (R6): no mutating verb → 405.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

const TABS = new Set(["task", "tool", "llm", "build", "error"]);

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ runId: string; tab: string }> },
): Promise<Response> {
  const { runId: rawRunId, tab } = await params;
  // Unknown tab → 404 (indistinguishable from a denied/missing Run — existence-hiding).
  if (!TABS.has(tab)) return new Response(null, { status: 404 });
  const runId = encodeURIComponent(rawRunId);
  const search = req.nextUrl.search;
  return proxyJson(req, `/api/runs/${runId}/logs/${tab}${search}`);
}
