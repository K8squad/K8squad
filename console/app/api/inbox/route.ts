// app/api/inbox/route.ts — BFF proxy for the Inbox aggregate (ISI-5535, E1 of ISI-5531, ADR-0026).
//
// GET-ONLY. Proxies the Go apiserver's GET /api/squad/inbox. The AuthorContext is resolved
// server-side from the ksquad_session cookie (§13 BFF choke point) so tenancy is transparent:
// this route forwards the session identity and surfaces the apiserver response verbatim.
//
// POST /api/inbox/seen for mark-read is a sibling route (see ./seen/route.ts).

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/squad/inbox");
}
