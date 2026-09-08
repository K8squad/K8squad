// app/api/projects/[projectId]/discussion/threads/route.ts — BFF proxy for a Project's discussion
// threads (Story 10.3 AC1/AC2; the room IS the Project, R1 — its threads are the room).
//
//   GET  — list the Project's threads (read model, story 10.1). The apiserver owns the AUTHORITATIVE
//          deny-by-default authZ gate (§13 / ADR-013); this route forwards the caller's session
//          identity and surfaces the apiserver response VERBATIM — a deny is existence-hiding, a 404
//          (or 401/403) stays as-is and is NEVER re-mapped to 403, so a Team-B caller cannot tell a
//          Team-A Project's threads from a missing Project.
//   POST — open a new thread. The console body is ONLY `{ title, body }`; provenance (author_*) is
//          stamped SERVER-SIDE from the authenticated principal (internal/discussion/handler.go) and
//          the BFF relays the body UNCHANGED, asserting no principal of its own (AC2/AC3).
//
// PUT/DELETE are structurally absent → 405.

import type { NextRequest } from "next/server";
import { proxyJson, proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  const projectId = encodeURIComponent((await params).projectId);
  // Forward the caller's ?limit/?offset paging verbatim.
  const search = req.nextUrl.search; // includes leading '?' or ''
  return proxyJson(
    req,
    `/api/projects/${projectId}/discussion/threads${search}`,
  );
}

export async function POST(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  const projectId = encodeURIComponent((await params).projectId);
  return proxyJsonWrite(
    req,
    `/api/projects/${projectId}/discussion/threads`,
    "POST",
  );
}
