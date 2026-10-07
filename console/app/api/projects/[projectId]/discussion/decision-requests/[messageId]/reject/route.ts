// app/api/projects/[projectId]/discussion/decision-requests/[messageId]/reject/route.ts — BFF proxy
// for the human REJECT shell of a kind='decision_request' message (ISI-5536 / ISI-5531 E2,
// ADR-0026 §4.4; client seam lib/discussion/api.ts rejectDecisionRequest). The console POSTs
// {reason?}; the Go apiserver (internal/apiserver/decisionrequest.go) owns the AUTHORITATIVE
// human-only authZ, the rejectRequiresReason enforcement (400 when a reason is demanded but absent),
// and the agent-resume continuation.
//
// Keyed by {projectId, messageId} ONLY (thread resolved apiserver-side). Status surfaced VERBATIM
// (agent → 403, absent/cross-tenant → 404, already-decided → 409, missing-required-reason → 400).
// GET/PUT/PATCH/DELETE → Next's 405.

import type { NextRequest } from "next/server";
import { proxyJsonWrite } from "@/lib/bff";
import { encodeProjectId } from "@/lib/projectId";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function POST(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string; messageId: string }> },
): Promise<Response> {
  const { projectId: p, messageId: m } = await params;
  const projectId = encodeProjectId(p);
  const messageId = encodeURIComponent(m);
  return proxyJsonWrite(
    req,
    `/api/projects/${projectId}/discussion/decision-requests/${messageId}/reject`,
    "POST",
  );
}
