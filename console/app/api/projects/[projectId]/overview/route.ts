// app/api/projects/[projectId]/overview/route.ts — BFF proxy for the project-overview
// time-series read model (ISI-4509 S4 endpoint GET /api/projects/{projectId}/overview,
// client seam lib/overview/projectSeries.ts). GET-ONLY.
//
// The console requests `?window=30d` (or `?from=&to=`); the whole query string is forwarded
// VERBATIM. Proxied to the Go apiserver, which owns the AUTHORITATIVE deny-by-default authZ gate
// (§13 / ADR-013): the session identity is forwarded and the apiserver status surfaced unchanged —
// a cross-tenant or absent project is a 404 (existence-hiding, NEVER re-mapped to 403).
//
// Without this route Next.js had no handler for the segment and answered 404 even though the
// apiserver route exists — a BFF proxy gap (class of ISI-5164 roster-empty), fixed by ISI-5250.
// No mutating verb is routed here (POST/PUT/PATCH/DELETE → 405).

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";
import { encodeProjectId } from "@/lib/projectId";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  const projectId = encodeProjectId((await params).projectId);
  const search = req.nextUrl.search; // includes leading '?' or ''
  return proxyJson(req, `/api/projects/${projectId}/overview${search}`);
}
