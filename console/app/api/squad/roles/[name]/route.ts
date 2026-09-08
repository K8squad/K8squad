// app/api/squad/roles/[name]/route.ts — BFF squad-role-detail proxy (ADR-0016 / ISI-4007).
//
// GET-ONLY. Proxies the apiserver's authoring-spec Role detail (GET /api/squad/roles/{name},
// fleetlist.go RoleDetail) to HYDRATE the compose EDIT form. Scoping is AUTHORITATIVE server-side
// (tenant ⇒ own squad by name; admin ⇒ target squad via ?team={uid}, forwarded here); a foreign or
// absent name is existence-hiding (404, never 403). Forwards session identity + ?team= and surfaces
// the response VERBATIM. No mutating verb is routed → 405.

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
  return proxyJson(req, `/api/squad/roles/${name}${qs}`);
}
