// app/api/squad/github/route.ts — BFF proxy for the fleet-wide GitHub summary
// (ISI-5705 WS-1b; GET /api/squad/github, githubsummary.go). The fleet recompose
// (ISI-5708 WS-3) consumes this for the GitHub summary strip + the authoritative
// per-project "Open PRs" count. GET-ONLY; the apiserver aggregates the synced scm
// mirror server-side (no outbound GitHub call) under the §13 BFF authz choke
// point, and this route surfaces its response VERBATIM (200 | 401 | 501 when the
// mirror read model is not wired). No mutating verb is routed (→ 405).

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(req: NextRequest): Promise<Response> {
  return proxyJson(req, "/api/squad/github");
}
