// app/api/runs/[runId]/events/route.ts — ISI-7484 durable Activity-history proxy.
//
// Routes the console's run-event hydration read to the apiserver's GET
// /api/runs/{runId}/events endpoint (the coord.outbox run-entity tail — the
// same journal the SSE projector live-tails and Last-Event-ID replays). Same
// proxyJson pattern as the run-detail proxy: forwards session identity, keeps
// RBAC upstream, no second authz path. useRunStream hydrates its initial
// Activity rows from THIS route on mount, then hands off to the live SSE tail;
// overlap dedupes by the shared outbox row id.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

// Never statically cache; history is dynamic.
export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ runId: string }> },
): Promise<Response> {
  const runId = encodeURIComponent((await params).runId);
  // Forward pagination query (afterId/limit) verbatim so a deep history can page.
  const search = req.nextUrl.search;
  return proxyJson(req, `/api/runs/${runId}/events${search}`);
}

