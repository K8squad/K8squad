// app/api/squad/teams/[uid]/route.ts — BFF squad-team-detail proxy (ISI-3964, Phase-2 of ISI-3941).
//
// GET-ONLY. Proxies the apiserver's fleet-aware Team detail (GET /api/squad/teams/{uid},
// fleetlist.go TeamDetail) — the squad header projection (counts + declared Agent/Project refs) a
// fleet browser reads without a second round-trip. Scoping is AUTHORITATIVE server-side: an admin
// may read ANY Team by object UID; a tenant only their own. A foreign/absent UID is existence-hiding
// (404, never re-mapped to 403), so a tenant cannot probe another squad's existence. This route
// forwards the caller's session identity and surfaces the response VERBATIM. No mutating verb is
// routed here → POST/PUT/PATCH/DELETE = 405.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ uid: string }> },
): Promise<Response> {
  const uid = encodeURIComponent((await params).uid);
  return proxyJson(req, `/api/squad/teams/${uid}`);
}
