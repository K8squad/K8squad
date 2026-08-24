// app/api/squad/overview/route.ts — BFF squad-overview proxy (story 8.1, ISI-2900).
//
// GET-ONLY. Proxies the Go apiserver's Team→Project→Run-status read model
// (GET /api/squad/overview, overview.go). The AUTHORITATIVE Team scoping lives in the
// apiserver: the caller's session resolves to an AuthorContext whose TeamID scopes the
// projection server-side (§7.3.3 tenancy root) — this BFF route forwards the caller's
// session identity and surfaces the apiserver's response VERBATIM (401 unauthenticated,
// 404 no-team, 501 read model not wired). No mutating verb is routed here (POST/PUT/
// PATCH/DELETE are structurally absent → 405).

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/squad/overview");
}
