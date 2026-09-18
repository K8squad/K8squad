// BFF route — Project File Explorer file stat / change metadata (ISI-4651, ISI-4649).
//
// GET /api/projects/[projectId]/files/stat?path=<file>
//   → proxies to Go apiserver GET /api/projects/{projectId}/files/stat?path=
//
// Pass-through only: no business logic, no status re-mapping.
// The response body is FileStat{name,type,size,modTime,git?{commitHash,author,message,
// timestamp},degraded?} — `git` is ABSENT (not an error) when the workspace is not a git
// checkout or git is unavailable in the reader pod.
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
  return proxyJson(req, `/api/projects/${encodeURIComponent(projectId)}/files/stat${search}`);
}
