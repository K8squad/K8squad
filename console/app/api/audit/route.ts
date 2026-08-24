// app/api/audit/route.ts — BFF audit-trail proxy (story 2.6 / ISI-2881).
//
// GET-ONLY. Proxies the Go apiserver's RBAC-scoped audit read model (the §13 choke point applies
// the real authz + admin/self scoping there). The filter query string is forwarded VERBATIM —
// the BFF parses nothing, so it can neither widen nor narrow a filter — and the apiserver's
// response (including its documented 501 when no reader is wired) is relayed verbatim. No
// mutating verb is routed here (POST/PUT/PATCH/DELETE are structurally absent → 405).

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  const search = req.nextUrl.searchParams.toString();
  return proxyJson(req, "/api/audit" + (search ? `?${search}` : ""));
}
