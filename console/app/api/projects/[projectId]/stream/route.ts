// app/api/projects/[projectId]/stream/route.ts — BFF SSE proxy for a Project's live surfaces
// (story 10.3 AC6). GET-ONLY.
//
// The discussion room rides the SAME one-bus SSE seam as every other live console surface
// (story 8.2): the browser holds a single EventSource against THIS route, and the handler proxies
// the Go apiserver's per-Project event stream UNBUFFERED (flush is incremental; `X-Accel-Buffering:
// no`). The browser never learns the apiserver URL (arch §13 / ADR-013). RBAC scoping and durable
// tail reconnect (Last-Event-ID) are the apiserver's responsibility; the BFF forwards identity +
// the resume header and adds no second authz path. READ-ONLY projection — no mutate/coordination
// verb rides the stream (AC5).

import type { NextRequest } from "next/server";
import { proxyEventStream } from "@/lib/bff";

// Never statically cache; this is a live stream.
export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  const projectId = encodeURIComponent((await params).projectId);
  return proxyEventStream(req, `/api/projects/${projectId}/stream`);
}
