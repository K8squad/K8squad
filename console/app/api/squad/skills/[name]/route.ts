// app/api/squad/skills/[name]/route.ts — BFF squad-skill-detail proxy (ISI-3961 / ADR-0016).
//
// GET-ONLY. Proxies the apiserver's single-skill VIEW (GET /api/squad/skills/{name},
// fleetlist.go SkillView) — the capability envelope + provenance the Skills surface renders, now
// also carrying the inline body (ISI-4007) so the compose EDIT form can HYDRATE and round-trip an
// inline skill. Scoping is AUTHORITATIVE server-side (tenant ⇒ own squad by name; admin ⇒ target
// squad via the optional ?team={uid} selector, forwarded here); a foreign or absent name is
// existence-hiding (404, never 403). Forwards session identity + ?team= and surfaces the response
// VERBATIM. No mutating verb is routed → 405.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ name: string }> },
): Promise<Response> {
  const name = encodeURIComponent((await params).name);
  const team = req.nextUrl.searchParams.get("team");
  const qs = team ? `?team=${encodeURIComponent(team)}` : "";
  return proxyJson(req, `/api/squad/skills/${name}${qs}`);
}
