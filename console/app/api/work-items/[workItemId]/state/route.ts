// app/api/work-items/[workItemId]/state/route.ts — BFF proxy for the HUMAN
// status-transition (stories 8.14a/8.14b, ADR-037). PATCH-ONLY.
//
// This is the ONE mutation the Tickets screen adds: a drag-and-drop (or
// quick-move) issues PATCH /work-items/{id}/state {to, expectedFrom} — an
// audited, concurrency-guarded, RBAC-gated operator override. The BFF forwards
// the caller's session identity and body UNCHANGED; the apiserver is the sole
// authority for the conditional UPDATE (409 on stale expectedFrom), the
// contributor/maintainer RBAC wall (§6.7.2), the audit record
// (initiated_by_user_id, §6.5) and the no-fence guarantee (§6.2 — the agent's
// claim row is never touched by this path). Status is relayed VERBATIM: 200,
// 400 (bad enum / blocked-as-to), 401/403 (viewer), 404 (existence-hiding) and
// 409 (stale) all reach the browser untouched so the board re-syncs to server
// truth. Until ISI-2909 hosts the upstream route, its documented status
// surfaces here unchanged — the console never fabricates a move.

import type { NextRequest } from "next/server";
import { proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function PATCH(
  req: NextRequest,
  { params }: { params: Promise<{ workItemId: string }> },
): Promise<Response> {
  const workItemId = encodeURIComponent((await params).workItemId);
  return proxyJsonWrite(req, `/api/work-items/${workItemId}/state`, "PATCH");
}
