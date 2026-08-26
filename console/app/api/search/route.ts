// app/api/search/route.ts — BFF global-search proxy (story 8.19 / ISI-2912).
//
// GET-ONLY. Proxies the Go apiserver's RBAC-scoped search read model (the §13 choke point applies
// the real authz + per-Project scoping there). The query string (`q`, `limit`) is forwarded VERBATIM
// — the BFF parses nothing, so it can neither widen nor narrow the search — and the apiserver's
// response is relayed verbatim: a blank-q 400, a deny (401/403/404, existence-hiding, never
// re-mapped), or a 5xx all surface unchanged. No mutating verb is routed here (POST/PUT/PATCH/DELETE
// are structurally absent → 405).

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  const search = req.nextUrl.searchParams.toString();
  return proxyJson(req, "/api/search" + (search ? `?${search}` : ""));
}
