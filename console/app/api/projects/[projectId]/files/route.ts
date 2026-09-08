// S4c BFF route — Project File Explorer directory listing (ISI-3991, ADR-0012 §D2 / S4c §Tasks 1).
//
// GET /api/projects/[projectId]/files?path=<dir>&page=<n>
//   → proxies to Go apiserver GET /api/projects/{projectId}/files?path=&page=
//
// Pass-through only: no business logic, no status re-mapping.
// A 404 (non-member / unknown project) is relayed verbatim — existence-hiding (AC6/NFR-SEC5).
// A 501 (WorkspaceReader not wired) is relayed verbatim so the console renders "not available yet".
// A 200 with degraded=true is the "workspace busy → last-committed snapshot" first-class state.
//
// Browser NEVER talks to the apiserver directly; this BFF is the sole authz choke point (§13).

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  const { projectId } = await params;
  const search = req.nextUrl.search;
  return proxyJson(req, `/api/projects/${encodeURIComponent(projectId)}/files${search}`);
}
