// app/api/squad/projects/route.ts — BFF squad-projects proxy (ISI-3943, follow-up to ISI-3941).
//
// GET-ONLY. Proxies the Go apiserver's fleet-aware Projects list (GET /api/squad/projects,
// overview.go squadProjects) — the surface the console Projects tab renders against. Like the
// squad-overview proxy, the AUTHORITATIVE scoping lives in the apiserver: the caller's session
// resolves to an AuthorContext that scopes the projection server-side (admin ⇒ fleet-wide,
// ADR-0010; tenant ⇒ their Team namespace) — this route forwards the caller's session identity and
// surfaces the apiserver's response VERBATIM (401 unauthenticated, 404 no-team, 501 read model not
// wired). No mutating verb is routed here (POST/PUT/PATCH/DELETE are structurally absent → 405).

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/squad/projects");
}
