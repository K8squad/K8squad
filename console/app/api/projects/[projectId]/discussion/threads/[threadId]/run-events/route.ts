// app/api/projects/[projectId]/discussion/threads/[threadId]/run-events/route.ts —
// BFF proxy for the thread's durable run-event hydration tail (ISI-7869, WS-C
// of parent ISI-7864; the discussion analog of the ISI-7484 run-events proxy).
// GET-ONLY: forwards the session identity, keeps RBAC upstream at the ONE §13
// choke point, surfaces the apiserver response VERBATIM (deny stays
// existence-hiding), and never caches (history is dynamic).

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";
import { encodeProjectId } from "@/lib/projectId";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string; threadId: string }> },
): Promise<Response> {
  const { projectId: p, threadId: t } = await params;
  const projectId = encodeProjectId(p);
  const threadId = encodeURIComponent(t);
  // Forward the pagination query (limit) verbatim so a deep tail can page.
  const search = req.nextUrl.search;
  return proxyJson(
    req,
    `/api/projects/${projectId}/discussion/threads/${threadId}/run-events${search}`,
  );
}
