// app/api/projects/[projectId]/discussion/decision-requests/[messageId]/answer/route.ts — BFF proxy
// for the human ANSWER shell of a kind='decision_request' message (ISI-5536 / ISI-5531 E2,
// ADR-0026 §4.4; client seam lib/discussion/api.ts answerDecisionRequest). The console POSTs the
// typed answer ({selectedOptionIds?, freeText?}); the Go apiserver (internal/apiserver/
// decisionrequest.go) owns the AUTHORITATIVE human-only authZ, the before-CAS selection validation,
// and the agent-resume continuation (RequestDispatch).
//
// Keyed by {projectId, messageId} ONLY — the apiserver resolves the card's thread from the message id,
// so no threadId rides the path (matching the apiserver route). The apiserver status is surfaced
// VERBATIM (agent caller → 403, cross-tenant/absent → 404, already-decided → 409). GET/PUT/PATCH/
// DELETE → Next's 405.

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
    `/api/projects/${projectId}/discussion/decision-requests/${messageId}/answer`,
    "POST",
  );
}
