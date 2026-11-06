// app/api/work-items/[workItemId]/activity/route.ts — BFF proxy for the
// two-phase comment UI's PROGRESS feed (ISI-7095 / ISI-7088). GET-ONLY.
//
// The sibling of ../[workItemId] (the human+summary thread read): where that
// path serves the default conversation, this one serves the run sub-step
// firehose (coord.comment kind='progress') the thread EXCLUDES (ISI-7094),
// backing the ticket view's per-run "Activity (N events)" expander. The console
// fetches it LAZILY — only when a run's expander opens — so a ticket that nobody
// expands never pays for the progress rows. The BFF forwards the caller's
// session identity unchanged; the apiserver owns the tenancy fence (cross-tenant
// → 404 existence-hiding) and relays its status verbatim: 200 (array of progress
// comments, each carrying kind + runId), 401, 404 and 501 (store-less) all reach
// the browser untouched.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ workItemId: string }> },
): Promise<Response> {
  const workItemId = encodeURIComponent((await params).workItemId);
  return proxyJson(req, `/api/work-items/${workItemId}/activity`);
}
