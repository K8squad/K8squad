// app/api/squad/skills/[name]/route.ts — BFF squad-skill-detail proxy (ISI-3962 S2, AC1 view;
// ISI-4002/ADR-0016 authoring reuse).
//
// GET-ONLY. Proxies the apiserver's fleet-aware single-Skill detail (GET /api/squad/skills/{name},
// fleetlist.go squadSkillDetail → SkillView) — the capability envelope (source provenance,
// mcpToolRefs, permissions, requires.toolchains/sidecars, owning Team) the Skills surface renders
// when a row is opened. ISI-4002 additionally consumes THIS route for Compose edit-form hydration:
// SkillView now carries the inline body, so `fromWire('skills', …)` reconstructs the SkillForm from
// it (no separate authoring route — skills reuse this one, unlike agents/roles/projects). Scoping is
// AUTHORITATIVE server-side: an admin may read ANY squad's skill by name (deterministic ns match on
// collision); a tenant only their own Team's. A foreign/absent name is existence-hiding (404, never
// re-mapped to 403), so a tenant cannot probe another squad. A cluster-less dev apiserver answers its
// documented 501. This route forwards the caller's session identity + the optional ?team= selector
// (harmless where the skill read ignores it) and surfaces the response VERBATIM. No mutating verb is
// routed here → POST/PUT/PATCH/DELETE = 405.
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
  const team = req.nextUrl.searchParams.get("team");
  const qs = team ? `?team=${encodeURIComponent(team)}` : "";
  return proxyJson(req, `/api/squad/skills/${name}${qs}`);
}
