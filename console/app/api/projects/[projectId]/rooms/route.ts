// app/api/projects/[projectId]/rooms/route.ts — BFF proxy for a Project's discussion rooms
// (story 10.3 AC4). GET-ONLY.
//
// The browser talks ONLY to this Next.js server; the room list is proxied to the Go apiserver's
// discussion read-model (internal/discussion, story 10.1) which owns the AUTHORITATIVE
// deny-by-default authZ gate (§12.3 / ADR-013). This route forwards the caller's session identity
// and surfaces the apiserver's response VERBATIM: a deny is existence-hiding — a 404 (or 401/403)
// stays as-is and is NEVER re-mapped to 403, so a Team-B caller cannot tell a Team-A Project's
// room from a missing one. No mutating verb is routed here (POST/PUT/PATCH/DELETE → 405).

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  const projectId = encodeURIComponent((await params).projectId);
  return proxyJson(req, `/api/projects/${projectId}/rooms`);
}
