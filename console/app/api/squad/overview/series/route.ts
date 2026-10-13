// app/api/squad/overview/series/route.ts — BFF proxy for the fleet overview
// time-series read model (ISI-5706 WS-1c; GET /api/squad/overview/series,
// squadseries.go). The fleet recompose (ISI-5708 WS-3) consumes this for the
// time-series row (tickets-by-status stacked area + in-window runs-by-status).
// GET-ONLY. The apiserver owns the authoritative Team scoping (admin ⇒ fleet,
// tenant ⇒ fenced) and surfaces per-tile degradation inline; this route forwards
// the caller's ?window / ?from / ?to query VERBATIM (proxyJson otherwise drops
// the query string) and relays the apiserver response unchanged.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  // proxyJson forwards only the path, so carry the trailing query (?window=30d
  // or ?from=&to=) across to the apiserver explicitly.
  return proxyJson(req, "/api/squad/overview/series" + (req.nextUrl.search || ""));
}
