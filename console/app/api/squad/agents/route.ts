// app/api/squad/agents/route.ts — BFF squad-agents proxy (ISI-3964, Phase-2 of ISI-3941).
//
// GET-ONLY. Proxies the apiserver's fleet-aware Agents list (GET /api/squad/agents,
// fleetlist.go FleetAgentList). Scoping is AUTHORITATIVE server-side (admin ⇒ every squad's
// Agents, ADR-0010; tenant ⇒ their own Team namespace). Forwards the caller's session identity and
// surfaces the response VERBATIM (401 / 404 / 501). No mutating verb is routed here → 405.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/squad/agents");
}
