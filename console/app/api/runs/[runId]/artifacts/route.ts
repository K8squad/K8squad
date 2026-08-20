// app/api/runs/[runId]/artifacts/route.ts — BFF artifact-browser list proxy (story 8.3, ISI-2900).
//
// GET-ONLY. Proxies the Go apiserver's artifact listing for one Run (the coordination
// record's coord.artifact rows plus the parsed structured handoff). The AUTHORITATIVE
// per-principal + Team-scope gate (NFR-SEC5, the 8.7d rule) lives in the apiserver; this
// BFF route forwards the caller's session identity and surfaces the apiserver's response
// VERBATIM. Critically: a 404 stays a 404 (existence-hiding — deny ≡ not-found), never
// re-mapped to 403. No mutating verb is routed here (structurally absent → 405).

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ runId: string }> },
): Promise<Response> {
  const { runId } = await params;
  return proxyJson(req, `/api/runs/${encodeURIComponent(runId)}/artifacts`);
}
