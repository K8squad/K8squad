// app/api/projects/[projectId]/discussion/threads/[threadId]/decision-requests/route.ts — BFF proxy
// for a thread's decision_request cards (ISI-5536 / ISI-5531 E2, ADR-0026 §4; client seam
// lib/discussion/api.ts listDecisionRequests + postDecisionRequest). Sibling of the proposals route.
//
//   GET  — the thread's decision cards (all phases) joined with lifecycle state, for the FE-4 render
//          side (the transcript read stays phase-less; the console joins by message id).
//   POST — raise an inert decision_request card; any principal may ask (an agent suggests, a human
//          answers). The body is relayed UNCHANGED; provenance is server-stamped.
//
// Proxied to the Go apiserver's discussion handler (internal/discussion/handler.go), which owns the
// AUTHORITATIVE deny-by-default authZ gate (§13 / ADR-013): the session identity is forwarded and the
// apiserver status surfaced VERBATIM — a cross-tenant or absent thread is a 404 (existence-hiding,
// NEVER re-mapped to 403). Without this route Next.js answers 404 for the segment even though the
// apiserver route exists (BFF proxy gap, class of ISI-5164/5250). PUT/PATCH/DELETE → 405.

import type { NextRequest } from "next/server";
import { proxyJson, proxyJsonWrite } from "@/lib/bff";
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
    `/api/projects/${projectId}/discussion/threads/${threadId}/decision-requests`,
  );
}

export async function POST(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string; threadId: string }> },
): Promise<Response> {
  const { projectId: p, threadId: t } = await params;
  const projectId = encodeProjectId(p);
  const threadId = encodeURIComponent(t);
  return proxyJsonWrite(
    req,
    `/api/projects/${projectId}/discussion/threads/${threadId}/decision-requests`,
    "POST",
  );
}
