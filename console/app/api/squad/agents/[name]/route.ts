// app/api/squad/agents/[name]/route.ts — BFF squad-agent-detail proxy (ADR-0016 / ISI-4007).
//
// GET-ONLY. Proxies the apiserver's authoring-spec Agent detail (GET /api/squad/agents/{name},
// fleetlist.go AgentDetail) — the compose write wire minus the write-only `project` scope field,
// used to HYDRATE the compose EDIT form so a PUT does not blow the spec away (ISI-3985). Scoping is
// AUTHORITATIVE server-side: a tenant reads only their own squad by name; an admin (no home
// namespace) names the target squad via ?team={uid} (the ADR-0016 D2 selector, forwarded here). A
// foreign/absent name (or an admin who names no/foreign squad) is existence-hiding (404, never
// re-mapped to 403). Forwards the caller's session identity + ?team= and surfaces the response
// VERBATIM. No mutating verb is routed → POST/PUT/PATCH/DELETE = 405.

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
  return proxyJson(req, `/api/squad/agents/${name}${qs}`);
}
