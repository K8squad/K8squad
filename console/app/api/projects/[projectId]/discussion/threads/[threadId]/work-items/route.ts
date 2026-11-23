// app/api/projects/[projectId]/discussion/threads/[threadId]/work-items/route.ts —
// BFF proxy for HUMAN-initiated ticket creation from a discussion thread
// (ISI-7708 endpoint / ISI-7710 console; client seam lib/discussion/api.ts
// createThreadTickets).
//
//   POST — relays the { tickets: [...] } body UNCHANGED to the Go apiserver's
//          thread-scoped work-items handler, which owns the AUTHORITATIVE wall:
//          human-only (an agent caller is 403), project Contributor RBAC,
//          whole-batch validation before any mint, backlog landing, and the
//          ksquad.discussion.* provenance backlink labels. The apiserver status
//          is surfaced VERBATIM — a cross-tenant or absent thread is a 404
//          (existence-hiding, NEVER re-mapped). Without this route Next.js has
//          no handler for the segment and answers 404 even though the apiserver
//          route exists — the BFF proxy gap class of ISI-5164/ISI-5250.
//
// POST only (the read side is the board's issues surface, not the thread);
// anything else → 405 by omission, matching the proposals proxy.

import type { NextRequest } from "next/server";
import { proxyJsonWrite } from "@/lib/bff";
import { encodeProjectId } from "@/lib/projectId";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function POST(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string; threadId: string }> },
): Promise<Response> {
  const { projectId: p, threadId: t } = await params;
  const projectId = encodeProjectId(p);
  const threadId = encodeURIComponent(t);
  return proxyJsonWrite(
    req,
    `/api/projects/${projectId}/discussion/threads/${threadId}/work-items`,
    "POST",
  );
}
