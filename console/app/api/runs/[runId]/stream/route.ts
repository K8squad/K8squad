// app/api/runs/[runId]/stream/route.ts — the ONE BFF SSE proxy (story 8.2).
//
// The browser holds exactly one EventSource against THIS route (see components/RunStream.tsx);
// this handler proxies the Go apiserver's SSE progress bus unbuffered. The browser never learns
// the apiserver URL and holds no apiserver credential (arch §13 / ADR-013 — AC2). Flush is
// incremental (AC3). RBAC scoping and durable-tail reconnect replay (Last-Event-ID) are the
// APISERVER's responsibility (§12.3 / §6.5 / §6.6 — AC4/AC5); the BFF forwards identity + the
// resume header and never adds a second authz path. This is a READ-ONLY projection: no mutate,
// claim, or kill verb rides the stream (AC6 — Kill Run is a separate control-plane POST).

import type { NextRequest } from "next/server";
import { proxyEventStream } from "@/lib/bff";

// Never statically cache; this is a live stream.
export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ runId: string }> },
): Promise<Response> {
  const runId = encodeURIComponent((await params).runId);
  return proxyEventStream(req, `/api/runs/${runId}/stream`);
}
