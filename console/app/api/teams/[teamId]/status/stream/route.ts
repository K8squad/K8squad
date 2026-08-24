// app/api/teams/[teamId]/status/stream/route.ts — BFF SSE proxy for live org-diagram status
// (story 8.10 "status updates live over SSE, §4.4 hub, same BFF proxy as 8.2").
//
// One EventSource per open org diagram rides THIS route (see lib/agents/useTeamStatus.ts). It
// proxies the apiserver's §4.4 SSE hub unbuffered — the browser never learns the apiserver URL and
// holds no apiserver credential (§13 / ADR-013). RBAC scoping + durable-tail reconnect replay
// (Last-Event-ID) are the apiserver's job; the BFF forwards identity + the resume header and adds
// no second authz path. READ-ONLY projection: agent status only, no mutate/claim/kill verb rides it.

import type { NextRequest } from "next/server";
import { proxyEventStream } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ teamId: string }> },
): Promise<Response> {
  const teamId = encodeURIComponent((await params).teamId);
  return proxyEventStream(req, `/api/teams/${teamId}/status/stream`);
}
