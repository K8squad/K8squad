// app/api/projects/[projectId]/discussion/threads/[threadId]/route.ts — BFF proxy for a single
// thread + its threaded live messages (Story 10.3 AC1/AC3). GET-ONLY.
//
// Proxied to the Go apiserver's discussion read-model (internal/discussion, story 10.1) which owns
// the AUTHORITATIVE deny-by-default authZ gate (§13 / ADR-013). The caller's session identity is
// forwarded and the apiserver response surfaced VERBATIM: a cross-tenant or absent thread is a 404
// (existence-hiding, NEVER re-mapped to 403), so a foreign thread is indistinguishable from missing.
// No mutating verb is routed here (POST/PUT/PATCH/DELETE → 405).

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
  return proxyJson(
    req,
    `/api/projects/${projectId}/discussion/threads/${threadId}`,
  );
}
