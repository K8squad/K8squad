// app/api/squad/agents/activity/route.ts — BFF squad agent-activity proxy (ISI-5704 WS-1a,
// consumed by the overview-UX redesign ISI-5697 / design ISI-5699).
//
// GET-ONLY. Proxies the apiserver's agent-activity read model (GET /api/squad/agents/activity,
// agentactivity.go AgentActivityList) — per agent its current-or-last run with a SERVER-classified
// last-thinking snippet, folding away the console's old N+1 GET /api/runs/{runId} fan-out. Scoping
// is AUTHORITATIVE server-side (admin ⇒ fleet-wide; tenant ⇒ their own Team). Response surfaced
// VERBATIM (401 / 404 / 501). No mutating verb is routed here → 405.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/squad/agents/activity");
}
