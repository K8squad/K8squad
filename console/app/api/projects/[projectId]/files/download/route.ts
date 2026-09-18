// S4c BFF route — Project File Explorer download (ISI-4650, ADR-0012 §D2).
//
// GET /api/projects/[projectId]/files/download?path=<p>
//   → proxies to Go apiserver GET /api/projects/{projectId}/files/download?path=
//
// Pass-through only: no business logic, no status re-mapping. A file path streams an
// octet-stream attachment; a directory path streams a server-built tar.gz archive.
// Content-Disposition (the apiserver-chosen filename) is relayed verbatim by
// proxyDownload. A 404 (non-member / unknown project) is relayed verbatim —
// existence-hiding (NFR-SEC5); a 501 (WorkspaceReader not wired) lets the console
// render "not available yet"; 413 (size cap) and 503 (workspace busy) surface as-is.
//
// Browser NEVER talks to the apiserver directly; this BFF is the sole authz choke point (§13).

import type { NextRequest } from "next/server";
import { proxyDownload } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  const { projectId } = await params;
  const search = req.nextUrl.search;
  return proxyDownload(
    req,
    `/api/projects/${encodeURIComponent(projectId)}/files/download${search}`,
  );
}
