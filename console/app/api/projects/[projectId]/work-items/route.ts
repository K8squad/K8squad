// app/api/projects/[projectId]/work-items/route.ts — BFF proxy for the Project
// Tickets read (stories 8.14b–d / 8.17). GET-ONLY.
//
// The browser talks ONLY to this Next.js server; the work-item list (and the
// `?parentId=` lazy-load for the sub-ticket tree) is proxied to the Go apiserver's
// work-items read-model, which owns the AUTHORITATIVE deny-by-default authZ gate
// (§12.3 / ADR-013) and the tenancy-scoped query predicates (§12.1). The BFF
// forwards the caller's session identity and the query string VERBATIM and
// surfaces the apiserver's response VERBATIM — a deny is existence-hiding (404
// stays 404, never re-mapped), and a documented 501 (read model not yet hosted,
// ISI-2909) stays 501 so the UI can render its honest "not available yet" state.
// No mutating verb is routed here (POST/PUT/PATCH/DELETE → 405).

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  const projectId = encodeURIComponent((await params).projectId);
  // Forward the server-side filter/sort/parentId predicates unchanged (8.14d AC1).
  const search = req.nextUrl.search;
  return proxyJson(req, `/api/projects/${projectId}/work-items${search}`);
}
