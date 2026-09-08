// app/api/squad/projects/[name]/route.ts — BFF squad-project-detail proxy (ADR-0016 / ISI-4007).
//
// GET-ONLY. Proxies the apiserver's authoring-spec Project detail (GET /api/squad/projects/{name},
// fleetlist.go ProjectDetail) to HYDRATE the compose EDIT form. NOTE the sibling
// /api/squad/projects LIST is a DIFFERENT read model (squad-overview); this by-name detail rides the
// fleet reader. Scoping is AUTHORITATIVE server-side (tenant ⇒ own squad by name; admin ⇒ target
// squad via ?team={uid}, forwarded here); a foreign or absent name is existence-hiding (404, never
// 403). Forwards session identity + ?team= and surfaces the response VERBATIM. No mutating verb is
// routed → 405.

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
  return proxyJson(req, `/api/squad/projects/${name}${qs}`);
}
