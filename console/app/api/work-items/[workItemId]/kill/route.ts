// app/api/work-items/[workItemId]/kill/route.ts — BFF run-kill seam (story 3.3 + 8.4 / ISI-2884).
//
// POST-only. The ≤2-click kill lives in the Go apiserver (fence-first CancelEnter on the coord
// claim); the operator's drive loop + kill sweep do the sandbox teardown and the terminal
// finish (cancelling → cancelled). The BFF relays the caller's session and the apiserver's
// answer VERBATIM — 200 (kill issued, phase Canceling), 404 (no claim), 409 (fence conflict —
// the screen offers retry), 501 (kill seam not hosted).
//
// CSRF posture: proxyJsonWrite carries the session cookie on a state-changing POST; the Origin
// check lands with the Epic 15 session hardening (SameSite=Strict) — tracked with ISI-2921.

import type { NextRequest } from "next/server";
import { proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function POST(
  _req: NextRequest,
  ctx: { params: Promise<{ workItemId: string }> },
): Promise<Response> {
  const { workItemId } = await ctx.params;
  return proxyJsonWrite(_req, `/api/work-items/${encodeURIComponent(workItemId)}/kill`, "POST");
}
