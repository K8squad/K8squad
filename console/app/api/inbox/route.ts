// app/api/inbox/route.ts — BFF proxy for the Inbox aggregate (ISI-5535, E1 of ISI-5531, ADR-0026).
//
// GET-ONLY. Proxies the Go apiserver's GET /api/squad/inbox. The AuthorContext is resolved
// server-side from the ksquad_session cookie (§13 BFF choke point) so tenancy is transparent:
// this route forwards the session identity and surfaces the apiserver response verbatim.
//
// POST /api/inbox/seen for mark-read is the sibling route in ./seen/route.ts.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  // Forward the ISI-5537 E3 "Mine" scope narrowing (?scope=mine). Only this one allow-listed value
  // is relayed — never the raw query string — so the BFF can't be used to smuggle arbitrary upstream
  // params. Absent/any-other value proxies the default (team-fenced; admin → fleet).
  const scope = req.nextUrl.searchParams.get("scope");
  const upstream =
    scope === "mine" ? "/api/squad/inbox?scope=mine" : "/api/squad/inbox";
  return proxyJson(req, upstream);
}
