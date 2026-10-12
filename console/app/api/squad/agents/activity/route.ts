// app/api/squad/agents/activity/route.ts — BFF proxy for the agent-activity read
// model (ISI-5704 WS-1a; GET /api/squad/agents/activity, agentactivity.go). The
// fleet recompose (ISI-5708 WS-3) consumes this for the current-runs agent-card
// grid. GET-ONLY: the apiserver owns the authoritative Team scoping server-side
// (admin ⇒ fleet-wide, tenant ⇒ fenced), and this route surfaces its response
// VERBATIM (200 | 401 | 404 no-team | 501 read model not wired). No mutating verb
// is routed (POST/PUT/PATCH/DELETE → 405).

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/squad/agents/activity");
}
