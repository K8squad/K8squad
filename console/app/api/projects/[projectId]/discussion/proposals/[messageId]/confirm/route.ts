// app/api/projects/[projectId]/discussion/proposals/[messageId]/confirm/route.ts — BFF proxy for the
// human CONFIRM shell of a kind='proposal' message (ISI-4928, plan ISI-4919 §4.4; client seam
// lib/discussion/api.ts confirmProposal). The console seam (and both the discussion room and the
// ticket surface, ISI-5284 WS-4) POST here with an empty body; the Go apiserver owns the AUTHORITATIVE
// deny-by-default authZ + the proposalconfirm.go fan-out that mints/assigns the proposed work.
//
// Keyed by {projectId, messageId} ONLY — the apiserver resolves the proposal's thread from the message
// id, so no threadId rides the path (matching internal/apiserver/proposalconfirm.go's route). Without
// this route Next.js had no handler for the segment and answered 404 even though the apiserver route
// exists — the same BFF proxy gap class as ISI-5164 / ISI-5250. The apiserver status is surfaced
// VERBATIM (a cross-tenant or absent proposal is a 404 — existence-hiding, never re-mapped). GET/PUT/
// PATCH/DELETE fall through to Next's 405.

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
    `/api/projects/${projectId}/discussion/proposals/${messageId}/confirm`,
    "POST",
  );
}
