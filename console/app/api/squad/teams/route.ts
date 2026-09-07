// app/api/squad/teams/route.ts — BFF squad-teams proxy (ISI-3964, Phase-2 of ISI-3941).
//
// GET-ONLY. Proxies the Go apiserver's fleet-aware Teams list (GET /api/squad/teams,
// fleetlist.go FleetTeamList) — the surface the console fleet team picker renders against. Like the
// squad-projects proxy, the AUTHORITATIVE scoping lives in the apiserver: the caller's session
// resolves to an AuthorContext that scopes the projection server-side (admin ⇒ fleet-wide over
// every squad, ADR-0010; tenant ⇒ their own Team only) — this route forwards the caller's session
// identity and surfaces the apiserver's response VERBATIM (401 unauthenticated, 404 no-team, 501
// read model not wired). No mutating verb is routed here (POST/PUT/PATCH/DELETE are structurally
// absent → 405); composing a Team stays the /api/compose write surface.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/squad/teams");
}
