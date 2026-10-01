// app/api/projects/[projectId]/discussion/proposals/[messageId]/dismiss/route.ts — BFF proxy for the
// human DISMISS shell of a kind='proposal' message (ISI-4928, plan ISI-4919 §4.4; client seam
// lib/discussion/api.ts dismissProposal). Dismiss records the decision (proposed → dismissed) and
// fans out NOTHING — no work is minted. Reused by the discussion room and the ticket surface
// (ISI-5284 WS-4).
//
// Keyed by {projectId, messageId} ONLY (the apiserver resolves the proposal's thread from the message
// id — internal/apiserver/proposalconfirm.go). The apiserver is the AUTHORITATIVE deny-by-default gate;
// its status is surfaced VERBATIM (absent/cross-tenant → 404 existence-hiding). Without this route
// Next answered 404 for the segment though the apiserver route exists (BFF gap class ISI-5164/5250).
// GET/PUT/PATCH/DELETE fall through to Next's 405.

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
    `/api/projects/${projectId}/discussion/proposals/${messageId}/dismiss`,
    "POST",
  );
}
