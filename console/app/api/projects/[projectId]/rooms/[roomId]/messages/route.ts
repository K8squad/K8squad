// app/api/projects/[projectId]/rooms/[roomId]/messages/route.ts — BFF proxy for a room's messages
// (story 10.3 AC3/AC4).
//
//   GET  — threaded message history (read model, story 10.1). Forwards the caller's ?limit/
//          ?offset/?threadDepth query verbatim.
//   POST — post a message or reply-in-thread. The console body is ONLY `{ body, parentId? }`;
//          provenance (author_*) is stamped SERVER-SIDE from the authenticated principal
//          (internal/discussion/auth.go). The BFF relays the body UNCHANGED and asserts no
//          principal of its own (AC3).
//
// Both verbs traverse the ONE authz choke point (arch §13 / ADR-013): the apiserver owns the
// deny-by-default gate and this route surfaces its status VERBATIM — a deny is existence-hiding
// (404 never re-mapped to 403, AC4). PUT/PATCH/DELETE are structurally absent → 405.

import type { NextRequest } from "next/server";
import { proxyJson, proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string; roomId: string }> },
): Promise<Response> {
  const { projectId: p, roomId: r } = await params;
  const projectId = encodeURIComponent(p);
  const roomId = encodeURIComponent(r);
  // Forward the caller's query (?limit=, ?offset=, ?threadDepth=) verbatim.
  const search = req.nextUrl.search; // includes leading '?' or ''
  return proxyJson(
    req,
    `/api/projects/${projectId}/rooms/${roomId}/messages${search}`,
  );
}

export async function POST(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string; roomId: string }> },
): Promise<Response> {
  const { projectId: p, roomId: r } = await params;
  const projectId = encodeURIComponent(p);
  const roomId = encodeURIComponent(r);
  return proxyJsonWrite(
    req,
    `/api/projects/${projectId}/rooms/${roomId}/messages`,
    "POST",
  );
}
