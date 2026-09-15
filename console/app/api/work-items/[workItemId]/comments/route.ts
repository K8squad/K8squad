// app/api/work-items/[workItemId]/comments/route.ts — BFF proxy for the human
// work-item COMMENT write (ISI-4406). POST-ONLY.
//
// The post-box half of the ticket-detail composer (S3 / ISI-4399, design ISI-4231
// §3): where the ../[workItemId] PATCH edits fields and ../[workItemId]/state moves
// lanes, this path appends a human comment to the item's thread. The BFF forwards the
// caller's session identity and body UNCHANGED; the apiserver is the sole authority
// for the human-only gate (403 for an agent token), the tenancy scope (cross-tenant →
// 404 existence-hiding), and server-stamping the author from the session principal —
// the body carries only { body } text, never authorship. Status is relayed VERBATIM:
// 201, 400, 401, 403, 404 and 501 (store-less) all reach the browser untouched so the
// composer appends optimistically on 201 and surfaces the real error otherwise.

import type { NextRequest } from "next/server";
import { proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function POST(
  req: NextRequest,
  { params }: { params: Promise<{ workItemId: string }> },
): Promise<Response> {
  const workItemId = encodeURIComponent((await params).workItemId);
  return proxyJsonWrite(req, `/api/work-items/${workItemId}/comments`, "POST");
}
