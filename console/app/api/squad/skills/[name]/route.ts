// app/api/squad/skills/[name]/route.ts — BFF squad-skill-detail proxy (ISI-3962 S2, AC1 view).
//
// GET-ONLY. Proxies the apiserver's fleet-aware single-Skill detail (GET /api/squad/skills/{name},
// fleetlist.go squadSkillDetail → SkillView) — the capability envelope (source provenance,
// mcpToolRefs, permissions, requires.toolchains/sidecars, owning Team) the Skills surface renders
// when a row is opened. The inline skill body is NEVER projected server-side. Scoping is
// AUTHORITATIVE server-side: an admin may read ANY squad's skill by name (deterministic ns match on
// collision); a tenant only their own Team's. A foreign/absent name is existence-hiding (404, never
// re-mapped to 403), so a tenant cannot probe another squad. A cluster-less dev apiserver answers its
// documented 501. This route forwards the caller's session identity and surfaces the response
// VERBATIM. No mutating verb is routed here → POST/PUT/PATCH/DELETE = 405.
//
// Path deviation (matches S1/ISI-3961): the read namespace is /api/squad/skills/{name} (mirrors
// /api/squad/teams/{uid}), NOT /api/skills/{name} — the latter is the Compose WRITE (PUT) route.

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
  return proxyJson(req, `/api/squad/skills/${name}`);
}
