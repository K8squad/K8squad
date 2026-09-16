// app/api/work-items/[workItemId]/dispatch/route.ts — BFF board-dispatch seam
// (ISI-4501, ADR-0022 / ISI-4411, workitemdispatch.go).
//
// POST-only. "Assign an agent" on the board is custody/dispatch-based, NOT a
// create/PATCH field: the Go apiserver stamps the human's chosen agent on the
// work item and advances backlog→todo so operator Intake mints the Run. The BFF
// relays the caller's session and the apiserver's answer VERBATIM — 200
// (dispatched, item → todo), 400 (agentId missing), 403 (agent not in the owning
// Team / dispatch is human-only), 404 (no such item), 409 (item not in backlog),
// 501 (dispatch seam not hosted).
//
// CSRF posture: proxyJsonWrite carries the session cookie on the state-changing
// POST; the Origin check lands with the Epic 15 session hardening (ISI-2921).

import type { NextRequest } from "next/server";
import { proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function POST(
  req: NextRequest,
  ctx: { params: Promise<{ workItemId: string }> },
): Promise<Response> {
  const { workItemId } = await ctx.params;
  return proxyJsonWrite(
    req,
    `/api/work-items/${encodeURIComponent(workItemId)}/dispatch`,
    "POST",
  );
}
