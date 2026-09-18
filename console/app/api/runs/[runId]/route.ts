// app/api/runs/[runId]/route.ts — ISI-4571 run detail proxy.
//
// Routes the console's run detail page to the apiserver's GET /api/runs/{runId} endpoint.
// Reuses the proxyJson pattern to forward session identity and preserve RBAC scoping.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

// Never statically cache; run details are dynamic.
export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ runId: string }> },
): Promise<Response> {
  const runId = encodeURIComponent((await params).runId);
  return proxyJson(req, `/api/runs/${runId}`);
}