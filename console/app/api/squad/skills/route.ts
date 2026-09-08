// app/api/squad/skills/route.ts — BFF squad-skills proxy (ISI-3964, Phase-2 of ISI-3941).
//
// GET-ONLY. Proxies the apiserver's fleet-aware Skills list (GET /api/squad/skills,
// fleetlist.go FleetSkillList). Scoping is AUTHORITATIVE server-side (admin ⇒ every squad's
// Skills, ADR-0010; tenant ⇒ their own Team namespace). Forwards the caller's session identity and
// surfaces the response VERBATIM (401 / 404 / 501). No mutating verb is routed here → 405.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/squad/skills");
}
