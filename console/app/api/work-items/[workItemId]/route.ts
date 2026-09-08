// app/api/work-items/[workItemId]/route.ts — BFF proxy for the human work-item
// FIELD-EDIT (S3 / ISI-3959). PATCH-ONLY.
//
// The sibling of the state route (../[workItemId]/state): where that path moves a
// card between lanes, this one edits the item's fields (title / body / parent).
// STATE IS NOT EDITABLE HERE — lane moves keep their own contract so the §13
// board-derivation invariant holds. The BFF forwards the caller's session identity
// and body UNCHANGED; the apiserver is the sole authority for the human-only gate,
// the tenancy scope (cross-tenant → 404 existence-hiding), and the
// optimistic-concurrency guard (409 on a stale expectedUpdatedAt). Status is
// relayed VERBATIM: 200, 400, 403 (viewer/agent), 404, 409 (stale) and 501
// (store-less) all reach the browser untouched so the list re-syncs to server
// truth rather than clobbering.

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
  return proxyJsonWrite(req, `/api/work-items/${workItemId}`, "PATCH");
}
